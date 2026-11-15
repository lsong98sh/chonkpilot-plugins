# 基本信息
```yaml
name: session_open
title: 打开会话窗口
args: session_open --input={RAW-INPUT-FILE}
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
在用户界面上**打开（或激活）某个会话的对话窗口**。用于 `session_create` 之后让用户直接看到
结果，或用户说"把那个会话打开"。窗口已打开 → 激活既有窗口（不新建）；达到窗口上限 →
`window_open_failed`。

`session_id` 省略 = **本会话**；`"@top"` = 用户当前正在看的顶层对话。

# 参数
```json
{
  "type": "object",
  "properties": {
    "session_id": {
      "type": "string",
      "description": "会话 id；省略 = 本会话；\"@top\" = 当前顶层对话"
    }
  }
}
```

# 返回值
```json
{ "ok": true, "window_id": "w-…", "session_id": "s-…", "dialog_id": "…" }
```

`unsupported` = 当前宿主无 GUI 窗口能力（headless）；`not_found` = 会话不存在；
`window_open_failed` = 窗口上限或会话不可用。