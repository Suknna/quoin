import { readFileSync } from 'node:fs'
import { basename, join } from 'node:path'
import { defineConfig, devices } from '@playwright/test'

const evidenceTicket = process.env.QUOIN_EVIDENCE_DIR ? basename(process.env.QUOIN_EVIDENCE_DIR) : undefined
const ticket = process.env.QUOIN_TICKET ?? (evidenceTicket?.match(/^T\d+$/) ? evidenceTicket : 'T03')
const fixture = ticket === 'T17' ? 'label-contract' : ticket === 'T04' ? 'alerts/realtime' : 'compose'
const teardown = ticket === 'T17' ? '../test/e2e/label-contract/teardown.mjs' : '../test/e2e/compose/teardown.mjs'
const browserTickets = new Set(readFileSync(join(import.meta.dirname, '../test/e2e/browser-tickets.txt'), 'utf8').match(/^T\d+$/gm) ?? [])
const browserTicket = browserTickets.has(ticket)
const browserBaseURL = browserTicket ? 'https://127.0.0.1:18480' : 'http://127.0.0.1:18080'
const browserStack = `../.artifacts/e2e-stack-${ticket.toLowerCase()}`

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
  // Browser-ticket failure output lives under the private stack. globalTeardown
  // removes that directory on every Playwright exit, including a failed test.
  outputDir: browserTicket ? `${browserStack}/playwright-output` : undefined,
  reporter: browserTicket ? [['list']] : [
    ['list'],
    ['html', { outputDir: `../.artifacts/tickets/${ticket}/playwright-report`, open: 'never' }],
  ],
  use: {
    baseURL: browserBaseURL,
    ignoreHTTPSErrors: browserTicket,
    trace: browserTicket ? 'off' : 'retain-on-failure',
    screenshot: browserTicket ? 'off' : 'only-on-failure',
    video: browserTicket ? 'off' : 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    command: `bash ../test/e2e/${fixture}/server.sh`,
    url: 'http://127.0.0.1:18083/ready',
    reuseExistingServer: false,
    // Fresh Chromium/Lintel images can take longer than seven minutes on a
    // cold local Docker cache; readiness must cover the real stack bootstrap.
    timeout: 1_200_000,
    stdout: 'ignore',
    stderr: 'ignore',
  },
  globalTeardown: teardown,
})
