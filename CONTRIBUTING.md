# Contributing 贡献指南

感谢你关注 CodeNexus。

## 红线（务必遵守）

- **严禁**在 issue、PR、讨论或提交中粘贴任何真实账号凭证（token / refreshToken / `storage.json` 内容等）。
  若不慎泄露，请立即在各上游平台吊销并重新授权。
- 本项目仅面向**自用账号**；**不接受**任何用于批量注册、共享池、付费转售的功能或讨论。

## 实现纪律

- **独立重写、不雷同**：可以参考既有实现的协议事实，但**不复制代码、不逐行移植、不搬运文件**；命名与代码结构自成一体。
- 新增上游实现集中在 `internal/upstream/<source>/`，通过统一上游接口暴露。
- 不引入不必要的外部依赖；标准库优先。

## 开发

要求：Go 1.26+（`go.mod` 为准），Node 22+（仅在重建面板前端时需要）。

构建与测试统一走 `scripts/build.sh`（它是 CI 与本地共用的**唯一事实源**）：

```bash
bash scripts/build.sh test     # gofmt + go vet + go test
bash scripts/build.sh build    # 当前平台产物（前端口已 embed）
bash scripts/build.sh web      # 重建面板前端（web/ → internal/panel/dist）
```

仅 Go 部分也可直接用标准工具：

```bash
go build ./...
go vet ./...
go test ./...
```

## 提交

- 分支命名：`feat/...`、`fix/...`、`docs/...`。
- 提交信息：遵循 [Conventional Commits](https://www.conventionalcommits.org/)（`feat:` / `fix:` / `docs:` / `refactor:` / `chore:`）。
- PR 请说明动机、改动范围与验证方式；涉及上游协议变更的，附最小复现与脱敏日志。

## 目录约定

- `cmd/` 仅放入口，业务逻辑放 `internal/`。
- 上游相关实现集中在 `internal/upstream/<source>/`，通过统一接口暴露。
- 面板前端源码在 `web/`，构建产物 embed 进 `internal/panel/dist`（**必须提交**）。
- 构建/发布脚本在 `scripts/`（`build.sh` 为唯一事实源）；CI 位于 `.github/workflows/`，只调用该脚本，不复制构建步骤。
