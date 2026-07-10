# AgentRay developer tasks.
#
# Agent tests come in two layers (see internal/agentcore/agent_realprovider_test.go):
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

.PHONY: help dev web build cli install-cli vet test test-agents test-stress check agent-funcs \
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
	$(GO) test ./internal/... -run 'TestReal_|RealProvider' -v -count=1

test-stress: ## Run the long-run stability / compaction stress test
	$(GO) test ./internal/agentcore/... -run TestLongRunStaysStableAcrossManyCompactions -v -count=1

check: vet test ## Vet + unit tests — the pre-commit gate

agent-funcs: ## List the agent test functions this Makefile targets
	@grep -rhn '^func TestReal_\|^func TestLongRun' internal/agentcore/*_test.go

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
	$(GO) test ./internal/sandbox/... -run 'ComputerUseAgent|BrowserUseAgent' -v -count=1
