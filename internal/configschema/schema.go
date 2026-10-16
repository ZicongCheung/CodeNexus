// Package configschema describes the configuration fields the console edits.
//
// The schema is the contract between the config file and the settings form: it
// says what a field is called, what values it accepts, and — the part users
// actually feel — whether saving it takes effect now or needs a restart. A
// field absent from here is still editable through the raw JSON editor, so the
// form is a convenience over the whole file rather than a subset that silently
// hides the rest.
package configschema

import (
	"encoding/json"
	"fmt"
	"strings"
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
	// Group collects fields under one heading in the form.
	Group string `json:"group"`
	// Label is the short name shown next to the control.
	Label string `json:"label"`
	// Help explains what the setting does and what happens if it is wrong.
	Help string `json:"help"`
	// Kind drives the control: bool, int, float, string, enum, hours, secret.
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

// Fields is the catalog the settings form renders. Order is display order.
func Fields() []Field {
	all := []Field{
		// Gateway
		hot("log_level", "网关", "日志级别", "写入标准输出的详细程度。改为 debug 会打印上游事件，排错时有用，平时留 info。", "enum",
			options("error", "warn", "info", "debug")),
		cold("listen", "网关", "监听地址", "HTTP 服务绑定的地址。改错会让服务起不来，需重启后生效。", "string",
			danger, placeholder("127.0.0.1:6828")),
		cold("api_key", "网关", "API 密钥", "客户端访问网关的密钥。修改后旧客户端立即失效，需重启后生效并同步更新客户端。", "secret",
			danger, placeholder("留空表示自动生成新的随机密钥")),
		cold("data_dir", "网关", "数据目录", "状态文件、用量库与配置快照的存放位置。需重启后生效。", "string", danger),
		cold("auth_dir", "网关", "凭证目录", "账号凭证文件的存放位置。需重启后生效。", "string", danger),

		// Account pool
		hot("pool.max_in_flight", "账号池", "单账号并发上限", "同一个账号同时承载的请求数。上游限流严格时调低。", "int"),
		hot("pool.credit_floor", "账号池", "积分为零阈值", "积分低于此值的账号不参与选号。", "int"),
		hot("pool.cooldown_seconds", "账号池", "冷却起始秒数", "账号出错后的首次冷却时长，失败次数越多递增。", "int"),
		hot("pool.cooldown_max_seconds", "账号池", "冷却上限秒数", "冷却时长的天花板，避免长时间连续失败把账号封死。", "int"),
		hot("pool.breaker_threshold", "账号池", "熔断失败次数", "连续失败达此次数后熔断该账号。", "int"),
		hot("pool.breaker_seconds", "账号池", "熔断时长秒数", "熔断后隔离该账号的时长。", "int"),
		hot("pool.failover_attempts", "账号池", "故障转移次数", "一次请求最多换几个账号重试。", "int"),
		hot("pool.wait_max_seconds", "账号池", "排队等待上限秒数", "无可用账号时排队等待的时长，超时返回 429。", "int"),
		hot("pool.weight_credit", "账号池", "权重·积分", "选号时积分余额的权重。", "float"),
		hot("pool.weight_idle", "账号池", "权重·空闲", "选号时空闲时长的权重。", "float"),
		hot("pool.weight_success", "账号池", "权重·成功率", "选号时历史成功率的权重。", "float"),
		hot("pool.weight_expiring", "账号池", "权重·临期积分", "选号时对即将过期积分的倾斜，越大越优先消耗临期账号。", "float"),
		hot("session_sticky.enabled", "账号池", "会话粘滞", "把同一会话固定到一个账号，减少上下文切换。", "bool"),
		hot("session_sticky.ttl_seconds", "账号池", "粘滞有效期秒数", "绑定保持多久后释放。", "int"),

		// Router
		hot("router.naming", "模型路由", "模型命名", "flat 暴露裸模型名并跨来源解析；prefixed 只暴露 来源/模型。", "enum",
			options("flat", "prefixed")),
		hot("router.same_name_policy", "模型路由", "同名策略", "多个来源提供同名模型时：auto_healthy 取最健康的账号，first 固定第一个来源。", "enum",
			options("auto_healthy", "first")),
		hot("router.allow_prefix", "模型路由", "允许前缀写法", "即使命名是 flat，也允许客户端用 来源/模型 显式指定来源。", "bool"),

		// Upstream
		hot("upstreams.trae-cn.enabled", "上游", "启用 Trae CN", "关闭后不再注册该来源，已有请求会失败。", "bool", danger),
		hot("upstreams.trae-cn.upstream_mode", "上游", "执行面", "remote 走网页面；ide 走模型网关（原生工具调用）；auto-route 按是否有工具自动分派；cli 走本机 Trae CLI。", "enum",
			options("auto-route", "remote", "solo", "ide", "work-agent", "cli")),
		hot("upstreams.trae-cn.auto_route", "上游", "自动分派", "auto-route 模式下：带工具的请求走 Agent 面，纯对话走 Remote 面。", "bool"),
		hot("upstreams.trae-cn.timeout_seconds", "上游", "请求超时秒数", "单个上游请求的超时时间。", "int"),
		hot("upstreams.trae-cn.max_tokens", "上游", "默认最大输出", "客户端未指定 max_tokens 时使用的上限。", "int"),
		hot("upstreams.trae-cn.debug_upstream", "上游", "调试上游", "把原始上游事件与出站请求体写入 debug 日志。含大量内容，排错时再开。", "bool"),
		hot("upstreams.trae-cn.base_url", "上游", "模型网关基址", "IDE Agent / Solo 端点的主机地址。改错会导致请求失败。", "string", danger),
		hot("upstreams.trae-cn.remote_base_url", "上游", "网页面基址", "Remote 对话端点（chat_sessions + events）的主机地址。", "string", danger),
		hot("upstreams.workbuddy.enabled", "上游", "启用 WorkBuddy", "是否注册 WorkBuddy / CodeBuddy 来源。", "bool"),
		hot("upstreams.workbuddy.timeout_seconds", "上游", "WorkBuddy 超时秒数", "该来源的请求超时时间。", "int"),

		// Trae高级能力（截断续写 / 1M 档位）
		hot("upstreams.trae-cn.auto_continue.enabled", "上游·高级", "截断自动续写",
			"上游把回答截断时，自动追加一轮把答案补完，用户仍只看到一条回答。每一轮都真实消耗积分。", "bool"),
		hot("upstreams.trae-cn.auto_continue.max_rounds", "上游·高级", "续写最大轮数",
			"单次请求最多追加几轮，用于封顶额外消耗。", "int"),
		hot("upstreams.trae-cn.auto_continue.text_threshold", "上游·高级", "碎片判定阈值",
			"「思考很长但正文只有碎片」的判定阈值（字符）。调大更容易触发续写。", "int"),
		hot("upstreams.trae-cn.auto_continue.similarity_threshold", "上游·高级", "重复停止阈值",
			"续写结果与上一轮的重合度超过此值时停止，避免无限循环（0~1）。", "float"),
		hot("upstreams.trae-cn.max_mode.enabled", "上游·高级", "1M 档位",
			"对账号目录标记为 max 的模型使用最大上下文档位（可用高级 JSON 里的 max_mode.models 限定模型白名单）。只会对账号确实拥有的档位生效，绝不虚构。", "bool"),

		// Features
		hot("features.sanitize_fingerprints", "出站改写", "清洗客户端指纹", "擦除上游内容过滤器会字面匹配的客户端提示词指纹。", "bool"),
		hot("features.repair_tool_markup", "出站改写", "修复工具标记", "把正文里的工具调用标记还原为结构化 tool_calls。", "bool"),

		// Schedule
		hot("schedule.timezone", "任务调度", "时区", "日界与整点任务的时区（IANA 名称）。上游按北京时间结算，默认与之对齐。", "string",
			placeholder("Asia/Shanghai")),
		hot("schedule.checkin_enabled", "任务调度", "自动签到", "每日按整点执行签到。", "bool"),
		hot("schedule.checkin_hours", "任务调度", "签到整点", "一天中执行签到的小时（本地时钟）。", "hours"),
		hot("schedule.growth_enabled", "任务调度", "成长任务", "每日扫取并完成成长任务。", "bool"),
		hot("schedule.growth_hours", "任务调度", "成长任务整点", "一天中执行成长任务扫描的小时。", "hours"),
		hot("schedule.activity_enabled", "任务调度", "活跃度任务", "每日完成活跃度任务。", "bool"),
		hot("schedule.activity_hours", "任务调度", "活跃度整点", "一天中执行活跃度任务的小时。", "hours"),
		hot("schedule.travel_enabled", "任务调度", "行程任务", "每日完成行程任务。", "bool"),
		hot("schedule.travel_hours", "任务调度", "行程整点", "一天中执行行程任务的小时。", "hours"),
		hot("schedule.keepalive_enabled", "任务调度", "保活任务", "每日执行账号保活。", "bool"),
		hot("schedule.keepalive_hours", "任务调度", "保活整点", "一天中执行保活的小时。", "hours"),
		hot("schedule.blackcat_enabled", "任务调度", "黑猫任务", "每日完成黑猫任务。", "bool"),
		hot("schedule.blackcat_hours", "任务调度", "黑猫整点", "一天中执行黑猫任务的小时。", "hours"),
		hot("schedule.balance_refresh_enabled", "任务调度", "余额刷新", "按短间隔刷新积分余额，不依赖整点任务。", "bool"),
		hot("schedule.balance_refresh_minutes", "任务调度", "余额刷新间隔分钟", "刷新的间隔分钟数。", "int"),
		hot("schedule.checkin_retry_enabled", "任务调度", "签到重试", "为当天未完成签到的账号补跑签到。", "bool"),
		hot("schedule.checkin_retry_seconds", "任务调度", "签到重试间隔秒数", "两次补跑之间的间隔。", "int"),
		hot("schedule.catch_up_on_start", "任务调度", "启动时补跑", "进程在当天整点之后启动时补跑一次维护任务。", "bool"),
		hot("schedule.include_disabled_in_tasks", "任务调度", "任务包含停用账号", "维护任务是否也作用于被停用的账号（仍不承载流量）。", "bool"),
		hot("schedule.account_delay_ms", "任务调度", "账号间隔毫秒", "一轮扫描中相邻账号的请求间隔，避免打爆上游。", "int"),
		hot("schedule.expiring_soon_hours", "任务调度", "临期积分窗口小时", "在此窗口内过期的积分会被优先消耗。0 关闭该信号。", "int"),

		// Observability
		hot("request_log.recent_limit", "可观测性", "最近请求条数", "面板「运行日志」保留的实时请求条数（内存环形）。", "int"),
		hot("request_log.stdout", "可观测性", "请求日志写标准输出", "是否把每条请求摘要打印到标准输出。", "bool"),
		hot("usage_store.enabled", "可观测性", "启用用量历史", "把请求摘要写入 SQLite 供「用量」页统计。关闭后面板只显示实时日志。", "bool"),
		hot("usage_store.max_records", "可观测性", "用量明细保留条数", "超出后裁剪最旧的记录。约每条约 200 字节。", "int"),
		cold("responses.ttl_seconds", "可观测性", "会话续链有效期（秒）",
			"`previous_response_id` 对应的上下文在内存中保留多久。过期后客户端需重发全量 input；正文绝不落盘。改后需重启。", "int"),
		cold("responses.max_entries", "可观测性", "会话续链条数上限",
			"内存中最多保留多少个会话上下文，超出按最久未用淘汰。改后需重启。", "int"),
		cold("responses.max_bytes", "可观测性", "会话续链字节上限",
			"所有会话上下文占用的总字节上限。改后需重启。", "int"),
		cold("responses.max_messages", "可观测性", "单会话消息数上限",
			"单个会话超过此消息数后不再缓存，避免一个长对话挤掉其它会话。改后需重启。", "int"),

		// Security
		cold("security.encrypt_credentials", "安全", "加密凭证", "用信封加密封存凭证文件。开关会改变存储格式，需重启后生效。", "bool", danger),
		cold("security.key_provider", "安全", "密钥后端", "auto 优先操作系统密钥库；os 强制系统密钥库；file 退回口令派生。需重启后生效。", "enum",
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
