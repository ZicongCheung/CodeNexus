# Roadmap 路线图

> 状态图例：`[ ]` 未开始 · `[~]` 进行中 · `[x]` 完成
>
> **详细执行计划见 [`EXECUTION_PLAN.md`](./EXECUTION_PLAN.md)**（含架构、数据模型、验收标准、风险登记册、团队 RACI）。
> 分期策略：**双链路先通，能力后补**（ADR-08）。实现纪律：**独立重写、不雷同**（ADR-12）。

## P0 · 骨架与可运行闭环 ✅
- [x] 仓库与许可、README、贡献指南、目录约定
- [x] `.gitignore`（含 `.workbuddy/`、前端依赖）
- [x] Go 模块（`github.com/ZicongCheung/CodeNexus`）+ `cmd/codenexus` 入口（`-config` / `-version`）
- [x] 配置加载 `internal/config`（`config.json`，随机 `api_key` 生成与一次性打印，原子写）
- [x] 鉴权 `internal/httpauth`（Bearer / `x-api-key` + 常量时间比较）
- [x] 上游接口 `internal/upstream`（`Upstream` 接口 + 注册表 + 统一类型）
- [x] 账号模型与池 `internal/account` + `internal/pool`（最简选号）
- [x] 协议入口 `internal/api`：`GET /healthz`、`GET /v1/models`、`POST /v1/chat/completions`（流式 + 非流式）
- [x] 首个上游适配器 `internal/upstream/workbuddy`（模型目录 + 对话）
- [x] 内嵌控制台 `internal/panel`（go:embed）+ `web/` Svelte 脚手架
- [x] 单元测试：config / httpauth / api / workbuddy（含 mock 上游）
- [x] 验收：`go build/vet/test` 全绿；`/healthz` 200；端到端闭环本地验证通过

## P1 · 来源 A（WorkBuddy / CodeBuddy）完整实现
> 范围决策 ADR-15~18：账号接入**全量** · 池调度**完整策略** · 出站改写**全量管线** · 定时任务**全量**。
> 调度与接入决策 ADR-19~23：首字节前故障转移 · 多信号会话粘性 · 复合加权选号 · 全池冷却短暂等待 · 脱敏与标记还原默认开启。

### P1a · 引擎与接入（已完成）
- [x] 凭证存储：0600 原子落盘 + 校验 + 来源推断（`internal/authstore`）
- [x] 池状态机：健康 / 冷却 / 熔断 / 模型冷却 / 并发槽 / 状态持久化（`internal/pool`）
- [x] 池调度：复合加权选号（额度 + 闲置 + 成功率）取 Top-K 加权随机
- [x] 池调度：429/402 冷却 + 指数退避（受 `Retry-After` 约束）
- [x] 池调度：熔断（连续瞬时失败达阈值）
- [x] 池调度：每账号并发槽（在途租约）
- [x] 池调度：会话粘性（多信号融合键 + TTL + GC）
- [x] 池调度：全池不可用等待最早恢复（上限 + `Retry-After`）
- [x] 请求故障转移：首字节前自动换号重试；非重试类错误不重试
- [x] 出站改写：强制流式 + 角色归一 + token 归一
- [x] 出站改写：思维链注入 / 回填
- [x] 出站改写：工具配对修复
- [x] 出站改写：内容指纹脱敏（默认开启，可配置）
- [x] 出站改写：原生工具标记还原（非流式 + 跨分片流式，默认开启，可配置）
- [x] 错误分类核心：kind + 可重试性 + 关键词/状态码映射（`internal/upstream/classify.go`）
- [x] 适配器接入管线与分类；端到端测试（故障转移 / 粘性 / 冷却 / 熔断 / 持久化 / 脱敏 / 标记还原）
- [x] 验收：`go build/vet/test` 全绿；启动冒烟通过（healthz / status / 无账号 503 / 账号加载与选号）

