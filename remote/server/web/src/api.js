// Remote Server REST 封装（严格对应 docs/spec/10-architecture/26-remote.md §4.3）
// 约定：所有请求带 credentials: 'include'；错误统一抛 ApiError。

const JSON_HEADERS = { 'Content-Type': 'application/json' }

const STATUS_TEXT = {
  400: '请求参数有误',
  401: '登录已过期，请重新登录',
  403: '没有权限访问该设备或工作区',
  404: '资源不存在',
  409: '会话正在运行',
  429: '操作过于频繁，请稍后再试',
  500: '服务器内部错误',
  502: '电脑返回异常',
  503: '电脑未连接',
  504: '电脑响应超时',
}

// 服务端错误码 → 中文提示（服务端消息为英文；含中文的消息优先原样展示，保留细节）。
const CODE_TEXT = {
  bad_request: '请求参数有误，请检查后重试',
  no_context: '请先选择电脑与工作区',
  unauthorized: '登录已过期，请重新登录',
  forbidden: '没有权限访问该设备或工作区',
  device_not_found: '设备不存在或已解绑',
  device_offline: '电脑未连接，请确认电脑上的 ChonkPilot 正在运行',
  device_timeout: '电脑响应超时，请稍后重试',
  device_error: '电脑返回异常，请稍后重试',
  workspace_not_found: '该工作区未在电脑上打开',
  method_not_allowed: '当前桌面端版本不支持该操作',
  not_found: '资源不存在',
  too_many_requests: '尝试次数过多，请稍后再试',
  user_exists: '用户名已存在',
  internal: '服务器内部错误，请稍后重试',
}

function hasCJK(s) {
  return /[\u4e00-\u9fa5]/.test(s || '')
}

export class ApiError extends Error {
  constructor(code, message, status) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.status = status
  }
}

export function errorMessage(err) {
  if (!err) return '操作失败'
  if (err instanceof ApiError) return err.message || '操作失败'
  if (typeof err === 'string') return err
  return err.message || '操作失败'
}

function buildUrl(path, params) {
  if (!params) return path
  const qs = new URLSearchParams()
  Object.keys(params).forEach((k) => {
    const v = params[k]
    if (v === undefined || v === null || v === '') return
    qs.set(k, String(v))
  })
  const s = qs.toString()
  return s ? `${path}?${s}` : path
}

async function request(path, options = {}) {
  const { method = 'GET', body, params, signal } = options
  const hasBody = body !== undefined
  let res
  try {
    res = await fetch(buildUrl(path, params), {
      method,
      credentials: 'include',
      headers: hasBody ? JSON_HEADERS : undefined,
      body: hasBody ? JSON.stringify(body) : undefined,
      signal,
    })
  } catch (err) {
    if (err && err.name === 'AbortError') throw err
    throw new ApiError('network_error', '网络连接失败，请检查网络后重试', 0)
  }

  const raw = await res.text().catch(() => '')
  let payload = null
  if (raw) {
    try {
      payload = JSON.parse(raw)
    } catch {
      payload = null
    }
  }

  if (!res.ok) {
    const e = payload && payload.error
    const code = (e && e.code) || `http_${res.status}`
    const rawMsg = (e && e.message) || ''
    const message =
      (hasCJK(rawMsg) && rawMsg) ||
      CODE_TEXT[code] ||
      STATUS_TEXT[res.status] ||
      rawMsg ||
      (typeof raw === 'string' && raw.length < 200 ? raw : '') ||
      `请求失败（${res.status}）`
    throw new ApiError(code, message, res.status)
  }

  return payload
}

function toMillis(v) {
  if (v === null || v === undefined || v === '') return 0
  if (typeof v === 'number') return v < 1e12 ? v * 1000 : v
  if (typeof v === 'string') {
    const n = Number(v)
    if (Number.isFinite(n)) return n < 1e12 ? n * 1000 : n
    const ms = Date.parse(v)
    return Number.isFinite(ms) ? ms : 0
  }
  return 0
}

