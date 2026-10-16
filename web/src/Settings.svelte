<script>
  // Settings edits the gateway's config file in place and reports, per field,
  // whether the change took effect immediately or needs a restart. The form
  // covers the fields people actually reach for; the JSON editor underneath it
  // keeps every other field reachable, so nothing is hidden by the form.

  import Icon from './ui/Icon.svelte'
  import { toastOk, toastErr } from './ui/toast.svelte.js'
  import { ask } from './ui/confirm.svelte.js'
  import { t } from './strings.js'

  const API = '/panel/api'

  let schema = $state([])
  let values = $state(null)
  let snapshots = $state([])
  let canRestart = $state(false)
  let loading = $state(true)
  let error = $state('')

  // draft holds only what the user touched, keyed by path.
  let draft = $state({})
  let mode = $state('form') // form | json
  let jsonText = $state('')
  let jsonError = $state('')
  let saving = $state(false)
  let result = $state(null)
  let confirmPaths = $state([])
  let showSecret = $state({})

  async function load() {
    loading = true
    error = ''
    try {
      const res = await fetch(`${API}/config`, { headers: { Accept: 'application/json' } })
      if (!res.ok) throw new Error(await readError(res))
      const body = await res.json()
      schema = body.schema || []
      values = body.config || {}
      snapshots = body.snapshots || []
      canRestart = !!body.canRestart
      draft = {}
      result = null
      confirmPaths = []
      jsonText = JSON.stringify(values, null, 2)
    } catch (err) {
      error = err.message
    } finally {
      loading = false
    }
  }

  async function readError(res) {
    try {
      const body = await res.json()
      return body.error || t.settings.err.request(res.status)
    } catch {
      return t.settings.err.request(res.status)
    }
  }

  function getPath(obj, path) {
    return path.split('.').reduce((o, k) => (o == null ? undefined : o[k]), obj)
  }

  function setPath(obj, path, val) {
    const keys = path.split('.')
    let o = obj
    for (let i = 0; i < keys.length - 1; i++) {
      if (o[keys[i]] == null || typeof o[keys[i]] !== 'object') o[keys[i]] = {}
      o = o[keys[i]]
    }
    o[keys[keys.length - 1]] = val
  }

  // current shows the drafted value when there is one, otherwise what is live.
  function current(field) {
    if (Object.prototype.hasOwnProperty.call(draft, field.path)) return draft[field.path]
    return getPath(values, field.path)
  }

  function set(field, val) {
    draft[field.path] = val
  }

  function isDirty(field) {
    return Object.prototype.hasOwnProperty.call(draft, field.path)
  }

  let dirtyCount = $derived(Object.keys(draft).length)

  function resetDraft() {
    draft = {}
    jsonText = JSON.stringify(values, null, 2)
    jsonError = ''
    result = null
    confirmPaths = []
  }

  // merged produces the whole config to send: the form only ever contributes
  // the fields it knows about, so the rest of the file is carried over intact.
  function merged() {
    const base = JSON.parse(JSON.stringify(values || {}))
    for (const [path, val] of Object.entries(draft)) setPath(base, path, val)
    return base
  }

  function switchToJson() {
    jsonText = JSON.stringify(merged(), null, 2)
    jsonError = ''
    mode = 'json'
  }

  function applyJson() {
    try {
      const parsed = JSON.parse(jsonText)
      values = parsed
      draft = {}
      jsonError = ''
      mode = 'form'
      result = null
    } catch (err) {
      jsonError = err.message
    }
  }

  async function save(confirm = false) {
    saving = true
    result = null
    error = ''
    try {
      const payload = mode === 'json' ? JSON.parse(jsonText) : merged()
      const res = await fetch(`${API}/config`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ config: payload, confirm }),
      })
      const body = await res.json()
      if (res.status === 409) {
        confirmPaths = body.confirm_required || []
        return
      }
      if (!res.ok) throw new Error(body.error || t.settings.err.save(res.status))
      values = payload
      draft = {}
      snapshots = body.snapshots || []
      confirmPaths = []
      result = {
        applied: body.applied || [],
        restart: body.restart_required || [],
      }
      jsonText = JSON.stringify(values, null, 2)
    } catch (err) {
      toastErr(err.message)
      error = err.message
    } finally {
      saving = false
    }
  }

  async function rollback(id) {
    const ok = await ask({
      title: t.settings.snapshot.rollbackTo,
      body: t.settings.snapshot.confirm(id),
      confirmLabel: t.settings.snapshot.rollbackTo,
      danger: true,
    })
    if (!ok) return
    saving = true
    error = ''
    try {
      const res = await fetch(`${API}/config/rollback`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id }),
      })
      const body = await res.json()
      if (!res.ok) throw new Error(body.error || t.settings.err.rollback(res.status))
      await load()
      result = { applied: body.applied || [], restart: body.restart_required || [] }
      toastOk(t.settings.saved)
    } catch (err) {
      toastErr(err.message)
      error = err.message
    } finally {
      saving = false
    }
  }

  async function restart() {
    const ok = await ask({
      title: t.settings.restartNow,
      body: t.settings.restartConfirm,
      confirmLabel: t.settings.restartNow,
      danger: true,
    })
    if (!ok) return
    error = ''
    try {
      const res = await fetch(`${API}/restart`, { method: 'POST' })
      if (!res.ok) {
        const body = await res.json().catch(() => ({}))
        throw new Error(body.error || t.settings.err.restart(res.status))
      }
      // The process is on its way out; give it a moment and come back.
      await new Promise((r) => setTimeout(r, 3000))
      location.reload()
    } catch (err) {
      // A restart closes the connection mid-flight, which surfaces as a
      // network error here even when it worked.
      await new Promise((r) => setTimeout(r, 1500))
      location.reload()
    }
  }

  // Group headings keep their schema order instead of being re-sorted, so the
  // page reads the way the config file is laid out.
  let groups = $derived.by(() => {
    const out = []
    for (const f of schema) {
      let g = out.find((x) => x.name === f.group)
      if (!g) {
        g = { name: f.group, fields: [] }
        out.push(g)
      }
      g.fields.push(f)
    }
    return out
  })

  function toggleHour(field, hour) {
    const list = Array.isArray(current(field)) ? [...current(field)] : []
    const at = list.indexOf(hour)
    if (at >= 0) list.splice(at, 1)
    else list.push(hour)
    list.sort((a, b) => a - b)
    set(field, list)
  }

  function labelFor(path) {
    const f = schema.find((x) => x.path === path)
    return f ? f.label : path
  }

  $effect(() => {
    load()
  })
