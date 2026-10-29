# 基本信息
```yaml
name: task_llm
title: 单次 LLM 调用
args: task_llm --input={RAW-INPUT-FILE}
async: never
category: task
hot: true
runtime: plugin
idempotent: false
output: stdout
timeout: 600
```

# 摘要
调用一次 LLM（无会话、无工具）并返回文本。适合摘要 / 改写 / 判定。API Key 由宿主持有，插件不可见。

# 参数
```json
{
  "type": "object",
  "required": ["prompt"],
  "properties": {
    "prompt": {
      "type": "string",
      "description": "用户提示词"
    },
    "system": {
      "type": "string",
      "description": "系统提示词（可选）"
    },
    "timeoutSec": {
      "type": "integer",
      "description": "超时秒数（缺省 120）"
    }
  }
}
```

# 返回值
```json
{ "text": "……" }
```
