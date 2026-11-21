// llmfind.go —— mcp_find 的 LLM 语义检索主体（一步 / 两步自适应）。
//
// 用户确认的设计：
//   - 按参与检索的总条数自适应：总量 ≤ two-step-threshold（插件配置，默认 300）→ 一步
//     （全候选一行式概述列表交检索 LLM 直接选 topN）；总量大 → 两步（先按域选域，再在
//     选中域内选条目）；
//   - 域 = 按 MCP server / 插件划分（_meta.server.alias / node 归组，不做语义域分类）；
//   - 返回 topN 条全量定义（tool 含 input_schema；prompt/resource 条目定义 = 正文另行
//     mcp_load，find 返回其概述）；
//   - LLM 输出严格 JSON（{"names":["…"]}）；解析失败 / 调用失败 → ok=false，调用方回退
//     本地表达式（find 语义：尽力返回结果）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// llmFindTimeout 是单次检索 LLM 调用的上限（两步 = 两次调用共用外层 metaToolTimeout）。
const llmFindTimeout = 45 * time.Second

// llmPickTopNMax 是 topN 的绝对上限（全量定义随条数线性耗 token；契约建议 limit ≤5）。
const llmPickTopNMax = 5

// llmFind 执行语义检索（一步/两步自适应）；ok=false = 走本地回退。
func (m *mcpPlugin) llmFind(ctx context.Context, in llmFindInput) (map[string]any, bool) {
	selLimit := in.limit
	if selLimit > llmPickTopNMax {
		selLimit = llmPickTopNMax
	}
	total := len(in.candidates)
	if total > in.twoStepThreshold {
		out, ok := m.llmFindTwoStep(ctx, in, selLimit)
		if ok {
			return out, true
		}
		return nil, false
	}
	out, ok := m.llmFindOneStep(ctx, in, selLimit)
	if !ok {
		return nil, false
	}
	return out, true
}

// llmFindOneStep 一步：全候选概述列表 → 检索 LLM 选 topN。
func (m *mcpPlugin) llmFindOneStep(ctx context.Context, in llmFindInput, selLimit int) (map[string]any, bool) {
	names, ok := m.llmPick(ctx, in.instanceID, in.model, llmPickPrompt{
		kind:     in.kind,
		limit:    selLimit,
		query:    in.query,
		match:    in.match,
		overview: overviewList(in.candidates),
		domains:  nil, // 一步不带域
	})
	if !ok {
		return nil, false
	}
	return m.llmFindResult(in, names, "llm"), true
}

// llmFindTwoStep 两步：先按域选域，再在选中域内选条目。
func (m *mcpPlugin) llmFindTwoStep(ctx context.Context, in llmFindInput, selLimit int) (map[string]any, bool) {
	// 域划分：server alias / node（资源无 server → 按来源节点）。
	domains, byDomain := groupDomains(in.candidates)
	// 第一步：域清单（每域一行 = 域名 + 条数 + 该域概述前 N 条拼样）→ 选域。
	pickedDomains, ok := m.llmPick(ctx, in.instanceID, in.model, llmPickPrompt{
		kind:     in.kind,
		limit:    8, // 域选多少个域：宽口径 8（第二步再精选条目）
		query:    in.query,
		match:    in.match,
		overview: domainSummary(domains, byDomain),
		domains:  domainNames(domains),
	})
	if !ok || len(pickedDomains) == 0 {
		return nil, false
	}
	// 第二步：选中域内全部条目的概述列表 → 选条目。
	var scoped []entry
	seen := make(map[string]struct{}, len(pickedDomains))
	for _, d := range pickedDomains {
		seen[d] = struct{}{}
	}
	for _, d := range domains {
		if _, ok := seen[d.name]; !ok {
			continue
		}
		scoped = append(scoped, byDomain[d.name]...)
	}
	names, ok := m.llmPick(ctx, in.instanceID, in.model, llmPickPrompt{
		kind:     in.kind,
		limit:    selLimit,
		query:    in.query,
		match:    in.match,
		overview: overviewList(scoped),
		domains:  nil,
	})
	if !ok {
		return nil, false
	}
	return m.llmFindResult(in, names, "llm-two-step"), true
}

// llmFindResult 组装 LLM 模式返回：topN 全量定义（tool 含 input_schema）。
// total = 候选总数；truncated = 候选多于返回。名字未命中候选 → 丢弃（不臆造）。
func (m *mcpPlugin) llmFindResult(in llmFindInput, names []string, mode string) map[string]any {
	if len(names) > in.limit {
		names = names[:in.limit]
	}
	byName := make(map[string]entry, len(in.candidates))
	for _, e := range in.candidates {
		byName[e.Name] = e
	}
	entries := make([]entry, 0, len(names))
	for _, n := range names {
		if e, ok := byName[n]; ok {
			entries = append(entries, e) // 全量定义：Schema 保留在 entry 上
		}
	}
	return map[string]any{
		"ok":        true,
		"type":      in.kind,
		"mode":      mode,
		"count":     len(entries),
		"total":     len(in.candidates),
		"truncated": len(in.candidates) > len(entries),
		"entries":   entries,
	}
}

