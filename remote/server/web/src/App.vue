<script setup>
import { onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { api, ApiError, errorMessage } from './api'
import {
  restartEventStream,
  startEventStream,
  stopEventStream,
  subscribeEvents,
} from './composables/useEventStream'
import { toast } from './composables/useToast'
import LoginView from './views/LoginView.vue'
import SessionListView from './views/SessionListView.vue'
import ChatView from './views/ChatView.vue'
import ToastHost from './components/ToastHost.vue'

// view: boot | login | list | chat
const view = ref('boot')
const me = ref(null)

const authLoading = ref(false)
const authError = ref('')

const chat = reactive({ id: '', title: '', running: false })
const chatNonce = ref(0)

let unsubscribe = null

function enterList() {
  view.value = 'list'
}

function openSession({ id, title, running }) {
  chat.id = id || ''
  chat.title = title || ''
  chat.running = Boolean(running)
  chatNonce.value += 1
  view.value = 'chat'
}

function openNewChat() {
  chat.id = ''
  chat.title = ''
  chat.running = false
  chatNonce.value += 1
  view.value = 'chat'
}

function onSessionCreated({ id, title }) {
  chat.id = id || chat.id
  if (title) chat.title = title
}

function backToList() {
  view.value = 'list'
}

function onContextChanged({ deviceId, workspaceId }) {
  me.value = { ...(me.value || {}), deviceId, workspaceId }
  // 服务端 SSE 订阅按「用户 + 设备 + 工作区」绑定：上下文变化后必须重开。
  restartEventStream()
}

// startStreamIfReady：没有上下文时 /api/events 会 400（服务端按绑定过滤），
// 等列表页自动选中设备/工作区后再建连。
function startStreamIfReady() {
  if (me.value && me.value.deviceId) startEventStream()
}

function onExpired() {
  stopEventStream()
  me.value = null
  view.value = 'login'
  authError.value = '登录已过期，请重新登录'
}

async function submitLogin({ username, password }) {
  authLoading.value = true
  authError.value = ''
  try {
    await api.login(username, password)
    me.value = await api.me()
    startStreamIfReady()
    enterList()
  } catch (err) {
    authError.value = errorMessage(err)
  } finally {
    authLoading.value = false
  }
}

async function logout() {
  try {
    await api.logout()
  } catch {
    // 忽略：本地直接回登录页
  }
  stopEventStream()
  me.value = null
  chat.id = ''
  chat.title = ''
  view.value = 'login'
  authError.value = ''
  toast('已退出登录')
}

onMounted(async () => {
  unsubscribe = subscribeEvents((type) => {
    if (type === '__unauthorized') onExpired()
  })
  try {
    me.value = await api.me()
    startStreamIfReady()
    enterList()
  } catch (err) {
    view.value = 'login'
    if (!(err instanceof ApiError && err.status === 401)) {
      authError.value = errorMessage(err)
    }
  }
})

// 组件常驻，手动清理订阅
onBeforeUnmount(() => {
  if (unsubscribe) unsubscribe()
  stopEventStream()
})
</script>

<template>
  <div class="page app-page">
    <div v-if="view === 'boot'" class="boot">
      <div class="boot-logo">CP</div>
      <p>正在连接…</p>
    </div>

    <LoginView
      v-else-if="view === 'login'"
      :loading="authLoading"
      :error="authError"
      @submit="submitLogin"
    />

    <SessionListView
      v-else-if="view === 'list'"
      :context="me"
      :username="me ? me.username : ''"
      @open="openSession"
      @new-chat="openNewChat"
      @logout="logout"
      @expired="onExpired"
      @context-changed="onContextChanged"
    />

    <ChatView
      v-else
      :key="chatNonce"
      :session="chat"
      @back="backToList"
      @created="onSessionCreated"
      @expired="onExpired"
    />

    <ToastHost />
  </div>
</template>

<style scoped>
.app-page {
  width: 100%;
  height: 100%;
  padding-left: var(--safe-left);
  padding-right: var(--safe-right);
}

.boot {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 14px;
  color: var(--text-faint);
  font-size: 13px;
}

.boot-logo {
  width: 56px;
  height: 56px;
  display: flex;
  align-items: center;
  justify-content: center;
  border-radius: 16px;
  background: var(--bg-elev-2);
  border: 1px solid var(--border);
  color: var(--accent);
  font-weight: 700;
  letter-spacing: 1px;
}
</style>