### P1b · 价值功能（已完成）
> 批次决策 ADR-24~27：**P1b1 账号接入与自愈 → P1b2 价值与可视** · 面板内嵌设备码授权 · 凭证加密提前到本批 · 面板直接统一信息架构。

#### P1b1 · 账号接入与自愈（已完成）
- [x] `internal/crypto` 密钥库后端（OS 密钥库：Windows DPAPI / macOS Keychain / Linux Secret Service；文件与内存降级）
- [x] 信封加密落盘（每凭证独立 AES-256-GCM 数据密钥）+ 旧的明文凭证自动迁移
- [x] 导出/导入（`codenexus auth export/import`，口令派生密钥包裹）
- [x] token 刷新与失效自愈（单飞刷新 + 后台预刷新 + 请求路径 401 自动重试一次 + 原子写回）
- [x] 授权流程：面板内嵌设备码授权（验证码 + 链接 + 后台轮询 + 自动入池）
- [x] 手工凭证导入（粘贴 JSON / 服务器本地路径 / 加密包三模式，校验后加密落盘）
- [x] 环境变量来源（`CODENEXUS_WORKBUDDY_TOKEN*` 首启注入，临时账号不落盘）
- [x] 本机客户端凭证自动发现 → **收敛为「指定路径导入」**（ADR-29：不做自动探测/解密，对齐参考实现）
- [x] 控制台访问控制（回环豁免 + 非回环强制 `api_key`）+ 响应安全头（ADR-28）
- [x] 控制台 UI（来源分组账号表 + 健康徽标 + 设备授权向导 + 导入面板）
- [x] 验收：`go build/vet/test` 全绿；启动冒烟通过（healthz / panel 静态资源 / session / accounts；非回环受保护）

#### P1b2 · 价值与可视（已完成 · 全量一次交付，ADR-30）
> 决策 ADR-30~36：全量一次交付 · 调度时区可配置（默认 `Asia/Shanghai`）· 持久化 last-run 并宽限补跑（仅保号类）· 成长执行器全量对齐参考 25 项 · 任务全部默认开启 · 任务中心全写入。
- [x] 定时任务框架（整点对齐 + 抗睡眠分段等待 + 迟到补跑宽限 + 逐项开关 + 可配置时区 + 重启补跑）
- [x] 核心任务：签到 / 保活 / 活跃上报 / 余额后台刷新
- [x] 成长类任务：旅行 / 夜猫子 / 连登 / 抽奖
- [x] 成长执行器全量 25 项（账号原生 4 / 桌面端事件链 13 / 小程序事件链 8），含事件链协议层、专家市场、真实对话取 `requestId`、反作弊节流
- [x] 额度与积分查询（账号余额 / 积分构成 / 窗口内过期 / 最早到期优先路由）
- [x] 每请求日志（TTFB / token 速率 / 账号 / 来源；内存环形 + stdout，不含提示词与凭据）
- [x] 面板任务中心（批量维护 / 待办队列 / 执行器目录 / 逐账号任务表与执行结果）与运行日志（统计卡 + 最近请求）
- [x] 验收：`go build/vet/test` 全绿；启动冒烟通过（session 报告 tasks/reqlog、catalog 返回 25 项、queue 与 metrics 正常、面板静态资源为新构建产物）

