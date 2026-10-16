<script>
  import { toasts, dismissToast } from './toast.svelte.js'
  import Icon from './Icon.svelte'
  import { t } from '../strings.js'

  const icons = { info: 'info', ok: 'check', warn: 'alert', error: 'alert' }
</script>

<!-- Toast host. It sits above every other layer and never takes pointer events
     except on the cards themselves, so it cannot swallow a click. -->
<div class="toast-host" aria-live="polite" aria-atomic="false">
  {#each toasts as toast (toast.id)}
    <div class="toast {toast.kind}" role="status">
      <span class="glyph"><Icon name={icons[toast.kind] ?? 'info'} size={16} /></span>
      <span class="body">{toast.text}</span>
      <button class="dismiss" onclick={() => dismissToast(toast.id)} aria-label={t.common.close}>
        <Icon name="close" size={14} />
      </button>
    </div>
  {/each}
</div>

<style>
  .toast-host {
    position: fixed;
    right: var(--space-5);
    bottom: var(--space-5);
    z-index: var(--z-toast);
    display: flex;
    flex-direction: column;
    align-items: flex-end;
    gap: var(--space-2);
    pointer-events: none;
    max-width: min(26rem, calc(100vw - 2 * var(--space-4)));
  }

  .toast {
    pointer-events: auto;
    display: flex;
    align-items: flex-start;
    gap: var(--space-2-5);
    padding: var(--space-3) var(--space-3) var(--space-3) var(--space-3-5);
    border-radius: var(--radius);
    border: 1px solid var(--border);
    background: var(--glass-bg);
    backdrop-filter: var(--nav-blur);
    -webkit-backdrop-filter: var(--nav-blur);
    box-shadow: var(--shadow-lg);
    font-size: var(--text-caption);
    line-height: var(--leading-normal);
    color: var(--foreground);
    animation: toast-in var(--transition-ios);
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

  .glyph {
    display: grid;
    place-items: center;
    width: 22px;
    height: 22px;
    border-radius: 999px;
    flex: none;
    margin-top: 1px;
  }

  .body {
    flex: 1;
    min-width: 0;
    word-break: break-word;
  }

  .dismiss {
    flex: none;
    display: grid;
    place-items: center;
    width: 22px;
    height: 22px;
    border: none;
    border-radius: 999px;
    background: none;
    color: var(--muted-foreground);
    cursor: pointer;
    transition: background var(--transition-fast), color var(--transition-fast);
  }

  .dismiss:hover {
    background: var(--surface-hover);
    color: var(--foreground);
  }

  .toast.ok .glyph {
    background: var(--success-bg);
    color: var(--success);
  }
  .toast.info .glyph {
    background: var(--info-bg);
    color: var(--info);
  }
  .toast.warn .glyph {
    background: var(--warning-bg);
    color: var(--warning);
  }
  .toast.error {
    border-color: color-mix(in srgb, var(--destructive) 40%, var(--border));
  }
  .toast.error .glyph {
    background: var(--destructive-bg);
    color: var(--destructive);
  }

  @media (max-width: 640px) {
    .toast-host {
      right: var(--space-4);
      left: var(--space-4);
      bottom: var(--space-4);
      align-items: stretch;
    }
  }
</style>
