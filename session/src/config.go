// config.go —— session 插件配置面（PLUGIN-UI-1）。
//
// 唯一链路：
//
//	webui/config.html → POST /plugins/session/api/{load,save,reset}
//	  → 宿主动作面（plugin.ui.rpc）→ 本文件（校验 + 默认兜底）
//	  → plugin/v1/config.user.{load,set,delete} → data 层 usr 配置（`plugins.session.*`）
//
// 键：
//   - plugins.session.cross-user          private | llm | public（缺省 private）
//   - plugins.session.run.max-concurrent  0..50（缺省 3；0 = 不限）
//   - plugins.session.semantic.enabled    true | false（缺省 false）
//   - plugins.session.semantic.base-url   OpenAI 兼容 base（启用时必填）
//   - plugins.session.semantic.api-key    可选鉴权
//   - plugins.session.semantic.model      embedding 模型名（启用时必填）
//   - plugins.session.semantic.max-embed-per-call  1..256（缺省 64）
//
// usr 优先、prj 作默认的读序在**宿主侧**实现（`server/plugin_caps_session.go` /
// `plugin_caps_session_run.go`）；本页只负责写 usr 层（用户自己的选择）。
package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	// keyCrossUser 是跨用户模式（private / llm / public）。
	keyCrossUser = "plugins.session.cross-user"
	// keyRunMax 是 session.run 并发上限（0 = 不限）。
	keyRunMax = "plugins.session.run.max-concurrent"

	// 语义检索（SESSION-SEMANTIC-1）：任意 OpenAI 兼容 /embeddings。
	keySemEnabled  = "plugins.session.semantic.enabled"
	keySemBaseURL  = "plugins.session.semantic.base-url"
	keySemAPIKey   = "plugins.session.semantic.api-key"
	keySemModel    = "plugins.session.semantic.model"
	keySemMaxEmbed = "plugins.session.semantic.max-embed-per-call"
	keySemAutoIdx  = "plugins.session.semantic.auto-index"
	keySemHNSW     = "plugins.session.semantic.hnsw-threshold"

	runMaxMin = 0
	runMaxMax = 50
	embedMin  = 1
	embedMax  = 256
	hnswMax   = 100000
)

// configKeys 是本插件用户配置键全集（稳定序）。
var configKeys = []string{
	keyCrossUser, keyRunMax,
	keySemEnabled, keySemBaseURL, keySemAPIKey, keySemModel, keySemMaxEmbed,
	keySemAutoIdx, keySemHNSW,
}

// configFields 是配置页字段声明（页面据 type/options 渲染）。
var configFields = []map[string]any{
	{
		"key": keyCrossUser, "type": "select", "label": "跨用户会话访问",
		"options": []map[string]string{
			{"value": "private", "label": "private — 只访问自己的会话（默认）"},
			{"value": "llm", "label": "llm — 模型可点名其他用户"},
			{"value": "public", "label": "public — 默认检索全项目（自己优先）"},
		},
		"hint": "usr 个人设置优先，项目默认由 prj 配置提供；private 下点名他人会明确拒绝。",
	},
	{
		"key": keyRunMax, "type": "integer", "label": "并发会话上限（0 = 不限）",
		"min": runMaxMin, "max": runMaxMax,
		"hint": "session_create / session_send 同时运行的轮次上限；超限排队。",
	},
	{
		"key": keySemEnabled, "type": "boolean", "label": "启用向量语义检索",
		"hint": "开启后 session_search 可用 mode=semantic / hybrid；需下方 embedding 配置。",
	},
	{
		"key": keySemBaseURL, "type": "text", "label": "Embedding Base URL",
		"hint": "OpenAI 兼容地址（如 https://qianfan.baidubce.com/v2 或 https://api.openai.com/v1）；插件会追加 /embeddings。",
	},
	{
		"key": keySemAPIKey, "type": "secret", "label": "Embedding API Key",
		"hint": "留空 = 服务端无鉴权（本地部署）；仅存 usr 配置。",
	},
	{
		"key": keySemModel, "type": "text", "label": "Embedding 模型",
		"hint": "如 bge-large-zh / text-embedding-3-small；换模型会触发按需重建向量。",
	},
	{
		"key": keySemMaxEmbed, "type": "integer", "label": "单次补索引上限",
		"min": embedMin, "max": embedMax,
		"hint": "每次 semantic / hybrid 检索最多补写多少条向量（缺省 64；历史消息随多次检索逐步覆盖）。",
	},
	{
		"key": keySemAutoIdx, "type": "boolean", "label": "后台全量索引",
		"hint": "开启后首次语义检索会在后台为当前用户补齐全部历史消息的向量；进度随 session_search 的 index 字段返回。",
	},
	{
		"key": keySemHNSW, "type": "integer", "label": "HNSW 启用阈值（向量数）",
		"min": 0, "max": hnswMax,
		"hint": "某用户向量数达到该值时，无过滤条件的 semantic 检索走 HNSW 近似检索（0 = 缺省 512）；结果会返回 approx=true。",
	},
}

