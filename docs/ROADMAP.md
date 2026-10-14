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

### P1b · 价值功能（待办）
- [ ] P1-1 账号接入：浏览器授权流程
- [ ] P1-1 账号接入：手工凭证导入
- [ ] P1-1 账号接入：环境变量来源
- [ ] P1-1 账号接入：本机客户端凭证自动发现
- [ ] P1-1 token 刷新与失效自愈
- [ ] P1-4 定时任务：签到 / 保活 / 活跃上报 / 余额后台刷新
- [ ] P1-4 定时任务：成长类（旅行 / 夜猫子 / 连登 / 抽奖）
- [ ] P1-5 额度与可观测：积分 / 余额查询、每请求日志
- [ ] P1-6 面板：来源 A 账号视图、添加账号、批量运维（P5 统一前的过渡）

## P2 · 来源 B（Trae CN）基础链路
- [ ] 认证：浏览器授权、手工令牌、环境变量
- [ ] 上游：Remote 端点 + 模型目录
- [ ] 统一账号池（Source 维度）+ 模型路由（扁平名 + 同名择优）
- [ ] `/v1/chat/completions` 流式 / 非流式打通

## P3 · 来源 B 认证补齐 + 端点扩展 + 风控
- [ ] 认证补齐：本地凭证自动发现（解密本机 `storage.json`）、本地 CLI 子进程
- [ ] 四端点 Remote / Solo / IDE Agent / Work Agent + 自动路由 + 失败回落
- [ ] 签到 + 风控码设备标识轮换 + 指数退避 + 后台错峰重试

## P4 · 协议超集与高级能力
- [ ] `/v1/responses`（Codex，含 `previous_response_id`）
- [ ] `/v1/messages` + `/v1/messages/count_tokens`（Anthropic）
- [ ] 工具调用（`delta.tool_calls` / `tool_use` / `function_call`）
- [ ] thinking 分离、自动续写、1M 上下文、图片输入、思考强度映射
- [ ] 用量与积分记录迁入 SQLite
- [ ] 来源 B 桌面端「自定义模型」入站兼容（纯协议翻译，无原生依赖；可选、可裁剪）

## P5 · 统一面板重设计 + 任务合并 + 配置热生效 + 凭证加密
- [ ] 面板信息架构改造（来源为一等公民，全局来源切换）
- [ ] 定时任务合并入统一任务中心
- [ ] 在线配置编辑 + 热加载
- [ ] 凭证信封加密（OS 密钥库：DPAPI / Keychain / Secret Service）+ 导出/导入

## P6 · 打包与发布
- [ ] 前端 embed 进二进制
- [ ] Windows 单 exe 优先；Linux / macOS 构建
- [ ] Docker 镜像（多阶段、非 root、HEALTHCHECK）
- [ ] 发布 Release 资产 + 校验和 + 用户文档

## v2 · 可选（v1 明确不做）
- [ ] 来源 B 桌面端原生桥出站传输 —— 依赖 Windows + 私有 DLL，与纯 Go 单二进制目标冲突（见 EXECUTION_PLAN N1 / §9.4）。**注**：其「自定义模型」入站兼容（无原生依赖）已纳入 v1。
