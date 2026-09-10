# #96 and #102 real local acceptance

This is the supported disposable integration environment for #96 Alertmanager intake and #102 platform-runtime lifecycle acceptance. It runs built Quoin, Stele, frontend, and the production Plinth adapter behind one local TLS gateway. It is not the MSW preview and does not run legacy browser suites.

## Requirements

- Linux with Docker Engine and Docker Compose v2, Go, Node/pnpm, Python 3, OpenSSL, curl, and jq.
- Access to cached or fetchable build dependencies on the first run.
- `8444` available by default. Set `QUOIN_E2E_PORT` when needed.

## Start and access

```bash
cd /home/suknna/code/quoin
make e2e-real-up
```

The default creates only `/home/suknna/code/quoin/.artifacts/e2e-102`, uses Compose project `quoin-e2e-102`, and serves `https://localhost:8444`. It deliberately does not reuse or stop an existing `.artifacts/e2e-real` hand-debug environment. The generated credential file is `/home/suknna/code/quoin/.artifacts/e2e-102/credentials.yaml` (mode `0600`); read it privately and never paste it into a ticket, terminal history, source file, or chat.

The startup path builds frontend, Quoin, Stele, and Plinth; creates deployment secrets via `quoin secrets bootstrap`; and creates the initial Admin using the attached supported `quoin admin create` CLI. It never seeds SQL or writes artificial health/registration rows.

Stop while retaining state for debugging:

```bash
make e2e-real-down
```

Remove only this harness’s generated state and credentials:

```bash
make e2e-real-down QUOIN_E2E_RUNTIME="$PWD/.artifacts/e2e-102"
QUOIN_E2E_RUNTIME="$PWD/.artifacts/e2e-102" QUOIN_E2E_PORT=8444 QUOIN_E2E_PROJECT=quoin-e2e-102 bash scripts/e2e-real/down.sh --purge
```

`--purge` only removes the chosen disposable directory. To use a different directory, port, and Compose project together:

```bash
make e2e-real QUOIN_E2E_RUNTIME="$PWD/.artifacts/my-e2e" QUOIN_E2E_PORT=9444 QUOIN_E2E_PROJECT=my-e2e
```

## #102 hand-debug: About, real registration, fault, recovery

1. Open `https://localhost:8444`, accept the short-lived self-signed certificate, and sign in as the generated Admin. Complete the forced password change.
2. Open **管理** → **关于** (`/admin/about`). The page is the only management surface for **运行时注册与轮换** and **维护**. It reports unknown facts as unknown; it does not invent health or versions.
3. In **运行时注册与轮换**, choose **准备首次注册** for `plinth`, confirm, and copy the once-only token. It is intentionally not written to any file.
4. In a separate terminal, run the supported production adapter registration helper and paste the token at its hidden prompt:

   ```bash
   QUOIN_E2E_RUNTIME="$PWD/.artifacts/e2e-102" QUOIN_E2E_PORT=8444 QUOIN_E2E_PROJECT=quoin-e2e-102 bash scripts/e2e-real/register-plinth.sh
   ```

   The helper supplies JSON only on attached stdin to `plinth register`; the production adapter calls Quoin’s TLS `Register` RPC and stores its returned long-term token atomically on Plinth’s state volume. It does not use argv, environment variables, Compose configuration, SQL, or a fake runtime adapter.
5. Refresh About until `plinth` is registered and connected. For a controlled actual disconnect, do not delete the state volume; stop only its container:

   ```bash
   QUOIN_E2E_RUNTIME="$PWD/.artifacts/e2e-102" QUOIN_E2E_PORT=8444 QUOIN_E2E_PROJECT=quoin-e2e-102 \
     docker compose --project-directory "$PWD" --env-file /dev/null -f deploy/e2e-real.compose.yaml stop plinth
   ```

6. Open the existing alert list. A `平台内部` Plinth-unavailable fault appears through the unified alert read model. It is neither an Alertmanager Delivery/Occurrence nor associated with a business system. Its detail has no AI analysis tab; direct analysis creation returns `422`, so no analysis dispatch occurs.
7. Start the same production adapter again:

   ```bash
   QUOIN_E2E_RUNTIME="$PWD/.artifacts/e2e-102" QUOIN_E2E_PORT=8444 QUOIN_E2E_PROJECT=quoin-e2e-102 \
     docker compose --project-directory "$PWD" --env-file /dev/null -f deploy/e2e-real.compose.yaml start plinth
   ```

   Its real control stream reconnects and resolves the same platform-fault lifecycle. Refresh About and Alerts; allow up to 20 seconds for the controlled state transition.
8. Create an Operator using the existing Admin users page, sign in in a separate browser profile, and confirm that platform fault alert reads work but `/admin/about`, `/api/v1/runtime`, and runtime registration commands return `403`. Operators have no runtime/maintenance URL entry.

## #96 intake path

