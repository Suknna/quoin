import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "@playwright/test";
import { chromium } from "playwright-core";

// T41 UI qualification (issue #64). Drives the REAL release browser
// artifacts (the Playwright-locked Chromium build and the
// qualification-resolved branded Chrome) through the fixed catalog matrix
// — every locally-native browser/arch × viewport × motion cell — against
// the live Compose-qualified Quoin site, and records one typed result per
// catalog cell for the ci/verify-ui-automation assert phase.
//
// Aspect mapping onto the frozen ui.automated ids: dom-contracts covers
// routing/deep-link/landmarks plus the login contract; domain-behavior
// covers seeded alert flows and real waiting states; the rest are as
// named. Never via the Lintel runtime, Journeys or the Explorer path
// (VERIFY-OBSERVATION-003: plain UI automation only).

const stackDir = join(import.meta.dirname, "..", "..", ".artifacts", "e2e-stack");
//The coordinator phases pass QUOIN_UI_MATRIX_DIR explicitly; the
//standalone acceptance command inherits the ticket evidence dir.
const evidenceDir = process.env.QUOIN_EVIDENCE_DIR ?? "";
const evidenceRoot = evidenceDir
  ? evidenceDir.startsWith("/")
    ? evidenceDir
    : join(import.meta.dirname, "..", "..", evidenceDir)
  : undefined;
const matrixDir = process.env.QUOIN_UI_MATRIX_DIR ?? (evidenceRoot ? join(evidenceRoot, "ui-matrix") : "");
const siteURL = process.env.QUOIN_UI_SITE ?? "https://127.0.0.1:18480";
const axeSource = readFileSync(join(import.meta.dirname, "fixtures", "axe-core.min.js.txt"), "utf8");

interface LocalSubject {
  cell_id: string;
  browser_subject: string;
  architecture: string;
  sha256: string;
  build: string;
  executable: string;
  viewport_css_px: number;
  motion_mode: string;
}

interface CellRecord {
  cell_id: string;
  browser: {
    schema: string;
    cell_id: string;
    browser_subject: string;
    architecture: string;
    url: string;
    sha256: string;
    bytes: number;
    version: string;
    build: string;
    executable: string;
  };
  started_at: string;
  finished_at: string;
  assertions: Record<string, { result: "passed" | "failed"; detail?: string }>;
  artifacts: Record<string, string>;
}

function readNewPassword(): string {
  const path = join(stackDir, "admin-new-password");
  if (!existsSync(path)) {
    throw new Error("new password fixture missing: " + path);
  }
  return readFileSync(path, "utf8").trim();
}

function loadLocalSubjects(): LocalSubject[] {
  if (!matrixDir) {
    throw new Error("QUOIN_UI_MATRIX_DIR is required for the T41 matrix spec");
  }
  const path = join(matrixDir, "local-subjects.json");
  if (!existsSync(path)) {
    throw new Error("local subject index missing: " + path);
  }
  const hostArch = "linux/" + (process.arch === "arm64" ? "arm64" : "amd64");
  const subjects = JSON.parse(readFileSync(path, "utf8")) as LocalSubject[];
  return subjects.filter((subject) => subject.architecture === hostArch);
}

function loadExistingResults(): CellRecord[] {
  const path = join(matrixDir, "matrix-results.json");
  if (!existsSync(path)) return [];
  const parsed = JSON.parse(readFileSync(path, "utf8"));
  return (parsed.cells ?? []) as CellRecord[];
}

function persistResults(cells: CellRecord[]): void {
  writeFileSync(
    join(matrixDir, "matrix-results.json"),
    JSON.stringify(
      {
        schema: "quoin-ui-automation-results-v1",
        invocation_id: process.env.QUOIN_UI_INVOCATION ?? "",
        generated_at: new Date().toISOString(),
        cells,
      },
      null,
      2,
    ),
  );
}

async function login(page: import("@playwright/test").Page, password: string): Promise<void> {
  await page.goto(siteURL + "/");
  await page.fill("#username", "admin");
  await page.fill("#password", password);
  await page.getByRole("button", { name: "登录" }).click();
  await expect(page.getByRole("navigation", { name: "全局模块" })).toBeVisible({
    timeout: 30_000,
  });
}

//The runner auto-starts tracing on contexts created inside a test when
//use.trace is retain-on-failure; these cells own their tracing
//artifacts, so the runner-level tracing stays off.
test.use({ trace: "off", screenshot: "off", video: "off" })

