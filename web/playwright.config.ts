import { basename } from 'node:path'
import { defineConfig, devices } from '@playwright/test'

const evidenceTicket = process.env.QUOIN_EVIDENCE_DIR ? basename(process.env.QUOIN_EVIDENCE_DIR) : undefined
const ticket = process.env.QUOIN_TICKET ?? (evidenceTicket?.match(/^T\d+$/) ? evidenceTicket : 'T03')
const fixture = ticket === 'T17' ? 'label-contract' : ticket === 'T04' ? 'alerts/realtime' : 'compose'
const teardown = ticket === 'T17' ? '../test/e2e/label-contract/teardown.mjs' : '../test/e2e/compose/teardown.mjs'
const browserBaseURL = ticket === 'T20' ? 'https://127.0.0.1:18480' : 'http://127.0.0.1:18080'

export default defineConfig({
  testDir: './e2e',
  // This spec asserts an empty Label Contract initial state, which is not true
  // in the shared multi-ticket fixture. It is collected only by its frozen
  // ticket command, where the isolated T17 stack provides that initial state.
  testIgnore: ticket === 'T17' ? undefined : '**/label-contract.spec.ts',
  timeout: 90_000,
  expect: { timeout: 15_000 },
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  // Browser-login policy forbids retaining page-derived reports, traces,
  // screenshots, or video. Other ticket suites keep their normal diagnostics.
  // T20's failure output lives under the private stack. globalTeardown removes
  // that directory on every Playwright exit, including a failed test.
  outputDir: ticket === 'T20' ? '../.artifacts/e2e-stack-t20/playwright-output' : undefined,
  reporter: ticket === 'T20' ? [['list']] : [
    ['list'],
    ['html', { outputDir: `../.artifacts/tickets/${ticket}/playwright-report`, open: 'never' }],
  ],
  use: {
    baseURL: browserBaseURL,
    ignoreHTTPSErrors: ticket === 'T20',
    trace: ticket === 'T20' ? 'off' : 'retain-on-failure',
    screenshot: ticket === 'T20' ? 'off' : 'only-on-failure',
    video: ticket === 'T20' ? 'off' : 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    command: `bash ../test/e2e/${fixture}/server.sh`,
    url: 'http://127.0.0.1:18083/ready',
    reuseExistingServer: false,
    timeout: 420_000,
    stdout: 'ignore',
    stderr: 'ignore',
  },
  globalTeardown: teardown,
})
