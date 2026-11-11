# 插件工具审查总览

> 输入：9 个插件（file / task / crontab / codegraph / web / mcp / browser / desktop / demo-python）共 43 个 LLM 可见工具的子代理文档，2026-11-08。
> 本文只做去重、归纳、排序；不重新审查工具、不修改源码/契约/产物。所有证据均引用各工具文档 §6/§7 及其中的源码行号。

## 1. 覆盖范围与遗漏

| 插件 | 工具数 | 索引 | 覆盖结论 |
|---|---:|---|---|
| file | 11 | `file/doc/tool-review/00-index.md` | A/B 两批全覆盖；7 个写工具已核实 |
| task | 8 | `task/doc/tool-review/00-index.md` | A/B 两批全覆盖 |
| crontab | 6 | `crontab/doc/tool-review/00-index.md` | 全覆盖 |
| codegraph | 9 | `codegraph/doc/tool-review/00-index.md` | A/B 两批全覆盖；管理工具 hot=false 未出文档（按设计） |
| web | 3 | `web/doc/tool-review/00-index.md` | 全覆盖 |
| mcp | 3 | `mcp/doc/tool-review/00-index.md` | 全覆盖 |
| browser | 1 | `browser/doc/tool-review/00-index.md` | 全覆盖 |
| desktop | 1 | `desktop/doc/tool-review/00-index.md` | 全覆盖 |
| demo-python | 1 | `src/initdata/capability/plugins/demo-python/doc/tool-review/00-index.md` | `demo_echo` 已出；非 hot 的 `demo_time/demo_counter/demo_llm` 按设计未出文档 |
| **合计** | **43** | | |

- 无工具插件已核实：`history` / `memory` / `remote` 的 manifest 均为 `mcp.mode=none`、`hot_tools=[]`（各自 `manifest.json:18-21`），不派子代理，不是遗漏。
- 未执行 build/test/插件进程/真实网络；所有结论是源码态。未比对 `dist`、`build/plugin-apps` 的运行时行为（task/crontab/web/mcp 均记录了产物漂移，只记录未修改）。
- `demo-python` 的 3 个非 hot 工具缺省不进 LLM 工具面，但白名单/按名调用仍可能触达；其中 `demo_llm`（插件内调宿主 LLM）未审查，若后续纳入默认白名单需补文档。
- 证据不足项已在各文档标 `[待核实]`，本文沿用，不拔高为结论。主要待核实项：gateway 对 builtin 插件工具级 `_meta` 的最终覆盖（file/task/crontab）、`sandbox.Policy.AllowsPath` 是否解析 symlink（file）、`task_run` 的 `file=exe + runtime=shell` 真实行为、前端是否按会话 `kind` 过滤 `subchat-state`、browser 同 profile 并发建 WebView2 的稳定性、Go 分组 import 提取、旧 `dist` 的 meta 契约是否仍部署、`task_stop` 成功路径的真实调用结果。

## 2. 结论统计（插件 × P0/P1/P2）

### 2.1 raw 计数（按各索引的“每工具问题行”原样汇总，跨工具重复计）

| 插件 | 工具数 | P0 | P1 | P2 | 备注 |
|---|---:|---:|---:|---:|---|
| file | 11 | 4 | 33 | 55 | A 批 1/10/21 + B 批 3/23/34 |
| task | 8 | 4 | 17 | 44 | 索引合计 |
| crontab | 6 | 0 | 18 | 32 | 索引合计 |
| codegraph | 9 | 5 | 50 | 76 | 同一根因（workdir、未就绪文案、limit）在 9 个工具重复计 |
| web | 3 | 1 | 12 | 17 | 索引合计 |
| mcp | 3 | 4 | 13 | 13 | 索引合计 |
| browser | 1 | 3 | 5 | 8 | 单工具 |
| desktop | 1 | 4 | 5 | 8 | 单工具 |
| demo-python | 1 | 0 | 2 | 7 | 单工具 |
| **合计（raw）** | **43** | **25** | **155** | **260** | **440 行，不等于缺陷数** |

> raw 口径说明：file-B 的 5 条跨工具共通项 C1–C5（P1 4 / P2 1）未计入其 3/23/34 的每工具行；若一并计入，file 为 4/37/56。其余插件同理存在“同一根因在多个工具文档重复计”，因此 raw 只能用于核对各文档产出量，不能当作全局问题数。

### 2.2 去重后计数（按根因合并；共性根因只计一次，不再回摊到各插件）

| 分组 | P0 | P1 | P2 | 小计 |
|---|---:|---:|---:|---:|
| 跨插件共性（§3） | 3 | 12 | 4 | 19 |
| file（§4.1） | 3 | 5 | 1 | 9 |
| task（§4.2） | 3 | 8 | 1 | 12 |
| crontab（§4.3） | 0 | 7 | 1 | 8 |
| codegraph（§4.4） | 0 | 8 | 1 | 9 |
| web（§4.5） | 1 | 2 | 1 | 4 |
| mcp（§4.6） | 3 | 1 | 1 | 5 |
| browser（§4.7） | 1 | 2 | 1 | 4 |
| desktop（§4.8） | 2 | 2 | 1 | 5 |
| demo-python（§4.9） | 0 | 1 | 1 | 2 |
| **合计（去重后）** | **16** | **48** | **13** | **77** |

> 去重口径：P0/P1 按“同一根因”合并（如 codegraph 5 个 P0 实为同一 workdir 根因）；P2 多为单工具字段/文案细节，按插件聚为 1 簇，不强行跨工具合并。跨插件共性根因（含只影响 file 内部多个工具的锁/沙箱项已下沉到 §4.1）只在 §3 计一次。

## 3. 跨插件共性问题（按根因合并）

