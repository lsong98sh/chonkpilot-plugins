// config_test.go —— 会话插件配置面（load / save / reset）与 UI 资源。
package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// configFake 模拟宿主配置能力（记录写入 / 删除）。
type configFake struct {
	data    map[string]string
	written map[string]string
	deleted []string
}

func (f *configFake) CallIntoWithTimeout(_ context.Context, _ time.Duration, method string, args, out any) error {
	switch method {
	case "plugin/v1/config.user.load":
		resp := out.(*userConfigLoadResp)
		resp.Data = map[string]string{}
		for k, v := range f.data {
			resp.Data[k] = v
		}
	case "plugin/v1/config.user.set":
		entries, _ := args.(map[string]any)["entries"].(map[string]any)
		if f.written == nil {
			f.written = map[string]string{}
		}
		for k, v := range entries {
			f.written[k] = v.(string)
			f.data[k] = v.(string)
		}
	case "plugin/v1/config.user.delete":
		key, _ := args.(map[string]any)["id"].(string)
		f.deleted = append(f.deleted, key)
		delete(f.data, key)
	}
	return nil
}

func TestConfigLoadMergesDefaults(t *testing.T) {
	f := &configFake{data: map[string]string{keyCrossUser: "public"}}
	p := &sessionPlugin{call: f}
	got, err := p.configLoad(context.Background(), nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m, _ := got.(map[string]any)
	vals, _ := m["values"].(map[string]string)
	if vals[keyCrossUser] != "public" || vals[keyRunMax] != "3" {
		t.Fatalf("应合并默认值：%+v", vals)
	}
	if vals[keySemEnabled] != "false" || vals[keySemMaxEmbed] != "64" {
		t.Fatalf("语义检索默认值异常：%+v", vals)
	}
	if vals[keySemAutoIdx] != "true" || vals[keySemHNSW] != "512" {
		t.Fatalf("全量索引 / HNSW 默认值异常：%+v", vals)
	}
}

func TestConfigSaveValidatesAndWrites(t *testing.T) {
	f := &configFake{data: map[string]string{}}
	p := &sessionPlugin{call: f}
	got, err := p.configSave(context.Background(), map[string]any{
		"values": map[string]any{keyCrossUser: "llm", keyRunMax: float64(5)},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if f.written[keyCrossUser] != "llm" || f.written[keyRunMax] != "5" {
		t.Fatalf("写入异常：%+v", f.written)
	}
	if m, _ := got.(map[string]any); m["keys"] == nil {
		t.Fatalf("返回缺 keys：%+v", m)
	}

	for _, bad := range []map[string]any{
		{keyCrossUser: "everyone"},
		{keyRunMax: float64(51)},
		{keySemEnabled: "maybe"},
		{keySemMaxEmbed: float64(0)},
		{"plugins.session.unknown": "x"},
	} {
		if _, err := p.configSave(context.Background(), map[string]any{"values": bad}); err == nil {
			t.Fatalf("非法配置应被拒：%+v", bad)
		}
	}
}

// TestConfigSaveSemanticRequiresEndpoint 覆盖跨字段校验：启用语义检索必须有 base-url + model。
func TestConfigSaveSemanticRequiresEndpoint(t *testing.T) {
	f := &configFake{data: map[string]string{}}
	p := &sessionPlugin{call: f}
	ctx := context.Background()

	if _, err := p.configSave(ctx, map[string]any{"values": map[string]any{
		keySemEnabled: true,
	}}); err == nil {
		t.Fatal("启用但缺 base-url / model 应被拒")
	}

	got, err := p.configSave(ctx, map[string]any{"values": map[string]any{
		keySemEnabled:  true,
		keySemBaseURL:  "http://127.0.0.1:8080/v1",
		keySemModel:    "bge-large-zh",
		keySemAPIKey:   "sk-test",
		keySemMaxEmbed: float64(32),
	}})
	if err != nil {
		t.Fatalf("save semantic: %v", err)
	}
	if f.written[keySemEnabled] != "true" || f.written[keySemMaxEmbed] != "32" {
		t.Fatalf("布尔/整数归一异常：%+v", f.written)
	}
	vals, _ := got.(map[string]any)["values"].(map[string]string)
	if vals[keySemModel] != "bge-large-zh" || vals[keySemAPIKey] != "sk-test" {
		t.Fatalf("返回合并值异常：%+v", vals)
	}
}

func TestConfigResetDeletesAllKeys(t *testing.T) {
	f := &configFake{data: map[string]string{keyCrossUser: "public"}}
	p := &sessionPlugin{call: f}
	got, err := p.configReset(context.Background(), nil)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(f.deleted) != len(configKeys) {
		t.Fatalf("应删除全部键：%+v", f.deleted)
	}
	vals, _ := got.(map[string]any)["values"].(map[string]string)
	if vals[keyCrossUser] != "private" || vals[keyRunMax] != "3" {
		t.Fatalf("恢复默认异常：%+v", vals)
	}
}

func TestUIAssetServesConfigAndRejectsTraversal(t *testing.T) {
	got, err := uiAssetGet(context.Background(), map[string]any{"path": "webui/config.html"})
	if err != nil {
		t.Fatalf("ui asset: %v", err)
	}
	m, _ := got.(map[string]any)
	data, _ := m["data"].([]byte)
	if len(data) == 0 || !strings.Contains(string(data), "会话工具设置") {
		t.Fatalf("config.html 内容异常（%d bytes）", len(data))
	}
	for _, bad := range []string{"../secret", "/etc/passwd", `webui\config.html`, "webui/../x"} {
		if _, err := uiAssetGet(context.Background(), map[string]any{"path": bad}); err == nil {
			t.Fatalf("越界路径应被拒：%q", bad)
		}
	}
	if _, err := uiAssetGet(context.Background(), map[string]any{"path": "webui/nope.html"}); err == nil {
		t.Fatal("未命中应报错")
	}
}