// ---------- 归一化（对字段差异保持宽容，避免契约小差异导致白屏） ----------

function normalizeSession(raw, index) {
  const s = raw || {}
  const summaryRaw =
    s.summary ?? s.last_message ?? s.lastMessage ?? s.preview ?? s.last_text ?? s.last
  let summary = ''
  if (typeof summaryRaw === 'string') summary = summaryRaw
  else if (summaryRaw && typeof summaryRaw === 'object') {
    summary = summaryRaw.text || summaryRaw.content || summaryRaw.message || ''
  }
  return {
    key: s.session_id || s.sessionId || s.id || `s-${index}`,
    id: s.session_id || s.sessionId || s.id || '',
    dialogId: s.dialog_id || s.dialogId || '',
    title: s.title || s.dialog_title || s.dialogTitle || s.name || '未命名会话',
    summary,
    updatedAt: toMillis(
      s.updated_at ?? s.updatedAt ?? s.last_active_at ?? s.lastActiveAt ?? s.created_at
    ),
    running: Boolean(s.running || s.busy),
  }
}

function normalizeMessage(raw, index) {
  const m = raw || {}
  const role = m.role || (m.is_user || m.isUser ? 'user' : 'assistant')
  let text = ''
  if (typeof m.text === 'string') text = m.text
  else if (typeof m.content === 'string') text = m.content
  else if (Array.isArray(m.content)) {
    text = m.content
      .map((c) => (typeof c === 'string' ? c : c && c.text))
      .filter(Boolean)
      .join('')
  }
  let tools = Array.isArray(m.tools)
    ? m.tools.map((t) => ({
        name: (t && (t.name || t.tool)) || '',
        status: (t && (t.status || t.state)) || '',
      }))
    : []
  // 工具卡片行（type=tool_pair）：一个工具调用 = 一条历史行，展示为工具行（不带空正文气泡）。
  if (!tools.length && m.type === 'tool_pair' && m.tool_name) {
    tools = [{ name: m.tool_name, status: m.tool_status || 'done' }]
  }
  return {
    id: m.id || m.message_id || m.messageId || `m-${index}`,
    role: role === 'user' ? 'user' : 'assistant',
    text,
    reasoning: m.reasoning || m.reason || '',
    tools,
    turnId: m.turn_id || '',
    createdAt: toMillis(m.created_at ?? m.createdAt),
  }
}

function parseJSONMaybe(v) {
  if (!v) return null
  if (typeof v === 'object') return v
  if (typeof v !== 'string') return null
  try {
    return JSON.parse(v)
  } catch {
    return null
  }
}

const KIND_TEXT = { tool: '工具', llm: '会话', ask_user: '提问' }

function normalizeTaskNode(node) {
  const n = node || {}
  const childrenRaw = n.children || n.subtasks || n.tasks || n.nodes
  const exec = parseJSONMaybe(n.exec_json) || {}
  const kindRaw = n.kind || n.node_type || ''
  return {
    id: n.id || n.node_id || n.task_id || n.taskId || '',
    title: n.title || n.name || n.tool || n.description || n.subject || '任务',
    state: n.state || n.status || '',
    tool: n.tool || n.tool_name || exec.tool || '',
    kind: KIND_TEXT[kindRaw] || kindRaw,
    sessionId: n.session_id || '',
    turnId: n.turn_id || '',
    createdAt: toMillis(n.created_at ?? n.createdAt),
    updatedAt: toMillis(n.updated_at ?? n.updatedAt),
    detail: n.args ?? n.detail ?? n.payload ?? exec.detail ?? null,
    children: Array.isArray(childrenRaw) ? childrenRaw.map(normalizeTaskNode) : [],
  }
}

