// Focus handling for surfaces that sit above the page.
//
// A dialog is a place the keyboard has to be able to leave the way it came in,
// so every one of them remembers what was focused before it opened and gives
// focus back on close. Only true modals trap Tab; a non-modal drawer must not,
// because the page behind it stays usable by design.

const FOCUSABLE =
  'a[href], button:not([disabled]), textarea:not([disabled]), input:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])'

function focusable(root) {
  if (!root) return []
  return [...root.querySelectorAll(FOCUSABLE)].filter(
    (el) => el.offsetWidth > 0 || el.offsetHeight > 0 || el === document.activeElement,
  )
}

// restoreFocus hands focus back to whatever opened the surface. Svelte calls
// destroy when the node leaves the DOM, which is exactly the close moment.
function restoreFocus(previous) {
  if (previous && previous.isConnected && typeof previous.focus === 'function') {
    previous.focus({ preventScroll: true })
  }
}

// trapFocus keeps Tab inside a modal and remembers the opener.
export function trapFocus(node) {
  const previous = document.activeElement
  // A dialog can name the control that should receive focus: the first
  // focusable element is often the close button, which is safe but useless.
  const first = node.querySelector('[data-autofocus]') || focusable(node)[0]
  if (first) first.focus({ preventScroll: true })
  else node.focus({ preventScroll: true })

  function onKeydown(e) {
    if (e.key !== 'Tab') return
    const items = focusable(node)
    if (!items.length) {
      e.preventDefault()
      return
    }
    const head = items[0]
    const tail = items[items.length - 1]
    if (e.shiftKey && document.activeElement === head) {
      e.preventDefault()
      tail.focus()
    } else if (!e.shiftKey && document.activeElement === tail) {
      e.preventDefault()
      head.focus()
    }
  }

  node.addEventListener('keydown', onKeydown)
  return {
    destroy() {
      node.removeEventListener('keydown', onKeydown)
      restoreFocus(previous)
    },
  }
}

// focusInto is the non-modal counterpart: park focus on a heading so a screen
// reader starts reading the surface, keep Tab free, and return focus on close.
export function focusInto(node) {
  const previous = document.activeElement
  requestAnimationFrame(() => {
    if (node && node.isConnected) node.focus({ preventScroll: true })
  })
  return {
    destroy() {
      restoreFocus(previous)
    },
  }
}