| 编号 | 级别 | 根因 | 受影响工具 | 证据（子代理文档） |
|---|---|---|---|---|
| G1 | P1 | 契约 YAML 的 `async/timeout/idempotent/category` 未被插件契约解析器消费：`contract.Parse` 只取 name/title/hot，Register 只写 `_meta.hot`；gateway 落 `auto`，超时取 manifest server 级值 | file 11、task 8、crontab 6（25 个 .tool.md 工具） | file 索引 C1（`contract/contract.go:72-79,101-105`）；task 索引共性 1；crontab 索引跨工具根因 |
| G2a | P0 | SDK 桥每次能力调用固定 15s（`Options.Timeout` 缺省/`p.Call` WithTimeout），击穿一切“等收尾”语义；超时后子任务继续跑但 id/结果丢失 | task_llm、task_status、task_run（长任务）、mcp_invoke | task 索引共性 2（`sdk/plugin.go:39,54,149-151,316-331`）；`task_llm.md` §6；`mcp_invoke.md` §6 |
| G2b | P1 | 工具级 `timeoutSec`/契约 `timeout` 与 gateway 阈值矛盾或完全无效：web 3×120s+重试、browser 引擎 120s、manifest 180/300/600s 各自为政 | web 3、browser_run、desktop_run、task_run/status、mcp_invoke、file/crontab 长操作 | `web_search.md`/`web_fetch.md`/`web_download.md` §6；`browser_run.md` §6；codegraph 索引第 8 条 |
| G3 | P1 | LLM 可见 description 只取 `# 摘要` 第一段，后续规则（沙箱、批量、阻塞语义、群聊约束、返回值）全部不可见；部分首段还与运行时矛盾 | file 11、task 8（19 个契约工具；crontab `[待核实]`） | file 索引 C4（`contract.go:174-193`）；task 索引 B1；`ask_user.md`/`subchat_open.md` §6 |
| G4 | P1 | schema 只在 `tools/list` 透出，handler 不做运行时校验；错误类型/缺失必填/超范围被静默回退或忽略 | file 11、codegraph 9、crontab 6、demo_echo、browser_run、ask_user | file 索引 C5；codegraph 索引 A 组第 7 条；`demo_echo.md` §6；`browser_run.md` §6 |
| G5 | P1 | isError 语义不自洽：插件全项失败仍 `IsError=false`、gateway 异步把 isError 抹成 done、crontab 失败是 `IsError=true` 裸文本而非 `{ok:false}`、web_download 4xx 当成功 | file 11、codegraph 9、task 7（task_stop 另列）、crontab 6、web_download、demo_echo | file 索引 C3；codegraph 索引第 8 条；task 索引共性 4/B3；crontab 索引跨工具根因；`web_download.md` §6 |
| G6 | P1 | 结果集无预算：返回无 `truncated`/`total`/`next`，单次可灌满上下文（正文、符号表、环、依赖图、任务树、下载 base64） | file_diff/find/replace、crontab_list、web_search/fetch、codegraph 6 个查询、vfts_query、mcp_find/load、task_status、browser_run、desktop_run、demo_echo | 各文档 §6；codegraph 索引第 8 条；`crontab_list.md` §6；`web_fetch.md` §6 |
| G7 | P1 | 输入侧 `limit/topN/topK` 无最大值或无上限，且 `total` 是截断后条数，模型把 50 当总量 | codegraph 9、vfts_query、file_find、web_search、mcp_find | `codegraph_symbol_search.md`/`callers`/`callees` §6；`vfts_query.md` §6；`mcp_find.md` §6 |
| G8 | P1 | 静默截断/静默丢弃参数：file `MaxTargets` 200 直接切片、mcp_find limit、task_status limit、task_stop cascade、subchat_open participants、crontab_update 空 cron，均无错误或 skipped | file 11、mcp_find、task_status、task_stop、subchat_open、crontab_update | file 索引 C2；`mcp_find.md` §6；`task_status.md`/`task_stop.md`/`subchat_open.md`/`crontab_update.md` §6 |
| G9 | P1 | 未就绪/构建中的错误文案指向 `hot=false`、LLM 工具面不可见的初始化工具（`codegraph_initialize`/`vfts_index`），且该工具仍可按名调用但模型无可靠发现路径 → 无恢复路径 | codegraph 9、vfts_query | codegraph 索引第 4/5 条；`vfts_query.md` §6；`codegraph_*.md` §6 |
| G10 | P0 | `workdir` 完全由 LLM 填写，不读 `_meta.chonkpilot.work_dir`、不校验当前项目/沙箱；查询还会对目标目录 `MkdirAll` 并读取既有索引 → 跨项目读取 + 越界写 | codegraph 9 | codegraph 索引 A 组第 3 条；5 个工具文档各记 P0 |
| G12 | P0 | UI 自动化插件未接入写沙箱与实例边界：`screenshot` 的 LLM 路径直接落盘、不读 `_meta.chonkpilot.sandbox`；`context_id` 无归属校验，顺序 id 可跨实例操作 | browser_run、desktop_run | `browser_run.md` §6（`main.go:139-149`）；`desktop_run.md` §6（同源 + `server.go:302-314`） |
| G13 | P1 | 超时/取消只放弃返回值，底层 `RemoveAll`/`writeAtomic`/复制/安装继续执行；对不可逆操作返回“失败”但实际已生效 | file_copy/delete/move/iconv、task_run（requires 安装） | 各文件 §6（`toolset/toolset.go:58-63`）；`task_run.md` §6/§7 |
| G14 | P1 | 大对象全量读入内存无上限：`io.ReadAll`/`ReadFile`/`make([]byte,n)`，可被 LLM 参数或上游响应放大 | file_copy/iconv/detect/find、web_download | `file_copy.md`/`file_iconv.md`/`file_detect.md`/`file_find.md` §6；`web_download.md` §6 |
| G16 | P2 | manifest/config 声明与实际消费不一致：`config.scope=prjusr` 却只读写 usr、`config.fields` 为空、`ui.rpc` 未列、权限仅声明；配置页“可配”是假象 | web、mcp、browser、desktop、demo_echo、crontab、task | 各索引“配置面/额外发现”；`demo_echo.md` §6 |
| G17 | P2 | `hot` 是展示过滤而非调用 ACL：`hot=false` 工具仍可被按名调用，白名单分支不看 hot；隐藏初始化工具因此“不可见但可调用” | codegraph、demo-python | codegraph 索引 A 组第 2 条；demo 索引非 hot 结论 |
| G18 | P2 | 契约示例/返回结构与实现漂移（字段名、形状、错误码表、枚举值），模型按文档解析会失败 | file、task_status、codegraph、crontab、web | 各文档 §6 的“契约 vs 实现”行 |
| G19 | P1 | 时间语义缺失：crontab 全部按进程本地时区且 `next_run_at` 只有 epoch 秒；file_find 日期过滤用 UTC 零点对本地 mtime | crontab 6、file_find | `crontab_add/list/update/enable/disable/delete.md` §6；`file_find.md` §6 |
| G21 | P2 | 产物/文档漂移：dist 旧 hot_tools/旧契约、exe 文件名与 manifest 不一致、README/设计文档指向已删路径 | task、crontab、web、mcp | 各索引“范围差异/其他观察” |

