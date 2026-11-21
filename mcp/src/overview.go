// overview.go —— 能力资产概述（mcp_find 渐进披露）的生成与存取。
//
// 职责（用户确认的设计）：
//   - **概述存数据库**（插件私有存储 storage.self，`<DataDir>/plugins/mcp/storage/overviews.json`，
//     与 config.go 的 usr 插件配置同为宿主管理的落点，不另造存储通道）；
//   - 第三方 MCP 注册 / 「加载工具」时对 **tool 和 prompt** 生成概述；resources 协议自带
//     name/description，不需要 LLM 概述；插件注册的工具概述保持现状（契约 description 人工
//     写好直接用，只有描述过简才 LLM 补全）；
//   - 概述内容 = **作用 + 输入/输出是什么**；不含详细 schema 与长示例（留在 mcp_load /
//     find 全量返回里）；
//   - 概述生成模型 = usr `llm.toolSummary`（空值回落 `defaultLLM`，读法对齐 llm.<子系统> 键
//     机制，每次现读热生效）；模型不可用 → 跳过（用原描述，下次再试）。
//
// 触发链路（两条入口共用同一生成器）：
//  1. 订阅 `data-mcp-refresh`（第三方 MCP 注册/保存/加载工具勾选后广播）→ 后台补生成；
//  2. 前端 EditMCPDialog「加载工具」后直接调插件动作 `overview.generate`（fire-and-forget，
//     立即返回 {ok, started}）。
package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

// overviewStoreKey 是概述文档在 storage.self 的键（ASCII，符合 [A-Za-z0-9._-]{1,64}）。
const overviewStoreKey = "overviews"

// overviewDoc 是概述文档形状：{(kind, name): 概述}。
// 键形如 "tool:github_create_issue"（冒号分隔；name 含任意字符，作为 map 键不受 KV 键字符集限制）。
type overviewDoc map[string]string

// overviewTimeout 是单次概述生成批次的宿主调用 + LLM 总预算（安全阀；批次内逐条 LLM）。
const overviewTimeout = 10 * time.Minute

// overviewMu 防并发去抖：同一时刻只跑一个生成批次（触发可能连续到达；后续触发排队复用结果）。
var overviewMu sync.Mutex

// overviewEntryKey 返回概述文档键。
func overviewEntryKey(kind, name string) string {
	return kind + ":" + name
}

// overviewOf 返回某条目的概述（没有 → ""，调用方回落原描述）。
func (o overviewDoc) overviewOf(kind, name string) string {
	return o[overviewEntryKey(kind, name)]
}

// loadOverviews 读概述文档（读失败 / 无文档 → 空 map，不报错：概述只是缓存性增强）。
func (m *mcpPlugin) loadOverviews(ctx context.Context, instanceID string) overviewDoc {
	if instanceID == "" {
		return overviewDoc{}
	}
	raw, err := m.p.DataRead(ctx, "storage.self", "get", instanceID, map[string]any{"key": overviewStoreKey})
	if err != nil {
		return overviewDoc{}
	}
	// 桥应答 = `{ok, result:{found, key, value}}`，UnwrapMap 剥壳后取 found/value。
	res := pluginsdk.UnwrapMap(raw)
	found, _ := res["found"].(bool)
	value, _ := res["value"].(map[string]any)
	if !found || value == nil {
		return overviewDoc{}
	}
	out := overviewDoc{}
	for k, v := range value {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// saveOverviews 写概述文档（读-改-写；写入失败只记日志）。
func (m *mcpPlugin) saveOverviews(ctx context.Context, instanceID string, doc overviewDoc) error {
	if instanceID == "" {
		return nil
	}
	payload := map[string]any{}
	for k, v := range doc {
		payload[k] = v
	}
	_, err := m.p.DataWrite(ctx, "storage.self", "set", instanceID, map[string]any{
		"key": overviewStoreKey, "value": payload,
	})
	return err
}

// ─── 触发入口 ─────────────────────────────────────────────────────

// handleMCPRefresh 订阅 data-mcp-refresh：第三方 MCP 注册/保存/「加载工具」落库后广播，
// 触发一次后台概述补全（instance_id 从事件载荷透传，storage.self 不消费它、仅作桥必填字段）。
func (m *mcpPlugin) handleMCPRefresh(_ context.Context, _ string, payload []byte) {
	var evt struct {
		InstanceID string `json:"instance_id"`
	}
	_ = json.Unmarshal(payload, &evt)
	go m.ensureOverviews(evt.InstanceID)
}

// handleOverviewGenerate 是插件动作 `overview.generate`（EditMCPDialog「加载工具」后 fire-and-forget）：
// 立即返回 {ok, started}，生成在后台执行（LLM 生成可能分钟级）。
func (m *mcpPlugin) handleOverviewGenerate(_ context.Context, payload map[string]any) (any, error) {
	instanceID := pluginsdk.Str(payload["instance_id"])
	go m.ensureOverviews(instanceID)
	return map[string]any{"ok": true, "started": true}, nil
}

// ensureOverviews 后台补全概述（去抖：并发触发只跑一个批次）。
func (m *mcpPlugin) ensureOverviews(instanceID string) {
	overviewMu.Lock()
	defer overviewMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), overviewTimeout)
	defer cancel()
	instanceID = m.resolveInstanceID(ctx, instanceID)
	if instanceID == "" {
		return // 无活动实例（桥 data 面必填 instance_id）→ 下次触发再试
	}
	if _, err := m.generateOverviews(ctx, instanceID); err != nil {
		m.p.Logger().Warn("mcp: 概述生成失败（保留既有概述）：" + err.Error())
	}
}

