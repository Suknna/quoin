// Playwright global teardown: removes every resource the e2e stack owns and
// records the disposition into the ticket cleanup evidence.
import { execSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { basename, join, resolve } from 'node:path'

export default async function globalTeardown() {
const repoRoot = join(import.meta.dirname, '..', '..', '..')
const evidenceTicket = process.env.QUOIN_EVIDENCE_DIR ? basename(process.env.QUOIN_EVIDENCE_DIR) : undefined
const ticket = process.env.QUOIN_TICKET ?? (evidenceTicket?.match(/^T\d+$/) ? evidenceTicket : '')
const browserTickets = new Set(readFileSync(join(repoRoot, 'test/e2e/browser-tickets.txt'), 'utf8').match(/^T\d+$/gm) ?? [])
const browserTicket = browserTickets.has(ticket)
const ticketSlug = ticket.toLowerCase()
const stack = join(repoRoot, '.artifacts', browserTicket ? `e2e-stack-${ticketSlug}` : 'e2e-stack')
const evidenceDir = process.env.QUOIN_EVIDENCE_DIR ?? join(repoRoot, '.artifacts', 'tickets', 'T01')
const composeProject = browserTicket ? `quoin-${ticketSlug}` : 'quoin'
const browserFixture = browserTicket ? `quoin-${ticketSlug}-auth-fixture` : ''
const composeFile = join(stack, 'state', 'quoin', 'compose', 'generated', 'compose.yaml')
const disposals = []
const cleanupFailures = []

function record(name, command) {
  try {
    execSync(command, { stdio: 'pipe', cwd: repoRoot })
    disposals.push({ name, disposition: 'removed' })
  } catch (error) {
    const detail = String(error.stderr ?? error.message).slice(0, 500)
    disposals.push({ name, disposition: 'removal-failed', detail })
    cleanupFailures.push(`${name}: ${detail}`)
  }
}

if (existsSync(composeFile)) {
  if (browserTicket) record('ticket authentication fixture', `if docker container inspect ${browserFixture} >/dev/null 2>&1; then docker rm -f ${browserFixture}; fi`)
  const downFlags = browserTicket ? 'down --volumes --remove-orphans' : 'down --remove-orphans'
  record('playwright containers+networks', `docker compose --project-name ${composeProject} --file "${composeFile}" ${downFlags}`)
  if (!browserTicket) record('playwright alertmanager+forwarder', 'docker rm -f e2e-am e2e-fwd')
}
// Every browser ticket uses a private image namespace, so teardown removes
// only images it built and never overwrites or deletes shared developer tags.
if (browserTicket) {
  record('ticket browser private images', `for image in quoin-${ticketSlug}/quoin:v0.1.0-dev quoin-${ticketSlug}/plinth:v0.1.0-dev quoin-${ticketSlug}/lintel:v0.1.0-dev quoin-${ticketSlug}/stele:v0.1.0-dev; do if docker image inspect "$image" >/dev/null 2>&1; then docker rmi "$image" || exit $?; fi; done`)
} else {
  record('playwright images', 'docker rmi quoin/quoin:v0.1.0-dev quoin/plinth:v0.1.0-dev quoin/lintel:v0.1.0-dev quoin/stele:v0.1.0-dev')
}
if (existsSync(join(stack, 'tls-proxy.pid'))) {
  record('ticket TLS proxy', `pid=$(cat "${join(stack, 'tls-proxy.pid')}"); if kill -0 "$pid" >/dev/null 2>&1; then kill "$pid"; fi`)
}
if (existsSync(join(stack, 'ready.pid'))) {
  record('ticket readiness server', `pid=$(cat "${join(stack, 'ready.pid')}"); if kill -0 "$pid" >/dev/null 2>&1; then kill "$pid"; fi`)
}
if (existsSync(join(stack, 'fixture-provider.pid'))) {
  record('ticket model fixture', `pid=$(cat "${join(stack, 'fixture-provider.pid')}"); if kill -0 "$pid" >/dev/null 2>&1; then kill "$pid"; fi`)
}
if (existsSync(stack)) {
  rmSync(stack, { recursive: true, force: true })
  disposals.push({ name: 'playwright stack directory (state, secrets copy, temp credentials)', disposition: 'removed' })
}

const cleanupPath = join(evidenceDir, 'cleanup.json')
let cleanup = {}
if (existsSync(cleanupPath)) {
  try {
    cleanup = JSON.parse(readFileSync(cleanupPath, 'utf8'))
  } catch {
    cleanup = {}
  }
}
cleanup.playwright = {
  resources: disposals,
  owned: browserTicket
    ? [`quoin-${ticketSlug} Compose containers/networks/volumes`, browserFixture, `quoin-${ticketSlug} images`, 'TLS proxy', 'readiness server', `e2e-stack-${ticketSlug} directory`]
    : ['Compose stack', 'TLS proxy', 'e2e stack directory'],
  note: 'e2e stack removed after Chromium ticket acceptance',
}
mkdirSync(evidenceDir, { recursive: true })
writeFileSync(cleanupPath, JSON.stringify(cleanup, null, 2))
if (cleanupFailures.length > 0) {
  throw new Error(`e2e cleanup failed: ${cleanupFailures.join('; ')}`)
}
}

// A browser-ticket webServer invokes this module directly when bootstrap
// fails before Playwright has registered globalTeardown. Normal successful runs
// use the exported hook above.
if (process.argv[1] && resolve(process.argv[1]) === import.meta.filename) {
  globalTeardown().catch((error) => {
    console.error(error)
    process.exitCode = 1
  })
}
