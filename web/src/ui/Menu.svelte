<script>
  // A row menu, not a menu bar. It anchors to one trigger, opens next to it,
  // and gets out of the way on the first sign of the page moving underneath it.
  //
  // The panel is `position: fixed` but stays inside the component tree. That is
  // deliberate: the ledger's rows live inside `.table`, which clips overflow to
  // round its corners, and inside `.table.scroll`, which scrolls sideways on
  // narrow screens. A fixed element escapes both clippings, so the panel does
  // not have to be teleported to the body — which would break Svelte's event
  // handling and add a portal to maintain. The one thing that would break this
  // is an ancestor with `transform`/`filter`/`contain`; the row never has one.
  import { tick } from 'svelte'
  import Icon from './Icon.svelte'
  import { t } from '../strings.js'

  let {
    items = [],
    label = '',
    onselect = () => {},
    disabled = false,
    width = 220,
  } = $props()

  let open = $state(false)
  let triggerEl = $state(null)
  let menuEl = $state(null)
  let pos = $state({ top: 0, left: 0, ready: false })
  // Where the trigger sat when the menu opened. Only a scroll that moves the
  // row out from under the panel may dismiss it; a scroll inside an unrelated
  // container (the live log tail, a side panel) must not take the menu with it.
  let anchorTop = 0
  let anchorLeft = 0

  const enabled = $derived(items.filter((it) => !it.disabled))

  function place() {
    if (!triggerEl || !menuEl) return
    const rect = triggerEl.getBoundingClientRect()
    const height = menuEl.offsetHeight
    const gap = 6
    const below = rect.bottom + gap
    // Open upwards when the panel would run past the viewport. Rows near the
    // bottom of a long pool are the common case, and a clipped menu looks like
    // a broken button.
    const flip = below + height > window.innerHeight - 8 && rect.top - gap - height > 8
    const top = flip ? rect.top - gap - height : below
    const left = Math.max(8, Math.min(rect.right - width, window.innerWidth - width - 8))
    anchorTop = rect.top
    anchorLeft = rect.left
    pos = { top: Math.round(top), left: Math.round(left), ready: true }
  }

  function openMenu() {
    if (disabled) return
    pos = { ...pos, ready: false }
    open = true
  }

  function close(returnFocus = true) {
    if (!open) return
    open = false
    if (returnFocus) triggerEl?.focus({ preventScroll: true })
  }

  function choose(item) {
    if (item.disabled) return
    close()
    onselect(item.id)
  }

  function move(step) {
    if (!enabled.length) return
    const at = enabled.indexOf(items.find((it) => it.id === document.activeElement?.dataset?.item))
    const next = at < 0 ? 0 : (at + step + enabled.length) % enabled.length
    menuEl?.querySelector(`[data-item="${enabled[next].id}"]`)?.focus()
  }

  function onKeydown(e) {
    if (e.key === 'Escape') {
      e.preventDefault()
      e.stopPropagation()
      close()
    } else if (e.key === 'ArrowDown') {
      e.preventDefault()
      move(1)
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      move(-1)
    } else if (e.key === 'Home') {
      e.preventDefault()
      enabled[0] && menuEl?.querySelector(`[data-item="${enabled[0].id}"]`)?.focus()
    } else if (e.key === 'End') {
      e.preventDefault()
      const last = enabled[enabled.length - 1]
      last && menuEl?.querySelector(`[data-item="${last.id}"]`)?.focus()
    } else if (e.key === 'Tab') {
      // Hand focus back to the trigger and let the browser continue from
      // there, so Tab walks out of the row instead of into a removed node.
      close()
    }
  }

  $effect(() => {
    if (!open || !menuEl) return
    place()
    // The panel is still visibility:hidden while it is being measured, and a
    // hidden node silently refuses focus. Waiting one tick lets the placement
    // land first; without it the keyboard stays on the trigger and every
    // arrow key falls through to page scrolling.
    tick().then(() => {
      if (!open || !menuEl) return
      menuEl.querySelector('[role="menuitem"]:not([disabled])')?.focus({ preventScroll: true })
    })
  })

  $effect(() => {
    if (!open) return
    // Close when the row actually moves out from under the panel. Scrolling
    // inside an unrelated container — the live log tail is the standing case —
    // must not take the menu with it.
    const dismiss = () => {
      const r = triggerEl?.getBoundingClientRect()
      if (!r || Math.abs(r.top - anchorTop) > 1 || Math.abs(r.left - anchorLeft) > 1) close(false)
    }
    const outside = (e) => {
      if (menuEl?.contains(e.target) || triggerEl?.contains(e.target)) return
      close(false)
    }
    window.addEventListener('scroll', dismiss, true)
    window.addEventListener('resize', dismiss)
    document.addEventListener('pointerdown', outside, true)
    return () => {
      window.removeEventListener('scroll', dismiss, true)
      window.removeEventListener('resize', dismiss)
      document.removeEventListener('pointerdown', outside, true)
    }
  })
</script>

<button
  class="btn icon menu-trigger"
  bind:this={triggerEl}
  class:on={open}
  aria-haspopup="menu"
  aria-expanded={open}
  aria-label={label || t.common.more}
  title={label || t.common.more}
  {disabled}
  onclick={() => (open ? close() : openMenu())}
  onkeydown={(e) => {
    if (!open) {
      if (e.key === 'ArrowDown' || e.key === 'Enter' || e.key === ' ') {
        e.preventDefault()
        openMenu()
      }
      return
    }
    // The panel owns the keys once it is open — including while focus is still
    // on this button. Left to the browser, an arrow key scrolls the page out
    // from under the menu and the scroll handler dismisses it on the way past.
    onKeydown(e)
  }}
>
  <Icon name="more" size={16} />
</button>

{#if open}
  <div
    class="menu"
    role="menu"
    tabindex="-1"
    aria-label={label || t.common.more}
    bind:this={menuEl}
    style="top:{pos.top}px;left:{pos.left}px;width:{width}px;visibility:{pos.ready ? 'visible' : 'hidden'}"
    onkeydown={onKeydown}
  >
    {#each items as item (item.id)}
      {#if item.divider}<div class="menu-sep" role="separator"></div>{/if}
      <button
        class="menu-item"
        class:danger={item.danger}
        role="menuitem"
        data-item={item.id}
        disabled={item.disabled}
        onclick={() => choose(item)}
      >
        {#if item.icon}<Icon name={item.icon} size={15} />{/if}
        <span>{item.label}</span>
      </button>
    {/each}
  </div>
{/if}

<style>
  /* The visual size is 32px with a 40px hit area, as the spec asks. The button
     keeps its 32px box so the row height does not grow; the padding does the
     rest of the hitting. */
  .menu-trigger {
    width: 32px;
    height: 32px;
    color: var(--text-2);
  }

  .menu-trigger.on {
    background: var(--surface-2);
    color: var(--text);
  }

  @media (pointer: coarse) {
    .menu-trigger {
      width: 40px;
      height: 40px;
    }
  }
</style>
