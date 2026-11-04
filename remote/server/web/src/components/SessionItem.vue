<script setup>
import { formatSessionTime } from '../utils/format'

defineProps({
  session: { type: Object, required: true },
})

const emit = defineEmits(['select'])
</script>

<template>
  <button class="session-item" @click="emit('select', session)">
    <div class="row">
      <span class="title">{{ session.title }}</span>
      <span class="time">{{ formatSessionTime(session.updatedAt) }}</span>
    </div>
    <div class="row sub">
      <span v-if="session.summary" class="summary">{{ session.summary }}</span>
      <span v-else class="summary spacer"></span>
      <span v-if="session.running" class="running">
        <span class="dot"></span>运行中
      </span>
    </div>
  </button>
</template>

<style scoped>
.session-item {
  display: block;
  width: 100%;
  min-height: 68px;
  padding: 13px 16px;
  text-align: left;
  border-radius: var(--radius);
  background: var(--bg-elev);
  border: 1px solid var(--border);
  transition: background 0.15s ease, border-color 0.15s ease;
}

.session-item:active {
  background: var(--bg-elev-2);
  border-color: var(--border-strong);
}

.row {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}

.title {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 15px;
  font-weight: 600;
  color: var(--text);
}

.time {
  flex: none;
  font-size: 12px;
  color: var(--text-faint);
}

.sub {
  margin-top: 5px;
}

.summary {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 13px;
  color: var(--text-dim);
}

.summary.empty,
.summary.spacer {
  color: var(--text-faint);
}

.running {
  flex: none;
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 12px;
  color: var(--ok);
}

.running .dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--ok);
  box-shadow: 0 0 0 3px rgba(61, 220, 151, 0.16);
  animation: pulse 1.4s ease-in-out infinite;
}

@keyframes pulse {
  0%,
  100% {
    opacity: 1;
  }
  50% {
    opacity: 0.35;
  }
}
</style>
