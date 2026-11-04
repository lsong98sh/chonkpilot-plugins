<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { api, ApiError, normalizeFileContent, normalizeFiles, normalizeTaskTree } from '../api'
import { useEventStream } from '../composables/useEventStream'
import { toast, toastError } from '../composables/useToast'
import AskPanel from '../components/AskPanel.vue'
import BottomSheet from '../components/BottomSheet.vue'
import MessageItem from '../components/MessageItem.vue'
import TaskTree from '../components/TaskTree.vue'

const props = defineProps({
  session: { type: Object, required: true },
})

const emit = defineEmits(['back', 'created', 'expired'])

const sid = ref(props.session.id || '')
const dialogId = ref(props.session.dialogId || '')
const title = ref(props.session.title || '')
const messages = ref([])
// 以列表页带过来的运行态初始化（打开运行中的会话应立即只显示「停止」）。
const busy = ref(Boolean(props.session.running))
const inputText = ref('')
const sending = ref(false)
const creating = ref(false)
const stopping = ref(false)
const loadingHistory = ref(false)
const hasMore = ref(false)
const beforeCursor = ref('')
const streamId = ref('')
// 手机端发送一律进 inbox（source=mobile）；2026-11-01 起两端共享**全量** inbox：
// H5 可查看/插话/删除/撤回任意来源（桌面或手机）的 pending 项。
const inboxItems = ref([])
const queuedCount = computed(() => inboxItems.value.length)
// 悬浮 inbox 图标 → 全量列表页（底部弹层）
const inboxOpen = ref(false)
const inboxBusy = ref(false)
const atBottom = ref(true)

const scroller = ref(null)
const inputEl = ref(null)

const askState = ref(null)
const askOpen = ref(false)
const submittingAsk = ref(false)

const tasksOpen = ref(false)
const tasks = ref([])
const tasksLoading = ref(false)
const taskDetail = ref(null)
let taskTimer = null

const filesOpen = ref(false)
const filesLoading = ref(false)
const filePath = ref('')
const fileEntries = ref([])
const fileStack = ref([''])
const filePreview = ref(null)
const fileLoading = ref(false)

const { status, subscribe, start } = useEventStream()

const connLabel = computed(() => {
  if (status.value === 'online') return '已连接'
  if (status.value === 'connecting') return '连接中'
  if (status.value === 'reconnecting') return '重连中'
  return '未连接'
})

const displayTitle = computed(() => title.value || '新聊天')

function localId(prefix) {
  return `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`
}

// ---------- 滚动 ----------

async function scrollToBottom(force = false) {
  await nextTick()
  const el = scroller.value
  if (!el) return
  if (!force && !atBottom.value) return
  el.scrollTop = el.scrollHeight
  atBottom.value = true
}

function maybeScroll() {
  scrollToBottom(false)
}

function onScroll() {
  const el = scroller.value
  if (!el) return
  atBottom.value = el.scrollHeight - el.scrollTop - el.clientHeight < 80
  if (el.scrollTop < 60 && hasMore.value && !loadingHistory.value) loadEarlier()
}

// ---------- 历史分页 ----------

async function loadInitial() {
  if (!sid.value) return
  loadingHistory.value = true
  try {
    const r = await api.listMessages(sid.value, { limit: 30, dialogId: dialogId.value })
    messages.value = r.messages
    hasMore.value = r.hasMore
    beforeCursor.value = r.nextBefore
    if (!title.value) {
      const first = r.messages.find((m) => m.role === 'user' && m.text)
      if (first) title.value = first.text.slice(0, 24)
    }
    await scrollToBottom(true)
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) emit('expired')
    else toastError(err)
  } finally {
    loadingHistory.value = false
  }
  loadInbox()
}

async function loadEarlier() {
  if (!sid.value || !hasMore.value || loadingHistory.value) return
  const el = scroller.value
  const prevHeight = el ? el.scrollHeight : 0
  const prevTop = el ? el.scrollTop : 0
  loadingHistory.value = true
  try {
    const r = await api.listMessages(sid.value, { before: beforeCursor.value, limit: 30, dialogId: dialogId.value })
    const seen = new Set(messages.value.map((m) => m.id))
    const older = r.messages.filter((m) => !seen.has(m.id))
    messages.value = older.concat(messages.value)
    hasMore.value = r.hasMore
    if (r.nextBefore) beforeCursor.value = r.nextBefore
    await nextTick()
    if (el) el.scrollTop = el.scrollHeight - prevHeight + prevTop
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) emit('expired')
    else toastError(err)
  } finally {
    loadingHistory.value = false
  }
}