> 拆分说明：用户示例把“file 写工具 symlink + browser/desktop screenshot + web_download”并为一类，本文按机制拆开——file 是“路径字符串未解析 symlink”（§4.1 F-P1-4），UI 是“完全未接入沙箱/实例归属”（G12），web_download 是“网络边界无 SSRF 防护”（§4.5 W-P0-1）。三者修复点不同，合并会丢失可执行性。

## 4. 插件特有问题

### 4.1 file（9 条根因）

| 编号 | 级别 | 根因 | 工具 | 证据 |
|---|---|---|---|---|
| F-P0-1 | P0 | 每个 hunk 只校验首条 context/`-` 行，其余上下文不比对、按行号盲消费 → 陈旧/错位补丁“成功”写错位置 | file_patch | `file_patch.md` §6（`patch.go:26,56-78`；契约 `:21`） |
| F-P0-2 | P0 | diff 头两行写入整份文件、正文无 `@@` hunk，产物不是 unified diff；`context` 参数完全未实现 → diff→patch 工作流必然失败 | file_diff | `file_diff.md` §6（`fileops/diff.go:25-27`；`edit.go:154,160`） |
| F-P0-3 | P0 | `dir`/`tree` 模式从不返回目录项：walk 对目录过滤后 `return nil`，`is_dir` 恒 false，tree 目录分支是死代码 | file_find | `file_find.md` §6（`query.go:120-128,146-161,315-327`） |
| F-P1-1 | P1 | `overwrite=false` 用 `os.ReadFile` 成功判定“不存在”，不可读文件被静默覆盖；`encoding=auto` 低置信度回落 UTF-8，可能改源编码 | file_write | `file_write.md` §6（`readwrite.go:672-692,729-745`） |
| F-P1-2 | P1 | `sampleBytes` 无上限，直接 `make([]byte,n)`，LLM 可传超大值导致插件 OOM | file_detect | `file_detect.md` §6（`query.go:358-362`） |
| F-P1-3 | P1 | 任意 `os.Rename` 错误都回退为“跨卷复制+删源”，权限/占用/目标为目录被误报；复制成功删源失败时状态不明 | file_move | `file_move.md` §6（`fsops.go:103-121`） |
| F-P1-4 | P1 | 7 个写工具的沙箱只做路径字符串前缀判定，不解析 symlink/junction；白名单内链接可把写入重定向到白名单外 | file_write/replace/patch/copy/move/delete/iconv | `file_copy.md`/`file_delete.md`/`file_iconv.md`/`file_move.md` §6（`sandbox.go:22-55`；`AllowsPath` 内部 `[待核实]`） |
| F-P1-5 | P1 | 契约在 write/replace/patch/iconv 声称“跨进程锁/锁内校验”，`fileops.acquireLock` 只有定义没有调用点；`expectMd5` 是 TOCTOU | file_write/replace/patch/iconv | file 索引跨工具 4（`lib/fileops/lock.go:36` 全仓无调用） |
| F-P2-1 | P2 | 契约细节簇：delete 的 `idempotent` 与 `missingOk` 矛盾且 `removedFiles` 从不返回；find 的 `size`/`meta`/`depth`/UTC 语义；detect 的 confidence 空串；iconv 的 bom 取值域；replace 的 skipped 用 `reason` | file 11 | 各文档 §6 P2 行 |

### 4.2 task（12 条根因）

