<script setup>
const props = defineProps({
  modelValue: { type: Boolean, default: false },
  title: { type: String, default: '' },
  dismissible: { type: Boolean, default: true },
})

const emit = defineEmits(['update:modelValue'])

function close() {
  if (!props.dismissible) return
  emit('update:modelValue', false)
}
</script>

<template>
  <Teleport to="body">
    <Transition name="sheet">
      <div v-if="modelValue" class="sheet-mask" @click="close">
        <div class="sheet" role="dialog" aria-modal="true" :aria-label="title" @click.stop>
          <div class="sheet-grip"></div>
          <div class="sheet-head">
            <h2>{{ title }}</h2>
            <button class="icon-btn" aria-label="关闭" @click="emit('update:modelValue', false)">
              <svg width="18" height="18" viewBox="0 0 24 24" fill="none" aria-hidden="true">
                <path d="M6 6l12 12M18 6L6 18" stroke="currentColor" stroke-width="2" stroke-linecap="round" />
              </svg>
            </button>
          </div>
          <div class="sheet-body">
            <slot />
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
