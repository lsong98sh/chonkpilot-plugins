import { readonly, ref } from 'vue'
import { api, ApiError } from '../api'
import { toast } from './useToast'

// 全应用共用一个 EventSource，避免多页面各自建连。
const status = ref('idle') // idle | connecting | online | reconnecting

const listeners = new Set()
let source = null
let started = false
let errorCount = 0

function dispatch(type, data) {
  listeners.forEach((fn) => {
    try {
      fn(type, data)
    } catch (err) {
      console.error('[remote] event handler failed', err)
    }
  })
}

function open() {
  if (source) return
  status.value = errorCount ? 'reconnecting' : 'connecting'
  let es
  try {
    es = new EventSource('/api/events', { withCredentials: true })
  } catch {
    status.value = 'reconnecting'
    return
  }
  source = es

  es.onopen = () => {
    const wasDown = status.value === 'reconnecting'
    status.value = 'online'
    errorCount = 0
    if (wasDown) dispatch('__reconnected', {})
  }

  es.onmessage = (ev) => {
    if (!ev || !ev.data) return
    let frame
    try {
      frame = JSON.parse(ev.data)
    } catch {
      return
    }
    const type = frame && (frame.type || frame.event)
    if (!type) return
    const data = (frame && (frame.data ?? frame.payload)) || {}
    // notice 无 session_id，全局提示
    if (type === 'notice') {
      toast(data.message || data.text || '提示')
      return
    }
    dispatch(type, data)
  }

  es.onerror = () => {
    status.value = 'reconnecting'
    errorCount += 1
    // EventSource 会自行重连；连续失败时校验登录态，过期则回登录页。
    if (errorCount === 3) {
      api
        .me()
        .then(() => {
          errorCount = 0
        })
        .catch((err) => {
          if (err instanceof ApiError && err.status === 401) dispatch('__unauthorized', {})
        })
    }
  }
}

export function startEventStream() {
  if (started) return
  started = true
  open()
}

export function stopEventStream() {
  started = false
  errorCount = 0
  status.value = 'idle'
  if (source) {
    source.close()
    source = null
  }
}

// restartEventStream 关闭后立即重开（切换设备/工作区后需要重新绑定服务端订阅）。
export function restartEventStream() {
  stopEventStream()
  startEventStream()
}

export function subscribeEvents(fn) {
  listeners.add(fn)
  return () => listeners.delete(fn)
}

export function useEventStream() {
  return {
    status: readonly(status),
    start: startEventStream,
    stop: stopEventStream,
    subscribe: subscribeEvents,
  }
}
