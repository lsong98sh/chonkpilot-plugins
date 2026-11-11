# 基本信息
```yaml
name: file_delete
title: 删除文件
args: file_delete --input={RAW-INPUT-FILE}
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
按批删除文件或目录。统一参数 `{targets[]}`。

**沙箱**：开启沙箱时，`targets[].path` 必须落在项目「可写目录」白名单内（递归），否则**整次调用被拒绝**；未开启沙箱不受此限制。

- 删目录必须显式 `recursive=true`（**空目录也一样**），否则该项 `failed`（防误删整棵树）。
- 符号链接只删除链接本身（`Lstat` 判定，不跟随目标）：`type:"symlink"` + `link_target`；不要求 `recursive`，不计算 md5/size。断链 symlink 仍算存在（`missingOk` 只在真正不存在时生效），正常删除链接。
- `missingOk` 缺省 false：目标不存在 → `failed`；`true` → `skipped`（此时重复删除才幂等）。
- 删除前记录 `md5`/`size`（仅普通文件；Windows 的 md5 从持有的独占句柄读取，非 Windows 从路径读取）；目录返回 `removedFiles`（非目录条目数，含 symlink）与 `removedBytes`（普通文件大小 + symlink 的 Lstat 自身大小，平台可能为 0），便于审计与事后恢复判断。文件 `md5` 计算失败时返回 `md5_skipped:"unreadable"`。
- **Windows 持有独占句柄直至 delete-on-close**：目录递归收集普通文件后，逐文件以 `DELETE`（单文件需 md5 时再加 `GENERIC_READ`）权限、`dwShareMode=0`、`OPEN_EXISTING` 打开并**持有句柄不关闭**。第一遍遇到共享/锁冲突的文件记入 pending 并**继续**处理后续文件；第一遍结束后对 pending 做 **3 轮重试，间隔 50ms / 100ms / 200ms**。全部拿到后，逐个 `SetFileInformationByHandle(FileDispositionInfo, {DeleteFile:1})` 标记 delete-on-close，再关闭所有句柄完成删除。任一文件最终拿不到 → 关闭全部已持有句柄（不标记 delete），**一个都不删**，返回 `code:"FILE_IN_USE"` + `locked_path`/`locked_files`；`ACCESS_DENIED`/其他错误 → 对应 `code`，同样一个都不删。标记阶段失败 → 对已标记句柄用 `DeleteFile:0` 清除标记并关闭全部（不删除）；若清除失败，关闭时可能删除部分文件 → `partial:true`。
- **文件全有全无；目录在文件删除后清理**：普通文件通过关闭独占句柄删除；随后 symlink 用 `os.Remove(link)`、空目录自底向上 `os.Remove`（**不用 `RemoveAll`**，避免把扫描/持有后新出现的文件静默删掉）。目录因新文件或被占用而残留 → `partial:true` + 预检计数，明确“文件已删、目录残留”。
- 单个 target 的目录树/文件条目数上限 **1000**（不跨 target 合计）：超过 → 该项 `failed`（`too many files (N > 1000); delete subdirectories in batches`），**不持有句柄、不删除**。
- **非 Windows**：没有独占句柄/delete-on-close 语义，保留「预检 no-op + `RemoveAll`」路径；**不承诺独占或全有全无**（unlink 不会阻止已打开文件）。
- 同批重复 `path`：后项 `failed`（`duplicate target`）。
- **不做回收站**：直接删除；调用方需自行负责（重要数据先 `file_copy` 备份）。
- 超时/取消后删除是否仍继续，属于 toolset 跨工具问题，**本工具不承诺**可在执行中被可靠中断。

**路径要求**：`path` 必须是绝对路径、`~/` 或 `!/` 开头；相对路径 → 该项 `failed`。
**安全**：拒绝删除文件系统根、盘符根、用户主目录根（`C:\`、`C:\Users` 等），该项 `failed`。

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
        "required": ["path"],
        "additionalProperties": false,
        "properties": {
          "path": {
            "type": "string",
            "description": "要删除的文件或目录路径"
          },
          "recursive": {
            "type": "boolean",
            "description": "删除任何目录（含空目录）都必须显式 true；缺省 false"
          },
          "missingOk": {
            "type": "boolean",
            "description": "目标不存在时视为 skipped 而非 failed；缺省 false"
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
    { "path": "C:\\proj\\tmp.log", "status": "ok", "type": "file", "size": 88, "md5": "…" },
    { "path": "C:\\proj\\dist", "status": "ok", "type": "dir", "removedFiles": 12, "removedBytes": 4096 },
    { "path": "C:\\proj\\link", "status": "ok", "type": "symlink", "link_target": "C:\\proj\\dist" }
  ],
  "summary": { "total": 3, "ok": 3, "failed": 0, "skipped": 0 }
}
```

失败项在 `status:"failed"` + `error` 之外，可能带：
- `code`：`FILE_IN_USE` / `ACCESS_DENIED` / `HOLD_FAILED`（仅 Windows 独占持有失败）；
- `locked_path`：持有失败的文件；`locked_files`：重试结束后仍被占用的全部文件（`FILE_IN_USE`）；
- `partial:true`：目录清理残留、delete-on-close 标记回滚失败、或关闭句柄失败，删除范围可能不完整；
- `removedFiles`/`removedBytes`：目录预检统计值（失败时也可能返回）。
