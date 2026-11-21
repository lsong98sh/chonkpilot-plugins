# 基本信息
```yaml
name: mcp_match
title: 本地表达式检索能力资产
async: sync
category: meta
hot: true
runtime: plugin
idempotent: true
output: stdout
timeout: 60
touch_files: false
```

# 摘要
在可用**工具 / 技能 / 提示 / 资源**中做**本地表达式检索**（不走 LLM），返回条目的名称、类别与概述（**不含 `input_schema`**；定义用 `mcp_load` 读取）。`mcp_find` 的 LLM 不可用回退路径同此逻辑。

- `query`：**搜索引擎式表达式**——多词默认 OR、`"短语"` 精确匹配、`-词` / `-"短语"` 排除；词级匹配（name/description），按匹配度排序（命中名称权重高于描述，短语再加权）。空 = 不过滤。
- `type`：`tool`（默认）/`skill`/`prompt`/`resource`/`all`。`skill` 与 `prompt` 共用同一来源。
- `limit`：缺省 20（可在插件配置中调整），上限 200。`type=all` 时在 tool → prompt → resource 之间**轮转取用**。
- 返回 `count`（本次条数）、`total`（过滤后总命中数）、`truncated`（是否因 limit 截断）与 `entries[]`；`entries[]` 含 `name/kind/type/description`（描述 = 概述优先，无概述时用原描述），工具另有 `server/hot`，资源另有 `uri/mimeType`。

检索面是宿主已注册的全局能力面（含当前实例与其它已打开实例）；`mcp_invoke` 仍会按当前 `work_dir` 的工具面重新准入。宿主列表读取失败时返回错误（不静默返回空结果）。

# 参数
```json
{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "搜索引擎式检索表达式：多词默认 OR（任一命中即保留）；\"短语\" 精确匹配；-词 / -\"短语\" 排除（命中排除项的条目被剔除）。匹配名称与描述，忽略大小写；空 = 不过滤"
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
      "description": "最多返回条数；缺省 20（插件配置可调），上限 200；超出时返回 truncated=true 与 total"
    }
  }
}
```

# 返回值
```json
{
  "ok": true,
  "type": "tool",
  "count": 2,
  "total": 2,
  "truncated": false,
  "entries": [
    {
      "name": "file_read",
      "kind": "tool",
      "type": "tool",
      "description": "读取文本文件",
      "server": "file",
      "hot": true
    }
  ]
}
```