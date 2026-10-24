// Every string the console shows, in one place.
//
// There is deliberately no language switch: the console ships in Chinese, and
// this table exists so copy can be reviewed, searched and reworded without
// hunting through markup. Entries that interpolate are functions so a caller
// cannot forget a placeholder.

export const t = {
  brand: {
    name: 'CodeNexus',
    title: 'CodeNexus 控制台',
    tagline: 'AI CODING GATEWAY',
    // The value half of the overview band's "上游 · WorkBuddy · Trae CN" pair.
    // The label lives beside it in overview.upstreamLabel; the rail keeps its
    // own shorter phrasing.
    upstreamValue: 'WorkBuddy · Trae CN',
  },

  // The 28px line at the foot of every page. One constant — what this product
  // is — and one sentence that changes with the view: what the numbers on
  // this page can and cannot be trusted for.
  foot: {
    brand: 'CodeNexus · 本机 AI 编码账号网关',
    overview: '本机服务运行中 · 账号与面板数据以实际运行状态为准',
    usage: '用量历史来自本机 usage.db · 不记录提示词与回复内容',
    credits: '余额是上游快照，不随请求实时变动 · 刷新会逐账号查询上游',
    tasks: '任务按账号串行执行 · 重复提交会被拦下',
    models: '模型目录由已接入账号实时聚合 · 未读取时不预填模型名',
    settings: '配置写入 config.json · 监听地址与端口改动需重启生效',
  },

  // The pulse at the foot of the rail. It answers one question — is the
  // gateway answering — and hides the version behind a hover so the rail
  // stays quiet.
  rail: {
    running: '运行中',
    offline: '未连接',
    offlineHint: '控制台连不上网关，页面数据可能已过期。',
    justStarted: '刚刚启动',
    uptime: (d) => `已运行 ${d}`,
  },

  nav: {
    aria: '主导航',
    // Six destinations, read top to bottom in the order the console answers
    // questions. Adding an account and reading the log are actions in the top
    // bar, not places to go, so they are absent from this list on purpose.
    overview: '概览',
    usage: '用量管理',
    credits: '积分详情',
    tasks: '任务中心',
    models: '模型详情',
    settings: '设置',
    addAction: '添加账号',
    logsAction: '日志',
    appearance: '外观',
    theme: { system: '跟随系统', light: '浅色', dark: '深色' },
  },

  time: {
    justNow: '刚刚',
    minutesAgo: (n) => `${n} 分钟前`,
    hoursAgo: (n) => `${n} 小时前`,
    daysAgo: (n) => `${n} 天前`,
    // A deadline counts down, not up: "2 天后" where a past event says "2 天前".
    inMinutes: (n) => `${n} 分钟后`,
    inHours: (n) => `${n} 小时后`,
    inDays: (n) => `${n} 天后`,
    // Uptime is elapsed time, not a timestamp, so its units are its own
    // sentence rather than a suffix on a number.
    uptimeDays: (d, h) => `${d} 天 ${h} 小时`,
    uptimeHours: (h, m) => `${h} 小时 ${m} 分`,
    uptimeMinutes: (m) => `${m} 分`,
  },

  common: {
    loading: '加载中…',
    // Every failure the operator can see is written here, including the ones
    // thrown before a component exists to call t(). A message assembled at the
    // throw site is a message no one can find twice.
    err: {
      parse: (status) => `响应解析失败（HTTP ${status}）`,
      request: (status) => `请求失败（HTTP ${status}）`,
    },
    close: '关闭',
    cancel: '取消',
    confirm: '确认',
    remove: '移除',
    retry: '重试',
    refresh: '刷新',
    refreshing: '刷新中…',
    dash: '—',
    expand: '展开',
    collapse: '收起',
    skipToContent: '跳到内容',
    underMinute: '不到 1 分钟',
    more: '更多操作',
    done: '完成',
  },

  health: {
    off: '已停用',
    paused: '已暂停',
    bad: '熔断',
    warn: '冷却中',
    ok: '健康',
    idle: '待命',
  },

  // The pool bar is the overview's first sentence: one status pill and the
  // build identity on top, four health counts, the per-source balances and
  // today's burn below. Nothing here costs an upstream call.
  pool: {
    title: '账号池',
    total: (n) => `共 ${n} 个账号`,
    segment: { ok: '可用', cooling: '冷却', broken: '熔断', off: '停用', paused: '暂停' },
    segmentHint: (label, n) => `只看${label}的 ${n} 个账号`,
    only: (label) => `仅看：${label}`,
    clearFilter: '清除筛选',
    today: '今日 Token',
    vsPrev: (v) => `较昨日同期 ${v}`,
    noBaseline: '暂无昨日同期基线',
    synced: (v) => `${v}更新`,
  },

  accounts: {
    // The pool's maintenance row. Same executors the scheduler runs, triggered
    // by hand — it sits above the table because it acts on the whole table.
    batch: {
      label: '批量维护',
      checkin: '全部签到',
      activity: '全部活跃',
      balance: '刷新额度',
      travel: '全部出行',
      keepalive: '全部保活',
    },
    // Columns follow the reference console. Credential state and expiry lost
    // their columns and moved onto the account name as a hover. The platform
    // column is gone too: the group head names the source and every row carries
    // its 6px dot, so a whole column repeated what two smaller cues already say.
    col: {
      health: '健康',
      account: '账号',
      status: '状态',
      credits: '积分',
      calls: '成功 / 失败',
      inFlight: '在途',
      usage: '用量',
      lastSuccess: '最近成功',
      actions: '操作',
    },
    action: {
      checkin: '签到',
      balance: '余额',
      tasks: '任务',
      keepalive: '保活',
      revive: '解冻',
      pause: '暂停',
      resume: '恢复',
      disable: '禁用',
      remove: '移除',
    },
    // The group head repeats the source total the pool bar shows, because a
    // collapsed group still has to say how much sits inside it.
    group: {
      credit: (v) => `${v} 积分`,
      expand: (name) => `展开 ${name}`,
      collapse: (name) => `收起 ${name}`,
    },
    sort: {
      asc: '升序',
      desc: '降序',
      none: '取消排序',
      hint: (col) => `按${col}排序`,
    },
    credentialRefreshable: '可刷新',
    credentialAccessOnly: '仅访问令牌',
    checkinPending: '未签到',
    expiresAt: (v) => `过期 ${v}`,
    inFlight: (n) => `${n} 进行中`,
    opDone: (op, label) => `${label} 已${op}`,
    locks: {
      empty: '当前没有被锁定的模型。',
      title: '模型锁定',
      hint: (n) => `${n} 个模型暂时不可用`,
      col: { model: '模型', account: '账号', until: '解锁时间' },
      nextFree: (v) => `最早解锁：${v}`,
    },
    removeConfirm: (label, source) => `移除账号 ${label}（${source}）？`,
    removeDone: (label) => `已移除 ${label}`,
    empty: {
      title: '还没有账号',
      body: '添加账号后，这里会显示健康状态、额度余额、调用成功与失败、Token 用量。',
    },
  },

  tasks: {
    meta: (n) => `${n} 个账号`,
    // Kept for the per-account task rows, which still write to these results.
    batch: {
      checkinAll: '全部签到',
      activityAll: '全部活跃',
      balanceAll: '刷新额度',
      travelAll: '全部出行',
      keepaliveAll: '全部保活',
    },
    queue: {
      // The band states three figures; these are their captions. A caption is
      // a noun, not a sentence — the number already carries the count.
      pendingLabel: '待办任务',
      claimLabel: '可领取',
      accountLabel: '个账号',
      scan: '扫描待办',
      run: '执行全部待办',
      running: '执行中…',
    },
    // The queue's two numbers live on the account row they belong to, so the
    // page does not list the same accounts twice.
    row: {
      pending: (n) => `待办 ${n}`,
      claimable: (n) => `可领 ${n}`,
      // Said once per account when the queue has nothing for it, so the row
      // does not leave an empty column where the count usually sits.
      idle: '无待办',
    },
    accounts: {
      title: '账号任务',
      hint: '每个 WorkBuddy 账号展开后显示任务、进度、状态与奖励',
      emptyTitle: '先在「添加账号」完成设备授权',
      empty: '任务中心需要有账号作为操作对象；授权完成后，签到、接取、领取任务与额度刷新都会出现在这里。',
      local: '本机',
      ops: '账号操作',
      credit: (value) => `额度 ${value}`,
      expiring: (n) => `${n} 即将过期`,
    },
    accountOps: {
      runAll: '执行全部',
      acceptAll: '接取全部',
      checkin: '签到',
      checkinDone: '已签',
      balance: '额度',
      keepalive: '保活',
      pause: '暂停',
      resume: '恢复',
    },
    table: {
      task: '任务',
      progress: '进度',
      status: '状态',
      reward: '奖励',
    },
    actions: { claim: '领取', run: '执行' },
    results: {
      more: (n) => `全部 ${n} 条`,
      less: '收起',
    },
    rewardCredit: '积分',
    rewardEnergy: '能量',
    rewardEnergyDelta: (n) => ` +${n} 能量`,
    loadingTasks: '加载任务中…',
    noTasks: '该账号没有可用的成长任务。',
    miniProgram: '小程序',
    checkinTitle: '今日已签到；再点一次会重签并刷新额度',
    state: {
      claimed: '已领取',
      locked: '未解锁',
      claimable: '可领取',
      completed: '已完成',
      inProgress: '进行中',
      pending: '待完成',
    },
    msg: {
      running: (what) => `${what} 执行中…`,
      accepted: (code) => `已接取 ${code}`,
      acceptedAll: '已接取全部可接取任务',
      claimed: (code, credit) => `${code} 领取成功：+${credit} 额度`,
      checkinDone: (who) => `${who}：今日签到完成`,
      checkinRetry: (who) => `${who}：签到未生效（上游限流或设备风控），已排入自动重试`,
      opDone: (who, label) => `${who}：${label} 已完成`,
      batchCheckin: '全部签到已执行；未生效的会按退避自动重试',
      batchDone: (label) => `${label} 执行完成`,
      queueDone: '队列执行完成',
    },
    result: {
      none: '没有可执行的任务',
      summary: (total, done, skipped, failed, credit) =>
        `执行 ${total} 项：${done} 完成 / ${skipped} 跳过 / ${failed} 失败` +
        (credit ? `，共 +${credit} 额度` : ''),
      claimed: (credit) => `已领奖 +${credit}`,
    },
  },

  logs: {
    title: '运行日志',
    disabled:
      '请求日志未启用。在配置中开启 reqlog 后，这里会显示最近的网关请求：接口、账号、模型、状态、耗时与 Token 用量，但绝不包含提示词、回复内容或任何凭据。',
    metric: {
      inFlight: '处理中',
      completed: '已完成',
      successRate: '成功率',
      avgDuration: '平均耗时',
      avgTtfb: '平均首字节',
    },
    col: {
      time: '时间',
      path: '接口',
      account: '账号',
      model: '模型',
      route: '执行面',
      status: '状态',
      duration: '耗时',
      token: 'Token',
      credit: '积分',
    },
    empty: '暂无请求记录',
    stream: '流式',
    pending: '待结算',
    surface: {
      remote: '远程',
      ide: 'IDE',
      cli: 'CLI',
      cloud: '云端',
      unknown: '未知',
    },
    surfaceTitle: '执行面：本次请求实际走的通道。1M 档位只在「远程」生效',
    // The window is already in tokens; showing "981504K" would be a unit lie.
    maxBadge: (ctx) => (ctx >= 1000 ? `1M · ${Math.round(ctx / 1000)}k` : ctx ? `1M · ${ctx}` : '1M'),
    maxTitle: '已按 1M 档位富化：放大上下文窗口并提升推理强度',
    continueBadge: (n) => `续写 ${n} 轮`,
    continueTitle: '上游截断后自动续写，已合并为一次回复',
    creditSources: {
      upstream: '上游随响应返回',
      session_usage: '按回合查询上游用量',
      pending: '上游尚未结算；稍后自动补齐',
    },
    creditNone: '本次请求没有可结算的积分',
  },

  usage: {
    title: '用量管理',
    loadingAria: '正在读取用量',
    disabled:
      '用量历史未启用。在配置中设置 usage_store.enabled 后，这里会按日汇总请求数、Token 与积分，并保留可筛选的明细；提示词、回复内容与任何凭据都不会落库。',
    // Labels for the window selector. "All" is its own query (a zero window),
    // not a very large day count: the store keeps everything, and pretending a
    // ten-year preset is the whole history would lie the day the store is
    // older than that.
    days: (d) => (d === 1 ? '今天' : `近 ${d} 天`),
    daysAll: '全部',
    custom: '自定义',
    rangeFrom: '起始时间',
    rangeTo: '结束时间',
    windowAll: '全部历史',
    windowCustomEmpty: '自定义区间',
    windowNow: '现在',
    // The sticky bar states the window the numbers below cover.
    windowMeta: (d) => {
      if (d === 1) return '今天'
      if (d === 3650) return '全部历史'
      return `近 ${d} 天`
    },
    aria: { daily: '每日 Token 用量（输出 / 新鲜输入 / 缓存）', dailyCredits: '每日积分消耗' },
    band: { output: '输出', fresh: '输入（未命中）', cached: '输入（缓存命中）' },
    // The metric switch decides what the headline and the chart measure. The
    // two are the window's volume and its bill; neither stands in for the
    // other, so the switch moves both together instead of splitting them
    // across two screens.
    metric: { label: '口径', tokens: 'Token', credits: '积分' },
    sourceLabel: '来源',
    // The baseline is the equally long window before this one, which is only
    // ever "yesterday" for a one-day preset at midnight. The label says what
    // it is instead of borrowing the pool bar's phrasing.
    vsPrev: '较上一时段',
    chart: {
      expand: '展开',
      collapse: '收起',
      legendCredits: '积分',
      readout: '悬停查看单日数值',
    },
    fold: { daily: '每日汇总', records: '消费与请求明细', dailyCount: (n) => `${n} 天`, recordCount: (n) => `${n} 条` },
    stat: {
      requests: '请求',
      successRate: '成功率',
      total: '总 Token',
      credits: '积分',
      cacheHit: '缓存命中',
    },
    daily: { title: '每日 Token 趋势', peak: (tokens) => `峰值 ${tokens}` },
    dailyCol: {
      date: '日期',
      requests: '请求',
      ok: '成功',
      failed: '失败',
      input: '输入',
      output: '输出',
      cached: '缓存命中',
      credits: '积分',
    },
    // The empty chart keeps its outline (eight bars) and offers the windows
    // that most likely hold data, so "no requests" ends in an action.
    empty: '所选窗口内无请求。',
    emptyHint: '换个窗口试试：',
    breakdown: {
      title: '用量分布',
      hint: (n) => `${n} 组`,
      empty: '这段时间还没有可聚合的记录。',
      sourceFilter: '按平台筛选',
      allSources: '全部平台',
      top: (n) => `Top ${n}`,
      showAll: '显示全部',
      showLess: '收起列表',
      // The right-hand panel answers "where did the window's credits go",
      // which the token table deliberately leaves out: tokens measure volume
      // and credits measure the bill, and one table showing both invites
      // adding them together.
      spend: {
        title: '额度消耗',
        hint: '窗口内消耗的积分',
        bySource: '按来源',
        byAccount: '按账号',
        other: '其他',
        empty: '这段时间没有积分消耗记录。',
      },
      col: {
        name: { account: '账号', model: '模型', wire: '接口' },
        requests: '请求',
        failed: '失败',
        input: '输入',
        output: '输出',
        total: '总 Token',
        cached: '缓存命中',
        credits: '积分',
        avg: '均耗时',
      },
    },
    dim: { account: '按账号', model: '按模型', wire: '按接口' },
    credit: {
      total: '消耗积分',
      perDay: '每天',
      perRequest: '每请求',
      perMillion: '每 M Token',
      models: '模型数',
    },
    records: {
      title: '消费与请求明细',
      empty: '没有匹配的记录',
      sourceAll: '全部平台',
      wireAll: '全部接口',
      modelPlaceholder: '模型筛选',
      outcomeAll: '全部结果',
      outcomeSuccess: '成功',
      outcomeHTTPError: 'HTTP 错误',
      // Filters collapse into one chip row once the table is folded behind its
      // 48px header: four selects in a row would outweigh the row that opens
      // them. The page-level source select above already covers "which
      // platform", so the record filters keep only what the page filter cannot
      // say — wire and outcome — plus a model search.
      loaded: (n) => `已加载 ${n} 条`,
      end: '已到最早一条',
      capped: (n) => `已加载全部 ${n} 条，再早的记录请缩小时间窗口`,
      // Every filter in this row carries an accessible name: four unlabelled
      // selects in a row read as "combo box" four times.
      aria: {
        source: '按平台筛选记录',
        wire: '按接口筛选记录',
        model: '按模型筛选记录',
        outcome: '按结果筛选记录',
      },
    },
  },

  overview: {
    // The top bar's meta slot. Zero is a fact worth stating, not an empty slot:
    // "0 个账号" explains why the ledger below is empty.
    meta: (n) => `${n} 个账号`,
    ledger: '账号台账',
    // The band's identity row pairs a 12px label with a 13px value: what it
    // listens on, and which upstreams answer behind it.
    listenLabel: '监听',
    upstreamLabel: '上游',
    copyEndpoint: '复制接口地址',
    copyShort: '复制',
    copied: '已复制',
    stat: {
      usable: '有效账号',
      cooling: '冷却中',
      disabled: '已禁用',
      paused: '已暂停',
      credits: '积分余量',
      tokensToday: '今日 Token',
    },
    // One line that answers "is anything wrong" without opening the panels.
    // Every count comes from data the folded panels would have shown.
    diag: {
      title: '诊断',
      expiry: (n) => `积分到期 ${n} 个账号`,
      serviceOk: '服务正常',
      serviceMissing: (n) => `服务 ${n} 项未读到`,
      locks: (n) => `模型锁定 ${n} 个模型`,
    },
    expiryBox: {
      title: '积分到期提醒',
      window: (h) => `未来 ${h} 小时内`,
      empty: '暂无即将过期的积分（以最近一次额度刷新为准）。',
      stale: '尚未读取过逐包数据；点「刷新」拉取上游后这里会显示到期分布。',
      expiring: (n, uid) => `${uid}：${n} 即将过期`,
      earliest: (text, remain) => `${text} 到期 ${remain} 积分`,
      check: '刷新',
      checking: '读取中…',
    },
    serviceBox: {
      title: '服务状态',
      version: '服务版本',
      uptime: '运行时长',
      remote: '监听地址',
      sources: '已注册上游',
      inFlight: '在途请求',
      sticky: '粘性会话',
      reqlog: '请求日志',
      usage: '用量历史',
      checkin: '自动签到',
      max: '1M 档位',
      on: '开启',
      off: '关闭',
    },
  },

  models: {
    meta: (n) => (n > 0 ? `${n} 个模型` : '未读取目录'),
    // The right-hand summary of a platform card: how many accounts of that
    // platform answered with a catalogue.
    accounts: (n) => `${n} 个账号`,
    search: '搜索模型…',
    searchAria: '搜索模型',
    filterAll: '全部能力',
    // Sorting is stated on the two columns where it means something: a
    // context window and a rate are the two numbers a caller compares models
    // by. The header button carries its own accessible state via aria-sort.
    sortContext: '按上下文排序',
    sortRate: '按积分倍率排序',
    filters: {
      reasoning: '思考',
      vision: '视觉',
      tools: '工具',
    },
    col: {
      model: '模型',
      context: '上下文',
      efforts: '思考档位',
      caps: '能力',
      maxOutput: '最大输出',
      rate: '积分倍率',
    },
    cap: { chat: '对话', reasoning: '思考', vision: '视觉', max: '1M', tools: '工具' },
    defaultMark: '默认',
    effortsOff: '不可调',
    empty: '还没有读到任何模型目录。',
    emptyHint: '目录在后台按账号刷新，也可以点「重新获取」立即拉取；未读取时不影响模型名。',
    emptyFiltered: '没有匹配的模型。',
    reload: '重新获取',
    clearFilter: '清除筛选',
    failed: (uid, err) => `${uid}：${err}`,
    test: {
      title: '模型连通性测试',
      // The head states the price of asking before the button is reachable.
      // With no catalogue there is nothing to ask yet, so the head says that
      // instead — a cost line over an empty form reads as a taunt.
      hint: '走真实链路发一次最小请求，会消耗少量积分',
      hintEmpty: '读取目录并选择模型后可发起测试',
      // The form reads its model from the catalogue above instead of a typed
      // list: a name no account carries cannot be tested, and the capabilities
      // the selector already knows are what gate the content below it.
      modelLabel: '测试模型',
      modelPlaceholder: '搜索或选择模型',
      pickerAria: '选择测试模型',
      pickerSearch: '搜索模型或平台',
      pickerEmpty: '没有匹配的模型',
      groupCount: (label, n) => `${label} · ${n}`,
      contentLabel: '测试内容',
      modeText: '文本回复',
      modeTool: '工具调用',
      thinking: '返回思考内容',
      effortLabel: '思考强度',
      effortDefault: '默认',
      timeoutLabel: '超时',
      timeoutCustom: '自定义',
      run: '开始测试',
      running: '测试中…',
      // A grayed control says why it is grayed; without the reason a gated
      // option reads as broken instead of as unsupported by this model.
      modeTextOff: '该模型不支持对话',
      modeToolOff: '该模型不支持工具调用',
      thinkingOff: '该模型不支持思考',
      effortOff: '该模型的思考档位不可调',
      needModel: '请先选择测试模型',
      noMode: '请至少勾选一种测试内容',
      // The result row states what was tested, not just that something was:
      // which model, which content, how long, what it cost, when, and the way
      // in to the raw exchange.
      resultEmpty: '尚未测试',
      resultEmptyHint: '读取目录后可选择模型进行测试',
      runningWait: '请求已发出，等待上游响应…',
      runningStep: (label, i, n) => `正在测试：${label}（${i}/${n}）`,
      timeoutIn: (n) => `超时将于 ${n} 秒后触发`,
      pass: '通过',
      fail: '失败',
      testing: '测试中',
      detail: '查看详情',
      retry: '重试',
      creditUnit: '积分',
      elapsed: (ms) => `${(ms / 1000).toFixed(1)}s`,
      // The probe is one non-streaming call per content, so the bound covers
      // the whole answer, not just its first byte.
      timeoutReason: (n) => `未在 ${n} 秒内收到响应`,
      drawer: {
        title: '测试详情',
        request: '请求',
        response: '响应',
        model: '模型',
        mode: '测试内容',
        effort: '思考强度',
        thinking: '返回思考内容',
        thinkingOn: '已开启',
        thinkingOff: '未开启',
        timeout: '超时',
        account: '账号',
        prompt: '提示词',
        reply: '回复内容',
        reasoning: '思考内容',
        toolCalls: '工具调用',
        finish: '停止原因',
        elapsed: '耗时',
        tokens: 'Token 用量',
        credits: '消耗积分',
        status: 'HTTP 状态',
        error: '错误',
        noContent: '（无回复内容）',
        stepOf: (i, n) => `第 ${i}/${n} 步`,
      },
    },
  },

  credits: {
    loadingAria: '正在读取额度',
    // The verb is 刷新 everywhere a balance is re-read (附录 A.8). The read
    // time lives in the breadcrumb, not in the band: stating it twice put the
    // same clock on one screen two ways.
    check: '刷新',
    refreshing: '刷新中…',
    probing: '正在读取逐包明细…（逐账号查询上游）',
    notRead: '余额未读取',
    notReadCredit: '尚未读取余额',
    perSource: '按来源计量',
    perSourceUnread: '尚未读取上游余额',
    // Pack-level, like the lapse chart below it: an account without packs
    // cannot say how much lapses, only when its first batch does.
    soon: '7 天内到期',
    cardsEmpty: '添加账号并刷新额度后显示余额构成。',
    packsEmptyTitle: '上游未提供逐包明细，只有汇总余额。',
    packsEmptyHint: '刷新额度后查看上游返回的额度、已用、剩余与到期时间。',
    empty: '还没有账号。',
    unnamedPack: '未命名积分包',
    readAt: (time) => `余额数据 ${time}`,
    expiry: {
      title: '积分到期',
      hint: '按剩余天数分桶 · 同色为同一来源，越浅越远',
      empty: '上游未给出逐包到期时间，无法画分布。',
      expired: '已到期',
      days: (n) => `${n} 天`,
      far: '15 天以上',
      farHint: (n) => `${n} 个到期日合计`,
      noDate: '无到期时间',
      undated: (n, amount) => `${n} 个包无到期时间 · 共 ${amount} 积分，不计入分布`,
      more: (n) => `更多 ${n} 项`,
      less: '收起',
    },
    cards: {
      title: '账号余额构成',
      used: (v) => `已用 ${v}`,
      total: (v) => `共 ${v}`,
      self: (n) => `×${n}`,
      since: (d) => `首发 ${d}`,
      recent: (date, rel) => `最近到期 ${date} · ${rel}`,
      more: (n) => `其他 ${n} 项`,
    },
    packs: {
      title: '逐账号逐包明细',
      hint: '按来源分组 · 默认展示最近到期 5 个，其余就地展开',
      count: (n) => (n > 0 ? `${n} 个包` : '无逐包'),
      accounts: (n) => `${n} 个账号`,
      sortAria: '逐包排序',
      sortEnd: '到期升序',
      sortSize: '面额降序',
      foldRest: (n, size, remain) => `其余未用完 ${n} 个包（面额合计 ${size} · 剩余 ${remain}），展开`,
      foldUsed: (n, size) => `已用完 ${n} 个包（面额合计 ${size}），展开`,
      foldClose: '收起',
      col: {
        name: '包',
        size: '面额',
        remain: '剩余',
        used: '已用',
        created: '发放',
        expires: '到期',
      },
      empty: '上游未提供逐包明细，只有汇总余额。',
    },
    failed: '读取失败',
    unlimited: '不限量',
    local: '本机 CLI',
    localNote: '本机 CLI 无独立额度',
  },

  add: {
    // The dialog's three tabs. This key used to sit in a second `add` object
    // that the later definition silently overwrote, so opening the dialog threw
    // before it could render.
    tab: { device: '设备授权', token: '粘贴令牌', batch: '批量导入' },
    // A source value is a stable identifier (`trae-cn`), not a name. Wherever
    // the dialog shows one to a person it shows this instead.
    sourceLabel: { workbuddy: 'WorkBuddy', 'trae-cn': 'Trae CN' },
    mode: { device: '设备授权登录', manual: '粘贴令牌', local: '从本机导入' },
    local: {
      title: '从本机导入',
      body: '读取本机已登录客户端的会话，直接导入账号，无需复制令牌。这是一次显式操作：仅在你点击后读取，不会在后台扫描。',
      source: '来源',
      button: '从本机导入账号',
      running: '读取中…',
      imported: (n, list) => `已导入 ${n} 个账号：${list}`,
      none: '本机未找到可导入的账号',
    },
    device: {
      title: '设备授权登录',
      body: '选择来源与区域后开始授权，浏览器会打开登录页；完成后此页面自动接收账号，无需手工粘贴令牌。',
      source: '来源',
      realm: '区域',
      realmCn: '中国大陆',
      realmGlobal: '国际',
      start: '开始授权',
      starting: '正在申请授权码…',
      waiting: '等待浏览器完成授权…',
      openLogin: '打开登录页',
      added: (label) => `已添加：${label}`,
      again: '再添加一个',
    },
    token: {
      title: '粘贴令牌',
      body: '直接从已登录的客户端复制令牌粘贴到此。令牌是本机凭据，仅保存在账号目录中，不会上传。',
      source: '来源',
      label: '令牌',
      labelWithHint: (hint) => `令牌（${hint}）`,
      refresh: '刷新令牌（可选）',
      uid: '用户 ID（可选）',
      note: '备注（可选）',
      notePlaceholder: '例如 主力号',
      placeholder: '访问令牌',
      optional: {
        label: (n) => `可选字段 · ${n} 项`,
        items: '刷新令牌 · 用户 ID · 备注',
      },
      submit: '添加账号',
      submitting: '添加中…',
      needToken: '请粘贴令牌，或填写刷新令牌',
      added: (label) => `已添加：${label}`,
    },
  },

  importAcc: {
    title: '导入账号',
    body: '粘贴导出的账号 JSON。数组，或形如 {accounts: [...]} 的对象都可以。',
    inlineLabel: '账号 JSON',
    submit: '导入',
    running: '导入中…',
    needJSON: '请粘贴账号 JSON',
    jsonBad: (err) => `JSON 解析失败：${err}`,
    done: (imported, total) => `导入 ${imported}/${total} 个账号`,
  },

  // The process log dialog. Separate from `logs` above, which is the request
  // table on the usage page: that one answers "what did each call cost", this
  // one answers "what is the gateway doing right now".
  logStream: {
    live: '实时',
    offline: '连接中断，正在重连',
    reconnect: '立即重试',
    retryHint: '丢弃当前连接并重新订阅日志流',
    pinOn: '自动滚动',
    pinOff: '已暂停',
    pinHint: '停止自动滚动，方便阅读正在刷新的日志',
    download: '下载',
    downloadHint: '导出当前频道已加载的日志',
    jump: '回到最新',
    empty: '暂无日志。',
    // A log line is one fixed row; the full text lives in the inspector strip
    // so a stack trace never has to be read by hovering.
    inspectHint: '点击任意行，在此查看完整内容',
    inspectClear: '关闭检视',
    pausedAria: (n) => (n ? `已暂停自动滚动，${n} 条新日志` : '已暂停自动滚动'),
    resumedAria: (n) => (n ? `已恢复自动滚动，${n} 条新日志` : '已恢复自动滚动'),
    ch: { all: '全部', task: '任务', chat: '对话', sys: '系统' },
    // Level sits beside the channel because the two answer different questions:
    // the channel says who is talking, the level says whether it needs you.
    level: { all: '全部级别', warn: '警告', error: '错误', aria: '按级别筛选日志' },
    // The stream is virtualised, so the browser's own find cannot reach a row
    // that was never rendered. This box is the only way to search it.
    search: { placeholder: '搜索日志…', aria: '搜索日志内容', clear: '清除日志搜索' },
    lines: (n) => `${n} 行`,
  },

  footer: '本控制台仅从本机回环地址使用；远程访问需在请求中携带网关 api_key。',

  settings: {
    loadingAria: '正在加载配置',
    sectionCount: (n) => `${n} 个分区`,
    search: {
      placeholder: '搜索配置项',
      aria: '搜索设置项（名称、说明或配置路径）',
      clear: '清除搜索',
      hits: (n) => `命中 ${n} 项`,
      none: '没有匹配的设置项，换个词试试。',
      // Searching flattens every section into one result list; the nav then
      // works as a facet rather than a destination.
      resultTitle: '搜索结果',
      inSection: (n) => `在「${n}」中`,
      facetClear: '查看全部分区的结果',
      facetHint: '选择分区可把结果收窄到该分区',
    },
    // One line under each section title. Ninety-five fields across seven
    // sections is only navigable if the section says what it owns before you
    // read a single label.
    section: {
      aria: '设置分区导航',
      desc: {
        服务: '监听地址、密钥与进程级行为。这里多数改动要重启后生效。',
        定时任务: '签到、成长任务与保活的执行节奏与并发。',
        账号池与流量治理: '账号选择顺序、并发上限、冷却与失败处理。',
        上游: '两个上游的连接地址、凭据与协议参数。',
        出站改写: '转发前对请求与响应的改写规则。',
        安全: '控制台访问控制与凭据的落盘方式。',
        日志与可观测: '日志级别、留存与实时流。',
      },
    },
    group: {
      collapse: '收起',
      expand: '展开',
      advanced: (n) => `高级项 ${n}`,
      advancedHide: '收起高级项',
    },
    platform: { workbuddy: 'WorkBuddy', trae: 'Trae' },
    saved: '已保存。',
    savedRestart: (a, b) => `已保存 ${a} 项 · ${b} 项需重启后生效`,
    restartNow: '立即重启',
    noAutoRestart: '当前运行方式不支持自动重启，请手动重启进程。',
    restartConfirm: '重启网关？当前连接会短暂中断。',
    confirm: {
      title: '确认保存',
      body: '以下字段会影响服务可达性或数据存储位置：',
      save: '确认保存',
      count: (n) => `其中 ${n} 项需要二次确认`,
    },
    undo: {
      label: '撤销最近一次保存',
      title: '撤销最近一次保存',
      confirm: (t) => `把配置回退到 ${t} 的版本？当前配置会被覆盖。`,
      none: '还没有可撤销的保存记录。',
      hint: '每次保存前自动留一份快照（最多 20 份）。误改了监听地址、数据目录或加密开关时，用它可以退回上一步。',
    },
    field: { restart: '需要重启', danger: '谨慎' },
    save: '保存',
    saving: '保存中…',
    saveCount: (n) => `保存 ${n} 项改动`,
    discard: '放弃改动',
    hoursAria: (label) => `${label} 整点`,
    listHint: '逗号或换行分隔',
    secretShow: '显示',
    secretHide: '隐藏',
    err: {
      request: (status) => `请求失败（${status}）`,
      save: (status) => `保存失败（${status}）`,
      rollback: (status) => `回滚失败（${status}）`,
      restart: (status) => `重启失败（${status}）`,
    },
  },
}

// labels maps a backend enum to display copy where one exists.
export const labels = {
  outcome: {
    success: '成功',
    http_error: 'HTTP 错误',
    stream_error: '流中断',
    interrupted: '已中断',
  },
}