| 编号 | 级别 | 根因 | 工具 | 证据 |
|---|---|---|---|---|
| T-P0-1 | P0 | `cwd` 被追加进可写白名单并对该目录打 Low IL；LLM 传任意绝对目录即可在沙箱外新建文件 | task_run | `task_run.md` §6（`src/sandbox.go:27-46`；契约 `task_run.tool.md:16`） |
| T-P0-2 | P0 | `requires` 的 pip/npm 安装以插件进程权限、无沙箱执行，可写 site-packages/`package.json`/`node_modules` | task_run | `task_run.md` §6（`exec.go:138-142,457-493`） |
| T-P0-3 | P0 | 成功停止被报为失败：data.write 路径要求 `{ok}` 应答，`onTaskStop` 成功只写 `v.Result` → 15s/30s 超时，任务实际已取消 | task_stop | `task_stop.md` §6（`main.go:486-490`；`plugins.go:317-327`） |
| T-P1-1 | P1 | `limit` 无效：`tasks`/`tree` 是对象，`trimTasks` 只认 `[]any` → 永不截断；任务树返回形状与契约不一致 | task_status | `task_status.md` §6（`main.go:327-332,384-390`） |
| T-P1-2 | P1 | `agent` 名拼错/不存在时静默退化为全能力子代理（无 persona、无工具收窄） | task_llm | `task_llm.md` §6（`turn.go:660-682,456-459`） |
| T-P1-3 | P1 | 任意退出码 5 被判定“需要管理员权限”；`file`=exe + `runtime=shell` 在 Windows 走 `powershell -File <exe>` 与契约用法矛盾 | task_run | `task_run.md` §6（`exec.go:403-415,329-332`） |
| T-P1-4 | P1 | `cascade` 参数被忽略，恒级联删除子任务/子代理；`cascade=false` 也杀子树 | task_stop | `task_stop.md` §6（`handler_task.go:24-29,61`） |
| T-P1-5 | P1 | `question` 必填但零校验：空串被登记为有效提问；isAskTool 分支跳过落库与 done，MCP 级错误会让 tool 行/任务节点永久 running | ask_user | `ask_user.md` §6（`main.go:406-420`；`turn.go:1614-1621`） |
| T-P1-6 | P1 | 恢复轮不重建 `nextRunMax`/`allowedTools`/`denyTools`，重启后 `next_run` 恒报“未启用”；补齐后 `nextRunItemID` 未恢复会丢新 prompt | next_run | `next_run.md` §6（`retry.go:320-352`；`nextrun.go:78-109`） |
| T-P1-7 | P1 | `participants` 坏项/重复项静默丢弃，有效项 <2 静默降级单聊仍 `ok:true`；`agent/scenario_id/llm` 只校验非空不校验存在 | subchat_open | `subchat_open.md` §6（`groupchat_sched.go:679-700`；`subchat.go:66-103`） |
| T-P1-8 | P1 | `subchat_finish` 无会话类型守卫：主会话/`sub_session` 调用也会把 lifecycle 写为 `ready` 并广播 | subchat_finish | `subchat_finish.md` §6（`subchat.go:148-162`；`session_facade.go:267-313`） |
| T-P2-1 | P2 | 契约细节簇：4 个迁移工具无 `# 返回值`；`next_run` 只回裸 `OK`；`task_run` 截断无标记/`maxOutputKB`；`task_status` state 枚举未说明；旧路径/旧 dist 残留 | task 8 | task 索引 B4/B5；各文档 §6 P2 行 |

### 4.3 crontab（8 条根因）

| 编号 | 级别 | 根因 | 工具 | 证据 |
|---|---|---|---|---|
| K-P1-1 | P1 | `work_dir` 空串可通过白名单并建成任务，首次触发 `work_dir_missing` 自动停用且不通知 LLM；enable 也不校验 | crontab_add/toggle | `crontab_add.md`/`crontab_toggle.md` §6（`main.go:606-619`；`schedule.go:232-236,304-311`） |
| K-P1-2 | P1 | `@every 1s` 被接受，与契约“不支持秒级”矛盾；`@every`/`@yearly`/`@midnight` 未文档化 | crontab_add/update | `crontab_add.md` §6（`schedule.go:18`；`parser.go:424-428`） |
| K-P1-3 | P1 | `Store` 先改内存后落盘；persist 失败造成“报告失败但状态已变”或重启复活 | add/update/toggle/delete | `crontab_add.md`/`crontab_toggle.md`/`crontab_delete.md` §6（`store.go:169-210`） |
| K-P1-4 | P1 | 先落盘后同步 PowerShell 通知；`notify-fallback=window` 可阻塞到用户点击，超过 gateway 60s 后模型重试产生重复任务 | crontab_add | `crontab_add.md` §6（`main.go:243-252`；`notify_windows.go:57-96`） |
| K-P1-5 | P1 | `idempotent:true` 但重复 enable 以 `now` 重算并覆盖 `next_run_at`，可能丢弃已到期未认领的一次执行 | crontab_toggle | `crontab_toggle.md` §6（`main.go:355-359`） |
| K-P1-6 | P1 | misfire/overlap 跳过只写 `runs.jsonl`，不写 `Task.LastRun`，且无 LLM 工具可读 runs → 模型把“上次成功”当“一直正常” | crontab_list | `crontab_list.md` §6（`schedule.go:135-143,212-215`） |
| K-P1-7 | P1 | `cron:""`/全空白被静默当作“不修改”，不报错不回显；成功返回无 `changed`/前后对比，no-op 与真改同形 | crontab_update | `crontab_update.md` §6（`main.go:267-274,310-317`） |
| K-P2-1 | P2 | 字段/示例簇：`next_run_at` 仅 epoch、无 `next_run_local`；list 无 limit/output_tail 4KB；`RunRecord.ExitCode/MS` omitempty；错误文案与契约示例不一致 | crontab 5 | `crontab_list.md`/`crontab_add.md` §6 P2 行 |

### 4.4 codegraph（9 条根因）