</script>

<section>
  {#if loading}
    <div class="card" aria-busy="true" aria-label={t.settings.loadingAria}>
      <div class="skeleton-list">
        <div class="skeleton"></div>
        <div class="skeleton short"></div>
        <div class="skeleton"></div>
      </div>
    </div>
  {:else if error && !values}
    <div class="card">
      <h2>{t.settings.title}</h2>
      <p class="msg error">{error}</p>
      <button class="btn ghost" onclick={load}>{t.common.retry}</button>
    </div>
  {:else}
    <div class="toolbar settings-toolbar">
      <div class="segmented">
        <button class:active={mode === 'form'} onclick={() => (mode = 'form')}>{t.settings.tab.form}</button>
        <button class:active={mode === 'json'} onclick={switchToJson}>{t.settings.tab.json}</button>
      </div>
      <span class="spacer"></span>
      <button class="btn ghost" onclick={load} disabled={saving}>
        <Icon name="refresh" size={15} />
        {t.settings.reload}
      </button>
    </div>

    {#if error}
      <p class="msg error">{error}</p>
    {/if}

    {#if result}
      <div class="card result">
        <div class="result-head">
          <span class="mark ok"><Icon name="check" size={15} /></span>
          <p class="ok-line">
            {t.settings.saved}
            {#if result.applied.length}
              <strong>{t.settings.applied(result.applied.length)}</strong>
            {/if}
            {#if result.restart.length}
              <span class="sep">·</span> <strong>{t.settings.restartNeeded(result.restart.length)}</strong>
            {/if}
          </p>
        </div>
        {#if result.restart.length}
          <ul class="muted small restart-list">
            {#each result.restart as p}
              <li>{labelFor(p)}（{p}）</li>
            {/each}
          </ul>
          {#if canRestart}
            <button class="btn primary" onclick={restart}>{t.settings.restartNow}</button>
          {:else}
            <p class="muted small">{t.settings.noAutoRestart}</p>
          {/if}
        {/if}
      </div>
    {/if}

    {#if confirmPaths.length}
      <div class="card warn-card">
        <div class="card-head">
          <div class="titles">
            <h2>{t.settings.confirm.title}</h2>
            <span class="hint">{t.settings.confirm.body}</span>
          </div>
          <span class="badge warn">{confirmPaths.length}</span>
        </div>
        <ul class="small restart-list">
          {#each confirmPaths as p}
            <li>{labelFor(p)}（{p}）</li>
          {/each}
        </ul>
        <div class="btn-row">
          <button class="btn primary" onclick={() => save(true)} disabled={saving}>{t.settings.confirm.save}</button>
          <button class="btn ghost" onclick={() => (confirmPaths = [])}>{t.common.cancel}</button>
        </div>
      </div>
    {/if}

    {#if mode === 'json'}
      <div class="card">
        <div class="card-head">
          <div class="titles">
            <h2>{t.settings.json.title}</h2>
            <span class="hint">{t.settings.json.body}</span>
          </div>
        </div>
        <textarea class="json-editor" spellcheck="false" bind:value={jsonText}></textarea>
        {#if jsonError}
          <p class="msg error">{t.settings.json.parseError(jsonError)}</p>
        {/if}
        <div class="btn-row">
          <button class="btn ghost" onclick={applyJson} disabled={!!jsonError}>{t.settings.json.apply}</button>
          <button class="btn primary" onclick={() => save(false)} disabled={saving || !!jsonError}>
            {saving ? t.settings.saving : t.settings.save}
          </button>
        </div>
      </div>
    {:else}
      {#each groups as group (group.name)}
        <div class="card">
          <h2>{group.name}</h2>
          <div class="fields">
            {#each group.fields as field (field.path)}
              <div class="field" class:dirty={isDirty(field)}>
                <div class="field-head">
                  <label class="field-label" for="cfg-{field.path}">{field.label}</label>
                  <span class="tags">
                    {#if isDirty(field)}<span class="badge primary">{t.settings.field.changed}</span>{/if}
                    {#if !field.hot}<span class="badge warn">{t.settings.field.restart}</span>{/if}
                    {#if field.danger}<span class="badge bad">{t.settings.field.danger}</span>{/if}
                  </span>
                </div>

                {#if field.kind === 'bool'}
                  <input
                    id="cfg-{field.path}"
                    type="checkbox"
                    checked={!!current(field)}
                    onchange={(e) => set(field, e.currentTarget.checked)}
                  />
                {:else if field.kind === 'enum'}
                  <select
                    id="cfg-{field.path}"
                    value={current(field) ?? ''}
                    onchange={(e) => set(field, e.currentTarget.value)}
                  >
                    {#each field.options || [] as opt}
                      <option value={opt}>{opt}</option>
                    {/each}
                  </select>
                {:else if field.kind === 'int'}
                  <input
                    id="cfg-{field.path}"
                    type="number"
                    step="1"
                    value={current(field) ?? 0}
                    oninput={(e) => set(field, Number(e.currentTarget.value))}
                  />
                {:else if field.kind === 'float'}
                  <input
                    id="cfg-{field.path}"
                    type="number"
                    step="0.1"
                    value={current(field) ?? 0}
                    oninput={(e) => set(field, Number(e.currentTarget.value))}
                  />
                {:else if field.kind === 'hours'}
                  <div class="hours" role="group" aria-label={t.settings.hoursAria(field.label)}>
                    {#each Array(24) as _, hour}
                      <button
                        type="button"
                        class="hour"
                        class:on={Array.isArray(current(field)) && current(field).includes(hour)}
                        aria-pressed={Array.isArray(current(field)) && current(field).includes(hour)}
                        onclick={() => toggleHour(field, hour)}>{hour}</button
                      >
                    {/each}
                  </div>
                {:else if field.kind === 'secret'}
                  <div class="secret">
                    <input
                      id="cfg-{field.path}"
                      type={showSecret[field.path] ? 'text' : 'password'}
                      value={current(field) ?? ''}
                      placeholder={field.placeholder || ''}
                      oninput={(e) => set(field, e.currentTarget.value)}
                    />
                    <button
                      class="btn icon"
                      aria-label={showSecret[field.path] ? t.settings.secretHide : t.settings.secretShow}
                      title={showSecret[field.path] ? t.settings.secretHide : t.settings.secretShow}
                      onclick={() =>
                        (showSecret = { ...showSecret, [field.path]: !showSecret[field.path] })}
                    >
                      <Icon name={showSecret[field.path] ? 'eyeOff' : 'eye'} size={16} />
                    </button>
                  </div>
                {:else}
                  <input
                    id="cfg-{field.path}"
                    type="text"
                    value={current(field) ?? ''}
                    placeholder={field.placeholder || ''}
                    oninput={(e) => set(field, e.currentTarget.value)}
                  />
                {/if}

                {#if field.help}<p class="help small">{field.help}</p>{/if}
              </div>
            {/each}
          </div>
        </div>
      {/each}
    {/if}

    <div class="card sticky-actions">
      <div class="btn-row">
        <button class="btn primary" onclick={() => save(false)} disabled={saving || dirtyCount === 0}>
          {saving ? t.settings.saving : dirtyCount ? t.settings.saveCount(dirtyCount) : t.settings.save}
        </button>
        <button class="btn ghost" onclick={resetDraft} disabled={saving || dirtyCount === 0}>
          {t.settings.discard}
        </button>
        {#if snapshots.length}
          <span class="spacer"></span>
          <label class="snap-pick">
            <span class="muted small">{t.settings.snapshot.rollbackTo}</span>
            <select id="snap" onchange={(e) => e.currentTarget.value && rollback(e.currentTarget.value)}>
              <option value="">{t.settings.snapshot.choose}</option>
              {#each snapshots as s}
                <option value={s.id}>{s.time}</option>
              {/each}
            </select>
          </label>
        {/if}
      </div>
      <p class="muted small note-line">{t.settings.snapshot.note}</p>
    </div>
  {/if}
</section>

<style>
  .settings-toolbar {
    margin-bottom: var(--space-4);
  }

  .fields {
    display: grid;
    gap: var(--space-4);
    margin-top: var(--space-4);
  }

  .field {
    display: grid;
    gap: var(--space-1-5);
    padding: var(--space-2-5) var(--space-3);
    margin-bottom: 0;
    border-radius: var(--radius);
    border: 1px solid transparent;
    border-left: 2px solid transparent;
    transition: background var(--transition-fast), border-color var(--transition-fast);
  }

  .field:hover {
    background: var(--surface-hover);
  }

  .field.dirty {
    border-left-color: var(--primary);
    background: var(--primary-subtle);
  }

  .field > input:not([type='checkbox']),
  .field > select,
  .field > .secret {
    max-width: 26rem;
  }

  .field-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }

  .field-head .field-label {
    font-weight: var(--font-semibold);
    font-size: var(--text-caption);
    color: var(--foreground);
    letter-spacing: var(--tracking-normal);
    margin: 0;
  }

  .tags {
    display: inline-flex;
    gap: var(--space-1-5);
  }

  .help {
    color: var(--muted-foreground);
    margin: 0;
    line-height: var(--leading-relaxed);
  }

  .secret {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .json-editor {
    width: 100%;
    min-height: 22rem;
    font-size: var(--text-sm);
    line-height: var(--leading-relaxed);
    margin-bottom: var(--space-3);
  }

  .hours {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
  }

  .hour {
    min-width: 32px;
    height: 28px;
    padding: 0 var(--space-1);
    font: inherit;
    font-size: var(--text-2xs);
    font-variant-numeric: tabular-nums;
    color: var(--muted-foreground);
    background: var(--secondary);
    border: 1px solid transparent;
    border-radius: var(--radius-sm);
    cursor: pointer;
    transition: background var(--transition-fast), color var(--transition-fast),
      transform var(--transition-fast);
  }

  .hour:hover {
    background: var(--surface-active);
  }

  .hour:active {
    transform: scale(0.94);
  }

  .hour.on {
    background: var(--primary);
    color: var(--primary-foreground);
  }

  .sticky-actions {
    position: sticky;
    bottom: var(--space-3);
    z-index: var(--z-sticky);
    box-shadow: var(--shadow-lg);
  }

  .note-line {
    margin: var(--space-3) 0 0;
    line-height: var(--leading-relaxed);
  }

  .snap-pick {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
  }

  .snap-pick select {
    width: auto;
    min-width: 10rem;
    height: 32px;
    font-size: var(--text-xs);
  }

  .result-head {
    display: flex;
    align-items: center;
    gap: var(--space-2-5);
  }

  .result .ok-line {
    margin: 0;
    font-size: var(--text-caption);
  }

  .result .sep {
    color: var(--muted-foreground);
  }

  .mark {
    display: grid;
    place-items: center;
    width: 26px;
    height: 26px;
    flex: none;
    border-radius: 999px;
  }

  .mark.ok {
    background: var(--success-bg);
    color: var(--success);
  }

  .restart-list {
    margin: var(--space-3) 0 0;
    padding-left: var(--space-4);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    line-height: var(--leading-relaxed);
  }

  .warn-card {
    border-left: 3px solid var(--warning);
  }

  .skeleton-list {
    display: grid;
    gap: var(--space-3);
  }

  .skeleton {
    height: 16px;
  }

  .skeleton.short {
    width: 45%;
  }

  @media (max-width: 640px) {
    .sticky-actions {
      position: static;
    }
    .json-editor {
      min-height: 16rem;
    }
  }
</style>
