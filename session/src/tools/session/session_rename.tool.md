# 基本信息
```yaml
name: session_rename
title: 会话改名
args: session_rename --input={RAW-INPUT-FILE}
async: sync
category: session
hot: true
runtime: plugin
idempotent: true
output: stdout
timeout: 30
touch_files: false
```

# 摘要
修改会话标题。用于 `session_create` 之后给人可辨识的名字，或用户说"把这个会话改名叫…"。
只支持**当前用户**的会话；标题 ≤ 128 字符、不含控制字符。

# 参数
```json
{
  "type": "object",
  "required": ["title"],
  "properties": {
    "session_id": {
      "type": "string",
      "description": "会话 id；省略 = 本会话；\"@top\" = 当前顶层对话"
    },
    "title": {
      "type": "string",
      "description": "新标题（必填，≤128 字符）"
    }
  }
}
```

# 返回值
```json
{ "accepted": true, "topic": "data-session-title" }
```

标题写入是**异步生效**的（宿主命令面）；返回 `accepted=true` 表示已受理，不保证下一次读取
立即可见。`args_invalid` = 标题为空 / 超长 / 含控制字符；`not_found` = 会话未索引到所在对话。
`dialog_id` 由宿主经会话元数据反查（LLM 无需、也无法提供）。