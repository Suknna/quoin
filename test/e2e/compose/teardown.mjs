// Playwright global teardown: each owned resource is removed then independently
// probed absent. A probe failure is an acceptance failure, not a best effort.
import { execSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { basename, join, resolve } from 'node:path'

export default async function globalTeardown() {
  const root = join(import.meta.dirname, '..', '..', '..')
  const ticket = process.env.QUOIN_TICKET ?? (basename(process.env.QUOIN_EVIDENCE_DIR ?? '').match(/^T\d+$/)?.[0] ?? '')
  const browserTicket = new Set(readFileSync(join(root, 'test/e2e/browser-tickets.txt'), 'utf8').match(/^T\d+$/gm) ?? []).has(ticket)
  const slug = ticket.toLowerCase(), stack = join(root, '.artifacts', browserTicket ? `e2e-stack-${slug}` : 'e2e-stack')
  const evidenceDir = process.env.QUOIN_EVIDENCE_DIR ?? join(root, '.artifacts', 'tickets', 'T01')
  const project = browserTicket ? `quoin-${slug}` : 'quoin', fixture = browserTicket ? `quoin-${slug}-auth-fixture` : ''
  const compose = join(stack, 'state', 'quoin', 'compose', 'generated', 'compose.yaml'), resources = []
  const shell = (command) => { try { execSync(command, { cwd: root, stdio: 'pipe', shell: '/bin/bash' }); return 0 } catch (e) { return e.status ?? 1 } }
  const removeProbe = (name, kind, removalCommand, probeCommand) => {
    const removeExitCode = shell(removalCommand), probeExitCode = shell(probeCommand)
    const absent = probeExitCode !== 0
    resources.push({ name, kind, removalCommand, removeExitCode, probeCommand, probeExitCode, observedFinalState: absent ? 'absent' : 'present' })
    if (removeExitCode !== 0 || !absent) throw new Error(`cleanup failed for ${name}: remove=${removeExitCode} probe=${probeExitCode}`)
  }
  if (existsSync(compose)) {
    removeProbe('ticket authentication fixture', 'container', `docker rm -f ${fixture} >/dev/null 2>&1 || true`, `docker container inspect ${fixture} >/dev/null 2>&1`)
    removeProbe('compose project containers', 'container-set', `docker compose --project-name ${project} --file "${compose}" down --volumes --remove-orphans >/dev/null`, `test -n "$(docker ps -aq --filter label=com.docker.compose.project=${project})"`)
    removeProbe('compose project networks', 'network-set', 'true', `test -n "$(docker network ls -q --filter label=com.docker.compose.project=${project})"`)
    removeProbe('compose project volumes', 'volume-set', 'true', `test -n "$(docker volume ls -q --filter label=com.docker.compose.project=${project})"`)
  }
  for (const image of browserTicket ? [`quoin-${slug}/quoin:v0.1.0-dev`,`quoin-${slug}/plinth:v0.1.0-dev`,`quoin-${slug}/lintel:v0.1.0-dev`,`quoin-${slug}/stele:v0.1.0-dev`] : []) removeProbe(`private image ${image}`, 'image', `docker rmi ${image} >/dev/null 2>&1 || true`, `docker image inspect ${image} >/dev/null 2>&1`)
  const shellAsync = (command) => new Promise((resolveProgress) => { /* synchronous probing helper below */ })
  const awaitGone = async (name, file) => {
    // SIGTERM then bounded wait then SIGKILL: an immediate kill -0 races a
    // process that has received the signal but not yet exited.
    const pid = join(stack, file)
    if (!existsSync(pid)) { resources.push({ name, kind: 'host-process', removalCommand: 'not started', probeCommand: 'pid file absent', probeExitCode: 1, observedFinalState: 'absent' }); return }
    const value = readFileSync(pid, 'utf8').trim()
    shell(`kill ${value} >/dev/null 2>&1 || true`)
    for (let i = 0; i < 50 && shell(`kill -0 ${value} >/dev/null 2>&1`) === 0; i++) await new Promise((done) => setTimeout(done, 100))
    if (shell(`kill -0 ${value} >/dev/null 2>&1`) === 0) shell(`kill -9 ${value} >/dev/null 2>&1 || true`)
    for (let i = 0; i < 50 && shell(`kill -0 ${value} >/dev/null 2>&1`) === 0; i++) await new Promise((done) => setTimeout(done, 100))
    const probeExitCode = shell(`kill -0 ${value} >/dev/null 2>&1`)
    const absent = probeExitCode !== 0
    resources.push({ name, kind: 'host-process', removalCommand: `kill ${value} (SIGTERM, then SIGKILL after 5s)`, probeCommand: `kill -0 ${value}`, probeExitCode, observedFinalState: absent ? 'absent' : 'present' })
    if (!absent) throw new Error(`cleanup failed for ${name}: still running after SIGKILL`)
  }
  for (const [name, file] of [['TLS proxy','tls-proxy.pid'],['readiness server','ready.pid'],['model fixture','fixture-provider.pid'],['Thanos fixture','fixture-thanos.pid']]) await awaitGone(name, file)
  if (existsSync(stack)) { rmSync(stack, { recursive: true, force: true }) }
  const stackProbe = shell(`test -e ${stack}`); resources.push({ name:'stack directory, TLS keys/cert, generated config, admin password, session cookie, browser profile state', kind:'file-tree', removalCommand:`rm -rf ${stack}`, probeCommand:`test -e ${stack}`, probeExitCode:stackProbe, observedFinalState: stackProbe !== 0 ? 'absent' : 'present' }); if (stackProbe === 0) throw new Error('stack directory remains')
  const phase = process.env.QUOIN_TEARDOWN_PHASE ?? 'bootstrap-exit'
  mkdirSync(evidenceDir, { recursive: true })
  const cleanupFile = join(evidenceDir, 'cleanup.json')
  // The bootstrap EXIT trap is the first teardown and owns the real resource
  // dispositions; Playwright's later globalTeardown must verify-and-append,
  // never overwrite that record with a no-op pass.
  let merged = { phases: [] }
  if (existsSync(cleanupFile)) { try { merged = JSON.parse(readFileSync(cleanupFile, 'utf8')) } catch { merged = { phases: [] } } }
  merged.phases.push({ phase, resources })
  writeFileSync(cleanupFile, JSON.stringify(merged, null, 2))
}
if (process.argv[1] && resolve(process.argv[1]) === import.meta.filename) globalTeardown().catch((error)=>{console.error(error);process.exitCode=1})
