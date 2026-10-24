// A tiny toast queue. Operations in the console are fire-and-poll, so the
// result of a click has to appear without the user hunting for it — and it has
// to be dismissible, because a sticky banner turns into noise.

let seq = 0

export const toasts = $state([])

export function dismissToast(id) {
  const at = toasts.findIndex((x) => x.id === id)
  if (at >= 0) toasts.splice(at, 1)
}

// push shows a message. kind is one of info | ok | warn | error. A duration of
// 0 keeps the toast until it is dismissed, which is what errors deserve: a
// failure that disappears after three seconds is a failure nobody read.
// At most three are on screen — the fourth pushes the oldest out, so a burst of
// batch results cannot stack into a wall that covers the page.
const MAX_VISIBLE = 3

export function pushToast(text, kind = 'info', duration = 3200) {
  if (!text) return 0
  const id = ++seq
  toasts.push({ id, text, kind })
  while (toasts.length > MAX_VISIBLE) toasts.shift()
  if (duration > 0) setTimeout(() => dismissToast(id), duration)
  return id
}

export const toastOk = (text) => pushToast(text, 'ok')
export const toastErr = (text) => pushToast(text, 'error', 0)