| 编号 | 级别 | 根因 | 工具 | 证据 |
|---|---|---|---|---|
| CG-P1-1 | P1 | description 写“id 优先”，实现是 name 优先（仅 name 空才读 id）；传 id+name 时 id 被忽略 | codegraph_callers | `codegraph_callers.md` §6（`server.go:105,475-486`） |
| CG-P1-2 | P1 | `resolved` 仅全项目唯一同名时为 true，公共方法名几乎永不解析；false 同时表示“多处同名”与“不存在”；多命中只取 `hits[0]`；非函数符号恒 `total=0` | codegraph_callees | `codegraph_callees.md` §6（`graph.go:486-501`；`server.go:507-516`） |
| CG-P1-3 | P1 | 多命中只返回 `hits[0]`，`matches>1` 时看不到其余候选；`file` 是大小写敏感精确路径，与 symbol_search 的子串语义相反 | codegraph_get_symbol_info | `codegraph_get_symbol_info.md` §6（`graph.go:406-422`；`path.go:344-353`） |
| CG-P1-4 | P1 | 空结果不可区分：文件未索引、语言不支持、query/kind/file 写错、索引为空都返回 `total:0`；与 `not_found`/参数错误两种形态并存 | symbol_search/callers/callees/dependency_graph | 各文档 §6（`graph.go:365-398`；`lang.go:22-30`） |
| CG-P1-5 | P1 | description 承诺 func/method，默认 `minCc=1` 时 type/class 以 complexity=1 混入结果 | codegraph_analyze_complexity | `codegraph_analyze_complexity.md` §6（`server.go:150`；`graph.go:567-576`） |
| CG-P1-6 | P1 | 无 `file` 过滤时一次返回全项目依赖表、无 limit/分页/截断；`require()`/动态 `import()` 与 Go 分组 import 大概率未提取 | codegraph_get_dependency_graph | `codegraph_get_dependency_graph.md` §6（`server.go:519-526`；`extract.go:372-392`，分组 import `[推断·高置信]`） |
| CG-P1-7 | P1 | 每次查询 `EnsureReady→Reconcile` 全仓 walk+stat、可能写索引，同 workdir `recMu` 串行；`Initialize` 不持 `recMu`，与单飞约定不一致 | codegraph 5/9 | codegraph 索引 A 组第 9 条（`index.go:182-233,276-390`） |
| CG-P1-8 | P1 | vfts 会话索引只覆盖主会话；子会话/其它 `session_id` 查询静默 0 命中 | vfts_query | `vfts_query.md` §6（`sessionindex.go:4-7,191`） |
| CG-P2-1 | P2 | `status.json` 查询门无生产写方（spec 已记录退场）；module_summary 的 path 前缀大小写敏感、统计口径未声明；参数数字只认 float64 | codegraph 9 | codegraph 索引第 8 条附带发现；各文档 §6 P2 行 |

### 4.5 web（2026-11-10 复核：P0/P1 已修，余 1 条 P2）

| 编号 | 级别 | 根因 | 工具 | 现状 |
|---|---|---|---|---|
| W-P0-1 | ~~P0~~ 已修 | 无 SSRF/内网防护 | web_download | 已默认拒绝 loopback/私网/链路本地，重定向逐跳校验，`headers` 不允许覆盖 `Host` |
| W-P1-1 | ~~P1~~ 已修 | 非 UTF-8 文本一律 base64 | web_download | 已按 Content-Type charset 解码 GBK/GB18030/UTF-16，失败才 base64 |
| W-P1-2 | ~~P1~~ 已修 | 上游 >8MiB 静默截断→JSON 解析失败 | web_search/web_fetch | 已改为明确报错并提示降低 count/关闭 need_content/改用 web_download |
| W-P2-1 | P2 | `config.scope=prjusr` 但只用 usr 层；`fetch` 复用 search 超时键；status 页不能区分工具；错误文案中英混杂 | web 3 | 详见 web 索引“跨工具一致性（遗留）” |

### 4.6 mcp（5 条根因）

| 编号 | 级别 | 根因 | 工具 | 证据 |
|---|---|---|---|---|
| M-P0-1 | P0 | 不传 `request_id`，桥按命令类 fire-and-forget，插件把 `{accepted:true}` ack 当结果返回 → 目标结果/失败/超时/取消全部丢失 | mcp_invoke | `mcp_invoke.md` §6（`main.go:264-270`；`capabilities.go:135,222-248`） |
| M-P0-2 | P0 | 不透传 `instance_id/session/turn/tool_call_id/purpose`：instance-scoped 工具报 unknown 且被吞，终态无法回填，展示名丢失 | mcp_invoke | `mcp_invoke.md` §6（`main.go:262-266`；`sdk/plugin.go:61-73`；`contextmeta.go:41-45`） |
| M-P0-3 | P0 | `mcp_invoke` 可自调用且无深度/次数限制；llm-server 只对顶层 `args.name` 判白 → 嵌套调用绕过场景工具白名单 | mcp_invoke | `mcp_invoke.md` §6（`main.go:240-271`；`toolgate.go:91-108`；`turn.go:1470-1481`） |
| M-P1-1 | P1 | mcp_find 检索面是全局注册表而非当前实例；`limit` 静默截断；`type=all` 工具先占满额度；prompts/resources 读取失败被吞成 0 | mcp_find | `mcp_find.md` §6（`main.go:143-169,275`；`primitives.go:64`） |
| M-P2-1 | P2 | 非 tool 分支缺 `kind` 参数（宿主指引却要求传）、返回正文/JSON 双份；宿主指引 `purpose/kind` 已过期；两套 meta 实现并存 | mcp_load/find | `mcp_load.md` §6；mcp 索引“额外发现”1/2 |

> `mcp_load` 的 P0（instance scope 导致技能/资源恒 not found）见 `mcp_load.md` §6，本文并入 M-P0-2 的“调用上下文缺失”根因；其修复同属“能力调用必须带 instance_id”，故不单列，避免重复计数。

### 4.7 browser（4 条根因）

