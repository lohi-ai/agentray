# AgentRay developer tasks.
#
# Agent tests come in two layers (see agentcore/integration/live_provider_test.go):
#   * deterministic faux-provider unit tests — always run, no credentials.
#   * env-gated real-provider tests (TestReal_*) — exercise a live model; they
#     SKIP (not fail) when AGENTRAY_TEST_OPENAI_* is unset, so `make test` stays
#     green without creds. `make test-agents` loads .env and runs them for real.
#
# The PATH `go` on this machine is too old; pin the modern toolchain but let an
# operator override:  make test GO=go
GO ?= /usr/local/go/bin/go

# Hot-reload runner for local dev (`go install github.com/air-verse/air@latest`).
# Reads .air.toml and rebuilds ./cmd/server on every .go change. The Go server
# does NOT load .env itself, so `make dev` sources it first (see LOAD_ENV).
AIR ?= air
WEB_PM ?= pnpm

# --- Sandbox images -------------------------------------------------------
# The computer_use / browser_use tools run in throwaway Docker session
# containers (see internal/sandbox/docker.go). The API talks to a Docker daemon
# via the `docker` CLI, so these images must be built ON THE HOST whose daemon
# the API uses — locally, or on the GCE VM during/after deploy. The image names
# below are exactly what the runtime falls back to, so building with them needs
# no extra config beyond AGENTRAY_SANDBOX_ENABLED=true.
DOCKER       ?= docker
CU_IMAGE     ?= agentray-computeruse:latest
BROWSER_IMAGE ?= agentray-browser:latest
SHELL_IMAGE  ?= agentray-sandbox:latest
# Build for the host arch by default. Cross-build for the amd64 VM from an arm64
# Mac with:  make sandbox-build PLATFORM=linux/amd64
PLATFORM     ?=
PLATFORM_ARG := $(if $(PLATFORM),--platform $(PLATFORM),)
# Bake in the cloakbrowser stealth Chromium:  make sandbox-build-browser ENABLE_CLOAK=1
ENABLE_CLOAK ?= 0

# Source .env (gitignored) into the recipe shell, matching the documented
# convention `set -a && . ./.env && set +a`. The leading `-` keeps it optional.
LOAD_ENV = set -a; [ -f .env ] && . ./.env; set +a;

.DEFAULT_GOAL := help

.PHONY: help dev web build cli install-cli vet test test-agents test-agentcore-race test-stress check agent-funcs \
        sdk-check sdk-check-npm sdk-check-python sdk-check-swift sdk-release sdk-resolve-tag \
        sandbox-build sandbox-build-cu sandbox-build-browser sandbox-build-shell \
        sandbox-check sandbox-setup test-sandbox

help: ## List available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | sort | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

dev: ## Run the Go API with hot reload (air), .env sourced into the process
	@command -v $(AIR) >/dev/null 2>&1 || { echo "air not found — go install github.com/air-verse/air@latest"; exit 1; }
	@$(LOAD_ENV) $(AIR)

web: ## Run the agentray Next.js web app (pnpm dev, port 3200)
	cd web && $(WEB_PM) dev

build: ## Build the server binary
	$(GO) build -o server ./cmd/server

cli: ## Build the agentray CLI binary (./agentray)
	$(GO) build -o agentray ./cmd/cli

install-cli: ## Install the agentray CLI into GOPATH/bin (go install)
	$(GO) install ./cmd/cli

vet: ## Run go vet across all packages
	$(GO) vet ./...

test: ## Run the deterministic unit suite (no credentials; real-provider tests skip)
	$(GO) test ./... -count=1

test-agents: ## Run the env-gated real-provider agent tests across all packages (loads .env)
	@$(LOAD_ENV) \
	if [ -z "$$AGENTRAY_TEST_OPENAI_API_KEY" ]; then \
	  echo "AGENTRAY_TEST_OPENAI_* not set — copy .env.example to .env and fill it."; exit 1; \
	fi; \
	$(GO) test ./... -run 'TestReal_|RealProvider' -v -count=1

test-agentcore-race: ## Run agentcore's durability/concurrency suite with the race detector
	$(GO) test -race ./agentcore/... -count=1

test-stress: ## Run the long-run stability / compaction stress test
	$(GO) test ./agentcore/... -run TestLongRunStaysStableAcrossManyCompactions -v -count=1

check: vet test ## Vet + unit tests — the pre-commit gate

# --- Published SDKs (sdk/) ------------------------------------------------
# Deliberately not part of `check`: these need npm/python/swift toolchains that
# a Go-only contributor should not have to install to commit. CI runs them on
# every change under sdk/ (.github/workflows/sdk.yml), and the release workflow
# runs them again on the tagged tree before it publishes anything.
#
# The artefact assertions live in sdk/scripts/ so that all three callers — this
# Makefile, the PR gate, and the release gate — check the same things. Building
# is not the same as shipping something installable: see docs/RELEASING-SDK.md
# for the wheel that was published empty for months and passed every check that
# did not look inside it.

sdk-check: sdk-check-npm sdk-check-python sdk-check-swift ## Typecheck, test and build the published SDKs

