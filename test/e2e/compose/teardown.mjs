// Playwright global teardown: removes every resource the e2e stack owns and
// records the disposition into the ticket cleanup evidence.
import { execSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'

export default async function globalTeardown() {
const repoRoot = join(import.meta.dirname, '..', '..', '..')
const ticket = process.env.QUOIN_TICKET ?? ''
const stack = join(repoRoot, '.artifacts', ticket === 'T20' ? 'e2e-stack-t20' : 'e2e-stack')
const evidenceDir = process.env.QUOIN_EVIDENCE_DIR ?? join(repoRoot, '.artifacts', 'tickets', 'T01')
const composeProject = ticket === 'T20' ? 'quoin-t20' : 'quoin'
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
  if (ticket === 'T20') record('ticket authentication fixture', 'if docker container inspect quoin-t20-auth-fixture >/dev/null 2>&1; then docker rm -f quoin-t20-auth-fixture; fi')
  const downFlags = ticket === 'T20' ? 'down --volumes --remove-orphans' : 'down --remove-orphans'
  record('playwright containers+networks', `docker compose --project-name ${composeProject} --file "${composeFile}" ${downFlags}`)
  if (ticket !== 'T20') record('playwright alertmanager+forwarder', 'docker rm -f e2e-am e2e-fwd')
}
// T20 uses a private image namespace, so it removes only the images it built
// and never overwrites or deletes shared developer image tags.
if (ticket === 'T20') {
  record('ticket-20 private images', 'for image in quoin-t20/quoin:v0.1.0-dev quoin-t20/plinth:v0.1.0-dev quoin-t20/lintel:v0.1.0-dev quoin-t20/stele:v0.1.0-dev; do if docker image inspect "$image" >/dev/null 2>&1; then docker rmi "$image" || exit $?; fi; done')
} else {
  record('playwright images', 'docker rmi quoin/quoin:v0.1.0-dev quoin/plinth:v0.1.0-dev quoin/lintel:v0.1.0-dev quoin/stele:v0.1.0-dev')
}
if (existsSync(join(stack, 'tls-proxy.pid'))) {
  record('ticket TLS proxy', `pid=$(cat "${join(stack, 'tls-proxy.pid')}"); if kill -0 "$pid" >/dev/null 2>&1; then kill "$pid"; fi`)
}
if (existsSync(join(stack, 'ready.pid'))) {
  record('ticket readiness server', `pid=$(cat "${join(stack, 'ready.pid')}"); if kill -0 "$pid" >/dev/null 2>&1; then kill "$pid"; fi`)
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
  owned: ticket === 'T20'
    ? ['quoin-t20 Compose containers/networks/volumes', 'quoin-t20-auth-fixture', 'quoin-t20 images', 'TLS proxy', 'readiness server', 'e2e-stack-t20 directory']
    : ['Compose stack', 'TLS proxy', 'e2e stack directory'],
  note: 'e2e stack removed after Chromium ticket acceptance',
}
mkdirSync(evidenceDir, { recursive: true })
writeFileSync(cleanupPath, JSON.stringify(cleanup, null, 2))
if (cleanupFailures.length > 0) {
  throw new Error(`e2e cleanup failed: ${cleanupFailures.join('; ')}`)
}
}

// The T20 webServer invokes this module directly when bootstrap fails before
// Playwright has registered globalTeardown. Normal successful runs use the
// exported hook above.
if (process.argv[1] && resolve(process.argv[1]) === import.meta.filename) {
  globalTeardown().catch((error) => {
    console.error(error)
    process.exitCode = 1
  })
}
