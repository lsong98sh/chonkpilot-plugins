<script setup>
defineProps({
  nodes: { type: Array, default: () => [] },
  depth: { type: Number, default: 0 },
})

const emit = defineEmits(['select'])

function stateClass(state) {
  const s = (state || '').toLowerCase()
  if (['running', 'started', 'pending', 'working', 'in_progress'].includes(s)) return 'state-running'
  if (['done', 'completed', 'complete', 'success', 'ok', 'finished'].includes(s)) return 'state-done'
  if (['error', 'failed', 'fail', 'cancelled', 'canceled', 'stopped'].includes(s)) return 'state-error'
  return ''
}
</script>

<template>
  <ul class="tree" :class="{ nested: depth > 0 }">
    <li v-for="node in nodes" :key="node.id || node.title" class="node">
      <button class="node-row" type="button" @click="emit('select', node)">
        <span class="node-dot" :class="stateClass(node.state)"></span>
        <span class="node-title">{{ node.title }}</span>
        <span v-if="node.state" class="node-state">{{ node.state }}</span>
      </button>
      <TaskTree
        v-if="node.children && node.children.length"
        :nodes="node.children"
        :depth="depth + 1"
        @select="emit('select', $event)"
      />
    </li>
  </ul>
</template>

<style scoped>
.tree {
  list-style: none;
  margin: 0;
  padding: 0;
}

.tree.nested {
  margin-left: 14px;
  padding-left: 12px;
  border-left: 1px solid var(--border);
}

.node {
  padding: 2px 0;
}

.node-row {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  min-height: 34px;
  padding: 6px 10px;
  border-radius: 8px;
  background: var(--bg-elev-2);
  border: 1px solid var(--border);
  color: inherit;
  font: inherit;
  text-align: left;
  cursor: pointer;
}

.node-row:active {
  background: var(--bg-hover);
}

.node-dot {
  flex: none;
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--text-faint);
}

.node-dot.state-running {
  background: var(--accent);
  animation: blink 1.2s ease-in-out infinite;
}

.node-dot.state-done {
  background: var(--ok);
}

.node-dot.state-error {
  background: var(--danger);
}

.node-title {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 13.5px;
}

.node-state {
  flex: none;
  font-size: 11px;
  color: var(--text-faint);
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
</style>
