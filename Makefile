COMPOSE = docker compose -f docker/compose.yaml
# Production: the development file plus compose.prod.yaml, settings from docker/.env (see docker/.env.example).
PROD = docker compose $(if $(wildcard docker/.env),--env-file docker/.env) -f docker/compose.yaml -f docker/compose.prod.yaml

.PHONY: bench dev db test upgrade-test wp2p-test docker-up docker-down prod-up prod-down prod-logs docker-test build publish-themes builder builder-watch builder-test builder-parity builder-fidelity builder-bump release lint e2e e2e-run smoke

## dev: run Postgres in Docker and the app locally, reading templates from ./web
dev: db
	PURSAE_WEB_DIR=web go run -tags devhub ./cmd/wpgo serve

# --wait: until the healthcheck passes, so the recipes that run psql or migrate right after do not hit a
# PostgreSQL that is still starting.
db:
	$(COMPOSE) up -d --wait db

## bench: Pursae vs WordPress benchmark (bench/run.py, about an hour; docs/developer/benchmarks.md).
## Builds both stacks in Docker on ports 18081/18082, results in bench/results/<date>/. BENCH_ARGS passes
## options, e.g. BENCH_ARGS="--setups wp-stock,pursae-cache --runs 1 --duration 5s" for a smoke test.
bench:
	python3 bench/run.py $(BENCH_ARGS)

## build: bin/pursae, plus bin/wpgo (the same binary under its older name, deprecated: removed no earlier than 1.0)
build:
	CGO_ENABLED=0 go build -buildmode=exe -tags nodynamic -o bin/pursae ./cmd/wpgo
	cp bin/pursae bin/wpgo

## test: unit tests plus the Postgres integration tests against the compose database. -p 1: the packages
## with integration tests (server, store, settings) each drop and recreate PURSAE_TEST_DB, so they must not
## run at the same time; the internal/server suite alone takes about ten minutes, past go test's default
## timeout. TEST_DB names the database (default wpgo_test): give parallel checkouts their own.
TEST_DB ?= wpgo_test
test: db
	PURSAE_TEST_ADMIN_DB=postgres://wpgo:wpgo@localhost:$${PURSAE_DB_PORT:-$${WPGO_DB_PORT:-5432}}/wpgo?sslmode=disable \
	PURSAE_TEST_DB=postgres://wpgo:wpgo@localhost:$${PURSAE_DB_PORT:-$${WPGO_DB_PORT:-5432}}/$(TEST_DB)?sslmode=disable \
	go test ./... -p 1 -count=1 -timeout 55m

## upgrade-test: sites created by older releases (v0.2.0 and the three newest tags) keep their data when
## this checkout takes them over (tools/upgrade-test.sh). Builds each tag, so it needs the release tags
## (git fetch --tags). UPGRADE_TAGS="v0.2.5 v0.2.8" picks the releases; databases are wpgo_upg_*.
upgrade-test: db
	tools/upgrade-test.sh

docker-up:
	$(COMPOSE) up -d --build

docker-down:
	$(COMPOSE) down

## prod-up: production build (updates from hub.pursae.com), settings from docker/.env, on 127.0.0.1 behind your TLS proxy
prod-up:
	@test -f docker/.env || { echo "Create docker/.env first: cp docker/.env.example docker/.env (set PURSAE_DB_PASSWORD and PURSAE_SITE_URL)"; exit 1; }
	$(PROD) up -d --build

prod-down:
	$(PROD) down

prod-logs:
	$(PROD) logs -f wpgo

## release: build every platform and publish GitHub release v$(VERSION) for Pursae Manager to import
## (tools/release.sh). BETA=1 marks a pre-release, DRY_RUN=1 only builds into dist/release.
release:
	@test -n "$(VERSION)" || { echo "usage: make release VERSION=0.2.0 [BETA=1] [DRY_RUN=1]"; exit 2; }
	tools/release.sh $(VERSION) $(if $(BETA),--beta) $(if $(DRY_RUN),--dry-run)

