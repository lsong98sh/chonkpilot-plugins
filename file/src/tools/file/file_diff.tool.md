# 基本信息
```yaml
name: file_diff
title: 比较文件
args: file_diff --input={RAW-INPUT-FILE}
async: never
category: file
hot: true
runtime: builtin
idempotent: true
output: stdout
timeout: 120
on_timeout: background
```

# 摘要
按批比较两个文件，输出**标准 unified diff**（只读）。统一参数 `{targets[]}`，每个目标是一对 `left` / `right`。

- 输出头为实际路径（`--- <left>` / `+++ <right>`），正文带 `@@ -a,b +c,d @@` hunk 头与行号，可直接交给 `file_patch` 消费（往返一致）。
- **文本级比较**：两侧各自按源编码解码后比较，不按文件大小拒绝。`identical=true` 表示**解码后的文本内容一致**；此时仍返回两侧 `left_encoding/right_encoding/left_bom/right_bom`，内容一致但编码/BOM 不同不会被吞掉。
- **二进制文件（含 NUL）不做文本比较**：任一侧头部判定为二进制 → 该项 `status=failed`、`code=BINARY_UNSUPPORTED`、`binary=true`，不生成 diff。
- 两侧内容完全一致 → 该项 `status=ok` + `identical=true` + 空 `diff`。
- `context` 控制每个 hunk 前后的上下文行数，缺省 `3`；可调到 `0`（只输出变更行）。相邻修改间隔不超过 `2*context` 时合并到同一 hunk。
- **输出上限 128K**：`diff` 最多返回 128K（按行边界截断）；超出时 `diff_truncated=true`、`diff_omitted=true`、`diff_bytes`=完整 diff 字节数、`diff_returned_bytes`=实际返回字节数。截断后的 diff **不可直接用于 file_patch**。

**路径要求**：`left` / `right` 必须是绝对路径、`~/` 或 `!/` 开头；相对路径 → 该项 `failed`。

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
        "required": ["left", "right"],
        "additionalProperties": false,
        "properties": {
          "left": {
            "type": "string",
            "description": "左侧文件（旧版本）路径"
          },
          "right": {
            "type": "string",
            "description": "右侧文件（新版本）路径"
          },
          "context": {
            "type": "integer",
            "minimum": 0,
            "description": "diff 上下文行数；缺省 3"
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
      "left": "C:\\proj\\a.old",
      "right": "C:\\proj\\a.new",
      "status": "ok",
      "identical": false,
      "left_encoding": "utf-8",
      "right_encoding": "utf-8",
      "left_bom": false,
      "right_bom": false,
      "added": 3,
      "removed": 1,
      "diff": "--- C:/proj/a.old\n+++ C:/proj/a.new\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c"
    }
  ],
  "summary": { "total": 1, "ok": 1, "failed": 0, "skipped": 0 }
}
```

内容一致但编码不同（例如 GBK vs UTF-8）：

```json
{ "left_encoding": "gbk", "right_encoding": "utf-8", "identical": true, "added": 0, "removed": 0, "diff": "" }
```

diff 超出 128K：

```json
{ "diff_truncated": true, "diff_omitted": true, "diff_bytes": 1834200, "diff_returned_bytes": 131072 }
```

任一侧为二进制（含 NUL）→ 该项 `failed`，不做文本比较：

```json
{ "status": "failed", "code": "BINARY_UNSUPPORTED", "binary": true, "error": "right: binary file (not supported)" }
```
