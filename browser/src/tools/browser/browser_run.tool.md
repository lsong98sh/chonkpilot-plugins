# browser_run

```yaml
name: browser_run
title: 浏览器操作
async: auto
timeout: 60
on_timeout: background
category: browser
hot: true
idempotent: false
```

# 摘要

驱动本机 WebView2 浏览器。`action=start/stop` 管理会话；其余 action 在当前 context 上执行原子操作，首次调用会自动开会话。

动作与参数（都在 `args` 内）：

- `navigate(url)`：打开 URL（仅 http/https/file/about）。别名 `goto` / `open`。
- `click(selector)`：点击第一个匹配元素。
- `fill(selector,text)`：设置输入框的值并触发 input/change。别名 `type`。
- `wait(selector,timeoutMs?)`：轮询等待元素出现；`timeoutMs` 缺省 10000，取值 1..600000。
- `eval(script)`：执行 JS 并返回结果；也接受 `args.script`（首选）。参数键别名 `eval` / `expr` / `js` / `code`；action 别名 `script` / `js`。
- `text(selector)`：返回元素 innerText（selector 必填；空文本会报 element not found）。
- `html()`：返回整页 outerHTML（可能很大）。
- `title()` / `url()`：读取标题 / 当前地址。
- `screenshot(path?)`：截图 PNG 落盘；省略 path 时使用插件配置的「截图默认目录」，两者都没有则直接报错。沙箱开启时 path 必须在可写白名单内，否则整次调用拒绝且不落盘。别名 `shot`。
- `console_dump()`：返回并清空页面控制台日志（最多 2000 条，JSON 字符串）。
- `reload` / `back` / `forward` / `set_html(html)` / `debug_open`。
- `ping` / `status`：引擎健康检查（`status` 与 `ping` 等价）。

约定：

- `context_id` 可显式指定，但必须属于当前实例/会话；跨实例操作会被拒绝。省略时复用当前实例的 context，没有则自动 start。
- `ready:false` 表示页面在超时内未加载完成，不是失败；可稍后用 `wait`/`text` 检查。
- `html` / `text` / `console_dump` / `eval` 的返回没有分页；超大页面可能触发传输上限。
- `debug_open` 需要插件配置开启 debug（DevTools）。

# 参数

```json
{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "enum": [
        "start", "stop", "navigate", "click", "fill", "wait", "eval", "script",
        "text", "html", "title", "url", "screenshot", "console_dump",
        "reload", "back", "forward", "set_html", "debug_open", "ping", "status",
        "goto", "open", "type", "shot", "js"
      ],
      "description": "动作名。主名见摘要；别名：goto/open=navigate、type=fill、shot=screenshot、js/script=eval"
    },
    "context_id": {
      "type": "string",
      "description": "可选：显式指定会话 context；必须属于当前实例/会话"
    },
    "purpose": {
      "type": "string",
      "description": "start / 自动开会话时的用途说明"
    },
    "args": {
      "type": "object",
      "description": "动作参数；只取当前 action 需要的键，其余忽略",
      "properties": {
        "url": {
          "type": "string",
          "description": "navigate 的目标地址（仅 http/https/file/about）"
        },
        "selector": {
          "type": "string",
          "description": "click/fill/wait/text 的 CSS 选择器"
        },
        "text": {
          "type": "string",
          "description": "fill 写入的文本"
        },
        "script": {
          "type": "string",
          "description": "eval/script 执行的 JS 代码（别名 eval/expr/js/code）"
        },
        "eval": {
          "type": "string",
          "description": "script 的别名"
        },
        "expr": {
          "type": "string",
          "description": "script 的别名（兼容旧描述里的 expr）"
        },
        "timeoutMs": {
          "type": "integer",
          "minimum": 1,
          "maximum": 600000,
          "description": "wait 的轮询上限（毫秒；缺省 10000，最大 600000）"
        },
        "path": {
          "type": "string",
          "description": "screenshot 落盘路径（可省略，但需配置截图默认目录）；支持绝对路径 / ~/ 开头；沙箱开启时须在可写白名单内"
        },
        "html": {
          "type": "string",
          "description": "set_html 写入的 HTML"
        }
      },
      "additionalProperties": true
    }
  },
  "required": ["action"]
}
```