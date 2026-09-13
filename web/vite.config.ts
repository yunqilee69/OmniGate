import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  base: '/manage/',
  server: {
    host: '127.0.0.1',
    port: 27778,
    proxy: {
      '/api': 'http://127.0.0.1:27777',
      '/v1': 'http://127.0.0.1:27777',
    },
  },
  build: { outDir: '../internal/webui/dist', emptyOutDir: true },
})
