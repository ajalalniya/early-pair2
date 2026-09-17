/// <reference types="vitest/config" />
import { fileURLToPath, URL } from 'node:url'
import { defineConfig, loadEnv } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  // In dev the API is proxied so relative `/api/v1` works without CORS setup.
  const backend = env.EARLYPAIR_BACKEND ?? 'http://localhost:8080'

  return {
    plugins: [vue()],
    resolve: {
      alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
    },
    server: {
      port: 5173,
      strictPort: true,
      proxy: {
        '/api/v1/stream': { target: backend.replace(/^http/, 'ws'), ws: true },
        '/api/v1': { target: backend, changeOrigin: true },
      },
    },
    build: {
      target: 'es2022',
      sourcemap: true,
      chunkSizeWarningLimit: 900,
    },
    test: {
      environment: 'jsdom',
      include: ['tests/unit/**/*.spec.ts'],
      globals: false,
    },
  }
})
