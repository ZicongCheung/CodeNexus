// Package configschema describes the configuration fields the console edits.
//
// The schema is the contract between the config file and the settings form: it
// says what a field is called, what values it accepts, where it belongs on the
// page, and — the part users actually feel — whether saving it takes effect now
// or needs a restart.
//
// Every field the console can reach lives here. There is no raw JSON escape
// hatch any more, so a setting absent from this catalog is a setting the
// operator cannot edit from the console at all.
package configschema

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Section names, in the order the settings page draws them. The console reads
// the order off the field list rather than this block; the constants exist so
// the same string is never spelled twice.
const (
	SectionService  = "服务"
	SectionSchedule = "定时任务"
	SectionPool     = "账号池与流量治理"
	SectionUpstream = "上游"
	SectionFeature  = "出站改写"
	SectionObserv   = "日志与可观测"
	SectionSecurity = "安全"
)

// Blocks split a section into labelled groups. Chiefly 上游 uses one per
// platform: the two sources share nothing but the heading, and mixing them
// makes it impossible to tell which box a timeout belongs to.
const (
	BlockPool        = "账号池"
	BlockRouter      = "会话与模型路由"
	BlockUpCommon    = "通用"
	BlockUpWorkBuddy = "WorkBuddy 上游"
	BlockUpTraeCN    = "Trae CN 上游"
)

// Platform tags a field with the source it governs, so the form can badge a
// setting instead of leaving the reader to guess whether "签到整点" also moves
// the Trae schedule.
const (
	PlatformAny       = ""
	PlatformWorkBuddy = "workbuddy"
	PlatformTraeCN    = "trae-cn"
	PlatformBoth      = "both"
)

// Task names the scheduler task a 定时任务 field belongs to. Grouping by task
// rather than by platform is deliberate: people look for "签到", not for "the
// WorkBuddy one of the two check-ins".
const (
	TaskCommon  = "通用"
	TaskCheckin = "签到"
	TaskGrowth  = "成长任务"
	TaskActive  = "活跃度"
	TaskTravel  = "行程"
	TaskKeep    = "保活"
	TaskNight   = "黑猫"
	TaskBalance = "余额刷新"
)

// Restart lists the fields that cannot be applied to a running process. They
// are structural: changing them means moving storage, rebinding the listener
// or re-sealing credentials, none of which can be done safely in place.
var Restart = []string{
	"listen",
	"api_key",
	"data_dir",
	"auth_dir",
	"security.key_provider",
	"security.encrypt_credentials",
	// An open database file cannot be relocated under the process that holds
	// it; the new path only takes effect once the store is reopened at boot.
	"usage_store.path",
	// The responses cache is created with the server; re-bounding it in place
	// would silently drop every session a client is mid-conversation on.
	"responses.ttl_seconds",
	"responses.max_entries",
	"responses.max_bytes",
	"responses.max_messages",
}

// Field describes one editable setting.
type Field struct {
	// Path is the dotted location in the config file.
	Path string `json:"path"`
	// Group is the section the field renders under.
	Group string `json:"group"`
	// Block is an optional sub-heading inside the section.
	Block string `json:"block,omitempty"`
	// Task is the scheduler task a 定时任务 field belongs to.
	Task string `json:"task,omitempty"`
	// Platform names the source the field governs, for the badge.
	Platform string `json:"platform,omitempty"`
	// Label is the short name shown next to the control.
	Label string `json:"label"`
	// Help explains what the setting does and what happens if it is wrong.
	Help string `json:"help"`
	// Kind drives the control: bool, int, float, string, enum, hours, secret,
	// list.
	Kind string `json:"kind"`
	// Options lists the allowed values for an enum.
	Options []string `json:"options,omitempty"`
	// Min and Max bound numeric fields. Zero means unbounded.
	Min float64 `json:"min,omitempty"`
	Max float64 `json:"max,omitempty"`
	// Hot is false when saving the field needs a process restart.
	Hot bool `json:"hot"`
	// Danger marks a field that can lock the operator out or drop data, so the
	// console asks for a second confirmation before saving it.
	Danger bool `json:"danger,omitempty"`
	// Advanced keeps a tuning or identity parameter behind the section's
	// disclosure. These are the settings whose defaults are the recommendation.
	Advanced bool `json:"advanced,omitempty"`
	// Primary marks the one switch that turns its task (or source) on. The
	// console lifts it into the group header; a list of loose booleans leaves
	// the reader to work out which one is the master switch.
	Primary bool `json:"primary,omitempty"`
	// Placeholder is the sample text shown in an empty text control.
	Placeholder string `json:"placeholder,omitempty"`
}

