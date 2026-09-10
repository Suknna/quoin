# Issue #96 acceptance follow-up

Date: 2026-09-10. Baseline: `f54920a`.

The baseline already contains the role-aware integration workbench, Alertmanager-specific form, Logical Alert Source credential lifecycle, restricted business-context projection, and production Stele → Quoin intake chain. This follow-up reuses that implementation rather than rebuilding it.

## Changes

- Added explicit real-ingress checks for invalid Bearer rejection (`401`) and repeated upstream notification retaining one Occurrence. A unique fixture label isolates retained-environment history.
- Removed the shared acceptance suite's dependence on a previously finalized Admin password. The #102 test now activates a fresh CLI-created Admin through the supported login/password APIs, while preserving retained-environment login.
- Expanded [real local instructions](e2e-real-local.md#96-intake-path) with secure sample delivery, Admin/Operator preparation, TLS trust, retry expectations, credential rotation, and unsupported capabilities.

## Executed checks

- `go test ./... -count=1`: passed.
- `pnpm --dir web typecheck`: passed, including after changes.
- `pnpm --dir web test`: 25 files / 123 tests passed, including after changes.
- Production frontend and Quoin/Stele/Plinth images built through `make e2e-real`.
- Real #96/#102 browser acceptance: two consecutive final runs passed, 2/2 each. #96 creates a source through its real UI, delivers through the TLS gateway and Stele, reads persisted alerts, and verifies Operator management denials with allowed business-context reads. #102 exercises the production Plinth registration/fault/recovery chain.
- `git diff --check`: passed.

## Failures observed, not hidden

The first fresh combined run failed #102 login with `401` because it assumed the final password existed; #96 passed. The test setup was corrected. The subsequent fresh run completed Admin activation but timed out loading the About heading; that timeout did not recur in two retained-environment reruns. An intermediate new assertion incorrectly assumed a source identifier existed on the alert summary; it was replaced with a unique fixture label and passed in both final runs. No production UI fix was inferred from the transient timeout.

## Hand-debug environment

The final retained environment is `https://localhost:9447`, Compose project `quoin-issue96-b1e22752-fresh`, runtime `.artifacts/issue96-b1e22752-fresh`. Its private `credentials.yaml` contains the generated Admin username and final password. The original isolated run at port `9446` is also retained. Neither environment touches the pre-existing deployment. Credentials are ignored artifacts, not committed documentation.

## Boundaries

This follow-up changes tests and documentation, not production frontend rendering. The five visual-change checks (hierarchy, pointer discoverability, 0/1/50/long text, 1920px width, and heading consistency) are therefore not newly claimed; the real automated browser flows are functional evidence, not a replacement for a full visual audit. The interactive browser independently reached the real deployment's login page only.

The fixture is an Alertmanager-compatible HTTP notification through the production ingress, not a running Alertmanager server. Full upstream routing/configuration/retry behavior is not newly verified. Prometheus, Thanos, Kubernetes, browser-platform setup, and business-model reconstruction remain outside this slice. No GitHub issue was closed or remote branch pushed by this follow-up.