// llmPickPrompt 是选条目 prompt 的输入。
type llmPickPrompt struct {
	kind     string // 请求 type（展示用）
	limit    int    // topN
	query    string
	match    string
	overview []string // 一行式概述列表（一步/第二步 = 条目；第一步 = 域）
	domains  []string // 非空 = 第一步（选域）：概述列表为域行
}

// llmPick 调一次检索 LLM 选出名字列表；ok=false = 调用失败/输出不可解析。
func (m *mcpPlugin) llmPick(ctx context.Context, instanceID, model string, p llmPickPrompt) ([]string, bool) {
	var sb strings.Builder
	sb.WriteString("检索类型：" + p.kind + "\n")
	if p.match != "" {
		sb.WriteString("检索意图：" + p.match + "\n")
	}
	if p.query != "" {
		sb.WriteString("检索关键词：" + p.query + "\n")
	}
	sb.WriteString("\n候选清单（一行一条，格式 = 名称: 概述）：\n")
	for _, line := range p.overview {
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	task := "条目"
	want := "条目名称"
	if len(p.domains) > 0 {
		task = "域"
		want = "域名"
	}
	prompt := fmt.Sprintf(
		"请从候选清单中选出与检索意图/关键词最相关的 %s，至多 %d 个，按相关性排序。"+
			"输出 JSON {\"names\":[\"<%s>\",…]}（无匹配输出 {\"names\":[]}），仅输出 JSON，不要解释。",
		task, p.limit, want)
	lctx, cancel := context.WithTimeout(ctx, llmFindTimeout)
	defer cancel()
	raw, err := m.p.CallWithTimeout(lctx, llmFindTimeout, "plugin/v1/llm.step", map[string]any{
		"prompt":      prompt,
		"system":      sb.String(),
		"instance_id": instanceID,
		"llm":         model,
	})
	if err != nil {
		return nil, false
	}
	var out struct {
		Text   string `json:"text"`
		Result struct {
			Text string `json:"text"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return nil, false
	}
	text := strings.TrimSpace(out.Text)
	if text == "" {
		text = strings.TrimSpace(out.Result.Text)
	}
	if text == "" {
		return nil, false
	}
	names, ok := parsePickedNames(text)
	if !ok {
		return nil, false
	}
	return names, true
}

// parsePickedNames 从 LLM 输出中提取 {"names":[...]}（容忍 ```json 围栏与前后杂文）。
func parsePickedNames(text string) ([]string, bool) {
	text = strings.TrimSpace(text)
	// 截取首个 { 到末个 }。
	if i := strings.Index(text, "{"); i >= 0 {
		if j := strings.LastIndex(text, "}"); j > i {
			text = text[i : j+1]
		}
	}
	var picked struct {
		Names []string `json:"names"`
	}
	if json.Unmarshal([]byte(text), &picked) != nil {
		return nil, false
	}
	out := make([]string, 0, len(picked.Names))
	for _, n := range picked.Names {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out, true
}

// ─── 概述行 / 域划分（纯函数，可单测）─────────────────────────────

// overviewList 渲染一行式概述列表：`- name(kind): 概述`。
func overviewList(list []entry) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, fmt.Sprintf("- %s(%s): %s", e.Name, e.Kind, e.Description))
	}
	return out
}

// domainEntry 是一个检索域（= MCP server / 插件来源分组）。
type domainEntry struct {
	name string
}

// groupDomains 按 server（资源按 uri 前缀外的来源字段缺省 = 按 kind 兜底）划域。
// 返回稳定序域名与 域名 → 条目。
func groupDomains(candidates []entry) ([]domainEntry, map[string][]entry) {
	byDomain := make(map[string][]entry)
	for _, e := range candidates {
		d := e.Server
		if d == "" {
			d = "其他" // 资源/知识库条目无 server 归属
		}
		byDomain[d] = append(byDomain[d], e)
	}
	names := make([]string, 0, len(byDomain))
	for n := range byDomain {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]domainEntry, 0, len(names))
	for _, n := range names {
		out = append(out, domainEntry{name: n})
	}
	return out, byDomain
}

// domainNames 取域名单列表。
func domainNames(domains []domainEntry) []string {
	out := make([]string, 0, len(domains))
	for _, d := range domains {
		out = append(out, d.name)
	}
	return out
}

// domainSummary 渲染两步第一步的域行：`- 域名: N 条 —— 概述样例`（每域至多 8 条拼样）。
func domainSummary(domains []domainEntry, byDomain map[string][]entry) []string {
	const sampleMax = 8
	out := make([]string, 0, len(domains))
	for _, d := range domains {
		list := byDomain[d.name]
		samples := make([]string, 0, sampleMax)
		for i, e := range list {
			if i >= sampleMax {
				break
			}
			samples = append(samples, e.Name)
		}
		out = append(out, fmt.Sprintf("- %s: %d 条 —— %s", d.name, len(list), strings.Join(samples, ", ")))
	}
	return out
}
