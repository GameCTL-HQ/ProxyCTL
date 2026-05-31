import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// In dev (`npm run dev` or scripts/localdev.sh), Vite serves the UI on
// :5173 and proxies /api/* to the Go backend on :8088 (set by
// scripts/localdev.sh, overridable via VITE_API_TARGET). In prod build
// the bundle goes to ../server/web/dist, which the Go binary //go:embeds
// — so the deploy shape stays "single static binary" exactly like GameCTL.
const apiTarget = process.env.VITE_API_TARGET || 'http://127.0.0.1:8088'

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../server/web/dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/api': {
        target: apiTarget,
        changeOrigin: true,
      },
    },
  },
})
