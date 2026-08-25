# Run release-validate's per-distribution snapshot matrix on release pull
# requests before a tag is minted.
.PHONY: build run clean web web-stub fmt lint check dev docs docs-open e2e e2e-schema-parity demo nix-vendor-hash guided-screenshots guided-screenshots-test origin-audit origin-audit-test

VERSION ?= dev

all: build

# web/package.json is the SOURCE OF TRUTH for web deps; all @peasant-labs/* deps resolve from
# the npm registry as published semver. web/ is pnpm-only: pnpm-lock.yaml is the single
# lockfile and pnpm is REQUIRED (fail loud — an npm install would resolve a second, divergent
# dependency tree that is not covered by the build gates).
web:
	@cd web && if ! command -v pnpm >/dev/null 2>&1; then \
		echo 'ERROR [make web] pnpm is not on PATH. web/ is pnpm-only (web/pnpm-lock.yaml is the single lockfile;'; \
		echo '  no npm build path is supported). Fix: run "corepack enable" (corepack ships'; \
		echo '  with Node) to activate the pnpm version pinned by web/package.json packageManager, or "npm i -g pnpm".'; \
		exit 1; \
	fi
	@cd web && pnpm install --frozen-lockfile && pnpm build
	# `next build` (output: export) CLEANS web/out/ and deletes the tracked
	# web/out/.gitkeep placeholder. Re-create it so the worktree stays clean
	# (goreleaser refuses a dirty tree) and the //go:embed all:web/out
	# tracked-file invariant holds (go install / nix). The real index.html +
	# _next/** the build wrote are gitignored.
	@touch web/out/.gitkeep
	@# EMBED GUARD: the //go:embed all:web/out binary MUST contain the lifted /review Changes
	@# surface. A stale/stub export (failed web build or placeholder out/) would otherwise silently ship a binary
	@# WITHOUT the surface — the user's "changes/change-detail unavailable" failure. Fail loud.
	@if ! grep -rqE 'gmp-changes-root' web/out 2>/dev/null; then \
		echo 'ERROR [make web] web/out does NOT contain the lifted /review Changes surface (marker "gmp-changes-root" absent) — refusing to embed a stale/stub export into bin/peasant.'; \
		echo '  Cause: a failed pnpm build, an unavailable published package, or a stale/stub web/out.'; \
		echo '  Fix: ensure pnpm can install the locked dependency graph, then rebuild.'; \
		exit 1; \
	fi

# web-stub writes a minimal placeholder into web/out so the
# `//go:embed all:web/out` directive compiles when the real Next.js dashboard
# has not been built. Compile-only checks use this target; artifact-producing
# workflows run `make web` and explicitly reject the stub.
#
# It is idempotent and NON-destructive: it writes web/out/index.html only when
# absent (a real `make web` output or a prior stub is left untouched). It also
# (re)creates the TRACKED placeholder web/out/.gitkeep so the //go:embed
# all:web/out invariant holds and the working tree stays clean even if a prior
# `next build` deleted it.
web-stub:
	@mkdir -p web/out
	@touch web/out/.gitkeep
	@[ -s web/out/index.html ] || printf '<!doctype html><meta charset="utf-8"><title>peasant</title><p>The peasant web dashboard is not bundled in this build. Run a full `make build` (or use the release binaries) for the dashboard.</p>\n' > web/out/index.html

fmt:
	@formatted=$$(go fmt ./...); \
	if [ -n "$$formatted" ]; then \
		echo "Files formatted:"; \
		echo "$$formatted"; \
	fi

lint: web-stub
	# Running golangci-lint is deferred until its existing findings are resolved.
	# Use go vet for now
	# golangci-lint run ./...
	go vet ./...

check: fmt lint
	ast-grep scan --config sgconfig.yml .
	# The key grep gate (internal/tui/gates/astrules/, enforced by
	# keys_astgrep_test.go) shells out to ast-grep too, but is gated behind
	# the "astgrep" build tag so a plain `go test ./...` never depends on the
	# binary - ast-grep is ALREADY a hard `make check` dependency via the
	# untagged scan above, so this adds no new external requirement here.
	go test -tags=astgrep -race ./internal/tui/gates/...
	go run github.com/peasant-labs/schema/cmd/release-guard check-workflow --policy .github/release-guard.policy.yml --release .github/workflows/release.yml
	go test -race ./...

# Local end-to-end skip-gate harness. Requires podman + a village
# checkout (VILLAGE_REPO, default sibling) or VILLAGE_BIN+SETUP_DEMO_BIN.
# Deliberately OUT of `check` — it needs containers + a cross-repo build and
# t.Skips when those are absent. See docs/e2e.md.
e2e:
	go test -race -tags=e2e -count=1 ./internal/e2e/...

