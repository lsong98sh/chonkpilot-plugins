<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { api, ApiError, errorMessage } from '../api'
import { subscribeEvents } from '../composables/useEventStream'
import { toast } from '../composables/useToast'
import { startOfToday } from '../utils/format'
import BottomSheet from '../components/BottomSheet.vue'
import SessionItem from '../components/SessionItem.vue'

const props = defineProps({
  context: { type: Object, default: null },
  username: { type: String, default: '' },
})

const emit = defineEmits(['open', 'new-chat', 'logout', 'expired', 'context-changed'])

const devices = ref([])
const sessions = ref([])
const loading = ref(true)
const error = ref('')
const search = ref('')
const filter = ref('all')
const contextOpen = ref(false)
const currentDeviceId = ref('')
const currentWorkspaceId = ref('')

const filters = [
  { id: 'all', label: '全部' },
  { id: 'running', label: '运行中' },
  { id: 'today', label: '今天' },
]

const currentDevice = computed(
  () => devices.value.find((d) => d.id === currentDeviceId.value) || null
)

const currentWorkspace = computed(() => {
  const dev = currentDevice.value
  if (!dev) return null
  return dev.workspaces.find((w) => w.id === currentWorkspaceId.value) || null
})

const contexts = computed(() =>
  devices.value.flatMap((device) =>
    device.workspaces.length
      ? device.workspaces.map((workspace) => ({ device, workspace }))
      : [{ device, workspace: null }]
  )
)

const canSwitch = computed(() => contexts.value.length > 1)

const contextLabel = computed(() => {
  const dev = currentDevice.value
  if (!dev) return '未连接任何电脑'
  const ws = currentWorkspace.value
  return ws ? `${dev.name} · ${ws.name}` : `${dev.name} · 无打开的工作区`
})

const canNewChat = computed(() => Boolean(currentDeviceId.value && currentWorkspaceId.value))

const filtered = computed(() => {
  const q = search.value.trim().toLowerCase()
  const todayStart = startOfToday()
  return sessions.value
    .filter((s) => {
      if (filter.value === 'running' && !s.running) return false
      if (filter.value === 'today' && s.updatedAt < todayStart) return false
      if (q) {
        const hay = `${s.title} ${s.summary}`.toLowerCase()
        if (!hay.includes(q)) return false
      }
      return true
    })
    .slice()
    .sort((a, b) => b.updatedAt - a.updatedAt)
})

function handleAuth(err) {
  if (err instanceof ApiError && err.status === 401) {
    emit('expired')
    return true
  }
  return false
}

async function resolveContext() {
  if (!devices.value.length) {
    currentDeviceId.value = ''
    currentWorkspaceId.value = ''
    return
  }
  const preferredDevice = props.context ? props.context.deviceId : ''
  const preferredWorkspace = props.context ? props.context.workspaceId : ''

  // 优先沿用上次选择，但离线设备不沿用（否则会停在一台已断开电脑的空列表上）。
  let device = devices.value.find((d) => d.id === preferredDevice && d.online)
  if (!device) device = devices.value.find((d) => d.online) || devices.value.find((d) => d.id === preferredDevice) || devices.value[0]
  const workspaces = device.workspaces || []
  const workspace = workspaces.find((w) => w.id === preferredWorkspace) || workspaces[0] || null

  currentDeviceId.value = device.id
  currentWorkspaceId.value = workspace ? workspace.id : ''

  const changed =
    device.id !== preferredDevice || (workspace ? workspace.id : '') !== preferredWorkspace
  if (changed && device.id) {
    try {
      await api.setContext(device.id, currentWorkspaceId.value)
    } catch (err) {
      // 电脑离线时仍尝试拉取列表（服务端可能仍有缓存）
      if (handleAuth(err)) throw err
    }
    // 同步给 App，避免返回列表时用旧上下文覆盖当前选择
    emit('context-changed', {
      deviceId: currentDeviceId.value,
      workspaceId: currentWorkspaceId.value,
    })
  }
}

async function fetchSessions() {
  sessions.value = await api.listSessions()
}

async function load(silent = false) {
  if (!silent) {
    loading.value = true
    error.value = ''
  }
  try {
    devices.value = await api.listDevices()
    await resolveContext()
    await fetchSessions()
    error.value = ''
  } catch (err) {
    if (handleAuth(err)) return
    if (!silent) error.value = errorMessage(err)
  } finally {
    loading.value = false
  }
}

async function pickContext(item) {
  if (!item.workspace) {
    toast('该电脑当前没有打开的工作区', 'error')
    return
  }
  const same =
    item.device.id === currentDeviceId.value && item.workspace.id === currentWorkspaceId.value
  contextOpen.value = false
  if (same) return
  currentDeviceId.value = item.device.id
  currentWorkspaceId.value = item.workspace.id
  loading.value = true
  try {
    await api.setContext(item.device.id, item.workspace.id)
    await fetchSessions()
    emit('context-changed', { deviceId: item.device.id, workspaceId: item.workspace.id })
  } catch (err) {
    if (!handleAuth(err)) toast(errorMessage(err), 'error')
  } finally {
    loading.value = false
  }
}

