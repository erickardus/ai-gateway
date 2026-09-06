import { writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The output directory is embedded with `go:embed all:dist`, and that directive
// fails to compile against a directory that does not exist. A clean checkout has
// no build output, so the repository tracks a .gitkeep there — which emptyOutDir
// then deletes on every build. Putting it back is what keeps `go build` working
// on a machine that has never run this.
const keepEmbedDirectiveSatisfiable = {
  name: 'keep-embed-directive-satisfiable',
  closeBundle() {
    writeFileSync(resolve(__dirname, '../internal/ui/dist/.gitkeep'), '')
  },
}

// The app is served from /ui by the gateway itself, so every emitted asset URL
// has to be rooted there rather than at /. It builds straight into
// internal/ui/dist, which is the directory go:embed reads: the alternative is a
// copy step that can be forgotten, leaving a binary serving last week's bundle.
export default defineConfig({
  base: '/ui/',
  plugins: [react(), keepEmbedDirectiveSatisfiable],
  build: {
    outDir: '../internal/ui/dist',
    emptyOutDir: true,
  },
  server: {
    // `npm run dev` proxies the API to a gateway running locally, so the UI can
    // be developed against real data with hot reload. Cookies survive because
    // the browser sees one origin.
    proxy: {
      '/ui/api': {
        target: 'http://localhost:4000',
        changeOrigin: false,
      },
    },
  },
})
