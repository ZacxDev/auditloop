# auditloop — build / run / test targets.
#
# NixOS notes: `npx` is broken here — call ./node_modules/.bin directly. If a
# tool is missing, wrap the command in `nix-shell -p <pkg> --run "..."`.

TAILWIND := ./node_modules/.bin/tailwindcss
CHROMIUM ?= $(shell command -v chromium 2>/dev/null || command -v chromium-browser 2>/dev/null || command -v chrome 2>/dev/null)

.PHONY: deps css build run dev test test-e2e test-docker fmt vet clean ux-audit ux-audit-push-test

PLAYWRIGHT := ./node_modules/.bin/playwright

deps: ## Install the Tailwind CLI + vendored axe-core (once).
	npm install

css: ## Build static/output.css from static/input.css (Tailwind v4).
	$(TAILWIND) -i static/input.css -o static/output.css --minify

build: css ## Build the single binary + the reference push CLI.
	go build -o bin/auditloop .
	go build -o bin/auditloop-push ./cmd/auditloop-push

run: build ## Run the built binary (reads env for config).
	./bin/auditloop

dev: css ## Run locally with auth bypassed (DEV_MODE), web+worker, on :8112.
	DEV_MODE=true AUDITLOOP_ROLE=all CRAWL_ALLOW_LOOPBACK=true \
	AUDITLOOP_CHROMIUM=$(CHROMIUM) go run .

test: ## Unit + integration tests (hermetic; e2e needs chromium — see test-e2e).
	AUDITLOOP_CHROMIUM=$(CHROMIUM) go test ./...

test-e2e: ## Just the hermetic browser e2e (fixture site + real chromium).
	AUDITLOOP_CHROMIUM=$(CHROMIUM) go test ./tests/e2e/ -v -run TestEndToEndCrawl -timeout 180s

test-e2e-plugin: ## P5 plugin-push e2e (builds + runs the auditloop-push CLI; no chromium).
	go test ./tests/e2e/ -v -run TestEndToEndPluginPush -timeout 120s

test-docker: ## Bring up Postgres+MinIO and run the S3/Postgres integration tests.
	docker-compose -f docker-compose.test.yml up -d
	@echo "waiting for services…"; sleep 5
	S3_ENDPOINT=127.0.0.1:9100 S3_ACCESS_KEY=minioadmin S3_SECRET_KEY=minioadmin S3_BUCKET=audit-artifacts \
	DATABASE_DRIVER=postgres DATABASE_URL="postgres://auditloop:auditloop@127.0.0.1:5433/auditloop?sslmode=disable" \
	go test ./internal/storage/ ./internal/db/ -run 'S3|Postgres|CRUD|Run' -v || (docker-compose -f docker-compose.test.yml down; exit 1)
	docker-compose -f docker-compose.test.yml down

ux-audit: ## Self-audit: boot local DEV_MODE auditloop + walk its OWN UI (auditloop audits itself).
	PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=true \
	$(PLAYWRIGHT) test -c tests/e2e/playwright.ux-audit.config.ts

ux-audit-push-test: ## Unit-test the opt-in auditloop-push shim (node:test, no browser).
	node --test --experimental-strip-types tests/e2e/ux-audit/_lib/push.test.mjs

fmt: ## gofmt the tree.
	gofmt -w .

vet: ## go vet.
	go vet ./...

clean:
	rm -rf bin *.db artifacts

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'
