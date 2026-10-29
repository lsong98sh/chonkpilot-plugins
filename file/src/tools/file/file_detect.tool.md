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
```

# 摘要
按批检测文件的 **编码 / BOM / 是否二进制**，返回候选编码与打分，**由调用方（LLM）看样本做最终仲裁**（工具不擅自决定）。

## 四层判定
1. **硬门槛**：BOM 存在 → 直接定编码；NUL 字节比例超阈 → `binary=true`。
2. **字符集分布统计**：字节区间分布打分（UTF-8 合法性 / 双字节区间密度）。
3. **常用字覆盖**：按候选编码解码后统计常用字（汉字/假名/谚文）占比。
4. **LLM 仲裁**（**不在工具内**）：返回 `candidates[]` + `scores[]` + `sample`，由调用方判断「乱码 vs 自然语言」。

## 关键规则
Shift-JIS 与 GBK 都能解出汉字，**必须靠「解出来是不是自然语言」区分**：
- 半角片假名（0xA1–0xDF）在 Shift-JIS 中是合法单字节；
- 在 GBK 中会与后继字节拼成生僻字；
- 这是最强判据之一，`sample` 会包含能体现该差异的片段。

## 置信度
- `confidence` ∈ `high` | `medium` | `low`。
- `high` = BOM 命中或 UTF-8 严格合法且常用字覆盖高。
- `low` = 多候选分数接近（典型：GBK / Big5 / Shift-JIS 撞车）→ **不要直接 `file_iconv`，先看 sample**。

**路径要求**：`path` 必须是绝对路径、`~/` 或 `!/` 开头；相对路径 → 整体失败。

# 参数
```json
{
  "type": "object",
  "required": ["purpose", "targets"],
  "additionalProperties": false,
  "properties": {
    "purpose": {
      "type": "string",
      "description": "本次检测的意图（用于审计与展示）"
    },
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
            "description": "取样字节数（决定 sample 长度与打分稳定性）；缺省 65536"
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
  "purpose": "判断老文档编码",
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
    }
  ],
  "summary": { "total": 1, "ok": 1, "failed": 0, "skipped": 0 }
}
```
