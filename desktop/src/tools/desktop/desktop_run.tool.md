# 基本信息
```yaml
name: desktop_run
title: 桌面操作
async: auto
timeout: 60
on_timeout: background
category: desktop
hot: true
runtime: plugin
idempotent: false
```

# 摘要
操作本机 Windows 桌面窗口（Win32）。`action=start/stop` 管理会话，`ping/status` 探活；其余 action 在当前会话上执行原子操作。

**目标窗口**：`window_info` / `focus` / `click` / `input` / `key` / `move` / `size` / `minimize` / `maximize` / `restore` **必须**指定目标窗口，方式二选一：
1. 每次调用在 `args.hwnd` / `args.title` / `args.class` 中给出（`hwnd` 优先，其次 `class`，再 `title`；`title` 先精确匹配、未命中再按子串匹配）；
2. 先 `action=start`，在 `args` 里给 `title` / `class` / `hwnd` 预绑定（meta 绑定），之后上述动作可省略目标。
首次调用自动开会话时**不会**预绑定目标。推荐流程：`list_windows` 查看 `hwnd/class/visible` → 选定目标 → `start` 绑定或每次带目标 → 操作。

- `list_windows`：枚举全部顶层窗口，每项返回 `hwnd/title/class/visible/client_screen_rect`；同名标题标 `duplicate_title=true`，隐藏窗口 `visible=false`。可选 `visible_only`（默认 false）与 `limit`（默认 0 = 全部，枚举顺序）。
- `find_window`：按 `hwnd` / `title` / `class` 定位并返回窗口信息；不要求先有会话。
- `window_info`：返回目标窗口的标题、类名、可见性与客户区屏幕矩形。
- `focus`：把目标窗口置前（非交互会话可能失败）。
- `click`：`x`/`y` 是**以目标窗口客户区左上角为原点的相对坐标**（不是屏幕坐标），缺省 0,0 即点击客户区左上角；`button` 取 `left`（缺省）/`right`/`middle`。
- `input`：向目标窗口逐字符发送 Unicode 文本（会先聚焦窗口）；`text` 必填。
- `key`：发送组合键/单键，格式如 `ctrl+s`、`alt+f4`、`enter`（会先聚焦窗口）；`keys` 必填。
- `move`：只把窗口移动到屏幕坐标 `x,y`，**不改变尺寸**。
- `size`：只把窗口尺寸改为 `w,h`，**不移动位置**；别名 `resize`。
- `minimize` / `maximize` / `restore`：改变窗口显示状态。
- `screenshot`：截取**目标窗口客户区**；未显式给目标时，若 `start` 绑定过窗口则截绑定窗口，否则（或 `full_screen=true`）截**主屏整屏**。`path` 必填，或由插件配置的「截图默认目录」自动补全文件名；落盘路径受沙箱 `writable_dirs` 限制，越界直接报错且不落盘。
- `start`：可带 `purpose` 与 `title/class/hwnd`（预绑定目标窗口）；`stop`：结束当前实例的会话，**不关闭目标窗口**。
- 别名：`type` = `input`，`shot` = `screenshot`，`resize` = `size`。
- 同一实例复用同一会话；`context_id` 可显式指定会话（缺省按当前实例复用）。

