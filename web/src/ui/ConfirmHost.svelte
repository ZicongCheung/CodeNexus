<script>
  import { confirmState, settle } from './confirm.svelte.js'
  import Icon from './Icon.svelte'
  import { t } from '../strings.js'

  // The dialog owns its own key handling: Escape cancels, Enter confirms. The
  // backdrop is the click target, so a click inside the card cannot dismiss it.
  let dialogEl = $state(null)

  $effect(() => {
    if (confirmState.open && dialogEl) dialogEl.focus()
  })

  function onKeydown(e) {
    if (!confirmState.open) return
    if (e.key === 'Escape') {
      e.preventDefault()
      settle(false)
    } else if (e.key === 'Enter' && !e.isComposing) {
      e.preventDefault()
      settle(true)
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
      bind:this={dialogEl}
      role="alertdialog"
      aria-modal="true"
      aria-labelledby="confirm-title"
      tabindex="-1"
    >
      <div class="head">
        <span class="mark" class:danger={confirmState.danger}>
          <Icon name={confirmState.danger ? 'alert' : 'info'} size={16} />
        </span>
        <h2 id="confirm-title">{confirmState.title}</h2>
      </div>
      {#if confirmState.body}
        <p class="body">{confirmState.body}</p>
      {/if}
      {#if confirmState.detail}
        <p class="detail">{confirmState.detail}</p>
      {/if}
      <div class="actions">
        <button class="btn ghost" onclick={() => settle(false)}>{confirmState.cancelLabel}</button>
        <button class="btn {confirmState.danger ? 'danger' : 'primary'}" onclick={() => settle(true)}>
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
    z-index: var(--z-modal-backdrop);
    display: grid;
    place-items: center;
    padding: var(--space-4);
    background: var(--overlay-bg);
    backdrop-filter: blur(3px);
    -webkit-backdrop-filter: blur(3px);
    animation: fade var(--transition-normal);
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
    z-index: var(--z-modal);
    width: min(26rem, 100%);
    background: var(--card);
    color: var(--card-foreground);
    border: 1px solid var(--border);
    border-radius: var(--card-radius);
    box-shadow: var(--shadow-modal);
    padding: var(--space-5);
    animation: pop var(--transition-ios);
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
    gap: var(--space-2-5);
  }

  .mark {
    display: grid;
    place-items: center;
    width: 28px;
    height: 28px;
    border-radius: 999px;
    flex: none;
    background: var(--primary-light);
    color: var(--primary);
  }

  .mark.danger {
    background: var(--destructive-bg);
    color: var(--destructive);
  }

  h2 {
    font-size: var(--text-lg);
    font-weight: var(--font-semibold);
    margin: 0;
  }

  .body {
    margin: var(--space-3) 0 0;
    font-size: var(--text-caption);
    color: var(--muted-foreground);
    line-height: var(--leading-relaxed);
  }

  .detail {
    margin: var(--space-3) 0 0;
    padding: var(--space-2-5) var(--space-3);
    border-radius: var(--radius);
    background: var(--secondary);
    font-family: var(--font-family-mono);
    font-size: var(--text-xs);
    word-break: break-all;
    max-height: 8rem;
    overflow: auto;
  }

  .actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
    margin-top: var(--space-5);
  }

  .btn {
    font: inherit;
    font-size: var(--text-caption);
    font-weight: var(--font-medium);
    height: var(--button-height);
    padding: 0 var(--space-5);
    border-radius: var(--button-radius);
    border: 1px solid transparent;
    cursor: pointer;
    transition: background var(--transition-fast), border-color var(--transition-fast),
      transform var(--transition-fast), opacity var(--transition-fast);
  }

  .btn:active {
    transform: scale(0.97);
  }

  .btn.ghost {
    background: var(--secondary);
    color: var(--foreground);
    border-color: var(--border);
  }

  .btn.ghost:hover {
    background: var(--surface-active);
  }

  .btn.primary {
    background: var(--primary);
    color: var(--primary-foreground);
  }

  .btn.primary:hover {
    filter: brightness(1.06);
  }

  .btn.danger {
    background: var(--destructive);
    color: #fff;
  }

  .btn.danger:hover {
    filter: brightness(1.06);
  }
</style>
