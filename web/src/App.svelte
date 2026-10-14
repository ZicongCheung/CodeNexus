<script>
  // CodeNexus console. Sources are a first-class concept: every account carries
  // the upstream it belongs to, and the whole UI is grouped around that.

  const API = '/panel/api'

  let session = $state(null)
  let accounts = $state([])
  let loading = $state(true)
  let banner = $state('')

  let tab = $state('accounts')

  // ── onboarding (device authorization) ────────────────────────────────────
  let authSource = $state('workbuddy')
  let authRealm = $state('cn')
  let authState = $state('')
  let authUrl = $state('')
  let authPhase = $state('idle') // idle | starting | waiting | done | error
  let authMsg = $state('')
  let pollTimer = null

  // ── import ───────────────────────────────────────────────────────────────
  let importMode = $state('inline') // inline | path | bundle
  let importText = $state('')
  let importPath = $state('')
  let importBundle = $state('')
  let importPass = $state('')
  let importPhase = $state('idle')
  let importMsg = $state('')
  let importResult = $state(null)

  // ── task center ──────────────────────────────────────────────────────────
  // The console never guesses what an account supports: it lists the task
  // catalogue, then loads one account's tasks on demand so a pool of twenty
  // credentials does not fan out into twenty upstream round trips on open.
  let catalog = $state([])
  let catalogErr = $state('')
  let expanded = $state({})
  let accountTasks = $state({}) // uid -> { loading, tasks, error }
  let opBusy = $state('') // "<uid>:<op>" for one account, "<op>" for a batch
  let taskMsg = $state('')
  let queue = $state([])
  let queueBusy = $state(false)
  let results = $state({}) // uid -> last growth run

  // ── request log ──────────────────────────────────────────────────────────
  let metrics = $state(null)
  let logEvents = $state([])
  let logTimer = null

  const sources = $derived(session?.sources ?? ['workbuddy'])

  const inlinePlaceholder =
    '[{"source":"workbuddy","uid":"...","access_token":"...","refresh_token":"..."}]'

  async function api(path, options = {}) {
    const res = await fetch(API + path, {
      headers: { 'Content-Type': 'application/json' },
      ...options,
    })
    const text = await res.text()
    let body = {}
    try {
      body = text ? JSON.parse(text) : {}
    } catch (err) {
      throw new Error(`响应解析失败（HTTP ${res.status}）`)
    }
    if (!res.ok || body.ok === false) {
      throw new Error(body.error || `请求失败（HTTP ${res.status}）`)
    }
    return body
  }

  async function loadSession() {
    try {
      session = await api('/session')
    } catch (err) {
      banner = err.message
    }
  }

  async function loadAccounts() {
    try {
      const body = await api('/accounts')
      accounts = body.accounts ?? []
    } catch (err) {
      banner = err.message
    } finally {
      loading = false
    }
  }

  async function refresh() {
    await Promise.all([loadSession(), loadAccounts()])
  }

  refresh()

  const grouped = $derived.by(() => {
    const map = new Map()
    for (const acc of accounts) {
      const key = acc.source || 'unknown'
      if (!map.has(key)) map.set(key, [])
      map.get(key).push(acc)
    }
    return [...map.entries()].map(([source, items]) => ({ source, items }))
  })

  function health(acc) {
    if (!acc.enabled) return { cls: 'off', text: '已停用' }
    if (acc.broken) return { cls: 'bad', text: '熔断' }
    if (acc.cooling) return { cls: 'warn', text: '冷却中' }
    if (acc.healthy) return { cls: 'ok', text: '健康' }
    return { cls: 'idle', text: '待命' }
  }

  function fmtTime(sec) {
    if (!sec) return '—'
    const d = new Date(sec * 1000)
    return d.toLocaleString()
  }

  async function removeAccount(acc) {
    if (!confirm(`移除账号 ${acc.label || acc.uid}（${acc.source}）？`)) return
    try {
      await api('/accounts/remove', {
        method: 'POST',
        body: JSON.stringify({ key: acc.key }),
      })
      await loadAccounts()
      await loadSession()
    } catch (err) {
      banner = err.message
    }
  }

  // ── device authorization ─────────────────────────────────────────────────
  async function startAuth() {
    stopPolling()
    authPhase = 'starting'
    authMsg = ''
    authUrl = ''
    authState = ''
    try {
      const body = await api('/auth/start', {
        method: 'POST',
        body: JSON.stringify({ source: authSource, realm: authRealm }),
      })
      authState = body.state
      authUrl = body.url
      authPhase = 'waiting'
      startPolling()
    } catch (err) {
      authPhase = 'error'
      authMsg = err.message
    }
  }

  function startPolling() {
    stopPolling()
    pollTimer = setInterval(pollAuth, 3000)
    pollAuth()
  }

  function stopPolling() {
    if (pollTimer) {
      clearInterval(pollTimer)
      pollTimer = null
    }
  }

  async function pollAuth() {
    if (!authState) return
    try {
      const body = await api('/auth/poll?state=' + encodeURIComponent(authState))
      if (body.done) {
        stopPolling()
        authPhase = 'done'
        authMsg = `已添加：${body.label || body.uid}`
        authState = ''
        await refresh()
      }
    } catch (err) {
      stopPolling()
      authPhase = 'error'
      authMsg = err.message
    }
  }

  function resetAuth() {
    stopPolling()
    authPhase = 'idle'
    authMsg = ''
    authUrl = ''
    authState = ''
  }

  // ── import ───────────────────────────────────────────────────────────────
  async function doImport() {
    importPhase = 'running'
    importMsg = ''
    importResult = null
    let payload
    if (importMode === 'inline') {
      if (!importText.trim()) {
        importPhase = 'error'
        importMsg = '请粘贴账号 JSON'
        return
      }
      let parsed
      try {
        parsed = JSON.parse(importText)
      } catch (err) {
        importPhase = 'error'
        importMsg = 'JSON 解析失败：' + err.message
        return
      }
      payload = { accounts: Array.isArray(parsed) ? parsed : parsed.accounts || [parsed] }
    } else if (importMode === 'path') {
      if (!importPath.trim()) {
        importPhase = 'error'
        importMsg = '请输入服务器上的文件路径'
        return
      }
      payload = { path: importPath.trim() }
    } else {
      if (!importBundle.trim() || !importPass) {
        importPhase = 'error'
        importMsg = '请粘贴加密包并填写口令'
        return
      }
      payload = { bundle: importBundle.trim(), passphrase: importPass }
    }
    try {
      const body = await api('/auth/import', {
        method: 'POST',
        body: JSON.stringify(payload),
      })
      importResult = body
      importPhase = 'done'
      importMsg = `导入 ${body.imported}/${body.total} 个账号`
      await refresh()
    } catch (err) {
      importPhase = 'error'
      importMsg = err.message
    }
  }

  function switchTab(name) {
    tab = name
    if (name !== 'add') resetAuth()
    stopLogPolling()
    if (name === 'tasks') {
      if (!catalog.length) loadCatalog()
      loadQueue()
    } else if (name === 'logs') {
      startLogPolling()
    }
  }

  // ── task center ──────────────────────────────────────────────────────────
  const classLabels = {
    account: '账号原生',
    desktop: '桌面端事件链',
    mini_program: '小程序事件链',
  }
  const classOrder = ['account', 'desktop', 'mini_program']

  const catalogGroups = $derived.by(() => {
    const map = new Map()
    for (const a of catalog) {
      const cls = a.class || 'account'
      if (!map.has(cls)) map.set(cls, [])
      map.get(cls).push(a)
    }
    return [...map.entries()]
      .sort((x, y) => classOrder.indexOf(x[0]) - classOrder.indexOf(y[0]))
      .map(([cls, items]) => ({ cls, label: classLabels[cls] || cls, items }))
  })

  async function loadCatalog() {
    try {
      const body = await api('/tasks/catalog')
      catalog = body.actions ?? []
      catalogErr = ''
    } catch (err) {
      catalogErr = err.message
    }
  }

  function setAccount(uid, patch) {
    accountTasks = { ...accountTasks, [uid]: { ...(accountTasks[uid] || {}), ...patch } }
  }

  async function loadAccountTasks(uid) {
    setAccount(uid, { loading: true, error: '' })
    try {
      const body = await api(`/accounts/${encodeURIComponent(uid)}/tasks`)
      setAccount(uid, { loading: false, tasks: body.tasks ?? [], error: '' })
    } catch (err) {
      setAccount(uid, { loading: false, tasks: [], error: err.message })
    }
  }

  async function toggleTasks(acc) {
    const uid = acc.uid
    const open = !expanded[uid]
    expanded = { ...expanded, [uid]: open }
    if (open && !accountTasks[uid]?.tasks?.length) await loadAccountTasks(uid)
  }

  async function reloadExpanded() {
    const uids = Object.keys(expanded).filter((k) => expanded[k])
    await Promise.all(uids.map((uid) => loadAccountTasks(uid)))
  }

  // guard serialises the console's write operations: one at a time, with the
  // button labelled after the key, so a double click cannot start two sweeps.
  async function guard(key, fn) {
    opBusy = key
    taskMsg = ''
    try {
      await fn()
    } catch (err) {
      taskMsg = err.message
    } finally {
      opBusy = ''
    }
  }

  async function acceptTask(acc, code) {
    await guard(`${acc.uid}:accept`, async () => {
      await api(`/accounts/${encodeURIComponent(acc.uid)}/tasks/accept`, {
        method: 'POST',
        body: JSON.stringify(code ? { task_code: code } : { all: true }),
      })
      taskMsg = code ? `已接取 ${code}` : '已接取全部可接取任务'
      await loadAccountTasks(acc.uid)
    })
  }

  async function claimTask(acc, code) {
    await guard(`${acc.uid}:claim:${code}`, async () => {
      const body = await api(`/accounts/${encodeURIComponent(acc.uid)}/tasks/claim`, {
        method: 'POST',
        body: JSON.stringify({ task_code: code }),
      })
      taskMsg = `${code} 领取成功：+${body.credit} 额度${body.energy ? ` +${body.energy} 能量` : ''}`
      await loadAccountTasks(acc.uid)
      await loadAccounts()
    })
  }

  async function runTask(acc, code) {
    await guard(`${acc.uid}:run:${code}`, async () => {
      const res = await api(`/accounts/${encodeURIComponent(acc.uid)}/tasks/auto`, {
        method: 'POST',
        body: JSON.stringify({ task_code: code }),
      })
      results = { ...results, [acc.uid]: [res] }
      taskMsg = describeResult(res)
      await loadAccountTasks(acc.uid)
      await loadAccounts()
    })
  }

  async function runAllTasks(acc) {
    await guard(`${acc.uid}:auto`, async () => {
      const body = await api(`/accounts/${encodeURIComponent(acc.uid)}/tasks/auto_all`, { method: 'POST' })
      results = { ...results, [acc.uid]: body.results ?? [] }
      taskMsg = summarize(body.results ?? [])
      await loadAccountTasks(acc.uid)
      await loadAccounts()
    })
  }

  async function accountOp(acc, op, label) {
    await guard(`${acc.uid}:${op}`, async () => {
      await api(`/accounts/${encodeURIComponent(acc.uid)}/${op}`, { method: 'POST' })
      taskMsg = `${acc.label || acc.uid}：${label} 已完成`
      await loadAccounts()
      if (expanded[acc.uid]) await loadAccountTasks(acc.uid)
    })
  }

  async function batch(op, label) {
    await guard(op, async () => {
      await api(`/batch/${op}`, { method: 'POST' })
      taskMsg = `${label} 执行完成`
      await loadAccounts()
      await reloadExpanded()
      await loadQueue()
    })
  }

  async function loadQueue() {
    queueBusy = true
    try {
      const body = await api('/tasks/queue')
      queue = body.queue ?? []
    } catch (err) {
      taskMsg = err.message
    } finally {
      queueBusy = false
    }
  }

  async function runQueue() {
    queueBusy = true
    taskMsg = ''
    try {
      const body = await api('/tasks/run_queue', { method: 'POST' })
      for (const row of body.accounts ?? []) {
        results = { ...results, [row.uid]: row.results ?? [] }
      }
      taskMsg = '队列执行完成'
      await loadQueue()
      await reloadExpanded()
      await loadAccounts()
    } catch (err) {
      taskMsg = err.message
    } finally {
      queueBusy = false
    }
  }

  function describeResult(r) {
    const parts = [`${r.task_code}：${r.status}`]
    if (r.progress_before || r.progress_after) {
      parts.push(`${r.progress_before || '—'} → ${r.progress_after || '—'}`)
    }
    if (r.claimed) parts.push(`已领奖 +${r.credit}`)
    if (r.message) parts.push(r.message)
    return parts.join(' · ')
  }

  function summarize(list) {
    if (!list.length) return '没有可执行的任务'
    const done = list.filter((r) => r.status === 'done').length
    const skipped = list.filter((r) => r.status === 'skipped').length
    const failed = list.filter((r) => r.status === 'error').length
    const credit = list.reduce((n, r) => n + (r.credit || 0), 0)
    return `执行 ${list.length} 项：${done} 完成 / ${skipped} 跳过 / ${failed} 失败${credit ? `，共 +${credit} 额度` : ''}`
  }

  function taskState(t) {
    if (t.claimed) return { cls: 'off', text: '已领取' }
    if (t.locked) return { cls: 'idle', text: '未解锁' }
    const cur = Number(t.current || 0)
    const target = Number(t.target || 0)
    if (t.claimable || (target > 0 && cur >= target)) return { cls: 'ok', text: '可领取' }
    if (t.accept_status === 'completed') return { cls: 'ok', text: '已完成' }
    if (cur > 0) return { cls: 'warn', text: '进行中' }
    return { cls: 'idle', text: '待完成' }
  }

  // ── request log ──────────────────────────────────────────────────────────
  function startLogPolling() {
    stopLogPolling()
    logTimer = setInterval(loadLogs, 3000)
    loadLogs()
  }

  function stopLogPolling() {
    if (logTimer) {
      clearInterval(logTimer)
      logTimer = null
    }
  }

  async function loadLogs() {
    try {
      const [m, l] = await Promise.all([api('/observability/metrics'), api('/observability/logs')])
      metrics = m
      logEvents = l.recent ?? []
    } catch (err) {
      taskMsg = err.message
    }
  }

  function fmtClock(t) {
    if (!t) return '—'
    const d = new Date(t)
    if (Number.isNaN(d.getTime())) return '—'
    return d.toLocaleTimeString()
  }
