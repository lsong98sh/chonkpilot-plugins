# 基本信息
```yaml
name: next_run
title: next_run
args: next_run --input={RAW-INPUT-FILE}
async: never
category: task
hot: true
output: stdout
runtime: plugin
timeout: 0
```

# 摘要
把**下一轮要执行的提示词**登记为排队输入：本轮结束后系统会自动起一个新轮次执行它（类似排队输入，与「继续」不同——「继续」只在本轮内追加）。适用场景：
- **输出 token 预算即将耗尽（约不足 5%）但任务尚未完成**；
- **一个阶段已完成、且明确还有下一阶段**（分段交付）。

`prompt` 必须是**下一轮的完整提示词**（自成一体：目标、已知进展、约束、验收标准），不要依赖本轮未输出的内容。调用成功后本轮请直接收尾并简要说明已登记下一轮，**不要重复输出下一轮内容**。同一轮多次调用以**最后一次**为准（覆盖前一条，计数按次累加）。到达连续自动接续上限时返回 `已自动接续 N 次，需用户判断是否继续`——此时请收尾并提示用户。仅主会话可用（子聊天 / 群聊 / 子会话由各自调度器驱动）。

**配额与丢弃**：连续自动接续默认上限 30 次（项目配置 `nextRunMax`，0 = 关闭；关闭时本工具不出现）。用户发新消息/插话会重置计数。若本轮最终以 error / interrupted / cancelled 收尾，本轮登记的排队项会被系统**丢弃**，不会自动执行——需要用户重新发起。

# 参数
```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "prompt": {
      "type": "string",
      "maxLength": 16384,
      "description": "下一轮要执行的完整提示词（自成一体；包含目标、已知进展、约束与验收标准）"
    }
  },
  "required": [
    "prompt"
  ]
}
```

# 返回值
- 成功登记 / 同轮覆盖：`已登记下一轮（第 N 次，上限 M）`（`isError=false`；N = 本次受理后的连续自动接续次数，M = 上限 `nextRunMax`；本轮结束后自动起新轮）。
- 到达上限：`已自动接续 N 次，需用户判断是否继续`（`isError=false`；本次未登记，已登记项保留）。
- 未启用：`next_run 未启用（自动接续上限为 0）`（`isError=false`；正常情况下工具已被移出工具面）。
- 参数 / 会话 / 数据失败：`isError=true` 的工具错误——`prompt 必填`、`prompt 超长（N 字符，上限 16384）`、`next_run 仅支持主会话（…）`、`错误: 登记下一轮失败（…）`。
- 本轮 error/interrupted/cancelled：已登记项被置 `discarded`，无额外 tool 结果，模型不会再看到该 prompt。
