# Issue #97 hand-debug and validation guide

This guide describes the supported disposable local environment for the #97 metrics-integration and business-declaration slice. It is for manual diagnosis and controlled validation; it is neither a claim that every command below has passed nor a substitute for the issue acceptance suite.

## Supported commands and URL

From the repository root:

```bash
cd /home/suknna/code/quoin
make e2e-real-up \
  QUOIN_E2E_RUNTIME="$PWD/.artifacts/e2e-97-delivery" \
  QUOIN_E2E_PORT=8459 \
  QUOIN_E2E_PROJECT=quoin-e2e-97-delivery
```

The isolated deployment URL is `https://localhost:8459`. Its generated private credentials are in:

```text
/home/suknna/code/quoin/.artifacts/e2e-97-delivery/credentials.yaml
```

The runtime directory and Compose project above are intentionally separate from the #96/#102 environment. Do not point this harness at a pre-existing data directory or use it to alter a non-disposable deployment.

Stop while retaining generated state:

```bash
make e2e-real-down \
  QUOIN_E2E_RUNTIME="$PWD/.artifacts/e2e-97-delivery" \
  QUOIN_E2E_PORT=8459 \
  QUOIN_E2E_PROJECT=quoin-e2e-97-delivery
```

Run the complete real acceptance against this isolated environment:

```bash
make e2e-real \
  QUOIN_E2E_RUNTIME="$PWD/.artifacts/e2e-97-delivery" \
  QUOIN_E2E_PORT=8459 \
  QUOIN_E2E_PROJECT=quoin-e2e-97-delivery
```

The final combined real suite passed `3/3` in 14.4 s: #102 in 7.7 s, the #96 intake path in 3.5 s, and #97 in 2.7 s. Its local log is `/tmp/quoin-97-combined-final.log`; the earlier focused #97 delivery run (`1/1`, 5.9 s) remains at `/tmp/quoin-97-delivery-test.log`. #97 exercised all three real authentication probes, a Thanos query and identity result reaching `Passed`, publication with no standalone scan, switching to Prometheus and verifying its actual source, plus Operator `403` enforcement. The flow uses the production adapter, public API, normal credential boundary, and Run/Attempt lifecycle; it does not infer success from mocks or direct database seeding. This is functional E2E evidence, not a complete browser visual audit. Do not substitute legacy real-browser suites or database writes.

## Legitimate test identities

The harness creates its initial Admin through the supported `quoin admin create` path, not by SQL seeding. Read the generated Admin credentials privately, sign in at the URL above, and complete any forced password change before configuring integrations.

Create an Operator through the Admin user-management UI or supported user API. Use a separate browser profile for the Operator so that its session cannot inherit Admin state. Do not put Admin passwords, Basic passwords, Bearer tokens, kubeconfigs, generated credentials, or copied configuration containing them in Git, tickets, screenshots, shell history, or browser persistent storage.

## Prerequisites for a real metric query

Prepare two controlled, independently reachable metric endpoints before creating integrations. They may be disposable local Prometheus-compatible fixtures, but requests must pass through the production connection adapter and normal application APIs.

For each endpoint, record its public URL, TLS trust material when applicable, and one of the supported authentication modes:

| Mode | Required input | Boundary |
| --- | --- | --- |
| `none` | No credential | The endpoint must explicitly allow unauthenticated requests. |
| `basic` | Username and password | The password is secret and must be stored only through the connection credential flow. |
| `bearer` | Bearer token | The token is secret and must be stored only through the connection credential flow. |

Prometheus and Thanos are separate integration types. Existing type-specific TLS configuration remains applicable; disabling certificate verification is not an authentication mode or a supported shortcut. A failed TLS, endpoint, protocol, or credential check must remain a visible validation failure rather than being silently accepted.

## Controlled validation sequence

