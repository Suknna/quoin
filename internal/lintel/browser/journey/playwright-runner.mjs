// The only executable Journey source for Lintel Browser v1. It intentionally
// exports no generic action protocol: every catalog version maps to one fixed,
// reviewed Playwright function below.
import { chromium } from 'playwright-core'

function requireRootedPath(value) {
  if (typeof value !== 'string' || !value.startsWith('/') || /\s/.test(value)) {
    throw new Error('journey parameter "path" must be a rooted path without whitespace')
  }
  return value
}

async function statusMarkerV2(page, startURL, params) {
  const target = new URL(requireRootedPath(params?.path), new URL(startURL).origin)
  await page.goto(target.toString(), { waitUntil: 'commit', timeout: 30_000 })
  const marker = page.locator('[data-quoin-status]')
  await marker.waitFor({ state: 'attached', timeout: 15_000 })
  const statusText = (await marker.textContent({ timeout: 5_000 }))?.trim().slice(0, 1000)
  if (!statusText) throw new Error('status marker had no text content')
  return { output: { statusText }, trace: [{ kind: 'navigate', path: target.pathname }, { kind: 'read_status_marker', length: statusText.length }] }
}

async function run(input) {
  const browser = await chromium.connectOverCDP(input.endpoint, { timeout: 3_000 })
  try {
    const pages = browser.contexts().flatMap(context => context.pages())
    if (pages.length !== 1) throw new Error('Journey operation must expose exactly one page')
    if (input.mode === 'probe') {
      // Probe classification needs the committed response URL, not every
      // unrelated resource's load event.
      await pages[0].goto(input.startUrl, { waitUntil: 'commit', timeout: 30_000 })
      return { output: {}, trace: [{ kind: 'probe_navigation' }] }
    }
    if (input.mode !== 'journey' || input?.journey?.id !== 'page.status-marker.v1' || input.journey.version !== 2) {
      throw new Error('unsupported versioned Playwright Journey')
    }
    return await statusMarkerV2(pages[0], input.startUrl, input.journey.params)
  } finally {
    await browser.close()
  }
}

let input = ''
process.stdin.setEncoding('utf8')
process.stdin.on('data', chunk => { input += chunk })
process.stdin.on('end', async () => {
  try {
    process.stdout.write(`${JSON.stringify(await run(JSON.parse(input)))}\n`)
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    process.exitCode = 1
  }
})
