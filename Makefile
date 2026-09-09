# Quoin v1 development entry points. The ticket acceptance script is the
# authoritative verification path; these targets are conveniences.

.PHONY: test vet web-typecheck web-lint web-test web-build images ticket-01 acceptance clean

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
	bash build/package/images.sh

ticket-01:
	bash test/integration/compose/acceptance.sh

acceptance: ticket-01

clean:
	rm -rf .artifacts/tickets web/playwright-report web/test-results
