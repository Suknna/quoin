// T17 Playwright teardown removes only the freshly generated browser fixture
// and proves it did not alter the pre-existing `quoin_internal` attachment set.
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, realpathSync, rmSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'

const repoRoot = join(import.meta.dirname, '..', '..', '..')
const evidenceDir = process.env.QUOIN_EVIDENCE_DIR ?? join(repoRoot, '.artifacts', 'tickets', 'T17')
const fixturePath = join(evidenceDir, 'ticket17-browser-fixture.json')
const sharedNetwork = 'quoin_internal'
const resources = []
let cleanupFailed = false

function record(name, args) {
  try {
    const output = execFileSync('docker', args, { cwd: repoRoot, stdio: 'pipe' }).toString()
    resources.push({ name, command: ['docker', ...args], exitCode: 0, disposition: 'removed', output: output.slice(0, 500) })
  } catch (error) {
    cleanupFailed = true
    resources.push({
      name,
      command: ['docker', ...args],
      exitCode: Number(error.status ?? 1),
      disposition: 'removal-failed',
      detail: String(error.stderr ?? error.message).slice(0, 500),
    })
  }
}

function inspectNetwork(name) {
  try {
    const output = execFileSync(
      'docker',
      ['network', 'inspect', '--format', '{{range $id, $container := .Containers}}{{$id}}={{$container.Name}}{{println}}{{end}}', name],
      { cwd: repoRoot, stdio: 'pipe' },
    ).toString()
    return { exists: true, attachments: output.trim() === '' ? [] : output.trim().split(/\s+/).sort() }
  } catch (error) {
    const detail = String(error.stderr ?? error.message).toLowerCase()
    if (detail.includes('not found') || detail.includes('no such network')) return { exists: false, attachments: [] }
    cleanupFailed = true
    return { exists: 'inspect-failed', attachments: [], error: detail.slice(0, 500) }
  }
}

function sameSnapshot(left, right) {
  return left.exists === right.exists && JSON.stringify(left.attachments) === JSON.stringify(right.attachments)
}

function inspectLabel(target, label) {
  try {
    return execFileSync('docker', ['inspect', '--format', `{{index .Config.Labels "${label}"}}`, target], { cwd: repoRoot, stdio: 'pipe' }).toString().trim()
  } catch {
    return null
  }
}

function readFixture() {
  if (!existsSync(fixturePath)) throw new Error(`T17 fixture manifest is missing: ${fixturePath}`)
  const fixture = JSON.parse(readFileSync(fixturePath, 'utf8'))
  const required = ['runId', 'project', 'stack', 'composeFile', 'imageOverride', 'internalNetwork', 'forwarder', 'alertmanager', 'readiness', 'images']
  if (!required.every((key) => Object.hasOwn(fixture, key))) throw new Error('T17 fixture manifest has missing fields')
  if (typeof fixture.runId !== 'string' || !/^e2e-t17-[a-z0-9]+$/.test(fixture.runId)) throw new Error('T17 fixture manifest has unsafe runId')
  if (fixture.project !== `quoin-t17-e2e-${fixture.runId.slice('e2e-t17-'.length)}`) throw new Error('T17 fixture manifest project is not derived from runId')
  if (typeof fixture.stack !== 'string' || typeof fixture.composeFile !== 'string' || typeof fixture.imageOverride !== 'string') throw new Error('T17 fixture manifest has unsafe paths')
  const expectedStack = resolve(repoRoot, '.artifacts', fixture.runId)
  let stackReal
  try { stackReal = realpathSync(fixture.stack) } catch { throw new Error('T17 fixture stack is missing or cannot be resolved') }
  if (stackReal !== expectedStack) throw new Error('T17 fixture stack is not the runId-derived directory')
  if (resolve(fixture.composeFile) !== resolve(expectedStack, 'state/quoin/compose/generated/compose.yaml')) throw new Error('T17 fixture compose path is not within its stack')
  if (resolve(fixture.imageOverride) !== resolve(expectedStack, 'fixture-images.compose.yaml')) throw new Error('T17 fixture override path is not within its stack')
  if (fixture.internalNetwork !== `${fixture.project}_internal`) throw new Error('T17 fixture network is not derived from project')
  if (fixture.forwarder !== `${fixture.project}-forwarder` || fixture.alertmanager !== `${fixture.project}-alertmanager` || fixture.readiness !== `${fixture.project}-ready`) throw new Error('T17 fixture container names are not derived from project')
  if (!Array.isArray(fixture.images) || !fixture.images.every((image) => typeof image === 'string' && image.startsWith(`${fixture.project}-image/`))) throw new Error('T17 fixture image tags are not ticket-owned')
  return fixture
}

function fixtureOwnsContainer(name, runId) {
  return inspectLabel(name, 'com.quoin.fixture') === runId
}

