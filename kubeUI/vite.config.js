import { writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// emptyOutDir wipes ../server/web/dist on every build, which also deletes the
// .gitkeep that keeps the directory present in git. That directory MUST exist
// in a fresh checkout: server/main.go has `//go:embed all:web/dist`, and the
// CI "go" job builds without running the UI build first — with no directory,
// the embed pattern matches nothing and CI dies with
// "pattern all:web/dist: no matching files found". (Happened for real: the
// .gitkeep was collaterally deleted in 961518d.) Re-create it after every
// build so the tracked placeholder can never go missing again.
const keepDistTracked = {
  name: 'gamectl-keep-dist-tracked',
  closeBundle() {
    writeFileSync(resolve(__dirname, '../server/web/dist/.gitkeep'), '')
  },
}

// In dev (`npm run dev` or scripts/localdev.sh), Vite serves the UI on
// :5173 and proxies /api/* to the Go backend on :8088 (set by
// scripts/localdev.sh, overridable via VITE_API_TARGET). In prod build
// the bundle goes to ../server/web/dist, which the Go binary //go:embeds
// — so the deploy shape stays "single static binary" exactly like GameCTL.
const apiTarget = process.env.VITE_API_TARGET || 'http://127.0.0.1:8088'

export default defineConfig({
  plugins: [react(), keepDistTracked],
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