test.describe("T41 固定矩阵 UI 资格 @ticket-41", () => {
  test("release 浏览器工件跨视口/动效矩阵的七方面证明", async () => {
    test.setTimeout(60 * 60 * 1000);
    const subjects = loadLocalSubjects();
    expect(subjects.length).toBeGreaterThan(0);
    const password = readNewPassword();
    const records = loadExistingResults().filter(
      (record) => !subjects.some((subject) => subject.cell_id === record.cell_id),
    );
    const failures: string[] = [];

    for (const subject of subjects) {
      try {
        const { record } = await runCell(subject, password);
        records.push(record);
        persistResults(records);
      } catch (error) {
        failures.push(subject.cell_id + ": " + describeError(error));
      }
    }
    expect(failures, "matrix cells with failures").toEqual([]);
  });
});

function describeError(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function freshRecord(subject: LocalSubject): CellRecord {
  return {
    cell_id: subject.cell_id,
    browser: {
      schema: "quoin-ui-browser-subject-v1",
      cell_id: subject.cell_id,
      browser_subject: subject.browser_subject,
      architecture: subject.architecture,
      url: "",
      sha256: subject.sha256,
      bytes: 0,
      version: "",
      build: subject.build,
      executable: subject.executable,
    },
    started_at: new Date().toISOString(),
    finished_at: new Date().toISOString(),
    assertions: {},
    artifacts: {},
  };
}

//drawerToggle returns the module-navigation toggle when this viewport
//actually renders one; 768 CSS px keeps the desktop layout, so presence,
//never a width threshold, decides the drawer flow.
async function drawerToggle(page: import("@playwright/test").Page) {
  const toggle = page.getByRole("button", { name: "打开模块导航" });
  return (await toggle.count()) > 0 && (await toggle.first().isVisible()) ? toggle.first() : null;
}

//openModules ensures the module navigation buttons are visible for
//interaction, opening the drawer only when it is collapsed.
async function openModules(page: import("@playwright/test").Page) {
  const toggle = await drawerToggle(page);
  if (toggle && (await toggle.getAttribute("aria-expanded")) === "false") {
    await toggle.click();
    //The drawer slides in over 180ms; measurements before it settles
    //would misread the moving overlay as focus occlusion.
    await page.waitForTimeout(350);
  }
}

//The bootstrap fixture alerts are short-lived: Alertmanager expires
//amtool-added alerts and send_resolved drains the Firing list. Later
//cells re-seed a probe alert through the same real journey
//(Alertmanager v2 -> forwarder -> Stele -> Quoin -> SSE); one fixed
//startsAt keeps every re-seed the same occurrence.
const matrixAlertStartsAt = new Date(Date.now() - 60_000).toISOString();
let matrixAlertSeeded = false;
async function ensureMatrixAlert(): Promise<void> {
  if (matrixAlertSeeded) return;
  const response = await fetch("http://127.0.0.1:19093/api/v2/alerts", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify([
      {
        labels: { alertname: "T41MatrixProbe", severity: "warning", instance: "matrix-1", job: "quoin" },
        startsAt: matrixAlertStartsAt,
        //A far-future endsAt keeps the probe firing for the whole matrix
        //pass; without one Alertmanager's resolve_timeout would retire it
        //within minutes exactly like the bootstrap fixture alerts.
        endsAt: new Date(Date.now() + 4 * 60 * 60 * 1000).toISOString(),
        generatorURL: "",
      },
    ]),
  });
  if (!response.ok) throw new Error("matrix alert re-seed rejected by Alertmanager: " + response.status);
  matrixAlertSeeded = true;
}

