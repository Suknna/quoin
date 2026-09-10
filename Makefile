# Quoin v1 development entry points. The ticket acceptance script is the
# authoritative verification path; these targets are conveniences.

.PHONY: test vet web-typecheck web-lint web-test web-build images ticket-01 acceptance e2e-real-up e2e-real e2e-real-down clean

test:
	go test ./... -count=1

vet:
	go vet ./...

web-typecheck:
	pnpm --dir web typecheck

web-lint:
	pnpm --dir web lint

web-test:
	pnpm --dir web test

web-build:
	pnpm --dir web install --frozen-lockfile
	pnpm --dir web build

images:
	bash deploy/images/build.sh

# #96/#102 real acceptance starts a disposable topology. Defaults deliberately
# use a separate #102 directory and project, preserving a user's #96 hand-debug
# deployment. Set QUOIN_E2E_RUNTIME (and a distinct port when concurrent) to
# select an explicit disposable directory.
QUOIN_E2E_RUNTIME ?= $(CURDIR)/.artifacts/e2e-102
QUOIN_E2E_PORT ?= 8444
QUOIN_E2E_PROJECT ?= quoin-e2e-102

e2e-real-up:
	QUOIN_E2E_RUNTIME="$(QUOIN_E2E_RUNTIME)" QUOIN_E2E_PORT="$(QUOIN_E2E_PORT)" QUOIN_E2E_PROJECT="$(QUOIN_E2E_PROJECT)" bash scripts/e2e-real/up.sh

e2e-real: e2e-real-up
	QUOIN_E2E_RUNTIME="$(QUOIN_E2E_RUNTIME)" QUOIN_E2E_PORT="$(QUOIN_E2E_PORT)" QUOIN_E2E_PROJECT="$(QUOIN_E2E_PROJECT)" QUOIN_E2E_CREDENTIALS_FILE="$(QUOIN_E2E_RUNTIME)/credentials.yaml" node scripts/e2e-real/run-playwright.mjs

e2e-real-down:
	QUOIN_E2E_RUNTIME="$(QUOIN_E2E_RUNTIME)" QUOIN_E2E_PORT="$(QUOIN_E2E_PORT)" QUOIN_E2E_PROJECT="$(QUOIN_E2E_PROJECT)" bash scripts/e2e-real/down.sh

ticket-01: e2e-real

acceptance: e2e-real

clean:
	rm -rf .artifacts/tickets web/playwright-report web/test-results
