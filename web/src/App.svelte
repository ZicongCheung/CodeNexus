<script>
  // CodeNexus console. Sources are a first-class concept: every account carries
  // the upstream it belongs to, and the whole UI is grouped around that.
  import Settings from './Settings.svelte'
  import Icon from './ui/Icon.svelte'
  import ToastHost from './ui/ToastHost.svelte'
  import ConfirmHost from './ui/ConfirmHost.svelte'
  import { toastOk, toastErr, pushToast } from './ui/toast.svelte.js'
  import { ask } from './ui/confirm.svelte.js'
  import { t } from './strings.js'

  const API = '/panel/api'

  let session = $state(null)
  let accounts = $state([])
  let loading = $state(true)
  let banner = $state('')

  let tab = $state('overview')
  let scrolled = $state(false)

  // ── theme ───────────────────────────────────────────────────────────────
  // The console follows the system by default but remembers an explicit choice,
  // because "I want the light one" should not be undone by the clock.
  const THEME_KEY = 'codenexus-theme'
  let theme = $state(localStorage.getItem(THEME_KEY) || 'system')
  let systemDark = $state(window.matchMedia('(prefers-color-scheme: dark)').matches)

  const media = window.matchMedia('(prefers-color-scheme: dark)')
  media.addEventListener('change', (e) => (systemDark = e.matches))

  let effectiveTheme = $derived(theme === 'system' ? (systemDark ? 'dark' : 'light') : theme)

  $effect(() => {
    document.documentElement.dataset.theme = effectiveTheme
  })

  function setTheme(next) {
    theme = next
    if (next === 'system') localStorage.removeItem(THEME_KEY)
    else localStorage.setItem(THEME_KEY, next)
  }

  // ── onboarding (device authorization) ────────────────────────────────────
  let addMode = $state('device') // device | manual | local
  let authSource = $state('workbuddy')
  let authRealm = $state('cn')
  let authState = $state('')
  let authUrl = $state('')
  let authPhase = $state('idle') // idle | starting | waiting | done | error
  let authMsg = $state('')
  let pollTimer = null

  // ── onboarding (import from this machine) ────────────────────────────────
  let localSource = $state('')
  let localPhase = $state('idle') // idle | running | done | error
  let localMsg = $state('')

  // ── onboarding (pasted token) ────────────────────────────────────────────
  let tokenSource = $state('')
  let tokenValue = $state('')
  let tokenRefresh = $state('')
  let tokenUid = $state('')
  let tokenLabel = $state('')
  let tokenPhase = $state('idle') // idle | running | done | error
  let tokenMsg = $state('')

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
  let queue = $state([])
  let queueBusy = $state(false)
  let results = $state({}) // uid -> last growth run

  // ── request log ──────────────────────────────────────────────────────────
  let metrics = $state(null)
  let logEvents = $state([])

  // ── persisted usage history ─────────────────────────────────────────────
  let usageDays = $state(30)
  let usageAvailable = $state(null)
  let usageSummary = $state(null)
  let usageModels = $state([])
  let usageDaily = $state([])
  let usageRecords = $state([])
  let usageFilter = $state({ wire: '', model: '', outcome: '' })
  let usageLoading = $state(false)
  let usageMsg = $state('')
  let logTimer = null

  // Only offer a flow the backend can actually serve: a source that cannot run
  // an interactive login must not appear in the device picker, and vice versa.
  const deviceSources = $derived(session?.device_sources ?? [])
  const manualSources = $derived(session?.manual_sources ?? [])
  const localSources = $derived(session?.local_sources ?? [])

  const manualSource = $derived(
    manualSources.includes(tokenSource) ? tokenSource : manualSources[0] ?? '',
  )
  const deviceSource = $derived(
    deviceSources.includes(authSource) ? authSource : deviceSources[0] ?? '',
  )
  const localTarget = $derived(
    localSources.includes(localSource) ? localSource : localSources[0] ?? '',
  )
  const tokenPlaceholder = $derived(
    manualSource === 'trae-cn' ? 'Cloud-IDE-JWT…' : t.add.token.placeholder,
  )
  const modeHint = $derived(manualSource === 'trae-cn' ? 'Cloud-IDE-JWT' : '')

  const inlinePlaceholder =
    '[{"source":"workbuddy","uid":"...","access_token":"...","refresh_token":"..."}]'

  const tabs = [
    { id: 'overview', label: t.nav.overview, icon: 'overview' },
    { id: 'accounts', label: t.nav.accounts, icon: 'accounts' },
    { id: 'tasks', label: t.nav.tasks, icon: 'tasks' },
    { id: 'logs', label: t.nav.logs, icon: 'logs' },
  ]
  const dataTabs = [
    { id: 'models', label: t.nav.models, icon: 'models' },
    { id: 'credits', label: t.nav.credits, icon: 'credits' },
    { id: 'usage', label: t.nav.usage, icon: 'usage' },
  ]
  const accessTabs = [
    { id: 'add', label: t.nav.add, icon: 'add' },
    { id: 'import', label: t.nav.import, icon: 'importAcc' },
  ]
  const themeOptions = [
    { value: 'system', label: t.nav.theme.system, icon: 'monitor' },
    { value: 'light', label: t.nav.theme.light, icon: 'sun' },
    { value: 'dark', label: t.nav.theme.dark, icon: 'moon' },
  ]

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
    if (!acc.enabled) return { cls: 'off', text: t.health.off }
    if (acc.broken) return { cls: 'bad', text: t.health.bad }
    if (acc.cooling) return { cls: 'warn', text: t.health.warn }
    if (acc.healthy) return { cls: 'ok', text: t.health.ok }
    return { cls: 'idle', text: t.health.idle }
  }

  function fmtTime(sec) {
    if (!sec) return t.common.dash
    const d = new Date(sec * 1000)
    return d.toLocaleString()
  }

  async function removeAccount(acc) {
    const label = acc.label || acc.uid
    const ok = await ask({
      title: t.common.remove,
      body: t.accounts.removeConfirm(label, acc.source),
      detail: acc.uid,
      confirmLabel: t.common.remove,
      danger: true,
    })
    if (!ok) return
    try {
      await api('/accounts/remove', {
        method: 'POST',
        body: JSON.stringify({ key: acc.key }),
      })
      await loadAccounts()
      await loadSession()
      toastOk(t.accounts.removeDone(label))
    } catch (err) {
      toastErr(err.message)
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
        body: JSON.stringify({ source: deviceSource, realm: authRealm }),
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
        authMsg = t.add.device.added(body.label || body.uid)
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

  async function submitToken() {
    if (!tokenValue.trim() && !tokenRefresh.trim()) {
      tokenPhase = 'error'
      tokenMsg = t.add.token.needToken
      return
    }
    tokenPhase = 'running'
    tokenMsg = ''
    try {
      const body = await api('/auth/token', {
        method: 'POST',
        body: JSON.stringify({
          source: manualSource,
          token: tokenValue.trim(),
          refresh_token: tokenRefresh.trim(),
          uid: tokenUid.trim(),
          label: tokenLabel.trim(),
        }),
      })
      tokenPhase = 'done'
      tokenMsg = t.add.token.added(body.label || body.uid)
      tokenValue = ''
      tokenRefresh = ''
      tokenUid = ''
      tokenLabel = ''
      await refresh()
    } catch (err) {
      tokenPhase = 'error'
      tokenMsg = err.message
    }
  }

  // ── onboarding (import from this machine) ────────────────────────────────
  async function importLocal() {
    localPhase = 'running'
    localMsg = ''
    try {
      const body = await api('/auth/local', {
        method: 'POST',
        body: JSON.stringify({ source: localTarget }),
      })
      const imported = body.imported ?? []
      localPhase = 'done'
      localMsg = imported.length
        ? t.add.local.imported(
            imported.length,
            imported.map((a) => a.label || a.uid).join('、'),
          )
        : t.add.local.none
      await refresh()
    } catch (err) {
      localPhase = 'error'
      localMsg = err.message
    }
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
        importMsg = t.importAcc.needJSON
        return
      }
      let parsed
      try {
        parsed = JSON.parse(importText)
      } catch (err) {
        importPhase = 'error'
        importMsg = t.importAcc.jsonBad(err.message)
        return
      }
      payload = { accounts: Array.isArray(parsed) ? parsed : parsed.accounts || [parsed] }
    } else if (importMode === 'path') {
      if (!importPath.trim()) {
        importPhase = 'error'
        importMsg = t.importAcc.needPath
        return
      }
      payload = { path: importPath.trim() }
    } else {
      if (!importBundle.trim() || !importPass) {
        importPhase = 'error'
        importMsg = t.importAcc.needBundle
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
      importMsg = t.importAcc.done(body.imported, body.total)
      await refresh()
    } catch (err) {
      importPhase = 'error'
      importMsg = err.message
    }
  }

  function switchTab(name) {
    if (tab !== name) window.scrollTo({ top: 0 })
    tab = name
    if (name !== 'add') {
      resetAuth()
      tokenPhase = 'idle'
      tokenMsg = ''
    }
    stopLogPolling()
    if (name === 'tasks') {
      if (!catalog.length) loadCatalog()
      loadQueue()
    } else if (name === 'logs') {
      startLogPolling()
    } else if (name === 'usage') {
      loadUsage()
    } else if (name === 'overview') {
      loadOverview()
    } else if (name === 'models') {
      if (!modelRows.length) loadModels()
    } else if (name === 'credits') {
      if (!creditRows.length) loadCredits()
    }
  }

  // ── overview ─────────────────────────────────────────────────────────────
  // The overview reads only what the gateway already knows: pool state, the
  // metrics window and the task queue. It never probes upstreams, so landing
  // on it costs nothing and cannot rate-limit an account.
  let overviewLoading = $state(false)
  let overviewMetrics = $state(null)
  let overviewQueue = $state([])
  let overviewUsage = $state(null)

  async function loadOverview() {
    overviewLoading = true
    const jobs = [
      loadAccounts(),
      api('/observability/metrics').then((m) => (overviewMetrics = m)).catch(() => (overviewMetrics = null)),
      api('/tasks/queue').then((q) => (overviewQueue = q.queue ?? [])).catch(() => (overviewQueue = [])),
      api('/usage/summary?days=1')
        .then((u) => (overviewUsage = u.available === false ? null : (u.summary ?? null)))
        .catch(() => (overviewUsage = null)),
    ]
    await Promise.all(jobs)
    overviewLoading = false
  }

  const healthCounts = $derived.by(() => {
    const c = { ok: 0, cooling: 0, broken: 0, off: 0, idle: 0, inFlight: 0 }
    for (const acc of accounts) {
      if (!acc.enabled) c.off++
      else if (acc.broken) c.broken++
      else if (acc.cooling) c.cooling++
      else if (acc.healthy) c.ok++
      else c.idle++
      c.inFlight += acc.in_flight || 0
    }
    return c
  })
  const creditsTotal = $derived(
    accounts.reduce((sum, a) => sum + (a.unlimited ? 0 : a.credits || 0), 0),
  )
  const hasUnlimited = $derived(accounts.some((a) => a.unlimited))

  // Accounts whose balance has been probed and that carry credits lapsing
  // inside the sweep window. An account never probed has no earliest date, and
  // the box says so instead of implying "nothing expires".
  const expiringSoon = $derived.by(() => {
    const windowMs = (session?.expiring_window_hours ?? 72) * 3600 * 1000
    const now = Date.now()
    return accounts
      .filter((a) => a.credits_known && (a.expiring > 0 || (a.earliest_expiry && a.earliest_expiry * 1000 - now < windowMs)))
      .sort((a, b) => (a.earliest_expiry || Infinity) - (b.earliest_expiry || Infinity))
  })
  const creditsProbed = $derived(accounts.some((a) => a.credits_known))

  const queuePending = $derived(
    overviewQueue.reduce((sum, row) => sum + (row.pending || 0), 0),
  )

  // ── models page ──────────────────────────────────────────────────────────
  let modelRows = $state([])
  let modelFailed = $state({})
  let modelsLoading = $state(false)
  let modelsQuery = $state('')
  let modelsCap = $state('')

  async function loadModels() {
    modelsLoading = true
    try {
      const body = await api('/models')
      modelRows = body.models ?? []
      modelFailed = body.failed ?? {}
    } catch (err) {
      banner = err.message
    } finally {
      modelsLoading = false
    }
  }

  const modelAccounts = $derived.by(() => {
    const map = new Map()
    for (const row of modelRows) {
      if (!map.has(row.uid)) map.set(row.uid, { uid: row.uid, label: row.label || row.uid, source: row.source, items: [] })
      map.get(row.uid).items.push(row)
    }
    return [...map.values()]
  })

  const filteredModels = $derived.by(() => {
    const q = modelsQuery.trim().toLowerCase()
    const groups = modelAccounts.map((g) => {
      let items = g.items
      if (q) items = items.filter((m) => m.id.toLowerCase().includes(q))
      if (modelsCap) items = items.filter((m) => (m.capabilities ?? []).includes(modelsCap) || (modelsCap === 'reasoning' && (m.thinking_efforts ?? []).length > 0))
      return { ...g, items }
    })
    return groups.filter((g) => g.items.length > 0)
  })

  // ── credits page ─────────────────────────────────────────────────────────
  let creditRows = $state([])
  let creditsLoading = $state(false)
  let creditsErr = $state('')

  async function loadCredits() {
    creditsLoading = true
    creditsErr = ''
    try {
      const body = await api('/credits')
      creditRows = body.accounts ?? []
    } catch (err) {
      creditsErr = err.message
    } finally {
      creditsLoading = false
    }
  }

  // Group one account's packs by lapse day. Packs without a date cannot sit on
  // an axis, so they fall into a separate "undated" bucket that the view
  // renders after the dated ones instead of inventing a day for them.
  function packsByDay(row) {
    const days = new Map()
    const undated = []
    for (const p of row.packs ?? []) {
      if (!p.expires_at) {
        undated.push(p)
        continue
      }
      const d = new Date(p.expires_at * 1000)
      const key = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
      if (!days.has(key)) days.set(key, 0)
      days.set(key, days.get(key) + (p.remain || 0))
    }
    return { days: [...days.entries()], undated }
  }

  function fmtDay(key) {
    const [y, m, d] = key.split('-')
    return `${m}-${d}`
  }

  // Bar width relative to the largest single day of that account, so one
  // account's chart never implies a global scale.
  function expiryBarWidth(row, remain) {
    let peak = 0
    for (const [, v] of packsByDay(row).days) peak = Math.max(peak, v)
    if (peak <= 0) return 0
    return Math.max(2, Math.round((remain / peak) * 100))
  }

  // ── task center ──────────────────────────────────────────────────────────
  const classLabels = t.tasks.catalog.classes
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
  // Failures surface as a toast, because a sweep that partially failed is not
  // something to leave sitting in a corner of one card.
  async function guard(key, fn) {
    opBusy = key
    try {
      await fn()
    } catch (err) {
      toastErr(err.message)
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
      toastOk(code ? t.tasks.msg.accepted(code) : t.tasks.msg.acceptedAll)
      await loadAccountTasks(acc.uid)
    })
  }

  async function claimTask(acc, code) {
    await guard(`${acc.uid}:claim:${code}`, async () => {
      const body = await api(`/accounts/${encodeURIComponent(acc.uid)}/tasks/claim`, {
        method: 'POST',
        body: JSON.stringify({ task_code: code }),
      })
      toastOk(
        t.tasks.msg.claimed(code, body.credit) +
          (body.energy ? ` +${body.energy} 能量` : ''),
      )
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
      toastOk(describeResult(res))
      await loadAccountTasks(acc.uid)
      await loadAccounts()
    })
  }

  async function runAllTasks(acc) {
    await guard(`${acc.uid}:auto`, async () => {
      const body = await api(`/accounts/${encodeURIComponent(acc.uid)}/tasks/auto_all`, { method: 'POST' })
      results = { ...results, [acc.uid]: body.results ?? [] }
      toastOk(summarize(body.results ?? []))
      await loadAccountTasks(acc.uid)
      await loadAccounts()
    })
  }

  async function accountOp(acc, op, label) {
    await guard(`${acc.uid}:${op}`, async () => {
      const body = await api(`/accounts/${encodeURIComponent(acc.uid)}/${op}`, { method: 'POST' })
      const who = acc.label || acc.uid
      if (op === 'checkin') {
        // The sweep logs a failure and moves on, so report what happened: a
        // rate-limited check-in stays open and the automatic retry picks it up.
        if (body.checkin_done) toastOk(t.tasks.msg.checkinDone(who))
        else pushToast(t.tasks.msg.checkinRetry(who), 'warn')
      } else {
        toastOk(t.tasks.msg.opDone(who, label))
      }
      await loadAccounts()
      if (expanded[acc.uid]) await loadAccountTasks(acc.uid)
    })
  }

  async function batch(op, label) {
    await guard(op, async () => {
      await api(`/batch/${op}`, { method: 'POST' })
      // A batch sweep reports nothing per account; the honest summary names the
      // automatic retry instead of claiming every account succeeded.
      if (op === 'checkin_all') pushToast(t.tasks.msg.batchCheckin, 'warn')
      else toastOk(t.tasks.msg.batchDone(label))
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
      toastErr(err.message)
    } finally {
      queueBusy = false
    }
  }

  async function runQueue() {
    queueBusy = true
    try {
      const body = await api('/tasks/run_queue', { method: 'POST' })
      for (const row of body.accounts ?? []) {
        results = { ...results, [row.uid]: row.results ?? [] }
      }
      toastOk(t.tasks.msg.queueDone)
      await loadQueue()
      await reloadExpanded()
      await loadAccounts()
    } catch (err) {
      toastErr(err.message)
    } finally {
      queueBusy = false
    }
  }

  function describeResult(r) {
    const parts = [`${r.task_code}：${r.status}`]
    if (r.progress_before || r.progress_after) {
      parts.push(`${r.progress_before || t.common.dash} → ${r.progress_after || t.common.dash}`)
    }
    if (r.claimed) parts.push(t.tasks.result.claimed(r.credit))
    if (r.message) parts.push(r.message)
    return parts.join(' · ')
  }

  function summarize(list) {
    if (!list.length) return t.tasks.result.none
    const done = list.filter((r) => r.status === 'done').length
    const skipped = list.filter((r) => r.status === 'skipped').length
    const failed = list.filter((r) => r.status === 'error').length
    const credit = list.reduce((n, r) => n + (r.credit || 0), 0)
    return t.tasks.result.summary(list.length, done, skipped, failed, credit)
  }

  function taskState(task) {
    const st = t.tasks.state
    if (task.claimed) return { cls: 'off', text: st.claimed }
    if (task.locked) return { cls: 'idle', text: st.locked }
    const cur = Number(task.current || 0)
    const target = Number(task.target || 0)
    if (task.claimable || (target > 0 && cur >= target)) return { cls: 'ok', text: st.claimable }
    if (task.accept_status === 'completed') return { cls: 'ok', text: st.completed }
    if (cur > 0) return { cls: 'warn', text: st.inProgress }
    return { cls: 'idle', text: st.pending }
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
      banner = err.message
    }
  }

  async function loadUsage() {
    usageLoading = true
    usageMsg = ''
    try {
      const days = usageDays
      const [sum, daily] = await Promise.all([
        api(`/usage/summary?days=${days}`),
        api(`/usage/daily?days=${days}`),
      ])
      usageAvailable = sum.available !== false
      usageSummary = sum.summary ?? null
      usageModels = sum.models ?? []
      usageDaily = daily.daily ?? []
      await loadUsageRecords()
    } catch (err) {
      usageMsg = err.message
    } finally {
      usageLoading = false
    }
  }

  async function loadUsageRecords() {
    const params = new URLSearchParams({ limit: '300' })
    for (const [k, v] of Object.entries(usageFilter)) {
      if (v) params.set(k, v)
    }
    const rec = await api(`/usage/records?${params}`)
    usageAvailable = rec.available !== false
    usageRecords = rec.records ?? []
  }

  function changeUsageDays(d) {
    usageDays = d
    loadUsage()
  }

  function usageBarHeight(row) {
    if (!usageDaily.length) return 0
    const peak = Math.max(...usageDaily.map((r) => r.total_tokens || 0), 1)
    return Math.max(2, Math.round(((row.total_tokens || 0) / peak) * 100))
  }

  function fmtTokens(n) {
    n = Number(n || 0)
    if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M'
    if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k'
    return String(n)
  }

  function fmtCredits(n) {
    return Number(n || 0).toFixed(2)
  }

  function fmtDateTime(value) {
    if (!value) return t.common.dash
    const d = new Date(value)
    if (Number.isNaN(d.getTime())) return t.common.dash
    return d.toLocaleString()
  }

  function fmtClock(value) {
    if (!value) return t.common.dash
    const d = new Date(value)
    if (Number.isNaN(d.getTime())) return t.common.dash
    return d.toLocaleTimeString()
  }

  // Where a charge came from. An unsettled turn never reads as a free one, so
  // "待结算" is shown instead of 0.
  function creditTitle(e) {
    if (!e.credits_source) return t.logs.creditNone
    return t.logs.creditSources[e.credits_source] || e.credits_source
  }

  function surfaceLabel(e) {
    return t.logs.surface[e.surface] || t.logs.surface.unknown
  }
