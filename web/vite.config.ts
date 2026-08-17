import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../internal/gen/web/dist',
    emptyOutDir: true,
    sourcemap: false,
    manifest: true,
  },
  server: {
    port: 5173,
    strictPort: true,
  },
  test: {
    // Vitest owns unit tests only; Playwright owns the e2e specs under e2e/.
    include: ['src/**/*.test.{ts,tsx}'],
    environment: 'jsdom',
  },
})