// ---------- 事件 ----------

function isCurrent(data) {
  if (!data) return false
  const eventSid = data.session_id || data.sessionId || ''
  if (!eventSid) return false
  if (sid.value) return eventSid === sid.value
  // 新会话首次发送：服务端可能先于 HTTP 响应推送事件，率先到达的 session_id 即当前会话
  if (creating.value) {
    sid.value = eventSid
    emit('created', { id: eventSid, title: title.value })
    return true
  }
  return false
}

function ensureStreamMessage() {
  if (streamId.value) {
    const found = messages.value.find((m) => m.id === streamId.value)
    if (found) return found
  }
  messages.value.push({
    id: localId('stream'),
    role: 'assistant',
    text: '',
    reasoning: '',
    tools: [],
    createdAt: Date.now(),
    streaming: true,
  })
  streamId.value = messages.value[messages.value.length - 1].id
  return messages.value[messages.value.length - 1]
}

function isRunningState(state) {
  const s = (state || '').toLowerCase()
  return ['running', 'started', 'pending', 'working', 'in_progress'].includes(s)
}

function applyTool(m, data) {
  const name = data.name || data.tool || '工具'
  const st = data.status || data.state || ''
  const last = m.tools[m.tools.length - 1]
  if (last && last.name === name && isRunningState(last.status)) last.status = st || last.status
  else m.tools.push({ name, status: st })
}

function finishStream(data) {
  const m = streamId.value ? messages.value.find((x) => x.id === streamId.value) : null
  if (m) {
    m.streaming = false
    const extra =
      data && typeof data.message === 'string'
        ? data.message
        : data && data.message && typeof data.message.text === 'string'
          ? data.message.text
          : ''
    if (!m.text && extra) m.text = extra
    if (!m.text && !m.reasoning && !m.tools.length) {
      const i = messages.value.findIndex((x) => x.id === m.id)
      if (i >= 0) messages.value.splice(i, 1)
    }
  }
  streamId.value = ''
  busy.value = false
  maybeScroll()
}

function normalizeOptions(raw) {
  if (!Array.isArray(raw)) return []
  return raw
    .map((o) => {
      if (o && typeof o === 'object') {
        const label = String(o.label ?? o.text ?? o.value ?? '')
        const value = o.value !== undefined && o.value !== null ? String(o.value) : label
        return { value, label }
      }
      return { value: String(o), label: String(o) }
    })
    .filter((o) => o.label !== '')
}

function handleEvent(type, data) {
  if (type === '__reconnected') {
    reloadAfterReconnect()
    return
  }
  if (type === '__unauthorized') {
    emit('expired')
    return
  }

  if (type === 'turn.start') {
    if (!isCurrent(data)) return
    busy.value = true
    ensureStreamMessage()
    maybeScroll()
    // 排队项在本轮开始前已被消费（pending → consumed）→ 刷新「排队中」条。
    loadInbox()
    return
  }

  if (type === 'message.delta') {
    if (!isCurrent(data)) return
    const m = ensureStreamMessage()
    m.text += data.text || data.delta || ''
    busy.value = true
    maybeScroll()
    return
  }

  if (type === 'message.reasoning') {
    if (!isCurrent(data)) return
    const m = ensureStreamMessage()
    m.reasoning += data.text || data.delta || ''
    busy.value = true
    maybeScroll()
    return
  }

  if (type === 'message.tool') {
    if (!isCurrent(data)) return
    const m = ensureStreamMessage()
    applyTool(m, data)
    maybeScroll()
    return
  }

  if (type === 'turn.complete') {
    if (!isCurrent(data)) return
    finishStream(data)
    mergeRemoteUserMessages(data.turn_id || '')
    loadInbox()
    return
  }

  if (type === 'ask') {
    if (!isCurrent(data)) return
    askState.value = {
      ask_id: data.ask_id || data.askId || '',
      question: data.question || data.text || '请作答',
      options: normalizeOptions(data.options),
      multi: Boolean(data.multi),
      custom: data.custom !== false,
    }
    askOpen.value = true
    return
  }

  if (type === 'inbox.changed') {
    if (!isCurrent(data)) return
    loadInbox()
    return
  }

  if (type === 'task.update') {
    if (!isCurrent(data)) return
    if (tasksOpen.value) scheduleTaskReload()
  }
}

