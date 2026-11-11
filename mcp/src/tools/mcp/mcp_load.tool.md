# 基本信息
```yaml
name: mcp_load
title: 加载工具/技能/资源定义
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
按名称加载一个条目的完整定义（只读、实时读取）。

- 工具（`kind=tool`）：返回 `input_schema`，可直接作为 `mcp_invoke` 的 `arguments` 依据。
- 技能/提示（`kind=skill|prompt`）：返回正文 `content`（同时保留 `text` 兼容字段）。
- 资源（`kind=resource`）：按 `uri`（缺失时用名称）读取，返回 `content`，可选 `mimeType`。
- 同名时**工具优先**，其次是提示/技能，最后是资源。
- 名称大小写敏感、精确匹配；未找到时返回错误。
- **加载不受检索配置影响**：按名称查工具是**全量**的（即使插件关闭了「检索包含非 hot 工具」，也能加载非 hot 工具）。
- 宿主列表/详情读取失败时返回明确错误（不会伪装成“未找到条目”）。
- 可加载**当前实例作用域**（项目/用户级能力节点）下的技能与资源：读取会带上本次调用上下文的 `instance_id`。

# 参数
```json
{
  "type": "object",
  "required": ["name"],
  "properties": {
    "name": {
      "type": "string",
      "description": "条目名称：工具名 / 技能或提示名 / 资源名或 uri（大小写敏感、精确匹配）"
    }
  }
}
```

# 返回值
```json
{
  "ok": true,
  "kind": "skill",
  "type": "skill",
  "name": "代码审查",
  "description": "按清单审查变更",
  "content": "……正文……",
  "text": "……正文……"
}
```