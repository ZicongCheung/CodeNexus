# CodeNexus

> **统一的 AI 编码账号网关** — 把 WorkBuddy / CodeBuddy 与 Trae CN 的账号池，汇聚成一个 OpenAI 兼容 API。
>
> **A unified gateway for AI coding accounts** — pool your WorkBuddy / CodeBuddy and Trae CN accounts behind a single OpenAI-compatible API.

<p align="center">
  <img alt="license" src="https://img.shields.io/badge/license-MIT-blue.svg">
  <img alt="go" src="https://img.shields.io/badge/Go-1.24%2B-00ADD8.svg?logo=go&logoColor=white">
  <img alt="status" src="https://img.shields.io/badge/status-WIP%20%C2%B7%20early%20skeleton-orange.svg">
  <img alt="platform" src="https://img.shields.io/badge/platform-Windows%20%7C%20Linux%20%7C%20macOS-lightgrey.svg">
</p>

**中文** · [English](#english)

---

## ⚠️ 免责声明（请先阅读）

本项目是一个**非官方、本地自用**的账号网关，用于把**你个人账号**的既有额度集中暴露为 OpenAI 兼容接口，方便自用工具（编辑器、脚本、本地 Agent）统一调用。

- 仅限**你本人拥有并合法使用的账号**，且仅用于**本机 / 私有环境自用**；
- 明确**反对**：批量注册小号对外提供付费 API、共享账号池、二次加壳售卖、任何商业转售；
- 上游接口可能随时变更而失效，且可能**违反上游平台服务条款**，存在**账号封禁**风险；
- 凭证以**加密**方式落盘，请勿外传、勿提交到公开仓库、勿对外暴露监听端口。

**继续使用即代表你已知悉并自行承担上述风险。** The authors assume no liability for any account suspension or terms-of-service violation.

---

## 简介

`CodeNexus` 把两类 AI 编码账号池收敛进**一个进程、一个端口、一个面板、一个 `base_url`**：

| 来源 | 出口协议 | 备注 |
|---|---|---|
| WorkBuddy / CodeBuddy | OpenAI Chat | 账号池 + 定时任务 + 积分体系 |
| Trae CN | OpenAI Chat / Codex Responses / Anthropic Messages | 四类上游端点 + 五种认证来源 |

两者对外统一为 OpenAI 兼容接口，由网关屏蔽来源差异。

---

## 特性

> 状态：🚧 早期骨架。以下为目标特性，实现进度见 [`docs/ROADMAP.md`](docs/ROADMAP.md)，完整执行计划见 [`docs/EXECUTION_PLAN.md`](docs/EXECUTION_PLAN.md)。

- **全协议超集（一个 `base_url`）**：OpenAI Chat（`/v1/chat/completions`）、OpenAI **Responses**（`/v1/responses`，Codex）、**Anthropic Messages**（`/v1/messages`，Claude Code）三类入口，均支持流式（SSE）与非流式。
- **多源账号池**：同一池内混合两类账号，按来源、额度、可用性调度。
- **扁平模型名 + 跨源自动择优**：同名模型在多来源间按健康度自动选择并可失败回退；另提供 `source/model` 别名精确指定。
- **上游适配器**：可插拔的上游接口，每个来源实现自己的鉴权、请求、流式解析与错误映射。
- **来源 B 全量认证来源**：本地凭证自动发现 / 环境变量 / 手工令牌 / 本地 CLI / 浏览器授权。
- **来源 B 全量四端点**：Remote / Solo / IDE Agent / Work Agent + 自动路由与失败回落，含风控退避与设备轮换。
- **调度与容错**：顺序 / 额度优先轮询、每账号并发槽位、429 冷却与指数退避、熔断、会话粘性。
- **统一网页控制台**：以「来源」为一等公民的单一面板 —— 账号、用量、额度、任务与签到、模型与档位、在线改配置、运行日志（前端 **Svelte** 构建后随二进制 embed）。
- **自动签到 / 保活**：按本地时区定时的签到、活跃、保活等任务（可逐项开关）。
- **凭证安全**：信封加密（数据密钥由 OS 密钥库保护：Windows DPAPI / macOS Keychain / Linux Secret Service），支持加密导出/导入迁移。
- **单文件部署**：Go 编译为单二进制，前端已内嵌，拷到任意机器即可运行。

---

## 设计决策摘要

关键方向已定稿（完整 ADR 见 [`docs/EXECUTION_PLAN.md`](docs/EXECUTION_PLAN.md) §4）：

| 决策 | 结论 |
|---|---|
| 技术栈 | **纯 Go 单二进制**，标准库优先，零运行时依赖 |
| 对外协议面 | **全协议超集**：OpenAI Chat + Responses（Codex）+ Anthropic Messages（Claude Code） |
| 模型路由 | **扁平名 + 同名跨源自动择优**，另设 `source/model` 别名精确通道 |
| 面板形态 | **统一重设计**，以「来源」为一等公民 |
| 来源 B 认证 | **全量五种** |
| 来源 B 端点 | **全量四端点 + 自动路由** |
| 凭证安全 | **信封加密 + OS 密钥库**，支持加密导出/导入 |
| 来源 B 桌面端能力 | **入站兼容纳入 v1**；**原生桥出站传输不纳入**（依赖私有 DLL，与单二进制目标冲突；列为 v2 可选） |
| 默认监听 | `127.0.0.1:6828` |

---

## 架构

```mermaid
flowchart LR
    C[OpenAI 兼容客户端<br/>编辑器 / Codex / Claude Code / 脚本]
    subgraph CN[CodeNexus 单进程]
        API[协议入口层<br/>Chat · Responses · Anthropic · models]
        ROUTER[统一路由与调度<br/>模型→来源 · 同名择优 · 会话粘性]
        POOL[(统一账号池)]
        SCHED[定时任务<br/>签到 · 保活]
        PANEL[内嵌网页控制台]
        subgraph UP[上游适配器]
            WB[WorkBuddy / CodeBuddy]
            TR[Trae CN]
        end
    end
    WBUP[WorkBuddy / CodeBuddy 上游]
    TRUP[Trae CN 上游]

    C -->|HTTP/SSE| API --> ROUTER
    ROUTER --> POOL
    SCHED --> POOL
    PANEL --- POOL
    ROUTER --> WB --> WBUP
    ROUTER --> TR --> TRUP
```

设计要点：

1. **入口统一、出口适配**：对外只暴露统一协议；对内通过上游接口屏蔽差异。
2. **池与调度解耦**：账号池只关心「有哪些凭证、状态如何」，路由只关心「这次请求该选谁」。
3. **控制台与 API 同源**：单一二进制同时服务 API 与控制台，部署面最小。

---

## 支持的来源

| 来源 | 形态 | 出口协议 |
|---|---|---|
| WorkBuddy / CodeBuddy | 账号池 + 网页控制台 | OpenAI Chat |
| Trae CN | 账号池 + 网页控制台 | OpenAI Chat / Responses / Anthropic Messages |

---

## 项目状态

🚧 **设计 / 骨架阶段**：本仓库正在按 [`docs/EXECUTION_PLAN.md`](docs/EXECUTION_PLAN.md) 分阶段落地，进度勾选见 [`docs/ROADMAP.md`](docs/ROADMAP.md)。

规划中的使用方式：

```bash
git clone https://github.com/ZicongCheung/CodeNexus.git
cd CodeNexus
go build -o codenexus ./cmd/codenexus
./codenexus -config config.json
```

首次启动将在工作目录生成 `config.json`（含随机 `api_key`，仅在日志中打印一次），随后浏览器打开 `http://127.0.0.1:6828/panel/`。

---

## 配置（规划）

`config.json` 预期字段（实现中会逐步补齐）：

```jsonc
{
  "listen": "127.0.0.1:6828",     // 监听地址，仅本机时用 127.0.0.1
  "api_key": "",                  // 客户端鉴权用；留空=不鉴权（危险，首启自动生成随机值）
  "auth_dir": "./auths",
  "data_dir": "./data",
  "pool": { "credit_floor": 100, "cooldown_seconds": 600 },
  "router": { "naming": "flat", "same_name_policy": "auto_healthy" },
  "upstreams": {
    "workbuddy": { "enabled": true },
    "trae-cn":   { "enabled": true, "auth_source": "auto", "upstream_mode": "remote", "auto_route": true }
  },
  "schedule": { "checkin_enabled": true, "keepalive_enabled": true }
}
```

> ⚠️ `api_key` 留空等于**完全不鉴权**，即使本机自用也务必设置。

### 环境变量

适合无头部署（凭证由平台的 Secret 管理，不落盘）：

| 变量 | 作用 |
|---|---|
| `CODENEXUS_LISTEN` | 覆盖监听地址 |
| `CODENEXUS_API_KEY` | 覆盖 `api_key` |
| `CODENEXUS_WORKBUDDY_TOKEN` / `_REFRESH_TOKEN` / `_UID` / `_DOMAIN` / `_EXPIRES_AT` / `_EXPIRES_IN` | 注入一个 WorkBuddy 账号 |
| `CODENEXUS_TRAE_TOKEN` / `_REFRESH_TOKEN` / `_UID` / `_LABEL` / `_EXPIRES_AT` / `_EXPIRES_IN` | 注入一个 Trae CN 账号（`_UID` 可省，网关会从 JWT 中读取） |

> 经环境变量注入的账号是**临时账号**：可用但不写入 `auths/`。

---

## 客户端接入

任何支持「自定义 `base_url` + API Key」的 OpenAI 兼容客户端均可接入：

- `base_url`：`http://127.0.0.1:6828/v1`
- `api_key`：`config.json` 中的 `api_key`
- `model`：从 `GET /v1/models` **动态获取**（上游模型目录随时变化，硬编码会 404）

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:6828/v1", api_key="<your-key>")
print([m.id for m in client.models.list().data])  # 先取真实模型 id
resp = client.chat.completions.create(
    model="<上一步的某个 id>",
    messages=[{"role": "user", "content": "你好"}],
    stream=True,
)
for chunk in resp:
    print(chunk.choices[0].delta.content or "", end="")
```

---

## 项目结构

> 目标结构（实现中逐步创建）：

```
CodeNexus/
├── cmd/codenexus/         # 程序入口
├── internal/
│   ├── api/               # 协议入口：Chat / Responses / Anthropic / models / status / healthz
│   ├── upstream/          # 上游接口 + 注册表；workbuddy/ · traecn/
│   ├── account/           # 统一账号模型 + 凭证
│   ├── pool/              # 账号池 + 选号
│   ├── router/            # 模型 → 来源 路由
│   ├── scheduler/         # 定时任务
│   ├── panel/             # 内嵌控制台（go:embed）
│   ├── store/             # 持久化（文件 + SQLite 遥测）
│   ├── crypto/            # 信封加密 + OS 密钥库
│   └── logx/              # 日志与指标
├── web/                   # 面板前端源码（Svelte + Vite）
├── docs/                  # 设计与路线图
├── go.mod
├── Dockerfile
└── README.md
```

---

## 路线图

见 [`docs/ROADMAP.md`](docs/ROADMAP.md)（渐进勾选）与 [`docs/EXECUTION_PLAN.md`](docs/EXECUTION_PLAN.md)（完整验收）。概览：

1. **P0 骨架** — 入口、配置、HTTP 服务、`/healthz`、上游接口与一条可运行闭环。
2. **P1 来源 A 适配** — WorkBuddy / CodeBuddy 上游实现（登录、签到、池调度、Chat）。
3. **P2 来源 B 基础链路** — 浏览器授权 / 手工 / 环境变量认证 + Remote 端点 + 统一池/路由 + 流式对话。
4. **P3 来源 B 认证补齐与端点扩展** — 本地凭证/CLI 认证、四端点 + 自动路由、风控退避。
5. **P4 协议超集与高级能力** — Responses / Anthropic / 工具调用 / thinking 分离 / 自动续写 / 1M。
6. **P5 统一面板 + 任务合并 + 配置热生效 + 凭证加密**。
7. **P6 打包与发布** — Windows 单 exe 优先，跨平台构建。

---

## 贡献

见 [`CONTRIBUTING.md`](CONTRIBUTING.md)。请勿在 issue / PR 中粘贴任何真实凭证。

---

## 许可证

[MIT](LICENSE) © 2026 ZicongCheung

---
---

# English

> **CodeNexus** — a gateway that pools your **WorkBuddy / CodeBuddy** and **Trae CN** accounts behind a single **OpenAI-compatible API**.

## ⚠️ Disclaimer

This is an **unofficial, local, self-hosted** account gateway that exposes **your own accounts'** quotas as an OpenAI-compatible endpoint for personal use.

- For **accounts you own and are legally allowed to use**, **locally / in private** only.
- Explicitly **opposed**: bulk-registering throwaway accounts to resell paid APIs, shared pools, repackaging/reselling.
- Upstream endpoints may break at any time and may **violate the upstream platform's Terms of Service**, risking **account suspension**.
- Credentials are stored **encrypted**. Never commit them, share them, or expose the listening port publicly.

**Use at your own risk.**

## Features

- **Full protocol superset (one `base_url`)** — OpenAI Chat, OpenAI **Responses** (Codex) and **Anthropic Messages** (Claude Code); streaming + non-streaming.
- **Multi-source account pool** — both account types in one pool, scheduled by source, credits and availability.
- **Flat model names + cross-source auto-select** — models sharing a name are picked from the healthiest source with automatic failover; `source/model` aliases pin an exact source.
- **Pluggable upstreams** — a single upstream interface; each source implements its own auth, request, streaming and error mapping.
- **Scheduling & resilience** — round-robin / credit-priority, per-account concurrency slots, 429 cooldown with exponential backoff, circuit breaking, session stickiness.
- **Unified web console** — a single panel with "source" as a first-class concept (Svelte, embedded into the binary).
- **Credential security** — envelope encryption backed by the OS keystore, with encrypted export/import.
- **Single-binary deployment** — Go single binary with the frontend embedded.

## Status & Quick Start

🚧 **Scaffolding stage** — see [`docs/EXECUTION_PLAN.md`](docs/EXECUTION_PLAN.md) for the full plan and [`docs/ROADMAP.md`](docs/ROADMAP.md) for progress.

```bash
git clone https://github.com/ZicongCheung/CodeNexus.git
cd CodeNexus
go build -o codenexus ./cmd/codenexus
./codenexus -config config.json
```

Open `http://127.0.0.1:6828/panel/`.

## Client Usage

- `base_url`: `http://127.0.0.1:6828/v1`
- `api_key`: the `api_key` from `config.json`
- `model`: fetch dynamically from `GET /v1/models`

## License

[MIT](LICENSE) © 2026 ZicongCheung
