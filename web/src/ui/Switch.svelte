<script>
  // A switch rather than a checkbox: every boolean here is "this behaviour is
  // on", and a track reads as state at a glance in a row of labels. The
  // checkbox stays underneath so keyboard and screen-reader behaviour is the
  // platform's, not a reimplementation.
  let { checked = false, label = '', onchange, disabled = false } = $props()
</script>

<label class="switch" class:on={checked} class:disabled>
  <input
    type="checkbox"
    {checked}
    {disabled}
    aria-label={label}
    onchange={(e) => onchange?.(e.currentTarget.checked)}
  />
  <span class="track" aria-hidden="true"></span>
</label>

<style>
  .switch {
    display: inline-flex;
    align-items: center;
    flex: none;
    cursor: pointer;
    /* 22px of track inside a 40px hit area, without pushing the row taller. */
    padding-block: 9px;
    margin-block: -9px;
  }

  .switch.disabled {
    cursor: not-allowed;
    opacity: 0.55;
  }

  .switch input {
    position: absolute;
    width: 1px;
    height: 1px;
    margin: 0;
    opacity: 0;
    pointer-events: none;
  }

  .track {
    position: relative;
    display: block;
    width: 40px;
    height: 22px;
    border-radius: 999px;
    background: var(--surface-3);
    transition: background var(--dur-1) var(--ease), box-shadow var(--dur-1) var(--ease);
  }

  .track::after {
    content: '';
    position: absolute;
    top: 2px;
    left: 2px;
    width: 18px;
    height: 18px;
    border-radius: 50%;
    background: var(--surface);
    transition: transform var(--dur-1) var(--ease);
  }

  .switch.on .track {
    background: var(--action);
  }

  .switch.on .track::after {
    transform: translateX(18px);
  }

  .switch input:focus-visible + .track {
    outline: 2px solid var(--action);
    outline-offset: 2px;
  }
</style>
