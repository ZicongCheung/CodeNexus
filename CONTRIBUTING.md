# Contributing 贡献指南

感谢你关注 CodeNexus。

## 红线（务必遵守）

- **严禁**在 issue、PR、讨论或提交中粘贴任何真实账号凭证（token / refreshToken / `storage.json` 内容等）。
  若不慎泄露，请立即在各上游平台吊销并重新授权。
- 本项目仅面向**自用账号**；**不接受**任何用于批量注册、共享池、付费转售的功能或讨论。

## 开发

要求：Go 1.22+。

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
