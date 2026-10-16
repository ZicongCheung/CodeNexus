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

## P3 · 来源 B 认证补齐 + 风控 ✅（已完成）
> 决策 ADR-41~45：`auto` = 自动发现 + 解密 + 面板一键导入（一律信封加密，不落明文）· `cli` = 作为 `upstream_mode` 的取值（全局模式，非池内伪账号）· `cli` 保真度 = 原生 JSON + 文本模式开关 + 复用既有护管线 · 全量一次交付 · 结算三级取值全量对齐参考**并带上并发归属闸门**。
- [x] `auto` 认证：本机 `storage.json` 自动发现（四个 `%APPDATA%` 候选，取首个 token 仍有效者）+ `tc` 格式解密（`SALT_A⊕SALT_B`，自校验 `SHA512`）+ 面板「从本机 Trae 导入」一键入池
- [x] `cli` 上游模式：`upstream_mode: cli`（可执行定位 / 参数拼装 / 括号配对 JSON 流 / 快照前缀差 / EOF 终止 / 单块超时 kill / 全局并发上限 / 工具禁用 / prompt 序列化与截断 / 文本模式开关）
- [x] ~~四端点 Remote / Solo / IDE Agent / Work Agent + 自动路由 + 失败回落~~ → 提前至 P2（ADR-38，两个物理面覆盖四个逻辑端点）
- [x] 签到 + 风控码设备标识轮换（`9074` 轮换一次并指数退避 / `9095` 显式非成功且不退避）+ 后台错峰重试（池侧软冷却路径已于 P2 打通，见 ADR-39）
- [x] 回合级积分结算接入（显式字段 → `get_session_usage` → 账号已用额度差值，接上三级优先级与**并发归属闸门**，ADR-45）
- [x] 不做：`src/auth_cli.py` 的 OAuth 设备码流程（上游 404、源码自述不可用）；CLI 多方言文本内工具块解析（ADR-43）；上游模式运行时热切换（留 P5）
- [x] 调研副本 `_deps/` 已删除（P3 收尾）

## P4 · 协议超集与高级能力（进行中）
> 决策 ADR-46~49：全量一次交付 · `previous_response_id` = 内存有界 LRU（不落盘）· Anthropic = 形状严格、校验从宽（第三轮按参考实现修订）· 落库 = 用量/积分 + 请求日志摘要（窄字段，正文与凭证永不落）。
> 第二轮决策（#43）：Anthropic `thinking` 保真（`signature` 最终取空串）· Responses 推理出**独立 `reasoning` 项** · 入站工具配对不合法**自动修复并记日志** · 分片**保真不造假**。
> 第三轮决策（#42）：`count_tokens` 本地估算 · 图片透传（工具结果内降级为可见占位）· 校验从宽（`max_tokens` 缺失填默认值）· `message_start` 看门狗保活（保留首字节前故障转移）。
> 第四轮决策（#41）：推理**默认隐藏**（显式 `thinking`/`include_reasoning` 才出，且压缩）· 孤儿工具输出**保留为惰性块**（不丢弃）· 工具类型**全量**（function/custom/namespace/tool_search + shell/patch 系列）· LRU 参数全对齐参考（含超限哨兵）。
> 第五轮决策（#44）：**纯 SQL 实时聚合**（不建聚合表，积分事后改账天然精确）· 明细**默认 10 万条**（可配置；参考的 1000 条是 JSON 全量重写成本的产物）· 面板「用量」页**最小可用含趋势**· 与内存 reqlog **各司其职**。
- [x] 协议中立对话核心（选号 / 首字节前故障转移 / 401 自愈 / 记账 / 结算；wire 决定形态）
- [x] 工具调用与 thinking 的中立下层：中立消息模型（`Message.ToolCalls`/`ToolCallID`）、分片追踪器（`index` 键控，增量/快照/乱序/重复）、配对校验（丢孤儿并记日志）
- [x] `/v1/messages`（Anthropic：`content_block` 流式事件、`stop_reason` 全枚举、thinking 块、`tool_use` `input_json_delta`、错误信封）
- [x] `/v1/messages/count_tokens`（本地估算，不触达上游、不计费）
- [x] 核心看门狗保活（`turn.IdleAfter` + `streamOpener`：静默超时即开流并 `ping`，仍保留首字节前换号）
- [x] `/v1/responses`（Codex：带 `sequence_number` 的事件流、`previous_response_id` 内存 LRU + 超限哨兵、工具名双向绑定、推理默认隐藏、孤儿输出惰性化）
- [x] 工具调用三向映射与配对落地（`delta.tool_calls` / `tool_use` / `function_call`）
- [x] **P4 真机探测与验收**（§9.9）：三线协议入口真机通过（含 `previous_response_id` 跨轮续链）；真机抓到并修复 6 个协议缺陷；新增 `debug_upstream` 调试开关
- [x] **工具调用闭环真机通过**（§9.9）：对照实验（起 `Trae2api-cn` 用**同一账号**实测）证明"上游不调用调用方工具"是**我方缺陷**而非上游限制 → 定位并修复 2 个 SOLO 流形状缺陷（`function_call` 扁平+分片被整通丢弃、`extra_info` 摘要污染正文）；非流式 / 流式 / 完整闭环（调用→结果→回答）均通过
- [x] 图片输入真机链路**可用**：8×8 纯色图红/蓝反例均答对（此前 1×1 像素样本过小导致误判"上游未消费"）
- [x] `reasoning_effort` 映射（§4.2）：原生档位 `light`/`high`/`extra_high`，按目录 `reasoning_effort_config` 向下夹取；仅在账号目录声明 support_thinking 时生效（ADR-53）
- [x] 用量与积分 + 请求日志摘要迁入 SQLite（`modernc.org/sqlite`，纯 Go 无 CGO）：`internal/usagestore` + 面板「用量」页（总览卡 / 每日趋势 / 模型分布 / 可筛选明细）
- [x] 自动续写（§4.2）：全启发式判定 + 相似度防重复 + 轮数封顶；工具回合永不续写；流式并入同一回答（ADR-54）
- [x] 1M Max 档位（§4.2）：`max_mode` 配置 + 实时目录确证 `max_mode=true` 才生效；绝不虚构账号未标记的档位（ADR-53）
- [x] `previous_response_id` LRU 参数暴露为配置（`responses.*`，需重启；ADR-56）
- [x] 来源 B 桌面端「自定义模型」入站兼容：**完整契约对齐**（三重识别 / 嵌套展开 / 私有累积 SSE / 连接性探测 `is_preset=false`；忽略入站 `Cloud-JWT` 走池内凭证）；差分测试对参考逐帧比对（ADR-55）

