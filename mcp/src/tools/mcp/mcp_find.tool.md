# 基本信息
```yaml
name: mcp_find
title: 语义检索工具与能力资产
async: sync
category: meta
hot: true
runtime: plugin
idempotent: true
output: stdout
timeout: 120
touch_files: false
```

# 摘要
按**检索意图**在可用**工具 / 技能 / 提示 / 资源**中做 LLM 语义检索，返回最相关条目的**全量定义**（工具含 `description + input_schema`，topN 小所以直接带定义，省掉 `mcp_load` 二次往返）。

- `query`：检索关键词/目标（可选；与 `match` 至少给出一个才走语义检索）。与 `match` 同时给出时一并交给检索 LLM；本地回退路径按**搜索引擎式表达式**匹配（多词默认 OR、`"短语"` 精确、`-词` 排除）。
- `match`：检索目的/意图（可选）。语义检索的主输入；与 `query` 同时给出时一并交给检索 LLM。两者都空 = 按 type 列出前 `limit` 条。
- 检索模型 = usr `llm.toolfind`（空值回落 `defaultLLM`）；**未配置任何可用模型 / LLM 调用失败 → 自动回退本地表达式匹配**（同 `mcp_match` 逻辑），返回 `mode: "local"` 标注。
- **一步/两步自适应**（按参与检索的总条数，阈值 = 插件配置 `two-step-threshold`，默认 300）：
  - 总量小 → 一步：全候选（一行式概述）交检索 LLM 直接选 topN；
  - 总量大 → 两步：先按**域**（MCP server / 插件来源分组）选域，再在选中域内选条目。
- `type`：`tool`（默认）/`skill`/`prompt`/`resource`/`all`。`skill` 与 `prompt` 共用同一来源，按条目自报类型区分。
- `limit`：topN 上限，缺省 20（插件配置可调），上限 200；LLM 模式建议 ≤5（全量定义随条数线性耗 token）。
- 返回 `count`（本次条数）、`total`（候选总数）、`truncated`、`mode`（`llm` = 一步语义检索；`llm-two-step` = 两步语义检索；`local` = 本地表达式回退）与 `entries[]`；LLM 模式 `entries[]` 条目为**全量定义**（工具含 `server/hot/input_schema`，资源含 `uri/mimeType`），本地回退条目只有概述（不含 `input_schema`，定义用 `mcp_load`）。
- **概述来源**：第三方 MCP 注册/「加载工具」时对 tool / prompt 生成的 LLM 概述（存库，检索 LLM 依据它选条目），或人工写好的描述；resources 协议自带 name/description。概述只含**作用 + 输入/输出**，不含详细 schema 与长示例（那些在 `mcp_load` / find 全量返回里）。

检索面是宿主已注册的全局能力面（含当前实例与其它已打开实例）；`mcp_invoke` 仍会按当前 `work_dir` 的工具面重新准入。宿主列表读取失败时返回错误（不再静默返回空结果）。

# 参数
```json
{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "检索关键词/目标；本地回退路径按搜索引擎式表达式匹配（多词默认 OR、\"短语\" 精确、-词排除，匹配 name/description，忽略大小写）"
    },
    "match": {
      "type": "string",
      "description": "检索目的/意图（可选）；语义检索的主输入（与 query 一并交给检索 LLM 选 topN）"
    },
    "type": {
      "type": "string",
      "enum": ["tool", "skill", "prompt", "resource", "all"],
      "default": "tool",
      "description": "检索类别：tool（默认）/skill/prompt/resource/all；skill 与 prompt 共用同一来源"
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 200,
      "default": 20,
      "description": "topN 上限；缺省 20（插件配置可调），上限 200；LLM 模式建议 ≤5（全量定义随条数线性耗 token）"
    }
  }
}
```

# 返回值
```json
{
  "ok": true,
  "type": "tool",
  "mode": "llm",
  "count": 2,
  "total": 137,
  "truncated": true,
  "entries": [
    {
      "name": "file_read",
      "kind": "tool",
      "type": "tool",
      "description": "读取文本文件",
      "server": "file",
      "hot": true,
      "input_schema": { "type": "object", "properties": {} }
    },
    {
      "name": "code_search",
      "kind": "tool",
      "type": "tool",
      "description": "在代码库中语义检索相关代码",
      "server": "codegraph",
      "hot": true,
      "input_schema": { "type": "object", "properties": { "query": { "type": "string" } } }
    }
  ]
}
```