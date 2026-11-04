import { reactive } from 'vue'
import { errorMessage } from '../api'

const toasts = reactive([])
let seq = 0

export function toast(message, kind = 'info', ttl = 3200) {
  if (!message) return
  const item = { id: ++seq, message, kind }
  toasts.push(item)
  setTimeout(() => {
    const i = toasts.findIndex((t) => t.id === item.id)
    if (i >= 0) toasts.splice(i, 1)
  }, ttl)
}

export function toastError(err) {
  toast(errorMessage(err), 'error')
}

export function useToasts() {
  return toasts
}
