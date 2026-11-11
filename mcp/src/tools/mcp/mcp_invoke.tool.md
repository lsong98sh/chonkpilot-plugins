# 基本信息
```yaml
name: mcp_invoke
title: 调用一个工具
async: sync
category: meta
hot: true
runtime: plugin
idempotent: false
output: stdout
timeout: 1860
on_timeout: fail
```

# 摘要
按名称**同步调用**一个已装配工具，并返回该工具的真实结果。

- `name` 必须是宿主暴露的工具名，且必须属于当前 `work_dir` 已打开实例的已装配工具面；`work_dir` 取自本次调用上下文，无需传入。
- `arguments` 必须是 JSON 对象（缺省 = `{}`；字符串/数组/null 会返回参数错误，**不会**静默清空后执行）。
- `timeoutSec`：本次同步等待上限（秒，5–1800，缺省 120）；目标工具自己的超时策略仍由宿主按其契约裁决。本工具契约 `timeout: 1860`，保证 1800 秒的等待能被外层容纳。
- 调用上下文会透传 `instance_id/session/turn/tool_call_id/display_name`，内层任务可正确归属与展示。
- **目标失败即失败**：目标返回 `isError` / 超时 / 取消时，本工具也返回错误，不会把 `{accepted:true}` 之类的受理回执当成结果。
- 目标若为异步工具（契约 `async`），返回 `status=pending|awaiting` 与 `task_id`；继续跟踪请用 `task_status`。
- **结果上限 1MiB**：目标结果（含失败错误文本）超过 1MiB 时返回 `truncated:true` 与 `total_bytes`，正文被截断（避免元调用把上下文打爆）；需要完整内容请直接调用目标工具或缩小参数。
- **禁止自调用**：`name=mcp_invoke` 会被直接拒绝（避免递归与绕过场景工具白名单）。

# 参数
```json
{
  "type": "object",
  "required": ["name"],
  "properties": {
    "name": {
      "type": "string",
      "description": "目标工具名（宿主暴露名；必须属于当前 work_dir 的已装配工具面；不能是 mcp_invoke）"
    },
    "arguments": {
      "type": "object",
      "description": "目标工具的参数对象（缺省 {}；必须是 JSON 对象）"
    },
    "timeoutSec": {
      "type": "integer",
      "minimum": 5,
      "maximum": 1800,
      "default": 120,
      "description": "同步等待上限（秒）；缺省 120，超出范围按 5/1800 收敛"
    }
  }
}
```

# 返回值
```json
{
  "ok": true,
  "tool": "file_read",
  "status": "complete",
  "result": "……目标工具的真实结果……"
}
```

异步目标返回（示意）：

```json
{
  "ok": true,
  "tool": "task_run",
  "status": "pending",
  "result": { "status": "pending", "task_id": "ex-0042" }
}
```

结果超过 1MiB 时：

```json
{
  "ok": true,
  "tool": "file_read",
  "status": "complete",
  "result": "……截断后的正文……（结果超过 1048576 字节，已截断；完整内容请直接调用目标工具或缩小范围）",
  "truncated": true,
  "total_bytes": 5242880
}
```