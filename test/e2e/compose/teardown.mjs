// Playwright global teardown: removes every resource the e2e stack owns and
// records the disposition into the ticket cleanup evidence.
import { execSync } from 'node:child_process'
import { existsSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

export default async function globalTeardown() {
const repoRoot = join(import.meta.dirname, '..', '..', '..')
const stack = join(repoRoot, '.artifacts', 'e2e-stack')
const evidenceDir = process.env.QUOIN_EVIDENCE_DIR ?? join(repoRoot, '.artifacts', 'tickets', 'T01')
const composeFile = join(stack, 'state', 'quoin', 'compose', 'generated', 'compose.yaml')
const disposals = []

function record(name, command) {
  try {
    execSync(command, { stdio: 'pipe', cwd: repoRoot })
    disposals.push({ name, disposition: 'removed' })
  } catch (error) {
    disposals.push({ name, disposition: 'removal-failed', detail: String(error.stderr ?? error.message).slice(0, 500) })
  }
}

if (existsSync(composeFile)) {
  record('playwright containers+networks', `docker compose --project-name quoin --file "${composeFile}" down --remove-orphans`)
  record('playwright alertmanager+forwarder', 'docker rm -f e2e-am e2e-fwd')
}
record('playwright images', 'docker rmi quoin/quoin:v0.1.0-dev quoin/plinth:v0.1.0-dev quoin/lintel:v0.1.0-dev quoin/stele:v0.1.0-dev')
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
cleanup.playwright = { resources: disposals, note: 'e2e stack removed after Chromium ticket acceptance' }
writeFileSync(cleanupPath, JSON.stringify(cleanup, null, 2))
}