async function runCell(subject: LocalSubject, password: string): Promise<{ record: CellRecord }> {
  const record = freshRecord(subject);
  const mark = (id: string, error?: unknown) => {
    record.assertions[id] = error
      ? { result: "failed", detail: describeError(error).slice(0, 400) }
      : { result: "passed" };
  };
  const artifactsDir = join(matrixDir, "artifacts", subject.cell_id);
  mkdirSync(artifactsDir, { recursive: true });
  record.started_at = new Date().toISOString();

  const browser = await chromium.launch({ executablePath: subject.executable });
  try {
    const context = await browser.newContext({
      viewport: { width: subject.viewport_css_px, height: 900 },
      reducedMotion: subject.motion_mode === "reduced" ? "reduce" : "no-preference",
      //The e2e edge terminates an ephemeral self-signed certificate for
      //the declared public origin; the cookie and WebSocket origin
      //contracts bind that origin, not the plain-loopback upstream.
      ignoreHTTPSErrors: true,
    });
    await context.tracing.start({ screenshots: true, snapshots: true });
    const page = await context.newPage();
    page.setDefaultTimeout(30_000);
    page.setDefaultNavigationTimeout(45_000);
    const log = (aspect: string) =>
      console.log(new Date().toISOString() + " " + subject.cell_id + " " + aspect);

    // [dom-contracts] routing, deterministic routes, landmarks
    try {
      await login(page, password);
      await expect(page.getByRole("heading", { name: "告警", level: 1 })).toBeVisible({
        timeout: 30_000,
      });
      const lang = await page.evaluate(() => document.documentElement.lang);
      expect(lang).toBe("zh-CN");
      await page.goto(siteURL + "/knowledge");
      await expect(page.getByRole("heading", { name: "知识", level: 1 })).toBeVisible();
      await page.goBack();
      await expect(page.getByRole("heading", { name: "告警", level: 1 })).toBeVisible();
      mark("dom-contracts");
      log("dom-contracts");
    } catch (error) {
      mark("dom-contracts", error);
    }

    // [keyboard-navigation] deterministic state, drawer when present,
    // Enter activation
    try {
      await page.goto(siteURL + "/alerts");
      await expect(page.getByRole("heading", { name: "告警", level: 1 })).toBeVisible();
      await openModules(page);
      const knowledge = page.getByRole("button", { name: "知识", exact: true });
      await knowledge.focus();
      await expect(knowledge).toBeFocused();
      await page.keyboard.press("Enter");
      await expect(page.getByRole("heading", { name: "知识", level: 1 })).toBeVisible();
      mark("keyboard-navigation");
      log("keyboard-navigation");
    } catch (error) {
      mark("keyboard-navigation", error);
    }

    // [focus-visibility-and-occlusion] visible focus ring, unoccluded
    try {
      await page.goto(siteURL + "/alerts");
      await expect(page.getByRole("heading", { name: "告警", level: 1 })).toBeVisible();
      await openModules(page);
      const alerts = page.getByRole("button", { name: "告警", exact: true });
      await alerts.focus();
      await expect(alerts).toBeFocused();
      const focusCheck = await page.evaluate(() => {
        const element = document.activeElement;
        if (!element) return { visible: false, occluded: true };
        const style = getComputedStyle(element);
        const decorates =
          style.outlineStyle !== "none" ||
          (style.outlineWidth !== "" && style.outlineWidth !== "0px") ||
          style.boxShadow !== "none";
        const rect = element.getBoundingClientRect();
        const at = document.elementFromPoint(
          rect.left + rect.width / 2,
          rect.top + rect.height / 2,
        );
        const occluded = !(at === element || (at && element.contains(at)));
        return { visible: decorates, occluded };
      });
      expect(focusCheck.visible, "focused control carries a visible focus style").toBe(true);
      expect(focusCheck.occluded, "focused control is not occluded").toBe(false);
      mark("focus-visibility-and-occlusion");
      log("focus-visibility-and-occlusion");
    } catch (error) {
      mark("focus-visibility-and-occlusion", error);
    }

    // [target-size] primary controls at least 24x24 CSS px (UI-TEST-003)
    try {
      await page.goto(siteURL + "/alerts");
      await expect(page.getByRole("heading", { name: "告警", level: 1 })).toBeVisible();
      await openModules(page);
      const sizes = await page.evaluate(() => {
        const small: string[] = [];
        for (const element of Array.from(document.querySelectorAll("button, a[href]"))) {
          const rect = element.getBoundingClientRect();
          if (rect.width === 0 || rect.height === 0) continue;
          if (rect.width < 24 || rect.height < 24) {
            small.push(
              (element.textContent ?? "").trim().slice(0, 20) +
                "@" + Math.round(rect.width) + "x" + Math.round(rect.height),
            );
          }
        }
        return small;
      });
      expect(sizes, "controls below the 24x24 target floor").toEqual([]);
      mark("target-size");
      log("target-size");
    } catch (error) {
      mark("target-size", error);
    }

    // [responsive-reflow] no page-level horizontal overflow at this width
    try {
      const overflow = await page.evaluate(
        () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
      );
      expect(overflow).toBeLessThanOrEqual(0);
      mark("responsive-reflow");
      log("responsive-reflow");
    } catch (error) {
      mark("responsive-reflow", error);
    }

    // [axe-rules] vendored axe-core, serious/critical must be zero. The
    // audit runs in a dedicated context that bypasses the production CSP
    // purely so the audit bundle can execute; every behavioral aspect
    // above ran in the CSP-enforced context.
    try {
      await runAxe(browser, subject, "knowledge-page", "/knowledge");
      await runAxe(browser, subject, "alerts-page", "/alerts");
      mark("axe-rules");
      log("axe-rules");
    } catch (error) {
      mark("axe-rules", error);
    }

    // [domain-behavior] seeded alert domain data, real waiting states
    try {
      await page.goto(siteURL + "/alerts");
      await expect(page.getByRole("heading", { name: "告警", level: 1 })).toBeVisible();
      const row = page.locator(".object-row", { hasText: /T03Probe|T41MatrixProbe/ }).first();
      if (!(await row.isVisible())) {
        await ensureMatrixAlert();
      }
      await expect(row).toBeVisible({ timeout: 30_000 });
      await row.click();
      await expect(page.getByRole("heading", { name: /T03Probe|T41MatrixProbe/ })).toBeVisible();
      await expect(page.getByRole("heading", { name: "标签" })).toBeVisible();
      // A real accepted background task must immediately show its waiting
      // feedback (UI-TEST-007); the deterministic provider finishes it.
      const analyze = page.getByRole("button", { name: "初步分析" });
      if (await analyze.count()) {
        await analyze.first().click();
        const waiting = page.getByText(/排队|运行中|已受理|分析中/);
        await expect(waiting.first()).toBeVisible({ timeout: 10_000 });
      }
      // Reduced motion must keep the equivalent static feedback: no
      // running animations anywhere on the observed page.
      if (subject.motion_mode === "reduced") {
        //Reduced-motion suppression is material, not nominal: the app
        //keeps animation names but pins durations near zero and stops
        //loops (styles/main.css), so the scan flags only material
        //motion: real durations or infinite iteration counts.
        const offenders = await page.evaluate(() => {
          const material: string[] = [];
          const parseMs = (value: string) => {
            const parsed = parseFloat(value);
            return value.endsWith("s") && !value.endsWith("ms")
              ? parsed * 1000
              : parsed;
          };
          for (const element of Array.from(document.querySelectorAll("*"))) {
            const style = getComputedStyle(element);
            if (
              style.animationName !== "none" &&
              (parseMs(style.animationDuration) > 10 ||
                style.animationIterationCount === "infinite")
            ) {
              material.push(element.tagName + "@animation");
            }
            if (
              style.transitionProperty !== "none" &&
              parseMs(style.transitionDuration) > 10
            ) {
              material.push(element.tagName + "@transition");
            }
          }
          return material;
        });
        expect(offenders, "reduced-motion cells must suppress material motion").toEqual([]);
        const suppression = await page.evaluate(() => {
          for (const sheet of Array.from(document.styleSheets)) {
            for (const rule of Array.from(sheet.cssRules)) {
              if (rule instanceof CSSMediaRule && rule.media.mediaText.includes("prefers-reduced-motion")) {
                return true;
              }
            }
          }
          return false;
        });
        expect(suppression, "the app ships a reduced-motion media contract").toBe(true);
        const mediaReduced = await page.evaluate(() =>
          window.matchMedia("(prefers-reduced-motion: reduce)").matches,
        );
        expect(mediaReduced).toBe(true);
      }
      mark("domain-behavior");
      log("domain-behavior");
    } catch (error) {
      mark("domain-behavior", error);
    }

    await page.screenshot({ path: join(artifactsDir, "final.png"), fullPage: false });
    await context.tracing.stop({ path: join(artifactsDir, "trace.zip") });
    record.artifacts = {
      screenshot: join(artifactsDir, "final.png"),
      trace: join(artifactsDir, "trace.zip"),
    };
    await context.close();
  } finally {
    record.finished_at = new Date().toISOString();
    await browser.close();
  }
  return { record };
}