// defaultConfig 缺键兜底（与宿主读取口径一致）。
func defaultConfig() map[string]string {
	return map[string]string{
		keyCrossUser:   "private",
		keyRunMax:      "3",
		keySemEnabled:  "false",
		keySemMaxEmbed: "64",
		keySemAutoIdx:  "true",
		keySemHNSW:     "512",
	}
}

// configActions 返回本插件承担的 UI 动作（load / save / reset）。
func (t *sessionPlugin) configActions() map[string]func(context.Context, map[string]any) (any, error) {
	return map[string]func(context.Context, map[string]any) (any, error){
		"load":  t.configLoad,
		"save":  t.configSave,
		"reset": t.configReset,
	}
}

func (t *sessionPlugin) configLoad(ctx context.Context, _ map[string]any) (any, error) {
	host, err := t.configRead(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取配置失败：%w", err)
	}
	return map[string]any{
		"ok": true, "fields": configFields,
		"values": mergeConfig(host), "explicit": host,
	}, nil
}

func (t *sessionPlugin) configSave(ctx context.Context, payload map[string]any) (any, error) {
	raw, _ := payload["values"].(map[string]any)
	if raw == nil {
		return nil, fmt.Errorf("config_invalid: 缺少 values")
	}
	entries := map[string]string{}
	keys := make([]string, 0, len(raw))
	for k, v := range raw {
		if !isKnownConfigKey(k) {
			return nil, fmt.Errorf("config_invalid: 未知配置键 %s", k)
		}
		val, err := normalizeConfigValue(k, v)
		if err != nil {
			return nil, err
		}
		entries[k] = val
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if err := t.validateSemanticCombo(ctx, entries); err != nil {
		return nil, err
	}
	if err := t.configWrite(ctx, entries); err != nil {
		return nil, fmt.Errorf("配置写入失败：%w", err)
	}
	host, err := t.configRead(ctx)
	if err != nil {
		host = entries
	}
	return map[string]any{"ok": true, "keys": keys, "values": mergeConfig(host)}, nil
}

func (t *sessionPlugin) configReset(ctx context.Context, _ map[string]any) (any, error) {
	for _, k := range configKeys {
		var out map[string]any
		if err := t.call.CallIntoWithTimeout(ctx, metaTimeout,
			"plugin/v1/config.user.delete", map[string]any{"id": k}, &out); err != nil {
			return nil, fmt.Errorf("恢复默认失败（%s）：%w", k, err)
		}
	}
	return map[string]any{"ok": true, "values": defaultConfig()}, nil
}

// userConfigLoadResp 是 `plugin/v1/config.user.load` 的应答（宿主只回本插件前缀）。
type userConfigLoadResp struct {
	Data map[string]string `json:"data"`
}

// configRead 读 usr 层已落库的键。
func (t *sessionPlugin) configRead(ctx context.Context) (map[string]string, error) {
	var res userConfigLoadResp
	if err := t.call.CallIntoWithTimeout(ctx, metaTimeout,
		"plugin/v1/config.user.load", map[string]any{}, &res); err != nil {
		return nil, err
	}
	if res.Data == nil {
		return map[string]string{}, nil
	}
	return res.Data, nil
}

func (t *sessionPlugin) configWrite(ctx context.Context, entries map[string]string) error {
	if len(entries) == 0 {
		return nil
	}
	values := make(map[string]any, len(entries))
	for k, v := range entries {
		values[k] = v
	}
	var out map[string]any
	return t.call.CallIntoWithTimeout(ctx, metaTimeout,
		"plugin/v1/config.user.set", map[string]any{"entries": values}, &out)
}

// mergeConfig 把宿主已落库值叠加到默认值上（缺失键 = 默认）。
func mergeConfig(host map[string]string) map[string]string {
	out := defaultConfig()
	for k, v := range host {
		if isKnownConfigKey(k) && strings.TrimSpace(v) != "" {
			out[k] = v
		}
	}
	return out
}

func isKnownConfigKey(key string) bool {
	for _, k := range configKeys {
		if k == key {
			return true
		}
	}
	return false
}

// normalizeConfigValue 校验并归一（非法 → 明确错误，不静默截断）。
func normalizeConfigValue(key string, raw any) (string, error) {
	switch key {
	case keyCrossUser:
		v := strings.ToLower(strings.TrimSpace(fmt.Sprint(raw)))
		switch v {
		case "private", "llm", "public":
			return v, nil
		}
		return "", fmt.Errorf("config_invalid: %s 只允许 private / llm / public（收到 %q）", key, v)
	case keyRunMax:
		n, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(raw)))
		if err != nil || n < runMaxMin || n > runMaxMax {
			return "", fmt.Errorf("config_invalid: %s 必须是 %d..%d 的整数", key, runMaxMin, runMaxMax)
		}
		return strconv.Itoa(n), nil
	case keySemEnabled:
		switch strings.ToLower(strings.TrimSpace(fmt.Sprint(raw))) {
		case "true", "1", "yes", "on":
			return "true", nil
		case "false", "0", "no", "off", "":
			return "false", nil
		}
		return "", fmt.Errorf("config_invalid: %s 只允许 true / false", key)
	case keySemBaseURL:
		return strings.TrimSpace(fmt.Sprint(raw)), nil
	case keySemAPIKey:
		return strings.TrimSpace(fmt.Sprint(raw)), nil
	case keySemModel:
		return strings.TrimSpace(fmt.Sprint(raw)), nil
	case keySemMaxEmbed:
		n, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(raw)))
		if err != nil || n < embedMin || n > embedMax {
			return "", fmt.Errorf("config_invalid: %s 必须是 %d..%d 的整数", key, embedMin, embedMax)
		}
		return strconv.Itoa(n), nil
	case keySemAutoIdx:
		switch strings.ToLower(strings.TrimSpace(fmt.Sprint(raw))) {
		case "true", "1", "yes", "on":
			return "true", nil
		case "false", "0", "no", "off", "":
			return "false", nil
		}
		return "", fmt.Errorf("config_invalid: %s 只允许 true / false", key)
	case keySemHNSW:
		n, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(raw)))
		if err != nil || n < 0 || n > hnswMax {
			return "", fmt.Errorf("config_invalid: %s 必须是 0..%d 的整数", key, hnswMax)
		}
		return strconv.Itoa(n), nil
	}
	return "", fmt.Errorf("config_invalid: 未知配置键 %s", key)
}

// validateSemanticCombo 做跨字段校验：启用语义检索时 base-url / model 不能为空。
//
// 校验对象是"已落库 + 本次提交"的合并结果——页面整体提交，因此单改 enabled 也能查到
// 旧的 base-url。
func (t *sessionPlugin) validateSemanticCombo(ctx context.Context, entries map[string]string) error {
	merged := map[string]string{}
	if host, err := t.configRead(ctx); err == nil {
		for k, v := range host {
			merged[k] = v
		}
	}
	for k, v := range entries {
		merged[k] = v
	}
	if !configTruthy(merged[keySemEnabled]) {
		return nil
	}
	if strings.TrimSpace(merged[keySemBaseURL]) == "" {
		return fmt.Errorf("config_invalid: 启用语义检索时 %s 必填", keySemBaseURL)
	}
	if strings.TrimSpace(merged[keySemModel]) == "" {
		return fmt.Errorf("config_invalid: 启用语义检索时 %s 必填", keySemModel)
	}
	return nil
}

// configTruthy 判定布尔配置（与宿主读侧同口径）。
func configTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}
