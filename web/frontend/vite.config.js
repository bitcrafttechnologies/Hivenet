import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import tailwindcss from '@tailwindcss/vite';

// Build order step 8: the built output lands in ../dist, which is the
// directory Go's `//go:embed all:dist` (web/web.go) embeds into the binary.
// Relative base so the emitted <script>/<link> tags work when served from
// the Go HTTP server's static handler at "/", not just Vite's own preview.
export default defineConfig({
  plugins: [svelte(), tailwindcss()],
  base: './',
  build: {
    outDir: '../dist',
    emptyOutDir: true,
  },
  server: {
    // Lets `npm run dev` talk to a hivenet backend running on the VM at
    // :8080 without CORS gymnastics, for local iteration only -- the
    // production path is always the embedded, same-origin build.
    proxy: {
      '/api': 'http://localhost:8080',
      '/ws': { target: 'ws://localhost:8080', ws: true },
    },
  },
});