function fixtureOwnsComposeContainers(project, runId) {
  try {
    const ids = execFileSync('docker', ['ps', '-aq', '--filter', `label=com.docker.compose.project=${project}`], { cwd: repoRoot, stdio: 'pipe' })
      .toString().trim().split(/\s+/).filter(Boolean)
    return ids.every((id) => fixtureOwnsContainer(id, runId))
  } catch {
    return false
  }
}

export default async function globalTeardown() {
  let fixture
  try {
    fixture = readFixture()
  } catch (error) {
    cleanupFailed = true
    resources.push({ name: 'T17 fixture manifest', disposition: 'unsafe-or-missing', detail: String(error) })
    fixture = null
  }

  let sharedBefore = { exists: 'baseline-missing', attachments: [] }
  let ownedAfter = { exists: 'not-inspected', attachments: [] }
  if (fixture) {
    const baselinePath = join(fixture.stack, 'shared-network-before')
    const baselineExitPath = join(fixture.stack, 'shared-network-before.exit')
    if (existsSync(baselinePath) && existsSync(baselineExitPath)) {
      const output = readFileSync(baselinePath, 'utf8').trim()
      sharedBefore = readFileSync(baselineExitPath, 'utf8').trim() === '0'
        ? { exists: true, attachments: output === '' ? [] : output.split(/\s+/).sort() }
        : { exists: false, attachments: [] }
    } else {
      cleanupFailed = true
      resources.push({ name: 'T17 shared network baseline', disposition: 'missing' })
    }

    // Remove standalone services only after proving their run label. A forged
    // manifest can therefore cause a failed cleanup, never another stack's rm.
    for (const [name, target] of [['T17 Alertmanager', fixture.alertmanager], ['T17 forwarder', fixture.forwarder], ['T17 readiness server', fixture.readiness]]) {
      if (fixtureOwnsContainer(target, fixture.runId)) record(name, ['rm', '-f', target])
      else {
        cleanupFailed = true
        resources.push({ name, disposition: 'ownership-mismatch', target })
      }
    }

    if (!fixtureOwnsComposeContainers(fixture.project, fixture.runId)) {
      cleanupFailed = true
      resources.push({ name: 'T17 Compose containers and networks', disposition: 'ownership-mismatch' })
    } else if (existsSync(fixture.composeFile) && existsSync(fixture.imageOverride)) {
      record('T17 Compose containers and networks', ['compose', '--project-name', fixture.project, '--file', fixture.composeFile, '--file', fixture.imageOverride, 'down', '--remove-orphans'])
    } else {
      cleanupFailed = true
      resources.push({ name: 'T17 Compose containers and networks', disposition: 'compose-projection-missing' })
    }

    ownedAfter = inspectNetwork(fixture.internalNetwork)
    if (ownedAfter.exists !== false) cleanupFailed = true

    for (const image of fixture.images) {
      if (inspectLabel(image, 'com.quoin.fixture') === fixture.runId) record(`T17 image tag ${image}`, ['image', 'rm', image])
      else {
        cleanupFailed = true
        resources.push({ name: `T17 image tag ${image}`, disposition: 'ownership-mismatch' })
      }
    }
    if (existsSync(fixture.stack)) {
      // realpath resolves symlinks before the final owned-directory deletion.
      const expectedStack = resolve(repoRoot, '.artifacts', fixture.runId)
      const stackReal = realpathSync(fixture.stack)
      if (stackReal !== expectedStack) {
        cleanupFailed = true
        resources.push({ name: 'T17 stack directory', disposition: 'ownership-mismatch', detail: stackReal })
      } else {
        rmSync(fixture.stack, { recursive: true, force: true })
        resources.push({ name: 'T17 stack directory (state, secrets, temporary credentials)', disposition: 'removed' })
      }
    }
  }

  const sharedAfter = inspectNetwork(sharedNetwork)
  if (!sameSnapshot(sharedBefore, sharedAfter)) cleanupFailed = true
  const networkEvidence = {
    owned: { network: fixture?.internalNetwork ?? 'not-created', expected: 'removed', after: ownedAfter },
    shared: { network: sharedNetwork, before: sharedBefore, after: sharedAfter, expected: 'unchanged' },
  }

  const cleanupPath = join(evidenceDir, 'cleanup.json')
  let cleanup = {}
  if (existsSync(cleanupPath)) {
    try { cleanup = JSON.parse(readFileSync(cleanupPath, 'utf8')) } catch { cleanup = {} }
  }
  cleanup.playwright = { resources, networks: networkEvidence, status: cleanupFailed ? 'failed' : 'passed' }
  mkdirSync(evidenceDir, { recursive: true })
  writeFileSync(cleanupPath, JSON.stringify(cleanup, null, 2))
  if (cleanupFailed) throw new Error(`T17 browser fixture cleanup failed; inspect ${cleanupPath}`)
}