# Cross-repo E2E must exercise matching schema-module contracts. CI passes the
# exact village checkout selected by VILLAGE_REF; this gate prevents a stale
# peer pin from silently testing a different contract version.
e2e-schema-parity:
	@if [ -z "$(VILLAGE_REPO)" ]; then \
		echo 'e2e schema parity failed'; \
		echo 'what: VILLAGE_REPO is empty'; \
		echo 'why: the gate needs the exact village checkout selected for the E2E run'; \
		echo 'where: Makefile target e2e-schema-parity'; \
		echo 'when: comparing peasant and village schema-module versions before E2E'; \
		echo 'means: contract parity cannot be proven, so the E2E result would be ambiguous'; \
		echo 'fix: set VILLAGE_REPO to a village checkout containing backend/go.mod'; \
		exit 1; \
	fi
	@peasant_schema=$$(go list -m -f='{{.Version}}' github.com/peasant-labs/schema) || { \
		echo 'e2e schema parity failed'; \
		echo 'what: the peasant schema-module version could not be read'; \
		echo 'why: go list could not resolve github.com/peasant-labs/schema from the root go.mod'; \
		echo 'where: Makefile target e2e-schema-parity'; \
		echo 'when: reading the peasant side of the cross-repo contract'; \
		echo 'means: schema-module parity cannot be evaluated'; \
		echo 'fix: restore a valid github.com/peasant-labs/schema requirement in go.mod'; \
		exit 1; \
	}; \
	village_schema=$$(cd "$(VILLAGE_REPO)/backend" && go list -m -f='{{.Version}}' github.com/peasant-labs/schema) || { \
		echo 'e2e schema parity failed'; \
		echo 'what: the village schema-module version could not be read'; \
		echo 'why: go list could not resolve github.com/peasant-labs/schema from VILLAGE_REPO/backend/go.mod'; \
		echo 'where: Makefile target e2e-schema-parity'; \
		echo 'when: reading the village side of the cross-repo contract'; \
		echo 'means: schema-module parity cannot be evaluated'; \
		echo 'fix: point VILLAGE_REPO at the pinned village checkout and confirm backend/go.mod is valid'; \
		exit 1; \
	}; \
	if [ "$$peasant_schema" != "$$village_schema" ]; then \
		echo 'e2e schema parity failed'; \
		echo "what: peasant uses $$peasant_schema while village uses $$village_schema"; \
		echo 'why: VILLAGE_REPO points to a checkout pinned to a different contract module'; \
		echo 'where: Makefile target e2e-schema-parity'; \
		echo 'when: before building the cross-repo E2E stack'; \
		echo 'means: a green run would not prove both consumers against the same wire contract'; \
		echo 'fix: select or re-pin a Village checkout whose backend/go.mod uses the Peasant schema version; in CI, update both workflow VILLAGE_REF pins together'; \
		exit 1; \
	fi; \
	echo "schema module parity: $$peasant_schema"

# Unasserted live demo of the same harness (run1 sends N, run2 sends 0,
# retraction drops one) with verbose output. See docs/e2e.md.
demo:
	go test -race -tags=e2e -count=1 -v -run TestSkipGateDemo ./internal/e2e/...

# Manual visual evidence for the mounted guided TUI. The build tag keeps the
# harness and its tests out of default Go builds and tests.
guided-screenshots:
	go run -tags=guided_screenshots ./cmd/peasant-guided-screenshots

guided-screenshots-test:
	go test -race -tags=guided_screenshots ./cmd/peasant-guided-screenshots

# Opt-in, read-only measurement harness that checks the production
# session-origin classifier's signal distribution against a real transcript
# history. Runs the production sessionorigin.Classify rule over the
# operator's OWN ~/.claude/projects and reports counts per deciding signal.
# It writes nothing anywhere. The build tag keeps it, and its tests, out of
# `go build ./...`, `make check`, and the shipped peasant binary -- same
# footing as guided-screenshots above.
origin-audit:
	go run -tags=origin_audit ./cmd/peasant-origin-audit

origin-audit-test:
	go test -race -tags=origin_audit ./cmd/peasant-origin-audit

build: web
	go build -ldflags "-X github.com/peasant-labs/peasant/internal/defaults.version=$(VERSION)" -o bin/peasant ./cmd/peasant
	@cd web && pnpm test:provider-build-provenance

nix-vendor-hash:
	./scripts/update-nix-vendor-hash.sh

go:
	go build -ldflags "-X github.com/peasant-labs/peasant/internal/defaults.version=$(VERSION)" -o bin/peasant ./cmd/peasant

run: web
	go run ./cmd/peasant web

dev:
	@if ! command -v pnpm >/dev/null 2>&1; then \
		echo 'ERROR [make dev] pnpm is required. Enable the version pinned by web/package.json, then retry.'; \
		exit 1; \
	fi
	@trap 'kill 0' EXIT; \
		go run ./cmd/peasant web start --dev & \
		(cd web && pnpm dev) & \
		wait

restart: build
	-./bin/peasant web stop
	./bin/peasant web start

docs: docs-cli
	@echo "Docs generated (CLI reference; API specs now live in the schema module)"

docs-cli: go
	./bin/peasant docgen docs/cli
	@echo "CLI docs generated in docs/cli/"

docs-open: docs
	xdg-open docs/api/index.html

clean:
	rm -rf bin/ web/out/ web/.next/