sdk-check-npm: ## @agentray/browser + @agentray/server — build, then verify the publishable tarball
	@for pkg in browser server; do \
	  echo "── sdk/$$pkg"; \
	  (cd sdk/$$pkg \
	    && npm ci --silent \
	    && npm run typecheck \
	    && npm test \
	    && npm run build \
	    && node ../scripts/verify-npm-tarball.mjs \
	    && node ../scripts/verify-consumer-install.mjs "$$(npm pack --silent)" \
	    && rm -f ./*.tgz) || exit 1; \
	done
	@cd sdk/browser && node ../scripts/verify-cdn-bundle.mjs

sdk-check-python: ## agentray (PyPI) — tests plus a wheel that actually contains the package
	@python3 -c 'import urllib3' 2>/dev/null \
	  || { echo "sdk-check-python needs the SDK's own dependency: pip install -e sdk/python"; exit 1; }
	@cd sdk/python && python3 tests/test_client.py
	@python3 -c 'import build' 2>/dev/null || { echo "sdk-check-python needs the build frontend: pip install build"; exit 1; }
	@cd sdk/python && rm -rf dist && python3 -m build
	@cd sdk/python && python3 ../scripts/verify-python-wheel.py

# sdk/swift is a submodule of lohi-ai/agentray-swift, which runs this same check
# in its own CI and cuts its own releases. It is here so a change can be made and
# tested from the monorepo; commit it in the submodule, then bump the pointer.
sdk-check-swift: ## AgentRay (SwiftPM) — submodule: lohi-ai/agentray-swift
	@test -f sdk/swift/Package.swift \
	  || { echo "sdk/swift is empty — run: git submodule update --init sdk/swift"; exit 1; }
	@cd sdk/swift && swift test

# --- Cutting an SDK release ----------------------------------------------
# GitHub Releases are the primary package host: the tag push builds the artefact,
# attaches it to a Release, and only then publishes to npm/PyPI — and skips that
# second step silently while the registry token is absent. Full runbook in
# docs/RELEASING-SDK.md.
#
# Swift is not here: tag bare semver in lohi-ai/agentray-swift and push it.
SDK_BUMP ?= patch
# browser and server share one check target; python has its own.
SDK_SUITE = $(if $(filter $(SDK_PKG),browser server),npm,$(SDK_PKG))

sdk-release: ## Bump + tag one SDK: make sdk-release SDK_PKG=browser SDK_BUMP=minor
	@test -n "$(filter $(SDK_PKG),browser server python)" \
	  || { echo "set SDK_PKG=browser|server|python (got '$(SDK_PKG)'); swift releases from lohi-ai/agentray-swift"; exit 1; }
	@$(MAKE) sdk-check-$(SDK_SUITE)
	@node sdk/scripts/cut-release.mjs "$(SDK_PKG)" "$(SDK_BUMP)"

sdk-resolve-tag: ## Show what a release tag would build: make sdk-resolve-tag TAG=browser-v0.2.0
	@node sdk/scripts/resolve-tag.mjs "$(TAG)"

agent-funcs: ## List the agent test functions this Makefile targets
	@grep -rhn '^func TestReal_\|^func TestLongRun' agentcore/*_test.go

# --- Sandbox setup (run on the Docker-equipped host the API uses) ---------

sandbox-build-cu: ## Build the computer_use toolchain image (docs/code/PDF stack)
	DOCKER_BUILDKIT=1 $(DOCKER) build $(PLATFORM_ARG) -f Dockerfile.computeruse -t $(CU_IMAGE) .

sandbox-build-browser: ## Build the browser_use image (Chromium + agent-browser; ENABLE_CLOAK=1 for stealth)
	DOCKER_BUILDKIT=1 $(DOCKER) build $(PLATFORM_ARG) --build-arg ENABLE_CLOAK=$(ENABLE_CLOAK) -f Dockerfile.browser -t $(BROWSER_IMAGE) .

sandbox-build-shell: ## Build the hardened run_shell image (opt-in; default backend is alpine)
	DOCKER_BUILDKIT=1 $(DOCKER) build $(PLATFORM_ARG) -f Dockerfile.sandbox -t $(SHELL_IMAGE) .

sandbox-build: sandbox-build-cu sandbox-build-browser ## Build the computer_use + browser_use images

sandbox-check: ## Report Docker availability and which sandbox images are present
	@$(DOCKER) info >/dev/null 2>&1 && echo "docker: available" || { echo "docker: NOT available — install/start Docker on this host"; exit 1; }
	@for img in $(CU_IMAGE) $(BROWSER_IMAGE) $(SHELL_IMAGE); do \
	  if $(DOCKER) image inspect "$$img" >/dev/null 2>&1; then echo "image present: $$img"; \
	  else echo "image MISSING: $$img"; fi; \
	done

sandbox-setup: sandbox-build sandbox-check ## Build the sandbox images, verify, then print the env knobs to enable them
	@echo; echo "Sandbox images ready. Enable the tools by setting in the API environment:"; echo
	@echo "  AGENTRAY_SANDBOX_ENABLED=true"
	@echo "  AGENTRAY_SANDBOX_COMPUTER_USE_IMAGE=$(CU_IMAGE)"
	@echo "  AGENTRAY_SANDBOX_BROWSER_IMAGE=$(BROWSER_IMAGE)"
	@echo "  # optional hardened run_shell backend:  AGENTRAY_SANDBOX_IMAGE=$(SHELL_IMAGE)"
	@echo
	@echo "On GCE these belong in infra/gce/<env>/app.env; the API container needs a Docker"
	@echo "socket + the docker CLI to reach the host daemon. A missing image degrades safely."

test-sandbox: ## Run the computer_use + browser_use integration tests (needs built images; loads .env)
	@$(LOAD_ENV) \
	$(GO) test ./sandbox/... -run 'ComputerUseAgent|BrowserUseAgent' -v -count=1
