# 基本信息
```yaml
name: web_search
title: 联网搜索
async: auto
timeout: 180
on_timeout: background
category: web
hot: true
```

# 摘要
联网检索（DeepSeek Search；需在 插件 → 联网能力 配置 API Key）。按 `query` 返回标题、链接、摘要/正文片段，
可选 `need_content=true` 让上游返回正文片段。默认 10 条、最多 50 条。

返回 `{provider:"deepseek", results:[…], count, count_requested, content_bytes, content_truncated}`；每条含 `title/url` 及可选的
`snippet/summary/content/site_name/site_icon/date/author/authority/score/rerank_score`。
`need_content=true` 时正文按 `max_content_bytes` 总预算（默认 256KiB、上限 8MiB）截断，被截条目置
`content_truncated:true`，响应顶层 `content_truncated` 表示是否有条目被截。
上游响应超过 8 MiB 时**明确报错**（不再静默截断导致 JSON 解析失败）：
请降低 `count` 或关闭 `need_content`，大文件/整页请改用 web_download。
空结果返回空数组 + `note:"no_results"`/`hint`，可放宽 `query` 后重试。未配置 API Key 直接报错且不发起请求。

单次上游尝试超时上限 55s（3 次尝试 + 退避 ≤168s，避免超过宿主 180s 后转后台）。

# 参数
```json
{
  "type": "object",
  "required": ["query"],
  "additionalProperties": false,
  "properties": {
    "query": {
      "type": "string",
      "minLength": 1,
      "maxLength": 128,
      "description": "检索词（1-128 字符）"
    },
    "count": {
      "type": "integer",
      "minimum": 1,
      "maximum": 50,
      "default": 10,
      "description": "返回条数（1-50，缺省 10）"
    },
    "need_content": {
      "type": "boolean",
      "default": false,
      "description": "是否返回正文片段（缺省 false；开启会显著增大返回体积）"
    },
    "ori_question": {
      "type": "string",
      "description": "原始问题（供上游检索改写参考，可选）"
    },
    "localization": {
      "type": "string",
      "description": "本地化检索 IP 地址（可选；缺省空串，原样转发给上游）"
    },
    "max_content_bytes": {
      "type": "integer",
      "minimum": 1,
      "maximum": 8388608,
      "default": 262144,
      "description": "need_content=true 时正文总预算（字节，默认 256KiB，上限 8MiB）；超出按条截断并置 content_truncated"
    }
  }
}
```

# 返回值
```json
{
  "provider": "deepseek",
  "results": [
    {
      "title": "标题",
      "url": "https://example.com/a",
      "snippet": "摘要片段",
      "content": "正文片段（need_content=true 时）"
    }
  ],
  "count": 1,
  "count_requested": 10,
  "content_bytes": 1234,
  "content_truncated": false
}
```

空结果：`{ "provider": "deepseek", "results": [], "count": 0, "count_requested": 10, "note": "no_results", "hint": "…" }`。
`count` 为实际返回条数，`count_requested` 为请求值（可能因上限 50 被截）；`content_truncated:true` 的条目已按预算截断。
超过上游 8 MiB 读取上限时返回错误：`响应超过上限 8MiB：请降低 count 或关闭 need_content，大文件请改用 web_download`。