export function normalizeTaskTree(raw) {
  if (!raw) return []
  let list = raw
  if (!Array.isArray(list)) list = raw.tasks || raw.nodes || raw.items || raw.tree || raw.data
  if (!list) return []
  if (!Array.isArray(list)) list = [list]
  return list.map(normalizeTaskNode)
}

// ---------- 端点 ----------

async function login(username, password) {
  try {
    return await request('/api/login', { method: 'POST', body: { username, password } })
  } catch (err) {
    // 登录页的 401 = 凭据错误（不是「登录已过期」）。
    if (err instanceof ApiError && err.status === 401) {
      throw new ApiError('invalid_credentials', '用户名或密码错误', 401)
    }
    throw err
  }
}

async function logout() {
  return request('/api/logout', { method: 'POST' })
}

async function me() {
  const p = await request('/api/me')
  const u = (p && (p.user || p.account)) || p || {}
  const ctx = (p && p.context) || {}
  return {
    username: u.username || u.name || '',
    role: u.role || '',
    deviceId: (p && (p.device_id || p.deviceId)) || ctx.device_id || ctx.deviceId || '',
    workspaceId: (p && (p.workspace_id || p.workspaceId)) || ctx.workspace_id || ctx.workspaceId || '',
  }
}

async function listDevices() {
  const p = await request('/api/devices')
  const list = Array.isArray(p) ? p : Array.isArray(p && p.devices) ? p.devices : []
  return list.map((d) => {
    const dev = d || {}
    const workspacesRaw = Array.isArray(dev.workspaces) ? dev.workspaces : []
    return {
      id: dev.device_id || dev.deviceId || dev.id || '',
      name: dev.name || dev.device_name || '未命名设备',
      platform: dev.platform || '',
      version: dev.version || '',
      online: Boolean(dev.online),
      lastSeen: toMillis(dev.last_seen ?? dev.lastSeen),
      protocol: dev.protocol || '',
      workspaces: workspacesRaw.map((w) => ({
        id: (w && (w.id || w.workspace_id || w.path)) || '',
        name: (w && (w.name || w.title || w.path)) || '未命名工作区',
        path: (w && w.path) || '',
      })),
    }
  })
}

async function setContext(deviceId, workspaceId) {
  return request('/api/context', {
    method: 'POST',
    body: { device_id: deviceId, workspace_id: workspaceId },
  })
}

async function listSessions() {
  const p = await request('/api/sessions')
  const list = Array.isArray(p)
    ? p
    : Array.isArray(p && p.list)
      ? p.list
      : Array.isArray(p && p.sessions)
        ? p.sessions
        : []
  return list.map(normalizeSession)
}

async function createSession(text) {
  const body = text ? { text } : {}
  const p = await request('/api/sessions', { method: 'POST', body })
  const src = (p && (p.session || p.data)) || p || {}
  return {
    sessionId: src.session_id || src.sessionId || src.id || '',
    turnId: src.turn_id || src.turnId || '',
    dialogId: src.dialog_id || src.dialogId || '',
    title: src.title || '',
  }
}

async function listMessages(sessionId, { before, limit = 30, dialogId } = {}) {
  const p = await request(`/api/sessions/${encodeURIComponent(sessionId)}/messages`, {
    params: { before, limit, dialog_id: dialogId },
  })
  let list = []
  if (Array.isArray(p)) list = p
  else if (p && Array.isArray(p.messages)) list = p.messages
  else if (p && Array.isArray(p.items)) list = p.items
  // 空行（工具调用声明等无正文行）不展示；工具卡片行已由 normalizeMessage 转成 tools。
  const messages = list
    .map(normalizeMessage)
    .filter((m) => m.text || m.reasoning || (m.tools && m.tools.length))
  const hasMore = Boolean(p && !Array.isArray(p) && (p.has_more ?? p.hasMore))
  // 分页游标 = 本页最早一条的 turn_id（宿主 before_turn_id 口径），不是消息 id。
  const firstWithTurn = messages.find((m) => m.turnId) || {}
  const nextBefore =
    (p && !Array.isArray(p) && (p.next_before ?? p.nextBefore)) ||
    firstWithTurn.turnId ||
    ''
  return { messages, hasMore, nextBefore }
}

