<script setup>
import { onMounted, ref } from 'vue'

const props = defineProps({
  loading: { type: Boolean, default: false },
  error: { type: String, default: '' },
})

const emit = defineEmits(['submit'])

const username = ref('')
const password = ref('')
const localError = ref('')
const usernameEl = ref(null)

onMounted(() => {
  if (usernameEl.value) usernameEl.value.focus()
})

function submit() {
  if (props.loading) return
  const u = username.value.trim()
  if (!u || !password.value) {
    localError.value = '请输入用户名和密码'
    return
  }
  localError.value = ''
  emit('submit', { username: u, password: password.value })
}
</script>

<template>
  <div class="login">
    <div class="brand">
      <div class="logo">CP</div>
      <h1>ChonkPilot Remote</h1>
      <p>在手机上继续电脑端的对话</p>
    </div>

    <form class="form" @submit.prevent="submit">
      <label class="field">
        <span class="label">用户名</span>
        <input
          ref="usernameEl"
          v-model="username"
          class="input"
          type="text"
          autocomplete="username"
          autocapitalize="off"
          autocorrect="off"
          spellcheck="false"
          placeholder="请输入用户名"
        />
      </label>

      <label class="field">
        <span class="label">密码</span>
        <input
          v-model="password"
          class="input"
          type="password"
          autocomplete="current-password"
          placeholder="请输入密码"
        />
      </label>

      <p v-if="props.error || localError" class="error">{{ props.error || localError }}</p>

      <button class="btn btn-primary submit" type="submit" :disabled="props.loading">
        {{ props.loading ? '登录中…' : '登录' }}
      </button>
    </form>

    <p class="foot">账号由 ChonkPilot Remote Server 管理</p>
  </div>
</template>

<style scoped>
.login {
  flex: 1;
  display: flex;
  flex-direction: column;
  justify-content: center;
  gap: 28px;
  width: 100%;
  max-width: 420px;
  margin: 0 auto;
  padding: 32px 24px calc(32px + var(--safe-bottom));
}

.brand {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
  text-align: center;
}

.logo {
  width: 64px;
  height: 64px;
  display: flex;
  align-items: center;
  justify-content: center;
  border-radius: 18px;
  background: var(--accent-soft);
  border: 1px solid rgba(79, 140, 255, 0.35);
  color: var(--accent);
  font-size: 22px;
  font-weight: 700;
  letter-spacing: 1px;
}

.brand h1 {
  font-size: 20px;
  font-weight: 600;
}

.brand p {
  font-size: 13px;
  color: var(--text-dim);
}

.form {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 7px;
}

.label {
  font-size: 13px;
  color: var(--text-dim);
}

.error {
  padding: 10px 12px;
  border-radius: var(--radius-sm);
  background: var(--danger-soft);
  border: 1px solid rgba(255, 107, 107, 0.35);
  color: #ffb4b4;
  font-size: 13px;
  line-height: 1.5;
}

.submit {
  margin-top: 4px;
  min-height: 48px;
  font-size: 16px;
}

.foot {
  text-align: center;
  font-size: 12px;
  color: var(--text-faint);
}
</style>