## P5 · 统一面板重设计 + 任务合并 + 配置热生效
- [x] 面板视觉与交互增强（IA 骨架已于 P1b1 落地，见 ADR-27，不重构）：主题切换（亮/暗/跟随系统）、加载骨架、表格与移动端适配、`focus-visible` / `prefers-reduced-motion`；不引第三方依赖
- [x] 面板「设置」页：表单 + 高级 JSON 双轨（61 个字段元数据来自 `internal/configschema`）
- [x] ~~定时任务合并入统一任务中心~~ → 提前至 P1b2（ADR-35）
- [x] 在线配置编辑 + 热加载：**分级热生效**（55 项即时生效、6 项标「需重启」）、保存前自动快照 + 面板内一键回滚、危险字段二次确认（后端 409）、一键重启（含新地址试绑）
- [x] 面板 UI/UX **二次重做**（ADR-60，二轮验收要求参考 `ZhangInvest-Frontend` + Apple 风格）：移植设计令牌（深浅两套 / iOS 过渡曲线 / 玻璃顶栏 / 1200px 容器）、导航改分组胶囊、Toast + Promise 式确认弹窗替换 `alert/confirm`、图标内联、文案抽表；顺带修掉「UI 套件只写在父组件 scoped 样式里、子组件继承不到」的旧问题
- [x] 1M 档位与截断续写的**可见性收口**（ADR-59/60）：进程日志与面板「运行日志」新增 `surface=` / `max=` / `continue=`（面板渲染为「远程 / 1M · 982k / 续写 N 轮」徽标）；续写合并去掉模型重发的开头；新增流式无进展看门狗 `stream.stall_seconds`
- [x] ~~凭证信封加密（OS 密钥库：DPAPI / Keychain / Secret Service）+ 导出/导入~~ → 提前至 P1b1（ADR-26）

## P6 · 打包与发布
- [x] 前端 embed 进二进制（`internal/panel` 的 `go:embed dist`，`web/` 由 `scripts/build.sh web` 重建）
- [x] Windows **双产物**（`CodeNexus.exe` 无黑框 + `CodeNexus-console.exe` 排障）+ 自研 Win32 托盘（五项菜单，零新增依赖，ADR-51）；Linux / macOS 构建（darwin/linux × amd64/arm64 全量）
- [x] Docker 镜像（多阶段、非 root、HEALTHCHECK、多架构 `linux/amd64,linux/arm64`）
- [x] 发布 Release 资产 + 校验和：`scripts/build.sh release` 出 6 平台归档 + `SHA256SUMS`；`scripts/package.py` 可复现打包、`scripts/checksums.py` 写校验和
- [x] 图标与版本元数据（纯标准库合成 `.ico` + `go-winres` `.syso`，ADR-52）；首次运行自动开浏览器
- [x] CI：`.github/workflows/ci.yml`（push/PR → `build.sh test`）、`release.yml`（tag `v*` → `build.sh release` → GitHub Release）；脚本为唯一事实源（ADR-50）
- [x] 用户文档（README 构建/发布/双产物/托盘说明）

## v2 · 可选（v1 明确不做）
- [ ] 来源 B 桌面端原生桥出站传输 —— 依赖 Windows + 私有 DLL，与纯 Go 单二进制目标冲突（见 EXECUTION_PLAN N1 / §9.4）。**注**：其「自定义模型」入站兼容（无原生依赖）已纳入 v1。