| 编号 | 级别 | 根因 | 工具 | 证据 |
|---|---|---|---|---|
| B-P0-1 | P0 | 描述/示例写 `eval(expr)`，实现只读 `args.script`，`js` 别名不可发现 → 按描述调用必得 `script is required` | browser_run | `browser_run.md` §6（`main.go:79,89`；`engine_webview.go:150-154`） |
| B-P1-1 | P1 | 实例→context 映射只在插件内存；janitor（10min）或引擎崩溃重启后不失效、无重建 → 后续永久 `unknown_context` | browser_run | `browser_run.md` §6（`server.go:340-375`；`client.go:218-239`） |
| B-P1-2 | P1 | `wait.timeoutMs` 无上限且引擎不可取消；所有 context 共用同一 WebView2 profile，跨实例共享 cookie/localStorage | browser_run | `browser_run.md` §6（`engine_webview.go:213-223`；`main.go:158-164`） |
| B-P2-1 | P2 | `args` 无子 schema，描述与实际参数名不符（`wait` 的 ms 模式、`text` 可选 selector、`screenshot(path?)`）；未配默认目录时省略 path 必失败 | browser_run | `browser_run.md` §6（`main.go:78-92`） |

### 4.8 desktop（5 条根因）

| 编号 | 级别 | 根因 | 工具 | 证据 |
|---|---|---|---|---|
| D-P0-1 | P0 | `screenshot` 不带目标窗口时 `rect=nil` 被解引用，引擎全仓无 `recover()` → 进程崩溃、所有 context 丢失 | desktop_run | `desktop_run.md` §6（`engine.go:132-155`；`capture_export.go:13-19`） |
| D-P0-2 | P0 | 10 个动作（window_info/focus/click/input/key/move/size/minimize/maximize/restore 及带目标 screenshot）要求目标窗口，schema/描述只写 `click(x,y)`；`start` 不传 meta，`bind at start` 不可达 | desktop_run | `desktop_run.md` §6（`main.go:78-92,152-166`；`engine.go:210-218`） |
| D-P1-1 | P1 | `move`/`size` 的 `SetWindowPos` 缺 `SWP_NOSIZE`/`SWP_NOMOVE`：move 把尺寸置 0，size 把窗口移到 (0,0)，仍返回成功 | desktop_run | `desktop_run.md` §6（`desktop/win32.go:66-75`） |
| D-P1-2 | P1 | `list_windows` 返回全部顶层窗口，无 `class`/`visible`，模型难以定位目标；同名/隐藏窗口风险高 | desktop_run | `desktop_run.md` §6（`engine.go:59-65,277-291`） |
| D-P2-1 | P2 | screenshot 的“目标窗口 vs 整屏”语义未声明；同 context 串行但不同 context 对同一窗口无互斥；`Close()` 空操作 | desktop_run | `desktop_run.md` §6/索引跨工具发现 |

### 4.9 demo-python（2 条根因）

| 编号 | 级别 | 根因 | 工具 | 证据 |
|---|---|---|---|---|
| E-P1-1 | P1 | `required:["text"]` 不生效：缺失/空串/`null` 全部 `isError=false`，输出空回显或 `None` | demo_echo | `demo_echo.md` §6（`plugin.py:82-86,193,181`） |
| E-P2-1 | P2 | 模板/配置簇：`config.fields` 为空 → `greeting/echoLimit/flaky` 不可配；README 与 manifest/plugin.json 漂移；省略 `hot_tools` 会回落 `["*"]`，模板复制有风险 | demo_echo | demo 索引“模板风险”；`demo_echo.md` §6 |

## 5. Top 10 优先修复（P0 → P1）

1. **P0｜mcp 元工具链**：`mcp_invoke` 补 `request_id` 走同步、透传 `instance/session/turn/tool_call_id/purpose`、禁止自调用；`mcp_load` 非 tool 分支补 `instance_id`。否则“检索→加载→调用”主链路假成功，且可绕过工具白名单。
2. **P0｜task_run 两处沙箱绕过**：`cwd` 不再进写白名单（仅作执行目录，且限 work_dir 子树）；`requires` 安装纳入沙箱或白名单化。
3. **P0｜browser/desktop 实例与沙箱边界**：`screenshot` 路径必须过 `_meta.chonkpilot.sandbox`；`context_id` 校验归属；desktop `screenshot` 无目标改为显式整屏或报错；目标窗口参数在 schema 中声明；`move/size` 补 `SWP_NOSIZE/SWP_NOMOVE`。
4. **P0｜codegraph workdir**：改为读 `_meta.chonkpilot.work_dir`（或强制项目内路径并拒绝相对路径），取消查询时的 `MkdirAll`；9 个工具共用一处修复。
5. **P0｜web_download SSRF**：拒绝环回/私网/链路本地/`localhost`，重定向后重新校验，`headers` 不允许覆盖 `Host`；同时给 `maxBytes` 加硬上限。
6. **P0｜task_stop 成功报失败**：改走 `plugin/v1/task.stop` 带 `request_id`（或 `task.cancel`），统一返回 `{ok,cancelled,task_id,state}`；真正实现 `cascade`。
7. **P0｜task_llm 15s 桥超时**：delegate 调用单独放大超时（按 `waitSec` 且有界），或默认“先 ack task_id + 自动 detach”，不再让 >15s 委派必然失败。
8. **P0｜file_patch hunk 校验**：按 `@@` 校验全部上下文/删除行与行数，任何不匹配整项失败；同时修 `+++`/`---` 内容行被吞与 CRLF。
9. **P0｜file_diff 输出**：头部改文件路径、正文补 `@@ -a,b +c,d @@` 与行号，实现 `context`；否则 diff→patch 工作流不可用。
10. **P0｜file_find dir/tree**：目录作为 item 返回（`is_dir:true`），dir 模式只列直接子项；`root` 非目录/symlink 显式失败。

> 未进 Top 10 的 P1 首项：mcp_find 全局检索面、codegraph 未就绪文案、crontab work_dir 死任务、task_status limit/形状、file 写锁缺失——均在第 3/4 节列出。

## 6. 低成本高收益改进（LLM 视角）

只收“改 schema/文案/默认值/返回字段”即可显著降低模型犯错的项；不含需要重写实现的安全项。