// NeedsRestart reports whether changing this path requires a restart.
func NeedsRestart(path string) bool {
	for _, p := range Restart {
		if p == path {
			return true
		}
	}
	return false
}

func hot(path, group, label, help, kind string, extra ...func(*Field)) Field {
	f := Field{Path: path, Group: group, Label: label, Help: help, Kind: kind, Hot: true}
	for _, fn := range extra {
		fn(&f)
	}
	return f
}

func cold(path, group, label, help, kind string, extra ...func(*Field)) Field {
	f := Field{Path: path, Group: group, Label: label, Help: help, Kind: kind, Hot: false}
	for _, fn := range extra {
		fn(&f)
	}
	return f
}

func options(values ...string) func(*Field) {
	return func(f *Field) { f.Options = values }
}

func placeholder(v string) func(*Field) {
	return func(f *Field) { f.Placeholder = v }
}

func danger(f *Field) { f.Danger = true }

func advanced(f *Field) { f.Advanced = true }

func primary(f *Field) { f.Primary = true }

func block(name string) func(*Field) {
	return func(f *Field) { f.Block = name }
}

func platform(name string) func(*Field) {
	return func(f *Field) { f.Platform = name }
}

func task(name string) func(*Field) {
	return func(f *Field) { f.Task = name }
}

