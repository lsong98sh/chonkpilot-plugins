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
执行一条本机命令或脚本（前台执行，带超时，返回退出码与输出）。`command` 与 `file` **二选一**：前者是脚本/命令内容，后者是脚本文件的绝对路径。`runtime` 缺省 `shell`（Windows = PowerShell，其它平台 = `/bin/sh`），可选 `cmd` / `bash` / `powershell` / `python` / `js` / `vbs` / `java`。**跑 exe 不需要额外 runtime**：用 `shell` 或 `cmd`，`file` 填 exe 绝对路径，或 `command` 写 exe 路径 + 参数。工具以**调用方进程权限**运行，不提权、不弹 UAC；遇到必须管理员权限的操作会返回 `ok:false` 并附可复制的命令行。**沙箱**：开关开启时脚本以 Low Integrity Level 运行，只能写项目「可写目录」白名单（`work_dir` + `.chonkpilot/tmp` + 配置目录，递归），读取不受限；未开启时不隔离。

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
      "description": "依赖包（python → pip install；js → npm install --no-save）"
    },
    "interpreter": {
      "type": "string",
      "description": "显式解释器/可执行文件路径（绝对路径或 ~/ 开头；优先级最高）"
    },
    "cwd": {
      "type": "string",
      "description": "工作目录（绝对路径；缺省 = 会话 work_dir / 插件默认目录）"
    },
    "timeoutSec": {
      "type": "integer",
      "minimum": 1,
      "maximum": 600,
      "description": "超时秒数（缺省用插件配置；超时终止整棵进程树）"
    },
    "env": {
      "type": "object",
      "additionalProperties": { "type": "string" },
      "description": "追加/覆盖的环境变量（同名优先于继承环境）"
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
```json
{
  "runtime": "shell",
  "command": "echo hi",
  "cwd": "E:\\proj",
  "exit_code": 0,
  "ms": 120,
  "output": "hi",
  "truncated": false,
  "ok": true
}
```

失败时 `ok:false` 并带 `error`（超时 / 退出码非 0 / 需要管理员权限——后者附可复制的命令行）。
