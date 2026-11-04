<script setup>
import { ref } from 'vue'
import { formatClock } from '../utils/format'

const props = defineProps({
  msg: { type: Object, required: true },
})

const reasoningOpen = ref(false)

function toggleReasoning() {
  reasoningOpen.value = !reasoningOpen.value
}

function toolClass(status) {
  const s = (status || '').toLowerCase()
  if (['running', 'started', 'pending', 'working'].includes(s)) return 'tool-running'
  if (['done', 'completed', 'complete', 'success', 'ok', 'finished'].includes(s)) return 'tool-ok'
  if (['error', 'failed', 'fail', 'cancelled', 'canceled', 'stopped'].includes(s)) return 'tool-error'
  return 'tool-idle'
}
</script>

<template>
  <div class="msg" :class="props.msg.role === 'user' ? 'msg-user' : 'msg-assistant'">
    <div class="bubble">
      <div v-if="props.msg.reasoning" class="reasoning">
        <button class="reason-toggle" @click="toggleReasoning">
          <svg
            class="arrow"
            :class="{ open: reasoningOpen }"
            width="12"
            height="12"
            viewBox="0 0 24 24"
            fill="none"
            aria-hidden="true"
          >
            <path d="M9 6l6 6-6 6" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
          </svg>
          <span>思考过程</span>
          <span v-if="props.msg.streaming" class="reason-live">进行中</span>
        </button>
        <div v-if="reasoningOpen" class="reason-text">{{ props.msg.reasoning }}</div>
      </div>

      <div v-if="props.msg.tools && props.msg.tools.length" class="tools">
        <div v-for="(tool, i) in props.msg.tools" :key="i" class="tool-line">
          <span class="tool-dot" :class="toolClass(tool.status)"></span>
          <span class="tool-name">{{ tool.name || '工具' }}</span>
          <span v-if="tool.status" class="tool-status">{{ tool.status }}</span>
        </div>
      </div>

      <div v-if="props.msg.text" class="text">{{ props.msg.text }}</div>
      <div v-else-if="props.msg.streaming" class="typing" aria-label="正在生成">
        <span></span><span></span><span></span>
      </div>
    </div>
    <div v-if="props.msg.createdAt" class="msg-time">
      {{ formatClock(props.msg.createdAt) }}
    </div>
  </div>
</template>

<style scoped>
.msg {
  display: flex;
  flex-direction: column;
  max-width: 100%;
  margin-bottom: 12px;
}

.msg-user {
  align-items: flex-end;
}

.msg-assistant {
  align-items: flex-start;
}

.bubble {
  max-width: 86%;
  padding: 10px 13px;
  border-radius: 16px;
  background: var(--bg-elev);
  border: 1px solid var(--border);
  overflow: hidden;
}

.msg-user .bubble {
  background: var(--accent);
  border-color: transparent;
  border-bottom-right-radius: 6px;
  color: #fff;
}

.msg-assistant .bubble {
  border-bottom-left-radius: 6px;
}

.text {
  white-space: pre-wrap;
  word-break: break-word;
  font-size: 15px;
  line-height: 1.6;
}

/* 思考过程 */
.reasoning {
  margin-bottom: 8px;
}

.reason-toggle {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  min-height: 28px;
  padding: 2px 2px;
  font-size: 12.5px;
  color: var(--text-dim);
}

.reason-toggle .arrow {
  transition: transform 0.15s ease;
}

.reason-toggle .arrow.open {
  transform: rotate(90deg);
}

.reason-live {
  padding: 0 6px;
  border-radius: 999px;
  background: var(--accent-soft);
  color: var(--accent);
  font-size: 11px;
  line-height: 18px;
}

.reason-text {
  margin-top: 6px;
  padding-left: 10px;
  border-left: 2px solid var(--border-strong);
  white-space: pre-wrap;
  word-break: break-word;
  font-size: 13px;
  line-height: 1.55;
  color: var(--text-dim);
}

/* 工具事件 */
.tools {
  display: flex;
  flex-direction: column;
  gap: 4px;
  margin-bottom: 8px;
}

.tool-line {
  display: flex;
  align-items: center;
  gap: 7px;
  padding: 5px 9px;
  border-radius: 8px;
  background: var(--bg-elev-2);
  border: 1px solid var(--border);
  font-size: 12px;
  color: var(--text-dim);
}

.tool-dot {
  flex: none;
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--text-faint);
}

.tool-dot.tool-running {
  background: var(--accent);
  animation: blink 1.2s ease-in-out infinite;
}

.tool-dot.tool-ok {
  background: var(--ok);
}

.tool-dot.tool-error {
  background: var(--danger);
}

.tool-name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--text);
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
}

.tool-status {
  flex: none;
  color: var(--text-faint);
  font-size: 11px;
}

@keyframes blink {
  0%,
  100% {
    opacity: 1;
  }
  50% {
    opacity: 0.3;
  }
}

/* 打字指示 */
.typing {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  height: 20px;
}

.typing span {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--text-faint);
  animation: bounce 1.2s ease-in-out infinite;
}

.typing span:nth-child(2) {
  animation-delay: 0.15s;
}

.typing span:nth-child(3) {
  animation-delay: 0.3s;
}

@keyframes bounce {
  0%,
  80%,
  100% {
    transform: translateY(0);
    opacity: 0.5;
  }
  40% {
    transform: translateY(-4px);
    opacity: 1;
  }
}

.msg-time {
  margin-top: 3px;
  padding: 0 4px;
  font-size: 11px;
  color: var(--text-faint);
}
</style>