// Fields is the catalog the settings form renders. Order is display order
// within a section, and a section's own order follows its first field.
func Fields() []Field {
	// The three constructors below exist so a Trae field cannot be added
	// without its platform block (and vice versa) — the block is part of what
	// the field *is*, not an afterthought at the call site.
	sched := func(path, label, help, kind string, extra ...func(*Field)) Field {
		return hot(path, SectionSchedule, label, help, kind, extra...)
	}
	wb := func(path, label, help, kind string, extra ...func(*Field)) Field {
		args := append([]func(*Field){block(BlockUpWorkBuddy), platform(PlatformWorkBuddy)}, extra...)
		return hot(path, SectionUpstream, label, help, kind, args...)
	}
	trae := func(path, label, help, kind string, extra ...func(*Field)) Field {
		args := append([]func(*Field){block(BlockUpTraeCN), platform(PlatformTraeCN)}, extra...)
		return hot(path, SectionUpstream, label, help, kind, args...)
	}

	all := []Field{
		// ── 服务 ────────────────────────────────────────────────────────────
		hot("log_level", SectionService, "日志级别", "写入标准输出的详细程度。改为 debug 会打印上游事件，排错时有用，平时留 info。", "enum",
			options("error", "warn", "info", "debug")),
		cold("listen", SectionService, "监听地址", "HTTP 服务绑定的地址。改错会让服务起不来，需重启后生效。", "string",
			danger, placeholder("127.0.0.1:6828")),
		cold("api_key", SectionService, "API 密钥", "客户端访问网关的密钥。修改后旧客户端立即失效，需重启后生效并同步更新客户端。", "secret",
			danger, placeholder("留空表示自动生成新的随机密钥")),
		cold("data_dir", SectionService, "数据目录", "状态文件、用量库与配置快照的存放位置。需重启后生效。", "string", danger),
		cold("auth_dir", SectionService, "凭证目录", "账号凭证文件的存放位置。需重启后生效。", "string", danger),

		// ── 定时任务：通用 ──────────────────────────────────────────────────
		sched("schedule.timezone", "时区", "日界与整点任务的时区（IANA 名称）。上游按北京时间结算，默认与之对齐。", "string",
			task(TaskCommon), placeholder("Asia/Shanghai")),
		sched("schedule.catch_up_on_start", "启动时补跑", "进程在当天整点之后启动时补跑一次维护任务，避免重启吃掉一整天。", "bool",
			task(TaskCommon)),
		sched("schedule.account_delay_ms", "账号间隔毫秒", "一轮扫描中相邻账号的请求间隔，避免打爆上游。", "int",
			task(TaskCommon)),
		sched("schedule.include_disabled_in_tasks", "任务包含停用账号", "维护任务是否也作用于被停用的账号（仍不承载流量）。", "bool",
			task(TaskCommon)),
		sched("schedule.expiring_soon_hours", "临期积分窗口小时", "在此窗口内过期的积分会被优先消耗。0 关闭该信号。", "int",
			task(TaskCommon), advanced),

		// ── 定时任务：签到（两个来源都有） ─────────────────────────────────
		sched("schedule.checkin_enabled", "启用签到", "每日按整点执行签到。关掉后两个来源都不再签到。", "bool",
			task(TaskCheckin), platform(PlatformBoth), primary),
		sched("schedule.checkin_hours", "签到整点", "一天中执行签到的小时（时区见「通用」）。可多选，多选即一天多次。", "hours",
			task(TaskCheckin), platform(PlatformBoth)),
		sched("schedule.checkin_retry_enabled", "签到重试", "为当天未完成签到的账号补跑签到。", "bool",
			task(TaskCheckin), platform(PlatformBoth), advanced),
		sched("schedule.checkin_retry_seconds", "签到重试间隔秒数", "两次补跑之间的间隔。", "int",
			task(TaskCheckin), platform(PlatformBoth), advanced),

		// ── 定时任务：成长任务 ─────────────────────────────────────────────
		sched("schedule.growth_enabled", "启用成长任务", "每日扫取并完成成长任务。", "bool",
			task(TaskGrowth), platform(PlatformWorkBuddy), primary),
		sched("schedule.growth_hours", "成长任务整点", "一天中执行成长任务扫描的小时。", "hours",
			task(TaskGrowth), platform(PlatformWorkBuddy)),

		// ── 定时任务：活跃度 ───────────────────────────────────────────────
		sched("schedule.activity_enabled", "启用活跃度任务", "每日完成活跃度任务。", "bool",
			task(TaskActive), platform(PlatformWorkBuddy), primary),
		sched("schedule.activity_hours", "活跃度整点", "一天中执行活跃度任务的小时。", "hours",
			task(TaskActive), platform(PlatformWorkBuddy)),

		// ── 定时任务：行程 ─────────────────────────────────────────────────
		sched("schedule.travel_enabled", "启用行程任务", "每日完成行程任务。", "bool",
			task(TaskTravel), platform(PlatformWorkBuddy), primary),
		sched("schedule.travel_hours", "行程整点", "一天中执行行程任务的小时。", "hours",
			task(TaskTravel), platform(PlatformWorkBuddy)),

		// ── 定时任务：黑猫 ─────────────────────────────────────────────────
		sched("schedule.blackcat_enabled", "启用黑猫任务", "每日完成黑猫（夜猫）任务。", "bool",
			task(TaskNight), platform(PlatformWorkBuddy), primary),
		sched("schedule.blackcat_hours", "黑猫整点", "一天中执行黑猫任务的小时。", "hours",
			task(TaskNight), platform(PlatformWorkBuddy)),

		// ── 定时任务：保活 ─────────────────────────────────────────────────
		sched("schedule.keepalive_enabled", "启用保活任务", "每日刷新账号凭证，避免闲置账号的令牌过期。两个来源都适用。", "bool",
			task(TaskKeep), platform(PlatformBoth), primary),
		sched("schedule.keepalive_hours", "保活整点", "一天中执行账号保活的小时。", "hours",
			task(TaskKeep), platform(PlatformBoth)),

		// ── 定时任务：余额刷新 ─────────────────────────────────────────────
		sched("schedule.balance_refresh_enabled", "启用余额刷新", "按短间隔刷新积分余额，不依赖整点任务。两个来源都有余额端点。", "bool",
			task(TaskBalance), platform(PlatformBoth), primary),
		sched("schedule.balance_refresh_minutes", "余额刷新间隔分钟", "刷新的间隔分钟数。", "int",
			task(TaskBalance), platform(PlatformBoth)),

		// ── 账号池与流量治理：账号池 ───────────────────────────────────────
		hot("pool.max_in_flight", SectionPool, "单账号并发上限", "同一个账号同时承载的请求数。上游限流严格时调低。", "int",
			block(BlockPool)),
		hot("pool.credit_floor", SectionPool, "积分为零阈值", "积分低于此值的账号不参与选号。", "int", block(BlockPool)),
		hot("pool.cooldown_seconds", SectionPool, "冷却起始秒数", "账号出错后的首次冷却时长，失败次数越多递增。", "int", block(BlockPool)),
		hot("pool.cooldown_max_seconds", SectionPool, "冷却上限秒数", "冷却时长的天花板，避免长时间连续失败把账号封死。", "int", block(BlockPool)),
		hot("pool.breaker_threshold", SectionPool, "熔断失败次数", "连续失败达此次数后熔断该账号。", "int", block(BlockPool)),
		hot("pool.breaker_seconds", SectionPool, "熔断时长秒数", "熔断后隔离该账号的时长。", "int", block(BlockPool)),
		hot("pool.failover_attempts", SectionPool, "故障转移次数", "一次请求最多换几个账号重试。", "int", block(BlockPool)),
		hot("pool.wait_max_seconds", SectionPool, "排队等待上限秒数", "无可用账号时排队等待的时长，超时返回 429。", "int", block(BlockPool)),
		hot("pool.weight_credit", SectionPool, "权重·积分", "选号时积分余额的权重。", "float", block(BlockPool), advanced),
		hot("pool.weight_idle", SectionPool, "权重·空闲", "选号时空闲时长的权重。", "float", block(BlockPool), advanced),
		hot("pool.weight_success", SectionPool, "权重·成功率", "选号时历史成功率的权重。", "float", block(BlockPool), advanced),
		hot("pool.weight_expiring", SectionPool, "权重·临期积分", "选号时对即将过期积分的倾斜，越大越优先消耗临期账号。", "float",
			block(BlockPool), advanced),

		// ── 账号池与流量治理：会话与路由 ───────────────────────────────────
		hot("session_sticky.enabled", SectionPool, "会话粘滞", "把同一会话固定到一个账号，减少上下文切换。", "bool",
			block(BlockRouter)),
		hot("session_sticky.ttl_seconds", SectionPool, "粘滞有效期秒数", "绑定保持多久后释放。", "int", block(BlockRouter)),
		hot("router.naming", SectionPool, "模型命名", "flat 暴露裸模型名并跨来源解析；prefixed 只暴露 来源/模型。", "enum",
			block(BlockRouter), options("flat", "prefixed")),
		hot("router.same_name_policy", SectionPool, "同名策略", "多个来源提供同名模型时：auto_healthy 取最健康的账号，first 固定第一个来源。", "enum",
			block(BlockRouter), options("auto_healthy", "first")),
		hot("router.allow_prefix", SectionPool, "允许前缀写法", "即使命名是 flat，也允许客户端用 来源/模型 显式指定来源。", "bool",
			block(BlockRouter)),

		// ── 上游：通用 ─────────────────────────────────────────────────────
		hot("stream.stall_seconds", SectionUpstream, "流式看门狗秒数",
			"上游保持连接却长时间不产出内容（例如只发心跳）时，判定这次尝试卡死并换下一个账号，避免用户干等到整体超时才收到 504。只统计正文/思考/工具调用，心跳不算进度。默认 90 秒；填负数关闭。对两个来源都生效。",
			"int", block(BlockUpCommon)),

		// ── 上游：WorkBuddy ────────────────────────────────────────────────
		wb("upstreams.workbuddy.enabled", "启用该来源", "是否注册 WorkBuddy / CodeBuddy 来源。关闭后该来源的请求立即失败。", "bool", primary),
		wb("upstreams.workbuddy.timeout_seconds", "请求超时秒数", "该来源单个请求的超时时间。", "int"),
		wb("upstreams.workbuddy.base_url", "接口基址", "上游主机地址。默认指向官方端点；只有自建代理时才需要改。", "string",
			danger, advanced),
		wb("upstreams.workbuddy.user_agent", "User-Agent", "出站请求的 UA。留空沿用适配器内置值。", "string", advanced),

		// ── 上游：Trae CN ──────────────────────────────────────────────────
		trae("upstreams.trae-cn.enabled", "启用该来源", "关闭后不再注册该来源，已有请求会失败。", "bool", danger, primary),
		trae("upstreams.trae-cn.upstream_mode", "执行面",
			"remote 走网页面；ide 走模型网关（原生工具调用）；auto-route 按是否有工具自动分派；cli 走本机 Trae CLI。",
			"enum", options("auto-route", "remote", "solo", "ide", "work-agent", "cli")),
		trae("upstreams.trae-cn.auto_route", "自动分派", "auto-route 模式下：带工具的请求走 Agent 面，纯对话走 Remote 面。", "bool"),
		trae("upstreams.trae-cn.timeout_seconds", "请求超时秒数", "单个上游请求的超时时间。", "int"),
		trae("upstreams.trae-cn.max_tokens", "默认最大输出", "客户端未指定 max_tokens 时使用的上限。", "int"),
		trae("upstreams.trae-cn.base_url", "模型网关基址", "IDE Agent / Solo 端点的主机地址。改错会导致请求失败。", "string", danger),
		trae("upstreams.trae-cn.remote_base_url", "网页面基址", "Remote 对话端点（chat_sessions + events）的主机地址。", "string", danger),
		trae("upstreams.trae-cn.checkin.enabled", "参与每日签到",
			"关闭后 Trae 账号不参与「定时任务 · 签到」，但仍会正常刷新余额。签到的时间点由定时任务统一决定，这里只决定「参不参加」。",
			"bool"),
		trae("upstreams.trae-cn.max_mode.enabled", "1M 档位",
			"对账号目录里标记为 max 的模型改用该账号最大的上下文档位。只对不带工具定义的请求生效：这类请求走 Remote 面，上游在那条链路上认档位字段；带 tools 的请求走 agent 面，上游那条链路不转发档位字段，所以不会生效——这是上游的限制，不是开关坏了。窗口取自账号自己的目录，同一档位下各模型并不相同（实测 glm-5.3 为 100 万、mimo-v2.6-flash 为 98.1 万）。生效时日志输出 max profile model=… context=… ，面板请求日志会带 max= 标记。",
			"bool"),
		trae("upstreams.trae-cn.max_mode.models", "1M 档位模型白名单",
			"只对这些模型尝试 1M 档位，逗号或换行分隔。留空表示所有被账号目录标记为 max 的模型都适用。", "list", advanced),
		trae("upstreams.trae-cn.auto_continue.enabled", "截断自动续写",
			"上游把回答截断时，自动追加一轮把答案补完，用户仍只看到一条回答。只在回答看起来被截断时才追加；答案看起来完整时不会多跑。每一轮都真实消耗积分，每轮都会在日志里输出 auto-continue round= 。",
			"bool"),
		trae("upstreams.trae-cn.auto_continue.max_rounds", "续写最大轮数", "单次请求最多追加几轮，用于封顶额外消耗。", "int", advanced),
		trae("upstreams.trae-cn.auto_continue.text_threshold", "碎片判定阈值",
			"「思考很长但正文只有碎片」的判定阈值（字符）。调大更容易触发续写。", "int", advanced),
		trae("upstreams.trae-cn.auto_continue.similarity_threshold", "重复停止阈值",
			"续写结果与上一轮的重合度超过此值时停止，避免无限循环（0~1）。", "float", advanced),
		trae("upstreams.trae-cn.debug_upstream", "调试上游", "把原始上游事件与出站请求体写入 debug 日志。含大量内容，排错时再开。", "bool",
			advanced),

		// Trae 签到风控（9074）参数
		trae("upstreams.trae-cn.checkin.interval_seconds", "签到请求间隔秒数",
			"同一轮签到中相邻两个账号的间隔。上游按设备风控，背靠背连发不划算。", "int", advanced),
		trae("upstreams.trae-cn.checkin.retry_after_seconds", "9074 重试基数秒数", "上游返回风控码 9074 后，最短等待多久再试。", "int",
			advanced),
		trae("upstreams.trae-cn.checkin.max_backoff_seconds", "9074 退避上限秒数", "连续 9074 时指数退避的等待上限。", "int", advanced),
		trae("upstreams.trae-cn.checkin.backoff_exponent_cap", "退避指数上限", "限制翻倍次数，让长时间 9074 稳定在分钟级而不是一直卡在上限。", "int",
			advanced),
		trae("upstreams.trae-cn.checkin.rotate_device_id", "9074 轮换设备 ID", "遇到 9074 时换一个派生的设备 ID 再试一次。", "bool",
			advanced),

		// Trae 协议端点（自建代理时才需要动）
		trae("upstreams.trae-cn.auth_base_url", "鉴权基址", "OAuth 令牌交换的主机地址。", "string", danger, advanced),
		trae("upstreams.trae-cn.ug_base_url", "签到基址", "每日签到接口的主机地址。", "string", danger, advanced),
		trae("upstreams.trae-cn.pay_base_url", "权益基址", "积分余额（entitlement-usage）接口的主机地址。", "string", danger, advanced),
		trae("upstreams.trae-cn.usage_base_url", "用量基址", "逐轮商业用量接口的主机地址。", "string", danger, advanced),
		trae("upstreams.trae-cn.authorize_url", "授权页地址", "交互式登录时打开的页面。", "string", advanced),
		trae("upstreams.trae-cn.client_id", "OAuth Client ID", "令牌交换时携带的客户端标识。", "string", advanced),
		trae("upstreams.trae-cn.web_login_port", "交互登录端口", "本机授权回调占用的端口。0 表示自动挑一个空闲端口。", "int", advanced),

		// Trae 设备身份（出站请求声明的客户端）
		trae("upstreams.trae-cn.device_brand", "设备型号", "出站请求声明的设备型号，默认值与桌面客户端一致。", "string", advanced),
		trae("upstreams.trae-cn.device_type", "设备类型", "出站请求声明的设备类型（windows / mac / linux）。", "string", advanced),
		trae("upstreams.trae-cn.os_version", "系统版本", "出站请求声明的操作系统版本。", "string", advanced),
		trae("upstreams.trae-cn.app_version", "应用版本", "出站请求声明的应用版本号。", "string", advanced),
		trae("upstreams.trae-cn.ide_version", "IDE 版本", "出站请求声明的 IDE 版本号。", "string", advanced),
		trae("upstreams.trae-cn.app_id", "应用 ID", "出站请求声明的客户端应用 ID。", "string", advanced),

		// Trae 本地 CLI 执行器（upstream_mode=cli）。参考实现的 CLI 子进程
		// 参数没有面板入口，这里只补一个可执行文件路径：默认会在 PATH 里按
		// 内置名称查找，装到非标准位置时这是唯一的救命开关。
		trae("upstreams.trae-cn.cli.command", "CLI 可执行文件", "本地 CLI 的路径。留空则在系统 PATH 里按内置名称查找。", "string", advanced),

		// ── 出站改写 ──────────────────────────────────────────────────────
		hot("features.sanitize_fingerprints", SectionFeature, "清洗客户端指纹", "擦除上游内容过滤器会字面匹配的客户端提示词指纹。", "bool"),
		hot("features.repair_tool_markup", SectionFeature, "修复工具标记", "把正文里的工具调用标记还原为结构化 tool_calls。", "bool"),

		// ── 日志与可观测 ──────────────────────────────────────────────────
		hot("request_log.recent_limit", SectionObserv, "最近请求条数", "面板「运行日志」保留的实时请求条数（内存环形）。", "int"),
		hot("request_log.stdout", SectionObserv, "请求日志写标准输出", "是否把每条请求摘要打印到标准输出。", "bool"),
		hot("usage_store.enabled", SectionObserv, "启用用量历史", "把请求摘要写入 SQLite 供「用量」页统计。关闭后面板只显示实时日志。", "bool"),
		hot("usage_store.max_records", SectionObserv, "用量明细保留条数", "超出后裁剪最旧的记录。约每条约 200 字节。", "int"),
		cold("usage_store.path", SectionObserv, "用量库路径", "SQLite 文件位置。留空放在数据目录下的 usage.db。改后需重启。", "string",
			advanced, placeholder("留空 = 数据目录/usage.db")),
		cold("responses.ttl_seconds", SectionObserv, "会话续链有效期（秒）",
			"`previous_response_id` 对应的上下文在内存中保留多久。过期后客户端需重发全量 input；正文绝不落盘。改后需重启。", "int", advanced),
		cold("responses.max_entries", SectionObserv, "会话续链条数上限",
			"内存中最多保留多少个会话上下文，超出按最久未用淘汰。改后需重启。", "int", advanced),
		cold("responses.max_bytes", SectionObserv, "会话续链字节上限",
			"所有会话上下文占用的总字节上限。改后需重启。", "int", advanced),
		cold("responses.max_messages", SectionObserv, "单会话消息数上限",
			"单个会话超过此消息数后不再缓存，避免一个长对话挤掉其它会话。改后需重启。", "int", advanced),

		// ── 安全 ──────────────────────────────────────────────────────────
		cold("security.encrypt_credentials", SectionSecurity, "加密凭证", "用信封加密封存凭证文件。开关会改变存储格式，需重启后生效。", "bool",
			danger),
		cold("security.key_provider", SectionSecurity, "密钥后端", "auto 优先操作系统密钥库；os 强制系统密钥库；file 退回口令派生。需重启后生效。", "enum",
			options("auto", "os", "file")),
	}
	for _, path := range Restart {
		for i := range all {
			if all[i].Path == path {
				all[i].Hot = false
			}
		}
	}
	return all
}

// ValueAt reads a dotted JSON path out of any config-shaped value. A round trip
// through JSON is enough here and keeps the rest of the code free of
// reflection.
func ValueAt(v any, path string) (any, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, false
	}
	parts := strings.Split(path, ".")
	for i, p := range parts {
		next, ok := m[p]
		if !ok {
			return nil, false
		}
		if i == len(parts)-1 {
			return next, true
		}
		m, ok = next.(map[string]any)
		if !ok {
			return nil, false
		}
	}
	return nil, false
}

// Changed reports whether any of the paths differs between two configs.
func Changed(before, after any, paths ...string) bool {
	for _, p := range paths {
		a, okA := ValueAt(before, p)
		b, okB := ValueAt(after, p)
		if okA != okB || fmt.Sprint(a) != fmt.Sprint(b) {
			return true
		}
	}
	return false
}