function onSelect(session) {
  if (!session.id) {
    toast('该会话缺少标识，无法打开', 'error')
    return
  }
  emit('open', { id: session.id, title: session.title, running: session.running })
}

let unsubscribe = null
let refreshTimer = null

// 轮次开始/结束 → 轻量刷新会话列表（运行中标记与时间随之更新；无需轮询）。
function scheduleRefresh() {
  if (refreshTimer) return
  refreshTimer = setTimeout(async () => {
    refreshTimer = null
    try {
      await fetchSessions()
    } catch (err) {
      handleAuth(err)
    }
  }, 300)
}

onMounted(() => {
  unsubscribe = subscribeEvents((type) => {
    if (type === '__reconnected') load(true)
    else if (type === 'turn.start' || type === 'turn.complete') scheduleRefresh()
  })
  load()
})

onBeforeUnmount(() => {
  if (unsubscribe) unsubscribe()
  if (refreshTimer) clearTimeout(refreshTimer)
})
</script>

<template>
  <div class="list-page">
    <header class="head">
      <div class="head-top">
        <div class="who">
          <span class="mini-logo">CP</span>
          <span class="user">{{ props.username || 'Remote' }}</span>
        </div>
        <button class="icon-btn" aria-label="退出登录" @click="emit('logout')">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" aria-hidden="true">
            <path
              d="M15 4h3a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2h-3M10 8l-4 4 4 4M6 12h10"
              stroke="currentColor"
              stroke-width="1.8"
              stroke-linecap="round"
              stroke-linejoin="round"
            />
          </svg>
        </button>
      </div>

      <button
        class="ctx-chip"
        :class="{ static: !canSwitch }"
        :aria-label="canSwitch ? '切换设备与工作区' : contextLabel"
        @click="canSwitch && (contextOpen = true)"
      >
        <span
          class="ctx-dot"
          :class="{ offline: !currentDevice || !currentDevice.online }"
        ></span>
        <span class="ctx-text">{{ contextLabel }}</span>
        <svg
          v-if="canSwitch"
          class="ctx-arrow"
          width="14"
          height="14"
          viewBox="0 0 24 24"
          fill="none"
          aria-hidden="true"
        >
          <path d="M6 9l6 6 6-6" stroke="currentColor" stroke-width="2" stroke-linecap="round" />
        </svg>
      </button>

      <div class="search">
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" aria-hidden="true">
          <circle cx="11" cy="11" r="7" stroke="currentColor" stroke-width="1.8" />
          <path d="M20 20l-3.5-3.5" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" />
        </svg>
        <input
          v-model="search"
          class="search-input"
          type="search"
          placeholder="搜索会话"
          enterkeyhint="search"
        />
        <button v-if="search" class="search-clear" aria-label="清空搜索" @click="search = ''">
          ✕
        </button>
      </div>

      <div class="filters">
        <button
          v-for="f in filters"
          :key="f.id"
          class="chip"
          :class="{ active: filter === f.id }"
          @click="filter = f.id"
        >
          {{ f.label }}
        </button>
      </div>
    </header>

    <div class="list-body">
      <div v-if="error" class="error-box">
        <p>{{ error }}</p>
        <button class="btn" @click="load()">重试</button>
      </div>

      <div v-else-if="loading && !sessions.length" class="hint">加载中…</div>

      <div v-else-if="!devices.length" class="empty">
        <p class="empty-title">还没有电脑连接</p>
        <p class="empty-desc">请先在电脑上打开 ChonkPilot，并在 Remote 插件里连接本服务器。</p>
        <button class="btn" @click="load()">刷新</button>
      </div>

      <div v-else-if="!sessions.length" class="empty">
        <p class="empty-title">还没有会话</p>
        <p class="empty-desc">新建一个聊天，开始在电脑上思考。</p>
        <button class="btn btn-primary" :disabled="!canNewChat" @click="emit('new-chat')">
          ＋ 新建聊天
        </button>
      </div>

      <div v-else-if="!filtered.length" class="empty">
        <p class="empty-title">没有符合条件的会话</p>
        <p class="empty-desc">换个关键词或筛选条件试试。</p>
      </div>

      <template v-else>
        <SessionItem v-for="s in filtered" :key="s.key" :session="s" @select="onSelect" />
      </template>
    </div>

    <button class="fab" :disabled="!canNewChat" @click="emit('new-chat')">
      <span class="fab-plus">＋</span>
      新建聊天
    </button>

    <BottomSheet v-model="contextOpen" title="切换设备 / 工作区">
      <div class="ctx-list">
        <button
          v-for="(item, i) in contexts"
          :key="`${item.device.id}-${item.workspace ? item.workspace.id : i}`"
          class="ctx-item"
          :class="{
            active:
              item.device.id === currentDeviceId &&
              (item.workspace ? item.workspace.id === currentWorkspaceId : false),
          }"
          @click="pickContext(item)"
        >
          <span class="ctx-dot" :class="{ offline: !item.device.online }"></span>
          <span class="ctx-name">
            <span class="ctx-device">{{ item.device.name }}</span>
            <span class="ctx-ws">{{ item.workspace ? item.workspace.name : '（无打开的工作区）' }}</span>
          </span>
          <svg
            v-if="
              item.device.id === currentDeviceId &&
              item.workspace &&
              item.workspace.id === currentWorkspaceId
            "
            width="16"
            height="16"
            viewBox="0 0 24 24"
            fill="none"
            aria-hidden="true"
          >
            <path d="M5 12l5 5L20 7" stroke="var(--accent)" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" />
          </svg>
        </button>
        <p v-if="!contexts.length" class="hint">没有可用的设备</p>
      </div>
    </BottomSheet>
  </div>
