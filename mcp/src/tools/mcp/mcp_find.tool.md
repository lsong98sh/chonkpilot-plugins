# 基本信息
```yaml
name: mcp_find
title: 检索工具与能力资产
async: sync
category: meta
hot: true
runtime: plugin
idempotent: true
output: stdout
timeout: 60
on_timeout: fail
```

# 摘要
在可用**工具 / 技能 / 提示 / 资源**中检索，返回条目的名称、类别与用途（不返回正文；正文/Schema 用 `mcp_load` 读取）。

- `query`：名称/描述的子串匹配，忽略大小写；空 = 不过滤。
- `match`：检索目的/意图（可选）；按空格分词后**加权排序**（命中名称权重高于描述），不会因为不命中而丢弃条目。
- `type`：`tool`（默认）/`skill`/`prompt`/`resource`/`all`。`skill` 与 `prompt` 共用同一来源，按条目自报类型区分。
- `limit`：缺省 20（可在插件配置中调整），上限 200。`type=all` 时在 tool → prompt → resource 之间**轮转取用**，避免工具先占满额度。
- 返回 `count`（本次条数）、`total`（过滤后总命中数）、`truncated`（是否因 limit 截断）与 `entries[]`；`entries[]` 含 `name/kind/type/description`，工具另有 `server/hot/input_schema`，资源另有 `uri/mimeType`。

检索面是宿主已注册的全局能力面（含当前实例与其它已打开实例）；`mcp_invoke` 仍会按当前 `work_dir` 的工具面重新准入。宿主列表读取失败时返回错误（不再静默返回空结果）。

# 参数
```json
{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "关键词；名称/描述的子串匹配，忽略大小写。空 = 列出该类型全部（受 limit 限制）"
    },
    "match": {
      "type": "string",
      "description": "检索目的/匹配意图（可选）；按空格分词后参与加权排序（命中名称优先于描述），不丢弃未命中的条目"
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
  "type": "all",
  "count": 3,
  "total": 12,
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
      "name": "代码审查",
      "kind": "skill",
      "type": "skill",
      "description": "按清单审查变更"
    },
    {
      "name": "kb://doc/1",
      "kind": "resource",
      "type": "resource",
      "description": "项目知识库文档",
      "uri": "kb://doc/1",
      "mimeType": "text/markdown"
    }
  ]
}
```