# 参数
```json
{
  "type": "object",
  "required": ["action"],
  "additionalProperties": false,
  "properties": {
    "action": {
      "type": "string",
      "enum": [
        "start",
        "stop",
        "list_windows",
        "find_window",
        "window_info",
        "focus",
        "click",
        "input",
        "type",
        "key",
        "screenshot",
        "shot",
        "move",
        "size",
        "resize",
        "minimize",
        "maximize",
        "restore",
        "ping",
        "status"
      ],
      "description": "动作名。start/stop 管理会话；list_windows/find_window 用于定位窗口；其余为原子窗口操作"
    },
    "context_id": {
      "type": "string",
      "description": "可选：显式指定会话 context（缺省按当前实例复用已开会话）"
    },
    "purpose": {
      "type": "string",
      "description": "可选：start / 自动开会话时的用途说明（缺省 llm）"
    },
    "args": {
      "type": "object",
      "additionalProperties": false,
      "description": "动作参数。目标窗口 hwnd/title/class 与动作参数都放这里；不同 action 只使用各自相关字段。需要目标窗口的动作（window_info/focus/click/input/key/move/size/minimize/maximize/restore）必须提供 hwnd/title/class 之一，或在 start 时预绑定。",
      "properties": {
        "hwnd": {
          "type": ["integer", "string"],
          "description": "目标窗口句柄（十进制数字或数字字符串）；与 title/class 同时给出时优先"
        },
        "title": {
          "type": "string",
          "description": "目标窗口标题；先精确匹配，未命中再按子串匹配（同名窗口请改用 hwnd）"
        },
        "class": {
          "type": "string",
          "description": "目标窗口类名（如 Notepad、Chrome_WidgetWin_1）；优先于 title"
        },
        "x": {
          "type": "integer",
          "description": "click：客户区左上角为原点的横坐标（缺省 0）；move：窗口左上角屏幕 x 坐标（缺省 0）"
        },
        "y": {
          "type": "integer",
          "description": "click：客户区左上角为原点的纵坐标（缺省 0）；move：窗口左上角屏幕 y 坐标（缺省 0）"
        },
        "w": {
          "type": "integer",
          "minimum": 1,
          "description": "size 的目标宽度（像素，必填且 > 0）"
        },
        "h": {
          "type": "integer",
          "minimum": 1,
          "description": "size 的目标高度（像素，必填且 > 0）"
        },
        "button": {
          "type": "string",
          "enum": ["left", "right", "middle"],
          "default": "left",
          "description": "click 的鼠标键（缺省 left）"
        },
        "text": {
          "type": "string",
          "description": "input 要输入的文本（必填；Unicode 逐字符发送）"
        },
        "keys": {
          "type": "string",
          "description": "key 的按键规格，如 ctrl+s / alt+f4 / enter（必填）"
        },
        "path": {
          "type": "string",
          "description": "screenshot 的落盘路径（PNG）；缺省用插件配置的截图默认目录。沙箱开启时必须在 writable_dirs 内"
        },
        "full_screen": {
          "type": "boolean",
          "default": false,
          "description": "screenshot：true = 忽略 start 绑定的目标窗口，强制截主屏整屏"
        },
        "visible_only": {
          "type": "boolean",
          "default": false,
          "description": "list_windows：仅返回可见窗口（visible=true）"
        },
        "limit": {
          "type": "integer",
          "minimum": 0,
          "default": 0,
          "description": "list_windows：最多返回条数（0 = 全部）"
        }
      }
    }
  }
}
```

# 返回值
成功返回各 action 的结构化结果，均带 `context_id`：

- `start`：`{context_id, purpose, engine:"win32"}`；带 meta 时目标窗口绑定失败会直接报错（不回退为无绑定）。
- `stop`：`{closed:true, context_id}`；无活动会话时 `{ok:true, stopped:false, reason:"无活动 context"}`。
- `list_windows`：`{windows:[{hwnd,title,class,visible,client_screen_rect:{left,top,right,bottom,width,height},duplicate_title?}],count,context_id}`。
- `find_window` / `window_info`：`{hwnd,title,class,visible,client_screen_rect}`。
- `focus`：`{hwnd,focused:true}`。
- `click`：`{hwnd,screen_x,screen_y,button}`。
- `input`：`{hwnd,chars}`；`key`：`{hwnd,keys}`。
- `screenshot`：`{path,bytes,format:"png"}`（PNG 二进制不返回，仅落盘）。
- `move`：`{hwnd,moved:true}`；`size`：`{hwnd,resized:true}`。
- `minimize` / `maximize` / `restore`：`{hwnd,state:"minimized|maximized|restored"}`。

失败时 `isError=true`，常见文案：`no target window: pass hwnd/title/class, or bind one at start (meta)`、`window not found by title substring: X`、`path is required`、`sandbox_denied: ...`、`operation panic: ...`。