## P2 · 来源 B（Trae CN）基础链路（已完成 · 全量一次交付，ADR-37~40）
> 决策 ADR-37~40：授权形态**面板内嵌 + 本机回环回调**（另附「粘贴令牌」通道）· 端点范围**直接全量四端点 + 自动路由** · 池语义**统一到池现有语义**（积分→`credits`；`9074`→软冷却 + 指数退避，与 `429` 同路径）· **全量一次交付**。
- [x] 认证：回环浏览器授权（一次性 `127.0.0.1` 监听 + `/authorize` 回调 + TTL 过期）、手工令牌（`CredentialAcceptor`，可只给刷新令牌当场换发）、环境变量（`CODENEXUS_TRAE_*`，临时态不落盘）
- [x] 令牌刷新：`ExchangeToken` 单飞；无刷新令牌归为认证类错误；过期时间只取上游字段（不读 JWT `exp`），提前 30 分钟刷新
- [x] 上游：Remote 会话面（建会话 + SSE 事件流，**累积快照前缀差**，心跳/元数据不计首字节）+ Agent 单跳面（原生工具数组，四种工具增量归一 + 去重）
- [x] 自动路由：请求带工具协议 → Agent 面；纯聊天 → Remote 面；Agent 面可重试且未吐字节时同账号回落 Remote 一次
- [x] 模型目录：内置 34 项 + 家族别名展开；大小写敏感的上游 `config_name` 映射；未知名透传
- [x] 错误分类：`1001` / `4008` / `4028` / `9074` / `429` / `2001` / `9095` → 统一 `ErrorKind`（映射池行为见 ADR-39）
- [x] 统一账号池（`PickOptions.Sources` 允许名单，同名模型跨源同一权重尺度择优）+ 模型路由（前缀精确通道 + 扁平名按目录声明顺序 + 未知来源跳过不猜测）
- [x] `/v1/chat/completions` 流式 / 非流式打通（两个来源共用同一套协议层与故障转移）
- [x] 面板：`POST /panel/api/auth/token`（粘贴令牌）+ 设备授权向导尊重所选来源 + `/api/session` 暴露 `device_sources`/`manual_sources`；添加账号页新增「粘贴令牌」通道
- [x] 验收：`go build/vet/test` 全绿；启动冒烟通过（session 报告 `trae-cn` 且两种开号通道正确、`/v1/models` 含 Trae 目录、同名模型跨源择优）

## P3 · 来源 B 认证补齐 + 端点扩展 + 风控
- [ ] 认证补齐：本地凭证自动发现（解密本机 `storage.json`）、本地 CLI 子进程
- [x] ~~四端点 Remote / Solo / IDE Agent / Work Agent + 自动路由 + 失败回落~~ → 提前至 P2（ADR-38，两个物理面覆盖四个逻辑端点）
- [ ] 签到 + 风控码设备标识轮换 + 指数退避 + 后台错峰重试（池侧软冷却路径已于 P2 打通，见 ADR-39）

## P4 · 协议超集与高级能力
- [ ] `/v1/responses`（Codex，含 `previous_response_id`）
- [ ] `/v1/messages` + `/v1/messages/count_tokens`（Anthropic）
- [ ] 工具调用（`delta.tool_calls` / `tool_use` / `function_call`）
- [ ] thinking 分离、自动续写、1M 上下文、图片输入、思考强度映射
- [ ] 用量与积分记录迁入 SQLite
- [ ] 来源 B 桌面端「自定义模型」入站兼容（纯协议翻译，无原生依赖；可选、可裁剪）

## P5 · 统一面板重设计 + 任务合并 + 配置热生效
- [ ] 面板视觉与交互增强（信息架构骨架已于 P1b1 落地，见 ADR-27，不重构）
- [x] ~~定时任务合并入统一任务中心~~ → 提前至 P1b2（ADR-35）
- [ ] 在线配置编辑 + 热加载
- [x] ~~凭证信封加密（OS 密钥库：DPAPI / Keychain / Secret Service）+ 导出/导入~~ → 提前至 P1b1（ADR-26）

## P6 · 打包与发布
- [ ] 前端 embed 进二进制
- [ ] Windows 单 exe 优先；Linux / macOS 构建
- [ ] Docker 镜像（多阶段、非 root、HEALTHCHECK）
- [ ] 发布 Release 资产 + 校验和 + 用户文档

## v2 · 可选（v1 明确不做）
- [ ] 来源 B 桌面端原生桥出站传输 —— 依赖 Windows + 私有 DLL，与纯 Go 单二进制目标冲突（见 EXECUTION_PLAN N1 / §9.4）。**注**：其「自定义模型」入站兼容（无原生依赖）已纳入 v1。
