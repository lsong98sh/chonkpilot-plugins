# 基本信息
```yaml
name: subchat_open
title: subchat_open
args: subchat_open --input={RAW-INPUT-FILE}
async: never
category: task
hot: true
output: stdout
runtime: plugin
timeout: 0
```

# 摘要
打开一个「子聊天」：新建一个**固定参数**（角色 / 场景 / LLM 锁定、用户不可切换）的独立会话，把你的交接内容**以用户口吻**写入作为它的首条消息，并让子 agent 自动先开口；父会话窗口随后为该子聊天**自动开窗**。

**两种形态由参与者数量决定**：
- **1 个参与者 = 单聊**（人机一对一）：走既有单聊路径，需要用户参与的事（需求讨论、方案设计等）用它。
- **N ≥ 2 个参与者 = 群聊**（多 agent 展示板，人只旁观）：机器人之间自动来回推进，你（主 agent）负责布置任务、兜底与判定收敛。群聊需要同时给出「本次目标 / 剧本（推进规则）/ 收敛与退出条件」。

本工具**立即**返回 `{ok, session_id, mode}`——**主轮不挂起**（与 `llm_run` 的「派发→挂起等→自动回填」相反）。`mode` = `single` / `group`，用于确认实际形态。**结果不回流**：调用返回即终态，之后该子聊天的产出不会再回到本会话。**群聊**完成后会在群聊窗口里留下结论记录。失败返回 `{ok:false,error}` 且工具 `isError=true`；窗口开窗由前端执行，窗口上限等失败不影响本工具返回。

**群聊的「目标 / 剧本 / 收敛条件」怎么写**（`goal` / `playbook` / `convergence` 三个参数，由你**现写**，没有预置模板）——参考样板（以「代码审查」为例）：

```
【群聊目标】
审查 src/foo/ 本次改动：不引入回归、不新增安全漏洞。

【参与者与职责】
- 审查员：审 diff，列出问题（按严重度）
- 程序员：按审查意见修改
- 测试员：跑回归测试（很贵，仅在被点名时执行）
- 主 agent：判收敛

【推进规则（靠发言末行的 @ 决定下一个叫谁）】
- 审查员发现问题 → @程序员
- 程序员改完     → @审查员
- 审查员复审：
    · 改动复杂 / 涉安全 → @测试员
    · 只是小修         → 不 @，直接报"已收敛"
- 测试员跑完     → @审查员
- 需要全员同步   → @全体

【收敛条件（主 agent 判定，要有耐心）】
- 判定前先看"有没有实质推进"（如：问题数是否在下降 / 是否每轮都在改同一处 / 是否只是措辞变化）
- 不得草率收敛
- 判据：连续 2 轮无新的实质问题，且（若调用过测试）测试通过 → 判"达成"，点亮「可以结束了」

【退出条件】
- 用户手动停
- 撞轮次上限 → 主 agent 终审 + 给进度摘要
```

要点：`participants` 里的**每个参与者都要给角色/职责**（会被注入给全体）；`playbook` 用**发言末行的 `@`** 决定下一个叫谁（`@某人` 可多个 / `@全体` / 不写 `@` = 全体）；`convergence` 写清「什么算收敛」。**主 agent 必须自己也在参与者名单里**（你是发起者、兜底裁判，也是参与者）。

# 参数
```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "agent": {
      "type": "string",
      "maxLength": 256,
      "description": "子聊天的固定角色 / **主 agent 名**：1 个参与者（单聊）时 = 子聊天对方角色；N ≥ 2（群聊）时 = 你（主 agent）的名字（主 agent 必为参与人，若未列入 participants 会被自动补入）。须为当前场景团队成员，可带 `<场景id>/` 前缀消歧。"
    },
    "prompt": {
      "type": "string",
      "maxLength": 16384,
      "description": "交接内容：由你**以用户口吻**撰写的首条消息（例如「我想要…」）。单聊时子 agent 看到的就是用户提出的需求；群聊时会作为群聊展示板的**首条消息**（= 布置任务 / 开场），落库。"
    },
    "llm": {
      "type": "string",
      "maxLength": 256,
      "description": "子聊天锁定的 LLM（provider 名）。群聊时全体参与者共用。"
    },
    "scenario_id": {
      "type": "string",
      "maxLength": 256,
      "description": "子聊天锁定的场景（场景目录名 / key）。群聊时全体参与者共用。"
    },
    "participants": {
      "type": "array",
      "maxItems": 20,
      "description": "参与者名单（**可选**）。**省略或只给 1 项 = 单聊**（走既有单聊路径）；**给 2 项及以上且每项有效 = 群聊**。每项含该参与者的 agent 标识与角色/职责；**顺序 = 群聊依次叫号的固定顺序**。主 agent（`agent` 参数）会被自动保证在名单内。坏项/重复项会返回参数错误，不会静默降级为单聊。",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "agent": {
            "type": "string",
            "maxLength": 256,
            "description": "参与者 agent 标识（场景团队成员名；可带 `<场景id>/` 前缀消歧）。"
          },
          "role": {
            "type": "string",
            "maxLength": 4000,
            "description": "该参与者的角色 / 职责（会被注入给全体参与者；如「审查员：审 diff，列出问题」）。"
          }
        },
        "required": [
          "agent"
        ]
      }
    },
    "goal": {
      "type": "string",
      "maxLength": 16384,
      "description": "群聊的**本次目标**（仅群聊用；可选但强烈建议给出）。会被每轮注入给全体参与者。"
    },
    "playbook": {
      "type": "string",
      "maxLength": 16384,
      "description": "群聊的**剧本 / 推进规则**（仅群聊用；可选但强烈建议给出）：写清各角色什么情况下 `@` 谁、什么情况下不 `@`、什么时候该做贵操作。样板见上方「摘要」。"
    },
    "convergence": {
      "type": "string",
      "maxLength": 16384,
      "description": "群聊的**收敛 / 退出条件**（仅群聊用；可选但强烈建议给出）：写清「什么算收敛」的判据（如连续 N 轮无新实质问题即判达成），以及退出条件。会被每轮注入给全体参与者。"
    }
  },
  "required": [
    "agent",
    "prompt",
    "llm",
    "scenario_id"
  ]
}
```

# 返回值
成功（`isError=false`）：

```json
{ "ok": true, "session_id": "subchat-…", "mode": "single" }
{ "ok": true, "session_id": "group-…", "mode": "group" }
```

- `session_id` 是新建子聊天 / 群聊会话 id；`mode` 明确本次实际形态。
- 调用返回即终态：主轮不挂起，子聊天后续产出**不回流**本会话。
- 重复调用会再建一个子聊天（非幂等）；不要因超时重试而多开。

失败（`isError=true`，文本为 JSON）：

```json
{ "ok": false, "error": "agent 必填（子聊天固定角色）" }
```

常见错误：四必填缺失、`participants` 含坏项/重复项/有效项不足 2、`prompt` 超长、固定参数（agent / scenario_id / llm）在能力注册表/usr llms 可用时校验不存在、建会话/首轮/写交接内容失败。部分失败可能已留下会话行（无回滚），重试前请先确认会话列表。
