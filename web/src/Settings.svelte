<script>
  // Settings is a form over the config file, organised the way an operator
  // navigates rather than the way the file is nested: seven sections, the
  // schedule by task, and the two upstreams in separate blocks.
  //
  // There is no raw JSON editor. That makes this catalog the complete set of
  // settings the console can reach, so the search box is not a convenience —
  // it is how a field gets found in a page with ninety-five of them.

  import { onMount } from 'svelte'
  import Icon from './ui/Icon.svelte'
  import Switch from './ui/Switch.svelte'
  import ConfigField from './ConfigField.svelte'
  import { toastOk, toastErr } from './ui/toast.svelte.js'
  import { setPageMeta } from './ui/pagemeta.svelte.js'
  import { ask } from './ui/confirm.svelte.js'
  import { t } from './strings.js'

  const API = '/panel/api'

  // One section is on screen at a time. The nav is the only thing that has to
  // survive a 95-field catalogue; the panel can then be as long as that one
  // section is.
  const DEFAULT_SECTION = '服务'
  const SECTION_KEY = 'codenexus-settings-section'

  let schema = $state([])
  let values = $state(null)
  let snapshots = $state([])
  let canRestart = $state(false)
  let loading = $state(true)
  let error = $state('')

  // draft holds only what the user touched, keyed by path.
  let draft = $state({})
  let saving = $state(false)
  let result = $state(null)
  let confirmPaths = $state([])

  let query = $state('')
  // Which section fills the panel. While searching this doubles as the facet:
  // a nav click narrows the flat result list to that section instead of
  // navigating away from it.
  let active = $state(readSection())
  let facetActive = $state(false)
  let advOpen = $state({})

  function readSection() {
    try {
      return localStorage.getItem(SECTION_KEY) || DEFAULT_SECTION
    } catch {
      return DEFAULT_SECTION
    }
  }

  function pickSection(name) {
    active = name
    // A nav click while searching is a narrowing gesture, not a jump: the
    // result list stays, it just loses the other sections.
    facetActive = true
    try {
      localStorage.setItem(SECTION_KEY, name)
    } catch {}
  }

  function clearFacet() {
    facetActive = false
  }

  // The page owns its own catalogue request: nothing above it fetches the
  // schema, so without this the form sits on its skeleton forever.
  onMount(load)

  // Its 56px action bar occupies the strip where toasts normally land. The
  // flag is page-owned so the toast host never needs to know the page.
  $effect(() => {
    document.body.dataset.formPage = '1'
    return () => {
      delete document.body.dataset.formPage
    }
  })

  const norm = (s) => String(s || '').toLowerCase()

  let q = $derived(norm(query.trim()))
  let searching = $derived(q.length > 0)

  let fieldByPath = $derived(new Map(schema.map((f) => [f.path, f])))

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
      // The remembered section may not exist in this build's catalogue; fall
      // back to the default rather than rendering an empty panel.
      if (!schema.some((f) => f.group === active)) active = DEFAULT_SECTION
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

  // A numeric box that was cleared arrives as ""; the wire wants a number, and
  // 0 is what "off / no limit" means for every numeric setting here.
  function coerce(field, raw) {
    if (field.kind === 'int') return raw === '' || raw == null ? 0 : Math.trunc(Number(raw)) || 0
    if (field.kind === 'float') return raw === '' || raw == null ? 0 : Number(raw) || 0
    if (field.kind === 'list') {
      if (Array.isArray(raw)) return raw
      return String(raw || '')
        .split(/[\n,]/)
        .map((s) => s.trim())
        .filter(Boolean)
    }
    return raw
  }

  // merged produces the whole config to send: the form only ever contributes
  // the fields it knows about, so the rest of the file is carried over intact.
  function merged() {
    const base = JSON.parse(JSON.stringify(values || {}))
    for (const [path, val] of Object.entries(draft)) {
      const f = fieldByPath.get(path)
      setPath(base, path, f ? coerce(f, val) : val)
    }
    return base
  }

  function resetDraft() {
    draft = {}
    result = null
    confirmPaths = []
  }

  async function save(confirm = false) {
    saving = true
    result = null
    error = ''
    try {
      const payload = merged()
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
        applied: (body.applied || []).length,
        restart: (body.restart_required || []).length,
      }
      toastOk(result.restart ? t.settings.savedRestart(result.applied, result.restart) : t.settings.saved)
    } catch (err) {
      toastErr(err.message)
      error = err.message
    } finally {
      saving = false
    }
  }

  // undo replaces the snapshot list with a single step back: the list was a
  // stack of timestamps nobody can tell apart.
  async function undo() {
    const latest = snapshots[0]
    if (!latest) {
      toastErr(t.settings.undo.none)
      return
    }
    const ok = await ask({
      title: t.settings.undo.title,
      body: t.settings.undo.confirm(prettySnapshot(latest.id)),
      confirmLabel: t.settings.undo.label,
      tone: 'danger',
    })
    if (!ok) return
    saving = true
    error = ''
    try {
      const res = await fetch(`${API}/config/rollback`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id: latest.id }),
      })
      const body = await res.json()
      if (!res.ok) throw new Error(body.error || t.settings.err.rollback(res.status))
      await load()
      result = {
        applied: (body.applied || []).length,
        restart: (body.restart_required || []).length,
      }
      toastOk(t.settings.saved)
    } catch (err) {
      toastErr(err.message)
      error = err.message
    } finally {
      saving = false
    }
  }

  function prettySnapshot(id) {
    const m = /^(\d{4})(\d{2})(\d{2})-(\d{2})(\d{2})(\d{2})$/.exec(id || '')
    if (!m) return id
    return `${m[1]}-${m[2]}-${m[3]} ${m[4]}:${m[5]}:${m[6]}`
  }

  async function restart() {
    const ok = await ask({
      title: t.settings.restartNow,
      body: t.settings.restartConfirm,
      confirmLabel: t.settings.restartNow,
      tone: 'warn',
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

  function toggleAdvanced(key) {
    advOpen = { ...advOpen, [key]: !advOpen[key] }
  }

  // A field matches on what it is called, where it lives, what it does, or the
  // group that owns it — typing 上游 should find the section even though no
  // single field carries the word in its label.
  function hit(f) {
    if (!searching) return true
    return (
      norm(f.label).includes(q) ||
      norm(f.path).includes(q) ||
      norm(f.help).includes(q) ||
      norm(f.group).includes(q) ||
      norm(f.task).includes(q) ||
      norm(f.block).includes(q)
    )
  }

  // model is the whole catalogue: sections in catalog order, each holding
  // either task groups (定时任务) or block groups (everything else), with the
  // master switch and the advanced tail already separated out. It is never
  // filtered — the nav needs a stable list to count hits against.
  let model = $derived.by(() => {
    const out = []
    const byName = new Map()
    for (const f of schema) {
      let sec = byName.get(f.group)
      if (!sec) {
        sec = { name: f.group, groups: [], dirty: 0, total: 0 }
        byName.set(f.group, sec)
        out.push(sec)
      }
      const kind = f.task ? 'task' : 'block'
      const name = f.task || f.block || ''
      let g = sec.groups.find((x) => x.kind === kind && x.name === name)
      if (!g) {
        g = { kind, name, fields: [], platform: f.platform || '' }
        sec.groups.push(g)
      }
      g.fields.push(f)
    }
    for (const sec of out) {
      for (const g of sec.groups) {
        g.primary = g.fields.find((f) => f.primary) || null
        const rest = g.fields.filter((f) => f !== g.primary)
        g.normal = rest.filter((f) => !f.advanced)
        g.adv = rest.filter((f) => f.advanced)
        g.dirty = g.fields.filter(isDirty).length
        g.key = `${sec.name}::${g.kind}:${g.name}`
        sec.dirty += g.dirty
        sec.total += g.fields.length
      }
    }
    return out
  })

  // Hits per section — the nav shows these instead of totals while searching,
  // which is what turns the nav into a facet.
  let sectionHits = $derived.by(() => {
    const m = new Map()
    if (!searching) return m
    for (const f of schema) {
      if (hit(f)) m.set(f.group, (m.get(f.group) || 0) + 1)
    }
    return m
  })

  let totalHits = $derived([...sectionHits.values()].reduce((a, b) => a + b, 0))

  // The flat result list. Advanced fields are included: a search that hides
  // the thing you searched for because it lives behind a disclosure is a
  // search that failed.
  let flatHits = $derived.by(() => {
    if (!searching) return []
    const list = schema.filter(hit)
    return facetActive ? list.filter((f) => f.group === active) : list
  })

  let activeSection = $derived(model.find((s) => s.name === active) || model[0] || null)

  // The top bar states the catalogue's scope. "7 个分区" is the one number that
  // tells you how big this page is before you scroll any of it.
  $effect(() => {
    setPageMeta(model.length ? t.settings.sectionCount(model.length) : '')
    return () => setPageMeta('')
  })

  // A boolean is one row, not two: its switch belongs at the end of the label
  // line, aligned with every other control's right edge. Every other kind
  // stacks its control under the label, because a text box needs the width.
  function inlineControl(f) {
    return f.kind === 'bool'
  }
</script>

<section>
  {#if loading}
    <!-- The outline the page settles into: a 200px nav card and a panel whose
         header keeps its 66px and whose body keeps the 14px field rhythm. -->
    <div class="cfg-skeleton" aria-busy="true" aria-label={t.settings.loadingAria}>
      <div class="cfg-nav-skeleton">
        <div class="skel skel-search"></div>
        {#each Array(7) as _}
          <div class="skel skel-nav-item"></div>
        {/each}
      </div>
      <div class="cfg-panel-skeleton">
        <div class="skel-panel-head">
          <div class="skel skel-title"></div>
          <div class="skel skel-desc"></div>
        </div>
        {#each Array(5) as _}
          <div class="skel skel-field"></div>
        {/each}
      </div>
    </div>
  {:else if error && !values}
    <div class="card">
      <p class="msg error">{error}</p>
      <button class="btn ghost" onclick={load}>{t.common.retry}</button>
    </div>
  {:else}
    {#if error}
      <p class="msg error">{error}</p>
    {/if}

    <div class="cfg">
      <!-- The nav is the page: seven sections is a list you can hold in your
           head, while ninety-five fields is not. It also carries the two things
           an accordion cannot show without scrolling — how many hits each
           section holds, and which sections are dirty. -->
      <nav class="cfg-nav" aria-label={t.settings.section.aria}>
        <div class="nav-search">
          <Icon name="search" size={12} />
          <input
            type="search"
            bind:value={query}
            placeholder={t.settings.search.placeholder}
            aria-label={t.settings.search.aria}
          />
          {#if searching}
            <button
              class="nav-clear"
              aria-label={t.settings.search.clear}
              title={t.settings.search.clear}
              onclick={() => {
                query = ''
                facetActive = false
              }}
            >
              <Icon name="close" size={12} />
            </button>
          {/if}
        </div>

        {#each model as sec (sec.name)}
          {@const n = searching ? sectionHits.get(sec.name) || 0 : sec.total}
          <button
            type="button"
            class="nav-item"
            class:on={active === sec.name && (!searching || facetActive)}
            class:empty={searching && n === 0}
            aria-current={!searching && active === sec.name ? 'true' : undefined}
            title={t.settings.search.facetHint}
            onclick={() => pickSection(sec.name)}
          >
            <span class="nav-dot" class:on={sec.dirty > 0} aria-hidden="true"></span>
            <span class="nav-name">{sec.name}</span>
            <span class="nav-n">{n}</span>
          </button>
        {/each}
      </nav>

      <!-- Narrow screens get the same list as a select: a 200px rail beside a
           one-column form leaves the form nothing to work with. -->
      <label class="cfg-nav-select">
        <span class="sr-only">{t.settings.section.aria}</span>
        <select value={active} onchange={(e) => pickSection(e.currentTarget.value)}>
          {#each model as sec (sec.name)}
            <option value={sec.name}>{sec.name}</option>
          {/each}
        </select>
      </label>

      <div class="cfg-panel">
        <header class="panel-head">
          <div class="panel-titles">
            <h2>{searching ? t.settings.search.resultTitle : activeSection?.name ?? ''}</h2>
            {#if searching}
              {#if facetActive}
                <button class="facet" onclick={clearFacet} title={t.settings.search.facetClear}>
                  {t.settings.search.inSection(active)}
                  <Icon name="close" size={10} />
                </button>
              {:else}
                <p class="panel-desc">{t.settings.search.hits(totalHits)}</p>
              {/if}
            {:else}
              <p class="panel-desc">{t.settings.section.desc[activeSection?.name] ?? ''}</p>
            {/if}
          </div>
        </header>

        <div class="cfg-body">
          {#if searching}
            {#if flatHits.length === 0}
              <p class="muted small">{t.settings.search.none}</p>
            {:else}
              <div class="fields">
                {#each flatHits as field (field.path)}
                  <ConfigField
                    {field}
                    value={current(field)}
                    dirty={isDirty(field)}
                    highlight={q}
                    section={field.task || field.block || field.group}
                    inline={inlineControl(field)}
                    onchange={(v) => set(field, v)}
                  />
                {/each}
              </div>
            {/if}
          {:else if activeSection}
            {#each activeSection.groups as g (g.key)}
              <div class="group" class:task={g.kind === 'task'} class:headed={!!(g.name || g.primary)}>
                {#if g.name || g.primary}
                  <div class="grp-head">
                    {#if g.name}<span class="grp-name">{g.name}</span>{/if}
                    {#if g.kind === 'task'}
                      {#if g.platform === 'both'}
                        <span class="src-chip">
                          <span class="src-dot" data-source="workbuddy"></span>{t.settings.platform.workbuddy}
                        </span>
                        <span class="src-chip">
                          <span class="src-dot" data-source="trae-cn"></span>{t.settings.platform.trae}
                        </span>
                      {:else if g.platform}
                        <span class="src-chip">
                          <span class="src-dot" data-source={g.platform}></span>
                          {g.platform === 'workbuddy' ? t.settings.platform.workbuddy : t.settings.platform.trae}
                        </span>
                      {/if}
                    {/if}
                    <span class="spacer"></span>
                    {#if g.primary}
                      <Switch
                        checked={!!current(g.primary)}
                        label={g.primary.label}
                        onchange={(v) => set(g.primary, v)}
                      />
                    {/if}
                  </div>
                {/if}

                <div class="fields">
                  {#each g.normal as field (field.path)}
                    <ConfigField
                      {field}
                      value={current(field)}
                      dirty={isDirty(field)}
                      highlight={q}
                      inline={inlineControl(field)}
                      onchange={(v) => set(field, v)}
                    />
                  {/each}
                </div>

                {#if g.adv.length && advOpen[g.key]}
                  <div class="fields adv">
                    {#each g.adv as field (field.path)}
                      <ConfigField
                        {field}
                        value={current(field)}
                        dirty={isDirty(field)}
                        highlight={q}
                        inline={inlineControl(field)}
                        onchange={(v) => set(field, v)}
                      />
                    {/each}
                  </div>
                {/if}

                {#if g.adv.length}
                  <button type="button" class="adv-toggle" onclick={() => toggleAdvanced(g.key)}>
                    <Icon name="chevron" size={13} class={advOpen[g.key] ? 'chev open' : 'chev'} />
                    {advOpen[g.key]
                      ? t.settings.group.advancedHide
                      : t.settings.group.advanced(g.adv.length)}
                  </button>
                {/if}
              </div>
            {/each}
          {/if}
        </div>

        <!-- A field that can lock the operator out is confirmed next to the
             button that commits it, not in a banner a scroll above it. -->
        {#if confirmPaths.length}
          <div class="gate">
            <div class="gate-head">
              <span class="gate-mark"><Icon name="alert" size={14} /></span>
              <div class="gate-text">
                <strong>{t.settings.confirm.title}</strong>
                <p>{t.settings.confirm.body}</p>
              </div>
            </div>
            <ul class="gate-list">
              {#each confirmPaths as p}
                <li>{fieldByPath.get(p)?.label ?? p}<span class="gate-path">{p}</span></li>
              {/each}
            </ul>
          </div>
        {/if}

        <!-- Saving a cold field is the one thing the form cannot do for you;
             say it here, on the row that also holds the restart button. -->
        {#if result?.restart && !canRestart}
          <p class="panel-note">{t.settings.noAutoRestart}</p>
        {/if}

        <!-- The bar is the panel's floor, not a second card: saving belongs to
             the form it saves, and a floating bar below the panel read as a
             page-level toolbar shared by every section.

             While a dangerous save waits for confirmation the bar becomes the
             confirmation: one orange button on screen at a time, in the place
             the save button already was. -->
        <div class="cfg-actions">
          {#if confirmPaths.length}
            <button class="btn primary" onclick={() => save(true)} disabled={saving}>
              {t.settings.confirm.save}
            </button>
            <button class="btn ghost" onclick={() => (confirmPaths = [])}>{t.common.cancel}</button>
            <span class="spacer"></span>
            <span class="act-note">{t.settings.confirm.count(confirmPaths.length)}</span>
          {:else}
            <button class="btn primary" onclick={() => save(false)} disabled={saving || dirtyCount === 0}>
              {saving ? t.settings.saving : dirtyCount ? t.settings.saveCount(dirtyCount) : t.settings.save}
            </button>
            <button class="btn ghost" onclick={resetDraft} disabled={saving || dirtyCount === 0}>
              {t.settings.discard}
            </button>
            <span class="spacer"></span>
            <button
              class="btn ghost"
              onclick={undo}
              disabled={saving || !snapshots.length}
              title={t.settings.undo.hint}
            >
              {t.settings.undo.label}
            </button>
            {#if canRestart}
              <button class="btn ghost" onclick={restart}>{t.settings.restartNow}</button>
            {/if}
          {/if}
        </div>
      </div>
    </div>
  {/if}
</section>

<style>
  /* ── two-column shell ──────────────────────────────────────────────────── */

  /* The panel is at least the height of the window, so a short section still
     puts its save bar on the floor of the screen instead of halfway up it.
     Declared once here: the loader measures itself with the same number. */
  section {
    --cfg-min: calc(100vh - var(--topbar-h) - 28px - 2 * var(--sp-7));
  }

  .cfg {
    /* Both cards end on the same line on a short section; the rail keeps its
       own height otherwise — stretching it would give a 39-field section a
       2800px empty card. */
    display: grid;
    grid-template-columns: 200px minmax(0, 1fr);
    gap: var(--sp-6);
    align-items: start;
    min-height: var(--cfg-min);
  }

  /* The rail is a card, not a floating list: on the page ground a bare column
     of labels reads as part of the panel's margin. The surface is what makes
     it a second region with its own edge. */
  .cfg-nav {
    position: sticky;
    top: calc(var(--topbar-h) + var(--sp-6));
    /* The rail shares the panel's floor on a short section — the two cards read
       as one frame — but a 39-field section would want a 2800px rail, so it is
       capped at the height of the window. */
    align-self: stretch;
    max-height: var(--cfg-min);
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
    padding: var(--sp-5);
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--r-md);
  }

  .nav-search {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    height: 30px;
    margin-bottom: var(--sp-2);
    padding: 0 9px;
    border: 1px solid var(--border);
    border-radius: var(--r-sm);
    background: var(--surface-2);
    color: var(--text-3);
  }

  .nav-search:focus-within {
    border-color: var(--action);
    box-shadow: 0 0 0 3px var(--action-soft);
  }

  .nav-search input {
    flex: 1;
    min-width: 0;
    height: 100%;
    padding: 0;
    font-size: var(--fs-11);
    color: var(--text);
    background: none;
    border: 0;
    outline: none;
  }

  .nav-search input::placeholder {
    color: var(--text-3);
  }

  /* The native search affordance is a 14px glyph that does not match the icon
     set; the clear button beside it is the one that must stay. */
  .nav-search input::-webkit-search-cancel-button {
    display: none;
  }

  .nav-clear {
    display: grid;
    place-items: center;
    width: 16px;
    height: 16px;
    flex: none;
    padding: 0;
    color: var(--text-3);
    background: none;
    border: 0;
    border-radius: var(--r-xs);
    cursor: pointer;
  }

  @media (hover: hover) and (pointer: fine) {
    .nav-clear:hover {
      color: var(--text);
      background: var(--surface-3);
    }
  }

  .nav-item {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    height: 34px;
    padding: 0 var(--sp-3);
    font: inherit;
    font-size: var(--fs-13);
    line-height: var(--lh-13);
    color: var(--text-2);
    text-align: left;
    background: none;
    border: 0;
    border-radius: var(--r-sm);
    cursor: pointer;
    transition: background var(--dur-1) var(--ease), color var(--dur-1) var(--ease);
  }

  @media (hover: hover) and (pointer: fine) {
    .nav-item:hover {
      background: var(--surface-2);
      color: var(--text);
    }
  }

  /* Active is the brand tint, not a grey step: the rail and the side nav use
     the same language, so "where am I" reads the same on both. */
  .nav-item.on,
  .nav-item.on:hover {
    background: var(--action-soft);
    color: var(--action-text);
    font-weight: var(--fw-medium);
  }

  /* A section with no hits is dimmed rather than removed: the list keeps its
     shape, so the eye does not re-learn it on every keystroke. */
  .nav-item.empty {
    opacity: 0.45;
  }

  .nav-name {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .nav-n {
    flex: none;
    font-size: var(--fs-10);
    line-height: var(--lh-10);
    color: var(--text-3);
    font-variant-numeric: tabular-nums;
  }

  .nav-item.on .nav-n {
    color: var(--action-text);
    opacity: 0.75;
  }

  /* Dirty is a dot, not a badge: seven badges would shout, one dot per changed
     section is a glance. */
  .nav-dot {
    flex: none;
    width: 5px;
    height: 5px;
    border-radius: 50%;
    background: transparent;
  }

  .nav-dot.on {
    background: var(--action);
  }

  .cfg-nav-select {
    display: none;
    margin-bottom: var(--sp-6);
  }

  /* The panel is the page's content surface. No max-width: the panel is the
     column, and a 920px cap left dead space to its right on every screen wide
     enough to fit the rail beside it. */
  .cfg-panel {
    display: flex;
    flex-direction: column;
    min-width: 0;
    min-height: var(--cfg-min);
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--r-md);
  }

  /* A fixed 66px band, centred: the header is a title plate, not the first
     row of the form, and a rule under it cut the panel into two cards. */
  .panel-head {
    display: flex;
    align-items: center;
    gap: var(--sp-5);
    min-height: 66px;
    flex: none;
    padding: 0 18px;
  }

  .panel-titles {
    display: grid;
    gap: 3px;
    min-width: 0;
  }

  .panel-head h2 {
    margin: 0;
    font-size: var(--fs-14);
    line-height: var(--lh-14);
    font-weight: var(--fw-semibold);
    letter-spacing: var(--track-tight);
  }

  /* The line under the title says what the section owns. Without it, "上游"
     and "出站改写" are two labels you have to open to tell apart. */
  .panel-desc {
    margin: 0;
    font-size: var(--fs-10);
    line-height: var(--lh-10);
    color: var(--text-3);
  }

  /* While searching, the nav narrows the list rather than navigating away. The
     head says which facet is on and is the way back out of it. */
  .facet {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-1);
    justify-self: start;
    padding: 1px 7px 1px 8px;
    font: inherit;
    font-size: var(--fs-10);
    line-height: var(--lh-10);
    color: var(--text-2);
    background: var(--surface-2);
    border: 0;
    border-radius: 999px;
    cursor: pointer;
  }

  @media (hover: hover) and (pointer: fine) {
    .facet:hover {
      color: var(--text);
      background: var(--surface-3);
    }
  }

  /* ── body ──────────────────────────────────────────────────────────────── */

  .cfg-body {
    display: flex;
    flex-direction: column;
    gap: var(--sp-7);
    padding: 0 18px var(--sp-6);
  }

  .fields {
    display: flex;
    flex-direction: column;
    gap: 14px;
  }

  .fields.adv {
    margin-top: 2px;
    padding-top: 14px;
    border-top: 1px dashed var(--border);
  }

  /* ── groups ────────────────────────────────────────────────────────────── */

  /* A task is a unit you switch on or off, so it opens with a rule and its
     name; a block is only a heading. Neither draws a surface: the panel
     already is one, and a card inside a card inside a form is a third edge
     the eye has to learn. */
  .group {
    display: flex;
    flex-direction: column;
    gap: 14px;
  }

  .group.headed {
    padding-top: 14px;
    border-top: 1px solid var(--border);
  }

  .group:first-child.headed {
    padding-top: 0;
    border-top: 0;
  }

  .grp-head {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    min-height: 22px;
  }

  .grp-name {
    font-size: var(--fs-12);
    line-height: var(--lh-12);
    font-weight: var(--fw-semibold);
    color: var(--text);
  }

  .adv-toggle {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    align-self: flex-start;
    margin-top: -2px;
    padding: var(--sp-1) var(--sp-3) var(--sp-1) var(--sp-2);
    font: inherit;
    font-size: var(--fs-11);
    color: var(--text-2);
    background: none;
    border: 0;
    border-radius: var(--r-sm);
    cursor: pointer;
    transition: color var(--dur-1) var(--ease), background var(--dur-1) var(--ease);
  }

  @media (hover: hover) and (pointer: fine) {
    .adv-toggle:hover {
      color: var(--text);
      background: var(--surface-2);
    }
  }

  .adv-toggle :global(.chev) {
    transform: rotate(-90deg);
    transition: transform var(--dur-3) var(--ease);
  }

  .adv-toggle :global(.chev.open) {
    transform: rotate(0deg);
  }

  /* ── confirm gate ──────────────────────────────────────────────────────── */

  .gate {
    display: grid;
    gap: var(--sp-4);
    margin: 0 18px var(--sp-5);
    padding: var(--sp-5);
    background: var(--warn-soft);
    border: 1px solid var(--warn);
    border-radius: var(--r-md);
  }

  .gate-head {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-3);
  }

  .gate-mark {
    display: grid;
    place-items: center;
    width: 20px;
    height: 20px;
    flex: none;
    color: var(--warn);
  }

  .gate-text {
    display: grid;
    gap: 2px;
    min-width: 0;
  }

  .gate-text strong {
    font-size: var(--fs-12);
    line-height: var(--lh-12);
    font-weight: var(--fw-semibold);
  }

  .gate-text p {
    margin: 0;
    font-size: var(--fs-10);
    line-height: var(--lh-11);
    color: var(--text-2);
  }

  .gate-list {
    display: grid;
    gap: var(--sp-1);
    margin: 0;
    padding: 0;
    list-style: none;
    font-size: var(--fs-11);
    line-height: var(--lh-11);
  }

  .gate-list li {
    display: flex;
    align-items: baseline;
    gap: var(--sp-3);
  }

  .gate-path {
    font-family: var(--font-mono);
    font-size: var(--fs-10);
    color: var(--text-3);
  }

  /* Stated from the bar, where the eye already is: how many of the dirty
     fields the confirmation is about. */
  .act-note {
    flex: none;
    font-size: var(--fs-10);
    line-height: var(--lh-10);
    color: var(--warn);
    font-variant-numeric: tabular-nums;
  }

  .panel-note {
    margin: 0 18px var(--sp-4);
    font-size: var(--fs-10);
    line-height: var(--lh-11);
    color: var(--warn);
  }

  /* ── actions ───────────────────────────────────────────────────────────── */

  /* 56px, pinned to the floor of the viewport: the save button must never be
     somewhere the operator has to go find. It is the last row of the panel and
     draws the panel's floor with a hairline. */
  .cfg-actions {
    position: sticky;
    bottom: 0;
    z-index: var(--z-sticky);
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    min-height: 56px;
    flex: none;
    margin-top: auto;
    padding: 0 18px;
    background: var(--surface);
    border-top: 1px solid var(--border);
    border-radius: 0 0 var(--r-lg) var(--r-lg);
  }

  /* The form's own controls, scoped to this panel: filled insets here, because
     a 900px-wide outlined box is a rectangle while a filled one is a field.
     Values are set in mono — every one of them is a path, a port, a number or
     an enum key, and monospace is what makes them line up down the column. The
     rest of the console keeps outlined controls over white cards. */
  .cfg-body :global(input:not([type='checkbox']):not([type='radio'])),
  .cfg-body :global(select),
  .cfg-body :global(textarea) {
    background-color: var(--surface-2);
    border-color: var(--border);
    font-family: var(--font-mono);
  }

  .cfg-body :global(input:not([type='checkbox']):not([type='radio']):focus),
  .cfg-body :global(select:focus),
  .cfg-body :global(textarea:focus) {
    border-color: var(--action);
    box-shadow: 0 0 0 3px var(--action-soft);
  }

  /* ── loading outline ──────────────────────────────────────────────────── */

  /* Same two columns, same row heights as the loaded page. The skeleton is a
     tracing of it, not a placeholder: the nav keeps its 34px rows and the panel
     keeps its 66px head and 14px field rhythm, so nothing moves when the schema
     arrives. */
  .cfg-skeleton {
    display: grid;
    grid-template-columns: 200px minmax(0, 1fr);
    gap: var(--sp-6);
    align-items: start;
    min-height: var(--cfg-min);
  }

  .cfg-nav-skeleton {
    display: flex;
    flex-direction: column;
    gap: 2px;
    align-self: stretch;
    max-height: var(--cfg-min);
    padding: var(--sp-5);
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--r-md);
  }

  .cfg-panel-skeleton {
    display: flex;
    flex-direction: column;
    gap: 14px;
    min-height: var(--cfg-min);
    padding-bottom: var(--sp-6);
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--r-md);
  }

  .skel {
    border-radius: var(--r-md);
    background: var(--surface-2);
  }

  .skel-search {
    height: 30px;
  }

  .skel-nav-item {
    height: 34px;
  }

  .skel-panel-head {
    display: grid;
    gap: 6px;
    align-content: center;
    min-height: 66px;
    padding: 0 18px;
  }

  .skel-title {
    height: 14px;
    width: 7rem;
  }

  .skel-desc {
    height: 10px;
    width: 60%;
  }

  .skel-field {
    height: 32px;
    margin: 0 18px;
  }

  @media (max-width: 999px) {
    .cfg-skeleton {
      grid-template-columns: minmax(0, 1fr);
    }
    .cfg-nav-skeleton {
      display: none;
    }
  }

  /* Below 1000px the rail has already taken a third of the panel's width; a
     200px second column is more navigation than the form can pay for. */
  @media (max-width: 999px) {
    .cfg {
      grid-template-columns: minmax(0, 1fr);
    }
    .cfg-nav {
      display: none;
    }
    .cfg-nav-select {
      display: block;
    }
  }

  @media (max-width: 640px) {
    .cfg-actions {
      position: static;
      flex-wrap: wrap;
      gap: var(--sp-2);
      min-height: 0;
      padding: var(--sp-3) 18px;
    }
  }
</style>
