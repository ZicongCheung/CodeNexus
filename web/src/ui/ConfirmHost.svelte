<script>
  import { confirmState, settle } from './confirm.svelte.js'
  import Icon from './Icon.svelte'
  import { t } from '../strings.js'
  import { trapFocus } from './focus.js'

  // Escape cancels; Enter is left to the platform. A button already confirms on
  // Enter when it has focus, and a global Enter handler would also confirm while
  // focus sat on "cancel" — which is the one place it must never do that.
  let cancelEl = $state(null)
  let confirmEl = $state(null)

  // Destructive dialogs open on the safe choice; everything else opens on the
  // action, so Enter still completes a routine confirmation in one keystroke.
  $effect(() => {
    if (!confirmState.open) return
    const target = confirmState.tone === 'danger' ? cancelEl : confirmEl
    if (target) target.focus({ preventScroll: true })
  })

  function onKeydown(e) {
    if (!confirmState.open) return
    if (e.key === 'Escape') {
      e.preventDefault()
      settle(false)
    }
  }

  function onBackdrop(e) {
    if (e.target === e.currentTarget) settle(false)
  }
</script>

<svelte:window onkeydown={onKeydown} />

{#if confirmState.open}
  <div class="backdrop" role="presentation" onclick={onBackdrop}>
    <div
      class="dialog"
      use:trapFocus
      role="alertdialog"
      aria-modal="true"
      aria-labelledby="confirm-title"
      tabindex="-1"
    >
      <div class="head">
        <span class="mark" data-tone={confirmState.tone}>
          <Icon name={confirmState.tone === 'info' ? 'info' : 'alert'} size={16} />
        </span>
        <h2 id="confirm-title">{confirmState.title}</h2>
      </div>
      {#if confirmState.body}
        <p class="body">{confirmState.body}</p>
      {/if}
      {#if confirmState.detail}
        <p class="detail">{confirmState.detail}</p>
      {/if}
      <div class="rule" aria-hidden="true"></div>
      <div class="actions">
        <button class="btn ghost" bind:this={cancelEl} onclick={() => settle(false)}>
          {confirmState.cancelLabel}
        </button>
        <button
          class="btn {confirmState.tone === 'danger' ? 'danger-solid' : 'primary'}"
          bind:this={confirmEl}
          onclick={() => settle(true)}
        >
          {confirmState.confirmLabel || t.common.confirm}
        </button>
      </div>
    </div>
  </div>
{/if}

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: var(--z-backdrop);
    display: grid;
    place-items: center;
    padding: var(--sp-6);
    background: var(--backdrop);
    animation: fade var(--dur-3) var(--ease);
  }

  @keyframes fade {
    from {
      opacity: 0;
    }
    to {
      opacity: 1;
    }
  }

  .dialog {
    z-index: var(--z-dialog);
    width: min(420px, 100%);
    background: var(--surface);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: var(--r-lg);
    box-shadow: var(--shadow-dialog);
    padding: var(--sp-7);
    animation: pop var(--dur-3) var(--ease-out);
  }

  @keyframes pop {
    from {
      opacity: 0;
      transform: translateY(8px) scale(0.97);
    }
    to {
      opacity: 1;
      transform: none;
    }
  }

  .head {
    display: flex;
    align-items: center;
    gap: var(--sp-4);
  }

  /* The mark is a 24px tile at the small radius: it identifies the tone of the
     dialog, it is not a badge. Three tones, three soft fills — the confirm
     button below carries the severity. */
  .mark {
    display: grid;
    place-items: center;
    width: 24px;
    height: 24px;
    border-radius: var(--r-sm);
    flex: none;
    background: var(--action-soft);
    color: var(--action);
  }

  .mark[data-tone='warn'] {
    background: var(--warn-soft);
    color: var(--warn);
  }

  .mark[data-tone='danger'] {
    background: var(--danger-soft);
    color: var(--danger);
  }

  h2 {
    font-size: var(--fs-16);
    font-weight: var(--fw-semibold);
    margin: 0;
  }

  .body {
    margin: var(--sp-5) 0 0;
    font-size: var(--fs-13);
    color: var(--text-2);
    line-height: var(--leading-relaxed);
  }

  .detail {
    margin: var(--sp-5) 0 0;
    padding: var(--sp-4) var(--sp-5);
    border-radius: var(--r-md);
    background: var(--surface-2);
    font-family: var(--font-mono);
    font-size: var(--fs-11);
    word-break: break-all;
    max-height: 8rem;
    overflow: auto;
  }

  /* A hairline, not a filled band: the actions belong to the dialog, and a
     second surface here would read as a footer bar. */
  .rule {
    margin-top: var(--sp-7);
    border-top: 1px solid var(--border);
  }

  .actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--sp-3);
    margin-top: var(--sp-5);
  }

  /* Colour, height, radius, focus and disabled all come from the global .btn.
     Only the width changes here: a dialog action is the only thing on screen,
     so it can afford the wider padding a toolbar button cannot. */
  .actions .btn {
    padding: 0 var(--sp-7);
  }
</style>
