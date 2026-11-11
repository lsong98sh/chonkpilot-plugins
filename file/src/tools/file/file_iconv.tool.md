# 基本信息
```yaml
name: file_iconv
title: 转换文件编码
args: file_iconv --input={RAW-INPUT-FILE}
async: never
category: file
hot: true
runtime: builtin
idempotent: true
output: stdout
timeout: 300
on_timeout: background
```

# 摘要
按批转换文件编码（如 GBK → UTF-8）。统一参数 `{targets[]}`。转换全程**流式**（O(1) 内存，不整文件读入）。

**沙箱**：开启沙箱时，`targets[].path` 必须落在项目「可写目录」白名单内（递归），否则**整次调用被拒绝**；未开启沙箱不受此限制。

## 源编码
- `from` 缺省 `auto`：复用 `file_detect` 的检测链（UTF-16 BOM → UTF-8 → GBK/ShiftJIS/Big5/EUCKR）。
- **低置信度拒绝自动转**：GBK / Big5 / Shift-JIS 互相撞车时 → 该项 `failed`，错误码 `encoding_undetermined (specify from explicitly)`，并附候选编码名数组；**要求调用方显式指定 `from`**。
- 显式给 `from` 时按以下顺序处理：
  1. 不支持的编码名 → `failed`，错误 `unsupported_encoding: <from>`；
  2. **actual detection**（BOM / 二进制 / 候选）：若检测置信度非 `low` 且检测到的实际编码 == `to` → `skipped`，`reason="already_target"`（保证**重试幂等**：第二次调用不会把已是 UTF-8 的文件再当 GBK 解码）；
  3. 否则做 **round-trip 校验**：将原文件流式解码后按 `from` 重新编码到目标同目录临时文件，再与原字节**分段（64 KiB）比较**，结果必须与原字节**完全一致**；不一致或无法重新编码 → `failed`，错误 `from_mismatch`（附 `detected` / `candidates`），**绝不写文件**；
  4. `from == to`：actual 可靠等于目标编码，或 round-trip 一致 → `skipped already_target`；否则 `from_mismatch`。
- **二进制拒绝**：源文件头部（最多 64 KiB）判定为二进制 → `failed`，错误 `binary file (not a text encoding)`；显式 `from` 也不能绕过。
- `to` 必填：空 / `auto` → `failed`，错误 `to is required`；不支持的编码名 → `failed`，错误 `unsupported_encoding: <to>`。

## 目标与 BOM
- `to` 必填（如 `utf-8` / `gb18030` / `utf-16le`）。
- `bom = keep`（默认：原文件有 BOM 则保留到目标编码；无则不添加）｜ `add`（目标编码支持则加）｜ `strip`（一律去掉）。`add` 仅对支持 BOM 的目标编码（`utf-8` / `utf-16le` / `utf-16be`）生效。
- 返回值的 `bom` ∈ `"none"`（未写 BOM）或目标编码名（实际写入了该编码的 BOM）。
- **legacy 目标无 BOM**：`gbk` / `gb18030` / `shift_jis` / `big5` / `euc-kr` 无法携带 BOM，`keep`/`add` 实际都返回 `bom:"none"`；若显式请求了 BOM（`add` 或源有 BOM 的 `keep`），额外返回 `bom_dropped:true`。
- **中文目标推荐 `gb18030`**（GBK 的超集，覆盖更全）。

