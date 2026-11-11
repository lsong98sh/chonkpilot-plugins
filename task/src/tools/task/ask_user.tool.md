# 基本信息
```yaml
name: ask_user
title: ask_user
args: ask_user --input={RAW-INPUT-FILE}
async: sync
category: task
hot: true
output: stdout
runtime: plugin
timeout: 0
on_timeout: background
policy_lock: true
```

# 摘要
向用户做**简单抉择**提问（是/否、少量选项），并**立即返回**「已向用户提问，正在等待回复」的占位结果（非阻塞）——不会在本轮内等待答案。**不要**用于开放式提问、需要长文本回答、或需要连续多轮澄清的问题——那类问题应直接用普通对话逐轮询问。能从上下文推断、或用合理默认值推进的，直接推进，不要调用本工具。

收到占位结果后：
- 可以继续完成其它**可独立完成**的任务；
- 如果暂时没有其它可做的事，直接输出文本「等待用户回复」并结束本轮对话；
- 用户答复会在稍后作为**后续用户消息**送达：主会话在本轮结束后的下一轮送达（`next_turn`）；群聊/子会话若已有运行轮会直接喂入，否则起新一轮；
- **不要臆测用户答案并继续推进**。当前轮的工具结果保持占位文本；历史记录中的同一 `ask_user` 行只会标记为「用户已回复 / 已超时」（答案正文仅出现在后续用户消息里，避免重复）。

# 使用建议
- **适用**：二选一 / 少量选项的确认；提问后不阻塞当前流程。
- **不适用**：开放式问题、需要用户写长文本、需要连续追问几轮才能定的事——改用普通对话逐轮问。
- **选项**：单选（缺省）/ `multi=true` 多选；`options` 为空或未提供时按**自由文本问答**处理（用户可直接输入，等价于「其他」）。有选项时 `custom=true` 会额外提供「其他」输入框；「其他」/自由文本的输入作为该问的答案返回。
- **同一会话可以提出多个问题**；每条答复到达后**立即投递**当前已答内容（并发答复可能合并为一条），不再要求同一会话全部答完——未答问题不会阻塞已答内容。不要让问题在语义上互相依赖。
- 有合理默认值时直接采用默认值并在回复中说明假设，不要为了确认而确认。
- **超时**：`timeout` 未设置 = 使用项目全局 `ask_timeout_sec`（全局也未配置则一直等待）；`0` = 用全局；`-1` = 明确不超时；`>0` = 覆盖全局。到点未答 → 系统自动回复：有 `recommended` 时单选取首个、多选按「、」拼接，否则回复「用户未在时限内回复，请按推荐做法继续」。该自动回复同用户答复一样，作为后续用户消息送达。
- **背景说明**：`context` 可选，用一两句话说明为什么问、影响什么、选项差别；会展示在 Ask 中心右侧详情。

# 参数
```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "question": {
      "type": "string",
      "maxLength": 2000,
      "description": "要问用户的问题（必填，不能为空；建议一两句话）"
    },
    "context": {
      "type": "string",
      "maxLength": 4000,
      "description": "可选：提问背景说明（为什么问、影响什么、选项差别），展示在 Ask 中心详情"
    },
    "options": {
      "type": "array",
      "maxItems": 20,
      "description": "可选项列表（用户可直接点选回答；multi=true 时可多选）。省略或空数组 = 自由文本问答（前端直接提供输入框）。元素为 {value,label}：value=随回答返回、label=展示文本；**也接受纯字符串**（等价 value=label=该字符串）；对象的 value/label 至少一个非空",
      "items": {
        "anyOf": [
          {
            "type": "string",
            "minLength": 1,
            "maxLength": 500
          },
          {
            "type": "object",
            "minProperties": 1,
            "additionalProperties": false,
            "properties": {
              "value": {
                "type": "string",
                "maxLength": 500,
                "description": "选项值（随回答返回）；缺省时回落到 label"
              },
              "label": {
                "type": "string",
                "maxLength": 500,
                "description": "选项展示文本"
              }
            }
          }
        ]
      }
    },
    "multi": {
      "type": "boolean",
      "description": "是否允许多选（默认 false）"
    },
    "custom": {
      "type": "boolean",
      "description": "有 options 时是否额外提供「其他」输入框（默认 false；multi=true 时「其他」输入可与多选并存）。省略 options / 空数组时前端自动按自由文本问答处理，无需设置本字段"
    },
    "recommended": {
      "type": "array",
      "maxItems": 20,
      "items": {
        "type": "string",
        "maxLength": 500
      },
      "description": "推荐选项**值**列表（按 value 匹配高亮；字符串或数组均容错接受，字符串按单元素数组处理）"
    },
    "timeout": {
      "type": "integer",
      "minimum": -1,
      "description": "可选：超时秒数。未设置 = 用项目全局 ask_timeout_sec（全局也未配置则一直等待）；0 = 用全局；-1 = 明确不超时；>0 = 覆盖全局。到点自动回复（优先取 recommended）并继续本轮"
    }
  },
  "required": [
    "question"
  ]
}
```

# 返回值
受理成功时立即返回以下纯文本（`isError=false`），任务节点保持 `running`：

```text
已向用户提问，正在等待回复。你可以继续完成其他可独立完成的任务；如果暂时没有其他可做的，请直接回复文本「等待用户回复」并结束本轮对话。用户答复到达后会作为后续消息送达。
```

用户答复、超时自动答复到达后，作为**后续用户消息**投递（不是本次工具结果）：

```text
【用户回答】
问：<question>
答：<answer>
```

同一次快照里若已收集到多条已答项（并发答复），按提问顺序合并；通常每条答复到达即单独投递，不等待同会话其它未答问题：

```text
【用户回答】
1. 问：<question1>
   答：<answer1>
2. 问：<question2>
   答：<answer2>
```

- 超时且无 `recommended`：`用户未在时限内回复，请按推荐做法继续。`
- 用户在会话里讨论后点「完成」：不投递用户消息；历史中的工具结果改写为 `参考后续的聊天记录`。
- 参数错误（如 `question` 为空/超长、`options` 超限、选项对象 `value/label` 全为空）：返回 `isError=true` 的工具错误，不登记提问。
- 历史/重载时，同一 `ask_user` 行的工具结果只写成 `用户已回复（答案见后续用户消息）` / `已超时，自动答复见后续用户消息`（或 `参考后续的聊天记录` / `提问已终止（任务已停止）`）；**答案正文只在后续用户消息里出现一次**，不会重复。