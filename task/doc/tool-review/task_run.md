---
plugin: task
tool: task_run
contract: src/tools/task/task_run.tool.md
handler: src/main.go:179
reviewed_at: 2026-11-09
status: fixed-p0-p1
---

# task_run 审查

## 1. 一句话结论

可用但有风险：执行器本体成熟（超时杀树、输出封顶、路径强校验），但**沙箱存在两条绕过**（`cwd` 被加入可写白名单；`requires` 安装完全在沙箱外）、**退出码 5 被误判为“需要管理员权限”**，且契约声称的 `file`=exe + `runtime=shell` 用法在 Windows 上实际失败。

## 2. 调用链

1. LLM `tool_call task_run` → `turn.dispatchToolCall`：建任务节点、落 running 行、交给独立 worker（`E:/BizWorks/chonkpilot/src/lib/llm/server/turn.go:1522-1578`）。
2. `gatewayArgs` 给非“进程内工具/已声明超时裁决”的工具**注入调用级 `async=always`**（`turn.go:342-358`，判据 `arbitratedTool` 读工具 `_meta.timeout`，见 `turn.go:360-376`）→ 本插件工具 `_meta` 无 timeout（见 §9），故默认配置下必然命中（用户 `tool_async` 覆盖可改变，非默认路径）。
3. gateway `doCall`：`mode=always` → 任务化后台执行并**立即返回 pending**，该档位**不设超时点**（`E:/BizWorks/chonkpilot-agent/gateway/src/mcpgateway.go:1027-1037`、`1004-1010`）。
4. 插件 MCP server `tools/call` → SDK handler `taskPlugin.run`（`src/main.go:179-223`）→ 读配置 → `scriptrun.Run`（`E:/BizWorks/chonkpilot-util/scriptrun/exec.go:107-202`）。
5. 结果 JSON 作为 MCP TextContent 回 gateway → 终态经 `mcp-tasks-report`（`mcpgateway.go:382-421`）→ `onGatewayTaskDone` 落 message 并续轮（`handler_task.go:128-181`）。

## 3. 契约面

YAML：`async: never`、`timeout: 600`、`hot: true`、`idempotent: false`、`output: stdout`（`task_run.tool.md:3-13`）。Schema 9 个参数：`command`/`file`（二选一，均非 required）、`runtime`（8 枚举）、`requires[]`、`interpreter`、`cwd`、`timeoutSec`（1–600）、`env{}`、`args[]`（`task_run.tool.md:18-67`）。

契约问题：
- `async: never` 与 `timeout: 600` **不会下发到 gateway**（插件只发 url/token/hot，见 §9），实际档位是 `always`（无超时点）；契约的“前台执行，带超时”只对插件自身 `timeoutSec` 成立。
- 摘要称“`file` 填 exe 绝对路径（用 shell）”可行（`task_run.tool.md:16`）——Windows `shell` 走 PowerShell `-File`，对 exe 不成立（见 P1-2）。
- 摘要称沙箱“只能写 work_dir + `.chonkpilot/tmp` + 配置目录”（`task_run.tool.md:16`），但实现会把 `cwd` 也加入白名单并打 Low IL（见 P0-1）。
- 摘要称“沙箱开启时脚本只能写白名单”，`requires` 的 pip/npm 安装不在沙箱内（见 P0-2）。
- 失败形状写“`ok:false` 并带 `error`”（`task_run.tool.md:83`），但**参数级失败**走 MCP `isError`（裸文本、无 `ok` 字段），见 P2-1。
- 未声明：`requires` 仅 python/js 可用、安装有 5 分钟独立上限且完成后才计入 `ms`；未暴露输出上限参数（只能由配置 `plugins.task.max-output-kb` 控制，最大 4096KB）。

## 4. 实现面