// resolveInstanceID 归一实例 id：事件/动作带就用；不带 → 取当前活动实例之一
// （plugin/v1/instance.list）。插件桥数据面要求 instance_id 非空。
func (m *mcpPlugin) resolveInstanceID(ctx context.Context, given string) string {
	if strings.TrimSpace(given) != "" {
		return strings.TrimSpace(given)
	}
	raw, err := m.p.Call(ctx, "plugin/v1/instance.list", map[string]any{})
	if err != nil {
		return ""
	}
	var res struct {
		Instances []string `json:"instances"`
	}
	if json.Unmarshal(raw, &res) != nil || len(res.Instances) == 0 {
		return ""
	}
	return res.Instances[0]
}

// ─── 生成主体 ─────────────────────────────────────────────────────

// overviewLLMRequestLimit 是单批次概述 LLM 请求条数上限（防一次注册把上下文打爆；超出下一批继续）。
const overviewLLMRequestLimit = 20

// generateOverviews 读工具/提示清单 → 找出**第三方 MCP** 中缺概述的 tool/prompt → LLM 生成概述
// （llm.toolSummary，空回落 defaultLLM）→ 落库。返回新增条数。
//
// 归属判定：工具按 `_meta.server.alias`（entryFromTool 的 server）、提示按 node（entryFromPrompt）；
// 只有命中在册 MCP（config.mcp.list）的条目才生成——插件/内置工具的契约描述人工写好，保持现状
// （用户口径：插件注册的工具概述不强制 LLM）。resources 协议自带 name/description，不生成。
func (m *mcpPlugin) generateOverviews(ctx context.Context, instanceID string) (int, error) {
	if instanceID == "" {
		return 0, nil // 桥 data.read 必填 instance_id（storage.self 亦经桥校验）；无实例身份就跳过
	}
	doc := m.loadOverviews(ctx, instanceID)

	// 候选：tool（全量、含非 hot——概述按注册面生成，不受检索过滤影响）+ prompt。
	tools, err := m.listTools(ctx, true)
	if err != nil {
		return 0, err
	}
	prompts, perr := m.promptEntries(ctx, "prompt")
	if perr != nil {
		prompts = nil // 提示列表读失败不阻塞工具概述
	}

	// 第三方 server 名集合：经 config.mcp.list 取在册 MCP。
	third := m.thirdPartyServers(ctx)
	if len(third) == 0 {
		return 0, nil
	}

	type pending struct{ kind, name, desc string }
	var jobs []pending
	addIf := func(kind, name, desc, server string) {
		if name == "" || server == "" {
			return
		}
		if _, ok := third[server]; !ok {
			return // 非第三方（内置/插件/目录节点）→ 保持现状
		}
		if len(doc[overviewEntryKey(kind, name)]) > 0 {
			return // 已有概述
		}
		jobs = append(jobs, pending{kind: kind, name: name, desc: desc})
	}
	for _, e := range tools {
		addIf("tool", e.Name, e.Description, e.Server)
	}
	for _, e := range prompts {
		addIf(e.Kind, e.Name, e.Description, e.Server)
	}

	generated := 0
	if len(jobs) > 0 {
		// 概述模型：usr llm.toolSummary（数据层读侧已回落 defaultLLM）。
		model := m.modelName(ctx, instanceID, "llm.toolSummary")
		if model == "" {
			// 模型不可用 → 不落概述（检索用原描述）；待配置模型后由下次触发补生成。
			return 0, nil
		}
		// 分批生成（单批 overviewLLMRequestLimit 条，逐条 LLM；ctx 预算耗尽即停，余量留待下次触发）。
		for len(jobs) > 0 && ctx.Err() == nil {
			batch := jobs
			if len(batch) > overviewLLMRequestLimit {
				batch = batch[:overviewLLMRequestLimit]
			}
			jobs = jobs[len(batch):]
			for _, j := range batch {
				overview, err := m.llmOverview(ctx, instanceID, model, j.kind, j.name, j.desc)
				if err != nil {
					continue // 单条失败跳过（下次触发再试）
				}
				doc[overviewEntryKey(j.kind, j.name)] = overview
				generated++
			}
			if err := m.saveOverviews(ctx, instanceID, doc); err != nil {
				return generated, err
			}
		}
		return generated, nil
	}
	return generated, nil
}

