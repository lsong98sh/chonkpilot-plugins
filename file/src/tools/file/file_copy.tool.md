# 基本信息
```yaml
name: file_copy
title: 复制文件
args: file_copy --input={RAW-INPUT-FILE}
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
按批复制文件或目录（递归）。统一参数 `{targets[]}`。

**沙箱**：开启沙箱时，`targets[].to`（目标）必须在项目「可写目录」白名单内（递归），`from`（源）作为删除/读取语义也一并校验，否则**整次调用被拒绝**；未开启沙箱不受此限制。

- 目录复制默认递归（`recursive` 缺省 true）。
- `overwrite` 缺省 false：目标已存在 → 该项 `failed`；`overwrite=true` 时才覆盖/合并。
- **流式复制**：文件用 `io.Copy` 流式写入，不整文件读入内存。**Windows**：目标文件独占打开（`dwShareMode=0`）后**就地复制**（`io.Copy` → `Truncate` → `Sync`），不 rename，放弃崩溃原子性；**非 Windows**：保留同目录临时文件 + `rename` 原子写，失败不留半截目标。复制路径不逐文件 `fsync`。
- **目录复制是合并，不是镜像**：同名文件被覆盖，但目标中多出的文件**不会删除**；目录逐文件独占/原子写，**没有全局目标目录锁**。
- **中间态可判断、可回滚**：目标原本不存在且中途失败 → 回滚删除本次新建的目标树/文件（Windows 就地复制失败时删除新建目标，非 Windows 临时文件从未 rename）；目标原本存在（目录合并，或 Windows 就地覆盖文件）且中途失败 → 保留已有目标，返回 `partial:true`（目录另给 `copiedFiles`）。
- **元数据**：文件复制保留源文件 mode（POSIX 权限位；Windows 仅只读位）与 mtime（Windows 用目标句柄 `SetFileTime` 设置）；新建目录尽量沿用源目录 mode。Windows ACL / 额外文件属性不保留。
- **符号链接**：文件 symlink 按目标文件内容复制（mode/mtime 也取目标文件）；目录 symlink 不被递归跟随。**目标树中出现目录 symlink → 该项 `failed`**（`target path traverses symlink (refused)`），防止目录合并通过预置的 junction/symlink 把写入重定向到白名单外；文件 symlink 目标在 `overwrite=false` 时视为已存在（broken symlink 也算）。
- **重复目标**：同一批内重复的 `to` → 后出现项 `failed`（`error:"duplicate target"`）。
- **并发/独占**：Windows 文件目标以独占句柄打开，复制期间其他应用无法同时打开该文件；目标被其他应用占用 → 该项 `failed` + `code:"FILE_IN_USE"`。文件目标的 `overwrite=false` 判定在 Windows 由独占打开完成（原子），其他情况为 Lstat 检查（非 Windows / 目录目标，best-effort）。**不使用锁文件**。
- 拒绝 `to` 落在 `from` 内部 → 该项 `failed`。

**路径要求**：`from` / `to` 必须是绝对路径、`~/` 或 `!/` 开头；相对路径 → 该项 `failed`。

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
        "required": ["from", "to"],
        "additionalProperties": false,
        "properties": {
          "from": { "type": "string", "description": "源路径" },
          "to": { "type": "string", "description": "目标路径" },
          "overwrite": {
            "type": "boolean",
            "description": "目标已存在时是否覆盖；缺省 false"
          },
          "recursive": {
            "type": "boolean",
            "description": "源为目录时是否递归复制；缺省 true"
          },
          "createDirs": {
            "type": "boolean",
            "description": "自动创建目标父目录；缺省 true"
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
    { "from": "C:\\proj\\a.json", "to": "C:\\proj\\backup\\a.json", "status": "ok", "type": "file", "bytes": 234 },
    { "from": "C:\\proj\\src", "to": "C:\\proj\\backup\\src", "status": "ok", "type": "dir", "files": 12, "bytes": 34567 },
    { "from": "C:\\proj\\big", "to": "C:\\proj\\backup\\big", "status": "failed", "type": "dir", "error": "...", "partial": true, "copiedFiles": 3 },
    { "from": "C:\\proj\\a.json", "to": "C:\\proj\\locked.json", "status": "failed", "type": "file", "error": "file is in use by another process: ...", "code": "FILE_IN_USE" }
  ],
  "summary": { "total": 4, "ok": 2, "failed": 2, "skipped": 0 }
}
```

- 成功项恒有 `from/to/status/type`。
- 文件成功项：`bytes` = 实际写入字节数。
- 目录成功项：`files` = 复制的文件数，`bytes` = 复制文件的总字节数。
- 失败项：`status:"failed"` + `error`。
- 目标被其他应用占用（Windows 独占打开冲突）→ 失败项额外返回 `code:"FILE_IN_USE"`；此时目标未被写入，**不返回** `partial`。
- 目标原本存在且复制中途失败 → `partial:true`（目标可能部分更新）；目录合并另返回 `copiedFiles`（已复制文件数）。目标原本不存在时已回滚，不返回 `partial`。
