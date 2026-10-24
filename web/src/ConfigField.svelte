<script>
  // One setting, one control. The parent owns the value; this component only
  // renders it and reports the next one, so a draft never has two sources of
  // truth.
  //
  // Numeric inputs deliberately emit an empty string when cleared: showing "0"
  // the moment the box empties makes the field impossible to retype. The parent
  // coerces it back to a number on save.

  import Icon from './ui/Icon.svelte'
  import Switch from './ui/Switch.svelte'
  import { t } from './strings.js'

  let { field, value, dirty = false, onchange, highlight = '', section = '', inline = false } =
    $props()

  let reveal = $state(false)

  const id = $derived(`cfg-${field.path.replaceAll('.', '-')}`)

  // The search box filters the page; marking the matched run inside the label
  // is what turns "why is this here" into "there it is".
  const escapeHtml = (s) =>
    String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c])

  function labelHtml() {
    const text = String(field.label)
    const q = highlight.trim().toLowerCase()
    if (!q) return escapeHtml(text)
    const at = text.toLowerCase().indexOf(q)
    if (at < 0) return escapeHtml(text)
    return (
      escapeHtml(text.slice(0, at)) +
      '<mark>' +
      escapeHtml(text.slice(at, at + q.length)) +
      '</mark>' +
      escapeHtml(text.slice(at + q.length))
    )
  }

  function listText(v) {
    if (Array.isArray(v)) return v.join(', ')
    return v == null ? '' : String(v)
  }

  function parseList(raw) {
    return raw
      .split(/[\n,]/)
      .map((s) => s.trim())
      .filter(Boolean)
  }

  function onNumber(e) {
    const raw = e.currentTarget.value
    onchange(raw === '' ? '' : Number(raw))
  }

  function toggleHour(hour) {
    const list = Array.isArray(value) ? [...value] : []
    const at = list.indexOf(hour)
    if (at >= 0) list.splice(at, 1)
    else list.push(hour)
    list.sort((a, b) => a - b)
    onchange(list)
  }

  // Roving tabindex across the 24 hour cells: arrows walk, Home/End jump to
  // the ends, and the cell that just took focus owns the single tab stop.
  let hourCursor = $state(0)
  let hourEls = []

  function onHourKey(e, hour) {
    const jump = { ArrowLeft: hour - 1, ArrowRight: hour + 1, Home: 0, End: 23 }
    if (!(e.key in jump)) return
    e.preventDefault()
    const next = Math.min(23, Math.max(0, jump[e.key]))
    hourCursor = next
    hourEls[next]?.focus()
  }

  // The design puts a one-line hint at the end of the label row. That works
  // for the 89 fields whose help is a phrase; the three that run to a
  // paragraph (1M 档位 is 258 characters) would be cut to a sentence fragment,
  // so those keep the paragraph under the control instead.
  const HINT_MAX = 60
  let hint = $derived((field.help || '').length <= HINT_MAX ? field.help : '')
  let longHelp = $derived((field.help || '').length > HINT_MAX ? field.help : '')
</script>

