# deploy/retired/browser — historical reference only

Retired 2026-09-14 (coordinator-approved browser removal). The browser plugin
and its Lintel browser runtime are no longer part of any active deployment
topology; these files are kept verbatim at their original relative layout for
historical reference only. Nothing applies, bundles, builds, or tests them.

- `compose.browser.yaml` — from `deploy/compose.browser.yaml` (Compose
  browser-enablement override).
- `config/lintel.yaml` — from `deploy/config/lintel.yaml` (Lintel component
  config).
- `config/quoin-browser.yaml` — from `deploy/config/quoin-browser.yaml`
  (quoin component config with `browser` in enabledPlugins).
- `kubernetes/lintel.yaml` — from `deploy/kubernetes/lintel.yaml` (opt-in
  Lintel overlay: lintel-config ConfigMap, lintel-state PVC, lintel
  Deployment, lintel-ops Service).

Still active and intentionally NOT retired:

- `deploy/images/lintel/Dockerfile` + `deploy/images/build.sh` keep the
  explicit Lintel image build capability.
- `cmd/lintel`, `internal/lintel` Lintel source stays in the tree.

Formal browser removal from an installed cluster follows the approved
offline recovery/retirement flow, never a bare `kubectl delete`.
