# 基本信息
```yaml
name: web_fetch
title: 网页抓取
async: auto
timeout: 180
on_timeout: background
category: web
hot: true
```

# 摘要
用 DeepSeek 网页抓取服务抓取单个 URL，返回页面标题与 Markdown 正文（需配置 DeepSeek API Key）。
真正抓取页面的是上游服务，本机只校验 URL 并转发；不渲染 JS、不过登录墙、不抓二进制/PDF。

需要原始内容（HTML/二进制）或下载文件请用 web_download。正文按 `maxBytes` 截断，
返回 `truncated` 标记；上游响应超过 8 MiB 时明确报错，请改用 web_download 落盘后本地读取。
`url` 必须是 http/https 绝对地址，不得携带账号密码。
单次上游尝试超时上限 55s（3 次尝试 + 退避 ≤168s，避免超过宿主 180s 后转后台）。
正文为空时返回 `note:"empty_content"` + hint（登录墙/反爬/纯 JS 渲染，可改用 web_download）。

# 参数
```json
{
  "type": "object",
  "required": ["url"],
  "additionalProperties": false,
  "properties": {
    "url": {
      "type": "string",
      "minLength": 1,
      "maxLength": 8192,
      "description": "http/https 绝对地址（<=8192 字节，不得携带账号密码）"
    },
    "maxBytes": {
      "type": "integer",
      "minimum": 1,
      "maximum": 8388608,
      "default": 4194304,
      "description": "返回正文 content 的字节上限（默认 4MiB，硬上限 8MiB）；超出时截断并置 truncated=true"
    }
  }
}
```

# 返回值
```json
{
  "provider": "deepseek",
  "url": "https://example.com/a",
  "title": "页面标题",
  "content": "# Markdown 正文",
  "bytes": 12345,
  "truncated": false
}
```

截断时 `truncated=true`，`bytes` 为实际返回的字节数；可提高 `maxBytes` 重抓，或改用 web_download。
正文为空时：`{…,"content":"","bytes":0,"note":"empty_content","hint":"…"}`。
正文为空常见于登录墙/纯 JS 页面，可改用 web_download 获取原始 HTML。