1. Sign in as Admin and open **运维中心 → 接入管理**. Create one Prometheus integration and one Thanos integration, or two independently configured integrations of the same type when testing isolation.
2. For each integration, choose its explicit authentication mode and submit the corresponding secret only in the credential fields. Run its controlled connectivity validation. A failure must keep the created integration available for correction or retry, without revealing the submitted secret or creating a duplicate integration.
3. Open **业务纳管** and start a non-secret business-declaration draft. Select exactly one metric integration. The canonical declaration uses top-level `metrics_connection_id` as a decimal numeric-string.
4. Optionally set `alert_source_ids` as decimal numeric-strings and `alert_source_labels` as exact string key/value pairs. These only restrict the selected sources; the Label Contract remains the authority for the business-system label and ingestion ownership.
5. Add the query and resource-identity rules needed for the controlled query. Do not add credentials, endpoint URLs, TLS secrets, or free-form connection configuration to the declaration.
6. Switch between the form and YAML views, then export/import the declaration. They must represent the same versioned declaration: unknown fields must fail explicitly and a round trip must not silently discard supported fields.
7. Request the explicit validation/try-run for the draft. On success, its GET state is `Passed` (not `Succeeded`). Confirm the response's existing `checkResults` plus `metricsConnectionId`, `identitySamples[]` (`discoveryKey`, `labels`), and `queryResults[]` (`planKey`, `checkKey`, `resultType`, `sampleCount`) identify the selected integration and the query outcome. These are controlled Run evidence, not resources, and remain separate from the published Observed Resource projection.
8. Publish only after validation produces the expected result. Change the declaration to reference the other integration, revalidate, and confirm `metricsConnectionId` and the returned query/identity evidence now identify that explicitly selected integration rather than a global/default connection.
9. As Operator, verify that alert and AI SRE business-context use remains available only within existing authorization, while direct integration, business-declaration, inspection-management, and credential-management URLs/APIs are denied. Hidden navigation alone is not evidence of this boundary.

## Expected outcomes and failure recovery

- A business declaration without `metrics_connection_id`, with a non-numeric ID representation, or with a missing, disabled, unauthorized, or ambiguous integration reference must be rejected. No execution may fall back to a globally enabled or first-listed metric connection.
- Prometheus and Thanos retain distinct types. Only `none`, `basic`, and `bearer` are supported authentication modes. Basic requires username/password; Bearer requires a Bearer token.
- Retrying a failed create, validation, or publish must use the normal idempotency and optimistic-concurrency paths. Reuse the original client command identity only for the same request; after a result-unknown network failure, first reread the relevant object or retry through the supported client behavior. Do not repeatedly create replacement integrations merely because a probe failed. Earlier Prometheus switching validation returned `500`; the completed final E2E reran the flow successfully, so retain request diagnostics when a retry does not reproduce the issue rather than treating a prior failure as success.
- Fix endpoint, TLS trust, protocol, username/password, or Bearer-token errors in the existing integration and rerun controlled validation. Validation failure must not expose a secret, overwrite a previously usable credential revision, or fabricate a successful result.
- An explicit draft validation/try-run is a controlled execution. It must not overwrite the formal Observed Resource projection and must not be represented as a completed scheduled inspection.
- Saving an integration, opening an integration or business page, publishing a declaration, and waiting through former refresh intervals must not launch a standalone resource-refresh scan. Scheduled inspections, alert analysis, and authorized AI SRE tool calls may collect only within the published declaration’s scope.

## Migration, retirement, and history boundaries

The former global-unique metric connection model is retired. Before opening the upgraded application, run the supported exclusive migration command against the deployment configuration:

```bash
quoin migrate --config PATH
```

The migration performs one atomic canonical rebuild. It migrates a legacy metric connection to a business declaration only when the mapping is deterministic. If more than one target or no unambiguous target exists, it reports the ambiguity and blocks enablement; it must not guess a binding or widen access. The migration retains historical configuration versions, Runs, Attempts, Observed Resources, FTS indexes, and monotonic sequence facts. Its migration coverage is tested from baseline `5547ea0`.

The standalone resource-refresh scheduler, entry point, and configuration are fully retired. Retirement does not erase historical configuration versions, Runs, Attempts, or Observed Resources. Existing active execution follows its normal cancellation and terminal-state rules; no new orphaned refresh work may be scheduled.

## Executed verification evidence and boundaries

- `go test ./... -count=1` passed across 64 packages.
- `go vet ./...` passed.
- Web tests passed: 25 files / 133 tests. Web lint and the production web build also passed.
- The final combined real suite passed 3/3 as recorded above. Its #97 result is functional evidence through the real stack, not mock inference.
- Interactive browser observations, screenshots, discovered layout defects and their repairs are recorded in [frontend acceptance evidence](frontend/issue-97-acceptance.md). Coverage includes business 0/1/50/long boundaries, the 50-integration table with long names, 390px and 1920px viewports, YAML/form behavior, dirty-state locking, and actual draft navigation/restoration. The document states remaining coverage limits; mock visual evidence is not production integration evidence.
- The #96/#102 `make e2e-real` Playwright flow covers Alertmanager intake and platform faults; the final combined suite additionally includes #97 metrics declarations.
- This slice does not make Kubernetes or browser integration mandatory for a business declaration. Their absent or unfinished contracts must not block the metrics-only path.
- Do not validate success by directly inserting rows, seeding an already-successful Run, mocking the final adapter response, or manually writing credentials to application storage. A controlled fixture is valid only when it uses the production adapter, public API, normal credential boundary, and Run/Attempt lifecycle.
