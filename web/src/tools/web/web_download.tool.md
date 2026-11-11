# 基本信息
```yaml
name: web_download
title: 下载/抓取原始内容
async: auto
timeout: 180
on_timeout: background
category: web
hot: true
```

# 摘要
本地直连抓取 URL（不经过 DeepSeek，不需要 API Key）：返回**原始内容**（文本按 Content-Type charset
解码为 UTF-8；二进制返回 base64），或按 `saveAs`/`dir`+`filename` 落盘下载。

**安全**：默认拒绝 loopback/私网/链路本地/`localhost`/`*.local` 等目标，重定向每一跳重新校验；
`headers` 不允许覆盖 `Host`。确需访问内网时，在 插件 → 联网能力 开启「允许访问内网」。
**状态**：4xx/5xx 一律返回错误（`isError=true`），错误页既不落盘也不当成功返回。
**体积**：**落盘模式不受 8MiB 内容上限限制**（流式写盘），默认单文件上限 2048MB（`maxMB` 可调，涵盖数百 MB 文件）；内容模式（直接返回正文/base64）才有 `maxBytes` 8MiB 硬上限，超过直接报错。
内容模式超出上限时截断并置 `truncated=true`，并回显本次 `limit_bytes`；若响应头带 Content-Length 则回显 `content_length`；chunked 响应没有总长度，`truncated=true` 只表示“还有更多”，无法给出丢失字节数（要读完才知道，这与截断目的冲突）。
**编码**：按响应头 charset 解码 GBK/GB18030/UTF-16 等；解码失败才退回 base64，并在 `encoding` 标明。
重定向默认跟随插件配置 `max-redirects` 次（0 = 不跟随），参数 `maxRedirects` 可覆盖；非整数会报错（不再静默当 0）。
落盘路径须在项目「可写目录」白名单内（沙箱开启时；递归，越界明确失败）。
落盘使用唯一临时文件（`<name>.part.*`），并发同名下载不会再互相截断。
`timeoutSec` 可设到 3600s，但宿主在 180s 会把任务转后台；大文件建议落盘并等待后台回执。

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
      "description": "http/https 绝对地址（<=8192 字节；默认拒绝本机/内网/链路本地目标）"
    },
    "method": {
      "type": "string",
      "default": "GET",
      "description": "HTTP 方法：GET（默认）/POST/PUT/DELETE/HEAD（大小写不敏感）"
    },
    "headers": {
      "type": "object",
      "additionalProperties": {"type": "string"},
      "description": "附加请求头（值必须为字符串；不允许覆盖 Host；非法值会报错）"
    },
    "body": {
      "type": "string",
      "description": "请求体（POST/PUT；GET/HEAD 也会原样发送）"
    },
    "saveAs": {
      "type": "string",
      "description": "落盘路径（绝对或 ~/ 开头；指向已存在目录则按 URL 文件名；不存在路径按文件名处理，父目录自动创建）。与 dir/filename 并存时 saveAs 优先"
    },
    "dir": {
      "type": "string",
      "description": "下载目录（绝对或 ~/ 开头；缺省用配置）"
    },
    "filename": {
      "type": "string",
      "description": "下载文件名（缺省取 Content-Disposition / URL 文件名）"
    },
    "maxBytes": {
      "type": "integer",
      "minimum": 1,
      "maximum": 8388608,
      "default": 4194304,
      "description": "内容模式本次读取上限（字节，硬上限 8MiB，超过直接报错）；缺省用配置 fetch-max-mb"
    },
    "maxMB": {
      "type": "integer",
      "minimum": 1,
      "maximum": 2048,
      "default": 2048,
      "description": "落盘模式单文件上限（MB，默认/硬上限 2048）；数百 MB 文件无需额外配置"
    },
    "overwrite": {
      "type": "boolean",
      "description": "同名是否覆盖（缺省用配置；否则自动加 -1）"
    },
    "timeoutSec": {
      "type": "integer",
      "minimum": 5,
      "maximum": 3600,
      "default": 60,
      "description": "本次超时（5-3600 秒；超过 md 的 180s 策略时会被转后台）"
    },
    "maxRedirects": {
      "type": "integer",
      "minimum": 0,
      "maximum": 20,
      "default": 5,
      "description": "最大重定向次数（0-20；0 = 不跟随；缺省用配置）"
    }
  }
}
```

# 返回值
内容模式（文本）：
```json
{
  "url": "https://example.com/a",
  "final_url": "https://example.com/a",
  "status": 200,
  "content_type": "text/html; charset=gbk",
  "redirects": 0,
  "bytes": 1234,
  "limit_bytes": 4194304,
  "truncated": false,
  "encoding": "gbk",
  "content": "解码后的 UTF-8 文本"
}
```

内容模式（二进制）：
```json
{
  "url": "https://example.com/a.bin",
  "final_url": "https://example.com/a.bin",
  "status": 200,
  "content_type": "application/octet-stream",
  "redirects": 0,
  "bytes": 1234,
  "limit_bytes": 4194304,
  "truncated": false,
  "encoding": "base64",
  "content_base64": "…"
}
```

落盘模式：
```json
{
  "url": "https://example.com/a.zip",
  "final_url": "https://example.com/a.zip",
  "status": 200,
  "content_type": "application/zip",
  "redirects": 0,
  "path": "E:\\dl\\a.zip",
  "saved_to": "E:\\dl\\a.zip",
  "bytes": 102400
}
```

错误：`HTTP 404` 等 4xx/5xx 返回 `isError=true`（不落盘）；SSRF 拦截、超过 8MiB 硬上限、
超时、沙箱越界同样为错误。