// 本轮结束后补齐「其他端（桌面）发出的用户消息」：H5 只消费助手侧事件，桌面侧插话/发起
// 的用户文本不会实时到达；轮末按 turn_id 从历史补插（文本去重，避免与本地乐观消息重复）。
async function mergeRemoteUserMessages(turnId) {
  const cur = sid.value
  if (!cur) return
  try {
    const r = await api.listMessages(cur, { limit: 30, dialogId: dialogId.value })
    if (cur !== sid.value) return
    const knownIds = new Set(messages.value.map((m) => m.id))
    const knownTexts = new Set(messages.value.filter((m) => m.role === 'user').map((m) => m.text))
    const missing = r.messages.filter(
      (m) =>
        m.role === 'user' &&
        (!turnId || m.turnId === turnId) &&
        !knownIds.has(m.id) &&
        !knownTexts.has(m.text)
    )
    if (!missing.length) return
    // 插在最后一条助手消息之前，保持「提问 → 回复」顺序
    let idx = messages.value.length
    for (let i = messages.value.length - 1; i >= 0; i--) {
      if (messages.value[i].role === 'assistant') { idx = i; break }
    }
    messages.value.splice(idx, 0, ...missing)
    maybeScroll()
  } catch {
    // 补齐失败不影响已展示内容（重进会话会重新拉全量历史）
  }
}

async function reloadAfterReconnect() {
  if (!sid.value) return
  loadingHistory.value = true
  try {
    const r = await api.listMessages(sid.value, { limit: 30, dialogId: dialogId.value })
    messages.value = r.messages
    hasMore.value = r.hasMore
    beforeCursor.value = r.nextBefore
    streamId.value = ''
    await scrollToBottom(true)
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) emit('expired')
  } finally {
    loadingHistory.value = false
  }
  if (tasksOpen.value) loadTasks()
  loadInbox()
}

// 拉取 pending inbox（全量：桌面与手机两端共享，不按来源过滤）。
async function loadInbox() {
  const cur = sid.value
  if (!cur) {
    inboxItems.value = []
    return
  }
  try {
    const r = await api.listInbox(cur)
    if (cur !== sid.value) return
    inboxItems.value = r.items
  } catch {
    // 读取失败保持现状（下次轮末 / 重连再刷）
  }
}

// ---------- inbox 列表页操作（2026-11-01 两端共享） ----------

function openInbox() {
  inboxOpen.value = true
  loadInbox()
}

function sourceLabel(it) {
  if (it.source === 'mobile') return '手机'
  if (it.source === 'llm') return '自动'
  return '桌面'
}

async function inboxAct(it, action) {
  if (!sid.value || !it || inboxBusy.value) return
  inboxBusy.value = true
  try {
    const r = await api.inboxAction(sid.value, it.id, action)
    if (!r.ok) toast(r.error || '操作失败', 'error')
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) emit('expired')
    else toastError(err)
  } finally {
    inboxBusy.value = false
    loadInbox()
  }
}

// 插话 / 取消插话（toggle）
function toggleInterject(it) {
  inboxAct(it, it.type === 'insert' ? 'revert' : 'promote')
}

// 撤回编辑：删除 + 内容回填输入框 + 回到聊天窗口
async function recallInbox(it) {
  if (!it) return
  const text = it.content || ''
  await inboxAct(it, 'discard')
  inboxOpen.value = false
  inputText.value = text
  await nextTick()
  if (inputEl.value) {
    inputEl.value.focus()
    autoGrow()
  }
}

// ---------- 发送 / 停止 / 作答 ----------

function removeMessage(id) {
  const i = messages.value.findIndex((m) => m.id === id)
  if (i >= 0) messages.value.splice(i, 1)
}

