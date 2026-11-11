# 基本信息
```yaml
name: file_move
title: 移动或重命名
args: file_move --input={RAW-INPUT-FILE}
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
按批移动 / 重命名文件或目录。统一参数 `{targets[]}`。

**沙箱**：开启沙箱时，`targets[].to`（目标）必须在项目「可写目录」白名单内（递归），`from`（源，删除语义）也一并校验，否则**整次调用被拒绝**；未开启沙箱不受此限制。

- **跨卷回退仅限 `EXDEV`**：`rename` 返回 `EXDEV`（跨卷）才回退为「复制成功后删源」；其他 `rename` 错误（权限/占用等）返回 `failed`，`error` 为 `rename: <原始错误>`，**不复制、不删源**。
- **同卷 rename 无锁**：同卷 `rename` 由操作系统原子完成文件/目录移动，**不使用锁文件，也不持有目标句柄**。跨卷文件复制在 Windows 上以目标文件独占打开后就地写（其他应用无法同时打开目标），非 Windows 保留临时文件 + `rename` 原子写；目录递归复制。复制失败不删源（宁可留下副本，也不丢数据）。
- **目录移动**：目标不存在 → 先 `rename`（跨卷则复制）成功后删源；目标已存在且 `overwrite=true` → `rename` 无法合并，直接复制合并后删源。**合并不是镜像**：目标中源没有的文件会保留。
- `overwrite` 缺省 false：目标已存在 → 该项 `failed`；源/目标类型不一致（文件 vs 目录）→ 该项 `failed`（`target type mismatch (source and target must both be file or both be dir)`）。**`overwrite=false` 的存在性检查是 best-effort**：检查与 `rename` 之间没有锁文件，外部进程恰好在此窗口创建目标时可能被覆盖。
- **中间态显式化**：复制失败且目标原本不存在 → 回滚本次新建的目标树（Windows 新建文件也会被删除），源保持完整；复制失败且目标原本存在（目录合并，或 Windows 就地覆盖文件）→ `failed + partial:true`（目录另给 `copiedFiles`）；复制成功但删源失败 → `failed + partial:true + source_removed:false`，并给出 `copiedFiles`/`bytes`。
- `to == from`（Windows 大小写不敏感、分隔符归一；其他平台精确比较）→ 该项 `skipped`，`reason:"same_path"`；仅 `to` 严格位于 `from` 内部才报自我嵌套 `failed`。
- **重复项**：同一批内重复 `to` → 后项 `failed`（`duplicate target`）；重复 `from` → 后项 `failed`（`duplicate source`）。
- **并发/独占**：同卷 `rename` 由 OS 原子完成；跨卷/目录合并复制使用目标文件独占（Windows `dwShareMode=0`；非 Windows 临时文件 + `rename`），目标被其他应用占用 → `code:"FILE_IN_USE"`。**不使用锁文件**。
- **计数**：文件成功返回 `bytes`（同卷 rename = 源 size；跨卷 = 实际复制字节）；目录成功返回 `files`，跨卷/目录合并另返回 `bytes`。
- **元数据**：同卷 `rename` 由文件系统保留 mode/mtime；跨卷复制保留源文件 mode/mtime（Windows 用目标句柄 `SetFileTime` 设置 mtime，仅只读位/时间尽力保留，ACL/额外属性不保留）。
- **符号链接**：源目录 symlink 不被递归跟随（`WalkDir` 语义）；跨卷复制/目录合并时源文件 symlink 按目标内容复制（同卷 `rename` 移动链接本身）。目标树中出现目录 symlink → 该项 `failed`（`target path traverses symlink (refused)`）；文件 symlink 目标在 `overwrite=false` 时视为已存在。

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
    { "from": "C:\\proj\\a.txt", "to": "C:\\proj\\docs\\a.txt", "status": "ok", "type": "file", "bytes": 123 },
    { "from": "C:\\proj\\dir", "to": "C:\\proj\\docs\\dir", "status": "ok", "type": "dir", "files": 12 },
    { "from": "C:\\proj\\b.txt", "to": "C:\\proj\\docs\\b.txt", "status": "failed", "type": "file", "error": "target exists (overwrite=false)" },
    { "from": "C:\\proj\\c.txt", "to": "C:\\proj\\docs\\c.txt", "status": "failed", "type": "file", "error": "copied but failed to remove source: ...", "partial": true, "source_removed": false, "copiedFiles": 1, "bytes": 234 },
    { "from": "C:\\proj\\same.txt", "to": "C:\\proj\\same.txt", "status": "skipped", "reason": "same_path", "error": "same_path" }
  ],
  "summary": { "total": 5, "ok": 2, "failed": 2, "skipped": 1 }
}
```

- 成功项恒有 `from/to/status/type`；文件成功另有 `bytes`，目录成功另有 `files`（跨卷/目录合并还有 `bytes`）。
- 失败项：`status:"failed"` + `error`。仅跨卷/目录合并的中间态额外返回 `partial:true`（目标原本存在且复制可能部分写入；目标原本不存在时已回滚）；删源失败时另有 `source_removed:false` 与 `copiedFiles`/`bytes`。
- 跨卷/目录合并复制时目标被其他应用占用 → 额外返回 `code:"FILE_IN_USE"`。
- `to == from` → `status:"skipped"` + `reason:"same_path"`（`error` 同值，兼容旧读取方式）。
- `bytes` 语义：同卷文件 `rename` 为源文件大小；跨卷复制为实际写入字节；`files` 为移动的文件数。
