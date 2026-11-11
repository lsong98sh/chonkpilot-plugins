# 基本信息
```yaml
name: task_llm
title: 委派子代理
args: task_llm --input={RAW-INPUT-FILE}
async: never
category: task
hot: true
runtime: plugin
idempotent: false
output: stdout
timeout: 600
```

# 摘要
委派一个**全能力子代理**：它拥有独立子会话 / 子轮次与完整工具面（可按 `agent` 定义收窄），
可读写文件、执行命令、调用其它工具，并可再委派下一级。返回子代理的最终回复；长任务可
`detach=true` 后经 `task_status(task_id=...)` 取结果与已产出内容。API Key 由宿主持有，插件不可见。

# 参数
```json
{
  "type": "object",
  "required": ["prompt"],
  "properties": {
    "prompt": {
      "type": "string",
      "description": "交给子代理的任务描述（必填）"
    },
    "agent": {
      "type": "string",
      "description": "可选：场景内 agent 名（收窄提示词 / 工具白名单 / LLM）；缺省 = 全能力"
    },
    "session_id": {
      "type": "string",
      "description": "可选：续用既有子会话 id（多轮委派同一子代理）；缺省新建"
    },
    "title": {
      "type": "string",
      "description": "可选：任务节点展示名（缺省取 prompt 首行）"
    },
    "waitSec": {
      "type": "integer",
      "minimum": 0,
      "maximum": 600,
      "description": "最多等待秒数；0/缺省 = 等到收尾（受 600s 上限保护，超长任务请用 detach）。到期返回 state=running"
    },
    "detach": {
      "type": "boolean",
      "description": "true = 立即返回 running（后台继续），用 task_status 取结果"
    }
  }
}
```

# 返回值
```json
{
  "ok": true,
  "session_id": "sub-…",
  "turn_id": "…",
  "task_id": "tk-…",
  "state": "done",
  "text": "子代理最终回复"
}
```

`state=running`（detach / 等待超时）**不返回正文**：用 `task_status(task_id=…)` 取终态结果。

等待上限：`waitSec` 到期即返回 `state=running`；`waitSec=0/缺省` 最多等到 600s。需要更长任务时用
`detach=true` 立即拿 `task_id`，再用 `task_status` 轮询，不要在单次调用里长时间阻塞。

`state=error` 且 `code=LLM_INCOMPLETE` 表示子代理输出不完整（截断）；`partial` 仅为已产出的
非终态内容，**不得当作最终回复**。子代理被取消时为 `state=cancelled`。只有 `state=done`
且带 `text` 才是完整结果。