- 参数解析：`asString`/`toInt`/`stringSlice`/`envMap`（`main.go:181-256`），容忍类型偏差；空串被忽略。
- 默认值：`cwd` = 配置 `default-cwd` → 会话 `work_dir` → 空（不设 Dir，落到插件进程 cwd）（`main.go:181-187`）；`timeout` = 配置默认 120s，参数覆盖，再夹到 1–600（`main.go:188-197`，`config.go:116-118`）；输出 = 配置 256KB（`config.go:116-118`），`maxOutputKB<=0` 兜底 256（`main.go:198-201`）。
- 硬限制：输出累计上限 = `min(配置KB, 8MB)`（`exec.go:153-156`、`59`）；单次脚本超时 ≤600s；依赖安装独立 5 分钟（`exec.go:62`）。无并发限制、无重入锁。
- 执行：`normalizeRuntime` 白名单（`exec.go:244-253`）；`command` 与 `file` 必须且仅有一个（`exec.go:114-116`）；`cwd`/`file`/`interpreter` 走 `pathutil.Resolve`（仅绝对 / `~/` / `!/`，相对拒绝，`exec.go:118-128`、`257-263`）。
- runtime 组装：`cmd`=`/c`；`bash`=`-c`；`shell`（Windows）= PowerShell `-NoProfile -NonInteractive -Command`，`file` 时用 `-File`（`exec.go:320-333`）；代码文件型 `python/js/powershell/vbs/bash/java` 用临时脚本（`scriptExt`，`exec.go:53-55`、`341-366`），临时文件 defer 删除。
- 进程清理：`exec.CommandContext` + `cmd.Cancel=killTree`（Windows `taskkill /T /F` 再 `Process.Kill`）+ `WaitDelay=3s`（`exec.go:206-241`）。
- `requires`：`python` → `pip install -q`；`js` → 无 `package.json` 先 `npm init -y`，再 `npm install --no-save`；以**插件进程权限**、`context.Background()+5min`、**无沙箱**执行（`exec.go:138-142`、`457-493`）。
- 环境：`os.Environ` + 宿主 `CHONKPILOT_*` + 用户 `env`，同名覆盖；去重按大小写敏感（`exec.go:136`、`427-453`）。
- 副作用：任意进程、网络、文件系统写入；`idempotent:false` 正确。并发同名 `task_run` 无互斥（由调用方负责）。
- 失败分级：参数级/前置条件 → Go error（MCP `isError`）；运行期（非 0 退出、超时、提权）→ `Result{ok:false,error}`（`exec.go:104-106`）。

## 5. 返回面：LLM 实际看到的所有形态

> 生产链路里本工具被强制 `async=always`：插件返回体先作为 gateway 任务结果，再经 `mcp-tasks-report` 回执落到 message 表，LLM 看到的是**回执文本**（`turn.handleAsyncDone`，`turn.go:1257-1293`）。下述 A/B/C 为回执文本内容。

A. 成功（`exit_code=0`）——LLM 看到完整 JSON：
```json
{"runtime":"shell","command":"<原命令>","cwd":"E:\\proj","exit_code":0,"ms":120,"output":"...","truncated":false,"ok":true}
```
gateway 任务态 `done`，`isError` 不置（引擎按 `completed` 续轮）。

