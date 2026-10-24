// Promise-based confirmation. window.confirm blocks the whole renderer and
// looks like the browser rather than the console; this gives the same guarantee
// (nothing happens until the user answers) with a dialog that belongs here.

export const confirmState = $state({
  open: false,
  title: '',
  body: '',
  detail: '',
  confirmLabel: '确认',
  cancelLabel: '取消',
  // 'info'    → neutral mark, primary confirm (routine)
  // 'warn'    → warn mark, primary confirm (caution, reversible)
  // 'danger'  → danger mark, solid-red confirm (irreversible)
  // Red is reserved for the last of the three: the rules page keeps severity
  // colour out of every button that still has a safe way back.
  tone: 'info',
})

let resolver = null

// ask opens the dialog and resolves true only on an explicit confirm.
export function ask({ title, body = '', detail = '', confirmLabel, cancelLabel, tone = 'info' }) {
  // A second ask while one is open resolves the first as cancelled, so a
  // stray click can never leave a promise hanging forever.
  if (resolver) {
    resolver(false)
    resolver = null
  }
  confirmState.open = true
  confirmState.title = title
  confirmState.body = body
  confirmState.detail = detail
  confirmState.confirmLabel = confirmLabel || '确认'
  confirmState.cancelLabel = cancelLabel || '取消'
  confirmState.tone = tone
  return new Promise((resolve) => {
    resolver = resolve
  })
}

export function settle(ok) {
  confirmState.open = false
  const fn = resolver
  resolver = null
  if (fn) fn(ok)
}
