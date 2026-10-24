<script>
  // CodeNexus console. Sources are a first-class concept: every account carries
  // the upstream it belongs to, and the whole UI is grouped around that.
  import { onMount, tick } from 'svelte'
  import Settings from './Settings.svelte'
  import Icon from './ui/Icon.svelte'
  import Menu from './ui/Menu.svelte'
  import ToastHost from './ui/ToastHost.svelte'
  import ConfirmHost from './ui/ConfirmHost.svelte'
  import { toastOk, toastErr, pushToast } from './ui/toast.svelte.js'
  import { pageMeta } from './ui/pagemeta.svelte.js'
  import { ask, confirmState } from './ui/confirm.svelte.js'
  import { trapFocus, focusInto } from './ui/focus.js'
  import { t } from './strings.js'

  const API = '/panel/api'

  let session = $state(null)
  let accounts = $state([])
  let loading = $state(true)

  // Three tiers, three carriers, no shared slot: a fatal panel failure owns the
  // banner, a section failure is drawn inside the card that failed, and an
  // operation failure is a toast. Two errors in different sections can be true
  // at the same time, so nothing may overwrite anything else.
  let fatal = $state('')
  let fatalFrom = $state('')
  let sectionErr = $state({}) // section key -> message

  // The banner belongs to whoever raised it. A successful account poll must not
  // hide a session failure, and vice versa.
  function raiseFatal(from, message) {
    if (fatalFrom === from && fatal === message) return
    fatal = message
    fatalFrom = from
  }

  function clearFatal(from) {
    if (fatalFrom !== from) return
    fatal = ''
    fatalFrom = ''
  }

  function setSection(key, message) {
    sectionErr = { ...sectionErr, [key]: message }
  }

  function clearSection(key) {
    if (!sectionErr[key]) return
    const next = { ...sectionErr }
    delete next[key]
    sectionErr = next
  }

  // ── routing ──────────────────────────────────────────────────────────────
  // The address bar is the state. A destination is a real hash route, so
  // back/forward, a reload and a pasted link all land on the same page.
  const TABS = ['overview', 'usage', 'credits', 'tasks', 'models', 'settings']

  function tabFromHash() {
    const raw = location.hash.replace(/^#\/?/, '').split('?')[0]
    return TABS.includes(raw) ? raw : 'overview'
  }

  // Accounts is the landing view. It carries the pool's own statistics at the
  // top, so a separate dashboard would only duplicate them behind one more hop.
  let tab = $state(tabFromHash())
  let scrolled = $state(false)

  // Each page states how old its numbers may be before a visit refetches them.
  // Inside the window the page paints from memory immediately instead of
  // flashing a skeleton it already has the answer for.
  const STALE_MS = { overview: 15_000, usage: 60_000, tasks: 15_000, credits: 300_000 }
  let loadedAt = $state({})

  function fresh(name) {
    const at = loadedAt[name] || 0
    return at > 0 && Date.now() - at < (STALE_MS[name] ?? 0)
  }

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

  // ── self-refreshing overview ──────────────────────────────────────────────
  // Nothing on the overview asks the user to press refresh. The counters come
  // from the gateway's own state and cost no upstream call, so they can poll
  // quietly; a tab that was left in the background catches up the moment it is
  // looked at again, which is the only moment staleness is visible.
  let overviewTimer = null

  function startOverviewPolling() {
    stopOverviewPolling()
    overviewTimer = setInterval(() => {
      if (tab === 'overview' && !document.hidden) loadOverview()
    }, 30_000)
  }

  function stopOverviewPolling() {
    if (overviewTimer) {
      clearInterval(overviewTimer)
      overviewTimer = null
    }
  }

  // ── onboarding (device authorization) ────────────────────────────────────
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
  let importText = $state('')
  let importPhase = $state('idle')
  let importMsg = $state('')
  let importResult = $state(null)

  // ── task center ──────────────────────────────────────────────────────────
  // The console never guesses what an account supports: it lists the task
  // catalogue, then loads one account's tasks on demand so a pool of twenty
  // credentials does not fan out into twenty upstream round trips on open.
  let expanded = $state({})
  let accountTasks = $state({}) // uid -> { loading, tasks, error }

  // Writes are serialised per account, not globally: signing in three accounts
  // is three independent jobs, and blocking all of them behind one person's
  // click is what made the old console feel dead. A batch sweep touches every
  // account, so it is exclusive with all of them. Reads never wait for either.
  let busyWrites = $state({}) // uid -> label of the running operation
  let busyBatch = $state('') // batch key while a sweep runs
  let batchLabel = $state('')
  const anyWrite = $derived(Object.keys(busyWrites).length > 0)
  const writeBlocked = (acc) => !!busyBatch || !!busyWrites[acc.uid]

  let queue = $state([])
  let queueBusy = $state(false)
  let results = $state({}) // uid -> last growth run

  // ── persisted usage history ─────────────────────────────────────────────
  // usageRange is a preset id or 'all' / 'custom'. Keeping one value instead
  // of a days number plus two date strings means the control can never claim
  // two windows at once.
  let usageRange = $state('7')
  let usageFrom = $state('')
  let usageTo = $state('')
  let usageAvailable = $state(null)
  let usageSummary = $state(null)
  let usagePrev = $state(null)
  let usageModels = $state([])
  let usageAccounts = $state([])
  let usageWires = $state([])
  let usageDim = $state('account') // account | model | wire
  // The page-level source selection. It rides on the query so the counters,
  // the chart, the breakdowns and the detail table all describe the same rows
  // — a filter that only narrowed one panel would make the page contradict
  // itself.
  let usageSource = $state('')
  // The headline's unit: volume (tokens) or bill (credits). One number owns
  // the metric head at a time; the other stays reachable through the strip and
  // the detail columns.
  let usageMetric = $state('tokens') // tokens | credits
  let usageDaily = $state([])
  let usageRecords = $state([])
  let usageFilter = $state({ wire: '', model: '', outcome: '' })
  let usageLoading = $state(false)
  let recordsLoading = $state(false)
  let recordsExhausted = $state(false)
  // The detail table is a timeline, not an archive: past this many rows the
  // answer is "narrow the window", not "keep loading".
  let recordsCapped = $state(false)
  const RECORDS_CAP = 2000
  // Analysis-canvas folds: both exact-number tables start closed so the
  // headline and the chart own the first screen. One click reveals either.
  let dailyOpen = $state(false)
  let recordsOpen = $state(false)
  // The diagnostic panels start folded: the ledger owns the first screen, and
  // the fold's own summary still says whether anything needs attention.
  let diagOpen = $state(false)
  let chartExpanded = $state(false)
  let hoverDay = $state(null) // index into usageDaily, null when the pointer is off
  let breakdownAll = $state(false) // Top 10 vs the whole dimension
  let spendDim = $state('account') // account | source — the right-hand credit bars

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

  // Seven destinations in the reference console's order. Adding an account is
  // not a destination but an action, so it lives in the top bar as a button
  // that opens a dialog rather than taking a slot in the navigation.
  // Six destinations, in the order the console is meant to be read: what is
  // happening now, what it cost, what is left, what can be earned, what is
  // available, and how it is all configured. The request log is deliberately
  // absent: it is something you glance at, so it lives behind a top-bar
  // button and not in a list of places to go.
  const navItems = [
    { id: 'overview', label: t.nav.overview, icon: 'overview' },
    { id: 'usage', label: t.nav.usage, icon: 'usage' },
    { id: 'credits', label: t.nav.credits, icon: 'credits' },
    { id: 'tasks', label: t.nav.tasks, icon: 'tasks' },
    { id: 'models', label: t.nav.models, icon: 'models' },
    { id: 'settings', label: t.nav.settings, icon: 'settings' },
  ]
  const themeOptions = [
    { value: 'system', label: t.nav.theme.system, icon: 'monitor' },
    { value: 'light', label: t.nav.theme.light, icon: 'sun' },
    { value: 'dark', label: t.nav.theme.dark, icon: 'moon' },
  ]

  // ── requests ─────────────────────────────────────────────────────────────
  // Polling and a person switching windows race each other. Every read takes a
  // channel (its endpoint, unless a caller needs a private one) and a sequence
  // number; a response that is no longer the newest in its channel is discarded
  // rather than painted. Writes never join a channel: a POST that was already
  // sent must not be cancelled by the next click.
  const inflight = new Map()
  const stale = () => Object.assign(new Error('stale'), { stale: true })
  const isStale = (err) => !!err?.stale

  async function api(path, options = {}) {
    const method = (options.method || 'GET').toUpperCase()
    if (method !== 'GET') return request(path, options)

    const channel = options.channel || path.split('?')[0]
    const prev = inflight.get(channel)
    if (prev) prev.controller.abort()
    const token = { seq: (prev?.seq ?? 0) + 1, controller: new AbortController() }
    inflight.set(channel, token)
    const { channel: _ignored, ...init } = options
    try {
      return await request(path, { ...init, signal: token.controller.signal }, token, channel)
    } catch (err) {
      if (err?.name === 'AbortError' || inflight.get(channel) !== token) throw stale()
      throw err
    }
  }

  async function request(path, options, token, channel) {
    const res = await fetch(API + path, {
      headers: { 'Content-Type': 'application/json' },
      ...options,
    })
    const text = await res.text()
    // A newer request in this channel already owns the screen.
    if (token && inflight.get(channel) !== token) throw stale()
    let body = {}
    try {
      body = text ? JSON.parse(text) : {}
    } catch (err) {
      throw new Error(t.common.err.parse(res.status))
    }
    if (!res.ok || body.ok === false) {
      throw new Error(body.error || t.common.err.request(res.status))
    }
    return body
  }

  async function loadSession() {
    try {
      session = await api('/session')
      clearFatal('session')
    } catch (err) {
      if (!isStale(err)) raiseFatal('session', err.message)
    }
  }

  async function loadAccounts() {
    try {
      const body = await api('/accounts')
      accounts = body.accounts ?? []
      clearFatal('accounts')
    } catch (err) {
      if (!isStale(err)) raiseFatal('accounts', err.message)
    } finally {
      loading = false
    }
  }

  async function refresh() {
    await Promise.all([loadSession(), loadAccounts()])
  }

  onMount(() => {
    // The address bar states the destination from the first frame, so a copy of
    // the URL is a link to the page the operator is looking at.
    if (location.hash !== `#/${tab}`) {
      history.replaceState({ tab }, '', `#/${tab}`)
    }
    document.documentElement.dataset.page = tab
    refresh().then(() => {
      startOverviewPolling()
      connectLogs()
      ensureTabData(tab)
    })
    document.addEventListener('visibilitychange', onVisibility)
    window.addEventListener('popstate', onPopState)
    // A hash typed into the address bar is a navigation too; some browsers
    // report it only as hashchange.
    window.addEventListener('hashchange', onPopState)
    return () => {
      document.removeEventListener('visibilitychange', onVisibility)
      window.removeEventListener('popstate', onPopState)
      window.removeEventListener('hashchange', onPopState)
      stopOverviewPolling()
      stopPolling()
      disconnectLogs()
    }
  })

  // Sources have a fixed order. Deriving it from the pool's answer would let
  // the groups swap places whenever an account was added, and a table that
  // reorders itself under a 30s poll is a table you have to re-read.
  const SOURCE_RANK = { workbuddy: 0, 'trae-cn': 1, trae: 1 }

  const grouped = $derived.by(() => {
    const map = new Map()
    for (const acc of accounts) {
      const key = acc.source || 'unknown'
      if (!map.has(key)) map.set(key, [])
      map.get(key).push(acc)
    }
    return [...map.entries()]
      .map(([source, items]) => ({ source, items }))
      .sort((a, b) => (SOURCE_RANK[a.source] ?? 9) - (SOURCE_RANK[b.source] ?? 9))
  })

  // The sticky bar names the destination and states what the numbers under it
  // cover. It replaces the page heading every view used to repeat: the rail
  // already shows where you are, and a heading only pushed the data down.
  const currentTitle = $derived(navItems.find((item) => item.id === tab)?.label ?? '')
  const currentMeta = $derived.by(() => {
    switch (tab) {
      case 'overview':
        return t.overview.meta(accounts.length)
      case 'usage':
        return usageWindowLabel()
      case 'credits':
        return creditsReadAt ? t.credits.readAt(fmtClock(creditsReadAt)) : t.credits.notRead
      case 'tasks':
        // The task centre only lists WorkBuddy accounts; counting Trae here
        // would state a population the page below cannot act on.
        return t.tasks.meta(taskAccounts.length)
      case 'models':
        return t.models.meta(modelRows.length)
      case 'settings':
        // The page owns this one: only the settings catalogue knows how many
        // sections it has, and the shell is not going to fetch it to find out.
        return pageMeta.text
      default:
        return ''
    }
  })

  // The rail's pulse states one thing — is the gateway answering — and puts the
  // port where it can be read at a glance. Version and uptime are details, so
  // they live in the tooltip instead of competing with the destination labels.
  const listenPort = $derived(
    typeof session?.listen === 'string' && session.listen.includes(':')
      ? ':' + session.listen.split(':').pop()
      : '',
  )
  // The band states the whole listen address, not just the port: "where do I
  // point a client" is answered by host and port together.
  const listenAddr = $derived(typeof session?.listen === 'string' ? session.listen : '')
  const statusText = $derived(
    session ? [t.rail.running, listenPort].filter(Boolean).join(' · ') : t.rail.offline,
  )
  const statusTip = $derived.by(() => {
    if (!session) return t.rail.offlineHint
    const bits = []
    if (session.version) bits.push(String(session.version))
    // "已运行 不到 1 分钟" puts a space between two Chinese words. A process
    // that just came up gets its own phrase instead.
    if (session.uptime) {
      bits.push(
        session.uptime < 60 ? t.rail.justStarted : t.rail.uptime(fmtDuration(session.uptime)),
      )
    }
    if (session.listen) bits.push(String(session.listen))
    return bits.length ? bits.join(' · ') : t.rail.running
  })

  // The one address a client needs. Built from the listen address the backend
  // reports, so it stays right when the port moves; empty until the session
  // has been read, which is why the pill only renders when there is one.
  const endpointUrl = $derived(
    typeof session?.listen === 'string' && session.listen.includes(':')
      ? `http://${session.listen}/v1`
      : '',
  )
  // The foot's left half is page-specific on purpose: each view has one thing
  // it must not be misread about (a frozen balance, a serialised queue).
  const footNote = $derived(t.foot[tab] ?? t.foot.overview)

  let endpointCopied = $state(false)
  let endpointTimer = 0
  async function copyEndpoint() {
    if (!endpointUrl) return
    try {
      await navigator.clipboard.writeText(endpointUrl)
      endpointCopied = true
      clearTimeout(endpointTimer)
      endpointTimer = setTimeout(() => (endpointCopied = false), 2000)
    } catch {
      // A blocked clipboard leaves the address on screen to be selected by
      // hand; silently pretending it copied would be worse than doing nothing.
      endpointCopied = false
    }
  }

  // Tab visits every destination, as a plain list of links should. The arrow
  // keys are an extra: they move focus without changing what the list is, so
  // nothing has to be re-explained to a screen reader.
  function navKeydown(e) {
    const keys = ['ArrowDown', 'ArrowUp', 'Home', 'End']
    if (!keys.includes(e.key)) return
    const link = e.currentTarget
    const list = link.closest('ul')
    if (!list) return
    const links = [...list.querySelectorAll('a')]
    const at = links.indexOf(link)
    if (at < 0) return
    e.preventDefault()
    let next = at
    if (e.key === 'Home') next = 0
    else if (e.key === 'End') next = links.length - 1
    else if (e.key === 'ArrowDown') next = (at + 1) % links.length
    else next = (at - 1 + links.length) % links.length
    links[next]?.focus()
  }

  // A model can be refused for one account while working everywhere else. Left
  // unsaid, a temporary refusal reads as a missing capability, so the console  // names the model, the account and the moment it frees up again.
  const modelLocks = $derived.by(() => {
    const out = []
    for (const acc of accounts) {
      for (const l of acc.model_locks ?? []) {
        out.push({ model: l.model, until: l.until, account: acc.label || acc.uid })
      }
    }
    return out.sort((a, b) => a.until - b.until)
  })
  const nextUnlock = $derived(modelLocks.length ? modelLocks[0].until : 0)

  // Health is one decision with two outputs: a tone for the row, and a bucket
  // for the pool bar. Keeping them in one function is what stops the counter
  // at the top from disagreeing with the rows underneath — they used to be
  // computed twice, and a paused account was "健康" in the table while the
  // counter filed it under 暂停.
  function health(acc) {
    // Paused first: the pool reports `enabled: false` for a paused account as
    // well, so testing enabled first labelled every pausable account "已停用"
    // while the counter above filed it under 暂停. The two are different
    // remedies (resume versus revive), so the row has to say which one it is.
    if (acc.paused) return { tone: 'idle', bucket: 'paused', text: t.health.paused }
    if (!acc.enabled) return { tone: 'off', bucket: 'off', text: t.health.off }
    if (acc.broken) return { tone: 'danger', bucket: 'broken', text: t.health.bad }
    if (acc.cooling) return { tone: 'warn', bucket: 'cooling', text: t.health.warn }
    if (acc.healthy) return { tone: 'ok', bucket: 'ok', text: t.health.ok }
    return { tone: 'idle', bucket: 'ok', text: t.health.idle }
  }

  function fmtTime(sec) {
    if (!sec) return t.common.dash
    return fmtStamp(sec * 1000)
  }

  // Uptime reads as elapsed time, not as a timestamp: "3 天 4 小时" answers
  // "has it been restarted" at a glance, which a date does not.
  function fmtDuration(sec) {
    if (!sec) return ''
    // "0 分" is not an answer. A process that came up thirty seconds ago has
    // been running for less than a minute, and saying so is the honest report.
    if (sec < 60) return t.common.underMinute
    const d = Math.floor(sec / 86400)
    const h = Math.floor((sec % 86400) / 3600)
    const m = Math.floor((sec % 3600) / 60)
    if (d) return t.time.uptimeDays(d, h)
    if (h) return t.time.uptimeHours(h, m)
    return t.time.uptimeMinutes(m)
  }

  // Two upstreams share the console, so the platform is a short, stable label
  // rather than whatever the internal source identifier happens to be.
  // Source names are spelled out everywhere: the group header has room for
  // them, and "WB" was a column marker from a tighter grid that now reads as
  // an unexplained abbreviation.
  function sourceLabel(src) {
    return sourceName(src)
  }

  function sourceName(src) {
    if (src === 'workbuddy') return 'WorkBuddy'
    if (src === 'trae-cn' || src === 'trae') return 'Trae'
    return src || t.common.dash
  }

  function ago(unix) {
    const sec = Math.max(0, Math.floor(Date.now() / 1000 - unix))
    if (sec < 60) return t.time.justNow
    const min = Math.floor(sec / 60)
    if (min < 60) return t.time.minutesAgo(min)
    const hr = Math.floor(min / 60)
    if (hr < 24) return t.time.hoursAgo(hr)
    const days = Math.floor(hr / 24)
    // Past a week "11 天前" stops being easier to read than a date, and it is
    // no longer a relative fact anyone acts on.
    if (days <= 7) return t.time.daysAgo(days)
    const d = new Date(unix * 1000)
    const sameYear = d.getFullYear() === new Date().getFullYear()
    const mm = String(d.getMonth() + 1).padStart(2, '0')
    const dd = String(d.getDate()).padStart(2, '0')
    return sameYear ? `${mm}-${dd}` : `${d.getFullYear()}-${mm}-${dd}`
  }

  // A.2: every relative time carries its absolute twin in the title, in one
  // fixed format — locale strings drift with the OS language and then the
  // "what exactly happened" answer is not comparable across machines.
  function fmtStamp(value) {
    if (!value) return ''
    const d = typeof value === 'number' && value < 1e12 ? new Date(value * 1000) : new Date(value)
    if (Number.isNaN(d.getTime())) return ''
    const p = (n) => String(n).padStart(2, '0')
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
  }

  // A.3: an identifier pasted into a ticket must stay recognisable at both
  // ends; tail-truncation hides the very digits that tell two apart.
  function midEllipsis(s, head = 8, tail = 5) {
    const v = String(s ?? '')
    if (v.length <= head + tail + 1) return v
    return `${v.slice(0, head)}…${v.slice(-tail)}`
  }

  // Credential state and expiry still matter, but not enough to own a column:
  // they ride on the account name, where a hover finds them.
  function credentialHint(acc) {
    const parts = [acc.uid]
    parts.push(
      acc.has_refresh_token ? t.accounts.credentialRefreshable : t.accounts.credentialAccessOnly,
    )
    if (acc.expires_at) parts.push(t.accounts.expiresAt(fmtTime(acc.expires_at)))
    return parts.join(' · ')
  }

  // One write at a time per account. The label travels with the key so the
  // running line can name the operation instead of printing `checkin_all`.
  async function guard(uid, label, fn) {
    if (busyBatch || busyWrites[uid]) return
    busyWrites = { ...busyWrites, [uid]: label }
    try {
      await fn()
    } catch (err) {
      toastErr(err.message)
    } finally {
      const next = { ...busyWrites }
      delete next[uid]
      busyWrites = next
    }
  }

  // The pool decides which maintenance operations an account can take, so the
  // row renders exactly the buttons that will work and never offers one that
  // would fail.
  async function runAccountOp(acc, op) {
    const label = t.accounts.action[op] ?? op
    await guard(acc.uid, label, async () => {
      await api(`/accounts/${encodeURIComponent(acc.uid)}/${op}`, { method: 'POST' })
      await loadAccounts()
      toastOk(t.accounts.opDone(label, acc.label || acc.uid))
    })
  }

  // The overview's task entry is a jump into an account's task list, not a jump
  // to an empty page: the list is requested here so the landing frame already
  // has the rows it promised.
  async function openAccountTasks(acc) {
    expanded = { ...expanded, [acc.uid]: true }
    switchTab('tasks')
    if (!accountTasks[acc.uid]?.tasks?.length) await loadAccountTasks(acc.uid)
  }

  async function removeAccount(acc) {
    const label = acc.label || acc.uid
    const ok = await ask({
      title: t.common.remove,
      body: t.accounts.removeConfirm(label, acc.source),
      detail: acc.uid,
      confirmLabel: t.common.remove,
      tone: 'danger',
    })
    if (!ok) return
    // Removing a credential is a write on that account: it waits for, and
    // blocks, any operation already touching it.
    await guard(acc.uid, t.common.remove, async () => {
      await api('/accounts/remove', {
        method: 'POST',
        body: JSON.stringify({ key: acc.key }),
      })
      await loadAccounts()
      await loadSession()
      toastOk(t.accounts.removeDone(label))
    })
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
    // A pasted export is either a bare array or an object that carries one.
    const payload = { accounts: Array.isArray(parsed) ? parsed : parsed.accounts || [parsed] }
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

  // Adding an account is an action, not a destination: a top-bar button that
  // opens a dialog, with one tab per way of getting credentials in.
  let showAdd = $state(false)
  let addTab = $state('device') // device | token | batch

  // The token tab's three extra fields fold away: most pastes carry only the
  // access token, and a client that exported more opens them explicitly.
  let optOpen = $state(false)

  // The title under the tab strip names whichever way in is open — one line of
  // intent per tab, so switching tabs explains itself without a paragraph.
  const addIntro = $derived.by(() => {
    if (addTab === 'token') return { title: t.add.token.title, body: t.add.token.body }
    if (addTab === 'batch') return { title: t.importAcc.title, body: t.importAcc.body }
    return { title: t.add.device.title, body: t.add.device.body }
  })

  // Esc is the universal "never mind" for a dialog. The backdrop button covers
  // the pointer; this covers the keyboard. Everything else (Enter inside the
  // form, Tab cycling) is left to the browser and the focus trap.
  function onModalKeydown(e) {
    if (e.key === 'Escape') {
      e.stopPropagation()
      closeAdd()
    }
  }

  // What the footer offers depends on where the flow has got to, not just which
  // tab is open: device authorization moves through four phases and each one has
  // a different next step. The footer is the one place that never scrolls away.
  const addActions = $derived.by(() => {
    if (addTab === 'device') {
      if (authPhase === 'idle' || authPhase === 'error') {
        return [
          { label: t.common.cancel, kind: 'ghost', run: closeAdd },
          { label: t.add.device.start, kind: 'primary', run: startAuth },
        ]
      }
      if (authPhase === 'starting') {
        return [
          { label: t.add.device.starting, kind: 'primary', busy: true, run: () => {} },
          { label: t.common.cancel, kind: 'ghost', run: resetAuth },
        ]
      }
      if (authPhase === 'waiting') {
        return [
          { label: t.add.device.openLogin, kind: 'ghost', href: authUrl, icon: 'external' },
          { label: t.common.cancel, kind: 'ghost', run: resetAuth },
        ]
      }
      return [
        { label: t.add.device.again, kind: 'ghost', run: resetAuth },
        { label: t.common.done, kind: 'primary', run: closeAdd },
      ]
    }
    if (addTab === 'token') {
      return [
        { label: t.common.cancel, kind: 'ghost', run: closeAdd },
        {
          label: tokenPhase === 'running' ? t.add.token.submitting : t.add.token.submit,
          kind: 'primary',
          busy: tokenPhase === 'running',
          run: submitToken,
        },
      ]
    }
    return [
      { label: t.common.cancel, kind: 'ghost', run: closeAdd },
      {
        label: importPhase === 'running' ? t.importAcc.running : t.importAcc.submit,
        kind: 'primary',
        busy: importPhase === 'running',
        run: doImport,
      },
    ]
  })

  // The skip control moves focus to the page body without touching the route.
  let mainEl = $state(null)

  function focusMain() {
    mainEl?.focus({ preventScroll: false })
  }

  // The process log is the other top-bar action. It is opened and closed often
  // and read while something else is happening, so it is a dialog rather than
  // a place in the rail.
  let showLogs = $state(false)

  // ── process log ──────────────────────────────────────────────────────────
  // One standing connection for the life of the page, opened at mount rather
  // than when the panel opens. A stream that starts on open spends its first
  // second negotiating, which is exactly when the operator is waiting to see
  // whether the button they just pressed did anything.
  //
  // The list is the console's own copy, so opening a second time is instant and
  // closing loses nothing. Rows are a fixed 26px and only the visible window is
  // rendered: a 2000-line buffer must not be 2000 nodes while a sweep runs.
  const LOG_CAP = 2000
  const LOG_TRIM = 200 // drop in chunks, so a full rebuild is rare
  const LOG_ROW = 26
  const LOG_OVERSCAN = 20
  const LOG_CHANNELS = [
    { id: 'all', label: () => t.logStream.ch.all },
    { id: 'task', label: () => t.logStream.ch.task },
    { id: 'chat', label: () => t.logStream.ch.chat },
    { id: 'sys', label: () => t.logStream.ch.sys },
  ]
  const LOG_CH_LABEL = {
    task: () => t.logStream.ch.task,
    chat: () => t.logStream.ch.chat,
    sys: () => t.logStream.ch.sys,
  }

  // Levels are a threshold, not an exact match: picking 警告 means "warn and
  // worse", which is what someone reaching for the filter actually wants. 错误
  // is then the narrower pick.
  const LOG_LEVELS = [
    { id: 'all', label: () => t.logStream.level.all },
    { id: 'warn', label: () => t.logStream.level.warn },
    { id: 'error', label: () => t.logStream.level.error },
  ]
  const LEVEL_RANK = { info: 0, warn: 1, error: 2 }

  let logRecords = $state([])
  let visibleLogs = $state([])
  let logCounts = $state({ all: 0, task: 0, chat: 0, sys: 0 })
  let logChannel = $state('all')
  let logLevel = $state('all')
  // The stream is virtualised, so a row that was never rendered is invisible
  // to the browser's own find. This box is the only search there is.
  let logQuery = $state('')
  let logQ = $derived(logQuery.trim().toLowerCase())
  let logPinned = $state(true)
  let logConnected = $state(false)
  let logUnseen = $state(0)
  let logAnnounce = $state('')
  let logSelected = $state(null)
  let logEl = $state(null)
  let logView = $state({ top: 0, height: 0 })
  // A plain cursor, not state: it decides whether an incoming record is a
  // duplicate, and re-rendering on it would be pointless.
  let logSeq = 0
  let logSource = null

  const matchesChannel = (rec, channel) => channel === 'all' || rec.channel === channel

  function matchesView(rec) {
    if (!matchesChannel(rec, logChannel)) return false
    if (logLevel !== 'all' && (LEVEL_RANK[rec.level] ?? 0) < LEVEL_RANK[logLevel]) return false
    if (logQ && !String(rec.text || '').toLowerCase().includes(logQ)) return false
    return true
  }

  // Filtering is incremental: a new line is appended to the visible list when
  // it matches, and the whole buffer is only re-filtered when it is trimmed in
  // a chunk or the channel changes.
  function rebuildLogs() {
    visibleLogs = logRecords.filter(matchesView)
    const counts = { all: logRecords.length, task: 0, chat: 0, sys: 0 }
    for (const r of logRecords) counts[r.channel] = (counts[r.channel] || 0) + 1
    logCounts = counts
  }

  function onLogRecord(rec) {
    logRecords.push(rec)
    logCounts = {
      ...logCounts,
      all: logCounts.all + 1,
      [rec.channel]: (logCounts[rec.channel] || 0) + 1,
    }
    if (matchesView(rec)) visibleLogs.push(rec)
    if (!logPinned) logUnseen += 1
    if (logRecords.length > LOG_CAP + LOG_TRIM) {
      logRecords.splice(0, LOG_TRIM)
      rebuildLogs()
    }
  }

  function connectLogs() {
    disconnectLogs()
    const es = new EventSource(`${API}/logs/stream`)
    logSource = es
    es.onopen = () => (logConnected = true)
    // The browser retries on its own; this flag only drives the bar that says
    // so, and the retry button forces a fresh connection instead of waiting.
    es.onerror = () => (logConnected = false)
    es.onmessage = (ev) => {
      let rec
      try {
        rec = JSON.parse(ev.data)
      } catch {
        return
      }
      // A reconnect replays from Last-Event-ID; anything at or below the cursor
      // was already drawn.
      if (!rec || !(rec.seq > logSeq)) return
      logSeq = rec.seq
      onLogRecord(rec)
    }
  }

  function disconnectLogs() {
    if (logSource) {
      logSource.close()
      logSource = null
    }
  }

  function retryLogs() {
    logConnected = false
    connectLogs()
  }

  function setLogChannel(id) {
    if (logChannel === id) return
    logChannel = id
    logUnseen = 0
    rebuildLogs()
  }

  function setLogLevel(id) {
    if (logLevel === id) return
    logLevel = id
    rebuildLogs()
  }

  // Follow the tail only while the reader is at the tail. Scrolling up is a
  // deliberate "I am reading this" and must not be undone by the next line.
  $effect(() => {
    if (!showLogs || !logPinned || !logEl) return
    visibleLogs.length
    logEl.scrollTop = logEl.scrollHeight
  })

  // The window is measured rather than assumed: a drawer that is resized or
  // opened at a different height must not render the wrong number of rows.
  $effect(() => {
    if (!showLogs || !logEl) return
    const measure = () => {
      logView = { top: logEl.scrollTop, height: logEl.clientHeight }
    }
    const ro = new ResizeObserver(measure)
    ro.observe(logEl)
    measure()
    return () => ro.disconnect()
  })

  const logWindow = $derived.by(() => {
    const height = logView.height || 480
    const start = Math.max(0, Math.floor(logView.top / LOG_ROW) - LOG_OVERSCAN)
    const count = Math.ceil(height / LOG_ROW) + LOG_OVERSCAN * 2
    return { start, rows: visibleLogs.slice(start, start + count) }
  })

  function onLogScroll() {
    if (!logEl) return
    const fromBottom = logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight
    const atTail = fromBottom < 32
    if (atTail !== logPinned) {
      logPinned = atTail
      if (!atTail) logAnnounce = t.logStream.pausedAria(logUnseen)
    }
    logView = { top: logEl.scrollTop, height: logEl.clientHeight }
  }

  function jumpToLogTail() {
    logPinned = true
    logAnnounce = t.logStream.resumedAria(logUnseen)
    logUnseen = 0
    if (logEl) logEl.scrollTop = logEl.scrollHeight
  }

  function toggleLogFollow() {
    logPinned = !logPinned
    if (logPinned) {
      logAnnounce = t.logStream.resumedAria(logUnseen)
      logUnseen = 0
      if (logEl) logEl.scrollTop = logEl.scrollHeight
    } else {
      logAnnounce = t.logStream.pausedAria(logUnseen)
    }
  }

  // `f` is a shortcut only while the reader is inside the panel. Binding it
  // globally would swallow the key in every other input on the page.
  function onLogKeydown(e) {
    const el = e.target
    const typing =
      el instanceof HTMLElement &&
      (el.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(el.tagName))
    if (typing) return
    if (e.key === 'f' || e.key === 'F') {
      e.preventDefault()
      toggleLogFollow()
    }
  }

  function downloadLogs() {
    const rows = visibleLogs.map((r) => {
      const ch = LOG_CH_LABEL[r.channel] ? `[${LOG_CH_LABEL[r.channel]()}] ` : ''
      return `${fmtClock(r.ts)} [${r.level}] ${ch}${r.text}`
    })
    if (!rows.length) {
      toastErr(t.logStream.empty)
      return
    }
    const blob = new Blob([rows.join('\n') + '\n'], { type: 'text/plain;charset=utf-8' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `codenexus-log-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '')}.txt`
    a.click()
    URL.revokeObjectURL(url)
  }

  function openAdd() {
    addTab = 'device'
    optOpen = false
    showAdd = true
  }

  function openLogs() {
    // Opening the log is a request for the newest lines, so it always lands at
    // the tail even if the last visit was paused mid-scroll.
    logPinned = true
    logUnseen = 0
    logSelected = null
    showLogs = true
  }

  function closeLogs() {
    showLogs = false
    logSelected = null
  }

  function closeAdd() {
    showAdd = false
    optOpen = false
    // A half-finished authorization must not survive the dialog: polling would
    // otherwise keep running against an endpoint nobody is looking at.
    stopPolling()
    resetAuth()
    tokenPhase = 'idle'
    tokenMsg = ''
    localPhase = 'idle'
    localMsg = ''
  }

  function switchTab(name, { push = true } = {}) {
    if (!TABS.includes(name)) name = 'overview'
    const changed = tab !== name
    if (changed) window.scrollTo({ top: 0 })
    tab = name
    // A.4: the page with a pinned control bar needs a taller scroll pad, and
    // the rule lives on <html> — it is the scroll container.
    document.documentElement.dataset.page = name
    if (changed) {
      resetAuth()
      tokenPhase = 'idle'
      tokenMsg = ''
    }
    if (push && location.hash !== `#/${name}`) {
      history.pushState({ tab: name }, '', `#/${name}`)
    }
    ensureTabData(name)
  }

  // A visit paints whatever is still fresh and asks the gateway only for the
  // rest. Credits are the exception that proves the rule: a balance read costs
  // one upstream call per account, so it is re-read at most every five minutes.
  function ensureTabData(name) {
    if (name === 'overview') {
      if (!fresh('overview')) loadOverview()
    } else if (name === 'usage') {
      if (!fresh('usage')) loadUsage()
    } else if (name === 'tasks') {
      if (!fresh('tasks')) loadQueue()
    } else if (name === 'models') {
      if (!modelRows.length) loadModels()
    } else if (name === 'credits') {
      if (!creditRows.length) creditRows = creditSnapshot()
      if (!fresh('credits')) loadCredits()
    }
  }

  function onPopState() {
    const next = tabFromHash()
    if (next !== tab) switchTab(next, { push: false })
  }

  // A hidden tab keeps no timers and no stream. The console usually shares a
  // screen with an editor, and a background panel must cost nothing upstream.
  function onVisibility() {
    if (document.hidden) {
      stopOverviewPolling()
      stopPolling()
      disconnectLogs()
    } else {
      startOverviewPolling()
      if (authPhase === 'waiting') startPolling()
      connectLogs()
      ensureTabData(tab)
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
  // Yesterday at the same clock. Absent when the store has no baseline, which
  // is different from a baseline of zero: zero would divide by nothing.
  let overviewPrev = $state(null)

  async function loadOverview() {
    overviewLoading = true
    const jobs = [
      loadAccounts(),
      api('/observability/metrics').then((m) => (overviewMetrics = m)).catch(() => (overviewMetrics = null)),
      api('/tasks/queue').then((q) => (overviewQueue = q.queue ?? [])).catch(() => (overviewQueue = [])),
      api('/usage/summary?days=1')
        .then((u) => {
          overviewUsage = u.available === false ? null : (u.summary ?? null)
          overviewPrev = u.available === false ? null : (u.prev ?? null)
        })
        .catch(() => {
          overviewUsage = null
          overviewPrev = null
        }),
    ]
    await Promise.all(jobs)
    overviewLoading = false
    loadedAt = { ...loadedAt, overview: Date.now() }
  }

  // ── overview: pool bar, filter, sort ─────────────────────────────────────
  let healthFilter = $state('') // a bucket id, or '' for the whole pool
  let collapsedGroups = $state({})
  let sortKey = $state('')
  let sortDir = $state('')

  // One loop feeds the four health counts, so the parts always add up to the
  // pool. Idle counts as usable: an account that has never served is available,
  // it just has no track record yet. The band shows four counts — the rules
  // page dropped 暂停 from the ladder — but the filter still reaches every
  // account: 停用 merges the off and paused buckets, because a count you can
  // display but not filter is a count that lies about the pool.
  const poolSegments = $derived.by(() => {
    const n = { ok: 0, cooling: 0, broken: 0, off: 0, paused: 0 }
    for (const acc of accounts) n[health(acc).bucket]++
    return [
      { id: 'ok', label: t.pool.segment.ok, n: n.ok, buckets: ['ok'] },
      { id: 'cooling', label: t.pool.segment.cooling, n: n.cooling, buckets: ['cooling'] },
      { id: 'broken', label: t.pool.segment.broken, n: n.broken, buckets: ['broken'] },
      { id: 'off', label: t.pool.segment.off, n: n.off + n.paused, buckets: ['off', 'paused'] },
    ]
  })

  const filterLabel = $derived(poolSegments.find((s) => s.id === healthFilter)?.label ?? '')
  const filterBuckets = $derived(poolSegments.find((s) => s.id === healthFilter)?.buckets ?? null)

  function toggleHealthFilter(id) {
    healthFilter = healthFilter === id ? '' : id
  }

  // Sorting is local and per group: an account's rank inside its own source is
  // the only comparison that means anything, because the two upstreams meter
  // different things.
  const SORT_PICK = {
    credits: (a) => (a.unlimited ? Number.POSITIVE_INFINITY : a.credits_known ? a.credits : null),
    calls: (a) => a.success,
    usage: (a) => a.total_tokens || 0,
    last: (a) => a.last_success_at || null,
  }

  function cycleSort(key) {
    if (sortKey !== key) {
      sortKey = key
      sortDir = 'desc'
    } else if (sortDir === 'desc') {
      sortDir = 'asc'
    } else {
      sortKey = ''
      sortDir = ''
    }
  }

  function sortState(key) {
    if (sortKey !== key || !sortDir) return 'none'
    return sortDir === 'asc' ? 'ascending' : 'descending'
  }

  function toggleGroup(source) {
    collapsedGroups = { ...collapsedGroups, [source]: !collapsedGroups[source] }
  }

  const groupsView = $derived.by(() =>
    grouped
      .map((g) => {
        const matching = filterBuckets
          ? g.items.filter((a) => filterBuckets.includes(health(a).bucket))
          : g.items
        let items = matching
        if (sortKey && sortDir) {
          const pick = SORT_PICK[sortKey]
          items = [...matching].sort((a, b) => {
            const av = pick(a)
            const bv = pick(b)
            // A missing reading is not a small one. It sinks to the bottom in
            // both directions, so "never measured" cannot pass for zero.
            if (av == null && bv == null) return 0
            if (av == null) return 1
            if (bv == null) return -1
            const d = av === bv ? 0 : av < bv ? -1 : 1
            return sortDir === 'asc' ? d : -d
          })
        }
        return {
          source: g.source,
          items,
          total: g.items.length,
          credit: g.items.reduce((s, a) => s + (a.unlimited ? 0 : a.credits || 0), 0),
          unlimited: g.items.some((a) => a.unlimited),
          known: g.items.some((a) => a.credits_known),
        }
      })
      .filter((g) => g.items.length > 0),
  )

  const todayDelta = $derived.by(() => {
    if (!overviewUsage || !overviewPrev) return null
    const base = overviewPrev.total_tokens || 0
    if (base <= 0) return null
    return (overviewUsage.total_tokens - base) / base
  })

  function fmtDelta(v) {
    const pct = Math.abs(v * 100)
    const shown = pct >= 10 ? Math.round(pct) : Math.round(pct * 10) / 10
    if (shown === 0) return '±0%'
    return `${v > 0 ? '↑' : '↓'} ${shown}%`
  }
  const creditsTotal = $derived(
    accounts.reduce((sum, a) => sum + (a.unlimited ? 0 : a.credits || 0), 0),
  )
  const hasUnlimited = $derived(accounts.some((a) => a.unlimited))

  // Credits are per-platform, and the band shows both platforms whether or not
  // the pool currently holds an account for them: a missing source reads "—",
  // never an absent row. WorkBuddy and Trae meter different things into
  // different packages, so one pooled total would be a number that answers no
  // question anyone has. Each source keeps its own subtotal instead.
  const creditsBySource = $derived.by(() => {
    const bySrc = new Map()
    for (const acc of accounts) {
      const key = acc.source === 'trae' ? 'trae-cn' : acc.source
      if (!bySrc.has(key)) bySrc.set(key, [])
      bySrc.get(key).push(acc)
    }
    return ['workbuddy', 'trae-cn'].map((source) => {
      const items = bySrc.get(source) || []
      return {
        source,
        total: items.reduce((s, a) => s + (a.unlimited ? 0 : a.credits || 0), 0),
        unlimited: items.some((a) => a.unlimited),
        known: items.some((a) => a.credits_known),
      }
    })
  })

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

  // Service facts come from the session endpoint. They are constants of the
  // running process, so they are read once rather than polled.
  const service = $derived.by(() => {
    const cap = session?.capabilities ?? {}
    return {
      version: session?.version ?? '',
      uptime: session?.uptime ?? 0,
      listen: session?.listen ?? '',
      sticky_sessions: cap.sticky_sessions,
      checkin_enabled: cap.checkin_enabled,
      max_mode_enabled: cap.max_mode,
    }
  })

  // The expiry card is a snapshot by default. Probing every package costs an
  // upstream round trip per account, so it happens only when asked for; the
  // result is shared for two minutes before another click is honoured.
  let expiryProbing = $state(false)
  let expiryProbedAt = $state(0)

  async function probeExpiry() {
    const now = Date.now()
    if (expiryProbing || now - expiryProbedAt < 120_000) return
    expiryProbing = true
    try {
      await api('/accounts/balance_all', { method: 'POST' })
      expiryProbedAt = Date.now()
      await loadAccounts()
    } catch (err) {
      toastErr(err.message)
    } finally {
      expiryProbing = false
    }
  }

  // Service state, in the order the reference console reports it. Rows whose
  // backend value is absent stay listed and read "—" rather than vanishing:
  // a row that disappears looks like a feature that was removed.
  const serviceRows = $derived.by(() => {
    const onOff = (v) => (v ? t.overview.serviceBox.on : t.overview.serviceBox.off)
    const rows = [
      { label: t.overview.serviceBox.version, text: service?.version ?? '', missing: !service?.version },
      { label: t.overview.serviceBox.uptime, text: service?.uptime ? fmtDuration(service.uptime) : '', missing: !service?.uptime },
      { label: t.overview.serviceBox.remote, text: service?.listen ?? '', missing: !service?.listen },
      { label: t.overview.serviceBox.sources, text: (session?.sources ?? []).join(' · '), missing: !(session?.sources ?? []).length },
      { label: t.overview.serviceBox.inFlight, text: overviewMetrics?.in_flight != null ? String(overviewMetrics.in_flight) : '', missing: overviewMetrics?.in_flight == null },
      { label: t.overview.serviceBox.sticky, text: session?.sticky_sessions != null ? onOff(session.sticky_sessions) : '', missing: session?.sticky_sessions == null },
      { label: t.overview.serviceBox.reqlog, text: overviewMetrics != null ? onOff(overviewMetrics.enabled) : '', missing: overviewMetrics == null },
      { label: t.overview.serviceBox.usage, text: overviewUsage ? onOff(true) : '', missing: !overviewUsage },
      { label: t.overview.serviceBox.checkin, text: service?.checkin_enabled != null ? onOff(service.checkin_enabled) : '', missing: service?.checkin_enabled == null },
      { label: t.overview.serviceBox.max, text: service?.max_mode_enabled != null ? onOff(service.max_mode_enabled) : '', missing: service?.max_mode_enabled == null },
    ]
    return rows
  })

  const queuePending = $derived(
    overviewQueue.reduce((sum, row) => sum + (row.pending || 0), 0),
  )

  // How many service facts the panel would show as a dash. The fold turns that
  // into one phrase, so the summary never invents a health state.
  const serviceMissing = $derived(serviceRows.filter((r) => r.missing).length)

  // ── models page ──────────────────────────────────────────────────────────
  let modelRows = $state([])
  let modelFailed = $state({})
  let modelCatalogued = $state({})
  let modelsLoading = $state(false)
  let modelsQuery = $state('')
  let modelsCap = $state('')

  async function loadModels() {
    modelsLoading = true
    try {
      const body = await api('/models')
      modelRows = body.models ?? []
      modelFailed = body.failed ?? {}
      modelCatalogued = body.accounts ?? {}
      clearSection('models')
    } catch (err) {
      if (!isStale(err)) setSection('models', err.message)
    } finally {
      modelsLoading = false
    }
  }

  // Models sort on the two numbers a caller actually compares: how much fits
  // in the window and what a turn costs. Both columns cycle asc → desc → off
  // like the ledger's, and a missing reading sinks in both directions.
  let modelSortKey = $state('') // 'context' | 'rate'
  let modelSortDir = $state('')

  function cycleModelSort(key) {
    if (modelSortKey !== key) {
      modelSortKey = key
      modelSortDir = 'desc'
    } else if (modelSortDir === 'desc') {
      modelSortDir = 'asc'
    } else {
      modelSortKey = ''
      modelSortDir = ''
    }
  }

  function modelSortState(key) {
    if (modelSortKey !== key || !modelSortDir) return 'none'
    return modelSortDir === 'asc' ? 'ascending' : 'descending'
  }

  // The rate is catalogue wording ("1.2x", "0.8 credits") rather than a
  // number; sort on the leading figure and leave the wording for the eye.
  function rateValue(m) {
    const raw = m.promo_credits || m.credits || ''
    const n = parseFloat(String(raw))
    return Number.isFinite(n) ? n : null
  }

  const MODEL_SORT = {
    context: (m) => (m.context_length > 0 ? m.context_length : null),
    rate: (m) => rateValue(m),
  }

  const filteredModels = $derived.by(() => {
    const q = modelsQuery.trim().toLowerCase()
    const rows = modelRows.filter((m) => {
      if (q && !m.id.toLowerCase().includes(q)) return false
      if (!modelsCap) return true
      // The reasoning filter asks whether the model takes a strength at all,
      // which is a different claim from carrying the capability bit.
      if (modelsCap === 'reasoning') return (m.thinking_efforts ?? []).length > 0
      return (m.capabilities ?? []).includes(modelsCap)
    })
    if (!modelSortKey || !modelSortDir) return rows
    const pick = MODEL_SORT[modelSortKey]
    return [...rows].sort((a, b) => {
      const av = pick(a)
      const bv = pick(b)
      if (av == null && bv == null) return 0
      if (av == null) return 1
      if (bv == null) return -1
      const d = av === bv ? 0 : av < bv ? -1 : 1
      return modelSortDir === 'asc' ? d : -d
    })
  })

  // ── platform cards ───────────────────────────────────────────────────────
  // The page answers "which platform offers what", so the catalogue is split
  // into one card per platform. Sorting happens above, on the whole set, and
  // each card keeps that order: a caller comparing two windows wants the same
  // ordering on both sides of the page.
  const PLATFORM_ORDER = ['workbuddy', 'trae-cn']
  const PLATFORM_LABEL = { workbuddy: 'WorkBuddy', 'trae-cn': 'Trae' }

  let collapsedPlatforms = $state({})

  function togglePlatform(source) {
    collapsedPlatforms = { ...collapsedPlatforms, [source]: !collapsedPlatforms[source] }
  }

  const modelPlatforms = $derived.by(() => {
    const sources = []
    for (const row of modelRows) {
      const src = row.source || 'unknown'
      if (!sources.includes(src)) sources.push(src)
    }
    sources.sort((a, b) => {
      const ia = PLATFORM_ORDER.indexOf(a)
      const ib = PLATFORM_ORDER.indexOf(b)
      const ra = ia < 0 ? PLATFORM_ORDER.length : ia
      const rb = ib < 0 ? PLATFORM_ORDER.length : ib
      return ra !== rb ? ra - rb : a.localeCompare(b)
    })

    const visible = new Map()
    for (const row of filteredModels) {
      const src = row.source || 'unknown'
      const bucket = visible.get(src)
      if (bucket) bucket.push(row)
      else visible.set(src, [row])
    }

    // A platform the filter emptied keeps its card: a card that disappears
    // reads as "this platform has no models", which is a different claim.
    return sources.map((source) => ({
      source,
      label: PLATFORM_LABEL[source] || source,
      rows: visible.get(source) ?? [],
      total: modelRows.filter((row) => (row.source || 'unknown') === source).length,
      accounts: modelCatalogued[source] ?? 0,
    }))
  })

  // ── connectivity test ────────────────────────────────────────────────────
  // The test sends a real, tiny completion. Only a real request answers "does
  // this model work on this account right now", and the price of asking is
  // small but not zero — which is why the form states it before the button.
  //
  // One model per run, picked from the catalogue above: a name no account
  // carries cannot be tested, and the capabilities that catalogue already
  // states are what gate the content row. Nothing is preselected — the first
  // request spends credits, so the choice stays explicit.
  let probeModel = $state('') // "source/id", the key of the picked row
  let probeModes = $state(['text'])
  let probeThinking = $state(false)
  let probeEffort = $state('') // '' leaves the strength to the model itself
  let probeTimeout = $state(30)
  let probeRunning = $state(false)
  let probeElapsed = $state(0) // live ms since the run began
  let probeStep = $state(-1) // how many content calls have landed
  let probeLast = $state(null) // the finished run, for the row and the drawer
  let probeDrawer = $state(false)
  let probeMsg = $state('')

  // Two anchored surfaces, one behaviour each: the model picker and the
  // timeout menu. Both are `position: fixed` — a card that clips overflow
  // must not clip them — and both close when their trigger moves or a
  // pointer lands anywhere else.
  let probePickerOpen = $state(false)
  let probePickerQuery = $state('')
  let probePickerIndex = $state(0)
  let probePickerPos = $state({ top: null, bottom: null, left: 0, ready: false })
  let probeTriggerEl = $state(null)
  let probeMenuEl = $state(null)
  let probeSearchEl = $state(null)
  let probeAnchorTop = 0
  let probeAnchorLeft = 0
  let probeTimeoutMenu = $state(false)
  let probeTimeoutDraft = $state('')
  let probeTimeoutPos = $state({ top: null, bottom: null, left: 0, ready: false })
  let probeTimeoutEl = $state(null)
  let probeTimeoutMenuEl = $state(null)
  let probeTimeoutInputEl = $state(null)
  // The one live anchor watch, whichever surface owns it right now.
  let probeAnchorWatch = null

  const PROBE_TIMEOUTS = [15, 30, 60]

  const probeRow = $derived(
    modelRows.find((m) => `${m.source || 'unknown'}/${m.id}` === probeModel) ?? null,
  )
  const probeCaps = $derived(probeRow?.capabilities ?? [])
  const probeEfforts = $derived(probeRow?.thinking_efforts ?? [])
  // What this model can be asked. Every catalogue carries the chat bit, so a
  // model without it is a catalogue bug, not a gated option — but the gate
  // reads the same either way.
  const probeCan = $derived({
    text: probeCaps.includes('chat'),
    tool: probeCaps.includes('tools'),
    thinking: probeCaps.includes('reasoning'),
    effort: probeEfforts.length > 0,
  })
  const probeModeLabel = (mode) => (mode === 'tool' ? t.models.test.modeTool : t.models.test.modeText)
  const probeSrcPrefix = (row) => `${row.source || 'unknown'}/`

  const probeGroups = $derived.by(() => {
    const q = probePickerQuery.trim().toLowerCase()
    const hit = (m) => {
      if (!q) return true
      const label = PLATFORM_LABEL[m.source] || m.source || ''
      return m.id.toLowerCase().includes(q) || label.toLowerCase().includes(q)
    }
    const groups = []
    for (const platform of modelPlatforms) {
      const rows = modelRows.filter((m) => (m.source || 'unknown') === platform.source && hit(m))
      if (rows.length) groups.push({ source: platform.source, label: platform.label, rows })
    }
    return groups
  })
  // One flat order behind the groups: the keyboard walks models, not headers.
  const probeOptions = $derived(
    probeGroups.flatMap((g) => g.rows.map((m) => `${m.source || 'unknown'}/${m.id}`)),
  )

  function toggleProbeMode(mode) {
    probeModes = probeModes.includes(mode)
      ? probeModes.filter((item) => item !== mode)
      : [...probeModes, mode]
  }

  function pickProbeModel(key) {
    probeModel = key
    // A different model is a different contract: the content row, the
    // thinking switch and the strength all reset to what the new row states.
    const caps = modelRows.find((m) => `${m.source || 'unknown'}/${m.id}` === key)?.capabilities ?? []
    probeModes = caps.includes('chat') ? ['text'] : []
    probeThinking = false
    probeEffort = ''
    closeProbePicker()
  }

  async function runProbe() {
    if (probeRunning) return
    const row = probeRow
    if (!row) {
      probeMsg = t.models.test.needModel
      return
    }
    const modes = probeModes.length ? probeModes : ['text']
    const account = row.account_keys?.[0] || ''
    probeRunning = true
    probeMsg = ''
    probeElapsed = 0
    probeStep = 0
    const steps = []
    try {
      // Serial, one content at a time: two probes on the same account at once
      // is how an upstream rate limit gets mistaken for a dead model.
      for (const mode of modes) {
        let result
        try {
          result = await api('/models/test', {
            method: 'POST',
            body: JSON.stringify({
              model: row.id,
              mode,
              effort: probeEffort,
              thinking: probeThinking,
              timeout: probeTimeout,
              account,
            }),
          })
        } catch (err) {
          result = { model: row.id, mode, ok: false, error: err.message, elapsed_ms: 0 }
        }
        steps.push(result)
        probeStep = steps.length
      }
      probeLast = {
        ok: steps.every((s) => s.ok),
        model: row.id,
        source: row.source || 'unknown',
        modes: [...modes],
        effort: probeEffort,
        thinking: probeThinking,
        timeout: probeTimeout,
        account: steps[0]?.account || account,
        steps,
        elapsed_ms: steps.reduce((sum, s) => sum + (s.elapsed_ms || 0), 0),
        credits: steps.reduce((sum, s) => sum + (s.credits || 0), 0),
        at: Date.now(),
      }
    } finally {
      probeRunning = false
      probeStep = -1
    }
  }

  // The live seconds ride the button: a run that outlasts the operator's
  // patience has already answered "not usable", and the clock is what says so.
  $effect(() => {
    if (!probeRunning) return
    const started = Date.now()
    const id = setInterval(() => (probeElapsed = Date.now() - started), 100)
    return () => clearInterval(id)
  })

  // ── picker placement ─────────────────────────────────────────────────────
  // Anchor on the edge that faces the trigger, never on the far one. A panel
  // whose height settles after the first measurement — a web font landing, a
  // scrollbar appearing, a child styled one frame late — then grows away from
  // the trigger instead of drifting into the very control it belongs to. The
  // flipped panel pins its bottom; the dropped one pins its top.
  function probePlaceAt(anchorEl, surfaceEl, width) {
    const rect = anchorEl.getBoundingClientRect()
    const height = surfaceEl.offsetHeight
    const gap = 6
    const below = rect.bottom + gap
    // Open upwards when the panel would run past the viewport — the test card
    // sits low on a long models page, and a clipped menu looks broken.
    const flip = below + height > window.innerHeight - 8 && rect.top - gap - height > 8
    const left = Math.max(8, Math.min(rect.left, window.innerWidth - width - 8))
    if (flip) {
      return {
        top: null,
        bottom: Math.round(window.innerHeight - rect.top + gap),
        left: Math.round(left),
      }
    }
    return { top: Math.round(below), bottom: null, left: Math.round(left) }
  }

  function watchProbeAnchor(anchorEl, surfaceEl, onMoved) {
    stopProbeAnchorWatch()
    const rect = anchorEl.getBoundingClientRect()
    probeAnchorTop = rect.top
    probeAnchorLeft = rect.left
    const dismiss = () => {
      const r = anchorEl.getBoundingClientRect()
      if (!r || Math.abs(r.top - probeAnchorTop) > 1 || Math.abs(r.left - probeAnchorLeft) > 1) onMoved()
    }
    const outside = (e) => {
      if (surfaceEl?.contains(e.target) || anchorEl.contains(e.target)) return
      onMoved()
    }
    window.addEventListener('scroll', dismiss, true)
    window.addEventListener('resize', dismiss)
    document.addEventListener('pointerdown', outside, true)
    probeAnchorWatch = { dismiss, outside }
  }

  function stopProbeAnchorWatch() {
    if (!probeAnchorWatch) return
    window.removeEventListener('scroll', probeAnchorWatch.dismiss, true)
    window.removeEventListener('resize', probeAnchorWatch.dismiss)
    document.removeEventListener('pointerdown', probeAnchorWatch.outside, true)
    probeAnchorWatch = null
  }

  function openProbePicker() {
    if (probeRunning) return
    probeTimeoutMenu = false
    probePickerQuery = ''
    const at = probeOptions.indexOf(probeModel)
    probePickerIndex = at >= 0 ? at : 0
    probePickerPos = { ...probePickerPos, ready: false }
    probePickerOpen = true
    tick().then(() => {
      if (!probePickerOpen) return
      watchProbeAnchor(probeTriggerEl, probeMenuEl, () => closeProbePicker(false))
      probeSearchEl?.focus({ preventScroll: true })
    })
  }

  // Placement runs twice: once as soon as the panel is in the tree, once more
  // after the next layout settle. A first measure taken while the panel is
  // still settling must not leave the panel drifting down into its own
  // trigger — the anchor relationship is the whole contract of a dropdown.
  function probePlacePicker() {
    if (!probeTriggerEl || !probeMenuEl) return
    const pos = probePlaceAt(probeTriggerEl, probeMenuEl, probeTriggerEl.offsetWidth)
    probePickerPos = { ...pos, ready: true }
    tick().then(() => {
      if (!probePickerOpen || !probeMenuEl) return
      const again = probePlaceAt(probeTriggerEl, probeMenuEl, probeTriggerEl.offsetWidth)
      if (
        again.top !== probePickerPos.top ||
        again.bottom !== probePickerPos.bottom ||
        again.left !== probePickerPos.left
      ) {
        probePickerPos = { ...again, ready: true }
      }
    })
  }

  $effect(() => {
    if (probePickerOpen) probePlacePicker()
  })

  function closeProbePicker(returnFocus = true) {
    if (!probePickerOpen) return
    probePickerOpen = false
    stopProbeAnchorWatch()
    if (returnFocus) probeTriggerEl?.focus({ preventScroll: true })
  }

  // The keyboard walks the flat option list; the group headers are labels,
  // not stops. Escape bubbles to the window chain, which closes the picker.
  function probePickerKey(e) {
    const n = probeOptions.length
    const at = Math.max(0, Math.min(probePickerIndex, n - 1))
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault()
      if (!n) return
      probePickerIndex = (at + (e.key === 'ArrowDown' ? 1 : -1) + n) % n
    } else if (e.key === 'Enter') {
      e.preventDefault()
      const key = probeOptions[at]
      if (key) pickProbeModel(key)
    }
  }

  $effect(() => {
    if (!probePickerOpen || !probeMenuEl) return
    probeMenuEl
      .querySelector(`[data-probe-option="${Math.max(0, Math.min(probePickerIndex, probeOptions.length - 1))}"]`)
      ?.scrollIntoView({ block: 'nearest' })
  })

  function openProbeTimeoutMenu() {
    if (probeRunning) return
    probePickerOpen = false
    probeTimeoutDraft = ''
    probeTimeoutPos = { ...probeTimeoutPos, ready: false }
    probeTimeoutMenu = true
    tick().then(() => {
      if (!probeTimeoutMenu) return
      watchProbeAnchor(probeTimeoutEl, probeTimeoutMenuEl, () => closeProbeTimeoutMenu(false))
    })
  }

  function probePlaceTimeoutMenu() {
    if (!probeTimeoutEl || !probeTimeoutMenuEl) return
    const pos = probePlaceAt(probeTimeoutEl, probeTimeoutMenuEl, 128)
    probeTimeoutPos = { ...pos, ready: true }
    tick().then(() => {
      if (!probeTimeoutMenu || !probeTimeoutMenuEl) return
      const again = probePlaceAt(probeTimeoutEl, probeTimeoutMenuEl, 128)
      if (
        again.top !== probeTimeoutPos.top ||
        again.bottom !== probeTimeoutPos.bottom ||
        again.left !== probeTimeoutPos.left
      ) {
        probeTimeoutPos = { ...again, ready: true }
      }
    })
  }

  // The panel's geometry as one style string: exactly one of top/bottom is a
  // number, the other stays auto, so the anchored edge is the only one that
  // can move. Ready gates visibility — a panel measured at 0,0 must not flash
  // in the corner before it is placed.
  const probePickerCss = $derived(
    [
      probePickerPos.top == null ? 'top:auto' : `top:${probePickerPos.top}px`,
      probePickerPos.bottom == null ? 'bottom:auto' : `bottom:${probePickerPos.bottom}px`,
      `left:${probePickerPos.left}px`,
      `width:${probeTriggerEl?.offsetWidth || 400}px`,
      `visibility:${probePickerPos.ready ? 'visible' : 'hidden'}`,
    ].join(';'),
  )

  const probeTimeoutCss = $derived(
    [
      probeTimeoutPos.top == null ? 'top:auto' : `top:${probeTimeoutPos.top}px`,
      probeTimeoutPos.bottom == null ? 'bottom:auto' : `bottom:${probeTimeoutPos.bottom}px`,
      `left:${probeTimeoutPos.left}px`,
      'width:128px',
      `visibility:${probeTimeoutPos.ready ? 'visible' : 'hidden'}`,
    ].join(';'),
  )

  $effect(() => {
    if (probeTimeoutMenu) probePlaceTimeoutMenu()
  })

  function closeProbeTimeoutMenu(returnFocus = true) {
    if (!probeTimeoutMenu) return
    probeTimeoutMenu = false
    stopProbeAnchorWatch()
    if (returnFocus) probeTimeoutEl?.focus({ preventScroll: true })
  }

  function clampProbeTimeout(v) {
    const n = Math.round(Number(v))
    if (!Number.isFinite(n)) return probeTimeout
    return Math.max(10, Math.min(600, n))
  }

  function pickProbeTimeout(v) {
    probeTimeout = clampProbeTimeout(v)
    closeProbeTimeoutMenu()
  }

  function commitProbeTimeoutDraft() {
    const v = probeTimeoutDraft.trim()
    if (v) probeTimeout = clampProbeTimeout(v)
    probeTimeoutDraft = ''
    closeProbeTimeoutMenu()
  }

  // The timeout the backend enforces is the whole bound, not the first byte:
  // the probe waits for one complete answer.
  function probeReason(step) {
    const err = step?.error || ''
    const m = /timed out after (\d+)s/.exec(err)
    return m ? t.models.test.timeoutReason(Number(m[1])) : err
  }

  // "今天 14:22" reads as a log line; anything older carries its date, and the
  // title keeps the full stamp either way (A.2).
  function fmtDayTime(value) {
    const d = new Date(value)
    if (Number.isNaN(d.getTime())) return t.common.dash
    const p = (n) => String(n).padStart(2, '0')
    const hm = `${p(d.getHours())}:${p(d.getMinutes())}`
    const midnight = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime()
    const days = Math.round((midnight(new Date()) - midnight(d)) / 86400000)
    if (days === 0) return `今天 ${hm}`
    if (days === 1) return `昨天 ${hm}`
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${hm}`
  }

  // ── credits page ─────────────────────────────────────────────────────────
  let creditRows = $state([])
  let creditsLoading = $state(false)
  // A balance read is one upstream query per account, so the first pass is
  // slow enough to need an outline. Later refreshes already have rows and keep
  // them on screen — swapping them for a skeleton would throw away the answer
  // to show a question.
  const creditsFirstLoad = $derived(creditsLoading && creditRows.length === 0)
  let creditsErr = $state('')
  // When the balances on screen were read. A credit balance is a snapshot of
  // an upstream state, so the page states its age instead of pretending to be
  // live; that is also why this page is not polled on a timer.
  let creditsReadAt = $state(0)

  // The pool already knows each account's last observed balance from routing,
  // so the page opens on that instead of a blank screen: a credit read is one
  // upstream call per account, and waiting for all of them before showing
  // anything would make the page feel broken on a slow upstream.
  function creditSnapshot() {
    return accounts.map((a) => ({
      key: a.key,
      uid: a.uid,
      source: a.source,
      label: a.label,
      local: a.local,
      unlimited: a.unlimited,
      known: a.credits_known,
      remain: a.credits || 0,
      total: a.credits_total || a.credits || 0,
      used: Math.max(0, (a.credits_total || 0) - (a.credits || 0)),
      earliest: a.earliest_expiry || 0,
      snapshot: true,
      packs: [],
    }))
  }

  async function loadCredits() {
    if (!creditRows.length) creditRows = creditSnapshot()
    creditsLoading = true
    creditsErr = ''
    try {
      const body = await api('/credits')
      creditRows = body.accounts ?? []
      creditsReadAt = Date.now()
      loadedAt = { ...loadedAt, credits: Date.now() }
    } catch (err) {
      if (!isStale(err)) creditsErr = err.message
    } finally {
      creditsLoading = false
    }
  }

  function creditDayKey(sec) {
    const d = new Date(sec * 1000)
    return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
  }

  // Calendar days to lapse, not hours: "3 天后" is the unit a person acts on,
  // and a calendar day keeps buckets comparable across accounts read at
  // different times. A batch that lapsed with credits still on it stays in the
  // chart as its own bucket, so an unused grant that already died is visible
  // instead of quietly leaving the total.
  function creditDaysLeft(sec) {
    const today = new Date()
    today.setHours(0, 0, 0, 0)
    const day = new Date(sec * 1000)
    day.setHours(0, 0, 0, 0)
    return Math.round((day - today) / 86400000)
  }

  function packExpiryText(sec) {
    if (!sec) return t.common.dash
    const days = creditDaysLeft(sec)
    return `${fmtExpiryDay(sec)} · ${days < 0 ? t.credits.expiry.expired : t.credits.expiry.days(days)}`
  }

  // The batch table prints the grant to the minute, not the second: the minute
  // is what tells two batches apart, and a year would eat a column that only
  // ever shows this month's grants.
  function fmtGrant(sec) {
    if (!sec) return t.common.dash
    const d = new Date(sec * 1000)
    const p = (n) => String(n).padStart(2, '0')
    return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
  }

  // One row per remaining-day bucket, both sources on a shared scale so the two
  // platforms can be compared inside a row and across rows. Only packs with a
  // stated lapse date contribute: a grant without one cannot be placed on the
  // axis, and inventing a date would draw credits that never expire.
  const creditExpiry = $derived.by(() => {
    const perDay = new Map()
    let undated = 0
    let undatedCount = 0
    for (const row of creditRows) {
      const src = row.source === 'trae' ? 'trae-cn' : row.source || 'unknown'
      const packs = row.packs ?? []
      if (!packs.length) continue
      // Packs can add up to more than the account's stated balance: an upstream
      // lists a grant twice, or mirrors a batch already drawn down. Filling
      // from the nearest lapse date and stopping at the balance keeps the axis
      // from drawing credits that are not there; undated packs take the rest.
      let budget = row.remain > 0 ? row.remain : Number.POSITIVE_INFINITY
      const dated = packs
        .filter((pack) => pack.expires_at && pack.remain > 0)
        .sort((a, b) => a.expires_at - b.expires_at)
      const undatedPacks = packs.filter((pack) => !pack.expires_at && pack.remain > 0)
      for (const pack of [...dated, ...undatedPacks]) {
        if (budget <= 0) break
        const take = Math.min(pack.remain || 0, budget)
        budget -= take
        if (!pack.expires_at) {
          undated += take
          undatedCount += 1
          continue
        }
        const days = Math.max(-1, creditDaysLeft(pack.expires_at))
        const bucket = perDay.get(days) ?? {}
        bucket[src] = (bucket[src] || 0) + take
        perDay.set(days, bucket)
      }
    }
    const buckets = [...perDay.entries()]
      .sort((x, y) => x[0] - y[0])
      .map(([days, amounts]) => ({
        days,
        amounts,
        total: Object.values(amounts).reduce((sum, v) => sum + v, 0),
      }))
    // Daily grain where a day changes a decision, one sum where it does not.
    // Hard-cutting at twelve rows hid the tail entirely on a real pool — the
    // WorkBuddy batches start nineteen days out, so the chart read as "only
    // Trae expires". Aggregating past two weeks keeps every credit on screen.
    const near = buckets.filter((row) => row.days <= 14)
    const far = buckets.filter((row) => row.days > 14)
    const rows = [...near]
    if (far.length) {
      const amounts = {}
      for (const row of far) {
        for (const [src, amount] of Object.entries(row.amounts)) {
          amounts[src] = (amounts[src] || 0) + amount
        }
      }
      rows.push({
        days: 15,
        far: far.length,
        amounts,
        total: Object.values(amounts).reduce((sum, v) => sum + v, 0),
      })
    }
    // The scale is set by the near rows: they are what a decision is made on,
    // and a tail bucket an order of magnitude larger would flatten them to
    // slivers. The aggregate bar clamps at full width and states its number —
    // "at least as big as the biggest nearby batch", not a fake proportion.
    let peak = 0
    for (const row of near) peak = Math.max(peak, row.total)
    if (!far.length) for (const row of rows) peak = Math.max(peak, row.total)
    return { rows, peak, undated, undatedCount }
  })

  // The band's third figure. It is pack-level like the chart above: an
  // account-level earliest date says when the first batch dies, never how much
  // dies with it, so summing that would overstate. Accounts whose packs were
  // not read contribute nothing rather than a guess.
  const creditSoon = $derived(
    creditExpiry.rows
      .filter((row) => row.days >= 0 && row.days <= 7)
      .reduce((sum, row) => sum + row.total, 0),
  )

  // Platform totals for the band, read from the same response as everything
  // else here. A platform with no account at all reads "—".
  const creditsPlatforms = $derived.by(() => {
    const bySrc = new Map()
    for (const row of creditRows) {
      const key = row.source === 'trae' ? 'trae-cn' : row.source
      const item = bySrc.get(key) ?? { source: key, remain: 0, known: false }
      if (!(row.failed || row.local || row.unlimited || row.known === false)) {
        item.remain += row.remain || 0
        item.known = true
      }
      bySrc.set(key, item)
    }
    return ['workbuddy', 'trae-cn'].map(
      (source) => bySrc.get(source) ?? { source, remain: 0, known: false },
    )
  })

  // Distance drives lightness: the nearer a batch lapses, the more ink it gets.
  // Colour stays with the platform, so a row reads as "how much dies on this
  // day, out of which source" without spending a second palette.
  function expiryOpacity(days) {
    if (days < 0 || days <= 3) return 1
    if (days <= 7) return 0.8
    if (days <= 14) return 0.6
    if (days <= 30) return 0.45
    return 0.3
  }

  // Three rows per card, then an aggregate: a card that grows with every extra
  // package name breaks the grid's baseline, and the fourth name onwards is
  // detail nobody scans.
  const PACK_SOURCE_ROWS = 3

  // Sources are the package names the upstream grants under: 「国内运营裂变包」
  // ×2 says the reward fired twice, and that is the number that explains how two
  // accounts with identical usage differ by a thousand credits.
  function packSources(row) {
    const byName = new Map()
    for (const pack of row.packs ?? []) {
      const name = pack.name || t.credits.unnamedPack
      const item = byName.get(name) ?? { name, n: 0, size: 0, remain: 0, created: 0 }
      item.n += 1
      item.size += pack.size || 0
      item.remain += pack.remain || 0
      if (pack.created_at && (!item.created || pack.created_at < item.created)) {
        item.created = pack.created_at
      }
      byName.set(name, item)
    }
    return [...byName.values()].sort((a, b) => b.size - a.size)
  }

  // Richest first: the question this view answers is "who is about to run out".
  const creditCards = $derived.by(() => {
    const rows = [...creditRows].sort((a, b) => (b.remain || 0) - (a.remain || 0))
    return rows.map((row) => {
      const sources = packSources(row)
      const shown = sources.slice(0, PACK_SOURCE_ROWS)
      const more = sources.slice(PACK_SOURCE_ROWS)
      return {
        ...row,
        sources: shown,
        moreCount: more.length,
        moreSize: more.reduce((sum, item) => sum + item.size, 0),
        packCount: (row.packs ?? []).length,
        nextExpiry: earliestPackExpiry(row),
      }
    })
  })

  // The batch table's sort and its two folds. Sorting by lapse answers "what do
  // I spend first"; sorting by size answers "where did the big grant go".
  let creditsSort = $state('end')
  let packFold = $state({})

  function togglePackFold(key) {
    packFold = { ...packFold, [key]: !packFold[key] }
  }

  // Five rows, then two folds. A batch table that lists twenty rows buries the
  // five that matter; spent packs stay reachable but stop competing for space.
  const PACK_ROWS = 5

  function packDetail(row) {
    const packs = row.packs ?? []
    const live = packs.filter((pack) => (pack.remain || 0) > 0)
    const used = packs.filter((pack) => (pack.remain || 0) <= 0)
    const bySize = (a, b) => (b.size || 0) - (a.size || 0)
    const byEnd = (a, b) => (a.expires_at || 0) - (b.expires_at || 0)
    const cmp =
      creditsSort === 'size'
        ? (a, b) => bySize(a, b) || byEnd(a, b)
        : (a, b) => byEnd(a, b) || bySize(a, b)
    live.sort(cmp)
    used.sort(cmp)
    const visible = live.slice(0, PACK_ROWS)
    const rest = live.slice(PACK_ROWS)
    const openRest = !!packFold[`${row.key}:rest`]
    const openUsed = !!packFold[`${row.key}:used`]
    const out = visible.map((pack) => ({ kind: 'pack', pack }))
    if (rest.length) {
      out.push({
        kind: 'fold',
        fold: 'rest',
        open: openRest,
        n: rest.length,
        size: rest.reduce((sum, pack) => sum + (pack.size || 0), 0),
        remain: rest.reduce((sum, pack) => sum + (pack.remain || 0), 0),
      })
      if (openRest) for (const pack of rest) out.push({ kind: 'pack', pack })
    }
    if (used.length) {
      out.push({
        kind: 'fold',
        fold: 'used',
        open: openUsed,
        n: used.length,
        size: used.reduce((sum, pack) => sum + (pack.size || 0), 0),
        remain: 0,
      })
      if (openUsed) for (const pack of used) out.push({ kind: 'pack', pack })
    }
    return out
  }

  // The card footers carry the nearest lapse date the pack list states, so
  // "which pack runs out first" is visible without opening the batch table.
  function earliestPackExpiry(row) {
    let best = 0
    for (const pack of row.packs ?? []) {
      if (pack.expires_at && (!best || pack.expires_at < best)) best = pack.expires_at
    }
    return best
  }

  function fmtExpiryDay(sec) {
    if (!sec) return ''
    const d = new Date(sec * 1000)
    return `${d.getMonth() + 1}-${String(d.getDate()).padStart(2, '0')}`
  }

  // Under three days out, the date earns its amber: it is the only deadline
  // on the card and silence would waste it. Spec A.2 makes that same window
  // speak in relative time — "18 小时后" lands harder than "10-21" — and the
  // absolute date stays reachable in the title and the waterfall.
  function isExpirySoon(sec) {
    return sec > 0 && sec * 1000 - Date.now() < 72 * 3600 * 1000
  }

  function fmtExpiry(sec) {
    if (!sec) return ''
    if (!isExpirySoon(sec)) return fmtExpiryDay(sec)
    const left = sec - Math.floor(Date.now() / 1000)
    if (left <= 0) return fmtExpiryDay(sec)
    const hr = Math.floor(left / 3600)
    if (hr >= 24) return t.time.inDays(Math.floor(hr / 24))
    if (hr >= 1) return t.time.inHours(hr)
    return t.time.inMinutes(Math.max(1, Math.floor(left / 60)))
  }

  // Sources in the rail's fixed order, so the same two platforms read the same
  // way here as in the overview ledger instead of following response order.
  const creditGroups = $derived.by(() => {
    const map = new Map()
    for (const row of creditRows) {
      const src = row.source === 'trae' ? 'trae-cn' : row.source || 'unknown'
      if (!map.has(src)) map.set(src, [])
      map.get(src).push(row)
    }
    const order = ['workbuddy', 'trae-cn']
    return [...map.entries()]
      .sort((x, y) => {
        const ix = order.indexOf(x[0])
        const iy = order.indexOf(y[0])
        return (ix < 0 ? 99 : ix) - (iy < 0 ? 99 : iy)
      })
      .map(([source, items]) => ({
        source,
        items: [...items].sort((a, b) => (b.remain || 0) - (a.remain || 0)),
      }))
  })

  // Pack rows stay folded: an account's totals answer the question at a glance,
  // and the per-batch split is only interesting once you ask for it.
  let openCredits = $state({})

  function toggleCredits(key) {
    openCredits = { ...openCredits, [key]: !openCredits[key] }
  }

  // ── task center ──────────────────────────────────────────────────────────
  function setAccount(uid, patch) {
    accountTasks = { ...accountTasks, [uid]: { ...(accountTasks[uid] || {}), ...patch } }
  }

  // Reading tasks is a read: it never queues behind a write and never goes
  // stale silently. The channel is per account so two expanded rows cannot
  // cancel each other.
  async function loadAccountTasks(uid) {
    setAccount(uid, { loading: true, error: '' })
    try {
      const body = await api(`/accounts/${encodeURIComponent(uid)}/tasks`, {
        channel: `tasks:${uid}`,
      })
      setAccount(uid, { loading: false, tasks: body.tasks ?? [], error: '' })
    } catch (err) {
      if (isStale(err)) return
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

  async function acceptTask(acc, code) {
    await guard(acc.uid, code || t.tasks.accountOps.acceptAll, async () => {
      await api(`/accounts/${encodeURIComponent(acc.uid)}/tasks/accept`, {
        method: 'POST',
        body: JSON.stringify(code ? { task_code: code } : { all: true }),
      })
      toastOk(code ? t.tasks.msg.accepted(code) : t.tasks.msg.acceptedAll)
      await loadAccountTasks(acc.uid)
    })
  }

  async function claimTask(acc, code) {
    await guard(acc.uid, `${t.tasks.actions.claim} ${code}`, async () => {
      const body = await api(`/accounts/${encodeURIComponent(acc.uid)}/tasks/claim`, {
        method: 'POST',
        body: JSON.stringify({ task_code: code }),
      })
      toastOk(
        t.tasks.msg.claimed(code, body.credit) +
          (body.energy ? t.tasks.rewardEnergyDelta(body.energy) : ''),
      )
      await loadAccountTasks(acc.uid)
      await loadAccounts()
    })
  }

  async function runTask(acc, code) {
    await guard(acc.uid, `${t.tasks.actions.run} ${code}`, async () => {
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
    await guard(acc.uid, t.tasks.accountOps.runAll, async () => {
      const body = await api(`/accounts/${encodeURIComponent(acc.uid)}/tasks/auto_all`, { method: 'POST' })
      results = { ...results, [acc.uid]: body.results ?? [] }
      toastOk(summarize(body.results ?? []))
      await loadAccountTasks(acc.uid)
      await loadAccounts()
    })
  }

  async function accountOp(acc, op, label) {
    if (op === 'acceptAll') {
      await acceptTask(acc, '')
      return
    }
    await guard(acc.uid, label, async () => {
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

  // A batch sweep touches every account, so it waits for all single-account
  // writes to finish and blocks new ones until it is done.
  async function batch(op, label) {
    if (busyBatch || anyWrite) return
    batchLabel = label
    busyBatch = op
    try {
      await api(`/batch/${op}`, { method: 'POST' })
      // A batch sweep reports nothing per account; the honest summary names the
      // automatic retry instead of claiming every account succeeded.
      if (op === 'checkin_all') pushToast(t.tasks.msg.batchCheckin, 'warn')
      else toastOk(t.tasks.msg.batchDone(label))
      await loadAccounts()
      await reloadExpanded()
      await loadQueue()
    } catch (err) {
      toastErr(err.message)
    } finally {
      busyBatch = ''
      batchLabel = ''
    }
  }

  async function loadQueue() {
    queueBusy = true
    try {
      const body = await api('/tasks/queue')
      queue = body.queue ?? []
      clearSection('tasks')
      loadedAt = { ...loadedAt, tasks: Date.now() }
    } catch (err) {
      if (!isStale(err)) setSection('tasks', err.message)
    } finally {
      queueBusy = false
    }
  }

  // The queue is keyed by uid and only lists accounts that have work. Joining
  // it with the pool is what lets a row say "待办 3" without a second list.
  const queueByUid = $derived.by(() => {
    const map = {}
    for (const row of queue) map[row.uid] = row
    return map
  })

  // The queue's totals, stated on the band rather than in a sentence: how much
  // work is waiting across the pool, and how much of it is already collectable.
  // Derived from this page's own queue, not the overview's snapshot of the same
  // endpoint — the two are polled on different clocks.
  const taskPending = $derived(queue.reduce((n, row) => n + (row.pending || 0), 0))
  const taskClaimable = $derived(queue.reduce((n, row) => n + (row.claimable || 0), 0))

  // The task centre is a WorkBuddy surface: growth tasks exist only there, and
  // a Trae row with nothing to expand read as "broken", not as "nothing to
  // do". Trae's check-in keeps running through the maintenance surface — it
  // just has no task card. (The queue endpoint never listed Trae either.)
  const taskAccounts = $derived(accounts.filter((a) => a.source === 'workbuddy'))

  // The account menu on this page is maintenance, not removal: dropping an
  // account belongs to the pool ledger, where the row is the subject.
  // Claim-all is the third write on an account and the least urgent of them, so
  // it lives here instead of taking a third link on a 36px row.
  function taskMenu(acc) {
    const items = [{ id: 'acceptAll', label: t.tasks.accountOps.acceptAll, icon: 'check' }]
    for (const act of acc.actions ?? []) {
      if (act === 'tasks' || act === 'remove') continue
      items.push({
        id: act,
        label: t.accounts.action[act] ?? act,
        icon: act === 'pause' ? 'pause' : act === 'resume' ? 'play' : undefined,
      })
    }
    return items
  }

  // A sweep can report dozens of results. Five is what fits without pushing the
  // next account off screen; the rest are one click away.
  const RESULT_ROWS = 5
  let resultsOpen = $state({})

  function toggleResults(uid) {
    resultsOpen = { ...resultsOpen, [uid]: !resultsOpen[uid] }
  }

  function resultRows(uid) {
    const list = results[uid] ?? []
    return resultsOpen[uid] ? list : list.slice(0, RESULT_ROWS)
  }

  // Running the queue is a sweep over every account, so it takes the same
  // exclusive slot as a batch maintenance run.
  async function runQueue() {
    if (busyBatch || anyWrite) return
    queueBusy = true
    busyBatch = 'run_queue'
    batchLabel = t.tasks.queue.run
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
      busyBatch = ''
      batchLabel = ''
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
    if (task.claimed) return { tone: 'off', text: st.claimed }
    if (task.locked) return { tone: 'idle', text: st.locked }
    const cur = Number(task.current || 0)
    const target = Number(task.target || 0)
    if (task.claimable || (target > 0 && cur >= target)) return { tone: 'ok', text: st.claimable }
    if (task.accept_status === 'completed') return { tone: 'ok', text: st.completed }
    if (cur > 0) return { tone: 'warn', text: st.inProgress }
    return { tone: 'idle', text: st.pending }
  }

  // A row's identity depends on which dimension is showing: an account row is
  // keyed by account plus platform (they are distinct credentials), a model by
  // its id, a wire by its name.
  function rowKey(r) {
    if (usageDim === 'account') return `${r.source || ''}:${r.account || ''}`
    if (usageDim === 'wire') return r.wire || ''
    return r.model || ''
  }

  // The chart's x-axis labels are dates; on a year-long window the month-day
  // is what distinguishes them, and the year is already in the range control.
  function shortDate(date) {
    if (!date) return ''
    return date.length >= 10 ? date.slice(5) : date
  }

  // The usage window ────────────────────────────────────────────────────
  // One query string, shared by every endpoint the page calls, so the counters,
  // the chart and the detail table can never disagree about which span — or
  // which upstream — they describe.
  function usageQuery() {
    const p = new URLSearchParams()
    if (usageSource) p.set('source', usageSource)
    if (usageRange === 'all') {
      p.set('all', '1')
    } else if (usageRange === 'custom') {
      const from = toUnix(usageFrom)
      const to = toUnix(usageTo)
      if (from) p.set('from', from)
      if (to) p.set('to', to)
    } else {
      p.set('days', usageRange)
    }
    return p.toString()
  }

  function toUnix(local) {
    if (!local) return ''
    const ms = new Date(local).getTime()
    return Number.isFinite(ms) ? String(Math.floor(ms / 1000)) : ''
  }

  // The label states the span, not the button that was pressed: after a custom
  // range the old "近 7 天" would be a lie the bar keeps repeating.
  function usageWindowLabel() {
    if (usageRange === 'all') return t.usage.windowAll
    if (usageRange === 'custom') {
      if (!usageFrom && !usageTo) return t.usage.windowCustomEmpty
      const from = usageFrom ? usageFrom.slice(0, 10) : '…'
      const to = usageTo ? usageTo.slice(0, 10) : t.usage.windowNow
      return `${from} → ${to}`
    }
    return t.usage.windowMeta(Number(usageRange))
  }

  // Switching the window quickly used to let a slow earlier response overwrite
  // a newer one. The generation counter is the page-level guarantee; the request
  // channels are the network-level one, so the old window is actually cancelled.
  let usageWindowSeq = 0

  async function loadUsage() {
    const seq = ++usageWindowSeq
    usageLoading = true
    try {
      const q = usageQuery()
      const [sum, daily] = await Promise.all([
        api(`/usage/summary?${q}`, { channel: 'usage.summary' }),
        api(`/usage/daily?${q}`, { channel: 'usage.daily' }),
      ])
      if (seq !== usageWindowSeq) return
      usageAvailable = sum.available !== false
      usageSummary = sum.summary ?? null
      // The baseline travels with the summary so the headline can state its
      // comparison without a second round trip. "All time" has no predecessor
      // and the field is simply absent there.
      usagePrev = sum.prev ?? null
      usageModels = sum.models ?? []
      usageAccounts = sum.accounts ?? []
      usageWires = sum.wires ?? []
      usageDaily = daily.daily ?? []
      await loadUsageRecords(true)
      if (seq !== usageWindowSeq) return
      clearSection('usage')
      loadedAt = { ...loadedAt, usage: Date.now() }
    } catch (err) {
      if (isStale(err) || seq !== usageWindowSeq) return
      setSection('usage', err.message)
    } finally {
      if (seq === usageWindowSeq) usageLoading = false
    }
  }

  // loadUsageRecords fetches one batch. reset=true starts from the newest row;
  // otherwise it asks for everything older than the last row already shown, so
  // "load more" appends instead of replacing. The detail table is a timeline,
  // and page numbers would be a fiction over data that grows at the top.
  let recordsSeq = 0

  async function loadUsageRecords(reset = false) {
    const seq = ++recordsSeq
    const params = new URLSearchParams({ limit: '200' })
    for (const [k, v] of Object.entries(usageFilter)) {
      if (v) params.set(k, v)
    }
    // The page-level source filter covers the detail table too — the rows the
    // headline counts must be the rows the table lists.
    if (usageSource) params.set('source', usageSource)
    if (!reset && usageRecords.length) {
      const last = usageRecords[usageRecords.length - 1]
      const ts = Math.floor(new Date(last.time).getTime() / 1000)
      if (ts > 0) params.set('before', String(ts))
    }
    recordsLoading = true
    try {
      const rec = await api(`/usage/records?${params}`, { channel: 'usage.records' })
      if (seq !== recordsSeq) return
      usageAvailable = rec.available !== false
      const batch = rec.records ?? []
      const next = reset ? batch : [...usageRecords, ...batch]
      if (next.length >= RECORDS_CAP) {
        usageRecords = next.slice(0, RECORDS_CAP)
        recordsCapped = true
        recordsExhausted = true
      } else {
        usageRecords = next
        recordsCapped = false
        recordsExhausted = batch.length < 200
      }
      clearSection('usageRecords')
    } catch (err) {
      if (isStale(err) || seq !== recordsSeq) return
      setSection('usageRecords', err.message)
    } finally {
      if (seq === recordsSeq) recordsLoading = false
    }
  }

  // A.4's right-edge veil must only appear when the table really runs past its
  // box — a constant fade over content that already fits just hides the last
  // column. The action keeps `.fade-right` honest across resizes and refetches.
  function scrollFade(node) {
    const sync = () => {
      const more = node.scrollWidth - node.clientWidth - node.scrollLeft > 2
      node.classList.toggle('fade-right', more)
    }
    sync()
    node.addEventListener('scroll', sync, { passive: true })
    const ro = new ResizeObserver(sync)
    ro.observe(node)
    for (const child of node.children) ro.observe(child)
    return {
      destroy() {
        node.removeEventListener('scroll', sync)
        ro.disconnect()
      },
    }
  }

  function onRecordsScroll(e) {
    const el = e.currentTarget
    if (!el || recordsLoading || recordsExhausted) return
    // A quarter-screen of runway is enough to fetch before the user reaches
    // the bottom, so the list never visibly stalls.
    if (el.scrollTop + el.clientHeight >= el.scrollHeight - 240) loadUsageRecords(false)
  }

  function changeUsageRange(next) {
    if (next === 'custom' && !usageFrom && !usageTo) {
      // Seed the pickers with the preset the user was just looking at, so
      // switching to custom is a refinement rather than a blank form.
      const now = new Date()
      const weekAgo = new Date(now.getTime() - 6 * 24 * 3600 * 1000)
      usageTo = localInput(now)
      usageFrom = localInput(weekAgo)
    }
    usageRange = next
    loadUsage()
  }

  // datetime-local wants "YYYY-MM-DDTHH:MM" in local time, and toISOString
  // would hand back UTC — which shifts the range by the offset.
  function localInput(date) {
    const pad = (n) => String(n).padStart(2, '0')
    return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
  }

  function applyCustomRange() {
    if (usageRange !== 'custom') return
    loadUsage()
  }

  // The three breakdowns are already in memory, so switching a dimension is a
  // sort, not a request.
  function setUsageDim(dim) {
    usageDim = dim
  }

  const usageRows = $derived.by(() => {
    if (usageDim === 'account') return usageAccounts
    if (usageDim === 'wire') return usageWires
    return usageModels
  })

  // The daily endpoint returns a row for every date in the window, empty or
  // not — so row count can never say "no data". The window is empty only when
  // nothing in it made a request, and that is what earns the ghost bars.
  const usageHasData = $derived(usageDaily.some((r) => (r.requests || 0) > 0))

  // The breakdowns arrive already narrowed by the page-level source filter, so
  // there is nothing left to filter client-side. The name stays because the
  // markup reads "the rows the table shows".
  const usageFilteredRows = $derived(usageRows)

  // The stacked chart's three bands. "input minus cache" is clamped at zero
  // because upstreams occasionally report a cache reading larger than the
  // prompt they measured it against, and a negative band would draw backwards.
  function tokenBands(row) {
    const cached = Math.max(0, row.cached_tokens || 0)
    const input = Math.max(0, row.input_tokens || 0)
    const fresh = Math.max(0, input - cached)
    const output = Math.max(0, row.output_tokens || 0)
    return { output, fresh, cached, total: Math.max(output + fresh + cached, 1) }
  }

  // Cache hit rate is derived from the same two numbers the chart draws, so
  // the headline percentage and the bands can never disagree.
  const cacheRate = $derived.by(() => {
    const cached = usageSummary?.cached_tokens || 0
    const input = usageSummary?.input_tokens || 0
    if (!input) return 0
    return Math.min(100, (cached / input) * 100)
  })

  // The headline number and its baseline, in the unit the metric switch names.
  // The comparison is the equally long window before this one; without a
  // baseline there is no delta rather than a fake ±0%.
  const usageHeadline = $derived.by(() => {
    const s = usageSummary
    if (!s) return { text: t.common.dash, label: '', delta: null }
    const credits = usageMetric === 'credits'
    const text = credits ? fmtCredits(s.credits) : fmtTokens(s.total_tokens)
    const label = credits ? t.usage.stat.credits : t.usage.stat.total
    const cur = credits ? s.credits || 0 : s.total_tokens || 0
    const base = credits ? usagePrev?.credits || 0 : usagePrev?.total_tokens || 0
    return { text, label, delta: base > 0 ? (cur - base) / base : null }
  })

  // The chart's peak follows the metric too: a credits axis scaled by token
  // volume would draw flat bars for an expensive low-token day.
  const chartPeak = $derived.by(() => {
    if (!usageDaily.length) return 1
    const vals = usageDaily.map((r) => (usageMetric === 'credits' ? r.credits || 0 : tokenBands(r).total))
    return Math.max(...vals, 1)
  })

  // The right-hand panel answers "where did the window's credits go". Bars
  // are credits only — tokens and credits never share one axis.
  const spendRows = $derived.by(() => {
    const map = new Map()
    if (spendDim === 'source') {
      for (const r of usageAccounts) {
        const key = r.source || 'unknown'
        map.set(key, (map.get(key) || 0) + (r.credits || 0))
      }
      return [...map.entries()]
        .map(([key, credits]) => ({ key, label: key, source: key, credits }))
        .sort((a, b) => b.credits - a.credits)
    }
    for (const r of usageAccounts) {
      const key = `${r.source || ''}:${r.account || ''}`
      map.set(key, { key, label: r.account || t.common.dash, source: r.source, credits: r.credits || 0 })
    }
    const rows = [...map.values()].sort((a, b) => b.credits - a.credits)
    if (rows.length <= 8) return rows
    const head = rows.slice(0, 8)
    const rest = rows.slice(8)
    head.push({
      key: '__other',
      label: t.usage.breakdown.spend.other,
      source: '',
      credits: rest.reduce((s, r) => s + r.credits, 0),
    })
    return head
  })

  const spendPeak = $derived.by(() => {
    if (!spendRows.length) return 1
    return Math.max(...spendRows.map((r) => r.credits), 1)
  })

  // Top 10 by default: the question is "who is spending", and a wall of 200
  // rows answers it no better than the first ten. The tail stays one click away.
  const breakdownRows = $derived(
    breakdownAll ? usageFilteredRows : usageFilteredRows.slice(0, 10),
  )

  // Numbers are read, not admired: thousands separators, at most two decimals,
  // no trailing zeros. `540.00` looks like a measured quantity when it is just
  // a count.
  function fmtCredits(n) {
    const v = Number(n || 0)
    if (!Number.isFinite(v)) return t.common.dash
    return v.toLocaleString('en-US', { minimumFractionDigits: 0, maximumFractionDigits: 2 })
  }

  // Percentages carry one decimal (A.1) so a rate of 99.06% and one of 99.9%
  // do not both print as 99%. An exact whole still drops the ".0".
  function fmtPct(v) {
    const n = Number(v || 0)
    if (!Number.isFinite(n)) return t.common.dash
    const s = n.toFixed(1)
    return `${s.endsWith('.0') ? s.slice(0, -2) : s}%`
  }

  // Tokens keep their digits below ten thousand and abbreviate above it, so a
  // column of figures stays comparable without becoming a wall of zeros.
  function fmtTokens(n) {
    const v = Number(n || 0)
    if (!Number.isFinite(v)) return t.common.dash
    const abs = Math.abs(v)
    if (abs >= 1e6) return `${trimZero((v / 1e6).toFixed(1))}M`
    if (abs >= 1e4) return `${trimZero((v / 1e3).toFixed(1))}K`
    return v.toLocaleString('en-US')
  }

  function trimZero(s) {
    return s.endsWith('.0') ? s.slice(0, -2) : s
  }

  function fmtDateTime(value) {
    return fmtStamp(value) || t.common.dash
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

<svelte:window
  onscroll={() => (scrolled = window.scrollY > 4)}
  onkeydown={(e) => {
    // Escape belongs to the topmost surface only. While a confirmation is up it
    // owns the key, so the page behind it must not close as well.
    if (e.key !== 'Escape' || confirmState.open) return
    if (showAdd) closeAdd()
    else if (showLogs) closeLogs()
    else if (probeDrawer) probeDrawer = false
    else if (probePickerOpen) closeProbePicker()
    else if (probeTimeoutMenu) closeProbeTimeoutMenu()
  }}
/>

<div class="shell">
  <!-- Keyboard users should not have to tab through six destinations to reach
       the table they came for. A button, not an anchor: the console routes on
       the hash, so href="#main" would be parsed as a route and rewritten. -->
  <button class="skip-link" onclick={focusMain}>{t.common.skipToContent}</button>
  <aside class="nav" aria-label={t.nav.aria}>
    <!-- Two lines, not one: the name alone left 60px of dead space and the
         rail read as if it had lost its masthead. The tagline is set in caps
         at 7.5px, so it sits under the name as a footnote rather than as a
         second headline competing with the destinations below. -->
    <div class="brand">
      <!-- The real artwork, not initials: the 256px PNG ships inside the Go
           binary and is served at /panel/logo.png, so the mark is the same
           one the tray and the window use. Empty alt — the brand name next to
           it already says what this is. -->
      <img class="logo" src="/panel/logo.png" alt="" width="26" height="26" />
      <span class="brand-lines">
        <span class="brand-name">{t.brand.name}</span>
        <span class="brand-tagline">{t.brand.tagline}</span>
      </span>
    </div>
    <!-- Arrow keys move between destinations; Tab still visits each one, so the
         list stays an ordinary list of links for assistive technology. The
         handler sits on the links themselves — a listener on the list would be
         attached to a non-interactive element. -->
    <ul>
      {#each navItems as item (item.id)}
        <li class:split={item.id === 'settings'}>
          <a
            href="#/{item.id}"
            class:on={tab === item.id}
            aria-current={tab === item.id ? 'page' : undefined}
            data-tip={item.label}
            data-tip-side="right"
            onkeydown={navKeydown}
            onclick={(e) => {
              e.preventDefault()
              switchTab(item.id)
            }}
          >
            <Icon name={item.icon} size={15} />
            <span class="nav-label">{item.label}</span>
          </a>
        </li>
      {/each}
    </ul>
    <!-- The pulse is the one place the shell states whether the gateway is
         answering. In the icon rail the words are visually hidden but stay in
         the accessibility tree, and the tooltip carries the details. -->
    <div class="rail-status" class:bad={!session} data-tip={statusTip} data-tip-side="up">
      <span class="rail-dot" aria-hidden="true"></span>
      <span class="rail-status-text">{statusText}</span>
      <span class="sr-only">{statusTip}</span>
    </div>
  </aside>

  <div class="main">
    <!-- The reference console carries the current destination in a slim sticky
         bar instead of repeating it as a page heading: the rail already says
         where you are, and the meta slot says what the numbers below cover. -->
    <header class="topbar" class:scrolled>
      <h2 class="view-title">{currentTitle}</h2>
      {#if currentMeta}
        <span class="sep" aria-hidden="true">/</span>
        <span class="meta">{currentMeta}</span>
      {/if}
      <span class="grow"></span>
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
      <button
        class="btn ghost icon"
        onclick={openLogs}
        aria-label={t.nav.logsAction}
        title={t.nav.logsAction}
      >
        <Icon name="logs" size={16} />
      </button>
      <button class="btn primary" onclick={openAdd}>
        <Icon name="add" size={15} />
        <span>{t.nav.addAction}</span>
      </button>
    </header>

    <main class="page" class:overview={tab === 'overview'} tabindex="-1" bind:this={mainEl}>
    {#if fatal}
      <div class="note error banner" role="alert">
        <Icon name="alert" size={16} />
        <span class="spacer">{fatal}</span>
        <button class="btn ghost sm" onclick={refresh}>{t.common.retry}</button>
        <button
          class="btn icon"
          onclick={() => {
            fatal = ''
            fatalFrom = ''
          }}
          aria-label={t.common.close}
        >
          <Icon name="close" size={15} />
        </button>
      </div>
    {/if}

    {#if tab === 'overview'}
      <!-- The landing page starts with the answer, not a title: one 88px band
           in two rows. The first says which build is answering and where to
           point a client; the second says how many accounts can serve a
           request, where the credits sit, and what today has burned. Two rows
           instead of three zones because the address belongs to the whole
           console, not to one column. Nothing here costs an upstream call. -->
      <section class="pool" aria-label={t.pool.title}>
        <!-- Row one is the console's own identity: which build is answering,
             where it listens, which upstreams stand behind it, and the one
             address a client needs. The brand mark stays in the rail — a second
             copy here would only repeat it. -->
        <div class="pool-ident">
          <span class="run-pill" class:bad={!session}>
            <span class="run-dot" aria-hidden="true"></span>
            {session ? t.rail.running : t.rail.offline}
          </span>
          {#if listenAddr}
            <span class="dot-sep" aria-hidden="true">·</span>
            <span class="ident-pair">
              <span class="ident-lbl">{t.overview.listenLabel}</span>
              <span class="ident-val mono">{listenAddr}</span>
            </span>
          {/if}
          <span class="dot-sep" aria-hidden="true">·</span>
          <span class="ident-pair">
            <span class="ident-lbl">{t.overview.upstreamLabel}</span>
            <span class="ident-val">{t.brand.upstreamValue}</span>
          </span>
          <span class="grow"></span>
          {#if endpointUrl}
            <span class="api-chip" title={endpointUrl}>{endpointUrl}</span>
            <button
              class="copy-btn"
              class:copied={endpointCopied}
              title={endpointCopied ? t.overview.copied : t.overview.copyEndpoint}
              aria-label={endpointCopied ? t.overview.copied : t.overview.copyEndpoint}
              onclick={copyEndpoint}
            >
              {t.overview.copyShort}
            </button>
          {/if}
        </div>

        <span class="pool-rule" aria-hidden="true"></span>

        <!-- Row two answers "can it serve, where do the credits sit, what did
             today burn": four health counts, the two balances, the day's
             tokens. The counts are filters — the same shortcut the filter chip
             below states in words. -->
        <div class="pool-metrics">
          {#each poolSegments as seg (seg.id)}
            {@const clickable = seg.n > 0 && seg.n < accounts.length}
            {#if clickable}
              <button
                class="seg-metric"
                class:on={healthFilter === seg.id}
                title={t.pool.segmentHint(seg.label, seg.n)}
                aria-pressed={healthFilter === seg.id}
                onclick={() => toggleHealthFilter(seg.id)}
              >
                <span class="v">{seg.n}</span>
                <span class="k">{seg.label}</span>
              </button>
            {:else}
              <span class="seg-metric" title="{seg.label} {seg.n}">
                <span class="v">{seg.n}</span>
                <span class="k">{seg.label}</span>
              </span>
            {/if}
          {/each}

          <span class="pool-vrule" aria-hidden="true"></span>

          <div class="pool-credits">
            {#each creditsBySource as c (c.source)}
              {@const shown = c.known ? (c.unlimited ? '∞' : fmtCredits(c.total)) : t.common.dash}
              <button class="src-balance" title={`${sourceName(c.source)} · ${shown}`} onclick={() => switchTab('credits')}>
                <span class="src-dot" data-source={c.source}></span>
                <span class="src-name">{sourceName(c.source)}</span>
                <span class="v mono">{shown}</span>
              </button>
            {/each}
          </div>

          <span class="grow"></span>

          <span class="pool-vrule" aria-hidden="true"></span>

          <span
            class="seg-metric today"
            title={todayDelta !== null
              ? t.pool.vsPrev(fmtDelta(todayDelta))
              : overviewUsage
                ? t.pool.noBaseline
                : ''}
          >
            <span class="v">{overviewUsage ? fmtTokens(overviewUsage.total_tokens) : t.common.dash}</span>
            <span class="k">{t.pool.today}</span>
          </span>
        </div>
      </section>

      {#if healthFilter}
        <div class="filter-row">
          <span class="filter-chip">
            {t.pool.only(filterLabel)}
            <button class="chip-x" aria-label={t.pool.clearFilter} onclick={() => (healthFilter = '')}>
              <Icon name="close" size={13} />
            </button>
          </span>
          <span class="muted small">{t.pool.total(groupsView.reduce((n, g) => n + g.items.length, 0))}</span>
        </div>
      {/if}

      <!-- The ledger's own header: a title and the five sweeps that act on
           exactly the table below. The buttons stay visible with an empty pool
           and disable themselves — an affordance you can see and cannot use
           reads as "add an account first", while a row that disappears reads as
           a bug. It also gives the table the one label the page was missing. -->
      <div class="ledger-head">
        <h2 class="ledger-title">{t.overview.ledger}</h2>
        <span class="grow"></span>
        <button class="btn ghost sm" onclick={() => batch('checkin_all', t.tasks.batch.checkinAll)} disabled={!accounts.length || !!busyBatch || anyWrite}>
          {t.accounts.batch.checkin}
        </button>
        <button class="btn ghost sm" onclick={() => batch('activity_all', t.tasks.batch.activityAll)} disabled={!accounts.length || !!busyBatch || anyWrite}>
          {t.accounts.batch.activity}
        </button>
        <button class="btn ghost sm" onclick={() => batch('balance_all', t.tasks.batch.balanceAll)} disabled={!accounts.length || !!busyBatch || anyWrite}>
          {t.accounts.batch.balance}
        </button>
        <button class="btn ghost sm" onclick={() => batch('travel_all', t.tasks.batch.travelAll)} disabled={!accounts.length || !!busyBatch || anyWrite}>
          {t.accounts.batch.travel}
        </button>
        <button class="btn ghost sm" onclick={() => batch('keepalive_all', t.tasks.batch.keepaliveAll)} disabled={!accounts.length || !!busyBatch || anyWrite}>
          {t.accounts.batch.keepalive}
        </button>
        {#if busyBatch}
          <span class="msg inline"><span class="spinner inline"></span> {t.tasks.msg.running(batchLabel)}</span>
        {/if}
      </div>

      <div class="overview-body">
      {#if loading}
        <div class="table">
          <!-- Four rows ≈ a head plus three accounts: for the common five-seat
               pool the swap to real content moves the page by a couple of
               pixels instead of a third of the ledger. -->
          {#each [{ w: '40%' }, { w: '100%' }, { w: '70%' }, { w: '55%' }] as row (row.w)}
            <div class="row skeleton-row" style="--cols: {row.w}"><span class="skeleton"></span></div>
          {/each}
        </div>
      {:else if accounts.length === 0}
        <div class="empty">
          <span class="empty-mark"><Icon name="user" size={18} /></span>
          <p class="empty-title">{t.accounts.empty.title}</p>
          <p class="muted">{t.accounts.empty.body}</p>
          <button class="btn primary" onclick={openAdd}>
            <Icon name="add" size={15} />
            {t.nav.addAction}
          </button>
        </div>
      {:else if groupsView.length === 0}
        <div class="empty slim">
          <p>{t.pool.only(filterLabel)}</p>
          <button class="btn ghost sm" onclick={() => (healthFilter = '')}>{t.pool.clearFilter}</button>
        </div>
      {:else}
        {#each groupsView as group (group.source)}
          {@const collapsed = !!collapsedGroups[group.source]}
          <section class="group">
            <!-- The head is the group's control, not a label: collapsing a
                 source is how you get the other one back on screen. -->
            <button
              class="group-head"
              aria-expanded={!collapsed}
              title={collapsed ? t.accounts.group.expand(sourceLabel(group.source)) : t.accounts.group.collapse(sourceLabel(group.source))}
              onclick={() => toggleGroup(group.source)}
            >
              <Icon name="chevronRight" size={13} class={collapsed ? 'caret' : 'caret open'} />
              <span class="src-dot" data-source={group.source}></span>
              <span class="group-name">{sourceLabel(group.source)}</span>
              <span class="group-count">{healthFilter ? `${group.items.length}/${group.total}` : group.total}</span>
              <span class="spacer"></span>
              <span class="group-credit mono">
                {group.known ? (group.unlimited ? '∞' : t.accounts.group.credit(fmtCredits(group.credit))) : t.common.dash}
              </span>
            </button>

            {#if !collapsed}
              <div class="table scroll accounts-table" use:scrollFade role="table" aria-label={sourceLabel(group.source)}>
                <div class="row head" role="row" style="--cols: var(--cols-accounts)">
                  <span class="col-mark" role="columnheader" aria-label={t.accounts.col.health}></span>
                  <span role="columnheader">{t.accounts.col.account}</span>
                  <span role="columnheader">{t.accounts.col.status}</span>
                  <span class="num" role="columnheader" aria-sort={sortState('credits')}>
                    <button class="th-btn" onclick={() => cycleSort('credits')}>
                      {t.accounts.col.credits}
                      <Icon name={sortState('credits') === 'ascending' ? 'arrowUp' : 'arrowDown'} size={13} class={sortState('credits') === 'none' ? 'sort-arrow dim' : 'sort-arrow'} />
                    </button>
                  </span>
                  <span class="num" role="columnheader" aria-sort={sortState('calls')}>
                    <button class="th-btn" onclick={() => cycleSort('calls')}>
                      {t.accounts.col.calls}
                      <Icon name={sortState('calls') === 'ascending' ? 'arrowUp' : 'arrowDown'} size={13} class={sortState('calls') === 'none' ? 'sort-arrow dim' : 'sort-arrow'} />
                    </button>
                  </span>
                  <span class="col-inflight num" role="columnheader">{t.accounts.col.inFlight}</span>
                  <span class="num" role="columnheader" aria-sort={sortState('usage')}>
                    <button class="th-btn" onclick={() => cycleSort('usage')}>
                      {t.accounts.col.usage}
                      <Icon name={sortState('usage') === 'ascending' ? 'arrowUp' : 'arrowDown'} size={13} class={sortState('usage') === 'none' ? 'sort-arrow dim' : 'sort-arrow'} />
                    </button>
                  </span>
                  <span class="num" role="columnheader" aria-sort={sortState('last')}>
                    <button class="th-btn" onclick={() => cycleSort('last')}>
                      {t.accounts.col.lastSuccess}
                      <Icon name={sortState('last') === 'ascending' ? 'arrowUp' : 'arrowDown'} size={13} class={sortState('last') === 'none' ? 'sort-arrow dim' : 'sort-arrow'} />
                    </button>
                  </span>
                  <span class="right" role="columnheader">{t.accounts.col.actions}</span>
                </div>
                {#each group.items as acc (acc.key)}
                  {@const h = health(acc)}
                  {@const rest = (acc.actions ?? []).filter((a) => a !== 'tasks' && a !== 'checkin')}
                  <div class="row" role="row" style="--cols: var(--cols-accounts)">
                    <span class="health-mark" data-tone={h.tone} title={h.text} role="cell"></span>
                    <span class="label" title={credentialHint(acc)} role="cell">
                      <span class="label-line">
                        <span class="truncate">{acc.label || acc.uid}</span>
                      </span>
                      <em title={acc.uid}>{midEllipsis(acc.uid)}</em>
                    </span>
                    <span class="cell-status" role="cell">
                      <span class="status {h.tone}">{h.text}</span>
                      {#if acc.checkin_applicable && !acc.checkin_done}
                        <span class="status warn">{t.accounts.checkinPending}</span>
                      {/if}
                    </span>
                    <span class="num mono" role="cell" class:bad-text={!acc.unlimited && acc.credits_known && acc.credits === 0}>
                      {#if acc.unlimited}∞{:else if acc.credits_known}{fmtCredits(acc.credits)}{:else}{t.common.dash}{/if}
                    </span>
                    <span class="num mono" role="cell">
                      {acc.success} / <span class:bad-text={acc.errors > 0}>{acc.errors}</span>
                    </span>
                    <span class="col-inflight num mono" role="cell">{acc.in_flight || 0}</span>
                    <span class="num mono cell-usage" role="cell">
                      {acc.total_tokens ? fmtTokens(acc.total_tokens) : t.common.dash}
                      {#if acc.last_tokens_per_second > 0}
                        <em>{acc.last_tokens_per_second.toFixed(1)} t/s</em>
                      {/if}
                    </span>
                    <span class="num small muted" role="cell" title={acc.last_success_at ? fmtStamp(acc.last_success_at * 1000) : ''}>{acc.last_success_at ? ago(acc.last_success_at) : t.common.dash}</span>
                    <span class="right cell-actions" role="cell">
                      {#if (acc.actions ?? []).includes('tasks')}
                        <button class="btn link" onclick={() => openAccountTasks(acc)}>{t.accounts.action.tasks}</button>
                      {/if}
                      {#if (acc.actions ?? []).includes('checkin')}
                        <button
                          class="btn link"
                          disabled={writeBlocked(acc)}
                          title={acc.checkin_done ? t.tasks.checkinTitle : ''}
                          onclick={() => accountOp(acc, 'checkin', t.accounts.action.checkin)}
                        >{acc.checkin_done ? t.tasks.accountOps.checkinDone : t.accounts.action.checkin}</button>
                      {/if}
                      {#if rest.length}
                        <Menu
                          items={rest.map((act) => ({
                            id: act,
                            label: t.accounts.action[act] ?? act,
                            danger: act === 'remove',
                            divider: act === 'remove',
                            icon: act === 'pause' ? 'pause' : act === 'resume' ? 'play' : undefined,
                          }))}
                          label={t.common.more}
                          onselect={(id) => {
                            if (id === 'remove') removeAccount(acc)
                            else runAccountOp(acc, id)
                          }}
                          disabled={writeBlocked(acc)}
                        />
                      {/if}
                    </span>
                  </div>
                {/each}
              </div>
            {/if}
          </section>
        {/each}
      {/if}

        <!-- Three questions asked right after something went wrong. Folded by
             default: the ledger owns the first screen, and the one-line
             summary still says whether anything needs attention. -->
        <div class="diag-block">
          <button class="fold-row" aria-expanded={diagOpen} onclick={() => (diagOpen = !diagOpen)}>
            <Icon name="chevron" size={13} class={diagOpen ? 'fold-chev open' : 'fold-chev'} />
            <span class="fold-title">{t.overview.diag.title}</span>
            <span class="hint">{t.overview.diag.expiry(expiringSoon.length)}</span>
            <span class="hint">·</span>
            <span class="hint">
              {serviceMissing ? t.overview.diag.serviceMissing(serviceMissing) : t.overview.diag.serviceOk}
            </span>
            <span class="hint">·</span>
            <span class="hint">{t.overview.diag.locks(modelLocks.length)}</span>
            <span class="spacer"></span>
            <span class="fold-action">{diagOpen ? t.usage.chart.collapse : t.usage.chart.expand}</span>
          </button>
          {#if diagOpen}
          <div class="diag">
          <section class="card diag-card">
            <div class="card-head">
              <div class="titles">
                <h2>{t.overview.expiryBox.title}</h2>
                {#if session}<span class="hint">{t.overview.expiryBox.window(session.expiring_window_hours ?? 72)}</span>{/if}
              </div>
              <button class="btn ghost sm" onclick={probeExpiry} disabled={expiryProbing}>
                {#if expiryProbing}<span class="spinner inline"></span>{/if}
                {expiryProbing ? t.overview.expiryBox.checking : t.overview.expiryBox.check}
              </button>
            </div>
            {#if !creditsProbed}
              <p class="muted small">{t.overview.expiryBox.stale}</p>
            {:else if expiringSoon.length === 0}
              <p class="muted small">{t.overview.expiryBox.empty}</p>
            {:else}
              <div class="ov-rows">
                {#each expiringSoon as acc (acc.key)}
                  <div class="ov-row">
                    <span class="truncate">{acc.label || acc.uid}</span>
                    {#if acc.expiring > 0}
                      <span class="status warn">{t.overview.expiryBox.earliest(fmtTime(acc.earliest_expiry), acc.expiring)}</span>
                    {:else}
                      <span class="status ok">{fmtTime(acc.earliest_expiry)}</span>
                    {/if}
                  </div>
                {/each}
              </div>
            {/if}
          </section>

          <section class="card diag-card">
            <div class="card-head">
              <div class="titles"><h2>{t.overview.serviceBox.title}</h2></div>
            </div>
            <div class="kv kv-2">
              {#each serviceRows as row (row.label)}
                <span class="k">{row.label}</span>
                <span class="v" class:missing={row.missing}>{row.missing ? t.common.dash : row.text}</span>
              {/each}
            </div>
          </section>

          <section class="card diag-card">
            <div class="card-head">
              <div class="titles">
                <h2>{t.accounts.locks.title}</h2>
                <span class="hint">{t.accounts.locks.hint(modelLocks.length)}</span>
              </div>
            </div>
            {#if modelLocks.length === 0}
              <p class="muted small">{t.accounts.locks.empty}</p>
            {:else}
              <div class="ov-rows">
                {#each modelLocks as l (l.model + l.account)}
                  <div class="ov-row">
                    <span class="mono small truncate">{l.model}</span>
                    <span class="muted small">{fmtTime(l.until)}</span>
                  </div>
                {/each}
              </div>
              <p class="muted small">{t.accounts.locks.nextFree(fmtTime(nextUnlock))}</p>
            {/if}
          </section>
          </div>
          {/if}
        </div>
      </div>
    {/if}

    {#if tab === 'tasks'}
      <div class="column">
        <!-- The queue states its numbers the way the credits page states a
             balance: a band of 22px figures first, the two actions after.
             "How much work is left" is a number, not a clause in a hint. -->
        <section class="card metric-band queue-band">
          <div class="metric-cell">
            <span class="v mono">{taskPending}</span>
            <span class="k">{t.tasks.queue.pendingLabel}</span>
          </div>
          <span class="metric-vrule" aria-hidden="true"></span>
          <div class="metric-cell">
            <span class="v mono">{taskClaimable}</span>
            <span class="k">{t.tasks.queue.claimLabel}</span>
          </div>
          <span class="metric-vrule" aria-hidden="true"></span>
          <div class="metric-cell">
            <span class="v mono">{queue.length}</span>
            <span class="k">{t.tasks.queue.accountLabel}</span>
          </div>
          <span class="spacer"></span>
          {#if sectionErr.tasks}
            <div class="note error" role="alert">
              <Icon name="alert" size={16} />
              <span>{sectionErr.tasks}</span>
              <button class="btn ghost sm" onclick={loadQueue}>{t.common.retry}</button>
            </div>
          {/if}
          <div class="btn-row">
            <button class="btn ghost" onclick={loadQueue} disabled={queueBusy}>
              <Icon name="refresh" size={15} />
              {t.tasks.queue.scan}
            </button>
            <button class="btn primary" onclick={runQueue} disabled={queueBusy || !!busyBatch || anyWrite}>
              {#if queueBusy}<span class="spinner"></span>{/if}
              {queueBusy ? t.tasks.queue.running : t.tasks.queue.run}
            </button>
          </div>
          <!-- The sweep reports one summary at the end, so the bar is
               indeterminate on purpose: a percentage that cannot be measured
               is worse than an honest "still going". -->
          {#if queueBusy}
            <div class="queue-progress" role="progressbar" aria-label={t.tasks.queue.running}><i></i></div>
          {/if}
        </section>

        <!-- One card for the whole roster: the head names the object and the
             body is either the list or the reason there is no list. A bare
             empty block left the page without a subject. -->
        <section class="card task-card">
          <div class="card-head">
            <div class="titles">
              <h2>{t.tasks.accounts.title}</h2>
              <span class="hint">{t.tasks.accounts.hint}</span>
            </div>
          </div>
        {#if taskAccounts.length === 0}
          <div class="empty inline">
            <span class="empty-mark"><Icon name="tasks" size={18} /></span>
            <p class="empty-title">{t.tasks.accounts.emptyTitle}</p>
            <p class="muted">{t.tasks.accounts.empty}</p>
            <button class="btn primary" onclick={openAdd}>
              <Icon name="add" size={15} />
              {t.nav.addAction}
            </button>
          </div>
        {:else}
          <div class="task-list">
            {#each taskAccounts as acc (acc.key)}
              {@const st = accountTasks[acc.uid] || {}}
              {@const q = queueByUid[acc.uid]}
              {@const h = health(acc)}
              {@const isOpen = !!expanded[acc.uid]}
              <div class="task-acc">
                <div class="task-row">
                  <!-- Name and health are one cluster. A status pinned to the
                       far edge reads as a column of its own and competes with
                       the queue count, which is what the row is actually for. -->
                  <span class="task-ident">
                    {#if acc.local}
                      <span class="task-name">
                        <span class="src-dot" data-source={acc.source}></span>
                        <span class="truncate">{acc.label || acc.uid}</span>
                      </span>
                      <span class="badge">{t.tasks.accounts.local}</span>
                    {:else}
                      <button class="task-name open-btn" aria-expanded={isOpen} onclick={() => toggleTasks(acc)}>
                        <Icon name="chevronRight" size={13} class={isOpen ? 'caret open' : 'caret'} />
                        <span class="src-dot" data-source={acc.source}></span>
                        <span class="truncate">{acc.label || acc.uid}</span>
                      </button>
                    {/if}
                    <span class="status {h.tone}">{h.text}</span>
                  </span>
                  {#if !acc.local}
                    {#if q && (q.pending > 0 || q.claimable > 0)}
                      <span class="q-counts">
                        {#if q.pending > 0}<span class="q-pending">{t.tasks.row.pending(q.pending)}</span>{/if}
                        {#if q.claimable > 0}<span class="q-claim">{t.tasks.row.claimable(q.claimable)}</span>{/if}
                      </span>
                    {:else}
                      <span class="q-counts idle">{t.tasks.row.idle}</span>
                    {/if}
                    <div class="acc-ops">
                      <button class="btn link" onclick={() => runAllTasks(acc)} disabled={writeBlocked(acc)}>{t.tasks.accountOps.runAll}</button>
                      <Menu
                        items={taskMenu(acc)}
                        label={t.tasks.accounts.ops}
                        onselect={(id) => accountOp(acc, id, t.accounts.action[id] ?? id)}
                        disabled={writeBlocked(acc)}
                      />
                    </div>
                  {/if}
                </div>

                {#if busyWrites[acc.uid]}
                  <p class="task-running"><span class="spinner inline"></span> {t.tasks.msg.running(busyWrites[acc.uid])}</p>
                {/if}

                {#if isOpen}
                  <div class="task-detail">
                    {#if st.loading}
                      <div class="table task-inner">
                        {#each Array(2) as _, i (i)}
                          <div class="row skeleton-row" style="--cols: 40%"><span class="skeleton"></span></div>
                        {/each}
                      </div>
                    {:else if st.error}
                      <p class="msg error">{st.error}</p>
                    {:else if !st.tasks || st.tasks.length === 0}
                      <p class="muted small">{t.tasks.noTasks}</p>
                    {:else}
                      <div class="table task-inner" role="table" aria-label={t.tasks.table.task}>
                        <div class="row head" role="row" style="--cols: var(--cols-tasks)">
                          <span role="columnheader">{t.tasks.table.task}</span>
                          <span role="columnheader">{t.tasks.table.progress}</span>
                          <span role="columnheader">{t.tasks.table.status}</span>
                          <span role="columnheader">{t.tasks.table.reward}</span>
                          <span class="right" role="columnheader"></span>
                        </div>
                        {#each st.tasks as task (task.code)}
                          {@const s = taskState(task)}
                          <div class="row" role="row" style="--cols: var(--cols-tasks)">
                            <span class="label" title={task.description || task.desc || task.code} role="cell">
                              <span class="truncate">{task.title || task.code}</span>
                              <em>{task.code}{task.mini_program ? ` · ${t.tasks.miniProgram}` : ''}</em>
                            </span>
                            <span class="cell-progress" role="cell">
                              {#if task.target > 0}
                                <span class="progress-track">
                                  <i style="width:{Math.min(100, Math.round(((task.current || 0) / task.target) * 100))}%"></i>
                                </span>
                                <span class="mono small">{task.current || 0}/{task.target}</span>
                              {:else}
                                <span class="muted small">{t.common.dash}</span>
                              {/if}
                            </span>
                            <span role="cell"><span class="status {s.tone}">{s.text}</span></span>
                            <span class="mono small" role="cell">
                              {#if task.credit > 0}+{task.credit} {t.tasks.rewardCredit}{/if}
                              {#if task.energy > 0}{task.credit > 0 ? ' · ' : ''}+{task.energy}{' '}
                                {t.tasks.rewardEnergy}{/if}
                            </span>
                            <span class="right actions" role="cell">
                              {#if task.claimable && !task.claimed}
                                <button class="btn link" onclick={() => claimTask(acc, task.code)} disabled={writeBlocked(acc)}>{t.tasks.actions.claim}</button>
                              {/if}
                              {#if !task.claimed}
                                <button class="btn link" onclick={() => runTask(acc, task.code)} disabled={writeBlocked(acc)}>{t.tasks.actions.run}</button>
                              {/if}
                            </span>
                          </div>
                        {/each}
                      </div>
                    {/if}

                    {#if results[acc.uid]?.length}
                      <ul class="results">
                        {#each resultRows(acc.uid) as r, i (r.task_code + i)}
                          <li class={r.status}>{describeResult(r)}</li>
                        {/each}
                      </ul>
                      {#if results[acc.uid].length > RESULT_ROWS}
                        <button class="btn link sm" onclick={() => toggleResults(acc.uid)}>
                          {resultsOpen[acc.uid] ? t.tasks.results.less : t.tasks.results.more(results[acc.uid].length)}
                        </button>
                      {/if}
                    {/if}
                  </div>
                {/if}
              </div>
            {/each}
          </div>
        {/if}
        </section>
      </div>
    {/if}

    {#if tab === 'usage'}
      {#if usageAvailable === false}
        <div class="card">
          <h2>{t.usage.title}</h2>
          <p>{t.usage.disabled}</p>
        </div>
      {:else}
        <div class="column">
          <div class="toolbar range-bar">
            <div class="segmented">
              {#each ['1', '3', '7', '30', '90'] as d (d)}
                <button class:active={usageRange === d} onclick={() => changeUsageRange(d)}>{t.usage.days(Number(d))}</button>
              {/each}
              <button class:active={usageRange === 'all'} onclick={() => changeUsageRange('all')}>{t.usage.daysAll}</button>
              <button class:active={usageRange === 'custom'} onclick={() => changeUsageRange('custom')}>
                <Icon name="calendar" size={15} />
                {t.usage.custom}
              </button>
            </div>
            {#if usageRange === 'custom'}
              <div class="range-pickers">
                <input type="datetime-local" bind:value={usageFrom} onchange={applyCustomRange} aria-label={t.usage.rangeFrom} />
                <span class="range-arrow" aria-hidden="true">→</span>
                <input type="datetime-local" bind:value={usageTo} onchange={applyCustomRange} aria-label={t.usage.rangeTo} />
              </div>
            {/if}
            <div class="toolbar-tail">
              <select
                class="src-select"
                bind:value={usageSource}
                onchange={loadUsage}
                aria-label={t.usage.sourceLabel}
              >
                <option value="">{t.usage.breakdown.allSources}</option>
                <option value="workbuddy">WB</option>
                <option value="trae-cn">Trae</option>
              </select>
              <div class="segmented" aria-label={t.usage.metric.label}>
                <button class:active={usageMetric === 'tokens'} onclick={() => (usageMetric = 'tokens')}>{t.usage.metric.tokens}</button>
                <button class:active={usageMetric === 'credits'} onclick={() => (usageMetric = 'credits')}>{t.usage.metric.credits}</button>
              </div>
            </div>
          </div>
          {#if sectionErr.usage}
            <div class="note error" role="alert">
              <Icon name="alert" size={16} />
              <span class="spacer">{sectionErr.usage}</span>
              <button class="btn ghost sm" onclick={loadUsage}>{t.common.retry}</button>
            </div>
          {/if}

          {#if !usageSummary}
            <!-- The page's own outline, drawn before the numbers arrive: a
                 72px head, a 200px chart, two folded rows. A generic three-bar
                 skeleton here would move the whole page by a screenful the
                 moment the data lands. -->
            <div class="usage-skeleton" aria-busy="true" aria-label={t.usage.loadingAria}>
              <div class="skel metric-head-skel"></div>
              <section class="card">
                <div class="card-head">
                  <div class="titles"><span class="skel skel-line" style="width:7rem"></span></div>
                </div>
                <div class="skel skel-chart"></div>
              </section>
              <div class="skel skel-fold"></div>
              <div class="skel skel-fold"></div>
            </div>
          {:else}
            <!-- The metric head: one headline in the unit the switch names,
                 its comparison against the equally long window before, and
                 three qualifying counts. Input/output/failed stay available in
                 the daily table and the breakdown; the head answers "how much"
                 before it answers "of what". -->
            <div class="metric-head">
              <div class="metric-main">
                <span class="metric-value">{usageHeadline.text}</span>
                <span class="metric-sub">
                  <span class="metric-label">{usageHeadline.label}</span>
                  {#if usageHeadline.delta !== null}
                    <span class="metric-dot" aria-hidden="true">·</span>
                    <span class="metric-delta-label">{t.usage.vsPrev}</span>
                    <span class="metric-delta" title={t.usage.vsPrev}>{fmtDelta(usageHeadline.delta)}</span>
                  {/if}
                </span>
              </div>
              <div class="metric-side">
                <div class="usage-metric">
                  <span class="k">{t.usage.stat.requests}</span>
                  <span class="v">{usageSummary.requests}</span>
                </div>
                <div class="usage-metric">
                  <span class="k">{t.usage.stat.successRate}</span>
                  <span class="v">{usageSummary.requests ? fmtPct((usageSummary.ok / usageSummary.requests) * 100) : fmtPct(0)}</span>
                </div>
                <div class="usage-metric">
                  <span class="k">{t.usage.stat.cacheHit}</span>
                  <span class="v">{fmtPct(cacheRate)}</span>
                </div>
              </div>
            </div>
          {/if}

          <section class="card chart-card">
              <div class="card-head">
                <div class="titles">
                  <h2>{usageMetric === 'credits' ? t.usage.aria.dailyCredits : t.usage.daily.title}</h2>
                  <span class="hint">
                    {usageMetric === 'credits'
                      ? t.usage.daily.peak(fmtCredits(chartPeak))
                      : t.usage.daily.peak(fmtTokens(chartPeak))}
                  </span>
                  {#if usageLoading}<span class="spinner inline" aria-hidden="true"></span>{/if}
                </div>
                <div class="legend">
                  {#if usageHasData}
                  {#if usageMetric === 'credits'}
                    <span class="legend-item"><span class="legend-dot band-credit"></span>{t.usage.chart.legendCredits}</span>
                  {:else}
                    <span class="legend-item"><span class="legend-dot band-output"></span>{t.usage.band.output}</span>
                    <span class="legend-item"><span class="legend-dot band-fresh"></span>{t.usage.band.fresh}</span>
                    <span class="legend-item"><span class="legend-dot band-cached"></span>{t.usage.band.cached}</span>
                  {/if}
                  {/if}
                  <button class="btn quiet sm" onclick={() => (chartExpanded = !chartExpanded)}>
                    {chartExpanded ? t.usage.chart.collapse : t.usage.chart.expand}
                  </button>
                </div>
              </div>
              {#if usageHasData}
              <div
                class="chart-area"
                class:expanded={chartExpanded}
                role="presentation"
                onmouseleave={() => (hoverDay = null)}
              >
                <div class="chart-grid" aria-hidden="true">
                  {#each [0, 1, 2, 3] as g (g)}
                    <div class="grid-line" style="top:{(g / 4) * 100}%"></div>
                  {/each}
                </div>
                <div class="stack-chart" role="img" aria-label={usageMetric === 'credits' ? t.usage.aria.dailyCredits : t.usage.aria.daily}>
                  {#each usageDaily as row, i (row.date)}
                    {@const b = tokenBands(row)}
                    <div
                      class="stack-col"
                      class:hovered={hoverDay === i}
                      role="presentation"
                      onmouseenter={() => (hoverDay = i)}
                    >
                      <div class="stack-bar">
                        {#if usageMetric === 'credits'}
                          <div class="seg band-credit" style="height:{((row.credits || 0) / chartPeak) * 100}%"></div>
                          {#if !row.credits}<div class="seg band-empty"></div>{/if}
                        {:else}
                          {#if b.cached}<div class="seg band-cached" style="height:{(b.cached / chartPeak) * 100}%"></div>{/if}
                          {#if b.fresh}<div class="seg band-fresh" style="height:{(b.fresh / chartPeak) * 100}%"></div>{/if}
                          {#if b.output}<div class="seg band-output" style="height:{(b.output / chartPeak) * 100}%"></div>{/if}
                          {#if !b.total}<div class="seg band-empty"></div>{/if}
                        {/if}
                      </div>
                      <span class="stack-label">{shortDate(row.date)}</span>
                    </div>
                  {/each}
                </div>
                {#if hoverDay !== null && usageDaily[hoverDay]}
                  {@const row = usageDaily[hoverDay]}
                  {@const b = tokenBands(row)}
                  <div class="chart-readout" role="status">
                    <span class="readout-date">{row.date}</span>
                    {#if usageMetric === 'credits'}
                      <span class="readout-item"><span class="legend-dot band-credit"></span>{t.usage.chart.legendCredits} {fmtCredits(row.credits || 0)}</span>
                    {:else}
                      <span class="readout-item"><span class="legend-dot band-output"></span>{t.usage.band.output} {fmtTokens(row.output_tokens)}</span>
                      <span class="readout-item"><span class="legend-dot band-fresh"></span>{t.usage.band.fresh} {fmtTokens(b.fresh)}</span>
                      <span class="readout-item"><span class="legend-dot band-cached"></span>{t.usage.band.cached} {fmtTokens(row.cached_tokens)}</span>
                      <span class="readout-item readout-total">{fmtTokens(row.total_tokens)}</span>
                    {/if}
                  </div>
                {/if}
              </div>
          {:else if !usageLoading}
            <!-- An empty window keeps the chart's outline: eight ghost bars
                 say "the canvas is here, the data is not", and the quick
                 windows offer the most likely span instead of a dead end. -->
            <div class="empty inline chart-empty">
                <div class="ghost-bars" aria-hidden="true">
                  {#each [38, 62, 45, 80, 30, 55, 42, 68] as h, i (i)}
                    <div class="ghost-bar" style="height:{h}%"></div>
                  {/each}
                </div>
                <p>{t.usage.empty}</p>
                <div class="empty-actions">
                  <span class="muted small">{t.usage.emptyHint}</span>
                  {#each ['3', '7', '30', 'all'] as d (d)}
                    <button class="btn ghost sm" onclick={() => changeUsageRange(d)}>
                      {d === 'all' ? t.usage.daysAll : t.usage.days(Number(d))}
                    </button>
                  {/each}
                </div>
            </div>
          {/if}
          </section>

          <!-- Credit spend is one quiet strip, not a second headline: the
               window's bill is context for the volume above it, and the
               per-unit rates only make sense next to each other. -->
          {#if usageSummary}
            <div class="credit-strip">
              <div class="credit-cell"><span class="k">{t.usage.credit.total}</span><span class="v">{fmtCredits(usageSummary.credits)}</span></div>
              <div class="credit-cell"><span class="k">{t.usage.credit.perDay}</span><span class="v">{fmtCredits(usageSummary.credits / Math.max(1, usageDaily.length))}</span></div>
              <div class="credit-cell"><span class="k">{t.usage.credit.perRequest}</span><span class="v">{usageSummary.requests ? fmtCredits(usageSummary.credits / usageSummary.requests) : t.common.dash}</span></div>
              <div class="credit-cell"><span class="k">{t.usage.credit.perMillion}</span><span class="v">{usageSummary.total_tokens ? fmtCredits(usageSummary.credits / (usageSummary.total_tokens / 1e6)) : t.common.dash}</span></div>
              <div class="credit-cell"><span class="k">{t.usage.credit.models}</span><span class="v">{usageSummary.models}</span></div>
            </div>
          {/if}

          <!-- The exact numbers behind the chart, folded: the canvas answers
               "when" at a glance and this table answers "precisely", without
               both owning the first screen. -->
          {#if usageSummary}
            <button class="fold-row" aria-expanded={dailyOpen} onclick={() => (dailyOpen = !dailyOpen)}>
              <Icon name="chevron" size={13} class={dailyOpen ? 'fold-chev open' : 'fold-chev'} />
              <span class="fold-title">{t.usage.fold.daily}</span>
              <span class="hint">{t.usage.fold.dailyCount(usageDaily.filter((r) => r.requests > 0).length)}</span>
              <span class="fold-action">{dailyOpen ? t.usage.chart.collapse : t.usage.chart.expand}</span>
            </button>
            {#if dailyOpen}
              <div class="table scroll" use:scrollFade>
                <div class="row head" style="--cols: var(--cols-daily)" role="row">
                  <span>{t.usage.dailyCol.date}</span>
                  <span>{t.usage.dailyCol.requests}</span>
                  <span>{t.usage.dailyCol.ok}</span>
                  <span>{t.usage.dailyCol.failed}</span>
                  <span>{t.usage.dailyCol.input}</span>
                  <span>{t.usage.dailyCol.output}</span>
                  <span>{t.usage.dailyCol.cached}</span>
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
                    <span class="muted small mono">{fmtTokens(row.cached_tokens)}</span>
                    <span class="muted small mono">{fmtCredits(row.credits)}</span>
                  </div>
                {/each}
              </div>
            {/if}
          {/if}

          <!-- The detail table is the drill of last resort: one row per
               request, folded until asked for, filters as chips because four
               selects in a row outweigh the row that opens them. -->
          <button class="fold-row" aria-expanded={recordsOpen} onclick={() => (recordsOpen = !recordsOpen)}>
            <Icon name="chevron" size={13} class={recordsOpen ? 'fold-chev open' : 'fold-chev'} />
            <span class="fold-title">{t.usage.fold.records}</span>
            <span class="hint">{t.usage.records.loaded(usageRecords.length)}</span>
            <span class="fold-action">{recordsOpen ? t.usage.chart.collapse : t.usage.chart.expand}</span>
          </button>
          {#if recordsOpen}
            <section class="card">
              <div class="card-head">
                <div class="titles">
                  <h2>{t.usage.records.title}</h2>
                  <span class="hint">{t.usage.records.loaded(usageRecords.length)}</span>
                </div>
                <div class="filters">
                  <div class="chip-row" role="group" aria-label={t.usage.records.aria.wire}>
                    {#each [
                      { value: '', label: t.usage.records.wireAll },
                      { value: 'chat', label: 'chat' },
                      { value: 'anthropic', label: 'anthropic' },
                      { value: 'responses', label: 'responses' },
                    ] as opt (opt.value)}
                      <button
                        class="chip"
                        aria-pressed={usageFilter.wire === opt.value}
                        onclick={() => {
                          usageFilter.wire = opt.value
                          loadUsageRecords(true)
                        }}>{opt.label}</button
                      >
                    {/each}
                  </div>
                  <div class="chip-row" role="group" aria-label={t.usage.records.aria.outcome}>
                    {#each [
                      { value: '', label: t.usage.records.outcomeAll },
                      { value: 'success', label: t.usage.records.outcomeSuccess },
                      { value: 'http_error', label: t.usage.records.outcomeHTTPError },
                    ] as opt (opt.value)}
                      <button
                        class="chip"
                        aria-pressed={usageFilter.outcome === opt.value}
                        onclick={() => {
                          usageFilter.outcome = opt.value
                          loadUsageRecords(true)
                        }}>{opt.label}</button
                      >
                    {/each}
                  </div>
                  <input
                    class="rec-model"
                    placeholder={t.usage.records.modelPlaceholder}
                    aria-label={t.usage.records.aria.model}
                    bind:value={usageFilter.model}
                    onchange={() => loadUsageRecords(true)}
                  />
                </div>
              </div>
              {#if sectionErr.usageRecords}
                <div class="note error" role="alert">
                  <Icon name="alert" size={16} />
                  <span class="spacer">{sectionErr.usageRecords}</span>
                  <button class="btn ghost sm" onclick={() => loadUsageRecords(true)}>
                    {t.common.retry}
                  </button>
                </div>
              {/if}
              <div class="table scroll records-scroll" use:scrollFade onscroll={onRecordsScroll}>
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
                {#if usageRecords.length === 0 && !recordsLoading}
                  <div class="row"><span class="muted small">{t.usage.records.empty}</span></div>
                {/if}
                {#each usageRecords as rec (rec.request_id)}
                  <div class="row" style="--cols: var(--cols-records)">
                    <span class="muted small">{fmtDateTime(rec.time)}</span>
                    <span class="mono small truncate">{rec.wire || t.common.dash}</span>
                    <span class="muted small truncate">{rec.account || t.common.dash}</span>
                    <span class="mono small truncate">{rec.model || t.common.dash}</span>
                    <span><span class="badge {rec.outcome === 'success' ? 'ok' : 'bad'}">{rec.status}</span></span>
                    <span class="muted small mono">{rec.duration_ms}ms</span>
                    <span class="cell-token">
                      <span class="mono small">{fmtTokens(rec.total_tokens)}</span>
                      {#if rec.cached_tokens}<em>cache {fmtTokens(rec.cached_tokens)}</em>{/if}
                    </span>
                    <span class="muted small" title={rec.credits_source || ''}>
                      {#if rec.credits_source === 'pending'}<span class="badge warn">{t.logs.pending}</span>
                      {:else if rec.credits_source}{fmtCredits(rec.credits)}
                      {:else}{t.common.dash}{/if}
                    </span>
                  </div>
                {/each}
                {#if recordsLoading}
                  <div class="row"><span class="muted small"><span class="spinner inline"></span> {t.common.loading}</span></div>
                {/if}
                {#if recordsExhausted && usageRecords.length > 0}
                  <div class="row">
                    <span class="muted small">
                      {recordsCapped
                        ? t.usage.records.capped(usageRecords.length)
                        : t.usage.records.end}
                    </span>
                  </div>
                {/if}
              </div>
            </section>
          {/if}

          <!-- Drill-down: the dimension table on the left (a lens on data
               already in memory), the credit-spend bars on the right (who the
               window's bill went to). 1.4:1 keeps the table readable while the
               bars stay a companion, not a rival. -->
          <div class="drill-grid">
            <section class="card drill-main">
              <div class="card-head">
                <div class="titles">
                  <h2>{t.usage.breakdown.title}</h2>
                  <span class="hint">{t.usage.breakdown.hint(usageFilteredRows.length)}</span>
                </div>
                <div class="btn-row">
                  <div class="segmented">
                    <button class:active={usageDim === 'account'} onclick={() => setUsageDim('account')}>{t.usage.dim.account}</button>
                    <button class:active={usageDim === 'model'} onclick={() => setUsageDim('model')}>{t.usage.dim.model}</button>
                    <button class:active={usageDim === 'wire'} onclick={() => setUsageDim('wire')}>{t.usage.dim.wire}</button>
                  </div>
                </div>
              </div>
              {#if usageFilteredRows.length === 0}
                <p class="muted small">{t.usage.breakdown.empty}</p>
              {:else}
                <div class="table scroll" use:scrollFade>
                  <div class="row head" style="--cols: var(--cols-breakdown)">
                    <span>{t.usage.breakdown.col.name[usageDim]}</span>
                    <span>{t.usage.breakdown.col.requests}</span>
                    <span>{t.usage.breakdown.col.failed}</span>
                    <span>{t.usage.breakdown.col.input}</span>
                    <span>{t.usage.breakdown.col.output}</span>
                    <span>{t.usage.breakdown.col.total}</span>
                    <span>{t.usage.breakdown.col.credits}</span>
                  </div>
                  {#each breakdownRows as r (rowKey(r))}
                    <div class="row" style="--cols: var(--cols-breakdown)">
                      <span class="label">
                        {#if usageDim === 'account'}
                          <span class="src-chip">
                            <span class="src-dot" data-source={r.source}></span>
                            <span class="truncate">{r.account || t.common.dash}</span>
                          </span>
                        {:else}
                          <span class="mono truncate">{usageDim === 'wire' ? r.wire : r.model || t.common.dash}</span>
                        {/if}
                      </span>
                      <span class="small mono">{r.requests}</span>
                      <span class="small mono" class:bad-text={r.failed > 0}>{r.failed || 0}</span>
                      <span class="muted small mono">{fmtTokens(r.input_tokens)}</span>
                      <span class="muted small mono">{fmtTokens(r.output_tokens)}</span>
                      <span class="small mono">{fmtTokens(r.total_tokens)}</span>
                      <span class="muted small mono">{fmtCredits(r.credits)}</span>
                    </div>
                  {/each}
                </div>
                {#if usageFilteredRows.length > 10}
                  <button class="btn quiet sm drill-more" onclick={() => (breakdownAll = !breakdownAll)}>
                    {breakdownAll ? t.usage.breakdown.showLess : t.usage.breakdown.showAll}
                  </button>
                {/if}
              {/if}
            </section>

            <section class="card drill-side">
              <div class="card-head">
                <div class="titles">
                  <h2>{t.usage.breakdown.spend.title}</h2>
                  <span class="hint">{t.usage.breakdown.spend.hint}</span>
                </div>
                <div class="segmented">
                  <button class:active={spendDim === 'account'} onclick={() => (spendDim = 'account')}>{t.usage.breakdown.spend.byAccount}</button>
                  <button class:active={spendDim === 'source'} onclick={() => (spendDim = 'source')}>{t.usage.breakdown.spend.bySource}</button>
                </div>
              </div>
              {#if spendRows.length === 0}
                <p class="muted small">{t.usage.breakdown.spend.empty}</p>
              {:else}
                <ul class="spend-bars">
                  {#each spendRows as row (row.key)}
                    <li class="spend-row">
                      <span class="spend-label">
                        {#if row.source && row.source !== 'unknown'}
                          <span class="src-dot" data-source={row.source}></span>
                        {/if}
                        <span class="truncate" title={row.label}>{row.label}</span>
                      </span>
                      <span class="spend-track">
                        <span class="spend-fill" style="width:{Math.max(2, (row.credits / spendPeak) * 100)}%"></span>
                      </span>
                      <span class="spend-num">{fmtCredits(row.credits)}</span>
                    </li>
                  {/each}
                </ul>
              {/if}
            </section>
          </div>

        </div>
      {/if}
    {/if}

    {#if tab === 'models'}
      <!-- One card per platform. The same model name is often carried by both
           sources at different windows and prices, so one merged table would
           print one platform's price under the other's name. -->
      <div class="models-toolbar">
        <label class="search">
          <Icon name="search" size={15} />
          <input
            type="search"
            placeholder={t.models.search}
            aria-label={t.models.searchAria}
            bind:value={modelsQuery}
          />
        </label>
        <div class="toolbar-tail">
          <div class="segmented">
            <button class:active={modelsCap === ''} onclick={() => (modelsCap = '')}>{t.models.filterAll}</button>
            <button class:active={modelsCap === 'reasoning'} onclick={() => (modelsCap = 'reasoning')}>{t.models.filters.reasoning}</button>
            <button class:active={modelsCap === 'vision'} onclick={() => (modelsCap = 'vision')}>{t.models.filters.vision}</button>
            <button class:active={modelsCap === 'tools'} onclick={() => (modelsCap = 'tools')}>{t.models.filters.tools}</button>
          </div>
          <button class="btn stroke sm" onclick={loadModels} disabled={modelsLoading}>{t.models.reload}</button>
        </div>
      </div>

      {#if sectionErr.models}
        <div class="note error" role="alert">
          <Icon name="alert" size={16} />
          <span class="spacer">{sectionErr.models}</span>
          <button class="btn ghost sm" onclick={loadModels}>{t.common.retry}</button>
        </div>
      {/if}

      {#if modelsLoading && !modelRows.length}
        <section class="card group">
          <div class="table">
            {#each Array(3) as _}
              <div class="row skeleton-row">
                <span class="skeleton"></span><span class="skeleton"></span><span class="skeleton"></span>
              </div>
            {/each}
          </div>
        </section>
      {:else if !modelRows.length}
        <section class="card group models-empty">
          <div class="empty inline">
            <p>{t.models.empty}</p>
            <p class="muted small">{t.models.emptyHint}</p>
          </div>
        </section>
      {:else}
        {#each modelPlatforms as group (group.source)}
          {@const collapsed = !!collapsedPlatforms[group.source]}
          <section class="card group">
            <button
              class="group-head"
              aria-expanded={!collapsed}
              title={collapsed ? t.accounts.group.expand(group.label) : t.accounts.group.collapse(group.label)}
              onclick={() => togglePlatform(group.source)}
            >
              <Icon name="chevronRight" size={13} class={collapsed ? 'caret' : 'caret open'} />
              <span class="group-name">{group.label}</span>
              <span class="group-count">{modelsCap || modelsQuery ? `${group.rows.length}/${group.total}` : group.total}</span>
              <span class="spacer"></span>
              <span class="group-meta">{t.models.accounts(group.accounts)}</span>
            </button>

            {#if !collapsed}
              <div class="table scroll models-table" use:scrollFade>
                <div class="row head" style="--cols: var(--cols-models)">
                  <span>{t.models.col.model}</span>
                  <span class="num" role="columnheader" aria-sort={modelSortState('context')}>
                    <button class="th-btn" onclick={() => cycleModelSort('context')}>
                      {t.models.col.context}
                      <Icon name={modelSortState('context') === 'ascending' ? 'arrowUp' : 'arrowDown'} size={13} class={modelSortState('context') === 'none' ? 'sort-arrow dim' : 'sort-arrow'} />
                    </button>
                  </span>
                  <span>{t.models.col.efforts}</span>
                  <span>{t.models.col.caps}</span>
                  <span class="num">{t.models.col.maxOutput}</span>
                  <span class="num" role="columnheader" aria-sort={modelSortState('rate')}>
                    <button class="th-btn" onclick={() => cycleModelSort('rate')}>
                      {t.models.col.rate}
                      <Icon name={modelSortState('rate') === 'ascending' ? 'arrowUp' : 'arrowDown'} size={13} class={modelSortState('rate') === 'none' ? 'sort-arrow dim' : 'sort-arrow'} />
                    </button>
                  </span>
                </div>
                {#if !group.rows.length}
                  <div class="empty inline">
                    <p>{t.models.emptyFiltered}</p>
                    <button
                      class="btn ghost sm"
                      onclick={() => {
                        modelsQuery = ''
                        modelsCap = ''
                      }}>{t.models.clearFilter}</button
                    >
                  </div>
                {:else}
                  {#each group.rows as m (m.source + '/' + m.id)}
                    {@const caps = (m.capabilities ?? []).filter((c) => c !== 'max')}
                    <div class="row" style="--cols: var(--cols-models)">
                      <span class="model-cell" title={`${m.source}/${m.id}`}>
                        <span class="src-name mono">{m.source}/</span><span class="model-name mono truncate">{m.id}</span>
                        {#if m.default}<span class="badge primary">{t.models.defaultMark}</span>{/if}
                      </span>
                      <span class="cell-num">
                        {#if m.context_length}{fmtTokens(m.context_length)}{:else}{t.common.dash}{/if}
                      </span>
                      <span class="cell-efforts">
                        {#if (m.thinking_efforts ?? []).length}
                          {#each m.thinking_efforts as effort (effort)}<span class="tag warm">{effort}</span>{/each}
                        {:else}
                          <span class="tag gray">{t.models.effortsOff}</span>
                        {/if}
                      </span>
                      <span class="cell-caps">
                        {#each caps as cap (cap)}<span class="badge info">{t.models.cap[cap] || cap}</span>{/each}
                      </span>
                      <span class="cell-num">
                        {m.max_output_tokens ? fmtTokens(m.max_output_tokens) : t.common.dash}
                      </span>
                      <!-- The price cell tells the truth in three shapes: a
                           discount in force (effective rate + label + the
                           struck listed rate), an offer that states only a
                           label (off-peak: listed rate + label), or a plain
                           rate. -->
                      <span class="cell-rate" title={m.promo_note || ''}>
                        {#if m.promo_credits}
                          <span class="rate-now">{m.promo_credits}</span>
                          {#if m.promo_label}<span class="tag" class:free={m.promo_factor === 0}>{m.promo_label}</span>{/if}
                          {#if m.credits}<span class="rate-was">{m.credits}</span>{/if}
                        {:else if m.credits}
                          <span class="rate-now">{m.credits}</span>
                          {#if m.promo_label}<span class="tag">{m.promo_label}</span>{/if}
                        {:else}
                          <span class="cell-num">{t.common.dash}</span>
                        {/if}
                      </span>
                    </div>
                  {/each}
                {/if}
              </div>
            {/if}
          </section>
        {/each}
      {/if}
      {#each Object.entries(modelFailed) as [uid, err]}
        <p class="msg error">{t.models.failed(uid, err)}</p>
      {/each}

      <!-- Connectivity test. One model per run, read from the catalogue above;
           the capabilities that catalogue states gate the content row, and the
           answer lands on a single line that opens the raw exchange. -->
      <section class="card probe-card">
        <div class="card-head">
          <div class="titles">
            <h2>{t.models.test.title}</h2>
            <span class="hint">{modelRows.length ? t.models.test.hint : t.models.test.hintEmpty}</span>
          </div>
          {#if probeMsg}<span class="meta-inline">{probeMsg}</span>{/if}
        </div>

        <div class="probe-body">
          <div class="probe-row">
            <div class="probe-field probe-field-model">
              <span class="field-label">{t.models.test.modelLabel}</span>
              <button
                type="button"
                class="probe-select"
                class:off={probeRunning}
                bind:this={probeTriggerEl}
                aria-haspopup="listbox"
                aria-expanded={probePickerOpen}
                aria-label={t.models.test.pickerAria}
                disabled={probeRunning}
                onclick={() => (probePickerOpen ? closeProbePicker() : openProbePicker())}
              >
                {#if probeRow}
                  <span class="probe-src">{probeSrcPrefix(probeRow)}</span><span class="probe-name">{probeRow.id}</span>
                {:else}
                  <span class="probe-ph">{t.models.test.modelPlaceholder}</span>
                {/if}
                <span class="spacer"></span>
                <Icon name="chevron" size={8} />
              </button>
            </div>

            <div class="probe-field">
              <span class="field-label">{t.models.test.contentLabel}</span>
              <div class="probe-checks">
                <label
                  class="probe-check"
                  class:on={probeModes.includes('text')}
                  class:off={!probeCan.text}
                  class:locked={probeRunning}
                  title={probeRow ? t.models.test.modeTextOff : t.models.test.needModel}
                >
                  <input
                    type="checkbox"
                    checked={probeModes.includes('text')}
                    disabled={!probeCan.text || probeRunning}
                    onchange={() => toggleProbeMode('text')}
                  />
                  <span class="probe-box" aria-hidden="true"><Icon name="check" size={9} stroke={2.5} /></span>
                  <span>{t.models.test.modeText}</span>
                </label>
                <label
                  class="probe-check"
                  class:on={probeModes.includes('tool')}
                  class:off={!probeCan.tool}
                  class:locked={probeRunning}
                  title={probeRow ? t.models.test.modeToolOff : t.models.test.needModel}
                >
                  <input
                    type="checkbox"
                    checked={probeModes.includes('tool')}
                    disabled={!probeCan.tool || probeRunning}
                    onchange={() => toggleProbeMode('tool')}
                  />
                  <span class="probe-box" aria-hidden="true"><Icon name="check" size={9} stroke={2.5} /></span>
                  <span>{t.models.test.modeTool}</span>
                </label>
                <label
                  class="probe-check"
                  class:on={probeThinking}
                  class:off={!probeCan.thinking}
                  class:locked={probeRunning}
                  title={probeRow ? t.models.test.thinkingOff : t.models.test.needModel}
                >
                  <input type="checkbox" bind:checked={probeThinking} disabled={!probeCan.thinking || probeRunning} />
                  <span class="probe-box" aria-hidden="true"><Icon name="check" size={9} stroke={2.5} /></span>
                  <span>{t.models.test.thinking}</span>
                </label>
              </div>
            </div>

            <div class="probe-field">
              <span class="field-label">{t.models.test.effortLabel}</span>
              <div class="probe-select-wrap" class:off={!probeCan.effort || probeRunning}>
                <select
                  class="probe-effort"
                  bind:value={probeEffort}
                  disabled={!probeCan.effort || probeRunning}
                  title={probeCan.effort ? '' : t.models.test.effortOff}
                >
                  <option value="">{t.models.test.effortDefault}</option>
                  {#each probeEfforts as effort (effort)}
                    <option value={effort}>{effort}</option>
                  {/each}
                </select>
                <Icon name="chevron" size={8} />
              </div>
            </div>

            <span class="spacer"></span>

            <button
              type="button"
              class="probe-timeout"
              class:off={probeRunning || !modelRows.length}
              bind:this={probeTimeoutEl}
              aria-haspopup="listbox"
              aria-expanded={probeTimeoutMenu}
              disabled={probeRunning}
              onclick={() => (probeTimeoutMenu ? closeProbeTimeoutMenu() : openProbeTimeoutMenu())}
            >
              <span class="probe-timeout-label">{t.models.test.timeoutLabel}</span>
              <span class="probe-timeout-value">{probeTimeout}s</span>
              <Icon name="chevron" size={8} />
            </button>

            <button
              type="button"
              class="btn probe-run"
              class:primary={!!probeRow && !!probeModes.length}
              class:running={probeRunning}
              disabled={probeRunning || !probeRow || !probeModes.length}
              onclick={runProbe}
            >
              {#if probeRunning}
                <span class="spinner"></span>
                <span>{t.models.test.running}</span>
                <span class="probe-run-clock">{(probeElapsed / 1000).toFixed(1)}s</span>
              {:else}
                <span>{t.models.test.run}</span>
              {/if}
            </button>
          </div>

          <div class="probe-rule"></div>

          <div class="probe-result">
            {#if probeRunning}
              <span class="probe-pill info">{t.models.test.testing}</span>
              <span class="probe-status">
                {probeModes.length > 1
                  ? t.models.test.runningStep(probeModeLabel(probeModes[Math.max(0, probeStep)]), Math.min(probeStep + 1, probeModes.length), probeModes.length)
                  : t.models.test.runningWait}
              </span>
              <span class="spacer"></span>
              <span class="probe-timeout-hint">{t.models.test.timeoutIn(probeTimeout)}</span>
            {:else if probeLast}
              <span class="probe-pill" class:ok={probeLast.ok} class:danger={!probeLast.ok}>
                {probeLast.ok ? t.models.test.pass : t.models.test.fail}
              </span>
              <span class="probe-model">
                <span class="probe-src">{probeSrcPrefix(probeLast)}</span><span class="probe-name">{probeLast.model}</span>
              </span>
              <span class="probe-sep">·</span>
              <span class="probe-content">{probeLast.modes.map(probeModeLabel).join(' + ')}</span>
              <span class="probe-sep">·</span>
              <span class="probe-elapsed">{t.models.test.elapsed(probeLast.elapsed_ms)}</span>
              <span class="probe-sep">·</span>
              {#if probeLast.ok}
                <span class="probe-cost">{fmtCredits(probeLast.credits)}</span>
                <span class="probe-cost-unit">{t.models.test.creditUnit}</span>
              {:else}
                <span class="probe-reason">{probeReason(probeLast.steps.find((s) => !s.ok))}</span>
              {/if}
              <span class="spacer"></span>
              <span class="probe-at" title={fmtStamp(probeLast.at)}>{fmtDayTime(probeLast.at)}</span>
              <button type="button" class="probe-detail" class:dim={!probeLast.ok} onclick={() => (probeDrawer = true)}>
                {t.models.test.detail}
                <Icon name="chevronRight" size={14} />
              </button>
              {#if !probeLast.ok}
                <button type="button" class="probe-retry" onclick={runProbe}>{t.models.test.retry}</button>
              {/if}
            {:else}
              <span class="probe-empty-value">{t.models.test.resultEmpty}</span>
              <span class="probe-empty-hint">{t.models.test.resultEmptyHint}</span>
              <span class="spacer"></span>
            {/if}
          </div>
        </div>
      </section>

      {#if probePickerOpen}
        <div
          class="probe-picker"
          role="listbox"
          aria-label={t.models.test.pickerAria}
          bind:this={probeMenuEl}
          style={probePickerCss}
        >
          <div class="probe-picker-search">
            <input
              type="search"
              bind:this={probeSearchEl}
              bind:value={probePickerQuery}
              placeholder={t.models.test.pickerSearch}
              oninput={() => (probePickerIndex = 0)}
              onkeydown={probePickerKey}
            />
          </div>
          <div class="probe-picker-rule"></div>
          <div class="probe-picker-list">
            {#each probeGroups as group (group.source)}
              <div class="probe-picker-group">{t.models.test.groupCount(group.label, group.rows.length)}</div>
              {#each group.rows as m (m.id)}
                {@const key = `${m.source || 'unknown'}/${m.id}`}
                <button
                  type="button"
                  class="probe-picker-option"
                  class:on={key === probeModel}
                  role="option"
                  aria-selected={key === probeModel}
                  data-probe-option={probeOptions.indexOf(key)}
                  onclick={() => pickProbeModel(key)}
                >
                  <span class="probe-src">{probeSrcPrefix(m)}</span><span class="probe-name">{m.id}</span>
                </button>
              {/each}
            {/each}
            {#if !probeOptions.length}
              <div class="probe-picker-empty">{t.models.test.pickerEmpty}</div>
            {/if}
          </div>
        </div>
      {/if}

      {#if probeTimeoutMenu}
        <div
          class="probe-tmenu"
          role="listbox"
          aria-label={t.models.test.timeoutLabel}
          bind:this={probeTimeoutMenuEl}
          style={probeTimeoutCss}
        >
          {#each PROBE_TIMEOUTS as v (v)}
            <button
              type="button"
              class="probe-tmenu-item"
              class:on={probeTimeout === v}
              role="option"
              aria-selected={probeTimeout === v}
              onclick={() => pickProbeTimeout(v)}
            >
              <span class="probe-tmenu-check">{probeTimeout === v ? '✓' : ''}</span>
              <span class="probe-tmenu-value">{v}s</span>
            </button>
          {/each}
          <div class="probe-tmenu-rule"></div>
          <div class="probe-tmenu-custom">
            <input
              type="number"
              min="10"
              max="600"
              bind:this={probeTimeoutInputEl}
              bind:value={probeTimeoutDraft}
              placeholder={t.models.test.timeoutCustom}
              onkeydown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault()
                  commitProbeTimeoutDraft()
                }
              }}
              onblur={() => commitProbeTimeoutDraft()}
            />
            <span class="probe-tmenu-unit">秒</span>
          </div>
        </div>
      {/if}

      {#if probeDrawer && probeLast}
        <button class="probe-veil" aria-label={t.common.close} onclick={() => (probeDrawer = false)}></button>
        <div
          class="probe-drawer"
          use:focusInto
          role="dialog"
          aria-modal="false"
          aria-label={t.models.test.drawer.title}
        >
          <header class="probe-drawer-head">
            <h2>{t.models.test.drawer.title}</h2>
            <button type="button" class="btn ghost icon" aria-label={t.common.close} onclick={() => (probeDrawer = false)}>
              <Icon name="close" size={16} />
            </button>
          </header>
          <div class="probe-drawer-body">
            <section class="probe-drawer-sec">
              <h3>{t.models.test.drawer.request}</h3>
              <dl class="probe-facts">
                <dt>{t.models.test.drawer.model}</dt>
                <dd class="mono12">{probeLast.source}/{probeLast.model}</dd>
                <dt>{t.models.test.drawer.mode}</dt>
                <dd>{probeLast.modes.map(probeModeLabel).join(' + ')}</dd>
                <dt>{t.models.test.drawer.effort}</dt>
                <dd>{probeLast.effort || t.models.test.effortDefault}</dd>
                <dt>{t.models.test.drawer.thinking}</dt>
                <dd>{probeLast.thinking ? t.models.test.drawer.thinkingOn : t.models.test.drawer.thinkingOff}</dd>
                <dt>{t.models.test.drawer.timeout}</dt>
                <dd class="mono12">{probeLast.timeout}s</dd>
                <dt>{t.models.test.drawer.account}</dt>
                <dd class="mono12">{probeLast.account || t.common.dash}</dd>
                <dt>{t.models.test.drawer.prompt}</dt>
                <dd class="probe-pre">{probeLast.steps[0]?.prompt || t.common.dash}</dd>
              </dl>
            </section>
            <section class="probe-drawer-sec">
              <h3>{t.models.test.drawer.response}</h3>
              {#each probeLast.steps as s, i (i)}
                <div class="probe-step">
                  <div class="probe-step-head">
                    <span class="probe-pill" class:ok={s.ok} class:danger={!s.ok}>
                      {s.ok ? t.models.test.pass : t.models.test.fail}
                    </span>
                    <span class="probe-step-label">
                      {t.models.test.drawer.stepOf(i + 1, probeLast.steps.length)} · {probeModeLabel(s.mode)}
                    </span>
                    <span class="spacer"></span>
                    <span class="probe-step-elapsed">{t.models.test.elapsed(s.elapsed_ms || 0)}</span>
                  </div>
                  {#if s.error}<p class="probe-drawer-err">{probeReason(s)}</p>{/if}
                  {#if s.content}
                    <div class="probe-kv">
                      <span class="probe-kv-label">{t.models.test.drawer.reply}</span>
                      <pre class="probe-pre">{s.content}</pre>
                    </div>
                  {:else}
                    <div class="probe-kv">
                      <span class="probe-kv-label">{t.models.test.drawer.reply}</span>
                      <span class="probe-kv-empty">{t.models.test.drawer.noContent}</span>
                    </div>
                  {/if}
                  {#if s.reasoning}
                    <div class="probe-kv">
                      <span class="probe-kv-label">{t.models.test.drawer.reasoning}</span>
                      <pre class="probe-pre dim">{s.reasoning}</pre>
                    </div>
                  {/if}
                  {#if s.tool_calls?.length}
                    <div class="probe-kv">
                      <span class="probe-kv-label">{t.models.test.drawer.toolCalls}</span>
                      {#each s.tool_calls as tc, ti (ti)}
                        <div class="probe-tool">
                          <span class="mono12">{tc.name}</span>
                          <pre class="probe-pre dim">{tc.arguments || t.common.dash}</pre>
                        </div>
                      {/each}
                    </div>
                  {/if}
                  <div class="probe-facts probe-step-facts">
                    {#if s.finish_reason}
                      <span>{t.models.test.drawer.finish} <b class="mono12">{s.finish_reason}</b></span>
                    {/if}
                    {#if s.total_tokens}
                      <span>{t.models.test.drawer.tokens} <b class="mono12">{fmtTokens(s.total_tokens)}</b></span>
                    {/if}
                    {#if s.credits}
                      <span>{t.models.test.drawer.credits} <b class="mono12">{fmtCredits(s.credits)}</b></span>
                    {/if}
                    {#if s.status}
                      <span>{t.models.test.drawer.status} <b class="mono12">{s.status}</b></span>
                    {/if}
                  </div>
                </div>
              {/each}
            </section>
          </div>
        </div>
      {/if}
    {/if}

    {#if tab === 'credits'}
      {#if creditsErr}
        <div class="note error" role="alert">
          <Icon name="alert" size={16} />
          <span class="spacer">{creditsErr}</span>
          <button class="btn ghost sm" onclick={loadCredits}>{t.common.retry}</button>
        </div>
      {/if}

      {#if creditsFirstLoad}
        <!-- The outline this page settles into: an 88px balance band, the
             300px lapse chart, the 176px account cards and the detail table.
             Reading packs is one upstream query per account, so this is the
             state the page sits in longest. -->
        <div class="credits-skeleton" aria-busy="true" aria-label={t.credits.loadingAria}>
          <div class="skel skel-block" style="height:88px"></div>
          <div class="skel skel-block" style="height:300px"></div>
          <div class="skel skel-block" style="height:176px"></div>
          <div class="skel skel-block" style="height:456px"></div>
        </div>
      {:else}

      <!-- Balance is per-platform: the two totals are stated side by side and
           never added up, because the upstreams meter different things into
           different packages. One pooled number would answer no question. The
           third figure counts packs only: an account-level earliest date says
           when the first batch dies, never how much goes with it. -->
      <section class="card metric-band">
        {#each creditsPlatforms as s (s.source)}
          <div class="metric-cell">
            <span class="v mono">{s.known ? fmtCredits(s.remain) : t.common.dash}</span>
            <span class="k">
              <span class="src-dot" data-source={s.source}></span>
              {sourceName(s.source)}
            </span>
          </div>
        {/each}
        <span class="metric-vrule" aria-hidden="true"></span>
        <div class="metric-cell">
          <span class="v mono">
            {creditExpiry.rows.length ? fmtCredits(creditSoon) : t.common.dash}
          </span>
          <span class="k">{t.credits.soon}</span>
        </div>
        <span class="spacer"></span>
        {#if !creditsProbed}
          <span class="metric-hint">{t.credits.perSourceUnread}</span>
        {/if}
        <button class="btn ghost sm" onclick={loadCredits} disabled={creditsLoading}>
          {creditsLoading ? t.credits.refreshing : t.credits.check}
        </button>
      </section>

      <!-- View 1: every pack that has a lapse date, on one shared axis, bucketed
           by how soon it dies. The legend is the two platforms, which is also
           the only place the balances are stated side by side unsummed. -->
      <section class="card">
        <div class="card-head">
          <div class="titles">
            <h2>{t.credits.expiry.title}</h2>
            <span class="hint">{t.credits.expiry.hint}</span>
          </div>
          <div class="credit-legend">
            {#each creditsPlatforms as p (p.source)}
              <span class="legend-item">
                <span class="src-dot" data-source={p.source}></span>
                {sourceName(p.source)}
              </span>
            {/each}
          </div>
        </div>

        {#if creditExpiry.rows.length === 0}
          <div class="empty inline">
            <p>{creditsLoading ? t.credits.probing : t.credits.expiry.empty}</p>
          </div>
        {:else}
          <div class="expiry-rows">
            {#each creditExpiry.rows as row (row.days)}
              <div class="expiry-row">
                <span class="when" title={row.far ? t.credits.expiry.farHint(row.far) : undefined}>
                  {row.days < 0
                    ? t.credits.expiry.expired
                    : row.far
                      ? t.credits.expiry.far
                      : t.credits.expiry.days(row.days)}
                </span>
                <div class="expiry-track">
                  <div
                    class="expiry-fill"
                    style="width:{Math.min(100, (row.total / creditExpiry.peak) * 100)}%"
                  >
                    {#each creditsPlatforms as p (p.source)}
                      {@const amount = row.amounts[p.source] || 0}
                      {#if amount > 0}
                        <span
                          class="expiry-seg"
                          data-source={p.source}
                          style="width:{(amount / Math.max(1, row.total)) * 100}%; opacity:{expiryOpacity(row.days)}"
                          title="{sourceName(p.source)} · {fmtCredits(amount)}"
                        ></span>
                      {/if}
                    {/each}
                  </div>
                </div>
                <span class="sum mono">{fmtCredits(row.total)}</span>
              </div>
            {/each}
          </div>
          {#if creditExpiry.undated > 0}
            <p class="expiry-foot">
              {t.credits.expiry.undated(creditExpiry.undatedCount, fmtCredits(creditExpiry.undated))}
            </p>
          {/if}
        {/if}
      </section>

      <!-- View 2: one card per account. The composition bar answers "why does
           this account hold more than that one" — the answer is packages, not
           usage. -->
      {#if creditCards.length === 0}
        <div class="empty inline"><p>{t.credits.cardsEmpty}</p></div>
      {:else}
        <div class="credit-cards">
          {#each creditCards as row (row.key)}
            <div class="credit-card">
              <div class="credit-card-head">
                <span class="src-dot" data-source={row.source}></span>
                <span class="who" title={row.uid}>{row.label || row.uid}</span>
                <span class="count">
                  {row.local || row.failed || row.known === false
                    ? t.common.dash
                    : t.credits.packs.count(row.packCount)}
                </span>
              </div>
              {#if row.local}
                <span class="credit-note">{t.credits.localNote}</span>
              {:else if row.failed}
                <span class="credit-note">{t.credits.failed}</span>
                {#if row.error}
                  <span class="credit-note truncate" title={row.error}>{row.error}</span>
                {/if}
                <button class="btn ghost xs" onclick={loadCredits}>{t.common.retry}</button>
              {:else if row.unlimited}
                <div class="credit-figure">
                  <span class="v">∞</span>
                  <span class="total">{t.credits.unlimited}</span>
                </div>
              {:else if row.known === false}
                <div class="credit-figure"><span class="v muted">{t.common.dash}</span></div>
                <span class="credit-note">{t.credits.notReadCredit}</span>
              {:else}
                <div class="credit-figure">
                  <span class="v mono">{fmtCredits(row.remain)}</span>
                  <span class="total">{t.credits.cards.total(fmtCredits(row.total))}</span>
                </div>
                {#if row.sources.length}
                  <div class="mixbar">
                    {#each row.sources as s, i (s.name)}
                      <i
                        data-source={row.source}
                        style="width:{(s.size / Math.max(1, row.total)) * 100}%; opacity:{1 - i * 0.25}"
                        title="{s.name} · {fmtCredits(s.size)}"
                      ></i>
                    {/each}
                  </div>
                  <span class="credit-note">
                    {row.nextExpiry
                      ? t.credits.cards.recent(fmtExpiryDay(row.nextExpiry), fmtExpiry(row.nextExpiry))
                      : t.credits.expiry.noDate}
                  </span>
                  <div class="src-legend">
                    {#each row.sources as s, i (s.name)}
                      <div class="src-legend-row">
                        <span class="swatch" data-source={row.source} style="opacity:{1 - i * 0.25}"></span>
                        <span class="nm" title={s.name}>{s.name}</span>
                        <span class="meta">
                          {[t.credits.cards.self(s.n), fmtCredits(s.size), s.created ? t.credits.cards.since(fmtExpiryDay(s.created)) : '']
                            .filter(Boolean)
                            .join(' · ')}
                        </span>
                      </div>
                    {/each}
                    {#if row.moreCount > 0}
                      <div class="src-legend-row">
                        <span class="swatch" data-source="unknown"></span>
                        <span class="nm">{t.credits.cards.more(row.moreCount)}</span>
                        <span class="meta">{fmtCredits(row.moreSize)}</span>
                      </div>
                    {/if}
                  </div>
                {:else}
                  <span class="credit-note">{t.credits.packs.empty}</span>
                {/if}
              {/if}
            </div>
          {/each}
        </div>
      {/if}

      <!-- View 3: the same accounts, per batch. Five rows then two folds: a
           table that lists twenty buries the five that matter. -->
      <section class="card credits-packs">
        <div class="card-head">
          <div class="titles">
            <h2>{t.credits.packs.title}</h2>
            <span class="hint">{t.credits.packs.hint}</span>
          </div>
          <div class="segmented" role="group" aria-label={t.credits.packs.sortAria}>
            <button class:active={creditsSort === 'end'} onclick={() => (creditsSort = 'end')}>
              {t.credits.packs.sortEnd}
            </button>
            <button class:active={creditsSort === 'size'} onclick={() => (creditsSort = 'size')}>
              {t.credits.packs.sortSize}
            </button>
          </div>
        </div>
        {#if creditGroups.length === 0}
          <!-- Without this branch the card kept its header and rendered nothing
               underneath: a blank card reads as a load failure, not as "no
               accounts yet". -->
          <div class="empty inline">
            <p>{t.credits.packsEmptyTitle}</p>
            <p class="muted small">{t.credits.packsEmptyHint}</p>
            <button class="btn ghost sm" onclick={openAdd}>{t.nav.addAction}</button>
          </div>
        {:else}
          {#each creditGroups as group (group.source)}
            <div class="pack-group">
              <div class="pack-group-head">
                <span class="src-dot" data-source={group.source}></span>
                <span>{sourceName(group.source)}</span>
                <span class="count">{t.credits.packs.accounts(group.items.length)}</span>
              </div>
              {#each group.items as row (row.key)}
                <button
                  class="pack-row"
                  class:open={openCredits[row.key]}
                  aria-expanded={!!openCredits[row.key]}
                  onclick={() => toggleCredits(row.key)}
                >
                  <Icon name="chevron" size={13} class="pack-chev" />
                  <span class="truncate" title={row.uid}>{row.label || row.uid}</span>
                  <span class="mono small">
                    {row.local || row.failed || row.unlimited || row.known === false
                      ? t.common.dash
                      : fmtCredits(row.remain)}
                  </span>
                  <span class="muted small">
                    {t.credits.packs.count((row.packs ?? []).length)}
                  </span>
                </button>
                {#if openCredits[row.key]}
                  {#if (row.packs ?? []).length}
                    <div class="table packs-table">
                      <div class="row head" style="--cols: var(--cols-packs)">
                        <span></span>
                        <span>{t.credits.packs.col.name}</span>
                        <span>{t.credits.packs.col.size}</span>
                        <span>{t.credits.packs.col.remain}</span>
                        <span>{t.credits.packs.col.used}</span>
                        <span>{t.credits.packs.col.created}</span>
                        <span>{t.credits.packs.col.expires}</span>
                      </div>
                      {#each packDetail(row) as item, i (i)}
                        {#if item.kind === 'pack'}
                          <div class="row" style="--cols: var(--cols-packs)">
                            <span class="pack-mark" data-source={row.source}></span>
                            <span class="small truncate" title={item.pack.name}>
                              {item.pack.name || t.credits.unnamedPack}
                            </span>
                            <span class="mono small">{item.pack.size ? fmtCredits(item.pack.size) : t.common.dash}</span>
                            <span class="mono small">{fmtCredits(item.pack.remain)}</span>
                            <span class="muted small mono">{fmtCredits(item.pack.used)}</span>
                            <span class="muted small mono">{fmtGrant(item.pack.created_at)}</span>
                            <span class="muted small mono">{packExpiryText(item.pack.expires_at)}</span>
                          </div>
                        {:else}
                          <button
                            class="pack-fold"
                            aria-expanded={item.open}
                            onclick={() => togglePackFold(row.key + ':' + item.fold)}
                          >
                            <Icon
                              name="chevron"
                              size={12}
                              class={item.open ? 'fold-chev open' : 'fold-chev'}
                            />
                            <span>
                              {item.open
                                ? t.credits.packs.foldClose
                                : item.fold === 'rest'
                                  ? t.credits.packs.foldRest(item.n, fmtCredits(item.size), fmtCredits(item.remain))
                                  : t.credits.packs.foldUsed(item.n, fmtCredits(item.size))}
                            </span>
                          </button>
                        {/if}
                      {/each}
                    </div>
                  {:else}
                    <p class="muted small pack-note">
                      {row.failed ? row.error : creditsLoading ? t.credits.probing : t.credits.packs.empty}
                    </p>
                  {/if}
                {/if}
              {/each}
            </div>
          {/each}
        {/if}
      </section>
      {/if}
    {/if}

    {#if tab === 'settings'}
      <Settings />
    {/if}

    {#if showAdd}
      <button class="modal-backdrop" type="button" aria-label={t.common.close} onclick={closeAdd}></button>
      <div
        class="modal"
        use:trapFocus
        role="dialog"
        aria-modal="true"
        aria-label={t.nav.addAction}
        tabindex="-1"
        onkeydown={onModalKeydown}
      >
        <!-- The tab strip leads: it is the dialog's one navigation, and the
             title under it names whichever way in is open. Switching tabs
             swaps the content and nothing else. -->
        <div class="modal-tabs segmented">
          <button
            class:active={addTab === 'device'}
            data-autofocus
            onclick={() => (addTab = 'device')}
          >
            {t.add.tab.device}
          </button>
          <button class:active={addTab === 'token'} onclick={() => (addTab = 'token')}>
            {t.add.tab.token}
          </button>
          <button class:active={addTab === 'batch'} onclick={() => (addTab = 'batch')}>
            {t.add.tab.batch}
          </button>
        </div>

        <div class="modal-intro">
          <h2>{addIntro.title}</h2>
          <p>{addIntro.body}</p>
        </div>

        <!-- Only the body scrolls. A dialog that scrolls as a whole lets the
             tab strip and the action leave the viewport together, which is how
             a long import ends with its own button off-screen. -->
        <div class="modal-body">
          {#if addTab === 'device'}
            {#if authPhase === 'idle' || authPhase === 'error'}
              <label class="field">
                <span class="field-label">{t.add.device.source}</span>
                <select bind:value={authSource}>
                  {#each deviceSources as s}<option value={s}>{t.add.sourceLabel[s] ?? s}</option>{/each}
                </select>
              </label>
              <label class="field">
                <span class="field-label">{t.add.device.realm}</span>
                <select bind:value={authRealm}>
                  <option value="cn">{t.add.device.realmCn}</option>
                  <option value="global">{t.add.device.realmGlobal}</option>
                </select>
              </label>
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
                  <p class="muted small mono url">{authUrl}</p>
                {/if}
              </div>
            {/if}

            {#if authPhase === 'done'}
              <div class="auth-wait">
                <p class="msg ok">{authMsg}</p>
              </div>
            {/if}
          {:else if addTab === 'token'}
            <label class="field">
              <span class="field-label">{t.add.token.source}</span>
              <select bind:value={tokenSource}>
                {#each manualSources as s}<option value={s}>{t.add.sourceLabel[s] ?? s}</option>{/each}
              </select>
            </label>
            <label class="field">
              <span class="field-label">{modeHint ? t.add.token.labelWithHint(modeHint) : t.add.token.label}</span>
              <textarea rows="3" bind:value={tokenValue} placeholder={tokenPlaceholder}></textarea>
            </label>

            <!-- The three optional fields fold away behind one row that names
                 them: the common paste is a single textarea, and the extras
                 are there when a client exported more than the access token. -->
            <button class="opt-fold" aria-expanded={optOpen} onclick={() => (optOpen = !optOpen)}>
              <Icon name="chevronRight" size={13} class={optOpen ? 'fold-chev open' : 'fold-chev'} />
              <span class="opt-label">{t.add.token.optional.label(3)}</span>
              <span class="opt-items">{t.add.token.optional.items}</span>
            </button>
            {#if optOpen}
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
            {/if}
            {#if tokenMsg}
              <p class="msg {tokenPhase === 'error' ? 'error' : 'ok'}">{tokenMsg}</p>
            {/if}

            {#if localSources.length}
              <h3 class="sub">{t.add.local.title}</h3>
              <p class="muted small">{t.add.local.body}</p>
              {#if localSources.length > 1}
                <label class="field">
                  <span class="field-label">{t.add.local.source}</span>
                  <select bind:value={localSource}>
                    {#each localSources as s}<option value={s}>{s}</option>{/each}
                  </select>
                </label>
              {/if}
              <button class="btn ghost" onclick={importLocal} disabled={localPhase === 'running'}>
                {#if localPhase === 'running'}<span class="spinner"></span>{/if}
                {localPhase === 'running' ? t.add.local.running : t.add.local.button}
              </button>
              {#if localMsg}
                <p class="msg {localPhase === 'error' ? 'error' : 'ok'}">{localMsg}</p>
              {/if}
            {/if}

          {:else}
            <!-- Pasting the export is the only import shape left. Reading a file
                 and opening an encrypted bundle both needed a path on this
                 machine, which is what the keeper already owns at startup. -->
            <label class="field">
              <span class="field-label">{t.importAcc.inlineLabel}</span>
              <textarea class="mono" rows="6" bind:value={importText} placeholder={inlinePlaceholder}></textarea>
            </label>

            {#if importMsg}
              <p class="msg {importPhase === 'error' ? 'error' : 'ok'}">{importMsg}</p>
            {/if}
            {#if importResult && importResult.errors?.length}
              <ul class="skips">
                {#each importResult.errors as e}<li>{e}</li>{/each}
              </ul>
            {/if}
          {/if}
        </div>

        <!-- The action lives here, not at the end of the form: every tab has one
             thing you came to do, and it must not be something you scroll to
             find. Which one it is depends on the tab and on how far the device
             flow has got. -->
        <div class="modal-foot">
          {#each addActions as act (act.label)}
            {#if act.href}
              <a class="btn ghost" href={act.href} target="_blank" rel="noreferrer">
                {#if act.icon}<Icon name={act.icon} size={15} />{/if}
                {act.label}
              </a>
            {:else}
              <button
                class="btn {act.kind}"
                disabled={act.busy}
                onclick={act.run}
              >
                {#if act.busy}<span class="spinner"></span>{/if}
                {act.label}
              </button>
            {/if}
          {/each}
        </div>
      </div>
    {/if}
  </main>

  <!-- The foot of every page: one constant, plus the one caveat that page
       must not be misread about. Outside <main> so it is not part of the
       scrolling content and not part of the page's measure. -->
  <footer class="page-foot">
    <span class="foot-dot" class:bad={!session} aria-hidden="true"></span>
    <span class="foot-note">{footNote}</span>
    <span class="grow"></span>
    <span class="foot-brand">{t.foot.brand}</span>
    {#if session?.version}
      <!-- The build lives in the foot, not in the health band: "which build is
           answering" is a once-a-look fact, and the band's identity row reads
           better without a dev tag parked in it. -->
      <span class="foot-version mono">{session.version}</span>
    {/if}
  </footer>
  </div>
</div>

{#if showLogs}
  <!-- The veil: the drawer is non-modal, but a dimmed page behind it says
       "the stream is the subject" without blocking the clicks that would
       matter if the drawer were gone. One click on it closes the drawer. -->
  <button
    class="log-veil"
    type="button"
    aria-label={t.common.close}
    onclick={closeLogs}
  ></button>
  <!-- Non-modal on purpose: the log is read while a sweep runs, so it may not
       block the page behind it. Focus lands on the panel heading and returns to
       the trigger on close; Tab stays free because the page is still usable. -->
  <div
    class="log-drawer"
    use:focusInto
    tabindex="-1"
    role="dialog"
    aria-modal="false"
    aria-labelledby="log-title"
    onkeydown={onLogKeydown}
  >
    <div class="log-head">
      <h2 id="log-title" tabindex="-1">{t.nav.logsAction}</h2>
      <!-- Connected is the default and needs no sentence; only the loss of it
           earns a bar. The dot is the quiet confirmation. -->
      <span
        class="log-live"
        class:on={logConnected}
        title={logConnected ? t.logStream.live : t.logStream.offline}
      >
        <span class="src-dot" aria-hidden="true"></span>
      </span>
      <span class="log-n">{t.logStream.lines(visibleLogs.length)}</span>
      <span class="spacer"></span>
      <button
        class="btn quiet sm"
        aria-pressed={logPinned}
        title={t.logStream.pinHint}
        onclick={toggleLogFollow}
      >
        <Icon name={logPinned ? 'pause' : 'play'} size={15} />
        {logPinned ? t.logStream.pinOn : t.logStream.pinOff}
      </button>
      <button
        class="btn icon"
        onclick={downloadLogs}
        aria-label={t.logStream.download}
        title={t.logStream.downloadHint}
      >
        <Icon name="importAcc" size={16} />
      </button>
      <button class="btn icon" aria-label={t.common.close} onclick={closeLogs}>
        <Icon name="close" size={16} />
      </button>
    </div>

    {#if !logConnected}
      <div class="log-lost" role="status">
        <Icon name="alert" size={15} />
        <span class="spacer">{t.logStream.offline}</span>
        <button class="btn ghost sm" onclick={retryLogs} title={t.logStream.retryHint}>
          {t.logStream.reconnect}
        </button>
      </div>
    {/if}

    <!-- Two questions, one row: who is talking, and does it need me. Follow and
         download live in the head because they are not filters — they change
         how the stream behaves, not what it shows. -->
    <div class="log-bar">
      <div class="segmented" role="group" aria-label={t.logStream.ch.all}>
        {#each LOG_CHANNELS as ch (ch.id)}
          <button
            class:active={logChannel === ch.id}
            aria-pressed={logChannel === ch.id}
            onclick={() => setLogChannel(ch.id)}
          >
            {ch.label()}
            <span class="chip-n">{logCounts[ch.id] ?? 0}</span>
          </button>
        {/each}
      </div>
      <div class="segmented" role="group" aria-label={t.logStream.level.aria}>
        {#each LOG_LEVELS as lv (lv.id)}
          <button
            class:active={logLevel === lv.id}
            aria-pressed={logLevel === lv.id}
            onclick={() => setLogLevel(lv.id)}
          >
            {lv.label()}
          </button>
        {/each}
      </div>
      <span class="spacer"></span>
      <div class="log-search">
        <Icon name="search" size={15} />
        <input
          type="search"
          value={logQuery}
          placeholder={t.logStream.search.placeholder}
          aria-label={t.logStream.search.aria}
          oninput={(e) => {
            logQuery = e.currentTarget.value
            rebuildLogs()
          }}
        />
        {#if logQuery}
          <button
            class="btn icon"
            aria-label={t.logStream.search.clear}
            onclick={() => {
              logQuery = ''
              rebuildLogs()
            }}
          >
            <Icon name="close" size={13} />
          </button>
        {/if}
      </div>
    </div>

    <!-- A live region that announces every line interrupts whatever the
         operator is reading, so it stays off; only the pause/resume summary is
         spoken, through this status line. -->
    <div class="sr-only" role="status" aria-live="polite">{logAnnounce}</div>

    <div
      class="log-stream"
      role="log"
      aria-live="off"
      aria-label={t.nav.logsAction}
      bind:this={logEl}
      onscroll={onLogScroll}
    >
      {#if visibleLogs.length}
        <div
          class="log-sizer"
          style="height:{visibleLogs.length * 26}px"
          aria-hidden="true"
        ></div>
        <div class="log-window" style="transform: translateY({logWindow.start * 26}px)">
          {#each logWindow.rows as rec (rec.seq)}
            <button
              type="button"
              class="ln"
              class:e={rec.level === 'error'}
              class:w={rec.level === 'warn'}
              class:sel={logSelected?.seq === rec.seq}
              title={rec.text}
              onclick={() => (logSelected = rec)}
            >
              <span class="lt">{fmtClock(rec.ts)}</span>
              {#if logChannel === 'all'}
                <span class="lch" data-ch={rec.channel}>
                  {LOG_CH_LABEL[rec.channel] ? LOG_CH_LABEL[rec.channel]() : rec.channel}
                </span>
              {/if}
              <span class="lx">{rec.text}</span>
            </button>
          {/each}
        </div>
      {:else}
        <p class="muted small log-empty">{t.logStream.empty}</p>
      {/if}
    </div>

    {#if logSelected}
      <div class="log-inspect">
        <div class="log-inspect-head">
          <span class="lt">{fmtClock(logSelected.ts)}</span>
          <span class="lch" data-ch={logSelected.channel}>
            {LOG_CH_LABEL[logSelected.channel]
              ? LOG_CH_LABEL[logSelected.channel]()
              : logSelected.channel}
          </span>
          <span class="spacer"></span>
          <button
            class="btn icon"
            aria-label={t.logStream.inspectClear}
            onclick={() => (logSelected = null)}
          >
            <Icon name="close" size={15} />
          </button>
        </div>
        <pre class="log-inspect-body">{logSelected.text}</pre>
      </div>
    {:else}
      <p class="log-hint small muted">{t.logStream.inspectHint}</p>
    {/if}

    {#if !logPinned}
      <button class="log-jump" onclick={jumpToLogTail}>
        <Icon name="chevron" size={15} />
        {t.logStream.jump}
      {#if logUnseen}<span class="log-unseen">{logUnseen}</span>{/if}
    </button>
  {/if}
  </div>
{/if}

<ToastHost />
<ConfirmHost />

<style>
  /* Two columns: a full-height rail carrying the brand and the destinations,
     and the working column beside it. The working column owns its own sticky
     bar, so the rail stays put while the page scrolls underneath.

     The 220px is decided by the table, not by taste: the account ledger needs
     864px, so below 1100 the rail gives up its width to the data. */
  .shell {
    display: grid;
    grid-template-columns: var(--rail-w) minmax(0, 1fr);
    min-height: 100vh;
  }

  .main {
    min-width: 0;
    display: flex;
    flex-direction: column;
  }

  /* ── top bar ───────────────────────────────────────────────────────────── */

  /* Transparent at rest so the page looks like one surface; it becomes a bar
     only once content is sliding under it. */
  .topbar {
    position: sticky;
    top: 0;
    z-index: var(--z-sticky-bar);
    display: flex;
    align-items: center;
    gap: var(--sp-4);
    height: var(--topbar-h);
    padding: 0 var(--page-pad-x);
    background: var(--topbar-bg);
    backdrop-filter: var(--topbar-blur);
    -webkit-backdrop-filter: var(--topbar-blur);
    border-bottom: 1px solid transparent;
    transition: background var(--dur-3) var(--ease), border-color var(--dur-3) var(--ease);
  }

  .topbar.scrolled {
    background: var(--topbar-bg-scrolled);
    border-bottom-color: var(--border);
  }

  .view-title {
    font-size: var(--fs-16);
    line-height: var(--lh-16);
    font-weight: var(--fw-semibold);
    letter-spacing: var(--track-tight);
    white-space: nowrap;
  }

  .topbar .sep {
    color: var(--text-2);
    font-size: var(--fs-12);
  }

  /* The meta slot states what the numbers below cover — a window, a count, or
     when the upstream was last read. Monospace so a changing value does not
     shift the row. */
  .topbar .meta {
    font-family: var(--font-mono);
    font-size: var(--fs-12);
    color: var(--text-2);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .topbar .grow {
    flex: 1;
    min-width: var(--sp-3);
  }

  /* ── rail ──────────────────────────────────────────────────────────────── */

  /* The brand block is exactly as tall as the top bar, and no rule divides it
     from the destinations: the rail is one surface, and a line here cut it into
     two panels that read as separate menus. */
  .brand {
    display: flex;
    align-items: center;
    gap: var(--sp-4);
    height: var(--topbar-h);
    flex: none;
    min-width: 0;
    padding: 0 var(--sp-6);
  }

  .brand-lines {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }

  /* The mark is the shipped artwork at 26px, rounded to the control radius so
     it sits on the same geometry as the buttons beside it. It is the only
     place the product's own image appears before the destinations, and it
     tells you which product this rail belongs to without a headline. */
  .logo {
    display: block;
    width: 26px;
    height: 26px;
    flex: none;
    border-radius: var(--r-sm);
  }

  .brand-name {
    font-size: var(--fs-14);
    line-height: var(--lh-14);
    font-weight: var(--fw-semibold);
    letter-spacing: var(--track-tight);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  /* Caps at 7.5px: a footnote under the name, not a second headline. The
     tracking is what keeps it legible at that size. */
  .brand-tagline {
    font-family: var(--font-mono);
    font-size: 7.5px;
    line-height: 9px;
    letter-spacing: 0.09em;
    color: var(--text-3);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .theme-switch {
    flex: none;
  }

  /* Three square icon buttons. The segmented container is 32px tall so it
     matches every other control in the bar; the buttons inside are 28px. */
  .theme-switch > button {
    width: 28px;
    height: 28px;
    padding: 0;
    justify-content: center;
  }

  .theme-opt .theme-label {
    display: none;
  }

  /* ── navigation rail ───────────────────────────────────────────────────── */

  .nav {
    display: flex;
    flex-direction: column;
    position: sticky;
    top: 0;
    align-self: start;
    height: 100vh;
    overflow-y: auto;
    background: var(--surface);
    border-right: 1px solid var(--border);
  }

  .nav ul {
    list-style: none;
    margin: 0;
    /* 16 across so the pill is 188 wide and its icon lands on the same 26px
       column as the mark above; 12 at the top is the gap the brand leaves. */
    padding: var(--sp-5) var(--sp-6) var(--sp-6);
    flex: 1;
  }

  .nav li + li {
    margin-top: 2px;
  }

  /* Settings is a different kind of destination from the five working
     surfaces, but the rail states that in the reading order, not with a rule:
     a hairline here cut one list into two menus and cost 16px of height. The
     item keeps the same 2px rhythm as its neighbours — dropping it made the
     active pill above butt straight into Settings, the one seam the eye
     catches while moving down the rail. */
  .nav li.split {
    margin-top: 2px;
    padding-top: 0;
    border-top: 0;
  }

  .nav a {
    position: relative;
    display: flex;
    align-items: center;
    gap: var(--sp-4);
    height: 34px;
    padding: 0 var(--sp-4);
    border-radius: var(--r-md);
    color: var(--text-2);
    font-size: var(--fs-13);
    line-height: var(--lh-13);
    font-weight: var(--fw-medium);
    text-decoration: none;
    white-space: nowrap;
    transition: background var(--dur-1) var(--ease), color var(--dur-1) var(--ease);
  }

@media (hover: hover) and (pointer: fine) {
    .nav a:hover {
      background: var(--surface-2);
      color: var(--text);
      text-decoration: none;
    }
  }

  /* Active is a tinted pill and brand ink, not an ink bar: the pill reads as
     "this is the surface you are on" from across the room, while a 2px bar at
     the very edge of a light rail disappears into the border. Orange here is
     the same mark as the logo — a place, not an action. */
  .nav a.on {
    background: var(--action-soft);
    color: var(--action-text);
  }

  /* The icon lives inside the Icon component, so it is not in this component's
     scope and has to be reached globally. */
  .nav a :global(svg) {
    flex: none;
    opacity: 0.85;
  }

  .nav a.on :global(svg) {
    opacity: 1;
  }

  /* ── rail pulse ────────────────────────────────────────────────────────── */

  /* One line at the foot of the rail: is the gateway answering, and on what
     port. Version and uptime are details, so they wait for a hover. */
  .rail-status {
    position: relative;
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    height: 42px;
    flex: none;
    padding: 0 var(--sp-6);
    border-top: 1px solid var(--border);
    color: var(--text-2);
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    white-space: nowrap;
  }

  .rail-dot {
    width: 6px;
    height: 6px;
    flex: none;
    border-radius: 50%;
    background: var(--ok);
  }

  .rail-status.bad .rail-dot {
    background: var(--danger);
  }

  .rail-status-text {
    overflow: hidden;
    text-overflow: ellipsis;
  }

@media (hover: hover) and (pointer: fine) {
    /* ── tooltip ───────────────────────────────────────────────────────────── */

    /* One CSS tooltip for the shell. Both targets sit at a fixed position, so a
       positioned pseudo-element is the right amount of machinery; a portal would
       buy nothing and cost a component.

       It is built only while hovered. An always-present, transparent pseudo-
       element still adds its width to the rail's scroll area, and a rail that can
       scroll sideways by a few pixels is worse than a tooltip with no fade-in. */
    [data-tip]:hover::after {
      content: attr(data-tip);
      position: absolute;
      z-index: var(--z-menu);
      padding: 3px var(--sp-3);
      border-radius: var(--r-sm);
      background: var(--text);
      color: var(--surface);
      font-size: var(--fs-11);
      line-height: var(--lh-11);
      font-weight: var(--fw-medium);
      white-space: nowrap;
      pointer-events: none;
      animation: tip-in var(--dur-1) var(--ease) 140ms both;
    }
  }

@media (hover: hover) and (pointer: fine) {
    [data-tip-side='right']:hover::after {
      left: calc(100% + var(--sp-2));
      top: 50%;
      transform: translateY(-50%);
    }
  }

@media (hover: hover) and (pointer: fine) {
    /* The pulse tooltip carries three facts, so it may wrap: a single line would
       be 215px wide and push the rail's scroll area by four pixels — enough to
       summon a horizontal scrollbar on hover. */
    [data-tip-side='up']:hover::after {
      left: var(--sp-3);
      bottom: calc(100% + var(--sp-2));
      max-width: 200px;
      white-space: normal;
    }
  }

  @keyframes tip-in {
    from {
      opacity: 0;
    }
    to {
      opacity: 1;
    }
  }

  /* ── page ──────────────────────────────────────────────────────────────── */

  /* The ledger's own header. A title on the left and the five sweeps on the
     right, both on the table's baseline: the actions sit on the thing they act
     on instead of floating in a toolbar of their own. 32px is a label row, not
     a card — it carries no fill and no border. */
  .ledger-head {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    min-height: 32px;
  }

  .ledger-title {
    margin: 0;
    font-size: var(--fs-13);
    line-height: var(--lh-13);
    font-weight: var(--fw-semibold);
    letter-spacing: var(--track-tight);
  }

  /* The ledger's body owns whatever height is left on the page, so the empty
     state and the diagnostic line land where the drawing puts them instead of
     leaving 300px of dead paper under a short table. */
  .overview-body {
    flex: 1 0 auto;
    display: flex;
    flex-direction: column;
    gap: var(--section-gap);
    min-height: 0;
  }

  .overview-body > .empty {
    flex: 1 1 auto;
    /* The body hands over the leftover height; the grid has to centre its
       tracks inside it, or each child stretches into a row of its own and the
       empty state reads as four elements spread top to bottom. */
    align-content: center;
  }

  .overview-body > .diag-block {
    flex: none;
  }

  /* ── page foot ─────────────────────────────────────────────────────────── */

  /* One line at the bottom of the window, outside the page measure. It is the
     only element that is the same on all six views: what this product is, and
     one caveat about the numbers above it. */
  .page-foot {
    display: flex;
    align-items: center;
    gap: 7px;
    height: 28px;
    flex: none;
    padding: 0 var(--page-pad-x);
    border-top: 1px solid var(--border);
    color: var(--text-3);
    font-size: var(--fs-10);
    line-height: 1;
    white-space: nowrap;
  }

  .foot-dot {
    width: 5px;
    height: 5px;
    flex: none;
    border-radius: 50%;
    background: var(--ok);
  }

  .foot-dot.bad {
    background: var(--danger);
  }

  .foot-note,
  .foot-brand {
    overflow: hidden;
    text-overflow: ellipsis;
  }

  /* The build tag sits beside the brand it qualifies, quiet enough to be
     furniture but present without a hover. */
  .foot-version {
    flex: none;
    padding-left: var(--sp-3);
    border-left: 1px solid var(--border);
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-3);
  }

  .page {
    flex: 1;
    width: 100%;
    max-width: var(--page-max);
    min-width: 0;
    margin: 0 auto;
    /* 20px top and bottom: the page breathes without pushing the first card
       below the fold. Left and right stay on --page-pad-x, which steps down at
       the 1439 / 1099 / 899 breakpoints. */
    padding: var(--sp-7) var(--page-pad-x);
  }

  /* Only the overview asks for the leftover height: its ledger should reach the
     diagnostic line, which in turn sits just above the foot. Opting in by
     class keeps the other five views stacking exactly as they did. */
  .page.overview {
    display: flex;
    flex-direction: column;
    /* v2 put the overview on the shared 16px rhythm: the band, the ledger head
       and the group cards breathe like every other page. The old 12px squeeze
       bought one more account row and cost the page its consistency. */
    min-height: calc(100vh - var(--topbar-h) - 28px);
  }

  /* One vertical rhythm for every page. Cards used to touch, which turned two
     hairlines into a single double line and made a stack read as one sheet. */
  .page > * + * {
    margin-top: var(--section-gap);
  }

  /* The skip target is a landing zone, not a control. A 2px ring around the
     whole page would read as an error state; the scroll position is the
     feedback that the jump happened. */
  .page:focus-visible {
    outline: none;
  }

  .lede {
    margin: 0;
    max-width: 52rem;
    font-size: var(--fs-13);
    color: var(--text-2);
    line-height: var(--leading-relaxed);
  }

  .lede-small {
    font-size: var(--fs-13);
    line-height: var(--leading-relaxed);
    max-width: 46rem;
  }

  .banner {
    margin-bottom: var(--sp-6);
  }

  /* ── groups ────────────────────────────────────────────────────────────── */

  /* One card per source: the frame carries the border and the 10px radius, the
     head is a 40px band of the card's own surface, and a single hairline
     separates it from the table — which then runs the full width of the card.
     The old stacked pair (a grey head card on a table card) drew two borders
     where the rules page draws one. The body's gap owns the 16px rhythm, so
     the card itself carries no margin.

     The group card is flush: `.card`'s 20px padding belongs to panels holding
     free-form content, and on a head + table pair it read as an unequal margin
     around the table — 20px of white on every side of a band the canvas draws
     edge to edge. The overview's `.group` never took `.card`, so only the
     model cards need the reset. */
  .card.group {
    padding: 0;
  }

  .group {
    border: 1px solid var(--border);
    border-radius: var(--r-md);
    background: var(--surface);
    overflow: hidden;
  }

  /* The head is the group's control, not a label: collapsing a source is how
     you get the other one back on screen. It sits flush on top of its table so
     the two read as one block. The inset is the canvas card-head's: 16 to the
     caret, 24 to the right-hand reading — a symmetric 10px made the head look
     like it belonged to the card's border rather than its content. */
  .group-head {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    width: 100%;
    height: 40px;
    padding: 0 24px 0 16px;
    border: 0;
    border-bottom: 1px solid transparent;
    background: none;
    color: var(--text);
    font: inherit;
    text-align: left;
    cursor: pointer;
    transition: background var(--dur-1) var(--ease);
  }

  /* The hairline only exists while there is a table under the head: a collapsed
     group is a 40px card, and a rule under nothing is a line that lies. The
     transparent default keeps the height identical in both states. */
  .group-head[aria-expanded='true'] {
    border-bottom-color: var(--border);
  }

@media (hover: hover) and (pointer: fine) {
    .group-head:hover {
      background: var(--surface-3);
    }
  }

  /* The table inside a group gives up its own frame: the card already drew it,
     and a second border 40px below the first reads as two cards. */
  .group .table {
    border: 0;
    border-radius: 0;
  }

  /* 30px and 11px (D53): the label row is a legend for the columns, one step
     lighter than the 44px rows it explains — the same rhythm the task centre
     uses. This covers the overview groups and the model cards alike, since
     both are `.card.group`; the vertical padding goes too: 4+4 of it would
     spend 8 of the 30px on air.

     The two heads do not share a label size: the model table's canvas frame
     draws 11px Regular, the overview's keeps the base 10px Medium. A blanket
     11px made the overview's head louder than its own drawing. */
  .group .table .row.head,
  .models-table .row.head {
    min-height: 30px;
    padding-block: 0;
    color: var(--text-3);
  }

  .models-table .row.head {
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    font-weight: var(--fw-regular);
  }

  /* The sort button's 36px floor would stretch a 30px head back to 44px.
     Inside a head the label itself is the hit area, so the floor drops to the
     row's own text measure — the arrow still never drifts from its word. */
  .group .table .row.head .th-btn,
  .models-table .row.head .th-btn {
    min-height: 22px;
  }

  /* The chevron lives inside the Icon component, so it is outside this
     component's style scope. */
  .group-head :global(.caret) {
    color: var(--text-2);
    transition: transform var(--dur-2) var(--ease);
  }

  .group-head :global(.caret.open) {
    transform: rotate(90deg);
  }

  .group-head .src-dot {
    width: 8px;
    height: 8px;
  }

  .group-name {
    font-size: var(--fs-13);
    font-weight: var(--fw-semibold);
    letter-spacing: var(--track-normal);
  }

  .group-count {
    font-family: var(--font-mono);
    font-size: var(--fs-11);
    font-weight: var(--fw-regular);
    font-variant-numeric: tabular-nums;
    color: var(--text-3);
  }

  /* The model card's count rides in the platform name's face: its canvas
     frame draws it sans (the overview group's count stays mono — that frame
     draws JetBrains Mono). */
  .card.group .group-count {
    font-family: inherit;
    font-variant-numeric: normal;
  }

  .group-credit {
    font-size: var(--fs-12);
    font-weight: var(--fw-semibold);
    color: var(--text-2);
  }

  /* ── layout helpers ────────────────────────────────────────────────────── */

  .column {
    display: flex;
    flex-direction: column;
    gap: var(--section-gap);
  }

  .section-title {
    margin: var(--sp-7) 0 var(--sp-2);
    font-size: var(--fs-16);
    font-weight: var(--fw-semibold);
  }

  /* ── table column sets ─────────────────────────────────────────────────── */

  .table {
    /* Nine columns: a 3px health stripe, the account, its state, credits,
       success/failure, in-flight, throughput, last success, then the actions.
       The platform column is gone — the group head names the source and every
       account row carries its 6px dot. */
    --cols-accounts: 3px minmax(8rem, 2.2fr) minmax(6.5rem, 1.1fr) minmax(3.5rem, 0.8fr)
      minmax(5rem, 0.9fr) 3rem minmax(5rem, 1fr) minmax(5rem, 0.9fr) 9rem;
    --cols-locks: minmax(0, 2fr) minmax(0, 1.2fr) minmax(8rem, 1fr);
    /* task 1fr / progress 9rem / status 6rem / reward 9rem / actions 3.5rem
       — 56px is what "领取 + 执行" needs (24 + 8 + 24); 80 was slack that read
       as an empty column. */
    --cols-tasks: minmax(0, 1fr) 9rem 6rem 9rem 3.5rem;
    --cols-daily: 6.5rem repeat(2, 4rem) 4rem repeat(4, minmax(4.5rem, 1fr));
    /* Seven columns: name, requests, failed, input, output, total, credits.
       Cache and average duration moved to the daily table and the detail
       rows — this table answers "who spent", and two more columns pushed the
       name column below readability. */
    --cols-breakdown: minmax(10rem, 1.6fr) 4.5rem 3.5rem repeat(3, minmax(4.5rem, 1fr)) 5.5rem;
    --cols-records: minmax(9rem, 1.1fr) 5rem minmax(6rem, 1fr) minmax(7rem, 1.1fr) 4.5rem
      minmax(6rem, 0.9fr) minmax(6.5rem, 1fr) 5.5rem;
    /* Six columns (v1.18): model, context, efforts, caps, max output, rate.
       One card per platform, so the platform column and the account column are
       gone — the card head names the source and counts its accounts. Efforts
       wraps instead of truncating, and the rate cell carries the discount. */
    --cols-models: minmax(0, 1fr) 56px 176px 196px 72px 150px;
    --cols-packs: 6px minmax(0, 1fr) 110px 110px 110px 150px 180px;
  }

  /* 1100-1439 is the laptop band, and it is exactly where the ledger runs out
     of width. In-flight is the one column worth dropping there: it is almost
     always 0 in a single-operator console, while the failure count beside it is
     the reason the table exists. Both tracks and cells go, or the remaining
     cells would slide one place to the left. */
  @media (max-width: 1439px) {
    .table {
      --cols-accounts: 3px minmax(8rem, 2.2fr) minmax(6.5rem, 1.1fr) minmax(3.5rem, 0.8fr)
        minmax(5rem, 0.9fr) minmax(5rem, 1fr) minmax(5rem, 0.9fr) 9rem;
    }
    .accounts-table .col-inflight {
      display: none;
    }
  }

  .table.scroll {
    overflow-x: auto;
  }

  /* One source for the horizontal minimum: a page may lower it per table
     (`.accounts-table` does on phones), and nothing else may hard-code it. */
  .table.scroll > .row {
    min-width: var(--table-min, 720px);
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

  .cell-status {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    flex-wrap: wrap;
  }

  .right.actions {
    display: flex;
    gap: var(--sp-5);
    justify-content: flex-end;
  }

  .task-skeleton {
    margin-top: var(--sp-5);
  }

  /* ── usage empty state ─────────────────────────────────────────────────── */

  /* Eight ghost bars hold the chart's outline so "no data" still reads as the
     canvas the user was promised, not as a missing feature. */
  .ghost-bars {
    display: flex;
    align-items: flex-end;
    gap: var(--sp-3);
    width: min(20rem, 100%);
    height: 72px;
    margin-bottom: var(--sp-4);
  }

  .ghost-bar {
    flex: 1 1 0;
    min-width: 6px;
    border-radius: var(--r-xs) var(--r-xs) 0 0;
    background: var(--surface-3);
  }

  .empty-actions {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    flex-wrap: wrap;
    justify-content: center;
    margin-top: var(--sp-2);
  }

  /* ── page skeletons ───────────────────────────────────────────────────── */

  /* Each outline is measured against the blocks it stands in for — the 72px
     metric head, the 200px chart, the 48px folded rows — so replacing it with
     the real thing moves nothing. A shared three-bar placeholder would be a
     page-height jump on arrival and another one on settle. */
  .skel {
    border-radius: var(--r-md);
    background: var(--surface-2);
  }

  .skel-line {
    display: block;
    height: 12px;
    border-radius: var(--r-sm);
  }

  .usage-skeleton {
    display: grid;
    gap: var(--section-gap);
  }

  .metric-head-skel {
    height: 72px;
  }

  .skel-chart {
    height: 200px;
    margin-top: var(--sp-5);
  }

  .skel-fold {
    height: 48px;
  }

  /* ── credits page ──────────────────────────────────────────────────────── */

  /* One 88px band: two platform totals that are never summed, the pack-level
     figure that says how much lapses this week, and the refresh that re-reads
     all of it. */
  .metric-band {
    display: flex;
    align-items: center;
    gap: var(--sp-8);
    min-height: 88px;
    padding: 0 var(--sp-6);
  }

  .metric-cell {
    display: flex;
    flex-direction: column;
    gap: 4px;
    min-width: 0;
  }

  .metric-cell .v {
    font-size: var(--fs-22);
    line-height: var(--lh-22);
    font-weight: var(--fw-semibold);
    font-variant-numeric: tabular-nums;
    color: var(--text);
  }

  .metric-cell .k {
    display: flex;
    align-items: center;
    gap: 5px;
    font-size: var(--fs-12);
    line-height: var(--lh-12);
    color: var(--text-3);
  }

  /* The divider separates "what I have" from "what is about to go". */
  .metric-vrule {
    width: 1px;
    height: 40px;
    flex: none;
    background: var(--border);
  }

  .metric-hint {
    font-size: var(--fs-11);
    color: var(--text-3);
  }

  .src-dot {
    width: 5px;
    height: 5px;
    border-radius: 999px;
    flex: none;
  }

  /* The card keeps its own 13px head scale: these are section titles on a page
     whose h2s are 16px, and a 16px head over a 12px table reads as two pages. */
  .credits-packs .card-head h2 {
    font-size: var(--fs-13);
    line-height: var(--lh-13);
  }


  .skel-block {
    border-radius: var(--r-md);
  }

  .credits-skeleton {
    display: grid;
    gap: var(--section-gap);
  }

  .skel-waterfall {
    height: 200px;
    margin-top: var(--sp-5);
  }

  .skel-cards {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: var(--sp-5);
  }

  .skel-card {
    height: 128px;
  }

  .table .row.skeleton-row {
    --cols: repeat(3, 1fr);
    padding: var(--sp-5) var(--sp-6);
  }

  .table .row.skeleton-row .skeleton {
    height: 12px;
  }

  /* ── task list ─────────────────────────────────────────────────────────── */

  /* One list, not one card per account: the rows are 48px bands in a single
     bordered block, so the eye can run down the names instead of re-finding
     the card edges each time. */
  .task-list {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }

  /* The roster card owns the frame, so the list inside it drops its own: two
     outlines around the same rows was the last nested box on this page. */
  /* 20px of top padding puts the title at y21, the offset the credits page's
     积分到期 card uses. The 44px min-height stays so the roster row keeps its
     y64 — dropping it would pull every task row up 6px for no visible gain. */
  .task-card {
    display: flex;
    flex-direction: column;
    padding: var(--card-pad) var(--card-pad) var(--sp-5);
  }

  .task-card > .card-head {
    align-items: flex-start;
    min-height: 44px;
    margin-bottom: 0;
  }

  .task-card .empty.inline {
    min-height: 380px;
  }

  .task-acc + .task-acc {
    border-top: 1px solid var(--border);
  }

  /* 36px, the height of a ledger account row: chevron, source dot and name
     share the first track, then the health pill, then the queue count. The
     balance used to sit here too — a task row does not spend credits, so that
     figure competed with the one that matters. */
  /* No side padding: the row already sits inside the card's own 20px, and a
     second inset pushed its chevron 20px right of the table text below it. */
  .task-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) 7.5rem auto;
    align-items: center;
    gap: var(--sp-4);
    min-height: 36px;
    padding: 0 var(--card-pad) 0 0;
  }

  .task-ident {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    min-width: 0;
  }

  .task-ident .status {
    flex: none;
  }

  .task-name {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    min-width: 0;
    font-size: var(--fs-13);
    line-height: var(--lh-13);
    font-weight: var(--fw-semibold);
  }

  .open-btn {
    padding: 0;
    border: 0;
    background: none;
    color: var(--text);
    font: inherit;
    font-weight: var(--fw-semibold);
    text-align: left;
    cursor: pointer;
  }

  .open-btn :global(.caret) {
    color: var(--text-2);
    transition: transform var(--dur-2) var(--ease);
  }

  /* chevronRight points right when folded, down when open — the same pair the
     credits page uses for its account rows. */
  .open-btn :global(.caret.open) {
    transform: rotate(90deg);
  }

  /* The queue's two numbers, on the row they belong to. Colour is a status
     signal here: work waiting is amber, credit already collectable is green. */
  .q-counts {
    display: inline-flex;
    align-items: center;
    justify-content: flex-end;
    gap: var(--sp-3);
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    font-weight: var(--fw-medium);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }

  .q-pending {
    color: var(--warn);
  }

  .q-claim {
    color: var(--ok);
  }

  .q-counts.idle {
    color: var(--text-3);
    font-weight: var(--fw-regular);
  }

  .acc-ops {
    display: flex;
    align-items: center;
    gap: var(--sp-4);
    font-size: var(--fs-12);
  }

  .task-running {
    margin: 0;
    padding: 0 0 var(--sp-3);
    font-size: var(--fs-12);
    color: var(--text-2);
  }

  /* The detail belongs to the card, not to a well inside it: it keeps the
     roster's hairlines and reaches the card's edges, the way the batch table
     does on the credits page. */
  .task-detail {
    display: flex;
    flex-direction: column;
    align-items: stretch;
    gap: var(--sp-3);
    padding: 0 0 var(--sp-4);
  }

  /* Same rule as the batch table on the credits page: below 820px the table
     scrolls sideways instead of squeezing the task name into a stub. */
  .task-inner {
    --table-min: 820px;
    margin-inline: calc(-1 * var(--card-pad));
    border: 0;
    border-radius: 0;
    background: transparent;
    overflow-x: auto;
    overscroll-behavior-x: contain;
  }

  .task-inner .row {
    min-height: 32px;
    padding: 0 var(--card-pad);
    gap: var(--sp-4);
  }

  /* 30px and 11px: the label row is a legend for the columns, one step
     lighter than the 32px rows it explains. */
  .task-inner .row.head {
    min-height: 30px;
    font-size: var(--fs-11);
    line-height: var(--lh-11);
  }

  /* The action cell holds still while the rest of the table scrolls. No
     divider: only cells that actually carry a button get an opaque background,
     so the empty ones stay transparent and the column leaves no seam. */
  .task-inner .row > :last-child {
    position: sticky;
    right: 0;
  }

  .task-inner .row.head > :last-child {
    background: var(--surface-2);
  }

  .task-inner .row:not(.head) > :last-child:has(.btn) {
    background: var(--surface);
  }

  @media (hover: hover) and (pointer: fine) {
    .task-inner .row:not(.head):hover > :last-child:has(.btn) {
      background: var(--surface-2);
    }
  }

  /* One line per task: the code rides behind the title in the smallest type
     instead of taking a second line and doubling the row height. The extra
     `.row` is load-bearing — theme.css ships `.table .row .label` as a stacked
     two-line cell (0,3,0), so a two-class selector loses to it and the row
     silently stays 45px. */
  .task-inner .row .label {
    flex-direction: row;
    align-items: baseline;
    gap: var(--sp-3);
  }

  .task-inner .row .label em {
    flex: none;
    font-variant-numeric: tabular-nums;
  }

  .cell-progress {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
  }

  .progress-track {
    display: block;
    width: 60px;
    height: 3px;
    flex: none;
    border-radius: 999px;
    background: var(--surface-3);
    overflow: hidden;
  }

  .progress-track i {
    display: block;
    height: 100%;
    border-radius: 999px;
    background: var(--action);
  }

  .results {
    margin: 0;
    padding-left: 0;
    font-size: var(--fs-11);
    color: var(--text-2);
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
  }

  .results li.error {
    color: var(--danger);
  }

  .results li.done {
    color: var(--ok);
  }

  /* ── usage chart ───────────────────────────────────────────────────────── */

  /* ── stacked token chart ───────────────────────────────────────────────── */

  /* The chart is DOM, not SVG: each day is a flex column of three stacked
     segments whose heights are percentages of the window's peak. It scales
     with the container and stays selectable, and a segment is one element —
     which is all this chart ever needs. */
  /* 276 = a 52px head plus a 224px chart. The inset narrows to 18px so the
     plot clears the card border while the head stays on the page's margin. */
  .chart-card {
    --card-pad: 18px;
    /* The card's own vertical padding would push a 52 + 224 stack past the
       276px the drawing gives it, so the head and the plot sit flush. */
    padding: 0 var(--card-pad);
  }

  /* The placeholder chart keeps the card's height so the strip of credits and
     the two folds under it do not jump the moment the first request lands. */
  .chart-empty {
    min-height: 224px;
    padding: var(--sp-4) var(--sp-6);
    border: 0;
    border-radius: 0;
    background: none;
  }

  .chart-card .card-head {
    align-items: center;
    min-height: 52px;
    margin-bottom: 0;
  }

  .chart-card .card-head h2 {
    font-size: var(--fs-13);
    line-height: var(--lh-13);
  }

  .chart-area {
    position: relative;
    /* 224 + the 52px card head = the 276px chart card. */
    height: 224px;
    margin: var(--sp-5) 0 0;
    padding-bottom: 1.1rem;
  }

  .chart-area.expanded {
    height: 320px;
  }

  /* Four grid lines behind the bars. They are drawn before the columns so a
     segment always sits on top of the rule it crosses. */
  .chart-grid {
    position: absolute;
    inset: 0 0 1.1rem;
    pointer-events: none;
  }

  .grid-line {
    position: absolute;
    left: 0;
    right: 0;
    height: 1px;
    background: var(--border);
  }

  .stack-chart {
    display: flex;
    align-items: stretch;
    gap: 2px;
    height: 100%;
    position: relative;
  }

  .stack-col {
    flex: 1 1 0;
    min-width: 3px;
    display: flex;
    flex-direction: column;
    justify-content: flex-end;
    position: relative;
  }

  /* The crosshair's column: a hairline through the plot marks which day the
     readout describes without tinting the bar itself. */
  .stack-col.hovered::before {
    content: '';
    position: absolute;
    top: 0;
    bottom: 1.1rem;
    left: 50%;
    width: 1px;
    background: var(--text-3);
    opacity: 0.6;
    pointer-events: none;
  }

  .stack-bar {
    display: flex;
    flex-direction: column-reverse;
    justify-content: flex-start;
    height: 100%;
    border-radius: 3px 3px 0 0;
    overflow: hidden;
    transition: opacity var(--dur-1) var(--ease);
  }

@media (hover: hover) and (pointer: fine) {
    .stack-col:hover .stack-bar {
      opacity: 0.82;
    }
  }

  .seg {
    width: 100%;
    min-height: 0;
  }

  /* Reading order from the axis up: output, fresh input, cache hit. The three
     are a neutral ramp, not three tints of the action colour — orange means
     "this is a button", and a chart is not a button. */
  .band-output {
    background: var(--chart-3);
    color: var(--chart-3);
  }

  .band-fresh {
    background: var(--chart-2);
    color: var(--chart-2);
  }

  .band-cached {
    background: var(--chart-1);
    color: var(--chart-1);
  }

  /* An idle day is an axis line, not a spike. */
  .band-empty {
    height: 2px;
    background: var(--border);
    color: var(--border);
  }

  /* The credits metric draws one series, so the middle step of the ramp is
     the bar. Using the darkest step would put the single column in
     competition with the chart's own readout text. */
  .band-credit {
    background: var(--chart-2);
    color: var(--chart-2);
    border-radius: 3px 3px 0 0;
  }

  /* The hover readout pins the crossed day's figures to the plot's top-left
     rather than following the pointer: a moving tooltip would jitter over
     the very columns it is describing. */
  .chart-readout {
    position: absolute;
    top: var(--sp-2);
    left: var(--sp-2);
    display: flex;
    align-items: center;
    gap: var(--sp-4);
    padding: var(--sp-2) var(--sp-3);
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--r-sm);
    box-shadow: var(--shadow-2);
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-2);
    font-variant-numeric: tabular-nums;
    pointer-events: none;
    z-index: var(--z-menu);
    max-width: calc(100% - var(--sp-4));
    flex-wrap: wrap;
  }

  .chart-readout .readout-date {
    font-family: var(--font-mono);
    color: var(--text);
    font-weight: var(--fw-medium);
  }

  .chart-readout .readout-item {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-1);
  }

  .chart-readout .readout-total {
    color: var(--text);
    font-weight: var(--fw-semibold);
  }

  /* ── metric head ───────────────────────────────────────────────────────── */

  /* One headline in the unit the switch names, its comparison, then three
     qualifying counts. 72px: the number is 28px and the row must not read as
     a second card stack. */
  .metric-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--sp-6);
    min-height: 96px;
    padding: 0 var(--sp-6) 0 20px;
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--r-md);
  }

  .metric-main {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 2px;
    min-width: 0;
  }

  /* The label sits under the figure, not beside it: at 32px the number is the
     headline, and a label on the same baseline shrank it to a caption. */
  .metric-sub {
    display: flex;
    align-items: baseline;
    gap: var(--sp-2);
    min-width: 0;
    flex-wrap: wrap;
  }

  .metric-dot {
    color: var(--text-3);
  }

  .metric-value {
    font-size: 32px;
    line-height: 1.1;
    font-weight: var(--fw-semibold);
    font-variant-numeric: tabular-nums;
    font-family: var(--font-mono);
  }

  .metric-label {
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-2);
    font-weight: var(--fw-medium);
  }

  /* D13's rule lives here too: burning more is not an achievement, so the
     delta is direction and magnitude only — never green. */
  .metric-delta {
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-2);
    font-variant-numeric: tabular-nums;
  }

  .metric-delta-label {
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-3);
  }

  .metric-side {
    display: flex;
    align-items: center;
    gap: var(--sp-8);
    flex: none;
  }

  /* The usage strip runs a tighter figure than the credits band: 15px with an
     11px caption, because three of them share the headline row. It used to be
     called .metric-cell too — the same name the credits band took over — and
     being later in the file it silently won there, shrinking 22px figures to
     15px. Own name, own size. */
  .usage-metric {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .usage-metric .k {
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-2);
    font-weight: var(--fw-medium);
  }

  .usage-metric .v {
    font-size: 15px;
    font-weight: var(--fw-semibold);
    font-variant-numeric: tabular-nums;
    display: flex;
    align-items: baseline;
    gap: 1px;
  }

  /* ── credit strip ──────────────────────────────────────────────────────── */

  /* Five numbers in one 48px band. It sits directly under the chart because it
     qualifies the window's bill; giving each figure a card of its own would
     rank them beside the headline they explain. Cells are separated by rules
     rather than gaps so the five rates read as one instrument. */
  .credit-strip {
    display: grid;
    grid-template-columns: repeat(5, 1fr);
    align-items: center;
    min-height: 48px;
    padding: 0 var(--sp-7);
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--r-md);
    gap: 0;
  }

  .credit-cell {
    position: relative;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 2px;
    min-width: 0;
  }

  /* A 24px rule between cells, centred on the band — a full-height divider
     would collide with the card's own top and bottom border. */
  .credit-cell + .credit-cell::before {
    content: '';
    position: absolute;
    left: 0;
    top: 50%;
    transform: translateY(-50%);
    width: 1px;
    height: 24px;
    background: var(--border);
  }

  .credit-cell .k {
    font-size: var(--fs-10);
    line-height: var(--lh-10);
    color: var(--text-2);
    font-weight: var(--fw-medium);
  }

  .credit-cell .v {
    font-size: var(--fs-13);
    line-height: var(--lh-13);
    font-weight: var(--fw-semibold);
    font-variant-numeric: tabular-nums;
    font-family: var(--font-mono);
  }

  /* ── folds ─────────────────────────────────────────────────────────────── */

  /* A 44px row that owns its table. The chevron carries the affordance; the
     row states what is inside and how much of it there is. */
  .fold-row {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    width: 100%;
    min-height: 44px;
    padding: 0 var(--sp-7);
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--r-md);
    color: var(--text);
    font: inherit;
    text-align: left;
    cursor: pointer;
    transition: background var(--dur-1) var(--ease);
  }

@media (hover: hover) and (pointer: fine) {
    .fold-row:hover {
      background: var(--surface-2);
    }
  }

  .fold-title {
    font-size: var(--fs-12);
    line-height: var(--lh-12);
    font-weight: var(--fw-semibold);
  }

  .fold-row .hint {
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-3);
    font-variant-numeric: tabular-nums;
  }

  .fold-action {
    margin-left: auto;
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-2);
    font-weight: var(--fw-medium);
  }

  /* Folded points right, unfolded points down: the same grammar as the pack
     folds below. The base icon is the down chevron, so a closed row turns it
     a quarter turn left and an open one restores it. */
  .fold-row :global(svg.fold-chev) {
    transform: rotate(-90deg);
    transition: transform var(--dur-2) var(--ease);
    color: var(--text-3);
  }

  .fold-row :global(svg.fold-chev.open) {
    transform: rotate(0deg);
  }

  /* ── drill-down ────────────────────────────────────────────────────────── */

  /* 1.4:1 keeps the dimension table readable while the credit bars stay a
     companion. minmax(0, …) lets both tracks shrink below their content —
     without the zero floor the side card's segmented control forced the grid
     wider than the viewport at exactly 1100px. Below 1280 the pair stacks;
     the table keeps its columns. */
  .drill-grid {
    display: grid;
    grid-template-columns: minmax(0, 1.4fr) minmax(0, 1fr);
    gap: var(--sp-7);
    align-items: start;
  }

  /* 124 = a 46px card head plus a 78px body. Holding that floor keeps the two
     cards the same height while one of them is empty. */
  .drill-grid > .card {
    min-height: 124px;
  }

  .drill-grid .card-head h2 {
    font-size: var(--fs-13);
    line-height: var(--lh-13);
  }

  @media (max-width: 1279px) {
    /* minmax(0, …) and not 1fr: a bare fr track floors at min-content, so the
       wide table inside sets the width of the whole page below 1280 instead of
       scrolling inside its own card. */
    .drill-grid {
      grid-template-columns: minmax(0, 1fr);
    }
  }

  .drill-more {
    margin-top: var(--sp-3);
  }

  .spend-bars {
    list-style: none;
    margin: var(--sp-3) 0 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--sp-3);
  }

  .spend-row {
    display: grid;
    grid-template-columns: minmax(0, 7rem) minmax(0, 1fr) 4.5rem;
    align-items: center;
    gap: var(--sp-3);
  }

  .spend-label {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    min-width: 0;
    font-size: var(--fs-12);
    line-height: var(--lh-13);
    color: var(--text-2);
  }

  .spend-track {
    height: 8px;
    background: var(--surface-3);
    border-radius: var(--r-xs);
    overflow: hidden;
  }

  .spend-fill {
    display: block;
    height: 100%;
    background: var(--chart-2);
    border-radius: var(--r-xs);
  }

  .spend-num {
    text-align: right;
    font-size: var(--fs-12);
    font-family: var(--font-mono);
    font-variant-numeric: tabular-nums;
    color: var(--text);
  }

  /* ── toolbar tail / chips ──────────────────────────────────────────────── */

  /* v1.18: the toolbar moved out of the card to the page level. One card per
     platform means a toolbar inside either card would belong to one platform
     while it filters both. The page's own 12px rhythm spaces it from the
     cards, so it takes no card furniture of its own. */
  .models-toolbar {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--sp-3);
    min-height: var(--control-h);
  }

  .models-toolbar .search {
    width: 288px;
  }

  /* The canvas spaces the toolbar's right-hand cluster 16px apart — count,
     then button. The base tail's 8px gap plus the count's own 8px margin
     happened to total 16 for the count but left the button at 8; scoped to
     the models toolbar so the usage page's tail keeps its tighter rhythm. */
  .models-toolbar .toolbar-tail {
    gap: var(--sp-6);
  }

  /* The empty catalogue keeps a roomy budget: one sentence in a 300px panel
     says "nothing here yet", the same sentence in an 88px band says "this box
     failed to load". The per-platform filtered empty stays at 88px — it sits
     under a head that already explains the platform. */
  .models-empty .empty.inline {
    min-height: 300px;
    background: none;
  }

  /* The card head's right-hand reading: how many accounts carry this
     platform's catalogue. One step quieter than the count beside the name. */
  .group-meta {
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-3);
    white-space: nowrap;
  }

  .toolbar-tail {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    margin-left: auto;
  }

  .src-select {
    width: auto;
    min-width: 7rem;
    height: 28px;
    font-size: var(--fs-12);
  }

  .chip-row {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-1);
  }

  .rec-model {
    width: auto;
    min-width: 9rem;
    height: 28px;
    font-size: var(--fs-12);
  }

  /* ── models cells ──────────────────────────────────────────────────────── */

  /* The canvas row body carries 16px between columns — the base table's 12px
     was written for the account ledger, and the model table's six columns
     read as one blurred band without the wider gutter. Vertical padding is
     12px, not the base 4px: a wrapped efforts tier needs the air (the row
     grows 44 → 71 on five tiers), and an unwrapped row rides its 44px floor
     either way. The overview account table keeps the base measures. */
  .models-table .row {
    gap: var(--sp-6);
    padding-block: var(--sp-5);
  }

  /* Caps stay on one line: the full list moves to the title, and a row that
     grows to three badge lines turns the table into a ladder of steps. */
  .cell-caps {
    display: flex;
    align-items: center;
    gap: var(--sp-1);
    min-width: 0;
    overflow: hidden;
    white-space: nowrap;
  }

  .cell-caps :global(.badge) {
    flex: none;
  }

  /* Efforts wrap instead of truncating (v1.18): "high, medium, low" is the
     answer to "can I tune this model", and a clipped list answers nothing.
     The row grows to fit — 44 → 71 on five tiers. Six pixels between tiers
     in both directions: the canvas wrap frame uses one gap, not a tighter
     row gap than column gap. */
  .cell-efforts {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
    min-width: 0;
  }

  /* A label tag, not the 26px filter chip `.chip` (D46's lesson): these state
     a fact about the row — a tier, an offer — and take no input. */
  .tag {
    display: inline-flex;
    align-items: center;
    flex: none;
    padding: 2px var(--sp-2);
    border-radius: var(--r-xs);
    background: var(--surface-2);
    color: var(--text-3);
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    font-weight: var(--fw-semibold);
    white-space: nowrap;
  }

  /* The warm pair is its own token pair (spec §10.18.6): borrowing --warn
     would read as "something is wrong", and --info belongs to the caps. */
  .tag.warm {
    background: var(--warm-chip-bg);
    color: var(--warm-chip-fg);
  }

  /* "限时免费" is a good thing, so it takes the success hue; the factor==0
     offer is the one case a rate disappears entirely. */
  .tag.free {
    background: var(--ok-soft);
    color: var(--ok);
  }

  /* Every figure in the table is 12px mono on the canvas — the context, the
     max output and the rate alike. The base row is 13px sans, so the cells
     restate the measure instead of inheriting it (`.mono`'s 0.95em on an
     11px `.small` landed at 10.5px, a third smaller than the drawing). */
  .cell-num {
    font-family: var(--font-mono);
    font-size: var(--fs-12);
    line-height: var(--lh-13);
    color: var(--text-3);
    text-align: right;
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }

  /* The source prefix rides one step under the model name but at the same
     size: the canvas draws both at 12px mono, the prefix in the tertiary
     tone. The card head already names the platform; the prefix is what a
     caller pastes into a config. */
  .model-cell {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    min-width: 0;
  }

  .src-name {
    font-size: var(--fs-12);
    color: var(--text-3);
    flex: none;
  }

  .model-name {
    font-size: var(--fs-12);
    color: var(--text);
  }

  /* The rate cell tells the truth in three shapes: a discount in force
     (effective rate + label + the struck listed rate), an offer that states
     only a label (listed rate + label), or a plain rate. */
  .cell-rate {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: var(--sp-2);
    min-width: 0;
  }

  /* The effective rate is the row's loudest number: full-strength text at
     the same 12px mono as the figures beside it. */
  .rate-now {
    font-family: var(--font-mono);
    font-size: var(--fs-12);
    line-height: var(--lh-13);
    color: var(--text);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }

  .rate-was {
    font-size: var(--fs-11);
    color: var(--text-3);
    text-decoration: line-through;
  }

  /* ── credits page ──────────────────────────────────────────────────────── */

  .expiry-more {
    margin-top: var(--sp-3);
  }

  .credit-expiry {
    margin-left: var(--sp-2);
    font-variant-numeric: tabular-nums;
    color: var(--text-2);
  }

  .credit-expiry.soon {
    color: var(--warn);
    font-weight: var(--fw-medium);
  }

  .stack-label {
    position: absolute;
    bottom: -1.05rem;
    left: 0;
    right: 0;
    text-align: center;
    font-size: var(--fs-11);
    font-variant-numeric: tabular-nums;
    color: var(--text-2);
    overflow: hidden;
    white-space: nowrap;
  }

  .legend {
    display: flex;
    align-items: center;
    gap: var(--sp-5);
    flex-wrap: wrap;
  }

  .legend-item {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    font-size: var(--fs-11);
    color: var(--text-2);
  }

  .legend-dot {
    width: 8px;
    height: 8px;
    border-radius: 2px;
  }

  /* The stat strip's cache rate sits under the number it qualifies rather
     than in a cell of its own. */
  .stat-sub {
    font-size: var(--fs-11);
    color: var(--text-2);
    font-variant-numeric: tabular-nums;
  }

  /* The detail table scrolls inside a fixed height so "load more" has a place
     to happen: an unbounded table would push its own footer off the page. */
  .records-scroll {
    max-height: 32rem;
    overflow-y: auto;
  }

  .cell-token {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .cell-token em {
    font-style: normal;
    font-size: var(--fs-11);
    color: var(--text-2);
  }

  .filters {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    flex-wrap: wrap;
  }

  /* ── misc ──────────────────────────────────────────────────────────────── */

  .empty-mark {
    display: grid;
    place-items: center;
    width: 38px;
    height: 38px;
    border-radius: 999px;
    background: var(--action-soft);
    color: var(--action);
    margin-bottom: var(--sp-1);
  }

  .auth-wait {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: var(--sp-5);
    margin-top: var(--sp-6);
  }

  .url {
    max-width: 100%;
    overflow-wrap: anywhere;
  }

  .skips {
    margin: var(--sp-5) 0 0;
    padding-left: var(--sp-6);
    color: var(--warn);
    font-size: var(--fs-11);
  }

  .spinner.inline {
    display: inline-block;
    vertical-align: -2px;
    margin-right: var(--sp-1);
  }

  /* ── overview ──────────────────────────────────────────────────────────── */

  .ov-chips {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-3);
  }

  .ov-chip {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-3);
    padding: var(--sp-2) var(--sp-5);
    border: 1px solid var(--border);
    border-radius: var(--r-md);
    background: var(--surface-2);
    cursor: pointer;
    max-width: 100%;
    transition: border-color var(--dur-1) var(--ease), background var(--dur-1) var(--ease);
  }

@media (hover: hover) and (pointer: fine) {
    .ov-chip:hover {
      border-color: var(--action);
      background: var(--action-soft);
    }
  }

  .ov-chip-name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: var(--fs-12);
  }

  .ov-rows {
    display: flex;
    flex-direction: column;
  }

  .ov-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--sp-5);
    padding: var(--sp-4) 0;
    border-bottom: 1px solid var(--border);
  }

  .ov-row:last-child {
    border-bottom: 0;
  }

  .kv {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: var(--sp-3) var(--sp-7);
    align-items: center;
  }

  .kv .k {
    color: var(--text-2);
    font-size: var(--fs-12);
  }

  .kv .v {
    font-size: var(--fs-12);
  }

  .warn-text {
    color: var(--warn);
  }

  .bad-text {
    color: var(--danger);
  }


  /* ── credits detail ─────────────────────────────────────────────────────
     Three views of one quantity: when it lapses (pool axis), who holds it
     (cards), and what it is made of (per-pack rows). */

  .btn.xs {
    height: 26px;
    padding: 0 var(--sp-3);
    font-size: var(--fs-11);
    gap: var(--sp-1);
  }

  /* The legend states the two sources once for the whole chart: colour is the
     platform, lightness is how far away a batch lapses. */
  .credit-legend {
    display: flex;
    align-items: center;
    gap: var(--sp-5);
    flex-wrap: wrap;
  }

  .legend-item {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    font-size: var(--fs-12);
    color: var(--text-3);
  }

  /* One row per remaining-day bucket on a shared scale. Both sources share the
     row, so a day when the two run out together reads without cross-checking,
     and a row per day beats a calendar: the question is "how soon", not
     "which date". */
  .expiry-rows {
    display: flex;
    flex-direction: column;
    gap: 10px;
    margin-top: var(--sp-6);
  }

  .expiry-row {
    display: grid;
    grid-template-columns: 3.5rem minmax(0, 1fr) 5.5rem;
    align-items: center;
    gap: var(--sp-4);
  }

  .expiry-row .when {
    font-size: var(--fs-12);
    font-variant-numeric: tabular-nums;
    color: var(--text-3);
  }

  .expiry-track {
    display: flex;
    height: 10px;
    border-radius: var(--r-pill);
    background: var(--surface-2);
    overflow: hidden;
  }

  /* Two scales in one row: the fill's width says how this bucket compares to
     the nearest one, the segments inside say how the bucket splits between
     platforms. Writing both onto the segments made the aggregate row paint one
     source over the other. */
  .expiry-fill {
    display: flex;
    height: 100%;
    min-width: 2px;
  }

  .expiry-seg {
    height: 100%;
    min-width: 2px;
  }

  .expiry-seg[data-source='workbuddy'],
  .mixbar i[data-source='workbuddy'],
  .pack-mark[data-source='workbuddy'],
  .swatch[data-source='workbuddy'] {
    background: var(--src-wb);
  }

  .expiry-seg[data-source='trae-cn'],
  .expiry-seg[data-source='trae'],
  .mixbar i[data-source='trae-cn'],
  .mixbar i[data-source='trae'],
  .pack-mark[data-source='trae-cn'],
  .pack-mark[data-source='trae'],
  .swatch[data-source='trae-cn'],
  .swatch[data-source='trae'] {
    background: var(--src-trae);
  }

  .expiry-seg[data-source='unknown'],
  .mixbar i[data-source='unknown'],
  .pack-mark[data-source='unknown'],
  .swatch[data-source='unknown'] {
    background: var(--text-2);
  }

  .expiry-row .sum {
    text-align: right;
    font-size: var(--fs-13);
    font-variant-numeric: tabular-nums;
    color: var(--text);
  }

  .expiry-foot {
    margin-top: var(--sp-4);
    font-size: var(--fs-11);
    color: var(--text-3);
  }

  /* As many columns as the width can carry. 340px is where the legend's
     package name stops being readable, so the grid asks for that and takes
     whatever else is free. auto-fill, not auto-fit: an unused track keeps its
     width, so a pool of two still shows two 380px cards in a three-column grid
     instead of two half-page slabs. */
  .credit-cards {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(340px, 1fr));
    gap: var(--section-gap);
  }

  /* 176px on the canvas: 14 head + 30 figure + 6 bar + 16 note + three 16px
     legend rows. The gaps are stated per block rather than once for the whole
     card, because the canvas tightens them under the head and opens them above
     the bar — one uniform gap reads as a stack of equal slabs. */
  .credit-card {
    display: flex;
    flex-direction: column;
    min-width: 0;
    padding: 14px 14px 6px;
    border: 1px solid var(--border);
    border-radius: var(--r-md);
    background: var(--surface);
  }

  .credit-card-head {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    min-width: 0;
    min-height: 18px;
    line-height: var(--lh-13);
  }

  .credit-card-head .who {
    font-size: var(--fs-13);
    font-weight: var(--fw-semibold);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .credit-card-head .count {
    margin-left: auto;
    flex: none;
    font-size: var(--fs-11);
    color: var(--text-3);
  }

  .credit-figure {
    display: flex;
    align-items: baseline;
    gap: var(--sp-3);
    margin-top: 6px;
    min-width: 0;
  }

  .credit-figure .v {
    font-size: var(--fs-22);
    line-height: var(--lh-22);
    font-weight: var(--fw-semibold);
    font-variant-numeric: tabular-nums;
    color: var(--text);
  }

  .credit-figure .total {
    font-size: var(--fs-11);
    color: var(--text-3);
  }

  /* The composition bar: same hue as the card's source, stepping down in
     opacity per package name. It answers "which packages make up this
     balance" without spending a second palette. */
  .mixbar {
    display: flex;
    height: 6px;
    margin-top: 10px;
    border-radius: var(--r-pill);
    background: var(--surface-2);
    overflow: hidden;
  }

  .mixbar i {
    height: 100%;
    min-width: 2px;
  }

  .credit-note {
    margin-top: 8px;
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-3);
  }

  /* A card that only carries a note and a retry still needs the button to sit
     apart from the text now that the card has no uniform gap. */
  .credit-card > .btn {
    align-self: flex-start;
    margin-top: var(--sp-3);
  }

  .src-legend {
    display: flex;
    flex-direction: column;
    gap: 6px;
    margin-top: auto;
    padding-top: 2px;
  }

  .src-legend-row {
    display: grid;
    grid-template-columns: 8px minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--sp-3);
    font-size: var(--fs-12);
    line-height: 16px;
  }

  /* The legend key: an 8px square that carries the source hue. Deliberately
     not `.chip` — that is the 26px outlined pill the usage page uses for
     filters, and borrowing the name stretched every key into an 18px bar. */
  .src-legend-row .swatch {
    width: 8px;
    height: 8px;
    border-radius: 2px;
    flex: none;
  }

  .src-legend-row .nm {
    color: var(--text-2);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .src-legend-row .meta {
    font-size: var(--fs-11);
    font-variant-numeric: tabular-nums;
    color: var(--text-3);
    white-space: nowrap;
  }

  /* ── pack detail ───────────────────────────────────────────────────────── */

  /* The batch list is separated from the card by full-width hairlines on the
     canvas, so the groups cancel the card's side padding and put it back
     inside each row instead of stopping short of the border. */
  .pack-group {
    margin-inline: calc(-1 * var(--card-pad));
  }

  .pack-group + .pack-group {
    margin-top: var(--sp-6);
  }

  .pack-group-head {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    min-height: 30px;
    padding: 0 var(--card-pad);
    border-bottom: 1px solid var(--border);
    font-size: var(--fs-12);
    font-weight: var(--fw-semibold);
    color: var(--text-2);
  }

  .pack-group-head .count {
    font-weight: var(--fw-regular);
    color: var(--text-3);
  }

  .pack-row {
    display: grid;
    grid-template-columns: 1rem minmax(0, 1fr) 6rem 7.5rem;
    align-items: center;
    gap: var(--sp-3);
    width: 100%;
    min-height: 36px;
    padding: 0 var(--card-pad);
    border: none;
    border-top: 1px solid var(--border);
    background: none;
    color: var(--text);
    font: inherit;
    font-size: var(--fs-13);
    text-align: left;
    cursor: pointer;
    transition: background var(--dur-1) var(--ease);
  }

@media (hover: hover) and (pointer: fine) {
    .pack-row:hover {
      background: var(--surface-2);
    }
  }

  .pack-row > :nth-child(3),
  .pack-row > :nth-child(4) {
    text-align: right;
    font-variant-numeric: tabular-nums;
  }

  /* The chevron lives inside the Icon component, so it is outside this
     component's style scope. */
  .pack-row :global(.pack-chev) {
    color: var(--text-2);
    transform: rotate(-90deg);
    transition: transform var(--dur-1) var(--ease);
  }

  .pack-row.open :global(.pack-chev) {
    transform: rotate(0deg);
  }

  /* A fold is a row too: it belongs inside the table's rhythm, not beside it. */
  .pack-fold {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    width: 100%;
    min-height: 30px;
    padding: 0 var(--card-pad);
    border: 0;
    border-top: 1px solid var(--border);
    background: none;
    color: var(--text-2);
    font: inherit;
    font-size: var(--fs-12);
    text-align: left;
    cursor: pointer;
    transition: color var(--dur-1) var(--ease), background var(--dur-1) var(--ease);
  }

@media (hover: hover) and (pointer: fine) {
    .pack-fold:hover {
      background: var(--surface-2);
      color: var(--text);
    }
  }

  .pack-fold :global(.fold-chev) {
    color: var(--text-3);
    transform: rotate(-90deg);
    transition: transform var(--dur-1) var(--ease);
  }

  .pack-fold :global(.fold-chev.open) {
    transform: rotate(0deg);
  }

  .pack-mark {
    width: 6px;
    height: 6px;
    border-radius: 999px;
    justify-self: start;
  }

  .pack-note {
    padding: var(--sp-3) var(--card-pad);
  }

  /* The batch table is not a card of its own: it sits inside the card that
     already draws the border, and its hairlines run edge to edge. Below
     1000px it scrolls sideways rather than squeezing the package name into a
     stub — the same rule the ledger follows. */
  .packs-table {
    --table-min: 1000px;
    border: 0;
    border-radius: 0;
    background: transparent;
    overflow-x: auto;
    overscroll-behavior-x: contain;
  }

  .packs-table .row {
    min-width: var(--table-min);
    min-height: 32px;
    padding: 0 var(--card-pad);
    gap: var(--sp-4);
  }

  /* 30px and 11px: the label row is a legend for the columns, one step
     lighter than the 32px rows it explains. */
  .packs-table .row.head {
    min-height: 30px;
    font-size: var(--fs-11);
    line-height: var(--lh-11);
  }

  .credits-packs .card-head {
    align-items: center;
    min-height: 44px;
  }

  /* ── pool maintenance row ──────────────────────────────────────────────── */

  /* The batch row moved into the ledger header, so the toolbar and its label
     are gone: a labelled strip above the table restated what the buttons
     already said. */
  .msg.inline {
    margin: 0;
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
  }

  /* ── usage window ──────────────────────────────────────────────────────── */

  .range-bar {
    justify-content: space-between;
    min-height: 48px;
    padding: 0 var(--sp-6);
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--r-md);
    /* §4.3: the window switch stays within reach while the chart scrolls
       under it — and scroll-padding-top accounts for its 48px on this page. */
    position: sticky;
    top: var(--topbar-h);
    z-index: var(--z-sticky-bar);
  }

  .range-pickers {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
  }

  .range-pickers input {
    font-size: var(--fs-13);
    padding: 5px 9px;
  }

  .range-arrow {
    color: var(--text-2);
    font-size: var(--fs-13);
  }

  /* ── queue progress ────────────────────────────────────────────────────── */

  /* Indeterminate on purpose: the sweep reports a summary when it finishes and
     nothing in between, so a percentage would be invented. */
  @keyframes queue-slide {
    from {
      transform: translateX(-100%);
    }
    to {
      transform: translateX(320%);
    }
  }

  /* ── responsive ────────────────────────────────────────────────────────── */

  /* ── breakpoints ───────────────────────────────────────────────────────── */

  /* Below 1100 the rail trades its labels for the table's width. The numbers
     are the ledger's: 1024 − 220 − 40 leaves 764px for a table that needs 864,
     while a 68px rail leaves 916 and the ten columns fit again. */
  @media (max-width: 1099px) {
    .shell {
      grid-template-columns: var(--rail-w-icon) minmax(0, 1fr);
    }

    /* Tooltips open outside the rail, so the rail must not clip them. Its
       content is shorter than any usable viewport, so nothing is lost. */
    .nav {
      overflow: visible;
    }

    .brand {
      justify-content: center;
      padding: 0;
    }

    /* The name leaves the layout but not the accessibility tree. */
    .brand-name,
    .nav-label,
    .rail-status-text {
      position: absolute;
      width: 1px;
      height: 1px;
      overflow: hidden;
      clip-path: inset(50%);
      white-space: nowrap;
    }

    .nav a {
      justify-content: center;
      padding: 0;
      height: 40px;
    }

    .nav a.on::before {
      left: var(--sp-1);
    }

    .rail-status {
      justify-content: center;
      padding: 0;
    }
  }

  @media (max-width: 1099px) {
    .metric-head {
      flex-wrap: wrap;
    }

    .metric-side {
      gap: var(--sp-6);
    }
  }

  @media (max-width: 899px) {
    .credit-strip {
      grid-template-columns: repeat(3, 1fr);
      gap: var(--sp-3);
    }

    /* The divider sits on a cell's left edge, which only reads as a separator
       while every cell shares one row. */
    .credit-cell + .credit-cell::before {
      display: none;
    }
  }

  @media (max-width: 759px) {
    .credit-strip {
      grid-template-columns: repeat(2, 1fr);
    }

    .metric-value {
      font-size: var(--fs-20);
      line-height: var(--lh-20);
    }

    .chart-readout {
      left: var(--sp-1);
      gap: var(--sp-2);
    }
  }

  /* Labels are only absent below 1100, so the tooltip is only drawn there. */
  @media (min-width: 1100px) {
    .nav a[data-tip]::after {
      display: none;
    }
  }

  @media (max-width: 899px) {
    .topbar {
      gap: var(--sp-3);
    }
    .topbar .meta,
    .topbar .sep {
      display: none;
    }
  }

  /* At phone width the destinations become one scrollable row above the working
     column, as the reference console does: a menu that must be opened costs a
     tap before you can even see where you could go. The brand steps aside —
     the active tab already says where you are. */
  @media (max-width: 759px) {
    .shell {
      grid-template-columns: minmax(0, 1fr);
    }
    .nav {
      position: static;
      height: auto;
      flex-direction: row;
      align-items: center;
      overflow-x: auto;
      overflow-y: hidden;
      border-right: none;
      border-bottom: 1px solid var(--border);
      scrollbar-width: none;
      /* A grid item defaults to min-width:auto, which means "never smaller than
         my content". Without this the row of tabs sets the width of the whole
         page and the overflow above never gets a chance to scroll — the page
         scrolls sideways instead, at every width below 759. */
      min-width: 0;
    }
    /* A row you scroll with a thumb needs a target the thumb can hit. */
    .nav a {
      height: 40px;
    }
    .nav::-webkit-scrollbar {
      display: none;
    }
    .brand,
    .rail-status,
    .nav a[data-tip]::after,
    .topbar .view-title {
      display: none;
    }
    .nav ul {
      display: flex;
      gap: var(--sp-1);
      padding: var(--sp-3) var(--sp-4);
      flex: none;
    }
    .nav li + li {
      margin-top: 0;
    }
    .nav li.split {
      margin: var(--sp-2) 0 0 var(--sp-2);
      padding: 0 0 0 var(--sp-2);
      border-top: 0;
      border-left: 1px solid var(--border);
    }
    .nav a {
      height: 40px;
      padding: 0 var(--sp-4);
      gap: var(--sp-3);
    }

    /* The active pill carries the tab at this width on its own: the tint plus
       the brand ink is enough once the row is horizontal and the label is back. */
    /* Undo the icon-rail hiding: at this width the labels are the navigation. */
    .nav-label {
      position: static;
      width: auto;
      height: auto;
      overflow: visible;
      clip-path: none;
    }
  }

  /* ── account table ─────────────────────────────────────────────────────── */

  /* The health stripe makes a row's state readable at a glance without
     spending a column of text on it. It is also the only colour on the row:
     the status cell repeats the state in words, so the stripe never has to
     carry the meaning alone. */
  .health-mark {
    align-self: stretch;
    min-height: 100%;
    border-radius: 2px;
    background: var(--border-strong);
  }

  .health-mark[data-tone='ok'] {
    background: var(--ok);
  }

  .health-mark[data-tone='warn'] {
    background: var(--warn);
  }

  .health-mark[data-tone='danger'] {
    background: var(--danger);
  }

  .health-mark[data-tone='off'] {
    background: var(--border-strong);
  }

  .health-mark[data-tone='idle'] {
    background: var(--text-3);
  }

  /* The account name carries its source dot inline, so the dot travels with
     the name when the row is read out of context (a phone scrolling sideways,
     a screenshot pasted into a bug report). */
  .label-line {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    min-width: 0;
  }

  .label-line .truncate {
    min-width: 0;
  }

  .cell-usage {
    display: flex;
    flex-direction: column;
    gap: 1px;
    line-height: var(--lh-12);
  }

  .cell-usage em {
    font-style: normal;
    font-size: var(--fs-11);
    font-weight: var(--fw-regular);
    color: var(--text-3);
  }

  .cell-actions {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: var(--sp-4);
  }

  /* A sortable header is a button that fills the cell, so the whole label is
     the hit area and the arrow never drifts away from its word. The floor is
     the header row's own height: without it the button is only as tall as the
     16px glyphs and the label is not the thing you hit. */
  .th-btn {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    min-height: var(--row-h-dense);
    padding: 0;
    border: 0;
    background: none;
    color: inherit;
    font: inherit;
    font-weight: inherit;
    cursor: pointer;
  }

@media (hover: hover) and (pointer: fine) {
    .th-btn:hover {
      color: var(--text);
    }
  }

  .th-btn :global(svg) {
    flex: none;
    transition: opacity var(--dur-1) var(--ease);
  }

  .th-btn :global(svg.dim) {
    opacity: 0.25;
  }

  /* ── model detail ───────────────────────────────────────────────────────
     The table answers "what can I ask for and what does it cost"; the test
     below answers "does it actually answer on this account". */

  .rate-now {
    font-weight: var(--fw-semibold);
    color: var(--ok);
  }

  /* The listed rate stays visible next to the discounted one: a discount that
     hides its own baseline cannot be judged. */
  .rate-was {
    text-decoration: line-through;
    color: var(--text-2);
    font-size: var(--fs-11);
  }

  .meta-inline {
    font-family: var(--font-mono);
    font-size: var(--fs-11);
    color: var(--text-2);
  }

  /* ── connectivity test ───────────────────────────────────────────────────
     Two bands under one head: the row that asks, and the line that answers.
     The model is read from the catalogue above rather than typed, so the
     selector is the widest control and everything after it is a choice about
     how to ask, not what to ask. */
  .probe-card {
    /* The card's own padding would inset the head's rule and the body's row
       alike; here the card is a frame, so each band owns its own inset. */
    padding: 0;
  }

  .probe-card .card-head {
    align-items: center;
    height: 40px;
    gap: var(--sp-3);
    margin-bottom: 0;
    padding: 0 24px 0 16px;
    border-bottom: 1px solid var(--border);
  }

  /* Title and hint share one line, as the drawing lays them out — a stack put
     a full sentence of grey under a 13px title and read as two headings. The
     pair wraps only when the window is too narrow for both. */
  .probe-card .card-head .titles {
    flex-direction: row;
    align-items: center;
    gap: var(--sp-3);
    flex-wrap: wrap;
  }

  /* 11, not the 10 the shared head uses: this hint states a price, and it has
     to read as prose rather than as a caption under the title. */
  .probe-card .card-head .hint {
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-3);
  }

  .probe-body {
    display: flex;
    flex-direction: column;
    gap: var(--sp-5);
    padding: var(--sp-6);
  }

  /* One row: the model, the content, the strength, then — pushed to the right
     edge — the bound and the action. Labels sit above their controls, so the
     row aligns on the controls' baseline, not the labels'. */
  .probe-row {
    display: flex;
    align-items: flex-end;
    gap: var(--sp-5);
    flex-wrap: wrap;
  }

  .probe-field {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    min-width: 0;
  }

  .probe-field-model {
    width: 400px;
    max-width: 100%;
  }

  /* The picker's trigger is drawn as the select it replaces: card tone, a
     control edge, the source prefix held back and the name in body colour. */
  .probe-select {
    display: flex;
    align-items: center;
    gap: var(--sp-1);
    width: 100%;
    height: var(--control-h);
    padding: 0 var(--sp-4);
    border: 1px solid var(--border-strong);
    border-radius: var(--r-sm);
    background: var(--surface);
    color: var(--text);
    font: inherit;
    font-size: var(--fs-12);
    text-align: left;
    cursor: pointer;
    transition: border-color var(--dur-1) var(--ease), box-shadow var(--dur-1) var(--ease);
  }

  @media (hover: hover) and (pointer: fine) {
    .probe-select:hover:not(:disabled) {
      border-color: var(--text-3);
    }
  }

  .probe-select:focus-visible {
    outline: none;
    border-color: var(--action);
    box-shadow: 0 0 0 3px var(--action-soft);
    color: var(--text);
  }

  /* The caret is the drawing's light grey, not the value's body colour. */
  .probe-select :global(svg) {
    flex: none;
    color: var(--text-3);
  }

  /* Locked while a run is in flight: one step off the card, a hairline edge,
     no severity — the state is "busy", not "wrong". */
  .probe-select.off {
    background: var(--surface-2);
    border-color: var(--border);
    opacity: 1;
  }

  .probe-src {
    font-family: var(--font-mono);
    font-size: var(--fs-12);
    color: var(--text-3);
    white-space: nowrap;
    flex: none;
  }

  .probe-name {
    font-family: var(--font-mono);
    font-size: var(--fs-12);
    color: var(--text);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    min-width: 0;
  }

  .probe-ph {
    font-size: var(--fs-12);
    color: var(--text-3);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .probe-checks {
    display: flex;
    align-items: center;
    height: var(--control-h);
    gap: var(--sp-6);
  }

  .probe-check {
    position: relative;
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    font-size: var(--fs-12);
    line-height: var(--lh-12);
    white-space: nowrap;
    cursor: pointer;
    flex: none;
  }

  /* The box is drawn, not native: a 15px square at radius 3 with a brand fill
     and a white tick is the drawing's control, and the native widget renders
     platform chrome that has no place in it. The input stays in the tree —
     invisible but still focusable — so keyboard and screen readers keep
     working unchanged. */
  .probe-check input[type='checkbox'] {
    position: absolute;
    width: 1px;
    height: 1px;
    margin: 0;
    opacity: 0;
  }

  .probe-box {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 15px;
    height: 15px;
    border: 1px solid var(--border-strong);
    border-radius: 3px;
    background: none;
    color: transparent;
    flex: none;
    transition: background var(--dur-1) var(--ease), border-color var(--dur-1) var(--ease),
      color var(--dur-1) var(--ease);
  }

  .probe-check.on .probe-box {
    background: var(--action);
    border-color: var(--action);
    color: var(--action-fg);
  }

  .probe-check input:focus-visible ~ .probe-box {
    border-color: var(--action);
    box-shadow: 0 0 0 3px var(--action-soft);
  }

  /* A gated option says which capability is missing; without the reason a
     dimmed control reads as broken rather than as unsupported by this model.
     The two dimmed states stay distinguishable: gated-and-unchecked is an
     empty stroke box — a filled grey square would claim "selected" — while
     gated-and-checked keeps the recessed fill with the tick in the muted
     colour, so "was chosen, now locked" survives the dimming. (The drawing
     omits the tick on the filled dimmed box; a bare grey square reads as a
     rendering bug, so the muted tick is kept deliberately.) */
  .probe-check.off,
  .probe-check.locked {
    color: var(--text-3);
    cursor: default;
  }

  .probe-check.off .probe-box,
  .probe-check.locked .probe-box {
    background: none;
    border-color: var(--border-strong);
    color: transparent;
  }

  .probe-check.off.on .probe-box,
  .probe-check.locked.on .probe-box {
    background: var(--surface-2);
    border-color: var(--border);
    color: var(--text-3);
  }

  /* The effort select keeps native semantics; only its arrow is drawn. The
     shared select's two 4px gradient wedges read as a solid triangle at this
     size, so they are dropped and the same 8px chevron the model picker uses
     sits on top instead — pointer-events off, so a click still opens the
     select itself. */
  .probe-select-wrap {
    position: relative;
    display: flex;
    align-items: center;
    width: 96px;
  }

  .probe-select-wrap :global(svg) {
    position: absolute;
    right: 9px;
    color: var(--text-3);
    pointer-events: none;
  }

  .probe-select-wrap.off :global(svg) {
    opacity: 0.55;
  }

  .probe-effort {
    width: 96px;
    padding: 0 24px 0 9px;
    background-image: none;
    background-color: var(--surface);
    border-color: var(--border-strong);
    font-family: var(--font-mono);
    font-size: var(--fs-12);
    color: var(--text);
  }

  .probe-rule {
    height: 1px;
    background: var(--border);
  }

  /* One line, four states — no table. The band is 32px so the card keeps its
     height whether it is waiting, running or answering. */
  .probe-result {
    display: flex;
    align-items: center;
    gap: var(--sp-4);
    min-height: 32px;
    font-size: var(--fs-12);
    line-height: var(--lh-12);
  }

  .probe-pill {
    display: inline-flex;
    align-items: center;
    height: 18px;
    padding: 0 7px;
    border-radius: var(--r-pill);
    background: var(--surface-2);
    color: var(--text-2);
    font-size: var(--fs-10);
    line-height: var(--lh-10);
    font-weight: var(--fw-medium);
    white-space: nowrap;
    flex: none;
  }

  .probe-pill.ok {
    background: var(--ok-soft);
    color: var(--ok);
  }

  .probe-pill.danger {
    background: var(--danger-soft);
    color: var(--danger);
  }

  .probe-pill.info {
    background: var(--info-soft);
    color: var(--info);
  }

  .probe-model {
    display: inline-flex;
    align-items: baseline;
    gap: var(--sp-1);
    min-width: 0;
  }

  .probe-sep {
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-3);
    flex: none;
  }

  /* The status and the reason carry the reading; the model and the numbers
     are the subject, so they keep the body's own colour. */
  .probe-content,
  .probe-status,
  .probe-reason {
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-2);
  }

  .probe-reason {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .probe-elapsed,
  .probe-cost {
    font-family: var(--font-mono);
    font-size: var(--fs-12);
    color: var(--text);
    white-space: nowrap;
    flex: none;
  }

  .probe-cost-unit {
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-3);
    flex: none;
  }

  .probe-at {
    font-family: var(--font-mono);
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-3);
    white-space: nowrap;
    flex: none;
  }

  .probe-timeout-hint {
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-3);
    white-space: nowrap;
    flex: none;
  }

  /* The way in to the raw exchange. On a pass it is the only action, so it
     takes the brand tone; on a fail the retry owns that and the detail steps
     back to the secondary colour. */
  .probe-detail,
  .probe-retry {
    display: inline-flex;
    align-items: center;
    gap: 2px;
    padding: 0;
    border: 0;
    background: none;
    color: var(--action-text);
    font: inherit;
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    white-space: nowrap;
    cursor: pointer;
    flex: none;
  }

  .probe-detail.dim {
    color: var(--text-3);
  }

  .probe-retry {
    margin-left: var(--sp-1);
  }

  .probe-empty-value {
    font-size: var(--fs-12);
    color: var(--text-3);
    white-space: nowrap;
  }

  .probe-empty-hint {
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-3);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  /* Timeout is a menu, not a number field: three bounds cover almost every
     run and the custom row takes the rest without inventing a spinner. */
  .probe-timeout {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-1);
    height: var(--control-h);
    padding: 0;
    border: 0;
    background: none;
    color: var(--text);
    font: inherit;
    cursor: pointer;
    flex: none;
  }

  .probe-timeout-label {
    font-size: var(--fs-11);
    color: var(--text-2);
  }

  .probe-timeout-value {
    font-family: var(--font-mono);
    font-size: var(--fs-12);
  }

  .probe-timeout.off .probe-timeout-label,
  .probe-timeout.off .probe-timeout-value {
    color: var(--text-3);
  }

  .probe-run {
    /* 88 idle, 120 while running: the clock widens the label it belongs to
     * rather than reflowing the row. */
    width: 88px;
    font-size: var(--fs-12);
  }

  .probe-run.running {
    width: 120px;
    gap: var(--sp-2);
  }

  /* The primary colour is spent on the action, so the disabled state falls
     back to the recessed chip instead of a half-faded orange. */
  .probe-run:disabled {
    opacity: 1;
    background: var(--surface-2);
    color: var(--text-3);
  }

  .probe-run-clock {
    font-family: var(--font-mono);
    font-size: var(--fs-12);
    color: var(--action-fg);
  }

  /* ── model picker ────────────────────────────────────────────────────────
     Fixed, not absolute: the card clips its overflow to round its corners,
     and a forty-six-model list has to escape that. Coordinates are measured
     from the trigger once the panel has been laid out. No shadow — the
     drawing separates the panel from the page with the hairline alone, and
     a 24px black halo under an upward-opening panel lands on its own
     trigger and reads as a disabled state. */
  .probe-picker {
    position: fixed;
    z-index: var(--z-menu);
    display: flex;
    flex-direction: column;
    overflow: hidden;
    /* Pinned by its bottom edge when flipped, so the panel can only grow into
       free space above — the cap keeps that growth inside the viewport. */
    max-height: calc(100vh - 24px);
    border: 1px solid var(--border);
    border-radius: var(--r-sm);
    background: var(--surface);
    /* The page's own rhythm (.page > * + *) is a 16px top margin, and a margin
       still counts on a fixed box: without this every overlay lands 16px low
       and covers the control it belongs to. */
    margin: 0;
  }

  .probe-picker-search {
    display: flex;
    align-items: center;
    height: 38px;
    padding: 0 var(--sp-4);
  }

  /* No box of its own: inside a panel the field is a row, and a second frame
     there would read as a dialog nested in a dialog. */
  .probe-picker-search input {
    height: 100%;
    padding: 0;
    border: 0;
    border-radius: 0;
    background: none;
    font-size: var(--fs-12);
  }

  .probe-picker-search input:focus {
    box-shadow: none;
  }

  .probe-picker-search input::placeholder {
    color: var(--text-3);
  }

  .probe-picker-rule,
  .probe-tmenu-rule {
    height: 1px;
    background: var(--border);
    flex: none;
  }

  .probe-picker-list {
    display: flex;
    flex-direction: column;
    gap: 2px;
    /* The only flexible part of the panel: it gives way first when the
       viewport caps the menu, and scrolls instead of squeezing the rows. */
    flex: 1 1 auto;
    min-height: 0;
    max-height: 300px;
    padding: var(--sp-2);
    overflow-y: auto;
  }

  /* A group header labels the platform; it is never a stop for the keyboard. */
  .probe-picker-group {
    display: flex;
    align-items: center;
    height: 24px;
    padding: 0 var(--sp-3);
    font-size: var(--fs-10);
    line-height: var(--lh-10);
    font-weight: var(--fw-medium);
    color: var(--text-3);
    /* Fixed-height rows inside a scrollable flex column: without flex: none
       the shrink algorithm compresses every row to squeeze the whole catalogue
       into the max-height, and a 34px option renders at 24. */
    flex: none;
  }

  .probe-picker-option {
    display: flex;
    align-items: center;
    gap: var(--sp-1);
    width: 100%;
    height: 34px;
    padding: 0 var(--sp-3);
    border: 0;
    border-radius: var(--r-xs);
    background: none;
    color: var(--text);
    font: inherit;
    font-size: var(--fs-12);
    text-align: left;
    cursor: pointer;
    transition: background var(--dur-1) var(--ease);
    flex: none;
  }

  @media (hover: hover) and (pointer: fine) {
    .probe-picker-option:hover {
      background: var(--surface-3);
    }
  }

  .probe-picker-option.on,
  .probe-tmenu-item.on {
    background: var(--surface-2);
  }

  .probe-picker-empty {
    padding: var(--sp-2) var(--sp-3);
    font-size: var(--fs-12);
  }

  /* ── timeout menu ────────────────────────────────────────────────────────
     Three bounds cover almost every run; the custom row takes the rest
     without inventing a spinner anybody has to hunt for. */
  .probe-tmenu {
    position: fixed;
    z-index: var(--z-menu);
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: var(--sp-2);
    border: 1px solid var(--border);
    border-radius: var(--r-sm);
    background: var(--surface);
    margin: 0;
  }

  .probe-tmenu-item {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    width: 100%;
    height: 30px;
    padding: 0 var(--sp-2);
    border: 0;
    border-radius: var(--r-xs);
    background: none;
    color: var(--text);
    font: inherit;
    font-size: var(--fs-11);
    text-align: left;
    cursor: pointer;
    flex: none;
  }

  @media (hover: hover) and (pointer: fine) {
    .probe-tmenu-item:hover {
      background: var(--surface-3);
    }
  }

  /* The check column is reserved whether or not a row carries it: without a
     fixed gutter the labels jump sideways when the bound changes. */
  .probe-tmenu-check {
    width: 10px;
    color: var(--action);
    flex: none;
  }

  .probe-tmenu-value {
    font-family: var(--font-mono);
  }

  .probe-tmenu-custom {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    padding: var(--sp-2) var(--sp-2) 0;
  }

  .probe-tmenu-custom input {
    height: var(--control-h-sm);
    font-family: var(--font-mono);
    font-size: var(--fs-11);
  }

  .probe-tmenu-unit {
    font-size: var(--fs-11);
    color: var(--text-3);
    flex: none;
  }

  /* ── test detail drawer ──────────────────────────────────────────────────
     A right-hand panel, not a dialog: the veil dims and dismisses, and the page
     behind it stays live. The raw exchange is read next to the form, not
     instead of it. */
  .probe-veil {
    position: fixed;
    inset: 0;
    z-index: var(--z-drawer-veil);
    border: none;
    padding: 0;
    /* Fixed boxes still honour margins, and this one sits under .page's 16px
       section gap — an undimmed strip would otherwise run along the top. */
    margin: 0;
    background: var(--backdrop);
    cursor: default;
    animation: veil-in var(--dur-3) var(--ease);
  }

  .probe-drawer {
    position: fixed;
    top: 0;
    right: 0;
    bottom: 0;
    z-index: var(--z-drawer);
    display: flex;
    flex-direction: column;
    width: min(560px, 100vw);
    padding: var(--sp-6);
    margin: 0;
    background: var(--surface);
    color: var(--text);
    border-left: 1px solid var(--border);
    overscroll-behavior: contain;
    animation: drawer-in var(--dur-3) var(--ease);
  }

  .probe-drawer-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--sp-4);
    min-height: 32px;
  }

  .probe-drawer-head h2 {
    margin: 0;
    font-size: var(--fs-13);
    line-height: var(--lh-13);
    font-weight: var(--fw-semibold);
  }

  .probe-drawer-body {
    display: flex;
    flex-direction: column;
    gap: var(--sp-6);
    flex: 1 1 auto;
    min-height: 0;
    margin-top: var(--sp-4);
    overflow-y: auto;
  }

  .probe-drawer-sec {
    display: flex;
    flex-direction: column;
    gap: var(--sp-3);
  }

  .probe-drawer-sec h3 {
    margin: 0;
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    font-weight: var(--fw-medium);
    letter-spacing: var(--track-wide);
    color: var(--text-3);
  }

  /* Label beside value, the settings-field rhythm: the label is the question
     and the value is the answer, never the other way round. */
  .probe-facts {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: var(--sp-2) var(--sp-5);
    margin: 0;
    font-size: var(--fs-12);
  }

  .probe-facts dt {
    color: var(--text-3);
    white-space: nowrap;
  }

  .probe-facts dd {
    margin: 0;
    min-width: 0;
    overflow-wrap: break-word;
  }

  .probe-step-facts {
    grid-template-columns: auto;
    gap: var(--sp-1);
    font-size: var(--fs-11);
    color: var(--text-2);
  }

  /* Kept whole. A truncated reply reads as a broken one, and the one-line
     summary the result row shows is derived from it, not the other way. */
  .probe-pre {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    white-space: pre-wrap;
    word-break: break-word;
    overflow-wrap: break-word;
  }

  .probe-pre.dim {
    color: var(--text-2);
  }

  .probe-step {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    padding: var(--sp-3) var(--sp-4);
    border: 1px solid var(--border);
    border-radius: var(--r-sm);
    background: var(--surface-2);
  }

  .probe-step-head {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
  }

  .probe-step-label {
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-2);
    white-space: nowrap;
  }

  .probe-step-elapsed {
    font-family: var(--font-mono);
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-3);
    white-space: nowrap;
    flex: none;
  }

  .probe-drawer-err {
    margin: 0;
    color: var(--danger);
  }

  .probe-kv {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
  }

  .probe-kv-label {
    font-size: var(--fs-10);
    line-height: var(--lh-10);
    font-weight: var(--fw-medium);
    letter-spacing: var(--track-wide);
    color: var(--text-3);
  }

  .probe-kv-empty {
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-3);
  }

  .cell-actions {
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: var(--sp-1) var(--sp-3);
  }

  /* The log dialog is a terminal, not a form: it keeps a fixed height so the
     newest line is always at the bottom edge instead of pushing the dialog
     taller every time something happens. */
  /* Service state and the lock pool are two answers to the same question
     ("why did that fail"), so they share a row. The lock pool is the taller
     one, and the service panel simply tops out at its own content. */
  /* ── overview: pool bar ─────────────────────────────────────────── */

  /* One card, two rows, one hairline between them. Row one is the console's own
     identity — which build is answering, where it listens, which upstreams stand
     behind it, and the one address a client needs; row two is the pool: how
     many can serve, where the credits sit, what today burned. The brand mark
     lives in the rail, not here — the band repeats nothing the rail already
     says. */
  .pool {
    display: flex;
    flex-direction: column;
    padding: 13px 16px 0;
    background: var(--surface);
    border-radius: var(--r-md);
  }

  .pool-ident {
    display: flex;
    align-items: center;
    gap: 12px;
    min-height: 26px;
    min-width: 0;
  }

  /* The status pill is a 22px capsule at the full radius: soft fill, 6px dot,
     11px word. The rules page puts status pills at r-pill, and offline takes
     the danger tint — a console that is not answering is the one state that
     must read at a glance. */
  .run-pill {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    height: 22px;
    padding: 0 8px 0 10px;
    flex: none;
    border-radius: var(--r-pill);
    background: var(--ok-soft);
    color: var(--ok);
    font-size: var(--fs-11);
    font-weight: var(--fw-medium);
    line-height: var(--lh-11);
    white-space: nowrap;
  }

  .run-pill.bad {
    background: var(--danger-soft);
    color: var(--danger);
  }

  .run-dot {
    width: 6px;
    height: 6px;
    flex: none;
    border-radius: 50%;
    background: currentColor;
  }

  /* Label 12px text-3, value 13px text-2, six pixels apart: the pair reads as
     one phrase ("监听 127.0.0.1:6875") without a colon doing the work. */
  .ident-lbl {
    font-size: var(--fs-12);
    line-height: var(--lh-12);
    color: var(--text-3);
    white-space: nowrap;
  }

  .ident-val {
    font-size: var(--fs-13);
    line-height: var(--lh-13);
    color: var(--text-2);
    white-space: nowrap;
  }

  .ident-pair {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
  }

  .dot-sep {
    color: var(--text-3);
    font-size: var(--fs-13);
    line-height: var(--lh-13);
  }

  /* The endpoint chip shows the address; the button beside it copies it. Two
     elements, not one: the chip is a reading surface and the button is the
     action, and a chip you can click is a chip you click by accident. */
  .api-chip {
    flex: none;
    min-width: 0;
    max-width: 320px;
    height: 26px;
    padding: 6px 10px;
    border-radius: var(--r-sm);
    background: var(--surface-2);
    color: var(--text-2);
    font-size: var(--fs-12);
    line-height: 14px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .copy-btn {
    flex: none;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    height: 26px;
    min-width: 52px;
    padding: 0 var(--sp-3);
    border: 0;
    border-radius: var(--r-sm);
    background: var(--surface-2);
    color: var(--text-2);
    font: inherit;
    font-size: var(--fs-12);
    line-height: 1;
    cursor: pointer;
    transition: background var(--dur-1) var(--ease), color var(--dur-1) var(--ease);
  }

  @media (hover: hover) and (pointer: fine) {
    .copy-btn:hover {
      background: var(--surface-3);
      color: var(--text);
    }
  }

  /* Copied is a fact, not a celebration: the word stays, the colour says it
     landed. Two seconds, then back. */
  .copy-btn.copied {
    color: var(--action);
  }

  .pool-rule {
    height: 1px;
    margin-top: 12px;
    background: var(--border);
  }

  /* Row two: four counts, a rule, the two balances, a rule, today. */
  .pool-metrics {
    display: flex;
    align-items: center;
    gap: 16px;
    min-height: 88px;
    min-width: 0;
  }

  /* The divider belongs to what precedes it: 16px in front, 28px behind, the
     rhythm the rules page draws between a count column and a balance column. */
  .pool-vrule {
    width: 1px;
    height: 52px;
    flex: none;
    margin-right: 12px;
    background: var(--border);
  }

  /* A count is a filter shortcut when it has something to hide, and plain text
     when it does not: the whole pool is not a useful subset. Number over label
     (22px over 12px); the 8px padding with its negative margin buys the row a
     tall target without moving the 16px rhythm — the column is 90px wide, the
     fourth ends at 424px and the rule lands at 440px, exactly where the rules
     page puts it. */
  .seg-metric {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 12px;
    min-width: 90px;
    padding: 10px 8px;
    margin: -10px -8px;
    border: 0;
    border-radius: var(--r-sm);
    background: none;
    color: var(--text);
    font: inherit;
    text-align: left;
    cursor: default;
  }

  button.seg-metric {
    cursor: pointer;
    transition: background var(--dur-1) var(--ease);
  }

  @media (hover: hover) and (pointer: fine) {
    button.seg-metric:hover {
      background: var(--surface-2);
    }
  }

  .seg-metric.on {
    background: var(--surface-2);
  }

  /* The number carries the weight; the label is the quiet one. v2 dropped the
     per-tone colours from these four — the ledger below says which account is
     which, and a band of coloured numbers out-shouts the table it summarises. */
  .seg-metric .v {
    font-size: var(--fs-22);
    line-height: var(--lh-22);
    font-weight: var(--fw-semibold);
    font-variant-numeric: tabular-nums;
    color: var(--text);
  }

  .seg-metric .k {
    font-size: var(--fs-12);
    line-height: var(--lh-12);
    color: var(--text-3);
  }

  /* Two rows, one per platform, dot and name beside the balance. The name
     column is 72px so both balances share one left edge — a ragged column of
     money is a column you read twice. */
  .pool-credits {
    display: flex;
    flex-direction: column;
    gap: 8px;
    min-width: 0;
  }

  .src-balance {
    display: flex;
    align-items: center;
    gap: 6px;
    min-height: 30px;
    padding: 0 var(--sp-2);
    margin: 0 calc(-1 * var(--sp-2));
    border: 0;
    border-radius: var(--r-sm);
    background: none;
    color: var(--text);
    font: inherit;
    text-align: left;
    cursor: pointer;
    transition: background var(--dur-1) var(--ease);
  }

  @media (hover: hover) and (pointer: fine) {
    .src-balance:hover {
      background: var(--surface-2);
    }
  }

  .src-balance .src-dot {
    flex: none;
  }

  .src-balance .src-name {
    min-width: 72px;
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-3);
    white-space: nowrap;
  }

  .src-balance .v {
    font-size: var(--fs-22);
    line-height: var(--lh-22);
    font-variant-numeric: tabular-nums;
    color: var(--text);
  }

  /* The active filter is a chip, not a hidden mode: without it, a table that
     is missing half its rows looks like a table with missing rows. */
  .filter-row {
    display: flex;
    align-items: center;
    gap: var(--sp-4);
    margin-bottom: var(--sp-5);
  }

  .filter-chip {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    height: 24px;
    padding: 0 var(--sp-1) 0 var(--sp-3);
    border-radius: var(--r-xs);
    background: var(--action-soft);
    color: var(--action);
    font-size: var(--fs-11);
    font-weight: var(--fw-medium);
  }

  .chip-x {
    display: grid;
    place-items: center;
    width: 20px;
    height: 20px;
    padding: 0;
    border: 0;
    border-radius: var(--r-xs);
    background: none;
    color: inherit;
    cursor: pointer;
  }

@media (hover: hover) and (pointer: fine) {
    .chip-x:hover {
      background: var(--action);
      color: var(--action-fg);
    }
  }

  /* ── overview: diagnostics ─────────────────────────────────────────────── */

  /* Three questions asked right after something went wrong: what expires, is
     the gateway healthy, which model is refusing. Equal columns, equal height. */
  .diag {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: var(--sp-6);
    align-items: stretch;
  }

  /* The fold and the panels it owns. Folded, this whole block is one 36px row
     whose summary already carries the three counts worth glancing at. */
  .diag-block {
    display: grid;
    gap: var(--sp-5);
  }

  /* 36px, not the 44 the data pages give their folds: this one is a footnote
     above the page foot, and it must not cost the ledger a row. */
  .diag-block .fold-row {
    min-height: 36px;
  }

  .diag-card {
    display: flex;
    flex-direction: column;
    gap: var(--sp-3);
    min-width: 0;
  }

  /* Ten facts in five rows instead of ten: a narrow card of stacked pairs made
     the whole band twice as tall as its neighbours. */
  .kv-2 {
    grid-template-columns: auto minmax(0, 1fr) auto minmax(0, 1fr);
    gap: var(--sp-3) var(--sp-4);
  }

  @media (max-width: 1279px) {
    .diag {
      grid-template-columns: minmax(0, 1fr);
    }
  }

  /* ── tasks: run bar ────────────────────────────────────────────────────── */

  /* The band inherits the credits page's metric rhythm; only the progress
     rail is its own. */
  .queue-band {
    position: relative;
    overflow: hidden;
  }

  .queue-progress {
    position: absolute;
    left: 0;
    right: 0;
    bottom: 0;
    height: 2px;
    overflow: hidden;
    border-radius: 0 0 var(--r-md) var(--r-md);
    background: var(--surface-2);
  }

  .queue-progress i {
    position: absolute;
    inset-block: 0;
    width: 32%;
    background: var(--action);
    animation: queue-slide 1.4s var(--ease) infinite;
  }

  .kv .v.missing {
    color: var(--text-2);
  }

  /* ── process log drawer ────────────────────────────────────────────────── */

  /* A right-hand panel, not a dialog: no backdrop, no focus trap, and the page
     behind it stays live. Reading the log while a sweep runs is the whole
     reason this surface exists. */
  /* The drawer's veil: the same dim as a dialog backdrop, but it belongs to
     the drawer layer and only ever dismisses the drawer — never a confirm
     that was opened from inside it. */
  .log-veil {
    position: fixed;
    inset: 0;
    z-index: var(--z-drawer-veil);
    border: none;
    padding: 0;
    background: var(--backdrop);
    cursor: default;
    animation: veil-in var(--dur-3) var(--ease);
  }

  @keyframes veil-in {
    from {
      opacity: 0;
    }
    to {
      opacity: 1;
    }
  }

  .log-drawer {
    position: fixed;
    top: 0;
    right: 0;
    bottom: 0;
    z-index: var(--z-drawer);
    display: flex;
    flex-direction: column;
    gap: var(--sp-3);
    width: min(720px, 100vw);
    padding: var(--sp-6);
    background: var(--surface);
    color: var(--text);
    border-left: 1px solid var(--border);
    overscroll-behavior: contain;
    animation: drawer-in var(--dur-3) var(--ease);
  }

  @keyframes drawer-in {
    from {
      opacity: 0;
      transform: translateX(16px);
    }
    to {
      opacity: 1;
      transform: none;
    }
  }

  .log-head {
    display: flex;
    align-items: center;
    gap: var(--sp-4);
    min-height: 56px;
    padding-bottom: var(--sp-2);
    border-bottom: 1px solid var(--border);
  }

  .log-head h2 {
    margin: 0;
    font-size: var(--fs-14);
    line-height: var(--lh-14);
    font-weight: var(--fw-semibold);
  }

  /* How much is here. A live stream with no count makes "did it stop, or is
     nothing happening" unanswerable. */
  .log-n {
    font-size: var(--fs-11);
    line-height: var(--lh-11);
    color: var(--text-3);
    font-variant-numeric: tabular-nums;
  }

  /* The stream drops while the gateway restarts; the bar says so and offers a
     retry that does not wait for the browser's own backoff. */
  .log-lost {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    min-height: 32px;
    padding: var(--sp-2) var(--sp-4);
    border-radius: var(--r-md);
    background: var(--warn-soft);
    color: var(--warn);
    font-size: var(--fs-11);
  }

  .log-live {
    display: inline-flex;
    align-items: center;
    flex: none;
  }

  .log-live .src-dot {
    background: var(--warn);
  }

  .log-live.on .src-dot {
    background: var(--ok);
  }

  /* One row, three answers: channel, level, text. Wrapping is allowed below
     720px but costs a line of log, so the segments stay compact. */
  .log-bar {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    min-height: 44px;
    flex-wrap: wrap;
  }

  .log-bar .segmented {
    flex: none;
  }

  .chip-n {
    margin-left: var(--sp-2);
    font-size: var(--fs-10);
    color: var(--text-3);
    font-variant-numeric: tabular-nums;
  }

  .log-bar .segmented button.active .chip-n {
    color: inherit;
    opacity: 0.75;
  }

  /* Search sits at the right edge: it is the one control that grows, and a
     growing control in the middle pushes the segments around as you type. */
  .log-search {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    flex: 1 1 12rem;
    max-width: 20rem;
    height: 32px;
    padding: 0 var(--sp-3);
    border: 1px solid var(--border-strong);
    border-radius: var(--r-md);
    background: var(--surface);
    color: var(--text-3);
  }

  .log-search:focus-within {
    border-color: var(--action);
    box-shadow: 0 0 0 3px var(--action-soft);
  }

  .log-search input {
    flex: 1;
    min-width: 0;
    height: 100%;
    padding: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-12);
    color: var(--text);
    background: none;
    border: 0;
    outline: none;
  }

  .chip-n {
    font-variant-numeric: tabular-nums;
    opacity: 0.72;
  }

  /* A terminal, not a form: one row is exactly 26px, which is what makes the
     virtual window arithmetically exact. Long lines are truncated visually and
     read in full in the inspector below. */
  .log-stream {
    position: relative;
    flex: 1 1 auto;
    min-height: 0;
    overflow-y: auto;
    padding: 0;
    border-top: 1px solid var(--border);
    border-bottom: 1px solid var(--border);
    background: var(--surface-2);
    font-family: var(--font-mono);
    font-size: var(--fs-12);
  }

  /* The sizer is the scrollbar's only job: it holds the height of the whole
     buffer while the window renders the rows that are actually visible. */
  .log-sizer {
    width: 1px;
  }

  .log-window {
    position: absolute;
    top: 0;
    left: 0;
    right: 0;
  }

  .ln {
    display: flex;
    align-items: baseline;
    gap: var(--sp-3);
    width: 100%;
    height: 26px;
    padding: 0 var(--sp-2);
    font: inherit;
    text-align: left;
    color: inherit;
    background: none;
    border: 0;
    /* Reserved, so adding severity never shifts the text sideways. */
    border-left: 2px solid transparent;
    white-space: nowrap;
    overflow: hidden;
    cursor: default;
  }

@media (hover: hover) and (pointer: fine) {
    .ln:hover {
      background: var(--surface-2);
    }
  }

  .ln.sel {
    background: var(--surface-3);
  }

  .lt {
    flex: none;
    color: var(--text-2);
    font-variant-numeric: tabular-nums;
  }

  .lch {
    flex: none;
    padding: 0 var(--sp-2);
    font-size: var(--fs-11);
    color: var(--text-2);
    border: 1px solid var(--border);
    border-radius: var(--r-sm);
  }

  .lch[data-ch='task'] {
    color: var(--src-wb);
    border-color: var(--src-wb-soft);
    background: var(--src-wb-soft);
  }

  .lch[data-ch='chat'] {
    color: var(--src-trae);
    border-color: var(--src-trae-soft);
    background: var(--src-trae-soft);
  }

  .lx {
    display: block;
    flex: 1 1 auto;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    color: var(--text);
  }

  /* The inspector carries the whole line, selectable, without giving up the
     fixed row height that keeps scrolling predictable. */
  .log-inspect {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    max-height: 9rem;
    padding: var(--sp-3);
    border: 1px solid var(--border);
    border-radius: var(--r-md);
    background: var(--surface);
  }

  .log-inspect-head {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    font-size: var(--fs-11);
    color: var(--text-2);
  }

  .log-inspect-body {
    margin: 0;
    overflow: auto;
    font-family: var(--font-mono);
    font-size: var(--fs-12);
    line-height: var(--leading-normal);
    white-space: pre-wrap;
    word-break: break-word;
    color: var(--text);
    user-select: text;
  }

  .log-hint {
    margin: 0;
  }

  /* Severity is a rule down the left edge rather than coloured text. At 12px
     mono, a screen of amber and red turns every line into a shout; a column of
     colour is something the eye can count without reading. */
  .ln.w {
    border-left-color: var(--warn);
  }

  .ln.e {
    border-left-color: var(--danger);
  }

  .log-empty {
    margin: 0;
  }

  .log-jump {
    position: absolute;
    bottom: 5rem;
    left: 50%;
    transform: translateX(-50%);
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    height: 28px;
    padding: 0 var(--sp-5);
    font: inherit;
    font-size: var(--fs-11);
    color: var(--action-fg);
    background: var(--action);
    border: 0;
    border-radius: var(--r-sm);
    box-shadow: var(--shadow-2);
    cursor: pointer;
  }

  .log-jump :global(svg) {
    transform: rotate(180deg);
  }

  .log-unseen {
    min-width: 18px;
    padding: 0 5px;
    border-radius: 9px;
    background: var(--action-fg);
    color: var(--action);
    font-size: var(--fs-11);
    font-variant-numeric: tabular-nums;
  }

  /* Below 1100px the drawer is the page: a 720px panel on a 900px screen would
     leave a sliver of unusable content behind it. */
  @media (max-width: 1100px) {
    .log-drawer {
      width: 100vw;
      border-left: 0;
    }
  }

  /* ── dialog ────────────────────────────────────────────────────────────── */

  .modal-backdrop {
    position: fixed;
    inset: 0;
    z-index: var(--z-backdrop);
    border: none;
    padding: 0;
    background: var(--backdrop);
    cursor: default;
  }

  /* Three bands, one of which scrolls. The tab strip and the action stay put
     while the form inside moves — otherwise a long paste pushes the button
     that submits it out of view. The title is a band of its own between them,
     not a head with a close button: the strip leads, the title names the open
     tab, and Esc or the backdrop dismisses. */
  .modal {
    position: fixed;
    z-index: var(--z-dialog);
    top: 50%;
    left: 50%;
    transform: translate(-50%, -50%);
    width: min(40rem, calc(100vw - var(--sp-9)));
    max-height: calc(100vh - var(--sp-11));
    display: grid;
    grid-template-rows: auto auto minmax(0, 1fr) auto;
    background: var(--surface);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: var(--r-lg);
    box-shadow: var(--shadow-dialog);
    overflow: hidden;
  }

  /* The strip is the dialog's one navigation: 396×36 centred on top, three
     equal segments on a surface-2 rail, the open one lifted to the card
     surface and named in the brand colour. */
  .modal-tabs {
    justify-self: center;
    margin-top: var(--sp-6);
    width: 396px;
    max-width: calc(100% - 2 * var(--sp-8));
    gap: var(--sp-2);
    padding: 3px;
  }

  .modal-tabs > button {
    flex: 1;
    min-width: 0;
    height: 30px;
    padding: 0 var(--sp-3);
    border-radius: var(--r-xs);
    font-size: var(--fs-13);
    justify-content: center;
  }

  .modal-tabs > button.active {
    background: var(--surface);
    color: var(--action);
    box-shadow: none;
  }

  /* Title and description under the strip: one line of intent per tab, no
     background block — the card is quiet enough already. */
  .modal-intro {
    padding: var(--sp-7) var(--sp-8) var(--sp-5);
  }

  .modal-intro h2 {
    margin: 0;
    font-size: var(--fs-16);
    line-height: var(--lh-16);
    font-weight: var(--fw-semibold);
    color: var(--text);
  }

  .modal-intro p {
    margin: var(--sp-2) 0 0;
    font-size: var(--fs-13);
    line-height: var(--lh-13);
    color: var(--text-2);
  }

  .modal-body {
    min-height: 0;
    max-height: 70vh;
    overflow-y: auto;
    padding: var(--sp-6) var(--sp-8);
    display: grid;
    gap: var(--sp-6);
    align-content: start;
    border-top: 1px solid var(--border);
  }

  /* In the dialog the labels grow to body size: 95-field settings grids stay
     at 11px, but three fields on a card read as a form, and a form's labels
     are the same size as its values. */
  .modal-body .field-label {
    font-size: var(--fs-13);
    line-height: var(--lh-13);
    letter-spacing: normal;
    color: var(--text);
  }

  .modal-body input:not([type='checkbox']):not([type='radio']),
  .modal-body select,
  .modal-body textarea {
    font-size: var(--fs-13);
    line-height: var(--lh-13);
  }

  /* One row that names the three optional fields and opens them: the common
     paste is a single textarea, and a client that exported more than the
     access token reveals the rest explicitly. */
  .opt-fold {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    width: 100%;
    height: 36px;
    padding: 0 var(--sp-5);
    border: none;
    border-radius: var(--r-sm);
    background: var(--surface-2);
    color: var(--text-2);
    font: inherit;
    font-size: var(--fs-13);
    cursor: pointer;
    text-align: left;
  }

  .opt-fold .opt-label {
    font-weight: var(--fw-medium);
    color: var(--text);
    white-space: nowrap;
  }

  .opt-fold .opt-items {
    margin-left: auto;
    font-size: var(--fs-12);
    line-height: var(--lh-12);
    color: var(--text-3);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .opt-fold :global(svg.fold-chev) {
    transition: transform var(--dur-2) var(--ease);
    color: var(--text-3);
    flex: none;
  }

  .opt-fold :global(svg.fold-chev.open) {
    transform: rotate(90deg);
  }

  /* No band, no border: the action sits on the card like the rest, right
     aligned, 16 above and 12 below. */
  .modal-foot {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: var(--sp-3);
    padding: var(--sp-6) var(--sp-8) var(--sp-5);
  }

  .modal-foot .btn {
    height: 32px;
    min-width: 80px;
    font-size: var(--fs-13);
    justify-content: center;
  }

  .modal-foot .btn.primary {
    min-width: 88px;
  }

  /* A phone cannot spare 396px for three labels: the strip goes full width
     and the segments share it equally. */
  @media (max-width: 640px) {
    .modal-tabs {
      width: 100%;
    }
  }

  @media (max-width: 640px) {
    .page {
      padding: var(--sp-7) var(--page-pad-x);
    }
    .topbar {
      padding: var(--sp-3) var(--page-pad-x);
    }
    /* A phone scrolls the pool sideways instead of dropping columns. Dropping
       them was the older rule, and it hid credits, calls, in-flight and last
       success — the four things you are most likely to open this page on a
       phone to check.
       Identity (stripe + account) sticks to the left and the action column
       sticks to the right, so a mid-scroll row still says who it is and can
       still be acted on without reaching the end. */
    .accounts-table {
      --table-min: 54rem;
    }
    .accounts-table .row > :nth-child(1) {
      position: sticky;
      left: 0;
      z-index: 1;
      background: var(--surface);
    }
    .accounts-table .row > :nth-child(2) {
      position: sticky;
      left: 0;
      z-index: 1;
      background: var(--surface);
      /* Both pinned cells start at the left edge; the name is indented far
         enough to clear the 3px stripe and the grid gap behind it, so the two
         backgrounds line up instead of overlapping the text. */
      padding-left: calc(3px + var(--sp-5));
    }
    .accounts-table .row > :last-child {
      position: sticky;
      right: 0;
      z-index: 1;
      background: var(--surface);
      box-shadow: var(--shadow-edge);
    }
    .accounts-table .row.head > :nth-child(1),
    .accounts-table .row.head > :nth-child(2),
    .accounts-table .row.head > :last-child {
      background: var(--surface-2);
    }

@media (hover: hover) and (pointer: fine) {
      .accounts-table .row:not(.head):hover > :nth-child(1),
      .accounts-table .row:not(.head):hover > :nth-child(2),
      .accounts-table .row:not(.head):hover > :last-child {
        background: var(--surface-2);
      }
    }
  }
</style>
