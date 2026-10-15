<script>
  // Settings edits the gateway's config file in place and reports, per field,
  // whether the change took effect immediately or needs a restart. The form
  // covers the fields people actually reach for; the JSON editor underneath it
  // keeps every other field reachable, so nothing is hidden by the form.

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
      return body.error || `请求失败（${res.status}）`
    } catch {
      return `请求失败（${res.status}）`
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
      if (!res.ok) throw new Error(body.error || `保存失败（${res.status}）`)
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
      error = err.message
    } finally {
      saving = false
    }
  }

  async function rollback(id) {
    if (!confirm(`回滚到快照 ${id}？当前配置会被覆盖。`)) return
    saving = true
    error = ''
    try {
      const res = await fetch(`${API}/config/rollback`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id }),
      })
      const body = await res.json()
      if (!res.ok) throw new Error(body.error || `回滚失败（${res.status}）`)
      await load()
      result = { applied: body.applied || [], restart: body.restart_required || [] }
    } catch (err) {
      error = err.message
    } finally {
      saving = false
    }
  }

  async function restart() {
    if (!confirm('重启网关？当前连接会短暂中断。')) return
    error = ''
    try {
      const res = await fetch(`${API}/restart`, { method: 'POST' })
      if (!res.ok) {
        const body = await res.json().catch(() => ({}))
        throw new Error(body.error || `重启失败（${res.status}）`)
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
    <div class="skeleton-list" aria-busy="true" aria-label="正在加载配置">
      <div class="skeleton-line"></div>
      <div class="skeleton-line short"></div>
      <div class="skeleton-line"></div>
    </div>
  {:else if error && !values}
    <div class="card">
      <h2>设置</h2>
      <p class="msg error">{error}</p>
      <button onclick={load}>重试</button>
    </div>
  {:else}
    <div class="segmented">
      <button class:active={mode === 'form'} onclick={() => (mode = 'form')}>表单</button>
      <button class:active={mode === 'json'} onclick={switchToJson}>高级（JSON）</button>
      <span class="spacer"></span>
      <button onclick={load} disabled={saving}>重新载入</button>
    </div>

    {#if error}
      <p class="msg error">{error}</p>
    {/if}

    {#if result}
      <div class="card result">
        <p class="ok-line">
          已保存。
          {#if result.applied.length}
            <strong>{result.applied.length}</strong> 项立即生效
          {/if}
          {#if result.restart.length}
            ；<strong>{result.restart.length}</strong> 项需重启后生效
          {/if}
          。
        </p>
        {#if result.restart.length}
          <ul class="muted small">
            {#each result.restart as p}
              <li>{labelFor(p)}（{p}）</li>
            {/each}
          </ul>
          {#if canRestart}
            <button class="primary" onclick={restart}>立即重启</button>
          {:else}
            <p class="muted small">当前运行方式不支持自动重启，请手动重启进程。</p>
          {/if}
        {/if}
      </div>
    {/if}

    {#if confirmPaths.length}
      <div class="card warn-card">
        <h2>确认保存</h2>
        <p class="small">以下字段会影响服务可达性或数据存储位置：</p>
        <ul class="small">
          {#each confirmPaths as p}
            <li>{labelFor(p)}（{p}）</li>
          {/each}
        </ul>
        <div class="row-actions">
          <button class="primary" onclick={() => save(true)} disabled={saving}>确认保存</button>
          <button onclick={() => (confirmPaths = [])}>取消</button>
        </div>
      </div>
    {/if}

    {#if mode === 'json'}
      <div class="card">
        <h2>配置文件（原始 JSON）</h2>
        <p class="muted small">
          表单未覆盖的字段都能在这里改。保存前会校验字段名，拼错的字段不会被静默忽略。
        </p>
        <textarea class="json-editor" spellcheck="false" bind:value={jsonText}></textarea>
        {#if jsonError}
          <p class="msg error">JSON 无法解析：{jsonError}</p>
        {/if}
        <div class="row-actions">
          <button onclick={applyJson} disabled={!!jsonError}>用这份内容填回表单</button>
          <button class="primary" onclick={() => save(false)} disabled={saving || !!jsonError}>
            {saving ? '保存中…' : '保存'}
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
                  <label for="cfg-{field.path}">{field.label}</label>
                  <span class="tags">
                    {#if isDirty(field)}<span class="badge dirty-badge">已改动</span>{/if}
                    {#if !field.hot}<span class="badge warn">需重启</span>{/if}
                    {#if field.danger}<span class="badge bad">谨慎</span>{/if}
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
                  <div class="hours" role="group" aria-label="{field.label} 整点">
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
                      class="link"
                      onclick={() =>
                        (showSecret = { ...showSecret, [field.path]: !showSecret[field.path] })}
                    >
                      {showSecret[field.path] ? '隐藏' : '显示'}
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

                <p class="help small">{field.help}</p>
              </div>
            {/each}
          </div>
        </div>
      {/each}
    {/if}

    <div class="card sticky-actions">
      <div class="row-actions">
        <button class="primary" onclick={() => save(false)} disabled={saving || dirtyCount === 0}>
          {saving ? '保存中…' : dirtyCount ? `保存 ${dirtyCount} 项改动` : '保存'}
        </button>
        <button onclick={resetDraft} disabled={saving || dirtyCount === 0}>放弃改动</button>
        {#if snapshots.length}
          <span class="spacer"></span>
          <label class="small muted" for="snap">回滚到</label>
          <select id="snap" onchange={(e) => e.currentTarget.value && rollback(e.currentTarget.value)}>
            <option value="">选择快照…</option>
            {#each snapshots as s}
              <option value={s.id}>{s.time}</option>
            {/each}
          </select>
        {/if}
      </div>
      <p class="muted small">
        保存前会自动留一份快照（最多保留 20 份）。改动监听地址、数据目录或加密开关会导致服务不可达时，可以用快照回滚。
      </p>
    </div>
  {/if}
</section>

<style>
  .fields {
    display: grid;
    gap: 14px;
  }
  .field {
    display: grid;
    gap: 4px;
    padding: 8px 10px;
    border-radius: 8px;
    border-left: 2px solid transparent;
  }
  .field.dirty {
    border-left-color: var(--accent);
    background: color-mix(in srgb, var(--accent) 6%, transparent);
  }
  .field-head {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .field-head label {
    font-weight: 600;
    font-size: 0.9rem;
  }
  .tags {
    display: inline-flex;
    gap: 6px;
  }
  .help {
    color: var(--muted);
    margin: 0;
  }
  .secret {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .json-editor {
    width: 100%;
    min-height: 320px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 0.8rem;
    line-height: 1.5;
  }
  .hours {
    display: flex;
    flex-wrap: wrap;
    gap: 3px;
  }
  .hour {
    min-width: 30px;
    padding: 3px 4px;
    font-size: 0.72rem;
    border: 1px solid var(--border);
    background: var(--bg);
    color: var(--muted);
    border-radius: 5px;
    cursor: pointer;
  }
  .hour.on {
    background: var(--accent);
    color: #fff;
    border-color: var(--accent);
  }
  .row-actions {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .spacer {
    flex: 1;
  }
  .sticky-actions {
    position: sticky;
    bottom: 12px;
  }
  .result .ok-line {
    margin: 0 0 6px;
  }
  .warn-card {
    border-left: 3px solid #d98324;
  }
  .skeleton-list {
    display: grid;
    gap: 10px;
  }
  .skeleton-line {
    height: 14px;
    border-radius: 6px;
    background: linear-gradient(90deg, var(--bg) 25%, var(--card) 50%, var(--bg) 75%);
    background-size: 200% 100%;
    animation: shimmer 1.2s infinite;
  }
  .skeleton-line.short {
    width: 45%;
  }
  @keyframes shimmer {
    from {
      background-position: 200% 0;
    }
    to {
      background-position: -200% 0;
    }
  }
  @media (prefers-reduced-motion: reduce) {
    .skeleton-line {
      animation: none;
    }
  }
  @media (max-width: 640px) {
    .sticky-actions {
      position: static;
    }
    .json-editor {
      min-height: 220px;
    }
  }
</style>
