<script>
  import { toasts, dismissToast } from './toast.svelte.js'
  import Icon from './Icon.svelte'
  import { t } from '../strings.js'
</script>

<!-- Toast host. It sits above every other layer and never takes pointer events
     except on the cards themselves, so it cannot swallow a click. -->
<div class="toast-host" aria-live="polite" aria-atomic="false">
  {#each toasts as toast (toast.id)}
    <div class="toast {toast.kind}" role="status">
      <span class="body">{toast.text}</span>
      <button class="dismiss" onclick={() => dismissToast(toast.id)} aria-label={t.common.close}>
        <Icon name="close" size={15} />
      </button>
    </div>
  {/each}
</div>

<style>
  .toast-host {
    position: fixed;
    right: var(--sp-8);
    bottom: var(--sp-8);
    z-index: var(--z-toast);
    display: flex;
    flex-direction: column;
    align-items: flex-end;
    gap: var(--sp-3);
    pointer-events: none;
    width: min(320px, calc(100vw - 2 * var(--sp-6)));
  }

  /* Solid, not glass: a toast has to be readable over whatever it covers, and a
     blur behind 12px text only reduces contrast. The kind rides on the text
     colour alone — no left rule, no icon tile: the sentence is the toast. */
  .toast {
    pointer-events: auto;
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    width: 100%;
    min-height: 44px;
    padding: var(--sp-4);
    border: 1px solid var(--border);
    border-radius: var(--r-lg);
    background: var(--surface);
    box-shadow: var(--shadow-2);
    font-size: var(--fs-13);
    line-height: var(--lh-13);
    color: var(--text);
    animation: toast-in var(--dur-3) var(--ease-out);
  }

  @keyframes toast-in {
    from {
      opacity: 0;
      transform: translateY(10px) scale(0.98);
    }
    to {
      opacity: 1;
      transform: none;
    }
  }

  /* The sentence is the toast: it takes the state colour and sits centred
     between the card edges and the dismiss that clears it. */
  .body {
    flex: 1;
    min-width: 0;
    text-align: center;
    word-break: break-word;
  }

  .dismiss {
    flex: none;
    display: grid;
    place-items: center;
    width: 22px;
    height: 22px;
    border: none;
    border-radius: var(--r-sm);
    background: none;
    color: var(--text-2);
    cursor: pointer;
    transition: background var(--dur-1) var(--ease), color var(--dur-1) var(--ease);
  }

  /* Touch has no hover: without the guard a tapped toast's close button keeps
     its pressed surface until the toast expires. */
  @media (hover: hover) and (pointer: fine) {
    .dismiss:hover {
      background: var(--surface-2);
      color: var(--text);
    }
  }

  .toast.ok .body {
    color: var(--ok);
  }
  .toast.info .body {
    color: var(--info);
  }
  .toast.warn .body {
    color: var(--warn);
  }
  .toast.error .body {
    color: var(--danger);
  }

  @media (max-width: 640px) {
    .toast-host {
      right: var(--sp-6);
      left: var(--sp-6);
      bottom: var(--sp-6);
      align-items: stretch;
    }
  }
</style>
