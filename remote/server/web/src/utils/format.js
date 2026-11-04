function pad(n) {
  return n < 10 ? `0${n}` : String(n)
}

export function startOfToday() {
  const now = new Date()
  return new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime()
}

/** 会话列表时间：今天显示 HH:mm，昨天显示「昨天」，更早显示日期。 */
export function formatSessionTime(ms) {
  if (!ms) return ''
  const d = new Date(ms)
  const now = new Date()
  const todayStart = startOfToday()
  if (ms >= todayStart) return `${pad(d.getHours())}:${pad(d.getMinutes())}`
  const yesterdayStart = todayStart - 24 * 60 * 60 * 1000
  if (ms >= yesterdayStart) return '昨天'
  if (d.getFullYear() === now.getFullYear()) return `${d.getMonth() + 1}月${d.getDate()}日`
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

/** 消息时间：仅显示 HH:mm。 */
export function formatClock(ms) {
  if (!ms) return ''
  const d = new Date(ms)
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`
}