<div class="field" class:dirty class:danger={!!field.danger}>
  <div class="field-head">
    <label class="field-label" for={inline ? undefined : id}>{@html labelHtml()}</label>
    {#if !field.hot}<span class="chip restart">{t.settings.field.restart}</span>{/if}
    {#if field.danger}<span class="flag danger">{t.settings.field.danger}</span>{/if}
    {#if section}<span class="chip sec">{section}</span>{/if}
    <span class="spacer"></span>
    {#if hint}<span class="field-hint" title={hint}>{hint}</span>{/if}
    {#if inline}
      <Switch checked={!!value} label={field.label} onchange={(v) => onchange(v)} />
    {/if}
  </div>

  {#if field.kind === 'bool'}
    {#if !inline}
      <Switch checked={!!value} label={field.label} onchange={(v) => onchange(v)} />
    {/if}
  {:else if field.kind === 'enum'}
    <select id={id} value={value ?? ''} onchange={(e) => onchange(e.currentTarget.value)}>
      {#each field.options || [] as opt}
        <option value={opt}>{opt}</option>
      {/each}
    </select>
  {:else if field.kind === 'int' || field.kind === 'float'}
    <input
      id={id}
      type="number"
      step={field.kind === 'int' ? '1' : '0.01'}
      value={value === '' || value == null ? '' : value}
      oninput={onNumber}
    />
  {:else if field.kind === 'hours'}
    <!-- One Tab stop for twenty-four toggles (§7.2): arrows walk the hours,
         Space/Enter flips the focused one — twenty-four tab presses to leave
         the grid would trap anyone crossing it. -->
    <div class="hours" role="group" aria-label={t.settings.hoursAria(field.label)}>
      {#each Array(24) as _, hour}
        <button
          type="button"
          class="hour"
          class:on={Array.isArray(value) && value.includes(hour)}
          aria-pressed={Array.isArray(value) && value.includes(hour)}
          tabindex={hour === hourCursor ? 0 : -1}
          bind:this={hourEls[hour]}
          onfocus={() => (hourCursor = hour)}
          onkeydown={(e) => onHourKey(e, hour)}
          onclick={() => toggleHour(hour)}>{hour}</button
        >
      {/each}
    </div>
  {:else if field.kind === 'secret'}
    <div class="secret">
      <input
        id={id}
        type={reveal ? 'text' : 'password'}
        value={value ?? ''}
        placeholder={field.placeholder || ''}
        oninput={(e) => onchange(e.currentTarget.value)}
      />
      <button
        class="btn icon"
        aria-label={reveal ? t.settings.secretHide : t.settings.secretShow}
        title={reveal ? t.settings.secretHide : t.settings.secretShow}
        onclick={() => (reveal = !reveal)}
      >
        <Icon name={reveal ? 'eyeOff' : 'eye'} size={16} />
      </button>
    </div>
  {:else if field.kind === 'list'}
    <textarea
      id={id}
      class="list-input"
      rows="2"
      value={listText(value)}
      placeholder={t.settings.listHint}
      onchange={(e) => onchange(parseList(e.currentTarget.value))}
    ></textarea>
  {:else}
    <input
      id={id}
      type="text"
      value={value ?? ''}
      placeholder={field.placeholder || ''}
      oninput={(e) => onchange(e.currentTarget.value)}
    />
  {/if}

  {#if longHelp}<p class="help small">{longHelp}</p>{/if}
</div>

<style>
  /* A field is a label row and a control, 14px apart from its neighbours. It
     draws no box of its own: the panel is the surface, and ninety-five tinted
     cells would turn one sheet into a grid of tiles. */
  .field {
    display: flex;
    flex-direction: column;
    gap: 6px;
    min-width: 0;
    /* The global .field carries a 12px bottom margin from the dialog forms it
       was written for. Here the list owns the rhythm, so a field has none. */
    margin: 0;
  }

  .field-head {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    min-height: 17px;
  }

  /* The label is quiet and the value is loud: in a form this dense the eye
     should land on what the setting is set to, not on ninety headings. */
  .field-head .field-label {
    margin: 0;
    font-weight: var(--fw-medium);
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-2);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .field-label :global(mark) {
    padding: 0 1px;
    color: var(--text);
    background: var(--action-soft);
    border-radius: 2px;
  }

  /* A phrase, right-aligned on the label line: it explains the value you are
     about to type without costing a row. */
  .field-hint {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: var(--fs-10);
    line-height: var(--lh-10);
    color: var(--text-3);
  }

  .chip {
    flex: none;
    padding: 0 6px;
    font-size: var(--fs-10);
    line-height: 16px;
    border-radius: 999px;
    white-space: nowrap;
  }

  /* Restart is a property of the field, not an alarm: neutral ground, amber
     text, the size of a word. */
  .chip.restart {
    height: 17px;
    line-height: 17px;
    padding: 0 7px;
    color: var(--warn);
    background: var(--surface-2);
  }

  /* A field that can lock the operator out is a word, not a second pill: the
     restart chip already owns the pill shape, and four rows of paired badges
     read as a tag cloud instead of a warning. */
  .flag {
    flex: none;
    font-size: var(--fs-10);
    line-height: 16px;
    white-space: nowrap;
  }

  .flag.danger {
    color: var(--danger);
  }

  /* Present only in search results, where the field's own section is the one
     thing the surrounding list cannot tell you. */
  .chip.sec {
    color: var(--text-3);
    background: var(--surface-2);
  }

  .field .help {
    margin: 0;
    max-width: 52rem;
    color: var(--text-3);
    font-size: var(--fs-10);
    line-height: var(--lh-11);
  }

  /* Changed is an outline on the control and a tint on the label: it marks the
     one row you edited without adding a badge, a bar or a height to the form.
     The label is what carries it for a switch, whose track has no border to
     tint and is already the accent colour when it is on. */
  .field.dirty .field-label {
    color: var(--action-text);
  }

  .field.dirty :global(input:not([type='checkbox'])),
  .field.dirty :global(select),
  .field.dirty :global(textarea) {
    border-color: var(--action);
  }

  .secret {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
  }

  .secret input {
    flex: 1;
    min-width: 0;
  }

  .list-input {
    min-height: 3.4rem;
    line-height: var(--leading-snug);
    resize: vertical;
  }

  .hours {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-1);
  }

  .hour {
    min-width: 30px;
    height: 26px;
    padding: 0 var(--sp-1);
    font: inherit;
    font-size: var(--fs-11);
    font-variant-numeric: tabular-nums;
    color: var(--text-2);
    background: var(--surface-2);
    border: 1px solid transparent;
    border-radius: var(--r-sm);
    cursor: pointer;
    transition: background var(--dur-1) var(--ease), color var(--dur-1) var(--ease),
      transform var(--dur-1) var(--ease);
  }

  @media (hover: hover) and (pointer: fine) {
    .hour:hover {
      background: var(--surface-3);
    }
  }

  .hour:active {
    transform: scale(0.94);
  }

  .hour.on {
    background: var(--action);
    color: var(--action-fg);
  }
</style>