</template>

<style scoped>
.list-page {
  position: relative;
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
}

.head {
  flex: none;
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: calc(10px + var(--safe-top)) 16px 10px;
  background: var(--bg);
  border-bottom: 1px solid var(--border);
}

.head-top {
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.who {
  display: flex;
  align-items: center;
  gap: 9px;
  min-width: 0;
}

.mini-logo {
  flex: none;
  width: 26px;
  height: 26px;
  display: flex;
  align-items: center;
  justify-content: center;
  border-radius: 8px;
  background: var(--accent-soft);
  color: var(--accent);
  font-size: 11px;
  font-weight: 700;
}

.user {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 14px;
  color: var(--text-dim);
}

.ctx-chip {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  min-height: var(--tap);
  padding: 0 12px;
  border-radius: var(--radius);
  border: 1px solid var(--border);
  background: var(--bg-elev);
  color: var(--text);
  font-size: 13.5px;
  text-align: left;
}

.ctx-chip.static {
  cursor: default;
}

.ctx-dot {
  flex: none;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--ok);
  box-shadow: 0 0 0 3px rgba(61, 220, 151, 0.15);
}

.ctx-dot.offline {
  background: var(--text-faint);
  box-shadow: 0 0 0 3px rgba(107, 116, 130, 0.15);
}

.ctx-text {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.ctx-arrow {
  flex: none;
  color: var(--text-faint);
}

.search {
  display: flex;
  align-items: center;
  gap: 8px;
  min-height: var(--tap);
  padding: 0 12px;
  border-radius: var(--radius);
  background: var(--bg-elev);
  border: 1px solid var(--border);
  color: var(--text-faint);
}

.search:focus-within {
  border-color: var(--accent);
}

.search-input {
  flex: 1;
  min-width: 0;
  min-height: 42px;
  border: 0;
  background: transparent;
  color: var(--text);
  font-size: 16px; /* ≥16px：避免 iOS 聚焦自动放大 */
  outline: none;
  -webkit-appearance: none;
}

.search-input::placeholder {
  color: var(--text-faint);
}

.search-input::-webkit-search-cancel-button {
  display: none;
}

.search-clear {
  flex: none;
  width: 24px;
  height: 24px;
  border-radius: 50%;
  background: var(--bg-elev-2);
  color: var(--text-dim);
  font-size: 11px;
}

.filters {
  display: flex;
  gap: 8px;
  overflow-x: auto;
  scrollbar-width: none;
}

.filters::-webkit-scrollbar {
  display: none;
}

.list-body {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  -webkit-overflow-scrolling: touch;
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 12px 16px calc(96px + var(--safe-bottom));
}

.error-box {
  display: flex;
  flex-direction: column;
  gap: 12px;
  align-items: flex-start;
  padding: 16px;
  border-radius: var(--radius);
  background: var(--danger-soft);
  border: 1px solid rgba(255, 107, 107, 0.3);
  color: #ffb4b4;
  font-size: 13px;
}

.empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
  padding: 56px 20px;
  text-align: center;
}

.empty-title {
  font-size: 15px;
  font-weight: 600;
}

.empty-desc {
  font-size: 13px;
  color: var(--text-faint);
  line-height: 1.6;
  max-width: 280px;
}

.fab {
  position: absolute;
  right: 16px;
  bottom: calc(18px + var(--safe-bottom));
  display: inline-flex;
  align-items: center;
  gap: 6px;
  min-height: var(--tap);
  padding: 0 20px;
  border-radius: 999px;
  background: var(--accent);
  color: #fff;
  font-size: 14.5px;
  font-weight: 600;
  box-shadow: 0 8px 24px rgba(79, 140, 255, 0.35);
  transition: opacity 0.15s ease;
}

.fab:disabled {
  opacity: 0.4;
  box-shadow: none;
}

.fab-plus {
  font-size: 17px;
  line-height: 1;
}

.ctx-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 4px 0 10px;
}

.ctx-item {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  min-height: var(--tap);
  padding: 10px 12px;
  border-radius: var(--radius-sm);
  border: 1px solid var(--border);
  background: var(--bg-elev-2);
  text-align: left;
}

.ctx-item.active {
  border-color: var(--accent);
  background: var(--accent-soft);
}

.ctx-name {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.ctx-device {
  font-size: 14px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.ctx-ws {
  font-size: 12px;
  color: var(--text-faint);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
