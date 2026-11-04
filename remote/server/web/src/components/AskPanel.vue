<script setup>
import { computed, ref } from 'vue'
import BottomSheet from './BottomSheet.vue'

const props = defineProps({
  ask: { type: Object, default: null },
  open: { type: Boolean, default: false },
  submitting: { type: Boolean, default: false },
})

const emit = defineEmits(['update:open', 'submit', 'later'])

const singleValue = ref('')
const multiValues = ref([])
const customText = ref('')
const error = ref('')

const options = computed(() => (props.ask && props.ask.options) || [])
const multi = computed(() => Boolean(props.ask && props.ask.multi))
const allowCustom = computed(() => Boolean(props.ask && props.ask.custom))

function isSelected(value) {
  return multi.value ? multiValues.value.includes(value) : singleValue.value === value
}

function pick(value) {
  error.value = ''
  if (multi.value) {
    const i = multiValues.value.indexOf(value)
    if (i >= 0) multiValues.value.splice(i, 1)
    else multiValues.value.push(value)
  } else {
    singleValue.value = value
  }
}

function submit() {
  const custom = customText.value.trim()
  const answer = multi.value ? multiValues.value.slice() : singleValue.value
  const empty = Array.isArray(answer) ? answer.length === 0 : !answer
  if (empty && !custom) {
    error.value = options.value.length ? '请选择一项或填写内容' : '请填写作答内容'
    return
  }
  error.value = ''
  emit('submit', { answer: empty ? custom : answer, custom })
}

function close() {
  emit('update:open', false)
}
</script>

<template>
  <BottomSheet
    :model-value="open"
    title="需要你的作答"
    :dismissible="false"
    @update:modelValue="emit('update:open', $event)"
  >
    <div class="ask">
      <p class="question">{{ ask ? ask.question : '' }}</p>

      <div v-if="options.length" class="options">
        <button
          v-for="opt in options"
          :key="opt.value"
          class="option"
          :class="{ selected: isSelected(opt.value) }"
          @click="pick(opt.value)"
        >
          <span class="mark" :class="multi ? 'mark-square' : 'mark-circle'">
            <span v-if="isSelected(opt.value)" class="mark-inner"></span>
          </span>
          <span class="option-label">{{ opt.label }}</span>
        </button>
      </div>

      <textarea
        v-if="allowCustom"
        v-model="customText"
        class="input custom"
        rows="3"
        placeholder="补充说明（可选）"
      ></textarea>

      <p v-if="error" class="err">{{ error }}</p>

      <div class="actions">
        <button class="btn btn-ghost" @click="emit('later')">稍后</button>
        <button class="btn btn-primary" :disabled="submitting" @click="submit">
          {{ submitting ? '提交中…' : '提交' }}
        </button>
      </div>
    </div>
  </BottomSheet>
</template>

<style scoped>
.ask {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding-bottom: 6px;
}

.question {
  font-size: 15px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
}

.options {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.option {
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
  transition: border-color 0.15s ease, background 0.15s ease;
}

.option.selected {
  border-color: var(--accent);
  background: var(--accent-soft);
}

.mark {
  flex: none;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 18px;
  height: 18px;
  border: 1.5px solid var(--border-strong);
  border-radius: 50%;
}

.mark-square {
  border-radius: 5px;
}

.option.selected .mark {
  border-color: var(--accent);
}

.mark-inner {
  width: 10px;
  height: 10px;
  border-radius: 50%;
  background: var(--accent);
}

.mark-square .mark-inner {
  border-radius: 2px;
}

.option-label {
  flex: 1;
  min-width: 0;
  font-size: 14.5px;
  word-break: break-word;
}

.custom {
  resize: none;
  min-height: 76px;
  line-height: 1.5;
}

.err {
  color: var(--danger);
  font-size: 12.5px;
}

.actions {
  display: flex;
  gap: 10px;
  padding-top: 2px;
}

.actions .btn {
  flex: 1;
}
</style>