## wp2p-test: WP to Pursae end to end (integrations/wordpress/test/run.sh): WordPress + WooCommerce in Docker
## build a package with the plugin, this checkout's binary imports it into wpgo_wp2p on the compose
## Postgres, users sign in with their WordPress passwords, the store matches. Needs internet; KEEP=1 keeps it up.
wp2p-test: db
	integrations/wordpress/test/run.sh

## e2e: wp-admin browser smoke test (Playwright, builder/test/e2e): builds bin/pursae, recreates the database
## wpgo_e2e on the compose PostgreSQL, installs a site through the setup-token link, logs in, publishes a post in
## the block editor, uploads an image, opens Dashboard › Updates and logs out (under a minute). Needs Playwright's
## Chromium (cd builder && npx playwright install chromium). Output: builder/.work/e2e/.
e2e: build db builder/node_modules/.package-lock.json
	$(COMPOSE) exec -T db psql -q -U wpgo -d wpgo -c 'DROP DATABASE IF EXISTS wpgo_e2e WITH (FORCE)' -c 'CREATE DATABASE wpgo_e2e'
	$(MAKE) e2e-run E2E_DATABASE_URL='postgres://wpgo:wpgo@localhost:'$${PURSAE_DB_PORT:-$${WPGO_DB_PORT:-5432}}'/wpgo_e2e?sslmode=disable'

## e2e-run: the same against E2E_DATABASE_URL (an empty database) and E2E_BIN (default bin/pursae), as CI runs it.
e2e-run: builder/node_modules/.package-lock.json
	@test -n "$(E2E_DATABASE_URL)" || { echo "e2e-run: set E2E_DATABASE_URL to an empty database"; exit 2; }
	cd builder && E2E_DATABASE_URL='$(E2E_DATABASE_URL)' E2E_BIN='$(or $(E2E_BIN),$(CURDIR)/bin/pursae)' npx playwright test -c test/e2e/playwright.config.ts

## smoke: smoke test of bin/pursae (tools/smoke-test.sh: version, migrate, serve, healthcheck, the setup-token
## installer, an install, the front page) on the database wpgo_smoke of the compose PostgreSQL, recreated first.
smoke: build db
	$(COMPOSE) exec -T db psql -q -U wpgo -d wpgo -c 'DROP DATABASE IF EXISTS wpgo_smoke WITH (FORCE)' -c 'CREATE DATABASE wpgo_smoke'
	DATABASE_URL='postgres://wpgo:wpgo@localhost:'$${PURSAE_DB_PORT:-$${WPGO_DB_PORT:-5432}}'/wpgo_smoke?sslmode=disable' tools/smoke-test.sh bin/pursae

## lint: golangci-lint (.golangci.yml) on what changed since LINT_BASE (default origin/main), as CI does.
## The tree has older findings; LINT_BASE= lints everything. Uses golangci-lint from PATH if it is
## there, else go run of the version CI pins.
GOLANGCI_LINT_VERSION = v2.15.0
GOLANGCI_LINT ?= $(or $(shell command -v golangci-lint 2>/dev/null),go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION))
LINT_BASE ?= origin/main
lint:
	$(GOLANGCI_LINT) run $(if $(LINT_BASE),--new-from-merge-base=$(LINT_BASE)) ./...

## docker-test: run the whole test suite inside a Go container next to Postgres
docker-test:
	$(COMPOSE) --profile test run --rm test

## publish-themes: import library/themes into Pursae Manager (tools/publish-themes.sh). Needs
## PURSAE_MANAGER_DIR (a pursae-manager checkout with its environment) or PURSAE_MANAGER (its binary);
## optional PREINSTALL=slug,slug|all (installed on every new site) and ACCESS=public|pro|private.
publish-themes:
	tools/publish-themes.sh $(if $(PREINSTALL),-preinstall $(PREINSTALL)) $(if $(ACCESS),-access $(ACCESS))

## builder: build the Pursae Web Builder (builder/ → web/web-builder/, committed). Needs Node 22+ (builder/.nvmrc)
## and git; Go and Docker builds use the committed dist and need no Node.
builder: builder/node_modules/.package-lock.json
	git submodule update --init builder/upstream
	cd builder && node scripts/build.mjs

