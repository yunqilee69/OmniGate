import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    host: '127.0.0.1',
    port: 17778,
    proxy: {
      '/api': 'http://127.0.0.1:17777',
      '/v1': 'http://127.0.0.1:17777',
    },
  },
  build: { outDir: '../internal/webui/dist', emptyOutDir: true },
})
