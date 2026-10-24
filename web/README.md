# CodeNexus Panel（控制台前端）

统一控制台，基于 **Svelte 5 + Vite**（纯 JavaScript，无 TypeScript）。

构建产物输出到 `../internal/panel/dist`，由 Go 的 `go:embed` 打进单二进制。

```bash
npm install
npm run dev            # 开发服务器：5173，/panel/api 代理到 config.json 的 listen 端口
npm run build          # 产出到 internal/panel/dist，随后 go build 即带上新前端
npm run check:tokens   # 设计令牌守卫（改动 theme.css 或组件样式后必跑）
```

约定：

- 所有设计令牌只在 `src/theme.css` 定义，组件里只引用不新造；
- 所有界面文案集中在 `src/strings.js`，不做 i18n 切换；
- `npm run dev` 需要后端同时在跑（`go run ./cmd/codenexus -config config.json`）。