</script>

<svelte:head>
  <title>{t.brand.title}</title>
</svelte:head>

<svelte:window onscroll={() => (scrolled = window.scrollY > 4)} />

<div class="shell">
  <header class="topbar" class:scrolled>
    <div class="topbar-inner">
      <div class="brand">
        <span class="logo" aria-hidden="true">{'{ }'}</span>
        <div class="brand-text">
          <span class="brand-name">{t.brand.name}</span>
          <span class="brand-sub">{t.brand.tagline}</span>
        </div>
      </div>

      <nav class="tabs" aria-label={t.nav.aria}>
        {#each tabs as item (item.id)}
          <button
            class="tab"
            data-tab={item.id}
            class:active={tab === item.id}
            aria-current={tab === item.id ? 'page' : undefined}
            onclick={() => switchTab(item.id)}
          >
            <Icon name={item.icon} size={16} />
            <span>{item.label}</span>
          </button>
        {/each}
        <span class="tab-sep" aria-hidden="true"></span>
        {#each dataTabs as item (item.id)}
          <button
            class="tab"
            data-tab={item.id}
            class:active={tab === item.id}
            aria-current={tab === item.id ? 'page' : undefined}
            onclick={() => switchTab(item.id)}
          >
            <Icon name={item.icon} size={16} />
            <span>{item.label}</span>
          </button>
        {/each}
        <span class="tab-sep" aria-hidden="true"></span>
        {#each accessTabs as item (item.id)}
          <button
            class="tab"
            data-tab={item.id}
            class:active={tab === item.id}
            aria-current={tab === item.id ? 'page' : undefined}
            onclick={() => switchTab(item.id)}
          >
            <Icon name={item.icon} size={16} />
            <span>{item.label}</span>
          </button>
        {/each}
        <button
          class="tab"
          data-tab="settings"
          class:active={tab === 'settings'}
          aria-current={tab === 'settings' ? 'page' : undefined}
          onclick={() => switchTab('settings')}
        >
          <Icon name="settings" size={16} />
          <span>{t.nav.settings}</span>
        </button>
      </nav>

      <div class="topbar-right">
        <span class="stat">
          <span class="count">{accounts.length}</span>
          <span class="muted small">{t.nav.accountsUnit}</span>
          {#if session}
            <span class="dot" title={session.service}></span>
          {/if}
        </span>
        <div class="segmented theme-switch" role="group" aria-label={t.nav.appearance}>
          {#each themeOptions as opt (opt.value)}
            <button
              class="theme-opt"
              class:active={theme === opt.value}
              aria-pressed={theme === opt.value}
              title={opt.label}
              onclick={() => setTheme(opt.value)}
            >
              <Icon name={opt.icon} size={15} />
              <span class="theme-label">{opt.label}</span>
            </button>
          {/each}
        </div>
      </div>
    </div>
  </header>

  <main class="page">
    {#if banner}
      <div class="note error banner">
        <Icon name="alert" size={16} />
        <span class="spacer">{banner}</span>
        <button class="btn icon" onclick={() => (banner = '')} aria-label={t.common.close}>
          <Icon name="close" size={14} />
        </button>
      </div>
    {/if}

    {#if tab === 'overview'}
      <div class="page-head">
        <div class="titles">
          <h1>{t.nav.overview}</h1>
          <p class="lede">{t.lede.overview}</p>
        </div>
        <div class="btn-row">
          <button class="btn ghost" onclick={loadOverview} disabled={overviewLoading}>
            <Icon name="refresh" size={15} />
            {t.overview.refresh}
          </button>
        </div>
      </div>

      <div class="stats">
        <div class="stat-card"><span class="k">{t.overview.stat.accounts}</span><span class="v">{accounts.length}</span></div>
        <div class="stat-card"><span class="k">{t.overview.stat.available}</span><span class="v">{healthCounts.ok}</span></div>
        <div class="stat-card"><span class="k">{t.overview.stat.cooling}</span><span class="v" class:warn-text={healthCounts.cooling > 0}>{healthCounts.cooling}</span></div>
        <div class="stat-card"><span class="k">{t.overview.stat.broken}</span><span class="v" class:bad-text={healthCounts.broken > 0}>{healthCounts.broken}</span></div>
        <div class="stat-card"><span class="k">{t.overview.stat.inFlight}</span><span class="v">{healthCounts.inFlight}</span></div>
        <div class="stat-card"><span class="k">{t.overview.stat.credits}</span><span class="v">{creditsTotal > 0 || hasUnlimited ? (hasUnlimited ? '∞' : fmtCredits(creditsTotal)) : t.common.dash}</span></div>
        <div class="stat-card"><span class="k">{t.overview.stat.requestsToday}</span><span class="v">{overviewUsage ? overviewUsage.requests : t.common.dash}</span></div>
        <div class="stat-card"><span class="k">{t.overview.stat.queuePending}</span><span class="v">{queuePending}</span></div>
      </div>

      <div class="ov-grid">
        <section class="card">
          <div class="card-head">
            <div class="titles">
              <h2>{t.overview.healthBox.title}</h2>
              {#if accounts.length}<span class="hint">{t.overview.healthBox.detail(healthCounts.ok + healthCounts.idle, accounts.length)}</span>{/if}
            </div>
          </div>
          {#if accounts.length === 0}
            <div class="empty slim"><p>{t.overview.healthBox.empty}</p></div>
          {:else}
            <div class="ov-chips">
              {#each accounts as acc (acc.key)}
                {@const h = health(acc)}
                <button class="ov-chip" onclick={() => switchTab('accounts')} title="{acc.uid} · {h.text}">
                  <span class="badge {h.cls}">{h.text}</span>
                  <span class="ov-chip-name">{acc.label || acc.uid}</span>
                  {#if acc.in_flight > 0}<span class="badge info">{acc.in_flight}</span>{/if}
                </button>
              {/each}
            </div>
          {/if}
        </section>

        <section class="card">
          <div class="card-head">
            <div class="titles">
              <h2>{t.overview.expiryBox.title}</h2>
              {#if session}<span class="hint">{t.overview.expiryBox.window(session.expiring_window_hours ?? 72)}</span>{/if}
            </div>
          </div>
          {#if !creditsProbed}
            <div class="empty slim"><p>{t.overview.expiryBox.stale}</p></div>
          {:else if expiringSoon.length === 0}
            <div class="empty slim"><p>{t.overview.expiryBox.empty}</p></div>
          {:else}
            <div class="ov-rows">
              {#each expiringSoon as acc (acc.key)}
                <div class="ov-row">
                  <span class="label">{acc.label || acc.uid}<em>{acc.uid}</em></span>
                  {#if acc.expiring > 0}
                    <span class="badge warn">{t.overview.expiryBox.earliest(fmtTime(acc.earliest_expiry), acc.expiring)}</span>
                  {:else}
                    <span class="badge ok">{fmtTime(acc.earliest_expiry)}</span>
                  {/if}
                </div>
              {/each}
            </div>
          {/if}
        </section>

        <section class="card">
          <div class="card-head">
            <div class="titles"><h2>{t.overview.serviceBox.title}</h2></div>
          </div>
          <div class="kv">
            <span class="k">{t.overview.serviceBox.sources}</span>
            <span class="v">{(session?.sources ?? []).join(' · ') || t.common.dash}</span>
            <span class="k">{t.overview.serviceBox.sticky}</span>
            <span class="v">{session?.sticky_sessions ?? t.common.dash}</span>
            <span class="k">{t.overview.serviceBox.reqlog}</span>
            <span class="v"><span class="badge {overviewMetrics?.enabled ? 'ok' : 'off'}">{overviewMetrics?.enabled ? t.overview.serviceBox.on : t.overview.serviceBox.off}</span></span>
            <span class="k">{t.overview.serviceBox.usage}</span>
            <span class="v"><span class="badge {overviewUsage ? 'ok' : 'off'}">{overviewUsage ? t.overview.serviceBox.on : t.overview.serviceBox.off}</span></span>
          </div>
        </section>
      </div>
    {/if}

    {#if tab === 'accounts'}
      <div class="page-head">
        <div class="titles">
          <h1>{t.nav.accounts}</h1>
          <p class="lede">{t.lede.accounts}</p>
        </div>
      </div>

      {#if loading}
        <div class="table">
          {#each Array(4) as _}
            <div class="row skeleton-row"><span class="skeleton"></span><span class="skeleton"></span><span class="skeleton"></span></div>
          {/each}
        </div>
      {:else if accounts.length === 0}
        <div class="empty">
          <span class="empty-mark"><Icon name="accounts" size={22} /></span>
          <p>{t.accounts.empty.title}</p>
          <p class="muted">{t.accounts.empty.body}</p>
          <button class="btn primary" onclick={() => switchTab('add')}>
            <Icon name="add" size={16} />
            {t.nav.add}
          </button>
        </div>
      {:else}
        {#each grouped as group (group.source)}
          <section class="group">
            <div class="group-head">
              <h2><span class="src">{group.source}</span></h2>
              <span class="badge outline">{group.items.length}</span>
            </div>
            <div class="table scroll accounts-table">
              <div class="row head" style="--cols: var(--cols-accounts)">
                <span>{t.accounts.col.account}</span>
                <span>{t.accounts.col.status}</span>
                <span>{t.accounts.col.credential}</span>
                <span>{t.accounts.col.calls}</span>
                <span>{t.accounts.col.expires}</span>
                <span class="right">{t.accounts.col.actions}</span>
              </div>
              {#each group.items as acc (acc.key)}
                {@const h = health(acc)}
                <div class="row" style="--cols: var(--cols-accounts)">
                  <span class="label" title={acc.uid}>
                    {acc.label || acc.uid}
                    <em>{acc.uid}</em>
                  </span>
                  <span class="cell-status">
                    <span class="badge {h.cls}">{h.text}</span>
                    {#if acc.in_flight > 0}
                      <span class="badge info">{t.accounts.inFlight(acc.in_flight)}</span>
                    {/if}
                  </span>
                  <span class="muted small">
                    {#if acc.has_refresh_token}{t.accounts.credentialRefreshable}{:else}{t.accounts.credentialAccessOnly}{/if}
                  </span>
                  <span class="muted small mono">{acc.success}✓ / {acc.errors}✗</span>
                  <span class="muted small">{fmtTime(acc.expires_at)}</span>
                  <span class="right">
                    <button class="btn link danger" onclick={() => removeAccount(acc)}>{t.common.remove}</button>
                  </span>
                </div>
              {/each}
            </div>
          </section>
        {/each}
      {/if}
    {/if}

    {#if tab === 'tasks'}
      <div class="page-head">
        <div class="titles">
          <h1>{t.nav.tasks}</h1>
          <p class="lede">{t.lede.tasks}</p>
        </div>
      </div>

      <div class="column">
        <div class="card">
          <div class="card-head">
            <div class="titles">
              <h2>{t.tasks.batch.title}</h2>
            </div>
            <div class="btn-row">
              <button class="btn ghost" onclick={loadQueue} disabled={queueBusy}>
                <Icon name="refresh" size={15} />
                {t.tasks.queue.refresh}
              </button>
              <button class="btn primary" onclick={runQueue} disabled={queueBusy}>
                {#if queueBusy}<span class="spinner"></span>{/if}
                {queueBusy ? t.tasks.queue.running : t.tasks.queue.run}
              </button>
            </div>
          </div>
          <div class="btn-row">
            <button class="btn" onclick={() => batch('checkin_all', t.tasks.batch.checkinAll)} disabled={!!opBusy}>
              {t.tasks.batch.checkinAll}
            </button>
            <button class="btn" onclick={() => batch('activity_all', t.tasks.batch.activityAll)} disabled={!!opBusy}>
              {t.tasks.batch.activityAll}
            </button>
            <button class="btn" onclick={() => batch('balance_all', t.tasks.batch.balanceAll)} disabled={!!opBusy}>
              {t.tasks.batch.balanceAll}
            </button>
            <button class="btn" onclick={() => batch('travel_all', t.tasks.batch.travelAll)} disabled={!!opBusy}>
              {t.tasks.batch.travelAll}
            </button>
            <button class="btn" onclick={() => batch('keepalive_all', t.tasks.batch.keepaliveAll)} disabled={!!opBusy}>
              {t.tasks.batch.keepaliveAll}
            </button>
          </div>
          {#if opBusy && !opBusy.includes(':')}
            <p class="msg"><span class="spinner inline"></span> {t.tasks.msg.running(opBusy)}</p>
          {/if}
        </div>

        <div class="card">
          <div class="card-head">
            <div class="titles">
              <h2>{t.tasks.queue.title}</h2>
              <span class="hint">{t.tasks.queue.hint}</span>
            </div>
            {#if queue.length}<span class="badge outline">{queue.length}</span>{/if}
          </div>
          {#if queue.length === 0}
            <p class="muted small">{t.tasks.queue.empty}</p>
          {:else}
            <div class="table">
              <div class="row head" style="--cols: var(--cols-queue)">
                <span>{t.tasks.queue.col.account}</span>
                <span>{t.tasks.queue.col.pending}</span>
                <span>{t.tasks.queue.col.claimable}</span>
              </div>
              {#each queue as row (row.uid)}
                <div class="row" style="--cols: var(--cols-queue)">
                  <span class="truncate">{row.label || row.uid}</span>
                  <span class="mono">{row.pending}</span>
                  <span class="mono">{row.claimable > 0 ? row.claimable : t.common.dash}</span>
                </div>
              {/each}
            </div>
          {/if}
        </div>

        <div class="card">
          <details>
            <summary>{t.tasks.catalog.title(catalog.length)}</summary>
            {#if catalogErr}
              <p class="msg error">{catalogErr}</p>
            {:else}
              {#each catalogGroups as g (g.cls)}
                <div class="catalog">
                  <h3>{g.label}<span class="badge outline">{g.items.length}</span></h3>
                  <ul>
                    {#each g.items as a (a.task_code)}
                      <li>
                        <code>{a.task_code}</code>
                        <span class="muted">{a.desc}</span>
                        {#if a.attempt}<span class="badge warn">{t.tasks.catalog.bestEffort}</span>{/if}
                      </li>
                    {/each}
                  </ul>
                </div>
              {/each}
            {/if}
          </details>
        </div>

        <section class="group">
          <div class="group-head">
            <h2><span class="src">{t.tasks.accounts.title}</span></h2>
            <span class="badge outline">{accounts.length}</span>
          </div>
          {#if accounts.length === 0}
            <div class="empty">
              <span class="empty-mark"><Icon name="tasks" size={22} /></span>
              <p>{t.accounts.empty.title}</p>
              <p class="muted">{t.tasks.accounts.empty}</p>
            </div>
          {/if}
          {#each accounts as acc (acc.key)}
            {@const st = accountTasks[acc.uid] || {}}
            <div class="card acc-card">
              <div class="acc-head">
                {#if acc.local}
                  <span class="acc-name">{acc.label || acc.uid}</span>
                {:else}
                  <button class="btn link strong caret" onclick={() => toggleTasks(acc)} aria-expanded={!!expanded[acc.uid]}>
                    <span class="caret-glyph" class:open={expanded[acc.uid]}>▸</span>
                    {acc.label || acc.uid}
                  </button>
                {/if}
                <span class="badge {health(acc).cls}">{health(acc).text}</span>
                {#if acc.local}<span class="badge idle">{t.tasks.accounts.local}</span>{/if}
                <span class="muted small">{acc.source}</span>
                {#if !acc.local}
                  <span class="muted small">
                    {t.tasks.accounts.credit(acc.credits_known ? acc.credits : t.common.dash)}
                    {#if acc.expiring > 0} · {t.tasks.accounts.expiring(acc.expiring)}{/if}
                  </span>
                {/if}
                <span class="spacer"></span>
                {#if !acc.local}
                  <div class="acc-ops">
                    <button class="btn link" onclick={() => runAllTasks(acc)} disabled={!!opBusy}>{t.tasks.accountOps.runAll}</button>
                    <button class="btn link" onclick={() => acceptTask(acc, '')} disabled={!!opBusy}>{t.tasks.accountOps.acceptAll}</button>
                    {#if acc.checkin_applicable}
                      <button
                        class="btn link"
                        onclick={() => accountOp(acc, 'checkin', t.tasks.accountOps.checkin)}
                        disabled={!!opBusy}
                        title={acc.checkin_done ? t.tasks.checkinTitle : ''}
                      >
                        {acc.checkin_done ? t.tasks.accountOps.checkinDone : t.tasks.accountOps.checkin}
                      </button>
                    {/if}
                    <button class="btn link" onclick={() => accountOp(acc, 'balance', t.tasks.batch.balanceAll)} disabled={!!opBusy}>{t.tasks.accountOps.balance}</button>
                    <button class="btn link" onclick={() => accountOp(acc, 'keepalive', t.tasks.accountOps.keepalive)} disabled={!!opBusy}>{t.tasks.accountOps.keepalive}</button>
                    <button
                      class="btn link"
                      onclick={() => accountOp(acc, acc.paused ? 'resume' : 'pause', acc.paused ? t.tasks.accountOps.resume : t.tasks.accountOps.pause)}
                      disabled={!!opBusy}
                    >
                      {acc.paused ? t.tasks.accountOps.resume : t.tasks.accountOps.pause}
                    </button>
                  </div>
                {/if}
              </div>

              {#if opBusy.startsWith(acc.uid + ':')}
                <p class="msg"><span class="spinner inline"></span> {t.tasks.msg.running(opBusy.split(':').slice(1).join(':'))}</p>
              {/if}

              {#if expanded[acc.uid]}
                {#if st.loading}
                  <div class="table task-skeleton">
                    {#each Array(2) as _}
                      <div class="row skeleton-row"><span class="skeleton"></span><span class="skeleton"></span><span class="skeleton"></span></div>
                    {/each}
                  </div>
                {:else if st.error}
                  <p class="msg error">{st.error}</p>
                {:else if !st.tasks || st.tasks.length === 0}
                  <p class="muted small">{t.tasks.noTasks}</p>
                {:else}
                  <div class="table scroll">
                    <div class="row head" style="--cols: var(--cols-tasks)">
                      <span>{t.tasks.table.task}</span>
                      <span>{t.tasks.table.progress}</span>
                      <span>{t.tasks.table.status}</span>
                      <span>{t.tasks.table.reward}</span>
                      <span class="right"></span>
                    </div>
                    {#each st.tasks as task (task.code)}
                      {@const s = taskState(task)}
                      <div class="row" style="--cols: var(--cols-tasks)">
                        <span class="label" title={task.description || task.desc || task.code}>
                          {task.title || task.code}
                          <em>{task.code}{task.mini_program ? ` · ${t.tasks.miniProgram}` : ''}</em>
                        </span>
                        <span class="muted small mono">{task.target > 0 ? `${task.current || 0}/${task.target}` : t.common.dash}</span>
                        <span><span class="badge {s.cls}">{s.text}</span></span>
                        <span class="muted small">
                          {#if task.credit > 0}+{task.credit} {t.usage.stat.credits}{/if}
                          {#if task.energy > 0} +{task.energy} 能量{/if}
                        </span>
                        <span class="right actions">
                          {#if task.claimable && !task.claimed}
                            <button class="btn link" onclick={() => claimTask(acc, task.code)} disabled={!!opBusy}>{t.tasks.actions.claim}</button>
                          {/if}
                          {#if !task.claimed}
                            <button class="btn link" onclick={() => runTask(acc, task.code)} disabled={!!opBusy}>{t.tasks.actions.run}</button>
                          {/if}
                        </span>
                      </div>
                    {/each}
                  </div>
                  <button class="btn link sm" onclick={() => loadAccountTasks(acc.uid)} disabled={!!opBusy}>
                    {t.tasks.actions.reload}
                  </button>
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
        </section>
      </div>
    {/if}

    {#if tab === 'logs'}
      <div class="page-head">
        <div class="titles">
          <h1>{t.nav.logs}</h1>
          <p class="lede">{t.lede.logs}</p>
        </div>
      </div>

      <div class="column">
        {#if !metrics || metrics.enabled === false}
          <div class="card">
            <h2>{t.logs.title}</h2>
            <p>{t.logs.disabled}</p>
          </div>
        {:else}
          <div class="stats">
            <div class="stat-card"><span class="k">{t.logs.metric.inFlight}</span><span class="v">{metrics.in_flight}</span></div>
            <div class="stat-card"><span class="k">{t.logs.metric.completed}</span><span class="v">{metrics.completed}</span></div>
            <div class="stat-card"><span class="k">{t.logs.metric.successRate}</span><span class="v">{metrics.success_rate ?? 0}<em>%</em></span></div>
            <div class="stat-card"><span class="k">{t.logs.metric.avgDuration}</span><span class="v">{Math.round(metrics.avg_duration_ms ?? 0)}<em>ms</em></span></div>
            <div class="stat-card">
              <span class="k">{t.logs.metric.avgTtfb}</span>
              <span class="v">{metrics.avg_ttfb_ms ? metrics.avg_ttfb_ms : t.common.dash}{metrics.avg_ttfb_ms ? 'ms' : ''}</span>
            </div>
          </div>
          <div class="table scroll">
            <div class="row head" style="--cols: var(--cols-logs)">
              <span>{t.logs.col.time}</span>
              <span>{t.logs.col.path}</span>
              <span>{t.logs.col.account}</span>
              <span>{t.logs.col.model}</span>
              <span>{t.logs.col.route}</span>
              <span>{t.logs.col.status}</span>
              <span>{t.logs.col.duration}</span>
              <span>{t.logs.col.token}</span>
              <span>{t.logs.col.credit}</span>
            </div>
            {#if logEvents.length === 0}
              <div class="row"><span class="muted small">{t.logs.empty}</span></div>
            {/if}
            {#each logEvents as e (e.request_id)}
              <div class="row" style="--cols: var(--cols-logs)">
                <span class="muted small mono">{fmtClock(e.time)}</span>
                <span class="mono small truncate">{e.path}</span>
                <span class="muted small truncate">{e.account || t.common.dash}</span>
                <span class="muted small mono truncate">{e.model || t.common.dash}</span>
                <span class="cell-route">
                  {#if e.surface}
                    <span class="badge outline" title={t.logs.surfaceTitle}>{surfaceLabel(e)}</span>
                  {/if}
                  {#if e.max_profile}
                    <span class="badge primary" title={t.logs.maxTitle}>{t.logs.maxBadge(e.max_context)}</span>
                  {/if}
                  {#if e.continued_rounds > 0}
                    <span class="badge info" title={t.logs.continueTitle}>{t.logs.continueBadge(e.continued_rounds)}</span>
                  {/if}
                  {#if !e.surface && !e.max_profile && !e.continued_rounds}
                    <span class="muted small">{t.common.dash}</span>
                  {/if}
                </span>
                <span><span class="badge {e.ok ? 'ok' : 'bad'}">{e.status}</span></span>
                <span class="muted small mono">
                  {e.duration_ms}ms{#if e.ttfb_ms} / {e.ttfb_ms}ms{/if}
                  {#if e.stream} · {t.logs.stream}{/if}
                </span>
                <span class="cell-stack">
                  <span class="mono small">{e.total_tokens || t.common.dash}</span>
                  {#if e.tokens_per_second}<em>{e.tokens_per_second}tok/s</em>{/if}
                </span>
                <span class="muted small" title={creditTitle(e)}>
                  {#if e.credits_source === 'pending'}
                    <span class="badge warn">{t.logs.pending}</span>
                  {:else if e.credits_source}
                    {e.credits_consumed ?? 0}
                  {:else}
                    {t.common.dash}
                  {/if}
                </span>
              </div>
            {/each}
          </div>
        {/if}
      </div>
    {/if}

    {#if tab === 'usage'}
      <div class="page-head">
        <div class="titles">
          <h1>{t.nav.usage}</h1>
          <p class="lede">{t.lede.usage}</p>
        </div>
      </div>

      {#if usageAvailable === false}
        <div class="card">
          <h2>{t.usage.title}</h2>
          <p>{t.usage.disabled}</p>
        </div>
      {:else}
        <div class="column">
          <div class="toolbar">
            <div class="segmented">
              {#each [7, 30, 90, 365] as d}
                <button class:active={usageDays === d} onclick={() => changeUsageDays(d)}>{t.usage.days(d)}</button>
              {/each}
            </div>
            <button class="btn ghost" onclick={loadUsage} disabled={usageLoading}>
              <Icon name="refresh" size={15} />
              {usageLoading ? t.common.refreshing : t.common.refresh}
            </button>
          </div>
          {#if usageMsg}<p class="msg error">{usageMsg}</p>{/if}

          {#if usageSummary}
            <div class="stats">
              <div class="stat-card"><span class="k">{t.usage.stat.requests}</span><span class="v">{usageSummary.requests}</span></div>
              <div class="stat-card"><span class="k">{t.usage.stat.successRate}</span><span class="v">{usageSummary.requests ? Math.round((usageSummary.ok / usageSummary.requests) * 100) : 0}<em>%</em></span></div>
              <div class="stat-card"><span class="k">{t.usage.stat.input}</span><span class="v">{fmtTokens(usageSummary.input_tokens)}</span></div>
              <div class="stat-card"><span class="k">{t.usage.stat.output}</span><span class="v">{fmtTokens(usageSummary.output_tokens)}</span></div>
              <div class="stat-card"><span class="k">{t.usage.stat.total}</span><span class="v">{fmtTokens(usageSummary.total_tokens)}</span></div>
              <div class="stat-card"><span class="k">{t.usage.stat.credits}</span><span class="v">{fmtCredits(usageSummary.credits)}</span></div>
            </div>
          {/if}

          {#if usageDaily.length}
            <div class="card">
              <div class="card-head">
                <div class="titles">
                  <h2>{t.usage.daily.title}</h2>
                  <span class="hint">{t.usage.daily.peak(fmtTokens(Math.max(...usageDaily.map((r) => r.total_tokens || 0))))}</span>
                </div>
              </div>
              <div class="bars" role="img" aria-label={t.usage.aria.daily}>
                {#each usageDaily as row (row.date)}
                  <div class="bar-col" title="{row.date} · {row.total_tokens || 0} tok · {fmtCredits(row.credits)} {t.usage.stat.credits}">
                    <div class="bar" class:zero={!row.total_tokens} style="height:{usageBarHeight(row)}%"></div>
                  </div>
                {/each}
              </div>
            </div>

            <div class="table scroll">
              <div class="row head" style="--cols: var(--cols-daily)">
                <span>{t.usage.dailyCol.date}</span>
                <span>{t.usage.dailyCol.requests}</span>
                <span>{t.usage.dailyCol.ok}</span>
                <span>{t.usage.dailyCol.failed}</span>
                <span>{t.usage.dailyCol.input}</span>
                <span>{t.usage.dailyCol.output}</span>
                <span>{t.usage.dailyCol.credits}</span>
              </div>
              {#each usageDaily.filter((r) => r.requests > 0) as row (row.date)}
                <div class="row" style="--cols: var(--cols-daily)">
                  <span class="mono small">{row.date}</span>
                  <span class="small mono">{row.requests}</span>
                  <span class="small mono">{row.ok}</span>
                  <span class="small mono">{row.failed}</span>
                  <span class="muted small mono">{fmtTokens(row.input_tokens)}</span>
                  <span class="muted small mono">{fmtTokens(row.output_tokens)}</span>
                  <span class="muted small mono">{fmtCredits(row.credits)}</span>
                </div>
              {/each}
            </div>
          {:else}
            <div class="empty"><p>{t.usage.empty}</p></div>
          {/if}

          {#if usageModels.length}
            <div class="card">
              <h2>{t.usage.models.title}</h2>
              <div class="table">
                <div class="row head" style="--cols: var(--cols-models)">
                  <span>{t.usage.modelCol.model}</span>
                  <span>{t.usage.modelCol.requests}</span>
                  <span>{t.usage.modelCol.input}</span>
                  <span>{t.usage.modelCol.output}</span>
                  <span>{t.usage.modelCol.total}</span>
                  <span>{t.usage.modelCol.credits}</span>
                </div>
                {#each usageModels as m (m.model)}
                  <div class="row" style="--cols: var(--cols-models)">
                    <span class="mono small truncate">{m.model || t.common.dash}</span>
                    <span class="small mono">{m.requests}</span>
                    <span class="muted small mono">{fmtTokens(m.input_tokens)}</span>
                    <span class="muted small mono">{fmtTokens(m.output_tokens)}</span>
                    <span class="small mono">{fmtTokens(m.total_tokens)}</span>
                    <span class="muted small mono">{fmtCredits(m.credits)}</span>
                  </div>
                {/each}
              </div>
            </div>
          {/if}

          <div class="card">
            <div class="card-head">
              <div class="titles">
                <h2>{t.usage.records.title}</h2>
              </div>
              <div class="filters">
                <select bind:value={usageFilter.wire} onchange={loadUsageRecords}>
                  <option value="">{t.usage.records.wireAll}</option>
                  <option value="chat">chat</option>
                  <option value="anthropic">anthropic</option>
                  <option value="responses">responses</option>
                </select>
                <input placeholder={t.usage.records.modelPlaceholder} bind:value={usageFilter.model} onchange={loadUsageRecords} />
                <select bind:value={usageFilter.outcome} onchange={loadUsageRecords}>
                  <option value="">{t.usage.records.outcomeAll}</option>
                  <option value="success">{t.usage.records.outcomeSuccess}</option>
                  <option value="http_error">{t.usage.records.outcomeHTTPError}</option>
                </select>
              </div>
            </div>
            <div class="table scroll">
              <div class="row head" style="--cols: var(--cols-records)">
                <span>{t.logs.col.time}</span>
                <span>{t.logs.col.path}</span>
                <span>{t.logs.col.account}</span>
                <span>{t.logs.col.model}</span>
                <span>{t.logs.col.status}</span>
                <span>{t.logs.col.duration}</span>
                <span>{t.logs.col.token}</span>
                <span>{t.logs.col.credit}</span>
              </div>
              {#if usageRecords.length === 0}
                <div class="row"><span class="muted small">{t.usage.records.empty}</span></div>
              {/if}
              {#each usageRecords as rec (rec.request_id)}
                <div class="row" style="--cols: var(--cols-records)">
                  <span class="muted small">{fmtDateTime(rec.time)}</span>
                  <span class="mono small truncate">{rec.wire || t.common.dash}</span>
                  <span class="muted small truncate">{rec.account || t.common.dash}</span>
                  <span class="mono small truncate">{rec.model || t.common.dash}</span>
                  <span><span class="badge {rec.outcome === 'success' ? 'ok' : 'bad'}">{rec.status}</span></span>
                  <span class="muted small mono">{rec.duration_ms}ms{#if rec.stream} · {t.logs.stream}{/if}</span>
                  <span class="muted small mono">{fmtTokens(rec.total_tokens)}</span>
                  <span class="muted small" title={rec.credits_source || ''}>
                    {#if rec.credits_source === 'pending'}<span class="badge warn">{t.logs.pending}</span>
                    {:else if rec.credits_source}{fmtCredits(rec.credits)}
                    {:else}{t.common.dash}{/if}
                  </span>
                </div>
              {/each}
            </div>
          </div>
        </div>
      {/if}
    {/if}

    {#if tab === 'models'}
      <div class="page-head">
        <div class="titles">
          <h1>{t.nav.models}</h1>
          <p class="lede">{t.lede.models}</p>
        </div>
        <div class="btn-row">
          <button class="btn ghost" onclick={loadModels} disabled={modelsLoading}>
            <Icon name="refresh" size={15} />
            {t.models.refresh}
          </button>
        </div>
      </div>

      <div class="toolbar">
        <label class="search-box">
          <Icon name="search" size={15} />
          <input type="search" placeholder={t.models.search} bind:value={modelsQuery} />
        </label>
        <div class="segmented">
          <button class:active={modelsCap === ''} onclick={() => (modelsCap = '')}>{t.models.filterAll}</button>
          <button class:active={modelsCap === 'max'} onclick={() => (modelsCap = 'max')}>{t.models.filters.max}</button>
          <button class:active={modelsCap === 'reasoning'} onclick={() => (modelsCap = 'reasoning')}>{t.models.filters.reasoning}</button>
          <button class:active={modelsCap === 'vision'} onclick={() => (modelsCap = 'vision')}>{t.models.filters.vision}</button>
        </div>
      </div>

      {#if modelsLoading && !modelRows.length}
        <div class="table">
          {#each Array(4) as _}
            <div class="row skeleton-row"><span class="skeleton"></span><span class="skeleton"></span><span class="skeleton"></span></div>
          {/each}
        </div>
      {:else if !filteredModels.length}
        <div class="empty"><p>{t.models.empty}</p></div>
      {:else}
        {#each filteredModels as group (group.uid)}
          <section class="group">
            <div class="group-head">
              <h2><span class="src">{group.source}</span> {group.label}</h2>
              <span class="badge outline">{t.models.count(group.items.length)}</span>
            </div>
            <div class="table scroll models-table">
              <div class="row head" style="--cols: var(--cols-models-page)">
                <span>{t.models.col.model}</span>
                <span>{t.models.col.context}</span>
                <span>{t.models.col.efforts}</span>
                <span>{t.models.col.caps}</span>
              </div>
              {#each group.items as m (m.id)}
                <div class="row" style="--cols: var(--cols-models-page)">
                  <span class="label mono">{m.id}</span>
                  <span class="muted small mono" title={m.capabilities?.includes('max') ? t.models.contextTitle : ''}>
                    {#if m.capabilities?.includes('max')}
                      <span class="badge max" title={t.models.contextTitle}>{t.logs.maxBadge(m.context_length)}</span>
                    {:else if m.context_length}
                      {fmtTokens(m.context_length)}
                    {:else}
                      {t.common.dash}
                    {/if}
                  </span>
                  <span class="muted small">
                    {(m.thinking_efforts ?? []).length ? m.thinking_efforts.join(' / ') : t.models.effortsOff}
                  </span>
                  <span class="cell-route">
                    {#each m.capabilities ?? [] as cap}
                      {#if cap !== 'max'}
                        <span class="badge info">{t.models.cap[cap] || cap}</span>
                      {/if}
                    {/each}
                    {#if !(m.capabilities ?? []).some((c) => c !== 'max')}<span class="muted small">{t.common.dash}</span>{/if}
                  </span>
                </div>
              {/each}
            </div>
          </section>
        {/each}
      {/if}
      {#each Object.entries(modelFailed) as [uid, err]}
        <p class="msg error">{t.models.failed(uid, err)}</p>
      {/each}
    {/if}

    {#if tab === 'credits'}
      <div class="page-head">
        <div class="titles">
          <h1>{t.nav.credits}</h1>
          <p class="lede">{t.lede.credits}</p>
        </div>
        <div class="btn-row">
          <button class="btn ghost" onclick={loadCredits} disabled={creditsLoading}>
            <Icon name="refresh" size={15} />
            {t.credits.refresh}
          </button>
        </div>
      </div>
      {#if creditsLoading}<p class="muted small">{t.credits.refreshing}</p>{/if}
      {#if creditsErr}<p class="msg error">{creditsErr}</p>{/if}

      {#each creditRows as row (row.key)}
        <section class="card">
          <div class="card-head">
            <div class="titles">
              <h2><span class="src">{row.source}</span> {row.label || row.uid}</h2>
              {#if row.local}<span class="hint">{t.credits.local}</span>{/if}
            </div>
            <div class="credit-sum">
              {#if row.failed}
                <span class="badge bad">{t.credits.failed}</span>
              {:else if row.unlimited}
                <span class="badge info">{t.credits.unlimited}</span>
              {:else}
                <span class="credit-num">{fmtCredits(row.remain)}</span>
                <span class="muted small">/ {fmtCredits(row.total)} · {t.credits.summary.used} {fmtCredits(row.used)}</span>
              {/if}
            </div>
          </div>
          {#if row.error}<p class="msg error">{row.error}</p>{/if}

          {#if !row.failed && !row.local && (row.packs ?? []).length}
            <div class="credit-layout">
              <div>
                <h3 class="sub">{t.credits.expiry.title}</h3>
                {#if (row.packs ?? []).some((p) => p.expires_at)}
                  <div class="expiry-bars">
                    {#each packsByDay(row).days as [day, remain] (day)}
                      <div class="expiry-row" title="{day} · {fmtCredits(remain)}">
                        <span class="mono small">{fmtDay(day)}</span>
                        <div class="expiry-track"><div class="expiry-fill" style="width:{expiryBarWidth(row, remain)}%"></div></div>
                        <span class="muted small mono">{fmtCredits(remain)}</span>
                      </div>
                    {/each}
                  </div>
                {:else}
                  <p class="muted small">{t.credits.expiry.empty}</p>
                {/if}
                {#if (row.packs ?? []).some((p) => !p.expires_at)}
                  <p class="muted small">{t.credits.summary.remain}（{t.common.dash}）：{fmtCredits((row.packs ?? []).filter((p) => !p.expires_at).reduce((s, p) => s + (p.remain || 0), 0))}</p>
                {/if}
              </div>
              <div>
                <h3 class="sub">{t.credits.packs.title}</h3>
                <div class="table packs-table">
                  <div class="row head" style="--cols: var(--cols-packs)">
                    <span>{t.credits.packs.col.name}</span>
                    <span>{t.credits.packs.col.size}</span>
                    <span>{t.credits.packs.col.used}</span>
                    <span>{t.credits.packs.col.remain}</span>
                    <span>{t.credits.packs.col.expires}</span>
                  </div>
                  {#each row.packs as p, i (i)}
                    <div class="row" style="--cols: var(--cols-packs)">
                      <span class="small">{p.name || t.common.dash}</span>
                      <span class="muted small mono">{p.size || t.common.dash}</span>
                      <span class="muted small mono">{p.used || t.common.dash}</span>
                      <span class="small mono">{p.remain}</span>
                      <span class="muted small">{p.expires_text || t.common.dash}</span>
                    </div>
                  {/each}
                </div>
              </div>
            </div>
          {:else if !row.failed && !row.local}
            <p class="muted small">{t.credits.packs.empty}</p>
          {/if}
        </section>
      {/each}
      {#if creditRows.length}<p class="muted small">{t.credits.stale}</p>{/if}
    {/if}

    {#if tab === 'settings'}
      <div class="page-head">
        <div class="titles">
          <h1>{t.nav.settings}</h1>
          <p class="lede">{t.lede.settings}</p>
        </div>
      </div>
      <Settings />
    {/if}

    {#if tab === 'add'}
      <div class="page-head">
        <div class="titles">
          <h1>{t.nav.add}</h1>
          <p class="lede">{t.lede.add}</p>
        </div>
      </div>

      <div class="card">
        <div class="segmented">
          <button class:active={addMode === 'device'} onclick={() => (addMode = 'device')}>{t.add.mode.device}</button>
          {#if manualSources.length}
            <button class:active={addMode === 'manual'} onclick={() => (addMode = 'manual')}>{t.add.mode.manual}</button>
          {/if}
          {#if localSources.length}
            <button class:active={addMode === 'local'} onclick={() => (addMode = 'local')}>{t.add.mode.local}</button>
          {/if}
        </div>

        {#if addMode === 'local'}
          <h2 class="section-title">{t.add.local.title}</h2>
          <p class="muted lede-small">{t.add.local.body}</p>
          {#if localSources.length > 1}
            <label class="field">
              <span class="field-label">{t.add.local.source}</span>
              <select bind:value={localSource}>
                {#each localSources as s}<option value={s}>{s}</option>{/each}
              </select>
            </label>
          {/if}
          <button class="btn primary" onclick={importLocal} disabled={localPhase === 'running'}>
            {#if localPhase === 'running'}<span class="spinner"></span>{/if}
            {localPhase === 'running' ? t.add.local.running : t.add.local.button}
          </button>
          {#if localMsg}
            <p class="msg {localPhase === 'error' ? 'error' : 'ok'}">{localMsg}</p>
          {/if}
        {:else if addMode === 'device' || !manualSources.length}
          <h2 class="section-title">{t.add.device.title}</h2>
          <p class="muted lede-small">{t.add.device.body}</p>

          {#if authPhase === 'idle' || authPhase === 'error'}
            <div class="field-row">
              <label class="field">
                <span class="field-label">{t.add.device.source}</span>
                <select bind:value={authSource}>
                  {#each deviceSources as s}<option value={s}>{s}</option>{/each}
                </select>
              </label>
              <label class="field">
                <span class="field-label">{t.add.device.realm}</span>
                <select bind:value={authRealm}>
                  <option value="cn">{t.add.device.realmCn}</option>
                  <option value="global">{t.add.device.realmGlobal}</option>
                </select>
              </label>
              <button class="btn primary" onclick={startAuth}>{t.add.device.start}</button>
            </div>
            {#if authPhase === 'error'}
              <p class="msg error">{authMsg}</p>
            {/if}
          {/if}

          {#if authPhase === 'starting'}
            <p class="msg"><span class="spinner inline"></span> {t.add.device.starting}</p>
          {/if}

          {#if authPhase === 'waiting'}
            <div class="auth-wait">
              <p class="msg"><span class="spinner inline"></span> {t.add.device.waiting}</p>
              {#if authUrl}
                <a class="btn ghost" href={authUrl} target="_blank" rel="noreferrer">
                  <Icon name="external" size={15} />
                  {t.add.device.openLogin}
                </a>
                <p class="muted small mono url">{authUrl}</p>
              {/if}
              <button class="btn quiet" onclick={resetAuth}>{t.common.cancel}</button>
            </div>
          {/if}

          {#if authPhase === 'done'}
            <div class="auth-wait">
              <p class="msg ok">{authMsg}</p>
              <button class="btn ghost" onclick={resetAuth}>{t.add.device.again}</button>
            </div>
          {/if}
        {:else}
          <h2 class="section-title">{t.add.token.title}</h2>
          <p class="muted lede-small">{t.add.token.body}</p>

          <label class="field">
            <span class="field-label">{t.add.token.source}</span>
            <select bind:value={tokenSource}>
              {#each manualSources as s}<option value={s}>{s}</option>{/each}
            </select>
          </label>
          <label class="field">
            <span class="field-label">{modeHint ? t.add.token.labelWithHint(modeHint) : t.add.token.label}</span>
            <textarea rows="3" bind:value={tokenValue} placeholder={tokenPlaceholder}></textarea>
          </label>
          <div class="field-row">
            <label class="field">
              <span class="field-label">{t.add.token.refresh}</span>
              <input bind:value={tokenRefresh} />
            </label>
            <label class="field">
              <span class="field-label">{t.add.token.uid}</span>
              <input bind:value={tokenUid} />
            </label>
            <label class="field">
              <span class="field-label">{t.add.token.note}</span>
              <input bind:value={tokenLabel} placeholder={t.add.token.notePlaceholder} />
            </label>
          </div>
          <button class="btn primary" onclick={submitToken} disabled={tokenPhase === 'running'}>
            {#if tokenPhase === 'running'}<span class="spinner"></span>{/if}
            {tokenPhase === 'running' ? t.add.token.submitting : t.add.token.submit}
          </button>
          {#if tokenMsg}
            <p class="msg {tokenPhase === 'error' ? 'error' : 'ok'}">{tokenMsg}</p>
          {/if}
        {/if}
      </div>
    {/if}

    {#if tab === 'import'}
      <div class="page-head">
        <div class="titles">
          <h1>{t.importAcc.title}</h1>
          <p class="lede">{t.importAcc.body}</p>
        </div>
      </div>

      <div class="card">
        <div class="segmented">
          <button class:active={importMode === 'inline'} onclick={() => (importMode = 'inline')}>{t.importAcc.mode.inline}</button>
          <button class:active={importMode === 'path'} onclick={() => (importMode = 'path')}>{t.importAcc.mode.path}</button>
          <button class:active={importMode === 'bundle'} onclick={() => (importMode = 'bundle')}>{t.importAcc.mode.bundle}</button>
        </div>

        {#if importMode === 'inline'}
          <label class="field">
            <span class="field-label">{t.importAcc.inlineLabel}</span>
            <textarea rows="8" bind:value={importText} placeholder={inlinePlaceholder}></textarea>
          </label>
        {:else if importMode === 'path'}
          <label class="field">
            <span class="field-label">{t.importAcc.pathLabel}</span>
            <input bind:value={importPath} placeholder={t.importAcc.pathPlaceholder} />
          </label>
        {:else}
          <label class="field">
            <span class="field-label">{t.importAcc.bundleLabel}</span>
            <textarea rows="5" bind:value={importBundle}></textarea>
          </label>
          <label class="field">
            <span class="field-label">{t.importAcc.passphrase}</span>
            <input type="password" bind:value={importPass} />
          </label>
        {/if}

        <button class="btn primary" onclick={doImport} disabled={importPhase === 'running'}>
          {#if importPhase === 'running'}<span class="spinner"></span>{/if}
          {importPhase === 'running' ? t.importAcc.running : t.importAcc.submit}
        </button>

        {#if importMsg}
          <p class="msg {importPhase === 'error' ? 'error' : 'ok'}">{importMsg}</p>
        {/if}
        {#if importResult && importResult.errors?.length}
          <ul class="skips">
            {#each importResult.errors as e}<li>{e}</li>{/each}
          </ul>
        {/if}
      </div>
    {/if}
  </main>

  <footer class="foot muted small">{t.footer}</footer>
</div>

<ToastHost />
<ConfirmHost />

<style>
  .shell {
    display: flex;
    flex-direction: column;
    min-height: 100vh;
  }

  /* ── top bar ───────────────────────────────────────────────────────────── */

  .topbar {
    position: sticky;
    top: 0;
    z-index: var(--z-sticky-header);
    background: var(--nav-bg);
    backdrop-filter: var(--nav-blur);
    -webkit-backdrop-filter: var(--nav-blur);
    border-bottom: 1px solid transparent;
    transition: background var(--transition-normal), border-color var(--transition-normal),
      box-shadow var(--transition-normal);
  }

  .topbar.scrolled {
    background: var(--nav-bg-scrolled);
    border-bottom-color: var(--border-subtle);
    box-shadow: var(--shadow-sm);
  }

  .topbar-inner {
    max-width: var(--page-max-width);
    margin: 0 auto;
    padding: var(--space-3) var(--page-padding-x);
    display: flex;
    align-items: center;
    gap: var(--space-4);
    flex-wrap: wrap;
  }

  .brand {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    min-width: 0;
  }

  .logo {
    display: grid;
    place-items: center;
    width: 34px;
    height: 34px;
    flex: none;
    border-radius: var(--radius);
    background: linear-gradient(145deg, var(--primary), #fb7185);
    color: #fff;
    font-family: var(--font-family-mono);
    font-size: var(--text-sm);
    font-weight: var(--font-semibold);
    letter-spacing: -0.04em;
    box-shadow: 0 2px 10px var(--glow-orange);
  }

  .brand-text {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }

  .brand-name {
    font-size: var(--text-base);
    font-weight: var(--font-semibold);
    letter-spacing: var(--tracking-tight);
    line-height: var(--leading-tight);
  }

  .brand-sub {
    font-size: var(--text-2xs);
    color: var(--muted-foreground);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .tabs {
    display: flex;
    align-items: center;
    gap: var(--space-0-5);
    flex: 1 1 auto;
    min-width: 0;
    overflow-x: auto;
    scrollbar-width: none;
  }

  .tabs::-webkit-scrollbar {
    display: none;
  }

  .tab {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1-5);
    height: 34px;
    padding: 0 var(--space-3);
    border: none;
    border-radius: 999px;
    background: none;
    color: var(--muted-foreground);
    font: inherit;
    font-size: var(--text-caption);
    font-weight: var(--font-medium);
    white-space: nowrap;
    cursor: pointer;
    transition: background var(--transition-fast), color var(--transition-fast);
  }

  .tab:hover {
    background: var(--surface-hover);
    color: var(--foreground);
  }

  .tab.active {
    background: var(--primary-light);
    color: var(--primary);
    font-weight: var(--font-semibold);
  }

  .tab-sep {
    width: 1px;
    height: 18px;
    margin: 0 var(--space-1-5);
    background: var(--border);
    flex: none;
  }

  .topbar-right {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    margin-left: auto;
  }

  .stat {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1-5);
    font-size: var(--text-sm);
    white-space: nowrap;
  }

  .count {
    font-size: var(--text-base);
    font-weight: var(--font-semibold);
    font-variant-numeric: tabular-nums;
  }

  .dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--success);
    box-shadow: 0 0 0 3px var(--success-bg);
  }

  .theme-switch {
    flex: none;
  }

  .theme-opt .theme-label {
    display: none;
  }

  /* ── page ──────────────────────────────────────────────────────────────── */

  .page {
    flex: 1;
    width: 100%;
    max-width: var(--page-max-width);
    margin: 0 auto;
    padding: var(--page-top) var(--page-padding-x) var(--page-bottom);
  }

  .page-head {
    display: flex;
    align-items: flex-end;
    justify-content: space-between;
    gap: var(--space-4);
    flex-wrap: wrap;
    margin-bottom: var(--space-5);
  }

  .page-head .titles {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: 0;
  }

  .page-head h1 {
    margin: 0;
    font-size: var(--text-2xl);
    font-weight: var(--font-semibold);
    letter-spacing: var(--tracking-tight);
  }

  .lede {
    margin: 0;
    max-width: 52rem;
    font-size: var(--text-caption);
    color: var(--muted-foreground);
    line-height: var(--leading-relaxed);
  }

  .lede-small {
    font-size: var(--text-caption);
    line-height: var(--leading-relaxed);
    max-width: 46rem;
  }

  .banner {
    margin-bottom: var(--space-4);
  }

  /* ── groups ────────────────────────────────────────────────────────────── */

  .group {
    margin-bottom: var(--space-6);
  }

  .group-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin-bottom: var(--space-3);
  }

  .group-head h2 {
    margin: 0;
    font-size: var(--text-base);
    font-weight: var(--font-semibold);
  }

  .src {
    font-family: var(--font-family-mono);
    font-size: var(--text-sm);
    background: var(--secondary);
    border-radius: var(--radius-sm);
    padding: 3px var(--space-2-5);
  }

  /* ── layout helpers ────────────────────────────────────────────────────── */

  .column {
    display: flex;
    flex-direction: column;
    gap: var(--section-gap);
  }

  .section-title {
    margin: var(--space-5) 0 var(--space-1-5);
    font-size: var(--text-lg);
    font-weight: var(--font-semibold);
  }

  /* ── table column sets ─────────────────────────────────────────────────── */

  .table {
    --cols-accounts: minmax(9rem, 2fr) minmax(8rem, 1.4fr) 6rem 6rem minmax(9rem, 1.2fr) 4rem;
    --cols-tasks: minmax(11rem, 2.2fr) 6rem 5.5rem minmax(7rem, 1.2fr) 6rem;
    --cols-queue: minmax(10rem, 2.4fr) 5rem 5rem;
    --cols-logs: 5.5rem minmax(8rem, 1.4fr) minmax(6rem, 1fr) minmax(7rem, 1.1fr) 7.5rem 4.5rem
      minmax(7rem, 1fr) minmax(6rem, 0.9fr) 5rem;
    --cols-daily: 6.5rem repeat(2, 4rem) 4rem repeat(3, minmax(4.5rem, 1fr));
    --cols-models: minmax(9rem, 1.6fr) 4.5rem repeat(3, minmax(4.5rem, 1fr)) 5.5rem;
    --cols-records: minmax(9rem, 1.1fr) 5rem minmax(6rem, 1fr) minmax(7rem, 1.1fr) 4.5rem
      minmax(7rem, 1fr) 5rem 5.5rem;
    --cols-models-page: minmax(11rem, 2fr) minmax(7rem, 1fr) minmax(9rem, 1.2fr) minmax(8rem, 1fr);
    --cols-packs: minmax(9rem, 1.6fr) 4.5rem 4.5rem 5rem minmax(7rem, 1fr);
  }

  .table.scroll {
    overflow-x: auto;
  }

  .table.scroll > .row {
    min-width: 720px;
  }

  .table.scroll .cell-route {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 3px;
  }

  .cell-stack {
    display: flex;
    flex-direction: column;
    gap: 1px;
    min-width: 0;
  }

  .cell-stack em {
    font-style: normal;
    font-size: var(--text-2xs);
    color: var(--muted-foreground);
    font-variant-numeric: tabular-nums;
  }

  .cell-status {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1-5);
    flex-wrap: wrap;
  }

  .right.actions {
    display: flex;
    gap: var(--space-3);
    justify-content: flex-end;
  }

  .task-skeleton {
    margin-top: var(--space-3);
  }

  .table .row.skeleton-row {
    --cols: repeat(3, 1fr);
    padding: var(--space-3) var(--space-4);
  }

  .table .row.skeleton-row .skeleton {
    height: 12px;
  }

  /* ── task cards ────────────────────────────────────────────────────────── */

  .acc-card {
    padding: var(--space-4);
    margin-bottom: var(--space-3);
  }

  .acc-head {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-2);
  }

  .acc-name {
    font-weight: var(--font-semibold);
  }

  .caret {
    gap: var(--space-1-5);
  }

  .caret-glyph {
    display: inline-block;
    color: var(--muted-foreground);
    transition: transform var(--transition-ios);
    font-size: 0.7em;
  }

  .caret-glyph.open {
    transform: rotate(90deg);
  }

  .acc-ops {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-3);
    font-size: var(--text-sm);
  }

  .results {
    margin: var(--space-3) 0 0;
    padding-left: var(--space-4);
    font-size: var(--text-xs);
    color: var(--muted-foreground);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .results li.error {
    color: var(--destructive);
  }

  .results li.done {
    color: var(--success);
  }

  .catalog h3 {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-sm);
    font-weight: var(--font-semibold);
    margin: var(--space-4) 0 var(--space-2);
  }

  .catalog ul {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-1-5);
  }

  .catalog li {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
    font-size: var(--text-xs);
  }

  /* ── usage chart ───────────────────────────────────────────────────────── */

  .bars {
    display: flex;
    align-items: flex-end;
    gap: 2px;
    height: 132px;
    margin: var(--space-2) 0 0;
  }

  .bar-col {
    flex: 1 1 0;
    min-width: 3px;
    height: 100%;
    display: flex;
    align-items: flex-end;
  }

  .bar {
    width: 100%;
    min-height: 2px;
    border-radius: 3px 3px 0 0;
    background: linear-gradient(to top, var(--primary), color-mix(in srgb, var(--primary) 55%, transparent));
    opacity: 0.9;
    transition: opacity var(--transition-fast), transform var(--transition-fast);
  }

  .bar-col:hover .bar {
    opacity: 1;
  }

  /* A day with no traffic is an axis line, not a spike: colouring it like real
     usage makes an idle week look busy. */
  .bar.zero {
    background: var(--border);
    opacity: 1;
  }

  .filters {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }

  .filters select,
  .filters input {
    width: auto;
    min-width: 9rem;
    font-size: var(--text-sm);
  }

  /* ── misc ──────────────────────────────────────────────────────────────── */

  .empty-mark {
    display: grid;
    place-items: center;
    width: 48px;
    height: 48px;
    border-radius: var(--radius-lg);
    background: var(--primary-light);
    color: var(--primary);
    margin-bottom: var(--space-1);
  }

  .auth-wait {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: var(--space-3);
    margin-top: var(--space-4);
  }

  .url {
    max-width: 100%;
    overflow-wrap: anywhere;
  }

  .skips {
    margin: var(--space-3) 0 0;
    padding-left: var(--space-4);
    color: var(--warning);
    font-size: var(--text-xs);
  }

  .spinner.inline {
    display: inline-block;
    vertical-align: -2px;
    margin-right: var(--space-1);
  }

  .foot {
    max-width: var(--page-max-width);
    width: 100%;
    margin: 0 auto;
    padding: var(--space-6) var(--page-padding-x) var(--space-10);
    text-align: center;
  }

  /* ── overview ──────────────────────────────────────────────────────────── */

  .ov-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--section-gap);
  }

  .ov-grid .card:last-child {
    grid-column: 1 / -1;
  }

  .ov-chips {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }

  .ov-chip {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1-5) var(--space-3);
    border: 1px solid var(--border);
    border-radius: var(--button-radius);
    background: var(--surface-2);
    cursor: pointer;
    max-width: 100%;
    transition: border-color var(--transition-fast), background var(--transition-fast);
  }

  .ov-chip:hover {
    border-color: var(--primary);
    background: var(--primary-light);
  }

  .ov-chip-name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: var(--text-sm);
  }

  .ov-rows {
    display: flex;
    flex-direction: column;
  }

  .ov-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    padding: var(--space-2-5) 0;
    border-bottom: 1px solid var(--border-light);
  }

  .ov-row:last-child {
    border-bottom: 0;
  }

  .ov-row .label {
    min-width: 0;
  }

  .kv {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: var(--space-2) var(--space-5);
    align-items: center;
  }

  .kv .k {
    color: var(--text-muted);
    font-size: var(--text-sm);
  }

  .kv .v {
    font-size: var(--text-sm);
  }

  .warn-text {
    color: var(--warning);
  }

  .bad-text {
    color: var(--danger);
  }

  .empty.slim {
    padding: var(--space-4) 0;
  }

  /* ── models & credits ──────────────────────────────────────────────────── */

  .search-box {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    padding: 0 var(--space-3);
    border: 1px solid var(--border);
    border-radius: var(--button-radius);
    background: var(--surface-2);
    color: var(--text-muted);
  }

  .search-box input {
    border: 0;
    background: transparent;
    padding: var(--space-2) 0;
    font-size: var(--text-sm);
    color: var(--text);
    min-width: 12rem;
  }

  .search-box input:focus {
    outline: none;
  }

  .search-box:focus-within {
    border-color: var(--primary);
  }

  .badge.max {
    color: var(--primary);
    background: var(--primary-light);
    font-weight: var(--font-semibold);
  }

  .credit-sum {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
  }

  .credit-num {
    font-size: var(--text-xl);
    font-weight: var(--font-semibold);
    font-variant-numeric: tabular-nums;
  }

  .credit-layout {
    display: grid;
    grid-template-columns: minmax(14rem, 1fr) minmax(0, 1.6fr);
    gap: var(--section-gap);
    margin-top: var(--space-4);
  }

  .sub {
    font-size: var(--text-sm);
    font-weight: var(--font-semibold);
    color: var(--text-muted);
    margin: 0 0 var(--space-2);
  }

  .expiry-bars {
    display: flex;
    flex-direction: column;
    gap: var(--space-1-5);
  }

  .expiry-row {
    display: grid;
    grid-template-columns: 3.2rem 1fr auto;
    align-items: center;
    gap: var(--space-2);
  }

  .expiry-track {
    height: 8px;
    border-radius: 980px;
    background: var(--surface-3, var(--surface-2));
    overflow: hidden;
  }

  .expiry-fill {
    height: 100%;
    border-radius: 980px;
    background: var(--primary);
    opacity: 0.85;
    transition: width var(--transition-ios);
  }

  .packs-table .row {
    min-width: 0;
  }

  /* ── responsive ────────────────────────────────────────────────────────── */

  @media (max-width: 1080px) {
    .brand-sub {
      display: none;
    }
  }

  @media (max-width: 900px) {
    .topbar-inner {
      gap: var(--space-2);
    }
    .brand {
      flex: 1 1 auto;
    }
    .topbar-right {
      order: 2;
    }
    .tabs {
      order: 3;
      flex-basis: 100%;
      margin: 0 calc(-1 * var(--space-1));
    }
    .tab-sep {
      display: none;
    }
  }

  @media (max-width: 640px) {
    .page {
      padding: var(--page-top) var(--page-padding-x-mobile) var(--space-12);
    }
    .topbar-inner {
      padding: var(--space-2-5) var(--page-padding-x-mobile);
    }
    .stat {
      display: none;
    }
    .page-head h1 {
      font-size: var(--text-xl);
    }
    .tab span:not(.theme-label) {
      display: none;
    }
    .tab {
      padding: 0 var(--space-2-5);
    }

    /* On a phone the account list keeps its identity, its state and its one
       action. A six-column grid would otherwise force a side-scroll that hides
       exactly the two things the row is for. */
    .table {
      --cols-accounts: minmax(0, 1fr) auto 3.5rem;
    }
    .accounts-table > .row,
    .table.scroll.accounts-table > .row {
      min-width: 0;
    }
    .accounts-table .row > :nth-child(3),
    .accounts-table .row > :nth-child(4),
    .accounts-table .row > :nth-child(5) {
      display: none;
    }

    .ov-grid,
    .credit-layout {
      grid-template-columns: 1fr;
    }
    .ov-grid .card:last-child {
      grid-column: auto;
    }
    .search-box input {
      min-width: 8rem;
    }
    .table {
      --cols-models-page: minmax(0, 1.4fr) minmax(0, 1fr) auto;
      --cols-packs: minmax(0, 1.4fr) auto auto minmax(0, 0.8fr);
    }
    .models-table > .row,
    .table.scroll.models-table > .row,
    .packs-table > .row {
      min-width: 0;
    }
    .models-table .row > :nth-child(3),
    .packs-table .row > :nth-child(2) {
      display: none;
    }
  }
</style>
