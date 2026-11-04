// 插件配置页（MEMORY-UI-1，2026-11-02）：webui 资源 + UI 动作面。
//
// 与 crontab 插件同构：`ui.selfServe=true` → 宿主经 `plugin/v1/ui.asset.get` 取资源；
// 页面内 `ckPlugin.call('rpc', {method, params})` → 本文件的 `OnRequest` 处理体。
//
// 数据面**零新增主题**：
//   - 项目配置读写 → `memory.*`（data-prj-config-list / data-prj-config-save）
//   - 记忆全文读写 → data-memory-read / data-memory-save / data-memory-delete
//   - 沉淀提示词    → plugin/v1/config.prompt.{get,set,delete}（prompt 域文件化键）
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"

	memory "github.com/chonkpilot/chonkpilot-plugin-memory"
	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

// promptKeyPrefix 是类别沉淀提示词在 prompt 域的键前缀（与主仓 `memory_prompt.<类别>` 同口径）。
const promptKeyPrefix = "memory_prompt."

// registerUI 注册配置页动作面。
func registerUI(p *pluginsdk.Plugin, plug *memory.Plugin) {
	p.OnRequest("config-load", func(_ context.Context, params map[string]any) (any, error) {
		iid, err := requireInstance(params)
		if err != nil {
			return nil, err
		}
		return plug.ConfigView(iid)
	})
	p.OnRequest("config-save", func(_ context.Context, params map[string]any) (any, error) {
		iid, err := requireInstance(params)
		if err != nil {
			return nil, err
		}
		entries := map[string]string{}
		if raw, ok := params["entries"].(map[string]any); ok {
			for k, v := range raw {
				entries[k] = pluginsdk.Str(v)
			}
		}
		if err := plug.SaveConfigEntries(iid, entries); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "saved": len(entries)}, nil
	})
	p.OnRequest("category-content", func(_ context.Context, params map[string]any) (any, error) {
		iid, err := requireInstance(params)
		if err != nil {
			return nil, err
		}
		content, err := plug.CategoryContent(iid, pluginsdk.Str(params["category"]))
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "content": content}, nil
	})
	p.OnRequest("category-save", func(_ context.Context, params map[string]any) (any, error) {
		iid, err := requireInstance(params)
		if err != nil {
			return nil, err
		}
		if err := plug.SaveCategoryContent(iid, pluginsdk.Str(params["category"]), pluginsdk.Str(params["content"])); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
	p.OnRequest("category-delete", func(ctx context.Context, params map[string]any) (any, error) {
		iid, err := requireInstance(params)
		if err != nil {
			return nil, err
		}
		cat := pluginsdk.Str(params["category"])
		if cat == "" {
			return nil, fmt.Errorf("category required")
		}
		if _, err := p.Call(ctx, "plugin/v1/memory.delete", map[string]any{
			"instance_id": iid,
			"data":        map[string]any{"category": cat},
		}); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
	p.OnRequest("prompt-save", func(ctx context.Context, params map[string]any) (any, error) {
		iid, err := requireInstance(params)
		if err != nil {
			return nil, err
		}
		cat := pluginsdk.Str(params["category"])
		if cat == "" {
			return nil, fmt.Errorf("category required")
		}
		if _, err := p.Call(ctx, "plugin/v1/config.prompt.set", map[string]any{
			"instance_id": iid,
			"data":        map[string]any{"key": promptKeyPrefix + cat, "value": pluginsdk.Str(params["value"])},
		}); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
	p.OnRequest("prompt-reset", func(ctx context.Context, params map[string]any) (any, error) {
		iid, err := requireInstance(params)
		if err != nil {
			return nil, err
		}
		cat := pluginsdk.Str(params["category"])
		if cat == "" {
			return nil, fmt.Errorf("category required")
		}
		if _, err := p.Call(ctx, "plugin/v1/config.prompt.delete", map[string]any{
			"instance_id": iid,
			"id":          promptKeyPrefix + cat,
		}); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
	// 手动沉淀（当前活动会话）：活动会话 → 对话库 id → 复用 memory.flush 订阅体（投递即回）。
	p.OnRequest("flush", func(ctx context.Context, params map[string]any) (any, error) {
		iid, err := requireInstance(params)
		if err != nil {
			return nil, err
		}
		session, dialogID := activeSession(ctx, p, iid)
		if session == "" {
			return map[string]any{"ok": false, "reason": "当前没有活动会话"}, nil
		}
		return plug.Flush(iid, session, dialogID), nil
	})
	p.OnRequest("plugin/v1/ui.asset.get", uiAssetGet)
}

// activeSession 取当前活动会话 + 对话库 id（取不到 → 空串，调用方给明确提示）。
func activeSession(ctx context.Context, p *pluginsdk.Plugin, instanceID string) (string, string) {
	raw, err := p.Call(ctx, "plugin/v1/session.active.get", map[string]any{"instance_id": instanceID})
	if err != nil {
		return "", ""
	}
	session := pluginsdk.Str(jsonField(raw, "session_id"))
	if session == "" {
		return "", ""
	}
	raw, err = p.Call(ctx, "plugin/v1/session.get", map[string]any{"instance_id": instanceID, "id": session})
	if err != nil {
		return session, ""
	}
	// `data-session-get` 结果 = `{data: 会话行|null}`；对话库 id 在会话行内（`data.dialog_id`）。
	if row, ok := jsonField(raw, "data").(map[string]any); ok {
		return session, pluginsdk.Str(row["dialog_id"])
	}
	return session, ""
}

// jsonField 取 JSON 对象里的一个键（解析失败 → nil）。
func jsonField(raw json.RawMessage, key string) any {
	var m map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m[key]
}

// requireInstance 取必填实例 id。
func requireInstance(params map[string]any) (string, error) {
	iid := strings.TrimSpace(pluginsdk.Str(params["instance_id"]))
	if iid == "" {
		return "", fmt.Errorf("instance_id required")
	}
	return iid, nil
}

// ── 资源面（插件自持 webui）───────────────

func uiAssetGet(_ context.Context, payload map[string]any) (any, error) {
	raw, _ := payload["path"].(string)
	rel, ok := normalizeUIAssetPath(raw)
	if !ok {
		return nil, fmt.Errorf("invalid_path: %q", raw)
	}
	b, err := fs.ReadFile(webuiFS, uiAssetRoot+"/"+rel)
	if err != nil {
		return nil, fmt.Errorf("not_found: %s", rel)
	}
	return map[string]any{"data": b, "size": len(b)}, nil
}

// normalizeUIAssetPath 把宿主传入路径归一为 webui 根内相对路径（越界一律拒绝）。
func normalizeUIAssetPath(p string) (string, bool) {
	s := strings.TrimSpace(p)
	if s == "" || strings.ContainsAny(s, `\:`) || strings.HasPrefix(s, "/") {
		return "", false
	}
	s = strings.TrimPrefix(s, uiAssetRoot+"/")
	if s == "" || s == "." || strings.HasPrefix(s, "/") || strings.Contains(s, "..") {
		return "", false
	}
	clean := path.Clean(s)
	if clean != s || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}