// 手机端发送：运行中 → 进 inbox 排队（本轮结束后自动发送），不插话、不拒绝；
// 空闲 → 同样经 inbox（服务端立即消费起新一轮）。新建会话首条走 create（无运行轮）。
async function send() {
  if (sending.value) return
  const text = inputText.value.trim()
  if (!text) return

  const willQueue = Boolean(sid.value && busy.value)
  let userMsgId = ''
  let queueId = ''
  if (willQueue) {
    queueId = localId('q')
    inboxItems.value.push({ id: queueId, content: text, type: 'next_turn', source: 'mobile', createdAt: '' })
  } else {
    userMsgId = localId('user')
    messages.value.push({
      id: userMsgId,
      role: 'user',
      text,
      reasoning: '',
      tools: [],
      createdAt: Date.now(),
      streaming: false,
    })
  }
  inputText.value = ''
  resetInputHeight()
  atBottom.value = true
  await scrollToBottom(true)

  sending.value = true
  try {
    if (!sid.value) {
      creating.value = true
      const res = await api.createSession(text)
      creating.value = false
      if (!res.sessionId) throw new ApiError('bad_response', '服务端未返回会话标识', 0)
      sid.value = res.sessionId
      dialogId.value = res.dialogId || dialogId.value
      title.value = res.title || title.value || text.slice(0, 24)
      busy.value = true
      emit('created', { id: res.sessionId, title: title.value })
    } else {
      const res = await api.sendMessage(sid.value, text, dialogId.value)
      if (!res.accepted) {
        // 失败：撤回乐观项（排队条 / 聊天气泡）
        if (queueId) inboxItems.value = inboxItems.value.filter((x) => x.id !== queueId)
        else removeMessage(userMsgId)
        toast(res.error || '发送失败', 'error')
        return
      }
      if (res.queued) {
        if (!willQueue) {
          // 本地状态过期（服务端判定仍在运行）→ 气泡转为排队项，避免立刻显示为已发送
          removeMessage(userMsgId)
          inboxItems.value.push({ id: localId('q'), content: text, type: 'next_turn', source: 'mobile', createdAt: '' })
        }
        toast('已进入队列，本轮结束后自动发送')
        loadInbox()
        return
      }
      // 立即发送：若此前误判为排队，把排队项还原为普通气泡
      if (willQueue) {
        inboxItems.value = inboxItems.value.filter((x) => x.id !== queueId)
        messages.value.push({
          id: userMsgId || localId('user'),
          role: 'user',
          text,
          reasoning: '',
          tools: [],
          createdAt: Date.now(),
          streaming: false,
        })
      }
      busy.value = true
    }
  } catch (err) {
    creating.value = false
    if (queueId) inboxItems.value = inboxItems.value.filter((x) => x.id !== queueId)
    else removeMessage(userMsgId)
    if (err instanceof ApiError && err.status === 401) {
      emit('expired')
    } else {
      inputText.value = text
      toastError(err)
    }
  } finally {
    sending.value = false
  }
}

async function stop() {
  if (!sid.value || stopping.value) return
  stopping.value = true
  try {
    await api.stopSession(sid.value)
    busy.value = false
    toast('已请求停止')
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) emit('expired')
    else toastError(err)
  } finally {
    stopping.value = false
  }
}

async function onAskSubmit({ answer, custom }) {
  const ask = askState.value
  if (!ask || !sid.value) return
  submittingAsk.value = true
  try {
    const body = { ask_id: ask.ask_id, answer }
    if (custom) body.custom = custom
    await api.answer(sid.value, body)
    askState.value = null
    askOpen.value = false
    toast('已提交作答')
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) emit('expired')
    else toastError(err)
  } finally {
    submittingAsk.value = false
  }
}

// ---------- 任务树 ----------

function openTasks() {
  if (!sid.value) {
    toast('会话尚未创建，暂无任务')
    return
  }
  tasksOpen.value = true
  loadTasks()
}

async function loadTasks() {
  if (!sid.value) return
  tasksLoading.value = true
  try {
    tasks.value = normalizeTaskTree(await api.tasks(sid.value))
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) emit('expired')
    else toastError(err)
  } finally {
    tasksLoading.value = false
  }
}

function scheduleTaskReload() {
  if (taskTimer) return
  taskTimer = setTimeout(() => {
    taskTimer = null
    if (tasksOpen.value) loadTasks()
  }, 600)
}

function selectTask(node) {
  taskDetail.value = node
}

// ---------- 文件浏览（只读） ----------

function formatSize(n) {
  const v = Number(n) || 0
  if (v < 1024) return `${v} B`
  if (v < 1024 * 1024) return `${(v / 1024).toFixed(1)} KB`
  return `${(v / 1024 / 1024).toFixed(1)} MB`
}

async function openFiles() {
  if (!sid.value) {
    toast('会话尚未创建，暂无文件')
    return
  }
  filesOpen.value = true
  filePreview.value = null
  fileStack.value = ['']
  await loadFiles('')
}

async function loadFiles(path) {
  filesLoading.value = true
  try {
    const r = normalizeFiles(await api.files(path || ''))
    filePath.value = r.path || path || ''
    fileEntries.value = r.entries
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) emit('expired')
    else toastError(err)
  } finally {
    filesLoading.value = false
  }
}

async function openFileEntry(entry) {
  if (!entry) return
  if (entry.isDir) {
    fileStack.value.push(entry.path)
    filePreview.value = null
    await loadFiles(entry.path)
    return
  }
  fileLoading.value = true
  try {
    filePreview.value = normalizeFileContent(await api.fileContent(entry.path))
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) emit('expired')
    else toastError(err)
  } finally {
    fileLoading.value = false
  }
}