async function sendMessage(sessionId, text, dialogId, insert = false) {
  const p = await request(`/api/sessions/${encodeURIComponent(sessionId)}/messages`, {
    method: 'POST',
    body: { text, dialog_id: dialogId, insert: !!insert },
  })
  const accepted = !p || p.accepted !== false
  const err = p && (p.error || p.message)
  return {
    accepted,
    queued: Boolean(p && p.queued),
    type: (p && p.type) || '',
    inboxId: (p && p.inbox_id) || '',
    error: typeof err === 'string' ? err : '',
  }
}

// 读该会话 pending inbox（2026-11-01：两端共享同一 inbox，全量返回，含 mobile 自己与他端插话项）。
async function listInbox(sessionId) {
  const p = await request(`/api/sessions/${encodeURIComponent(sessionId)}/inbox`)
  const list = Array.isArray(p) ? p : p && Array.isArray(p.items) ? p.items : []
  return {
    items: list.map((x) => ({
      id: String((x && x.id) || ''),
      content: String((x && x.content) || ''),
      type: String((x && x.type) || ''),
      source: String((x && x.source) || ''),
      createdAt: (x && x.created_at) || '',
    })),
  }
}

// inbox 项操作：promote（插话）/ revert（取消插话）/ discard（删除·撤回）。
async function inboxAction(sessionId, id, action) {
  const p = await request(`/api/sessions/${encodeURIComponent(sessionId)}/inbox/${action}`, {
    method: 'POST',
    body: { id },
  })
  return { ok: !p || p.ok !== false, type: (p && p.type) || '', error: (p && p.error) || '' }
}

async function stopSession(sessionId) {
  return request(`/api/sessions/${encodeURIComponent(sessionId)}/stop`, { method: 'POST' })
}

async function answer(sessionId, payload) {
  return request(`/api/sessions/${encodeURIComponent(sessionId)}/answer`, {
    method: 'POST',
    body: payload,
  })
}

async function tasks(sessionId) {
  return request(`/api/sessions/${encodeURIComponent(sessionId)}/tasks`)
}

async function stopTask(taskId) {
  return request(`/api/tasks/${encodeURIComponent(taskId)}/stop`, { method: 'POST' })
}

export function normalizeFiles(raw) {
  const r = raw || {}
  const list = Array.isArray(r)
    ? r
    : r.children || r.entries || r.files || r.items || []
  return {
    path: r.path || '',
    entries: (Array.isArray(list) ? list : [])
      .map((e) => {
        const it = e || {}
        const isDir = it.is_dir === true || it.isDir === true || it.type === 'dir' || it.type === 'directory'
        return {
          name: it.name || (it.path || '').split(/[\\/]/).filter(Boolean).pop() || '',
          path: it.path || '',
          isDir,
          size: it.size || 0,
          mtime: toMillis(it.mtime ?? it.modified ?? it.updated_at),
        }
      })
      .filter((e) => e.name),
  }
}

export function normalizeFileContent(raw) {
  const r = raw || {}
  const content = typeof r.content === 'string' ? r.content : typeof r.text === 'string' ? r.text : ''
  return {
    path: r.path || '',
    content,
    truncated: Boolean(r.truncated),
  }
}

async function files(path) {
  const q = path ? `?path=${encodeURIComponent(path)}` : ''
  return request(`/api/files${q}`)
}

async function fileContent(path) {
  return request(`/api/files/content?path=${encodeURIComponent(path)}`)
}

export const api = {
  login,
  logout,
  me,
  listDevices,
  setContext,
  listSessions,
  createSession,
  listMessages,
  sendMessage,
  listInbox,
  inboxAction,
  stopSession,
  answer,
  tasks,
  stopTask,
  files,
  fileContent,
}
