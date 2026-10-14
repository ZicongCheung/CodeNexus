import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// The built app is emitted straight into internal/panel/dist so it can be
// embedded into the Go binary via go:embed.
export default defineConfig({
  plugins: [svelte()],
  base: '/panel/',
  build: {
    outDir: '../internal/panel/dist',
    emptyOutDir: true,
  },
})