// thirdPartyServers 经 plugin/v1/config.mcp.list 取在册第三方 MCP server 名集合（读失败 → 空）。
func (m *mcpPlugin) thirdPartyServers(ctx context.Context) map[string]struct{} {
	raw, err := m.p.Call(ctx, "plugin/v1/config.mcp.list", map[string]any{})
	if err != nil {
		return nil
	}
	var res struct {
		List []map[string]any `json:"list"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return nil
	}
	out := map[string]struct{}{}
	for _, item := range res.List {
		name := pluginsdk.Str(item["name"])
		if name != "" {
			out[name] = struct{}{}
		}
	}
	return out
}

// llmOverview 用概述模型为一条资产生成概述（plugin/v1/llm.step，非流式单步）。
// prompt 要求：作用 + 输入/输出是什么；不含 schema 与示例；原文语言；一段话。
func (m *mcpPlugin) llmOverview(ctx context.Context, instanceID, model, kind, name, desc string) (string, error) {
	system := "You are a tool-catalog summarizer. Given a tool/prompt name and its terse description, write ONE concise sentence in the original language covering: what it does (作用) and what input it takes / what output it returns (输入/输出). Do NOT include parameter schemas, examples, or code. Output ONLY the sentence."
	prompt := "Kind: " + kind + "\nName: " + name
	if strings.TrimSpace(desc) != "" {
		prompt += "\nDescription: " + desc
	}
	raw, err := m.p.CallWithTimeout(ctx, 90*time.Second, "plugin/v1/llm.step", map[string]any{
		"prompt":      prompt,
		"system":      system,
		"instance_id": instanceID,
		"llm":         model,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		Text   string `json:"text"`
		Result struct {
			Text string `json:"text"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	text := strings.TrimSpace(out.Text)
	if text == "" {
		text = strings.TrimSpace(out.Result.Text)
	}
	if text == "" {
		return "", errEmptyLLM
	}
	if len([]rune(text)) > 400 { // 概述要求一段话；异常长输出截到 400 字（概述本来就是短的）
		text = string([]rune(text)[:400])
	}
	return text, nil
}

// errEmptyLLM 区分「模型返回空文本」与调用失败。
var errEmptyLLM = errEmpty{}

type errEmpty struct{}

func (errEmpty) Error() string { return "llm: 返回空文本" }

// modelName 取子系统模型 provider 名（usr llm.<子系统> → 回落 defaultLLM → llms 折算）。
// 读法对齐 memory 插件 subsystemLLM（data-user-config-load 一次现读；不缓存 → 热生效）：
// 值 = llmref（name 字符串 / 旧 int 索引 / 数据层已回落的 defaultLLM 值）。
func (m *mcpPlugin) modelName(ctx context.Context, instanceID, key string) string {
	if instanceID == "" {
		return ""
	}
	raw, err := m.p.DataRead(ctx, "config", "usr.get", instanceID, nil)
	if err != nil {
		return ""
	}
	// 桥应答 = `{ok, result:{data:<usr 配置视图>}}`（data-user-config-load 的 DataPayload）。
	res := pluginsdk.UnwrapMap(raw)
	data, _ := res["data"].(map[string]any)
	if data == nil {
		return ""
	}
	return llmRefName(data[key], data["llms"])
}

// llmRefName 把 llmref 取值折算为 provider name（口径对齐主仓 data.LLMRefName：字符串原样；
// 旧 int 索引 → llms[idx].name，越界/负数回落下标 0；lls 空 → ""）。
func llmRefName(v any, llms any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	idx := 0
	switch n := v.(type) {
	case int:
		idx = n
	case int64:
		idx = int(n)
	case float64:
		idx = int(n)
	}
	switch list := llms.(type) {
	case []any:
		if len(list) == 0 {
			return ""
		}
		if idx < 0 || idx >= len(list) {
			idx = 0
		}
		m, _ := list[idx].(map[string]any)
		name, _ := m["name"].(string)
		return strings.TrimSpace(name)
	case []map[string]any:
		if len(list) == 0 {
			return ""
		}
		if idx < 0 || idx >= len(list) {
			idx = 0
		}
		name, _ := list[idx]["name"].(string)
		return strings.TrimSpace(name)
	}
	return ""
}