builder/node_modules/.package-lock.json: builder/package.json builder/package-lock.json
	cd builder && npm ci --ignore-scripts --no-audit --no-fund

## builder-watch: rebuild the editor on change (pair with `make dev`, which reads web/ live)
builder-watch: builder/node_modules/.package-lock.json
	git submodule update --init builder/upstream
	cd builder && node scripts/build.mjs --watch

## builder-test: host unit tests (vitest), UI string lint (no Turkish outside the i18n catalog), audit the built dist
## (no AI provider strings, manifest/BUILD.json shape, size budgets) and check BUILD.json against builder/UPSTREAM_SHA
## and the patch hashes. The host tests import the patched upstream copy (builder/.work/upstream), so it is
## prepared first from the pinned submodule and builder/patches (the dist in web/web-builder is not touched).
builder-test: builder/node_modules/.package-lock.json
	git submodule update --init builder/upstream
	cd builder && node scripts/build.mjs --prepare
	cd builder && npm test --silent
	node builder/scripts/lint-strings.mjs
	node builder/test/audit.mjs
	go test ./web -run TestWebBuilderDistIsCurrent -count=1

## builder-parity: Playwright parity suite (builder/test/parity): every renderStatic fixture drawn by the editor's live
## preview and as the static snapshot, pixel-compared at 1280/820/400 px (≤ 0.1 % differing pixels), the snapshot
## without JavaScript, and the site runtime's behaviours. Needs `make builder` (run here when its scratch copy is
## missing) and Playwright's Chromium; skips with a message when the browser is not installed
## (cd builder && npx playwright install chromium); set PARITY_STRICT=1 (CI) to make a skip fail. Report: builder/.work/parity/report.json.
builder-parity: builder/node_modules/.package-lock.json
	@test -d builder/.work/upstream/src/builder/live || $(MAKE) builder
	@cd builder && { node test/parity/check-browsers.mjs; rc=$$?; if [ $$rc -eq 3 ]; then if [ -n "$$PARITY_STRICT" ]; then echo '[parity] PARITY_STRICT set: skip counts as failure'; exit 1; fi; exit 0; elif [ $$rc -ne 0 ]; then exit $$rc; fi; \
		npx playwright test -c test/parity/playwright.config.ts; }

## builder-fidelity SRC=<folder> SITE=<url> [MAP=map.json] [OUT=dir] [SCENARIOS=file.json] [ARGS=…]: compare every
## page of a static HTML site (SRC) with the page the site import made of it on a running Pursae (SITE), full page at
## 1440/1024/768/390 px (builder/test/fidelity/fidelity.mjs); report.json and report.html in OUT. Needs Playwright's
## Chromium (cd builder && npx playwright install chromium).
OUT ?= builder/.work/fidelity
builder-fidelity: builder/node_modules/.package-lock.json
	@test -n "$(SRC)" -a -n "$(SITE)" || { echo "usage: make builder-fidelity SRC=<folder> SITE=<url> [MAP=map.json] [OUT=dir] [SCENARIOS=file.json]"; exit 2; }
	node builder/test/fidelity/fidelity.mjs --src "$(SRC)" --site "$(SITE)" --out "$(OUT)" $(if $(MAP),--map "$(MAP)") $(if $(SCENARIOS),--scenarios "$(SCENARIOS)") $(ARGS)

## builder-bump SHA=<commit>: move builder/upstream and builder/UPSTREAM_SHA to an upstream commit, then
## rebuild (patches must still apply). Stages the pin; commit it with web/web-builder.
builder-bump:
	@test -n "$(SHA)" || { echo "usage: make builder-bump SHA=<upstream commit>"; exit 1; }
	git submodule update --init builder/upstream
	git -C builder/upstream fetch --quiet origin
	git -C builder/upstream -c advice.detachedHead=false checkout --quiet --detach "$(SHA)"
	git -C builder/upstream rev-parse HEAD > builder/UPSTREAM_SHA
	git add builder/upstream builder/UPSTREAM_SHA
	$(MAKE) builder