async function upDir() {
  if (fileStack.value.length <= 1) return
  fileStack.value.pop()
  filePreview.value = null
  await loadFiles(fileStack.value[fileStack.value.length - 1])
}

// ---------- 输入框 ----------

function autoGrow() {
  const el = inputEl.value
  if (!el) return
  el.style.height = 'auto'
  el.style.height = `${Math.min(el.scrollHeight, 120)}px`
}

function resetInputHeight() {
  const el = inputEl.value
  if (el) el.style.height = 'auto'
}

function onKeydown(e) {
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault()
    send()
  }
}

let unsubscribe = null
let busyRecheckTimer = null

// 列表带来的「运行中」可能已过期（打开前一瞬刚结束）→ 有界复查一次，避免输入区被永久锁死。
async function recheckInitialBusy() {
  busyRecheckTimer = null
  if (!busy.value || !sid.value) return
  try {
    const list = await api.listSessions()
    const row = (Array.isArray(list) ? list : []).find((s) => s.id === sid.value)
    if (row && !row.running) busy.value = false
  } catch {
    // 复查失败保持现状（以事件为准）
  }
}

onMounted(() => {
  unsubscribe = subscribe(handleEvent)
  start()
  if (sid.value) loadInitial()
  else nextTick(() => inputEl.value && inputEl.value.focus())
  if (busy.value) busyRecheckTimer = setTimeout(recheckInitialBusy, 5000)
})

onBeforeUnmount(() => {
  if (unsubscribe) unsubscribe()
  if (taskTimer) clearTimeout(taskTimer)
  if (busyRecheckTimer) clearTimeout(busyRecheckTimer)
})
</script>