## 幂等、流式写入与批调度
- 源已是目标编码 → `status=skipped` + `reason="already_target"`，**不重写文件**（幂等）；already_target 快判在创建任何临时文件之前完成。
- **独占打开 + 流式转码**：每个文件以独占句柄打开；原文件先完整备份到目标同目录 `.ck-iconv-*` 临时文件（同时计算 `bytesBefore` / `md5Before`），随后按 `from → to` 流式写出到另一临时文件（同时计算 `bytesAfter` / `md5After`）。源经 `io.SectionReader` + `transform.Reader` 分段读取，全程 64 KiB 级缓冲，**O(1) 内存**（不整文件 `io.ReadAll`，不新增 `maxFileBytes` 上限）。
- **临时文件**：backup / round-trip / out 均位于目标同目录（`.ck-iconv-*`）；成功后关闭并删除，失败也全部清理。非 Windows 的输出临时文件被 `rename` 后路径已消失，删除为 no-op。
- **提交**：Windows 在独占句柄上 `Seek(0)` → 复制临时输出 → `Truncate(bytesAfter)` → `Sync`；失败用 backup 回滚，回滚失败标 `partial:true`。非 Windows 将临时输出 `Sync`/`Close`、按目标 mode `Chmod` 后 `Rename` 覆盖目标（原子）；失败目标不变，不标 `partial`。
- `expectMd5` 可选：在独占句柄内、任何修改前（backup 与 `md5Before` 已完成）校验；不一致 → `skipped` + 当前 `md5`（无 TOCTOU）。
- **批调度**：先解析全部路径；同一批次内重复 `path` → **后项** `failed`，错误 `duplicate target`（不合并、不做 last-wins，第一项照常执行）。
- 每个有效唯一路径第一遍**只尝试一次**独占打开；共享/锁冲突（`FILE_IN_USE`）记 pending 并继续处理其他文件；全部文件处理完后按 50/100/200ms 重试 3 轮，仍失败 → 该项 `failed` + `code:"FILE_IN_USE"`。
- `results` 按**入参顺序**返回（预分配、按索引填充），`summary.total` = 入参数量。
- 平台差异（写提交）：Windows 为独占句柄**就地写**（放弃 rename 的崩溃原子性）；非 Windows 为“临时输出 + rename”原子写。

**路径要求**：`path` 必须是绝对路径、`~/` 或 `!/` 开头；相对路径 → 该项 `failed`。

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
      "items": {
        "type": "object",
        "required": ["path", "to"],
        "additionalProperties": false,
        "properties": {
          "path": { "type": "string", "description": "目标文件路径" },
          "to": {
            "type": "string",
            "description": "目标编码（utf-8 | utf-16le | utf-16be | gbk | gb18030 | shift_jis | big5 | euc-kr）；空/auto → to is required，不支持的名字 → unsupported_encoding"
          },
          "from": {
            "type": "string",
            "description": "源编码；缺省 auto（低置信度会拒绝，要求显式指定）。显式值会做 actual detection + round-trip 校验，不一致 → from_mismatch 且不写"
          },
          "bom": {
            "type": "string",
            "enum": ["keep", "add", "strip"],
            "description": "BOM 处理；缺省 keep"
          },
          "expectMd5": {
            "type": "string",
            "description": "期望的当前文件 md5；不一致则跳过该项"
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
  "results": [
    {
      "path": "C:\\proj\\a.txt",
      "status": "ok",
      "from": "gbk",
      "to": "utf-8",
      "bom": "none",
      "bytesBefore": 1000,
      "bytesAfter": 1450,
      "md5Before": "…",
      "md5After": "…"
    },
    {
      "path": "C:\\proj\\b.txt",
      "status": "failed",
      "error": "encoding_undetermined (specify from explicitly)",
      "candidates": ["gbk", "shift_jis", "big5"]
    },
    {
      "path": "C:\\proj\\c.txt",
      "status": "failed",
      "error": "from_mismatch",
      "detected": "utf-8",
      "candidates": ["utf-8", "gbk", "big5"]
    },
    {
      "path": "C:\\proj\\d.txt",
      "status": "failed",
      "error": "unsupported_encoding: utf-32"
    },
    { "path": "C:\\proj\\e.txt", "status": "skipped", "reason": "already_target" },
    { "path": "C:\\proj\\e.txt", "status": "failed", "error": "duplicate target" }
  ],
  "summary": { "total": 6, "ok": 1, "failed": 4, "skipped": 1 }
}
```