| 改动 | 覆盖工具 | LLM 收益 |
|---|---|---|
| 所有列表/查询返回补 `total` + `truncated`（或 `next`/`next_after`） | file_find/file_diff、crontab_list、web_search/fetch、codegraph 6、vfts_query、mcp_find/load、task_status、demo_echo | 模型知道“还有没有”，不再把 50 当总量 |
| file `finish()` 在 `summary.failed>0 && ok==0` 时置 `isError=true`（或返回 `ok:false`） | file 11 | 单目标失败不再被当成功 |
| `MaxTargets` 默认与 schema 对齐（2000）或返回 `summary.truncated` + skipped 项 | file 11 | 不再静默少做 1800 项 |
| 契约 `# 摘要` 第一段补关键约束（阻塞/非阻塞、结果不回流、沙箱、批量、上限） | file 11、task 8 | 模型从工具列表即可正确规划，不靠调用后学习 |
| codegraph 未就绪文案改为“索引未就绪，请在插件设置中初始化/等待”并将门禁错误置 `isError=true` | codegraph 9、vfts_query | 不再指示调用不可见工具，模型有可执行恢复路径 |
| `demo_echo` 对 `text` 缺失/空串返回参数错误 | demo_echo | 区分“回显为空”与“参数没传” |
| web_search schema 补 `count{default:10,min:1,max:50}`、`need_content{default:false}`，返回 `requested/effective_count` | web_search | 少猜默认值 |
| web_download 4xx/5xx 置错误、`maxBytes` clamp、错误文案给下一步 | web_download | 不再把 404 页面当成功保存 |
| crontab_list 加 `limit`（默认 50）+ `output_tail` 默认 512B + `next_run_local`/`timezone` | crontab_list | 直接压住数百 KB 上下文并免除时区换算 |
| task_status 真正切 `tasks.list`/`tree.nodes` 并返回 `total/truncated`；契约改成真实形状 | task_status | 一次解析成功、可控 token |
| file_delete 返回 `removedFiles`/`removedBytes`；file_detect 返回 `confidence:"unknown"` + `encoding:""` | file_delete/file_detect | 审计可核对，不再踩空枚举 |
| browser 同时接受 `expr` 与 `script`（别名）+ schema 写明；screenshot 默认目录缺失时提前报错 | browser_run | 按描述调用即可成功 |
| desktop `list_windows` 补 `class`/`visible`；schema 声明“目标窗口必填/可省略=整屏” | desktop_run | 可定位窗口，不再盲选 |
| mcp_invoke 对 `name="mcp_invoke"` 直接拒绝并提示；`arguments` 非 object 报参数错误 | mcp_invoke | 消除无意义自套与空参数执行 |
| task ask_user 对空 `question` 报错 | ask_user | 参数错误立即可见，不产生用户看不见的提问 |

## 7. 需要拍板的 breaking change

只列会改变 LLM 可见契约或既有调用方行为的项；“需同步”列说明改动面。

| # | 变更 | 影响面 | 需同步 |
|---|---|---|---|
| 1 | `mcp_invoke` 从“异步 ack”改为同步返回真实结果，并透传完整调用上下文 | 返回体从 `{ok,result:{accepted}}` 变为目标结果；受 SDK 15s 限制 | gateway 桥、SDK 超时策略、契约/示例、前端展示、测试 |
| 2 | 禁止 `mcp_invoke` 自调用/限制递归深度 | 现有 `mcp_invoke(mcp_invoke(...))` 调用会失败 | 插件准入、gateway 或插件侧深度限制、指引文案 |
| 3 | `task_stop` 返回统一 `{ok,cancelled,task_id,state}`，并真正实现 `cascade=false` | 成功由“报错”变“成功”；`cascade=false` 不再杀子树 | 宿主 `task.stop` 应答、契约、任务树/前端、测试 |
| 4 | `task_status` 返回形状改为 `tasks:{list:[]}`、`tree:{nodes:[]}`，`limit` 真正生效 | 按旧契约解析的调用方会失败 | 契约、插件、前端/任务树展示、测试 |
| 5 | `subchat_open` 的 `participants` 有效项 <2 时改为报错；建会话前校验 `agent/llm/scenario_id` | 现有“静默降级单聊”的调用会失败 | 契约、错误文案、测试 |
| 6 | `subchat_finish` 增加会话 `kind` 守卫，非子聊天返回失败 | 主会话/子会话调用由“成功”变“失败” | 契约、前端 `subchat-state` 过滤、测试（产品需确认群聊主 agent 是否允许） |
| 7 | `file_diff` 输出改为标准 unified diff（路径头 + `@@` + 行号），并实现 `context` | 依赖旧“全文头/无 hunk”输出的调用方/提示词会变 | 契约示例、`file_patch` 联调、测试 |
| 8 | codegraph `workdir` 改为 `_meta` 注入/项目内校验，查询不再 `MkdirAll` | schema 的 `required:["workdir"]` 与直接调用方式改变 | 契约/schema、GUI 内部搜索调用（`local.go:706-710`）、测试 |
| 9 | `web_download` 4xx/5xx 置错误、`maxBytes` 强制 clamp、`timeoutSec` 上限收紧 | 原本“成功保存错误页/设超长超时”的调用会失败 | 契约、配置页说明、测试（内网调试需 `allowPrivate` 开关，产品拍板） |
| 10 | `file_delete` 的 `idempotent:true` 与 `missingOk` 二选一；`file_find` 的 `depth` 缺省改 1 | 重复删除/省略 depth 的行为变化 | 契约、测试、调用方提示词 |
| 11 | crontab 拒绝 `@every`（或最小 60s）、空 `cron` 报错、list 默认 limit | 既有 `@every` 任务/传空串求稳的调用失配 | 契约、迁移说明、调度器、测试 |
| 12 | `demo_echo` 的 `required:["text"]` 真正生效 | 空/缺失 `text` 由“成功空回显”变“参数错误” | 契约/示例、模板文档、测试 |
| 13 | desktop `screenshot` 无目标改为整屏或报错，目标窗口参数进 schema；`start` 支持 meta 绑定 | 现有按描述省略目标窗口的调用行为改变 | schema、引擎、契约、测试（产品确认整屏语义） |
| 14 | file 批量超过 `MaxTargets` 由“静默截断”改为“报错或返回 skipped” | 大批量调用从“部分成功”变“显式失败/可感知” | 契约、配置默认值、测试 |

