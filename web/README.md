# CodeNexus Panel (前端)

统一控制台，基于 **Svelte 5 + Vite + TypeScript**。

构建产物输出到 `../internal/panel/dist`，由 Go 的 `go:embed` 打进单二进制。

```bash
npm install
npm run dev     # 本地开发（代理到 Go 服务）
npm run build   # 产出到 internal/panel/dist
```
