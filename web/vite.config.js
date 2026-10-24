import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

// The API port comes from config.json so the dev proxy follows it if you move
// the server. Falls back to the default when config.json is missing, because
// vite must still start for anyone who only touches the frontend.
let apiTarget = 'http://127.0.0.1:6875'
try {
  const cfg = JSON.parse(readFileSync(fileURLToPath(new URL('../config.json', import.meta.url)), 'utf8'))
  const port = String(cfg.listen || '').split(':').pop()
  if (port) apiTarget = `http://127.0.0.1:${port}`
} catch {
  /* keep the default */
}

// The built app is emitted straight into internal/panel/dist so it can be
// embedded into the Go binary via go:embed.
export default defineConfig({
  plugins: [svelte()],
  base: '/panel/',
  build: {
    outDir: '../internal/panel/dist',
    emptyOutDir: true,
  },
  // Dev only. `scripts/dev.mjs` runs the API with `go run` on the configured
  // port while vite serves the live frontend here, so proxying /panel/api lets
  // one URL give you both hot reload and real data. Production ignores this
  // block entirely — the Go binary serves the embedded dist itself.
  server: {
    port: 5173,
    host: '127.0.0.1',
    strictPort: true,
    proxy: {
      '^/panel/api': {
        target: apiTarget,
        changeOrigin: true,
      },
      // The rail's mark is embedded in the Go binary, not in the SPA bundle:
      // without this the dev server answers 404 for the one image the shell
      // renders before anything else.
      '^/panel/logo.png': {
        target: apiTarget,
        changeOrigin: true,
      },
    },
  },
})