<template>
  <div class="chat">
    <header class="chat-head">
      <button class="icon-btn" aria-label="返回会话列表" @click="emit('back')">
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" aria-hidden="true">
          <path d="M15 5l-7 7 7 7" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
        </svg>
      </button>

      <div class="chat-title">
        <div class="title-text">{{ displayTitle }}</div>
        <div class="conn" :class="`conn-${status}`">
          <span class="conn-dot"></span>
          <span>{{ connLabel }}</span>
        </div>
      </div>

      <button
        v-if="askState && !askOpen"
        class="icon-btn ask-badge"
        aria-label="有待作答的问题"
        @click="askOpen = true"
      >
        ?
      </button>

      <button class="tasks-btn" @click="openFiles">文件</button>
      <button class="tasks-btn" @click="openTasks">任务</button>
    </header>

    <div ref="scroller" class="messages" @scroll="onScroll">
      <div v-if="loadingHistory && !messages.length" class="hint">加载中…</div>
      <button
        v-else-if="hasMore"
        class="load-more"
        :disabled="loadingHistory"
        @click="loadEarlier"
      >
        {{ loadingHistory ? '加载中…' : '加载更早的消息' }}
      </button>

      <MessageItem v-for="m in messages" :key="m.id" :msg="m" />

      <div v-if="!messages.length && !loadingHistory" class="empty-chat">
        <p class="empty-title">{{ sid ? '暂无消息' : '开始新的对话' }}</p>
        <p class="empty-desc">{{ sid ? '发送一条消息继续这个会话' : '在下方输入你的问题，回车发送' }}</p>
      </div>
    </div>

    <!-- 悬浮 inbox 图标：点击打开全量 inbox 列表页（两端共享；撤回后自动回聊天并回填输入框） -->
    <button v-if="sid" class="inbox-fab" aria-label="收件箱" @click="openInbox">
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" aria-hidden="true">
        <path d="M3 13h5l2 3h4l2-3h5" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
        <path d="M5 5h14l2 8v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4z" stroke="currentColor" stroke-width="2" stroke-linejoin="round" />
      </svg>
      <span v-if="queuedCount" class="inbox-fab-badge">{{ queuedCount }}</span>
    </button>

    <footer class="composer">
      <div v-if="busy || queuedCount" class="composer-strip">
        <div class="strip-info">
          <span v-if="queuedCount" class="strip-text">
            已排队 {{ queuedCount }} 条 · 本轮结束后自动发送
          </span>
          <span v-else class="strip-text">回复中 · 输入的消息将排队发送</span>
          <ul v-if="queuedCount" class="queue-list">
            <li v-for="q in inboxItems.slice(0, 3)" :key="q.id" class="queue-item">
              {{ q.content }}
            </li>
          </ul>
        </div>
        <button v-if="busy" class="stop-btn stop-btn-sm" :disabled="stopping || !sid" @click="stop">
          <span class="stop-square"></span>
          {{ !sid ? '发送中…' : stopping ? '正在停止…' : '停止' }}
        </button>
      </div>
      <div class="composer-row">
        <textarea
          ref="inputEl"
          v-model="inputText"
          class="composer-input"
          rows="1"
          :placeholder="busy ? '输入消息（发送后排队）' : '输入消息…'"
          enterkeyhint="send"
          @input="autoGrow"
          @keydown="onKeydown"
        ></textarea>
        <button class="send-btn" :disabled="!inputText.trim() || sending" @click="send">
          {{ busy ? '排队' : '发送' }}
        </button>
      </div>
    </footer>

    <AskPanel
      v-if="askState"
      :key="askState.ask_id"
      :ask="askState"
      :open="askOpen"
      :submitting="submittingAsk"
      @update:open="askOpen = $event"
      @submit="onAskSubmit"
      @later="askOpen = false"
    />

    <BottomSheet v-model="inboxOpen" title="收件箱">
      <div class="inbox-list">
        <p v-if="!inboxItems.length" class="hint">收件箱为空</p>
        <div v-for="it in inboxItems" :key="it.id" class="inbox-row" :class="{ 'is-intent': it.type === 'insert' }">
          <span class="inbox-src">{{ sourceLabel(it) }}</span>
          <span class="inbox-text">{{ it.content }}</span>
          <span class="inbox-acts">
            <button class="inbox-act" :disabled="inboxBusy" :aria-label="it.type === 'insert' ? '取消插话' : '插话'" @click="toggleInterject(it)">
              {{ it.type === 'insert' ? '取消插话' : '插话' }}
            </button>
            <button class="inbox-act danger" :disabled="inboxBusy" aria-label="删除" @click="inboxAct(it, 'discard')">删除</button>
            <button class="inbox-act" :disabled="inboxBusy" aria-label="撤回编辑" @click="recallInbox(it)">撤回</button>
          </span>
        </div>
      </div>
    </BottomSheet>

    <BottomSheet v-model="tasksOpen" title="任务">
      <div class="tasks">
        <div v-if="tasksLoading" class="hint">加载中…</div>
        <TaskTree v-else-if="tasks.length" :nodes="tasks" @select="selectTask" />
        <p v-else class="hint">暂无任务</p>

        <div v-if="taskDetail" class="task-detail">
          <div class="task-detail-head">
            <strong>{{ taskDetail.title }}</strong>
            <button class="link-btn" @click="taskDetail = null">收起</button>
          </div>
          <dl>
            <template v-if="taskDetail.state"><dt>状态</dt><dd>{{ taskDetail.state }}</dd></template>
            <template v-if="taskDetail.tool"><dt>工具</dt><dd>{{ taskDetail.tool }}</dd></template>
            <template v-if="taskDetail.kind"><dt>类型</dt><dd>{{ taskDetail.kind }}</dd></template>
            <template v-if="taskDetail.turnId"><dt>轮次</dt><dd>{{ taskDetail.turnId }}</dd></template>
            <template v-if="taskDetail.updatedAt"><dt>更新时间</dt><dd>{{ new Date(taskDetail.updatedAt).toLocaleString() }}</dd></template>
          </dl>
          <pre v-if="taskDetail.detail">{{ JSON.stringify(taskDetail.detail, null, 2) }}</pre>
        </div>
      </div>
    </BottomSheet>

    <BottomSheet v-model="filesOpen" title="文件">
      <div class="files">
        <div class="files-bar">
          <button v-if="fileStack.length > 1" class="link-btn" @click="upDir">← 上一级</button>
          <span class="files-path">{{ filePath || '工作区根目录' }}</span>
        </div>

        <div v-if="filesLoading || fileLoading" class="hint">加载中…</div>
        <ul v-else-if="!filePreview" class="file-list">
          <li v-for="e in fileEntries" :key="e.path || e.name">
            <button class="file-row" @click="openFileEntry(e)">
              <span class="file-icon">{{ e.isDir ? '▸' : '·' }}</span>
              <span class="file-name" :class="{ dir: e.isDir }">{{ e.name }}</span>
              <span v-if="!e.isDir" class="file-size">{{ formatSize(e.size) }}</span>
            </button>
          </li>
          <li v-if="!fileEntries.length" class="hint">空目录</li>
        </ul>

        <div v-else class="file-preview">
          <div class="files-bar">
            <button class="link-btn" @click="filePreview = null">← 返回列表</button>
            <span class="files-path">{{ filePreview.path }}</span>
          </div>
          <pre>{{ filePreview.content }}</pre>
          <p v-if="filePreview.truncated" class="hint">内容已截断</p>
        </div>
      </div>
    </BottomSheet>
  </div>
