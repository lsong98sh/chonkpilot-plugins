# 基本信息
```yaml
name: file_write
title: 写入文件
args: file_write --input={RAW-INPUT-FILE}
async: never
category: file
hot: true
runtime: builtin
idempotent: false
output: stdout
timeout: 120
on_timeout: background
```

# 摘要
按批写入文件（覆盖或新建）。统一参数 `{targets[]}`。

**沙箱**：开启沙箱时，`targets[].path` 必须落在项目「可写目录」白名单内（递归），否则**整次调用被拒绝**（未做任何写入）；未开启沙箱不受此限制。

- **独占写（Windows）**：以独占方式（`dwShareMode=0`）打开目标后**就地读写**，写期间其他应用无法打开/读到半截内容。目标被其他应用打开/锁定时**先跳过**：第一遍继续处理批内其他文件，其余文件处理完后按 50ms / 100ms / 200ms 重试 3 轮（每轮重试所有仍 pending 的文件）；仍失败 → 该项 `failed`（`code:"FILE_IN_USE"`），不写。
- **原子性代价（Windows）**：就地写不再具备“临时文件 + rename”的崩溃原子性；进程在写一半崩溃可能留下部分内容。写失败且已写入部分字节 → 该项附 `partial:true`（目标可能不完整）。
- **非 Windows**：没有真正的独占打开语义，保留“同目录临时文件 + rename”原子写路径（写失败不留半截文件）。
- `overwrite=false` 且目标已存在 → 该项 `failed`（不覆盖）。存在性由独占打开判定：文件存在但被占用/不可写 → `failed`，**不会**当成“不存在”覆盖。
- `createDirs` 默认 true：自动创建父目录；false 且父目录缺失 → 该项 `failed`。
- **编码**：`encoding` 是**按该编码写入**（如 `gbk` 就写 GBK），显式值覆盖默认策略。缺省 `auto`：
  - **新文件**：使用插件配置页的「默认字符集」（未配置 → UTF-8）；
  - **已有文件**：保持原编码回写；若原编码是 Windows 中文 ANSI（GBK/GB18030）且配置了默认字符集，则转为默认字符集；
  - 旧文件检测置信度低 → 该项 `failed`（`encoding ambiguous…`），要求显式传 `encoding`，不再静默改成 UTF-8。
  - 配置页默认字符集为空 = 自动（新文件 UTF-8、旧文件保持原编码）。
- **BOM**：仅对 **UTF-8 输出**生效，**默认无 BOM（`strip`）**。
  - `strip`（默认）：不写 BOM；
  - `add`：写 UTF-8 BOM；
  - `keep`：仅当旧文件本身是 UTF-8 且带 BOM 时保留；
  - 非 UTF-8 编码（gbk/shift_jis/utf-16 等）恒 `bom=none`，`bom` 参数不生效。

**路径要求**：`path` 必须是绝对路径、`~/` 或 `!/` 开头；相对路径 → 该项 `failed`。
**同路径去重（last-wins）**：按解析后的路径分组，同一路径只执行**最后一个** target；前面的重复项 `skipped`，`reason`/`error` = `superseded_by_later_target`。最后一个 target 本身校验/打开/写失败 → 只该项 `failed`，前面的 skipped **不回退执行**。
**并发（批调度）**：同一时刻最多持有一个文件句柄，处理完立即释放，不跨文件持锁。第一遍按入参顺序对每个文件（去重后的 winner）**只尝试一次**独占打开；共享/锁冲突记 pending 并继续处理后面的文件；全部文件处理完后对 pending 做 3 轮 50/100/200ms 重试，仍失败 → `failed` + `code:"FILE_IN_USE"`。非 Windows 的独占是协作 flock（不遵守 flock 的其他应用仍可读写），调度器照常跳过/重试。
**批量语义**：结果数组长度 = 入参 `targets` 长度，**严格按入参顺序**；skipped/failed 都计入 `summary.total`。单个目标失败不影响同批其他目标；批量**非事务**——已成功的写入不回滚，失败/取消可能留下部分已写文件。整体成败看顶层 `ok`。

# 参数
```json
{
  "type": "object",
  "required": ["targets"],
  "additionalProperties": false,
  "properties": {
    "targets": {
      "type": "array",
      "minItems": 1,
      "maxItems": 2000,
      "description": "写入目标；单文件也写成 1 个元素的数组。插件配置 plugins.file.max-targets 可调低（默认 2000），超过配置值将直接参数错误（不静默截断）",
      "items": {
        "type": "object",
        "required": ["path", "content"],
        "additionalProperties": false,
        "properties": {
          "path": {
            "type": "string",
            "description": "目标文件路径（必须为绝对路径、~/ 或 !/ 开头）"
          },
          "content": {
            "type": "string",
            "description": "文件内容（完整覆盖）"
          },
          "encoding": {
            "type": "string",
            "description": "按该编码写入（auto | utf-8 | utf-16le | utf-16be | gbk | gb18030 | shift_jis | big5 | euc-kr），显式值覆盖默认策略；缺省 auto：新文件用配置页默认字符集（未配置=UTF-8），已有文件保持原编码（GBK/GB18030 可转默认字符集）；auto 低置信度 → 该项 failed，请显式指定"
          },
          "bom": {
            "type": "string",
            "enum": ["keep", "add", "strip"],
            "default": "strip",
            "description": "BOM 处理，仅对 UTF-8 输出生效；缺省 strip（无 BOM）。keep = 旧文件本身是 UTF-8 且带 BOM 时保留；add = 写 UTF-8 BOM；strip = 不写。非 UTF-8 编码恒无 BOM"
          },
          "createDirs": {
            "type": "boolean",
            "description": "自动创建父目录；缺省 true；false 且父目录缺失 → 该项 failed"
          },
          "overwrite": {
            "type": "boolean",
            "description": "允许覆盖已存在文件；缺省 true；false 时目标存在即 failed（不可读/被占用也按存在处理）"
          }
        }
      }
    }
  }
}
```

# 返回值
```json
{
  "ok": false,
  "results": [
    {
      "path": "C:\\proj\\a.json",
      "status": "ok",
      "action": "created",
      "size": 234,
      "md5": "…",
      "encoding": "utf-8",
      "bom": "none"
    },
    { "path": "C:\\proj\\b.txt", "status": "failed", "error": "target exists (overwrite=false)" }
  ],
  "summary": { "total": 2, "ok": 1, "failed": 1, "skipped": 0 }
}
```

`bom` 取值：`none`（无 BOM）或 `utf-8`（写了 UTF-8 BOM）；非 UTF-8 输出恒 `none`。
写失败且已写入部分字节时，该项附 `partial:true`，表示目标内容可能不完整（`file_write` 没有旧内容快照，不自动回滚）。
同路径重复项示例：`{"path":"…","status":"skipped","reason":"superseded_by_later_target","error":"superseded_by_later_target"}`；该文件仍只有最后一个 target 的结果是 `ok`/`failed`。
`summary.total` = 入参 `targets` 数量（skipped/failed 都计入）；`results` 与入参一一对应、顺序不变。
顶层 `ok` = 本次调用无失败项（`summary.failed==0`）；逐项目标失败只写在该项，不中断同批其他目标。
