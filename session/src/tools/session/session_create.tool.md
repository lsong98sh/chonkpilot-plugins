# 基本信息
```yaml
name: session_create
title: 新建顶层会话
args: session_create --input={RAW-INPUT-FILE}
async: auto
category: session
hot: true
runtime: plugin
idempotent: false
output: stdout
timeout: 600
touch_files: true
```

# 摘要
新建一个**顶层对话**（新 dialog，出现在会话列表里），并**自动跑首轮**。用于用户说
"把这三件事分解成三个 session"：每个需求调一次，得到三个可以各自继续的独立对话。

返回 `session_id` / `dialog_id` / `turn_id` / `task_id`。默认**异步**：`state=running`，
之后用 `task_status(task_id=…)` 取结果；`wait_sec>0` 可同步等待到终态（上限 600s）。

三个会话**共享同一个项目工作区**：并发跑首轮可能同时改同一批文件。除非用户明确要求并行，
建议一次只创建一个、等它完成再建下一个。

不要用它做"委派子任务"——那用 `task_llm`（子代理，不产生新的顶层对话）。

# 参数
```json
{
  "type": "object",
  "required": ["prompt"],
  "properties": {
    "prompt": {
      "type": "string",
      "description": "该会话首轮的任务描述（必填）"
    },
    "title": {
      "type": "string",
      "description": "会话标题（建议给可辨识的名字，如「登录模块重构」）；缺省取 prompt 首行"
    },
    "agent": {
      "type": "string",
      "description": "可选：场景内 agent 名（收窄提示词 / 工具白名单 / LLM）；缺省 = 全能力"
    },
    "scenario_id": {
      "type": "string",
      "description": "可选：场景 id；缺省 = 当前轮次场景"
    },
    "wait_sec": {
      "type": "integer",
      "minimum": 0,
      "maximum": 600,
      "description": "0/缺省 = 立即返回 running；>0 = 最多等该秒数后返回终态"
    }
  }
}
```

# 返回值
```json
{
  "ok": true,
  "state": "running",
  "session_id": "split-…",
  "dialog_id": "…",
  "turn_id": "…",
  "task_id": "tk-…",
  "message": "会话已启动；结果请用 session_status(task_id=…) 或 task_status 获取"
}
```

`state=done` 时另带 `text`（首轮最终回复）。`session_busy` = 并发会话已达上限（默认 3 个）
或同会话已有轮次；`agent_not_found` = agent 名不存在；`instance_not_found` = 项目未打开。

# 注意
- 新会话是**顶层对话**，不是当前会话的子会话；不要传 `parent_session_id`（不存在该参数）。
- 会话 id 不能由调用方指定；创建后用返回值里的 `session_id` 继续操作。
- 用户后续要接着聊时，直接用界面打开该会话即可；模型追发消息用 `session_send`。