B. 运行期失败（非 0 退出 / 超时 / 需管理员 / 启动失败）——同一 JSON，`ok:false` + `error`：
- 超时：`error:"超时（600s）已终止（已杀进程树）"`，`exit_code` 为杀进程后的码或 -1（`exec.go:179-184`）。
- 非 0：`error:"脚本执行失败（exit N）：exit status N"`（`exec.go:194-198`）；`output` 可能已有部分输出。
- 提权：`error` 为整段“需要管理员权限…请以管理员身份打开 PowerShell / CMD 后手动粘贴执行：\n<命令行>\n（原始错误：…）”`（`exec.go:419-423`）。
- 启动失败：`error` = 原始 `exec` 错误文本，`exit_code=-1`（`exec.go:199-201`）。
gateway 任务态仍是 `done`（插件未置 MCP `isError`），LLM 只能靠 `ok:false` 自判。

C. 参数级失败（同缺/同给 command+file、runtime 非法、路径不合规、脚本文件不存在、解释器未配置、临时脚本创建/写入失败、requires 安装失败）——**裸文本 + isError=true**，无 JSON、无 `exit_code`；经 gateway 回执照样作为任务 `done` 文本投喂（`exec.go:107-150`、`main.go:219-221`、SDK `plugin.go:552-555`）。

D. 截断：`truncated:true`，`output` 为前 N 字节（`exec.go:495-532`）；**文本内无截断提示**，LLM 若不看 `truncated` 会当作完整输出。

E. 取消：父轮次停止/任务被 `task_stop` 时 gateway 任务态 `cancelled`，LLM 看到 `任务结束(cancelled)`（`turn.go:1275-1277`）；若插件内部 ctx 先取消，则 `Result.ok=false,error:"context canceled"`（`exec.go:179-201`）。

F. 异步已受理：gateway `pending`（`{"status":"pending","task_id":...}`，文本“任务已转后台执行”，`mcpgateway.go:1311-1320`）由 turn worker 消费，**不进入 LLM 上下文**；外部 MCP 客户端可见。

G. 运行中：无（本工具无进度上报；任务节点 `pending/running` 仅前端可见）。

H. 超时裁决 `awaiting`：本工具在 `always` 档位不可达；仅当用户 `tool_async` 覆盖/`params.AsyncMode` 把档位改成 `never/manual` 时才可能出现（`mcpgateway.go:1138-1159`），届时 LLM 收到 `{"status":"awaiting","task_id","reason":"timeout","timeout_s","options":["wait","cancel"]}` 文本“任务等待用户裁决”。

I. 网关级内部错误（provider 断开、熔断、执行池拒绝）：同步路径落 `错误: <msg>`（`turn.go:1596-1610`）；若已进入后台执行后失败，只回 `任务结束(error)`（错误全文被 `handleAsyncDone` 丢弃，`turn.go:1278-1280`）。

J. 输出体积：默认 256KB（约 6–7 万 token）可一次灌满上下文；无分页、无 next 指针，唯一补救是让用户调小配置或脚本自行重定向。

## 6. 一致性与问题

| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P0 | **沙箱绕过（cwd）**：`cwd` 被追加进可写白名单并对该目录打 Low IL，LLM 传任意绝对目录即可让沙箱脚本在其内新建文件；契约声称只写 work_dir/临时目录/配置目录 | `src/sandbox.go:27-46`（`dirs=append(dirs,cwd)`、`tagLowIL`）；`task_run.tool.md:16` | 写隔离失效，可写出项目目录 |
| P0 | **沙箱绕过（requires）**：pip/npm 安装以插件进程权限、无沙箱执行，可写 site-packages / `cwd` 下 `package.json`、`node_modules`；LLM 可控包名 | `exec.go:138-142`、`457-493`；`task_run.tool.md:16` | 沙箱声称的“只能写白名单”不成立；供应链面 |
| P1 | 任意退出码 5 被判定“需要管理员权限”，给出误导性复制命令行 | `exec.go:403-415`（`ee.ExitCode()==5`） | 脚本常见失败码被误诊，模型反复重试/打扰用户 |
| P1 | 契约声称 `file`=exe + `runtime=shell` 可跑；Windows 走 `powershell -File <exe>`，按 PowerShell 语义非 `.ps1` 会报“不是脚本文件” [待核实：以一次真实调用确认] | `exec.go:329-332`；`task_run.tool.md:16` | 模型按文档用法必失败，且错误文本不解释原因 |
| P1 | `async: never`/`timeout: 600` 未下发 gateway；实际被 `gatewayArgs` 强制 `always`，gateway 侧无超时点 | `turn.go:342-358`、`mcpgateway.go:1027-1037`；`task_run.tool.md:6,12` | 契约语义失效；`isError`/任务态与契约不一致 |
| P2 | 参数级失败不返回契约承诺的 `ok:false+error`，而是 isError 裸文本 | `exec.go:107-150`；`task_run.tool.md:83` | 模型需两套解析逻辑 |
| P2 | 截断只在 `truncated` 标记，输出内无提示；`TrimSpace` 会吃掉首行缩进 | `exec.go:175`、`520-525` | 模型易把截断输出当完整结果 |
| P2 | `requires` 安装不随工具超时/取消停止，且不计入 `ms`；超时只杀单进程非进程树 | `exec.go:138-142`、`457-493`（`context.Background()`） | 取消后仍残留安装进程；耗时统计失真 |
| P2 | `env` 去重大小写敏感，Windows 覆盖 `PATH` 可能同时保留 `Path` | `exec.go:427-453` | 环境变量覆盖不确定 |
| P2 | `args` 追加到 `command` 形态的语义随 runtime 变化（PowerShell `-Command`/cmd `/c`），契约未说明 | `exec.go:144-151`、`308-337`；`task_run.tool.md:60-64` | 模型误以为等价于脚本参数 |
| P2 | 非 Windows 的 `shell` 实际是 `sh`（PATH），契约写 `/bin/sh` | `exec.go:280-285` | 文案级不一致 |
| P2 | 输出上限最大可配到 4096KB（≈ 百万级 token），单次可爆上下文；无按调用的输出上限参数 | `config.go:54`、`main.go:198-201` | token 爆量风险 |

## 7. LLM 视角优化 / 改善 / 增强

| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| `cwd` 只允许 work_dir 子树（或将 `cwd` 从写白名单剔除、仅作为执行目录） | 优化（安全） | 消除写隔离绕过 | 低，`src/sandbox.go` 1 文件 | 可能阻断“在别的盘构建”，需契约写明 | 建议现在做 |
| `requires` 安装纳入沙箱或改为只允许白名单包/禁用 | 优化（安全） | 消除第二处绕过 | 中，`exec.go`+契约 | 破坏 `requires` 现有用法 | 排期做（至少加文档警告） |
| 提权判定只认启动错误码 740 / EACCES，移除“任意 exit 5” | 优化 | 失败诊断正确、可恢复 | 低，`exec.go:403-415` | 无（错误文案更准） | 建议现在做 |
| Windows `shell` + `file` 指向非 ps1 时改为 `cmd /c` 或报明确错误 | 改善 | 按文档用法可成功 | 低，`exec.go:320-333` | 无 | 建议现在做 |
| 失败统一为 `{ok:false,error,exit_code,output}` JSON（取消/参数错误也包一层），仅真正协议错误用 isError | 改善 | 单一解析路径、可恢复 | 中，`main.go`+契约 | 需同步改契约/测试 | 排期做 |
| 截断时在 `output` 尾部追加 `\n…[输出已截断，共 N 字节，仅保留前 M 字节]`，并暴露 `output_bytes` | 改善 | 模型不会误判完整性 | 低，`exec.go` | 文本形状变化 | 建议现在做 |
| 暴露可选 `maxOutputKB`（并夹到配置上限） | 增强 | 模型可控制 token | 低，契约+schema | 新增可选参数，向后兼容 | 建议现在做 |
| `requires` 安装使用工具 ctx 派生（可取消）并计入 `ms` | 优化 | 取消生效、耗时可解释 | 低 | 无 | 建议现在做 |

## 8. 建议的测试 / 验收（不执行）

1. 沙箱开启、`cwd` 指向 `%USERPROFILE%\Documents`，脚本在该目录建文件 → 期望 OS 拒绝；当前会成功（复现 P0-1）。
2. 沙箱开启、`requires:["requests"]`，观察安装进程写入 site-packages / cwd；期望安装受白名单约束或明确禁止。
3. 脚本 `exit 5` → 期望普通失败（非“需要管理员权限”文案）。
4. `runtime=shell` + `file=<某个 .exe>` → 断言成功（当前预期失败，需先修）。
5. `command` 与 `file` 同时为空/同时有值 → 断言 isError 文本清晰。
6. 输出 300KB 文本 → 断言 `truncated:true` 且长度=配置上限；校验 LLM 文本是否含截断提示。
7. 600s 超时终止：脚本派生孙进程 → 断言无残留进程（`taskkill /T`）。
8. 取消：脚本 sleep 300s，任务面板停止 → 断言进程树被杀、任务态 cancelled、无孤儿。
9. `env` 覆盖 `Path`（Windows）→ 断言子进程只看到一份 PATH。
10. 相对路径 `cwd`/`file`/`interpreter` → 断言拒绝且文案含允许写法。

## 9. 基线修正（仅当共通基线不成立时）

- **基线 6（“契约 YAML 的 async/timeout 决定 gateway 行为”）对本插件不成立**：插件契约由 `go:embed` 打进 exe，只用于 `contract.Parse` 生成 MCP schema；`plugin.mcp.register` 只上报 `{url, token, hot_tools}`（`E:/BizWorks/chonkpilot-framework/plugin/src/sdk/plugin.go:586-591`），gateway 侧从 `tools/list` 读不到 `_meta.async/_meta.timeout`，最终回落 server 级 `timeout=600`（`E:/BizWorks/chonkpilot-framework/plugin/src/mgr/api.go:345-354`）且被宿主注入 `async=always`。审查本插件时必须按 `always` 档位理解返回面。
- **基线 5 的默认值注入位置**：本插件的默认值来自 `configManager.Settings()`（`src/config.go:116-118`），不是 `main.go applyDefaults`；契约里的 `timeoutSec` 上下限与实现一致（1–600）。
- **基线 4 的路径规则**：本工具使用 `pathutil.Resolve`（绝对 / `~/` / `!/`，相对拒绝），与 file 域同族但**没有 symlink 解析与读侧沙箱**；写侧隔离是进程级（Low IL + Job），不是路径预检。

## 10. 修复记录（2026-11-09）

本次只改 `task` 插件与共用库 `chonkpilot-util/scriptrun`（crontab 业务语义不变；`scriptrun` 仅新增向后兼容字段）。§6 表格保持审查时原貌，以下逐条对应并说明现状。

| 原问题 | 现状 | 修复点 |
|---|---|---|
| P0 · 沙箱绕过（`cwd` 加入可写白名单） | **已修复（2026-11-08，§6 该行已过时）** | `sandboxOptions` 只使用 gateway 下发的 `_meta.chonkpilot.sandbox.writable_dirs`，`cwd` 不再追加；`resolveCwd` 只允许 work_dir 子树。 |
| P0 · 沙箱绕过（`requires` 全局安装） | **已修复（2026-11-08 项目内 deps + 2026-11-09 安装沙箱化，§6 该行已过时）** | 安装目标固定为 `work_dir/.chonkpilot/deps`（缺 work_dir 拒绝）；安装命令改为复用 `execute`：Low IL + Job、ctx 可取消、超时杀进程树；安装前对 deps 打 Low IL 并临时追加进安装用白名单（仅安装，不扩大脚本白名单）；pip `--target`、npm `--prefix`、TEMP/TMP/TMPDIR/cache 仍指向 deps。 |
| P1 · 任意 exit 5 被判定“需要管理员权限” | **已修复** | `requiresElevation` 删除 `ee.ExitCode()==5` 分支，只保留启动错误 740 / ACCESS_DENIED(5)；运行期 exit 5 走普通“脚本执行失败（exit 5）”。 |
| P1 · 契约声称 `file`=exe + `runtime=shell`，实际走 PowerShell `-File` | **已修复** | Windows `shell` + `file` 按扩展名分派：`.ps1` → `powershell -NoProfile -NonInteractive -File`；`.bat/.cmd` → `cmd /c`；`.exe/.com` → 直接执行；其它扩展名明确报错并提示改用 `runtime=cmd` 或显式 interpreter。`runtime=cmd` 的 `/c` 语义未改；`command` 形态仍是 PowerShell `-Command`。 |
| P1 · `async: never` / `timeout: 600` 未下发 gateway | **未处理（平台/宿主链路问题）** | 属插件注册与 gateway 档位问题，超出本次 task/scriptrun 范围；仍按 `always` 档位理解返回面。 |
| P2 · 参数级失败不走 `ok:false+error` | **未改（只补文档）** | 参数级失败仍为 MCP `isError` 文本；契约“返回值”段已注明该形态，运行期失败仍返回 `ok:false+error` JSON。 |
| P2 · 截断只在 `truncated` 标记、输出内无提示；`TrimSpace` 吃掉首行缩进 | **部分修复** | 新增 `outputCapture`：超限时 `output` 尾部追加“完整输出见 `<output_file>`”（落盘失败则为“输出已截断，完整输出落盘失败”），并返回 `output_file` / `output_bytes` / `total_bytes` / `output_file_error`；仅超阈值才创建 `<work_dir>/.chonkpilot/tmp/task-run-*.log`（0600，不自动删除；无 work_dir 用系统临时目录）。`TrimSpace` 行为保留（首行缩进仍会被去掉）。 |
| P2 · `requires` 不随取消停止、不计入 `ms`、超时只杀单进程 | **已修复** | `installRequires(ctx, sb, …)` 内部 `context.WithTimeout(ctx, 5min)`；命令复用 `execute`（`cmd.Cancel=killTree` + WaitDelay）；`Run` 的 `start` 已移到安装之前，`ms` 含安装耗时；安装输出用 64KiB 小 capture，失败时截断附在错误里。 |
| P2 · `env` 去重大小写敏感 | **已修复** | `mergeChildEnv` 在 Windows 按 `strings.ToUpper` 归一变量名去重（base 与 groups 同一规则，`PATH`/`Path` 只保留一份，后组覆盖前组）；非 Windows 保持精确匹配。 |
| P2 · `args` 语义随 runtime 变化 | **未改** | 契约仍未展开说明；行为保持不变。 |
| P2 · 非 Windows `shell` 实际是 PATH 的 `sh`，契约写 `/bin/sh` | **未改（文案级）** | 契约摘要仍写“其它平台 = `/bin/sh`”；实现为 `sh`（PATH）。 |
| P2 · 输出上限最大 4096KB、无按调用上限 | **已修复** | `task_run` 新增可选 `maxOutputKB`（1..配置上限；超配置夹到配置值；未传用配置）；`scriptrun.Options.MaxOutput` 上限仍由调用方控制，本次未新增任何文件大小上限。 |

实现落点：
- `E:/BizWorks/chonkpilot-util/scriptrun/exec.go`：`Options.CaptureDir`、`Result` 新字段、`outputCapture`（替换 `cappedBuffer`）、`installRequires`/`installSandbox`、`requiresElevation`、`buildCommand`（Windows shell + file 分派）、`mergeChildEnv`/`envKey`。
- `E:/BizWorks/chonkpilot-plugins/task/src/main.go`：`maxOutputKB` 参数夹取、`captureDirFor`。
- 契约：`src/tools/task/task_run.tool.md`。
- 测试：`chonkpilot-util/scriptrun/exec_test.go`（`TestMergeChildEnvCaseInsensitive`、`TestRequiresElevationExit5NotElevation`、`TestBuildCommandShellFileWindows`、`TestOutputCaptureWritesFile`）；`task/src/tools_test.go`（`TestTaskRunOutputFile`、`TestTaskRunExit5NotElevation`、`TestTaskRunShellFileDispatch`）。
- `crontab` 不传 `CaptureDir`，保持“只截断、不落盘”的旧行为；`Options`/`Result` 新增字段向后兼容。