</template>

<style scoped>
.chat {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
  position: relative;
}

/* 悬浮 inbox 图标（右下角，输入区上方） */
.inbox-fab {
  position: absolute;
  right: 14px;
  bottom: calc(96px + var(--safe-bottom));
  z-index: 5;
  width: 42px;
  height: 42px;
  border-radius: 50%;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  background: var(--bg-elev);
  border: 1px solid var(--border);
  color: var(--accent);
  box-shadow: 0 4px 14px rgba(0, 0, 0, 0.28);
}

.inbox-fab:active {
  background: var(--bg-hover);
}

.inbox-fab-badge {
  position: absolute;
  top: -4px;
  right: -4px;
  min-width: 17px;
  height: 17px;
  line-height: 17px;
  padding: 0 4px;
  border-radius: 9px;
  background: var(--accent);
  color: #fff;
  font-size: 11px;
  text-align: center;
}

.inbox-list {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 4px 0 10px;
}

.inbox-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 10px;
  border-radius: 10px;
  border: 1px solid var(--border);
  background: var(--bg-elev);
}

/* 已登记插话意向：左侧强调色条 */
.inbox-row.is-intent {
  box-shadow: inset 3px 0 0 var(--accent);
}

.inbox-src {
  flex: none;
  font-size: 11px;
  color: var(--text-faint);
}

.inbox-text {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 13px;
  color: var(--text);
}

.inbox-acts {
  flex: none;
  display: flex;
  align-items: center;
  gap: 4px;
}

.inbox-act {
  min-height: 28px;
  padding: 0 8px;
  border-radius: 999px;
  border: 1px solid var(--border);
  background: var(--bg-elev-2);
  color: var(--text-dim);
  font-size: 12px;
}

.inbox-act:active {
  background: var(--bg-hover);
}

.inbox-act.danger {
  color: var(--danger);
  border-color: rgba(255, 107, 107, 0.4);
}

.chat-head {
  flex: none;
  display: flex;
  align-items: center;
  gap: 2px;
  padding: calc(6px + var(--safe-top)) 6px 6px;
  background: var(--bg);
  border-bottom: 1px solid var(--border);
}

.chat-title {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 1px;
  padding: 0 4px;
}

.title-text {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 15px;
  font-weight: 600;
}

.conn {
  display: flex;
  align-items: center;
  gap: 5px;
  font-size: 11.5px;
  color: var(--text-faint);
}

.conn-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--text-faint);
}

.conn-online .conn-dot {
  background: var(--ok);
  box-shadow: 0 0 0 3px rgba(61, 220, 151, 0.14);
}

.conn-connecting .conn-dot,
.conn-reconnecting .conn-dot {
  background: var(--warn);
  animation: pulse 1.2s ease-in-out infinite;
}

@keyframes pulse {
  0%,
  100% {
    opacity: 1;
  }
  50% {
    opacity: 0.3;
  }
}

.ask-badge {
  color: var(--warn);
  border: 1px solid rgba(240, 180, 41, 0.45);
  width: 30px;
  height: 30px;
  min-width: 30px;
  font-weight: 700;
  font-size: 13px;
}

.tasks-btn {
  flex: none;
  min-height: var(--tap);
  padding: 0 14px;
  margin-right: 6px;
  border-radius: 999px;
  border: 1px solid var(--border);
  color: var(--text-dim);
  font-size: 13px;
}

.tasks-btn:active {
  background: var(--bg-hover);
}

.messages {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  -webkit-overflow-scrolling: touch;
  display: flex;
  flex-direction: column;
  padding: 14px 14px 6px;
}

.load-more {
  align-self: center;
  margin-bottom: 12px;
  min-height: 32px;
  padding: 0 14px;
  border-radius: 999px;
  border: 1px solid var(--border);
  color: var(--text-faint);
  font-size: 12.5px;
}

.empty-chat {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 6px;
  padding: 40px 20px;
  text-align: center;
}

.empty-title {
  font-size: 15px;
  font-weight: 600;
  color: var(--text-dim);
}

.empty-desc {
  font-size: 13px;
  color: var(--text-faint);
}

.composer {
  flex: none;
  display: flex;
  flex-direction: column;
  align-items: stretch;
  gap: 8px;
  padding: 10px 14px calc(10px + var(--safe-bottom));
  background: var(--bg);
  border-top: 1px solid var(--border);
}

