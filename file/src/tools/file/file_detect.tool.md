# 基本信息
```yaml
name: file_detect
title: 检测文件编码
args: file_detect --input={RAW-INPUT-FILE}
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
按批检测文件的 **编码 / BOM / 是否二进制**，返回候选编码与打分，**由调用方（LLM）看样本做最终仲裁**（工具不擅自决定）。

## 判定顺序
1. **硬门槛**：BOM 存在 → 直接定编码；NUL 字节比例超阈 → `binary=true`。
2. **严格 UTF-8 快判**：全部字节合法即 `utf-8` / `high`。
3. **chardet 统计检测**（`joshtechnologygroup/chardet`）：主结果 + 候选列表；极短样本（<8 字节）与不支持编码回落内置候选打分。
4. **常用字覆盖**：按候选编码解码后统计常用字（汉字/假名/谚文）占比，作为候选排序/置信度依据。
5. **LLM 仲裁**（**不在工具内**）：返回 `candidates[]`（`[{encoding,score}]`）+ `sample`，由调用方判断「乱码 vs 自然语言」。

## 关键规则
Shift-JIS 与 GBK 都能解出汉字，**必须靠「解出来是不是自然语言」区分**：
- 半角片假名（0xA1–0xDF）在 Shift-JIS 中是合法单字节；
- 在 GBK 中会与后继字节拼成生僻字；
- 这是最强判据之一，`sample` 会包含能体现该差异的片段。

## 置信度
- `confidence` ∈ `high` | `medium` | `low`（恒为非空；无候选/样本不足时给 `low`）。
- `high` = BOM 命中、二进制命中，或 UTF-8 严格合法且常用字覆盖高。
- `low` = 多候选分数接近（典型：GBK / Big5 / Shift-JIS 撞车）→ **不要直接 `file_iconv`，先看 sample**。
- `encoding` 恒存在：空文件无法判定时为 `unknown`；二进制为 `binary`。

## 取样窗口与字段
- `sampleBytes` 只控制**检测窗口**（读取头部多少字节做判定），不线性决定 `sample` 长度；`sample` 恒最多 **512 rune**。
- `sampleBytes` 上限 **4194304（4MiB）**；超过 → 该项 `failed` + `code:"SAMPLE_TOO_LARGE"`，错误 `sampleBytes too large (max 4MiB)`，不读取、不分配。
- 二进制判定只看**头部**（BOM 之外的 NUL 探测取前 8KiB）：`encoding:"binary"`、`confidence:"high"`、`binary:true`，**不返回** `sample` / `candidates`。
- `candidates` 结构统一为 `[{encoding,score}]`（不返回 `scores[]`）。
- **已知边界**：只检测头部；若 NUL 出现在探测窗口之后（“头文本 + 后二进制”混合文件），`binary=false` 是预期结果。

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
        "required": ["path"],
        "additionalProperties": false,
        "properties": {
          "path": { "type": "string", "description": "待检测文件路径" },
          "sampleBytes": {
            "type": "integer",
            "minimum": 64,
            "maximum": 4194304,
            "description": "检测窗口字节数（只控制读取头部多少字节；sample 恒最多 512 rune）；缺省 65536，超过 4MiB → SAMPLE_TOO_LARGE"
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
      "path": "C:\\proj\\legacy.txt",
      "status": "ok",
      "binary": false,
      "bom": "none",
      "encoding": "gbk",
      "confidence": "low",
      "candidates": [
        { "encoding": "gbk", "score": 0.62 },
        { "encoding": "shift_jis", "score": 0.58 },
        { "encoding": "big5", "score": 0.31 }
      ],
      "sample": "第一行文字…（用于人工/LLM 判断是否乱码）"
    },
    {
      "path": "C:\\proj\\image.bin",
      "status": "ok",
      "binary": true,
      "bom": "none",
      "encoding": "binary",
      "confidence": "high"
    },
    {
      "path": "C:\\proj\\empty.txt",
      "status": "ok",
      "binary": false,
      "bom": "none",
      "encoding": "unknown",
      "confidence": "low"
    }
  ],
  "summary": { "total": 3, "ok": 3, "failed": 0, "skipped": 0 }
}
```
