# Roadmap 路线图

> 状态图例：`[ ]` 未开始 · `[~]` 进行中 · `[x]` 完成

## P0 · 骨架 Bootstrap
- [x] 仓库与许可、README、贡献指南、目录约定
- [ ] `cmd/codenexus` 入口 + `-config` / `-version`
- [ ] 配置加载（`config.json`，含随机 `api_key` 生成与一次性打印）
- [ ] HTTP 服务骨架 + `GET /healthz`

## P1 · 上游适配器 Upstream adapters
- [ ] 抽象 `Upstream` 接口：`ListModels()` / `Chat()` / `ChatStream()` / `Auth()`
- [ ] `workbuddy` 适配器（参考 `linguo2625469/workbuddy2api-panel`）
- [ ] `trae-cn` 适配器（参考 `autumnsentiment/Trae2api-cn`，含 9074 风控退避）
- [ ] 统一的错误映射与重试语义

## P2 · 账号池与调度 Pool & scheduling
- [ ] 统一凭证模型与存储（`auths/`，明文落盘 + 文件权限收紧）
- [ ] 轮询策略：顺序 / 积分优先；每账号并发槽位
- [ ] 429 冷却 + 指数退避 + 熔断
- [ ] 积分下限过滤（`credit_floor`）

## P3 · 统一 API Unified API
- [ ] `GET /v1/models`（按来源汇总，附上下文长度等元数据）
- [ ] `POST /v1/chat/completions`（流式 SSE + 非流式聚合）
- [ ] `tool_calls` 支持与 thinking 分离回填

## P4 · 控制台与任务 Console & tasks
- [ ] 内嵌网页控制台（账号 / 用量 / 积分 / 任务 / 配置 / 日志）
- [ ] 定时任务：签到 / 活跃 / 保活（可逐项开关，按本地时区）
- [ ] 在线改配置 + 热加载

## P5 · 打包与发布 Packaging
- [ ] 前端 embed 进二进制
- [ ] 跨平台构建（Windows / Linux / macOS）
- [ ] 发布 Release 资产 + 校验和
