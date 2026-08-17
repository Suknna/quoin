import { defineConfig, devices } from '@playwright/test'

export default defineConfig({
  testDir: './e2e',
  timeout: 90_000,
  expect: { timeout: 15_000 },
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  reporter: [
    ['list'],
    ['html', { outputDir: '../.artifacts/tickets/T03/playwright-report', open: 'never' }],
  ],
  use: {
    baseURL: 'http://127.0.0.1:18080',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    command: 'bash ../test/e2e/compose/server.sh',
    url: 'http://127.0.0.1:18083/ready',
    reuseExistingServer: false,
    timeout: 420_000,
    stdout: 'ignore',
    stderr: 'ignore',
  },
  globalTeardown: '../test/e2e/compose/teardown.mjs',
})
