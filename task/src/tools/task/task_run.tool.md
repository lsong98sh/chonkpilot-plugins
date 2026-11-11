# 基本信息
```yaml
name: task_run
title: 执行命令/脚本
args: task_run --input={RAW-INPUT-FILE}
async: never
category: task
hot: true
runtime: plugin
idempotent: false
output: stdout
timeout: 600
```

# 摘要
执行一条本机命令或脚本（前台执行，带超时，返回退出码与输出）。`command` 与 `file` **二选一**：前者是脚本/命令内容，后者是脚本文件的绝对路径。`runtime` 缺省 `shell`（Windows = PowerShell，其它平台 = `/bin/sh`），可选 `cmd` / `bash` / `powershell` / `python` / `js` / `vbs` / `java`。**跑 exe 不需要额外 runtime**：Windows 下 `runtime=shell` + `file` 会按扩展名分派 —— `.ps1` → `powershell -File`、`.bat/.cmd` → `cmd /c`、`.exe/.com` → 直接执行；其它扩展名明确报错（请改用 `runtime=cmd` 或显式 interpreter）。工具以**调用方进程权限**运行，不提权、不弹 UAC；只有**启动**失败于 740/ACCESS_DENIED 才返回“需要管理员权限”提示（运行期退出码 5 是普通失败）。**沙箱**：开关开启时脚本以 Low Integrity Level 运行，只能写项目「可写目录」白名单（`work_dir` + `.chonkpilot/tmp` + 配置目录，递归），读取不受限；未开启时不隔离。**cwd 只作执行目录**，必须是 `work_dir` 子树（越界拒绝），不会因此获得写权限。**requires** 安装到项目内 `work_dir/.chonkpilot/deps`（并把该目录加入本次执行环境），不写全局 site-packages / node_modules；缺少 `work_dir` 时明确报错；安装已沙箱化、随调用取消、耗时计入 `ms`。**输出**：默认只返回前 `maxOutputKB`（或配置上限）字节预览；超过阈值时完整 stdout+stderr 写入 `<work_dir>/.chonkpilot/tmp/task-run-*.log`（无 `work_dir` 用系统临时目录，0600，**不自动删除**），并在返回值给出 `output_file` / `total_bytes` / `output_bytes`；未超过阈值不落盘。Windows 下 `env` 变量名大小写不敏感（`PATH`/`Path` 视为同一个变量，子进程只保留一份）。

# 参数
```json
{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "description": "脚本/命令内容（与 file 二选一）"
    },
    "file": {
      "type": "string",
      "description": "脚本文件绝对路径（与 command 二选一；支持 ~/ 用户目录、!/ 临时目录）"
    },
    "runtime": {
      "type": "string",
      "enum": ["shell", "cmd", "bash", "powershell", "python", "js", "vbs", "java"],
      "description": "执行 runtime（缺省 shell；Windows 的 shell = PowerShell）"
    },
    "requires": {
      "type": "array",
      "items": { "type": "string" },
      "description": "依赖包（python → pip install --target；js → npm install --prefix）。安装在项目内 work_dir/.chonkpilot/deps，不写全局 site-packages/node_modules；需要 work_dir；安装受沙箱约束、随调用取消，耗时计入 ms"
    },
    "interpreter": {
      "type": "string",
      "description": "显式解释器/可执行文件路径（绝对路径或 ~/ 开头；优先级最高）"
    },
    "cwd": {
      "type": "string",
      "description": "工作目录（绝对路径；必须是会话 work_dir 的子树，越界拒绝；缺省 = 会话 work_dir / 插件默认目录）。只影响执行位置，不会获得额外写权限"
    },
    "timeoutSec": {
      "type": "integer",
      "minimum": 1,
      "maximum": 600,
      "description": "超时秒数（缺省用插件配置；超时终止整棵进程树）"
    },
    "maxOutputKB": {
      "type": "integer",
      "minimum": 1,
      "maximum": 4096,
      "description": "本次调用的输出预览上限（KB，可选）。超过插件配置上限时夹到配置值；缺省用配置。输出超过该值时完整 stdout+stderr 落盘，见返回值 output_file"
    },
    "env": {
      "type": "object",
      "additionalProperties": { "type": "string" },
      "description": "追加/覆盖的环境变量（同名优先于继承环境；Windows 下变量名大小写不敏感，PATH/Path 视为同一个变量）"
    },
    "args": {
      "type": "array",
      "items": { "type": "string" },
      "description": "追加到命令行的参数"
    }
  }
}
```

# 返回值
成功且未超输出上限：
```json
{
  "runtime": "shell",
  "command": "echo hi",
  "cwd": "E:\\proj",
  "exit_code": 0,
  "ms": 120,
  "output": "hi",
  "truncated": false,
  "output_bytes": 2,
  "total_bytes": 2,
  "ok": true
}
```

输出超过上限（完整输出已落盘）：
```json
{
  "runtime": "shell",
  "command": "<原命令>",
  "cwd": "E:\\proj",
  "exit_code": 0,
  "ms": 850,
  "output": "<前 N 字节预览>\n…[输出已截断：完整输出见 E:\\proj\\.chonkpilot\\tmp\\task-run-123456.log]",
  "truncated": true,
  "output_file": "E:\\proj\\.chonkpilot\\tmp\\task-run-123456.log",
  "output_bytes": 262144,
  "total_bytes": 524288,
  "ok": true
}
```

字段语义：
- `output`：输出预览（最多 `maxOutputKB` 字节；截断时尾部附提示）。
- `truncated`：stdout+stderr 是否超过上限（无论是否成功落盘）。
- `output_file`：仅超限时出现，完整 stdout+stderr 的绝对路径（`<work_dir>/.chonkpilot/tmp/task-run-*.log`；无 `work_dir` 时为系统临时目录）。文件 0600，**不自动删除**，需要时由调用方自行读取/清理。未超限不创建文件。
- `output_bytes`：预览字节数（不含截断提示）。
- `total_bytes`：stdout+stderr 原始总字节数。
- `output_file_error`：落盘失败原因；此时 `output_file` 为空，`output` 仍为截断预览，脚本执行不受影响。
- `ms`：包含 `requires` 安装耗时的总耗时。

失败时 `ok:false` 并带 `error`（超时 / 退出码非 0 / 启动需要管理员权限——后者附可复制的命令行）。运行期 `exit 5` 按普通非 0 失败处理，不再误报需要管理员权限。参数级失败（例如 runtime 非法、`shell` + 不支持的脚本扩展名、缺少 `work_dir` 的 `requires`）以工具错误文本返回。