async function runAxe(
  browser: import("playwright-core").Browser,
  subject: LocalSubject,
  label: string,
  route: string,
): Promise<void> {
  const context = await browser.newContext({
    viewport: { width: subject.viewport_css_px, height: 900 },
    reducedMotion: subject.motion_mode === "reduced" ? "reduce" : "no-preference",
    ignoreHTTPSErrors: true,
    bypassCSP: true,
  });
  try {
    const page = await context.newPage();
    await page.goto(siteURL + route);
    await page.waitForLoadState("networkidle");
    await page.addScriptTag({ content: axeSource });
    const violations = await page.evaluate(async () => {
      const axe = (window as unknown as AxeGlobal).axe;
      //The audit owns a bounded budget: a wedged scan fails the cell
      //instead of hanging the matrix.
      const bounded = Promise.race([
        axe.run({ runOnly: { type: "tag", values: ["wcag2a", "wcag2aa"] } }),
        new Promise<never>((_, reject) =>
          setTimeout(() => reject(new Error("axe scan exceeded its budget")), 45_000),
        ),
      ]);
      const result = await bounded;
      return result.violations
        .filter((violation) => violation.impact === "serious" || violation.impact === "critical")
        .map((violation) => violation.id + "(" + violation.impact + ")");
    });
    expect(violations, "axe serious/critical violations on " + label).toEqual([]);
  } finally {
    await context.close();
  }
}

interface AxeGlobal {
  axe: {
    run: (options: unknown) => Promise<{
      violations: Array<{ id: string; impact: string }>;
    }>;
  };
}