</script>

<svelte:head>
  <title>CodeNexus 控制台</title>
</svelte:head>

<div class="shell">
  <header>
    <div class="brand">
      <span class="logo">CN</span>
      <div>
        <h1>CodeNexus</h1>
        <p class="muted">统一 AI 编码账号网关 · 控制台</p>
      </div>
    </div>
    <div class="stat">
      <span class="count">{accounts.length}</span>
      <span class="muted">个账号</span>
      {#if session}
        <span class="dot"></span>
        <span class="muted">{session.service}</span>
      {/if}
    </div>
  </header>

  {#if banner}
    <div class="banner">
      <span>{banner}</span>
      <button class="link" onclick={() => (banner = '')}>关闭</button>
    </div>
  {/if}

  <nav class="tabs">
    <button class:active={tab === 'accounts'} onclick={() => switchTab('accounts')}>账号</button>
    <button class:active={tab === 'tasks'} onclick={() => switchTab('tasks')}>任务中心</button>
    <button class:active={tab === 'logs'} onclick={() => switchTab('logs')}>运行日志</button>
    <button class:active={tab === 'add'} onclick={() => switchTab('add')}>添加账号</button>
    <button class:active={tab === 'import'} onclick={() => switchTab('import')}>导入</button>
  </nav>

  {#if tab === 'accounts'}
    <section>
      {#if loading}
        <p class="muted pad">加载中…</p>
      {:else if accounts.length === 0}
        <div class="empty">
          <p>还没有账号。</p>
          <p class="muted">前往「添加账号」完成设备授权，或从「导入」接入已有凭据。</p>
        </div>
      {:else}
        {#each grouped as group (group.source)}
          <div class="group">
            <h2>
              <span class="src">{group.source}</span>
              <span class="pill">{group.items.length}</span>
            </h2>
            <div class="table">
              <div class="row head">
                <span>账号</span>
                <span>状态</span>
                <span>凭据</span>
                <span>调用</span>
                <span>过期</span>
                <span></span>
              </div>
              {#each group.items as acc (acc.key)}
                {@const h = health(acc)}
                <div class="row">
                  <span class="label" title={acc.uid}>
                    {acc.label || acc.uid}
                    <em class="uid">{acc.uid}</em>
                  </span>
                  <span>
                    <span class="badge {h.cls}">{h.text}</span>
                    {#if acc.in_flight > 0}
                      <span class="muted small">· {acc.in_flight} 进行中</span>
                    {/if}
                  </span>
                  <span class="muted small">
                    {#if acc.has_refresh_token}可刷新{:else}仅访问令牌{/if}
                  </span>
                  <span class="muted small">{acc.success}✓ / {acc.errors}✗</span>
                  <span class="muted small">{fmtTime(acc.expires_at)}</span>
                  <span class="right">
                    <button class="link danger" onclick={() => removeAccount(acc)}>移除</button>
                  </span>
                </div>
              {/each}
            </div>
          </div>
        {/each}
      {/if}
    </section>
  {/if}

  {#if tab === 'tasks'}
    <section class="column">
      <div class="card">
        <div class="rowhead">
          <h2>批量维护</h2>
          <span class="muted small">与定时调度共用同一套幂等执行器，随时可手动触发。</span>
        </div>
        <div class="ops">
          <button onclick={() => batch('checkin_all', '全部签到')} disabled={!!opBusy}>全部签到</button>
          <button onclick={() => batch('activity_all', '全部活跃')} disabled={!!opBusy}>全部活跃</button>
          <button onclick={() => batch('balance_all', '刷新额度')} disabled={!!opBusy}>刷新额度</button>
          <button onclick={() => batch('travel_all', '全部出行')} disabled={!!opBusy}>全部出行</button>
          <button onclick={() => batch('keepalive_all', '全部保活')} disabled={!!opBusy}>全部保活</button>
          <span class="spacer"></span>
          <button onclick={loadQueue} disabled={queueBusy}>刷新队列</button>
          <button class="primary" onclick={runQueue} disabled={queueBusy}>
            {queueBusy ? '执行中…' : '执行队列'}
          </button>
        </div>
        {#if opBusy && !opBusy.includes(':')}
          <p class="msg">{opBusy} 执行中…</p>
        {/if}
        {#if taskMsg}
          <p class="msg ok">{taskMsg}</p>
        {/if}
      </div>

      <div class="card">
        <div class="rowhead">
          <h2>待办队列</h2>
          <span class="muted small">仍有未完成任务的启用账号。</span>
        </div>
        {#if queue.length === 0}
          <p class="muted small">队列为空：所有账号的任务都已完成或已领取。</p>
        {:else}
          <div class="qtable">
            <div class="qrow qhead"><span>账号</span><span>待完成</span><span>可领取</span></div>
            {#each queue as row (row.uid)}
              <div class="qrow">
                <span>{row.label || row.uid}</span>
                <span>{row.pending}</span>
                <span>{row.claimable > 0 ? row.claimable : '—'}</span>
              </div>
            {/each}
          </div>
        {/if}
      </div>

      <div class="card">
        <details>
          <summary>执行器目录（{catalog.length}）</summary>
          {#if catalogErr}
            <p class="msg error">{catalogErr}</p>
          {:else}
            {#each catalogGroups as g (g.cls)}
              <div class="catalog">
                <h3>{g.label}<span class="pill">{g.items.length}</span></h3>
                <ul>
                  {#each g.items as a (a.task_code)}
                    <li>
                      <code>{a.task_code}</code>
                      <span class="muted">{a.desc}</span>
                      {#if a.attempt}<span class="badge warn">尽力而为</span>{/if}
                    </li>
                  {/each}
                </ul>
              </div>
            {/each}
          {/if}
        </details>
      </div>

      <div class="group">
        <h2>
          <span class="src">账号任务</span>
          <span class="pill">{accounts.length}</span>
        </h2>
        {#if accounts.length === 0}
          <div class="empty">
            <p>还没有账号。</p>
            <p class="muted">先在「添加账号」完成设备授权，任务中心才有可操作的对象。</p>
          </div>
        {/if}
        {#each accounts as acc (acc.key)}
          {@const st = accountTasks[acc.uid] || {}}
          <div class="card acc-card">
            <div class="acc-head">
              <button class="link caret" onclick={() => toggleTasks(acc)}>
                {expanded[acc.uid] ? '▾' : '▸'} {acc.label || acc.uid}
              </button>
              <span class="badge {health(acc).cls}">{health(acc).text}</span>
              <span class="muted small">{acc.source}</span>
              <span class="muted small">
                额度 {acc.credits_known ? acc.credits : '—'}
                {#if acc.expiring > 0} · {acc.expiring} 即将过期{/if}
              </span>
              <span class="spacer"></span>
              <button class="link" onclick={() => runAllTasks(acc)} disabled={!!opBusy}>执行全部</button>
              <button class="link" onclick={() => acceptTask(acc, '')} disabled={!!opBusy}>接取全部</button>
              <button class="link" onclick={() => accountOp(acc, 'checkin', '签到')} disabled={!!opBusy}>签到</button>
              <button class="link" onclick={() => accountOp(acc, 'balance', '刷新额度')} disabled={!!opBusy}>额度</button>
              <button class="link" onclick={() => accountOp(acc, 'keepalive', '保活')} disabled={!!opBusy}>保活</button>
              <button
                class="link"
                onclick={() => accountOp(acc, acc.paused ? 'resume' : 'pause', acc.paused ? '恢复' : '暂停')}
                disabled={!!opBusy}
              >
                {acc.paused ? '恢复' : '暂停'}
              </button>
            </div>

            {#if opBusy.startsWith(acc.uid + ':')}
              <p class="msg small">{opBusy.split(':').slice(1).join(':')} 执行中…</p>
            {/if}

            {#if expanded[acc.uid]}
              {#if st.loading}
                <p class="muted small pad-sm">加载任务中…</p>
              {:else if st.error}
                <p class="msg error">{st.error}</p>
              {:else if !st.tasks || st.tasks.length === 0}
                <p class="muted small">该账号没有可用的成长任务。</p>
              {:else}
                <div class="ttable">
                  <div class="trow thead">
                    <span>任务</span><span>进度</span><span>状态</span><span>奖励</span><span></span>
                  </div>
                  {#each st.tasks as t (t.code)}
                    {@const s = taskState(t)}
                    <div class="trow">
                      <span class="label" title={t.description || t.desc || t.code}>
                        {t.title || t.code}
                        <em class="uid">{t.code}{t.mini_program ? ' · 小程序' : ''}</em>
                      </span>
                      <span class="muted small">{t.target > 0 ? `${t.current || 0}/${t.target}` : '—'}</span>
                      <span><span class="badge {s.cls}">{s.text}</span></span>
                      <span class="muted small">
                        {#if t.credit > 0}+{t.credit} 额度{/if}
                        {#if t.energy > 0} +{t.energy} 能量{/if}
                      </span>
                      <span class="right actions">
                        {#if t.claimable && !t.claimed}
                          <button class="link" onclick={() => claimTask(acc, t.code)} disabled={!!opBusy}>领取</button>
                        {/if}
                        {#if !t.claimed}
                          <button class="link" onclick={() => runTask(acc, t.code)} disabled={!!opBusy}>执行</button>
                        {/if}
                      </span>
                    </div>
                  {/each}
                </div>
                <button class="link tiny" onclick={() => loadAccountTasks(acc.uid)} disabled={!!opBusy}>重新加载</button>
              {/if}

              {#if results[acc.uid]?.length}
                <ul class="results">
                  {#each results[acc.uid] as r, i (r.task_code + i)}
                    <li class={r.status}>{describeResult(r)}</li>
                  {/each}
                </ul>
              {/if}
            {/if}
          </div>
        {/each}
      </div>
    </section>
  {/if}

  {#if tab === 'logs'}
    <section class="column">
      {#if !metrics || metrics.enabled === false}
        <div class="card">
          <h2>运行日志</h2>
          <p class="muted small">
            请求日志未启用。在配置中开启 reqlog 后，这里会显示最近的网关请求：接口、账号、模型、状态、耗时与令牌用量，
            但绝不包含提示词、回复内容或任何凭据。
          </p>
        </div>
      {:else}
        <div class="stats">
          <div class="stat-card"><span class="k">处理中</span><span class="v">{metrics.in_flight}</span></div>
          <div class="stat-card"><span class="k">已完成</span><span class="v">{metrics.completed}</span></div>
          <div class="stat-card"><span class="k">成功率</span><span class="v">{metrics.success_rate ?? 0}%</span></div>
          <div class="stat-card"><span class="k">平均耗时</span><span class="v">{metrics.avg_duration_ms ?? 0}<em>ms</em></span></div>
          <div class="stat-card">
            <span class="k">平均首字节</span>
            <span class="v">{metrics.avg_ttfb_ms ? metrics.avg_ttfb_ms : '—'}{metrics.avg_ttfb_ms ? 'ms' : ''}</span>
          </div>
        </div>
        <div class="table">
          <div class="row logrow loghead">
            <span>时间</span>
            <span>接口</span>
            <span>账号</span>
            <span>模型</span>
            <span>状态</span>
            <span>耗时</span>
            <span>Token</span>
          </div>
          {#if logEvents.length === 0}
            <div class="row logrow"><span class="muted small">暂无请求记录</span></div>
          {/if}
          {#each logEvents as e (e.request_id)}
            <div class="row logrow">
              <span class="muted small">{fmtClock(e.time)}</span>
              <span class="mono small">{e.path}</span>
              <span class="muted small">{e.account || '—'}</span>
              <span class="muted small">{e.model || '—'}</span>
              <span><span class="badge {e.ok ? 'ok' : 'bad'}">{e.status}</span></span>
              <span class="muted small">
                {e.duration_ms}ms{#if e.ttfb_ms} / {e.ttfb_ms}ms{/if}
                {#if e.stream} · 流式{/if}
              </span>
              <span class="muted small">
                {e.total_tokens || '—'}{#if e.tokens_per_second} · {e.tokens_per_second}tok/s{/if}
              </span>
            </div>
          {/each}
        </div>
      {/if}
    </section>
  {/if}

  {#if tab === 'add'}
    <section class="card">
      <h2>设备授权登录</h2>
      <p class="muted">
        选择来源与区域后开始授权，浏览器会打开登录页；完成后此页面自动接收账号，无需手工粘贴令牌。
      </p>

      {#if authPhase === 'idle' || authPhase === 'error'}
        <div class="form">
          <label>
            <span>来源</span>
            <select bind:value={authSource}>
              {#each sources as s}<option value={s}>{s}</option>{/each}
            </select>
          </label>
          <label>
            <span>区域</span>
            <select bind:value={authRealm}>
              <option value="cn">中国大陆</option>
              <option value="global">国际</option>
            </select>
          </label>
          <button class="primary" onclick={startAuth}>开始授权</button>
        </div>
        {#if authPhase === 'error'}
          <p class="msg error">{authMsg}</p>
        {/if}
      {/if}

      {#if authPhase === 'starting'}
        <p class="muted">正在申请授权码…</p>
      {/if}

      {#if authPhase === 'waiting'}
        <div class="auth-wait">
          <p class="spin">等待浏览器完成授权…</p>
          {#if authUrl}
            <p>
              <a href={authUrl} target="_blank" rel="noreferrer">打开登录页</a>
            </p>
            <p class="muted small mono">{authUrl}</p>
          {/if}
          <button class="link" onclick={resetAuth}>取消</button>
        </div>
      {/if}

      {#if authPhase === 'done'}
        <div class="auth-done">
          <p class="msg ok">{authMsg}</p>
          <button class="primary" onclick={resetAuth}>再添加一个</button>
        </div>
      {/if}
    </section>
  {/if}

  {#if tab === 'import'}
    <section class="card">
      <h2>导入账号</h2>
      <p class="muted">支持直接粘贴 JSON、读取服务器本地文件，或使用口令解密导出的加密包。</p>

      <div class="segmented">
        <button class:active={importMode === 'inline'} onclick={() => (importMode = 'inline')}>粘贴 JSON</button>
        <button class:active={importMode === 'path'} onclick={() => (importMode = 'path')}>本地文件</button>
        <button class:active={importMode === 'bundle'} onclick={() => (importMode = 'bundle')}>加密包</button>
      </div>

      {#if importMode === 'inline'}
        <label class="stack">
          <span>账号 JSON（数组，或形如 <code>&#123;accounts: [...]&#125;</code> 的对象）</span>
          <textarea rows="8" bind:value={importText} placeholder={inlinePlaceholder}></textarea>
        </label>
      {:else if importMode === 'path'}
        <label class="stack">
          <span>服务器文件路径</span>
          <input bind:value={importPath} placeholder="D:/backup/accounts.json" />
        </label>
      {:else}
        <label class="stack">
          <span>加密包内容（JSON 或 base64）</span>
          <textarea rows="5" bind:value={importBundle}></textarea>
        </label>
        <label class="stack">
          <span>口令</span>
          <input type="password" bind:value={importPass} />
        </label>
      {/if}

      <button class="primary" onclick={doImport} disabled={importPhase === 'running'}>
        {importPhase === 'running' ? '导入中…' : '导入'}
      </button>

      {#if importMsg}
        <p class="msg {importPhase === 'error' ? 'error' : 'ok'}">{importMsg}</p>
      {/if}
      {#if importResult && importResult.errors?.length}
        <ul class="skips">
          {#each importResult.errors as e}<li>{e}</li>{/each}
        </ul>
      {/if}
    </section>
  {/if}

  <footer class="muted small">
    本控制台仅从本机回环地址使用；远程访问需在请求中携带网关 api_key。
  </footer>
</div>

<style>
  :global(:root) {
    --bg: #f6f7f9;
    --surface: #ffffff;
    --border: #e3e6ea;
    --text: #1a1d21;
    --muted: #6b7280;
    --accent: #2f6fed;
    --ok: #15803d;
    --ok-bg: #e7f6ec;
    --warn: #b45309;
    --warn-bg: #fdf2e2;
    --bad: #b91c1c;
    --bad-bg: #fbe9e9;
    --idle-bg: #eef1f5;
  }
  @media (prefers-color-scheme: dark) {
    :global(:root) {
      --bg: #14161a;
      --surface: #1c1f24;
      --border: #2b3037;
      --text: #e6e8eb;
      --muted: #9aa2ad;
      --accent: #5b8dff;
      --ok-bg: #12281a;
      --warn-bg: #2c2109;
      --bad-bg: #2c1414;
      --idle-bg: #232830;
    }
  }

  .shell {
    font-family: ui-sans-serif, system-ui, -apple-system, 'Segoe UI', Roboto, sans-serif;
    color: var(--text);
    background: var(--bg);
    min-height: 100vh;
    padding: 2rem 1.5rem 3rem;
    box-sizing: border-box;
  }
  .shell > * {
    max-width: 62rem;
    margin-left: auto;
    margin-right: auto;
  }

  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    margin-bottom: 1.5rem;
  }
  .brand { display: flex; align-items: center; gap: .85rem; }
  .logo {
    display: grid;
    place-items: center;
    width: 2.5rem; height: 2.5rem;
    border-radius: .6rem;
    background: var(--accent);
    color: #fff;
    font-weight: 700;
    letter-spacing: .02em;
  }
  h1 { font-size: 1.15rem; margin: 0; }
  header p { margin: .15rem 0 0; font-size: .8rem; }
  .stat { display: flex; align-items: center; gap: .45rem; font-size: .85rem; }
  .count { font-size: 1.1rem; font-weight: 700; font-variant-numeric: tabular-nums; }
  .dot { width: .35rem; height: .35rem; border-radius: 50%; background: var(--ok); }

  .muted { color: var(--muted); }
  .small { font-size: .78rem; }
  .mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; }
  .pad { padding: 2rem 0; }
  .right { text-align: right; }

  .banner {
    display: flex; align-items: center; justify-content: space-between; gap: 1rem;
    background: var(--bad-bg); color: var(--bad);
    border: 1px solid var(--border);
    border-radius: .5rem;
    padding: .6rem .9rem;
    margin-bottom: 1rem;
    font-size: .85rem;
  }

  .tabs { display: flex; gap: .25rem; border-bottom: 1px solid var(--border); margin-bottom: 1.25rem; }
  .tabs button {
    background: none; border: none; cursor: pointer;
    padding: .55rem .9rem;
    font: inherit; color: var(--muted);
    border-bottom: 2px solid transparent;
    margin-bottom: -1px;
  }
  .tabs button.active { color: var(--text); border-bottom-color: var(--accent); font-weight: 600; }

  .group { margin-bottom: 1.75rem; }
  .group h2 { display: flex; align-items: center; gap: .5rem; font-size: .95rem; margin: 0 0 .6rem; }
  .src {
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    background: var(--idle-bg);
    border-radius: .35rem; padding: .12rem .5rem;
  }
  .pill {
    font-size: .72rem; color: var(--muted);
    background: var(--idle-bg); border-radius: 999px; padding: .1rem .5rem;
  }

  .table { background: var(--surface); border: 1px solid var(--border); border-radius: .6rem; overflow: hidden; }
  .row {
    display: grid;
    grid-template-columns: 2fr 1.2fr 1fr 1fr 1.2fr 4rem;
    gap: .75rem;
    align-items: center;
    padding: .65rem .9rem;
    border-top: 1px solid var(--border);
    font-size: .85rem;
  }
  .row:first-child { border-top: none; }
  .row.head { background: var(--bg); color: var(--muted); font-size: .75rem; text-transform: uppercase; letter-spacing: .04em; }
  .label { display: flex; flex-direction: column; gap: .1rem; font-weight: 500; min-width: 0; }
  .label .uid { font-style: normal; color: var(--muted); font-size: .72rem; font-family: ui-monospace, monospace; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

  .badge { display: inline-block; border-radius: 999px; padding: .1rem .55rem; font-size: .74rem; font-weight: 600; }
  .badge.ok { background: var(--ok-bg); color: var(--ok); }
  .badge.warn { background: var(--warn-bg); color: var(--warn); }
  .badge.bad { background: var(--bad-bg); color: var(--bad); }
  .badge.off { background: var(--idle-bg); color: var(--muted); }
  .badge.idle { background: var(--idle-bg); color: var(--muted); }

  .empty { text-align: center; padding: 3rem 1rem; background: var(--surface); border: 1px dashed var(--border); border-radius: .6rem; }

  .card { background: var(--surface); border: 1px solid var(--border); border-radius: .6rem; padding: 1.25rem; }
  .card h2 { font-size: 1rem; margin: 0 0 .35rem; }
  .card > p { margin-top: 0; font-size: .85rem; }

  .form { display: flex; align-items: flex-end; gap: 1rem; flex-wrap: wrap; margin-top: 1rem; }
  label { display: flex; flex-direction: column; gap: .3rem; font-size: .82rem; }
  label > span { color: var(--muted); }
  .stack { margin-top: .9rem; }
  select, input, textarea {
    font: inherit; color: var(--text); background: var(--bg);
    border: 1px solid var(--border); border-radius: .45rem; padding: .5rem .6rem;
  }
  textarea { width: 100%; box-sizing: border-box; font-family: ui-monospace, monospace; font-size: .8rem; resize: vertical; }
  input { min-width: 16rem; }
  select { min-width: 9rem; }

  button.primary {
    font: inherit; cursor: pointer;
    background: var(--accent); color: #fff; border: none;
    border-radius: .45rem; padding: .55rem 1.1rem; font-weight: 600;
  }
  button.primary:disabled { opacity: .6; cursor: default; }
  button.link { background: none; border: none; color: var(--accent); cursor: pointer; font: inherit; padding: 0; }
  button.link.danger { color: var(--bad); }

  .segmented { display: inline-flex; margin: .9rem 0 0; border: 1px solid var(--border); border-radius: .5rem; overflow: hidden; }
  .segmented button {
    font: inherit; cursor: pointer; background: var(--surface); color: var(--muted);
    border: none; padding: .45rem .85rem; border-right: 1px solid var(--border);
  }
  .segmented button:last-child { border-right: none; }
  .segmented button.active { background: var(--accent); color: #fff; }

  .msg { margin-top: .9rem; font-size: .85rem; }
  .msg.ok { color: var(--ok); }
  .msg.error { color: var(--bad); }
  .skips { margin: .6rem 0 0; padding-left: 1.2rem; color: var(--warn); font-size: .8rem; }

  .auth-wait, .auth-done { margin-top: 1rem; display: flex; flex-direction: column; gap: .5rem; align-items: flex-start; }
  .spin::after {
    content: ''; display: inline-block; width: .6rem; height: .6rem; margin-left: .5rem;
    border: 2px solid var(--border); border-top-color: var(--accent); border-radius: 50%;
    animation: rot .8s linear infinite; vertical-align: middle;
  }
  @keyframes rot { to { transform: rotate(360deg); } }

  code { background: var(--idle-bg); border-radius: .25rem; padding: .05rem .3rem; font-size: .8em; }
  footer { margin-top: 2rem; text-align: center; }

  /* ── task center ─────────────────────────────────────────────────────── */
  .column { display: flex; flex-direction: column; gap: 1rem; }
  .rowhead { display: flex; align-items: baseline; justify-content: space-between; gap: 1rem; flex-wrap: wrap; margin-bottom: .6rem; }
  .rowhead h2 { margin: 0; }
  .spacer { flex: 1 1 auto; }
  .pad-sm { padding: .75rem 0; }

  .ops { display: flex; align-items: center; flex-wrap: wrap; gap: .5rem; }
  .ops button {
    font: inherit; cursor: pointer;
    background: var(--surface); color: var(--text);
    border: 1px solid var(--border); border-radius: .45rem;
    padding: .45rem .85rem;
  }
  .ops button:disabled { opacity: .55; cursor: default; }

  .qtable, .ttable { border: 1px solid var(--border); border-radius: .5rem; overflow: hidden; margin-top: .5rem; }
  .qrow {
    display: grid; grid-template-columns: 2.4fr 1fr 1fr;
    gap: .6rem; align-items: center;
    padding: .5rem .75rem; border-top: 1px solid var(--border); font-size: .82rem;
  }
  .qrow:first-child { border-top: none; }
  .trow {
    display: grid; grid-template-columns: 2.2fr 1fr 1fr 1.5fr 5rem;
    gap: .6rem; align-items: center;
    padding: .5rem .75rem; border-top: 1px solid var(--border); font-size: .82rem;
  }
  .trow:first-child { border-top: none; }
  .qhead, .trow.thead {
    background: var(--bg); color: var(--muted);
    font-size: .7rem; text-transform: uppercase; letter-spacing: .04em;
  }
  .right.actions { display: flex; gap: .6rem; justify-content: flex-end; }
  button.link.tiny { font-size: .76rem; margin-top: .6rem; }

  .acc-card { padding: .9rem 1rem; margin-bottom: .6rem; }
  .acc-head { display: flex; align-items: center; flex-wrap: wrap; gap: .5rem; }
  button.link.caret { font-weight: 600; color: var(--text); }

  .results { margin: .75rem 0 0; padding-left: 1.1rem; font-size: .78rem; color: var(--muted); }
  .results li.error { color: var(--bad); }
  .results li.done { color: var(--ok); }

  .catalog h3 { display: flex; align-items: center; gap: .45rem; font-size: .85rem; margin: .9rem 0 .35rem; }
  .catalog ul { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: .3rem; }
  .catalog li { display: flex; align-items: center; gap: .5rem; flex-wrap: wrap; font-size: .8rem; }
  details summary { cursor: pointer; font-size: .9rem; font-weight: 600; }
  details[open] summary { margin-bottom: .4rem; }

  /* ── request log ─────────────────────────────────────────────────────── */
  .stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(7rem, 1fr)); gap: .6rem; }
  .stat-card {
    display: flex; flex-direction: column; gap: .2rem;
    background: var(--surface); border: 1px solid var(--border);
    border-radius: .5rem; padding: .7rem .85rem;
  }
  .stat-card .k { font-size: .72rem; color: var(--muted); text-transform: uppercase; letter-spacing: .04em; }
  .stat-card .v { font-size: 1.15rem; font-weight: 700; font-variant-numeric: tabular-nums; }
  .stat-card .v em { font-style: normal; font-size: .7rem; font-weight: 500; color: var(--muted); margin-left: .15rem; }

  .row.logrow { grid-template-columns: 1fr 1.7fr 1.2fr 1.1fr .7fr 1.3fr .8fr; }
  .row.loghead { background: var(--bg); color: var(--muted); font-size: .7rem; text-transform: uppercase; letter-spacing: .04em; }
</style>