## 8. 建议的后续批次（按“契约/实现/测试/展示”分组）

**契约**
- 统一错误信封：所有插件失败返回 `{ok:false,error,code}`（或保留 isError + JSON 正文），删除“契约写 `{ok:false}` 但实际裸文本”的漂移（crontab/file/task/web）。
- 统一截断/分页字段：`total/returned/truncated/next` 命名一致；补齐 `maxItems/maximum/default` 与 `additionalProperties:false`。
- 契约首段承载关键约束；补 `# 返回值` 段（task 4 个迁移工具、crontab 6、web 3、demo）。
- 删除装饰性 `async/timeout/idempotent` 或明确标注“仅文档”，避免误导（file/task/crontab）。
- 修正 task_status 形状、file_delete idempotent、codegraph callers id/name 优先级、file_find depth 等已知不一致。

**实现**
- 安全批次：task_run cwd/requires、file 写工具 symlink、browser/desktop screenshot+context、web_download SSRF、mcp_invoke 自调用。
- 正确性批次：mcp_invoke request_id/上下文、mcp_load instance_id、task_stop 应答/cascade、task_llm 超时、file_patch hunk、file_diff 格式/context、file_find 目录、codegraph workdir。
- 体量批次：所有 limit/topN/topK 加上限与 truncated；crontab_list 分页/裁剪；web 8MiB 显式错误；file 批量 MaxTargets。
- 状态一致性批次：crontab Store 落盘顺序、通知异步化、misfire 写 LastRun；file 锁接线；task_status 切片。

**测试**
- 端到端：插件 `Text` → MCP → gateway → worker（task 迁移后无真实链路用例）。
- 安全回归：cwd/requires 绕过、junction/symlink、screenshot 任意路径、context_id 越权、SSRF/重定向、mcp_invoke 自调用。
- 数据正确性：CRLF 补丁、hunk 上下文错位、diff→patch 往返、GBK/UTF-16 转码、并发同名下载、批量 targets 截断。
- 语义：4xx 下载、空 question、participants 降级、cascade=false、task_stop 成功路径、恢复轮 next_run。
- 索引：Go grouped import 语料、大仓库 Reconcile 延迟/并发、vfts 子会话。

**展示**
- 工具描述：首段补阻塞/返回值/沙箱/上限；删除“见摘要”悬空引用。
- 返回展示：`truncated`/`total`/`next`、`next_run_local`/`timezone`、`removedFiles`、`mode:"single|group"`。
- 错误展示：每条失败给“下一步”（重试/换工具/初始化/改参数），不再只透出 Go errno。
- 嵌套调用：`purpose`/`tool_call_display_name` 透传到 mcp_invoke 内层与 subchat 子会话。
- 状态页：web 覆盖 download、crontab 展示 misfire/overlap、codegraph 暴露初始化状态。

## 9. 文档索引

- 全局：`E:/BizWorks/chonkpilot-plugins/doc/tool-review/00-cross-plugin-summary.md`（本文件）
- file：`E:/BizWorks/chonkpilot-plugins/file/doc/tool-review/00-index.md`；11 篇 `file_read/file_write/file_replace/file_patch/file_diff/file_find/file_copy/file_move/file_delete/file_iconv/file_detect.md`
- task：`E:/BizWorks/chonkpilot-plugins/task/doc/tool-review/00-index.md`；8 篇 `task_run/task_llm/task_status/task_stop/ask_user/next_run/subchat_open/subchat_finish.md`
- crontab：`E:/BizWorks/chonkpilot-plugins/crontab/doc/tool-review/00-index.md`；6 篇 `crontab_add/update/list/enable/disable/delete.md`
- codegraph：`E:/BizWorks/chonkpilot-plugins/codegraph/doc/tool-review/00-index.md`；9 篇 `codegraph_symbol_search/get_symbol_info/callers/callees/get_dependency_graph/find_circular_deps/analyze_complexity/get_module_summary/vfts_query.md`
- web：`E:/BizWorks/chonkpilot-plugins/web/doc/tool-review/00-index.md`；3 篇 `web_search/web_fetch/web_download.md`
- mcp：`E:/BizWorks/chonkpilot-plugins/mcp/doc/tool-review/00-index.md`；3 篇 `mcp_find/mcp_load/mcp_invoke.md`
- browser：`E:/BizWorks/chonkpilot-plugins/browser/doc/tool-review/00-index.md`；`browser_run.md`
- desktop：`E:/BizWorks/chonkpilot-plugins/desktop/doc/tool-review/00-index.md`；`desktop_run.md`
- demo-python：`E:/BizWorks/chonkpilot/src/initdata/capability/plugins/demo-python/doc/tool-review/00-index.md`；`demo_echo.md`
- 专项（三层一致性）：`E:/BizWorks/chonkpilot-plugins/doc/tool-review/00-tool-invocation-common-review.md`（YAML 契约 / 界面工具设置 / gateway 实际处理）
- 目标架构与修改方案：`E:/BizWorks/chonkpilot-plugins/doc/tool-review/00-tool-invocation-target-and-plan.md`（基于 7 条要求）