1. Sign in with the generated Admin identity and complete the forced password change. When using the automated acceptance command afterwards, use the generated `admin.password` as the final password; the runner reads that value from the private credential file.
2. Open **运维中心** → **接入管理** (`/integrations`) → **Alertmanager**. Create a unique source key, and copy the receiver YAML and Bearer credential before dismissing the one-time dialog. The public receiver URL must match this deployment (by default `https://localhost:8444/stele/alerts`), not a container address. Other catalog platforms are unavailable in this slice; model providers remain in administrator settings.
3. Send the following controlled fixture through the public receiver. Run it from the repository root in Bash; the hidden prompt keeps the source credential out of command history and process arguments. For a custom runtime, change `runtime` and `receiver` to match the chosen deployment. Do not use the Admin password as the Bearer credential.

   ```bash
   runtime="$PWD/.artifacts/e2e-102"
   receiver='https://localhost:8444/stele/alerts'
   read -rsp 'One-time source Bearer credential: ' source_token; printf '\n'
   printf 'header = "Authorization: Bearer %s"\n' "$source_token" |
     curl --config - --cacert "$runtime/gateway/tls/tls.crt" \
       --silent --show-error --output /dev/null --write-out 'HTTP %{http_code}\n' \
       --header 'Content-Type: application/json' \
       --data-binary '{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"ManualAlertmanagerIntake","service":"checkout","severity":"critical"},"annotations":{"summary":"Manual public Stele acceptance"},"startsAt":"2026-09-10T00:00:00Z"}]}' \
       "$receiver"
   unset source_token
   ```

4. Expect `204` only after gateway → Stele → Quoin commits the Delivery. Open the current alert list and find `ManualAlertmanagerIntake`; the source detail should show its latest valid event. With no matching Label Contract, retain the existing unassigned/intake result rather than inventing business ownership. Before the first valid event, the source is waiting, not faulty.
5. Repeat the same event with the same labels and `startsAt`. This is another upstream delivery, not another active Occurrence. Send a resolved event with the same labels and `startsAt`, `status: "resolved"` at both levels, and an `endsAt` after `startsAt` to end that lifecycle.
6. Create an **Operator** through the Admin users page (not SQL), then sign in in a separate browser profile and finish its forced password change. Confirm the alert remains readable, management navigation is absent, and direct `/integrations`, business-management, inspection-management, and administrator URLs show access denied. In that authenticated session, `/api/v1/alert-sources`, `/api/v1/business-systems`, `/api/v1/label-contracts`, and `/api/v1/runtime` must return `403`; `/api/v1/business-context` remains readable without management secrets.

### Receiver setup and failure recovery

- For an actual Alertmanager, merge the copied receiver into its existing `receivers` and reference its name from the desired `route`. The fixture above tests the production ingress without requiring Alertmanager itself. To use this local self-signed gateway, mount `gateway/tls/tls.crt` into Alertmanager and configure `http_config.tls_config.ca_file`; keep TLS verification enabled. `localhost` must resolve to the gateway from the Alertmanager process, so a separately containerized Alertmanager needs an intentionally reachable deployment URL and matching certificate. See the [official Alertmanager configuration reference](https://prometheus.io/docs/alerting/latest/configuration/).
- Newly created or rotated credentials can briefly return `401` while Stele refreshes its credential snapshot. Wait up to 15 seconds and retry. A deliberately invalid credential must continue to return `401`; verify the receiver URL and exact Bearer value rather than recreating the source repeatedly.
- `503` means the relay did not confirm a committed result. Restore Quoin/relay availability and retry the same payload. Do not treat a timeout as proof that nothing committed; existing Delivery/Occurrence semantics handle relay retries and repeated upstream notifications.
- A `204` acknowledges durable processing, not that every item was valid. Malformed or rejected input may be recorded as an intake issue without advancing the latest-valid-event timestamp. Check the alert/intake outcome, not just the HTTP status.
- If the one-time reveal was dismissed or lost, it cannot be read again. Rotate through the source detail and save the new credential securely. After the new credential has received a valid event, retire the old generation through the existing lifecycle. Disable only a disposable test source when checking revocation; subsequent deliveries must fail and historical alerts remain historical facts.
- The source credential and copied YAML are secrets. Do not put them in tickets, screenshots, browser persistent storage, or committed configuration. The harness's Admin credentials and the source Bearer are separate credentials.

For the 0/1/50/long-source-key visual boundary data, after completing Admin password change:

```bash
QUOIN_E2E_RUNTIME="$PWD/.artifacts/e2e-102" bash scripts/e2e-real/prepare-boundary-data.sh
```

## Automated acceptance

```bash
make e2e-real
```

This starts the isolated harness as needed and runs only `web/e2e/real/issue-96.integration.spec.ts` and `web/e2e/real/issue-102.platform-fault.integration.spec.ts`. #102 creates the runtime registration through the actual About UI, relays the one-time token only through Plinth’s attached-stdin CLI, stops/starts the real container, observes unified platform-fault lifecycle, checks API/URL Operator denial, and confirms no analysis dispatch. Legacy real tests remain opt-in through `QUOIN_REAL_E2E_LEGACY=1`.

## Limits

- Lintel is intentionally absent from this focused topology; #102 covers one real production runtime adapter (Plinth) rather than faking both slots.
- The gateway certificate is local, self-signed, and short-lived.
- The controlled Alertmanager-compatible POST uses the production Stele webhook adapter but does not launch a full Alertmanager process.
- This #102 harness always starts a fresh database. The current schema gate intentionally accepts only exact fresh-v1, zero-history state; an older schema database is rejected on digest mismatch. No supported in-place historical migration is delivered here, and deleting user data is not a remedy.
- A stopped Quoin cannot report its own outage through its unavailable UI; #102 validates peer runtime lifecycle after Quoin is available.
