# 基本信息
```yaml
name: file_replace
title: 替换文本
args: file_replace --input={RAW-INPUT-FILE}
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
按批做精确文本替换。统一参数 `{targets[]}`。

**沙箱**：开启沙箱时，`targets[].path` 必须落在项目「可写目录」白名单内（递归），否则**整次调用被拒绝**；未开启沙箱不受此限制。

- `find` 为空 → 该项 `failed`（不做"清空文件"这种隐式动作）；同组其他项 `skipped`（`reason:"not_applied"`），该文件不写。
- `all` 缺省 true：替换全部出现；`false` 只替换第一处。
- 未找到 `find` → 该项 `failed`（`search not found`），便于调用方发现"条件写错了"。
- `expectMd5` 可选：**独占打开期间先校验**（Windows 独占句柄，其他应用无法同时写入），不一致 → 同文件所有项 `skipped`（`reason:"md5 mismatch"`，附当前 `md5`）。同组多个**不同**的非空 `expectMd5` → 同文件所有项 `failed`（`reason`/`code` = `conflicting_expectMd5`）；相同值只对**原始内容**校验一次（所有修改前）。
- **同路径合并**：按解析后的路径分组；同一文件多个 target 只**独占打开一次、读一次、写一次**，组内按 `targets[]` 输入顺序依次替换（每项使用自己的 `find`/`replace`/`all`，各自返回 `replacements`）。
- **文件级全有全无**：组内第 k 项应用失败（`find` 不存在等）→ 第 k 项 `failed`，同组其他项 `skipped`（`reason:"not_applied"`），该文件**不写**；打开/读/解码/编码失败 → 同组所有项 `failed`；写失败用 `oldData` 回滚，回滚失败 → 同组所有项附 `partial:true`。
- **独占写（Windows）**：以独占方式（`dwShareMode=0`）打开后**就地读写**；目标被其他应用打开/锁定时**先跳过**（记 pending、继续后面的文件），其余文件处理完后按 50ms / 100ms / 200ms 重试 3 轮，仍失败 → 同文件所有项 `failed` + `code:"FILE_IN_USE"`。就地写不具备 rename 的崩溃原子性；写失败时用内存中的旧内容回滚（Seek 0、写回、Truncate、Sync）。
- **非 Windows**：没有真正的独占打开语义，保留“同目录临时文件 + rename”原子写路径；独占仅是协作 flock。
- **批量语义**：单目标/单文件失败不影响同批其他文件，批量**非事务**（不同文件之间不回滚）。
- **编码**：**解码与回写使用同一个编码名**（`AutoName`/`DecodeAuto` 同序），源文件是 GBK/ShiftJIS/UTF-16 等就按同编码写回，不会“按 A 解码、按 B 回写”；源文件 BOM 状态保持。二进制文件 → 该项 `failed`（不静默按文本改写）。
- **返回精简**：成功项只回 `status` + `replacements` + 实际写回的 `encoding`，**不返回 diff / md5**（避免大文件上下文爆量；需要 diff 用 `file_diff`）。

**路径要求**：`path` 必须是绝对路径、`~/` 或 `!/` 开头；相对路径 → 该项 `failed`。
**并发（批调度）**：同一时刻最多持有一个文件句柄，处理完立即释放。第一遍按入参顺序对每个路径组**只尝试一次**独占打开；共享/锁冲突记 pending 并继续处理后面的文件；全部文件处理完后对 pending 做 3 轮 50/100/200ms 重试，仍失败 → 同文件所有项 `failed` + `code:"FILE_IN_USE"`。非 Windows 的 flock 是协作锁，不遵守者仍可读写。
**结果顺序**：`results` 长度 = 入参 `targets` 长度，**严格按入参顺序**；同文件分组只改变执行方式（一次打开/读/写），不合并或重排结果。skipped/failed 都计入 `summary.total`。

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
        "required": ["path", "find", "replace"],
        "additionalProperties": false,
        "properties": {
          "path": {
            "type": "string",
            "description": "目标文件路径（必须为绝对路径、~/ 或 !/ 开头）"
          },
          "find": {
            "type": "string",
            "description": "要查找的原文字符串（非正则；不能为空）"
          },
          "replace": {
            "type": "string",
            "description": "替换成的文本（可为空串 = 删除匹配）"
          },
          "all": {
            "type": "boolean",
            "description": "替换全部出现；缺省 true（false = 只替换第一处）"
          },
          "expectMd5": {
            "type": "string",
            "description": "期望的当前文件 md5；不一致则跳过该项（防并发覆盖）"
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
  "ok": true,
  "results": [
    {
      "path": "C:\\proj\\a.go",
      "status": "ok",
      "encoding": "gbk",
      "replacements": 3
    },
    { "path": "C:\\proj\\b.go", "status": "skipped", "reason": "md5 mismatch",
      "error": "md5 mismatch", "md5": "…" }
  ],
  "summary": { "total": 2, "ok": 1, "failed": 0, "skipped": 1 }
}
```

成功项只回 `status` + `replacements` + `encoding`，**不返回 diff / md5**；skipped 同时给 `reason` 与 `error`（兼容两种读取方式）。
写失败时用旧内容回滚；回滚也失败 → 同文件所有项附 `partial:true` 并给出明确错误（目标可能不完整）。
同文件组内失败：第 k 项 `failed`（原因见该项 `error`），同组其他项 `skipped` + `reason:"not_applied"`；文件保持原内容。
`results` 与入参 `targets` 一一对应、顺序不变；`summary.total` = 入参数量（skipped/failed 都计入）。
顶层 `ok` = 本次调用无失败项（`summary.failed==0`）。