.composer-row {
  display: flex;
  align-items: flex-end;
  gap: 10px;
}

/* 运行中 / 有排队：状态条（排队提示 + 停止） */
.composer-strip {
  display: flex;
  align-items: flex-start;
  gap: 10px;
}

.strip-info {
  flex: 1;
  min-width: 0;
}

.strip-text {
  font-size: 12px;
  color: var(--text-faint);
}

.queue-list {
  margin: 4px 0 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 3px;
}

.queue-item {
  font-size: 12px;
  color: var(--text);
  background: var(--bg-elev);
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 4px 8px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.composer-input {
  flex: 1;
  min-width: 0;
  min-height: var(--tap);
  max-height: 120px;
  padding: 11px 14px;
  border-radius: 22px;
  border: 1px solid var(--border);
  background: var(--bg-elev);
  color: var(--text);
  font-size: 16px;
  line-height: 1.4;
  resize: none;
  outline: none;
  -webkit-appearance: none;
}

.composer-input::placeholder {
  color: var(--text-faint);
}

.composer-input:focus {
  border-color: var(--accent);
}

.send-btn {
  flex: none;
  min-width: 64px;
  min-height: var(--tap);
  padding: 0 16px;
  border-radius: 999px;
  background: var(--accent);
  color: #fff;
  font-size: 15px;
  font-weight: 600;
  transition: opacity 0.15s ease;
}

.send-btn:disabled {
  opacity: 0.4;
}

.stop-btn {
  flex: 1;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  min-height: var(--tap);
  border-radius: 22px;
  background: var(--danger-soft);
  border: 1px solid rgba(255, 107, 107, 0.4);
  color: var(--danger);
  font-size: 15px;
  font-weight: 600;
}

.stop-btn:disabled {
  opacity: 0.55;
}

/* 运行中状态条里的紧凑「停止」（发送输入框仍保留在下方） */
.stop-btn-sm {
  flex: none;
  min-height: 32px;
  height: 32px;
  padding: 0 12px;
  border-radius: 999px;
  font-size: 13px;
  gap: 6px;
}

.stop-square {
  width: 10px;
  height: 10px;
  border-radius: 2px;
  background: var(--danger);
}

.tasks {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 4px 0 10px;
}

.link-btn {
  align-self: flex-start;
  padding: 2px 0;
  border: 0;
  background: transparent;
  color: var(--accent);
  font: inherit;
  font-size: 12.5px;
  cursor: pointer;
}

.task-detail {
  margin-top: 6px;
  padding: 10px 12px;
  border-radius: 10px;
  background: var(--bg-elev);
  border: 1px solid var(--border);
}

.task-detail-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  margin-bottom: 8px;
  font-size: 13.5px;
}

.task-detail dl {
  display: grid;
  grid-template-columns: auto 1fr;
  gap: 4px 10px;
  margin: 0;
  font-size: 12.5px;
}

.task-detail dt {
  color: var(--text-faint);
}

.task-detail dd {
  margin: 0;
  color: var(--text-dim);
  word-break: break-all;
}

.task-detail pre {
  margin: 8px 0 0;
  padding: 8px;
  max-height: 220px;
  overflow: auto;
  border-radius: 8px;
  background: var(--bg-elev-2);
  font-size: 11.5px;
  white-space: pre-wrap;
  word-break: break-all;
}

.files {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 4px 0 10px;
}

.files-bar {
  display: flex;
  align-items: center;
  gap: 10px;
}

.files-path {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--text-faint);
  font-size: 12px;
}

.file-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.file-row {
  display: flex;
  align-items: center;
  gap: 9px;
  width: 100%;
  min-height: 40px;
  padding: 7px 10px;
  border-radius: 8px;
  border: 1px solid var(--border);
  background: var(--bg-elev-2);
  color: inherit;
  font: inherit;
  text-align: left;
  cursor: pointer;
}

.file-row:active {
  background: var(--bg-hover);
}

.file-icon {
  flex: none;
  width: 14px;
  text-align: center;
  color: var(--text-faint);
}

.file-name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 13.5px;
}

.file-name.dir {
  color: var(--accent);
}

.file-size {
  flex: none;
  color: var(--text-faint);
  font-size: 11.5px;
}

.file-preview pre {
  margin: 0;
  padding: 10px;
  max-height: 55vh;
  overflow: auto;
  border-radius: 10px;
  border: 1px solid var(--border);
  background: var(--bg-elev);
  font-size: 12px;
  line-height: 1.55;
  white-space: pre-wrap;
  word-break: break-word;
}
</style>
