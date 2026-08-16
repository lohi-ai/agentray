package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	HTTPAddr           string
	PostgresURL        string
	ClickHouseAddr     string
	ClickHouseDatabase string
	ClickHouseUser     string
	ClickHousePassword string
	// ClickHouseROUser / ClickHouseROPassword name a least-privilege ClickHouse
	// account used for every agent- or user-authored SELECT (run_sql, /api/sql/run,
	// saved queries). It is provisioned by migrateClickHouse with GRANT SELECT on the
	// project database only and a readonly=2 profile, so a table-function bypass
	// (`SELECT * FROM url(...)`) fails with a grant error instead of exfiltrating.
	// Empty user = fall back to the primary connection (dev convenience); production
	// sets it. Password may be empty for a trusted local CH.
	ClickHouseROUser     string
	ClickHouseROPassword string
	RedisURL             string
	NATSURL              string
	IngestSubject        string
	// IngestJetStream turns the event pipeline durable. When true (default) the
	// ingest subject is backed by a file-storage JetStream stream: publishes wait
	// for a broker ack (HTTP 200 means "durably queued") and the worker acks each
	// message only after the ClickHouse insert succeeds, so a worker crash, server
	// restart, or ClickHouse outage redelivers instead of dropping events. Set
	// false to fall back to fire-and-forget core NATS (dev/tests without a JS-
	// enabled broker).
	IngestJetStream bool
	// IngestStreamName is the JetStream stream that captures IngestSubject.
	IngestStreamName string
	// IngestDLQSubject receives batches that exhaust IngestMaxDeliver redelivery
	// attempts (poison payloads). Republish them with `agentray-server replay-dlq`.
	IngestDLQSubject string
	// IngestMaxDeliver bounds redelivery attempts before a batch is dead-lettered.
	IngestMaxDeliver int
	// PipelineMetricsProjectAPIKey names the project that pipeline self-metrics
	// (system.pipeline.* events: flush size, insert failures, dead-letters, ingest
	// lag) are written to, so the existing alerting/dashboards observe the pipeline
	// itself. Defaults to DefaultProjectAPIKey; empty disables self-metrics.
	PipelineMetricsProjectAPIKey string
	RateLimitPerMinute           int
	DefaultProjectName           string
	DefaultProjectAPIKey         string
	AllowedOrigins               string
	// Sandbox toggles isolated execution for agent tools that run untrusted code
	// (run_shell today). Off by default — agents stay analytics-only. When on and
	// Docker is reachable, each run gets a DockerSandbox + the injection guard.
	SandboxEnabled      bool
	SandboxImage        string   // optional override (e.g. a hardened minimal-PATH image); empty = backend default
	SandboxCUImage      string   // optional image for persistent computer_use sessions (rich python/pandoc/office toolchain); empty = SandboxImage
	SandboxBrowserImage string   // optional image for persistent browser_use sessions (Chrome + agent-browser); empty disables real browsing
	SandboxNetworkAllow []string // optional egress allowlist for computer_use (comma-separated hosts); empty = open network (#5b)
	SandboxDockerBin    string   // optional docker CLI path; empty = "docker"
	// SandboxRequired withholds run_shell / computer_use / browser_use unless a
	// working sandbox is wired, instead of running them on the host inside the
	// agent's workspace.
	//
	// It defaults to Hosted, and that coupling is the whole point. Requiring
	// Docker to run an agent at all is a wall in front of every new user, and the
	// common self-host is one operator on one machine the agent is already
	// trusted with — so a local install stays open. A hosted, multi-tenant
	// install is the opposite case, and AGENT-GOVERNANCE.md states the rule it
	// has to keep: a hosted deployment "never gains a host shell by omission".
	// Deriving it means nobody has to remember a second env var for the
	// deployment where forgetting it hands one tenant's prompt-injected agent a
	// shell in the process that serves all of them. AGENTRAY_SANDBOX_REQUIRED
	// still overrides in either direction.
	SandboxRequired bool
	// AgentWorkspaceRoot is the BASE for per-conversation agent workspaces
	// (<base>/<workspaceId>/<projectId>/<agentId>/<conversationId>). Empty uses
	// ~/.agentray/workspaces, so the file tools work with nothing configured.
	AgentWorkspaceRoot string
	SeedDemo           bool // AGENTRAY_SEED_DEMO: seed ~2 days of synthetic events into the default project on first boot (compose quickstart only, #3b)
	// Hosted marks this as the managed cloud (agentray.lohi2.com) rather than a
	// `docker compose up` instance. Off by default so a self-host operator never
	// sees a pricing page or a usage ceiling for a plan they cannot buy — the web
	// app reads it off the auth payload and hides every plan surface when false.
	Hosted bool
	// CredentialsEnabled turns on the {{cred:NAME}} secret vault (governance F7).
	// Off by default. When on, the host loads every AGENTRAY_CRED_* env var into
	// an in-memory vault and threads it into every run, so an agent can use a
	// secret by name without the model ever seeing the literal value.
	CredentialsEnabled bool
	// HTTPToolEnabled turns on the outbound http_request tool — the worked
	// consumer of the credential vault. Off by default. It is refused unless a
	// non-empty host allowlist is configured.
	HTTPToolEnabled    bool
	HTTPToolAllowHosts string // comma-separated exact-match host allowlist
	HTTPToolAllowHTTP  bool   // permit plain http:// (default: https only)
	// AgentTraceFile, when set, is a path the per-LLM-call trace is appended to as
	// JSONL (request messages, response, tokens, computed cost, latency). Empty —
	// the default — disables trace emission; cost is still computed and persisted.
	AgentTraceFile string
	// AgentMaxContextTokens overrides the loop's soft compaction budget (the
	// context size above which old turns are summarized). 0 — the default —
	// keeps agentcore's 300k. A deployment/test knob to tune or exercise
	// compaction.
	//
	// It is a CEILING, not the budget: the answering model's own context window
	// caps it further, per rung, so this never has to be sized for the smallest
	// model a workspace might configure. Lower it to spend less; the per-tier
	// context window (Settings → AI Provider) is the right place to describe a
	// specific endpoint.
	AgentMaxContextTokens int
	// AgentKeepRecentTokens overrides how much recent context compaction keeps
	// verbatim (the rest of the older span is summarized). 0 — the default —
	// keeps agentcore's 20k. Must be below AgentMaxContextTokens for the LLM
	// summary path to engage; a deployment/test knob paired with the budget above.
	AgentKeepRecentTokens int
	// Hosted default model pool. When a workspace has no BYOK key, runs and the
	// Settings "has_key" flag fall back to this so the first ask works without
	// pasting a key. Empty API key disables the fallback (BYOK-only deploy).
	DefaultModelProvider string
	DefaultModelBaseURL  string
	DefaultModelAPIKey   string
	DefaultModelFlash    string
	DefaultModelLite     string
	DefaultModelPro      string
}

func FromEnv() Config {
	return Config{
		HTTPAddr:                     env("HTTP_ADDR", ":8080"),
		PostgresURL:                  env("POSTGRES_URL", "postgres://lohi:lohi@localhost:5434/lohi_analytics?sslmode=disable"),
		ClickHouseAddr:               env("CLICKHOUSE_ADDR", "localhost:9000"),
		ClickHouseDatabase:           env("CLICKHOUSE_DATABASE", "lohi_analytics"),
		ClickHouseUser:               env("CLICKHOUSE_USER", "lohi"),
		ClickHousePassword:           os.Getenv("CLICKHOUSE_PASSWORD"),
		ClickHouseROUser:             os.Getenv("CLICKHOUSE_RO_USER"),
		ClickHouseROPassword:         os.Getenv("CLICKHOUSE_RO_PASSWORD"),
		RedisURL:                     env("REDIS_URL", "redis://localhost:6389/0"),
		NATSURL:                      env("NATS_URL", "nats://localhost:4223"),
		IngestSubject:                env("INGEST_SUBJECT", "agentray.events.ingest"),
		IngestJetStream:              envBool("INGEST_JETSTREAM", true),
		IngestStreamName:             env("INGEST_STREAM_NAME", "AGENTRAY_EVENTS"),
		IngestDLQSubject:             env("INGEST_DLQ_SUBJECT", "agentray.events.dlq"),
		IngestMaxDeliver:             envInt("INGEST_MAX_DELIVER", 5),
		PipelineMetricsProjectAPIKey: env("PIPELINE_METRICS_PROJECT_API_KEY", env("DEFAULT_PROJECT_API_KEY", "lohi_dev_project_token")),
		RateLimitPerMinute:           envInt("RATE_LIMIT_PER_MINUTE", 600),
		DefaultProjectName:           env("DEFAULT_PROJECT_NAME", "AgentRay local"),
		DefaultProjectAPIKey:         env("DEFAULT_PROJECT_API_KEY", "lohi_dev_project_token"),
		AllowedOrigins:               env("ALLOWED_ORIGINS", "http://localhost:3100,http://127.0.0.1:3100,http://localhost:3200,http://127.0.0.1:3200"),
		SandboxEnabled:               envBool("AGENTRAY_SANDBOX_ENABLED", false),
		SandboxImage:                 os.Getenv("AGENTRAY_SANDBOX_IMAGE"),
		SandboxCUImage:               os.Getenv("AGENTRAY_SANDBOX_COMPUTER_USE_IMAGE"),
		SandboxBrowserImage:          os.Getenv("AGENTRAY_SANDBOX_BROWSER_IMAGE"),
		SandboxNetworkAllow:          envList("AGENTRAY_SANDBOX_NETWORK_ALLOW"),
		SandboxDockerBin:             os.Getenv("AGENTRAY_SANDBOX_DOCKER_BIN"),
		AgentWorkspaceRoot:           os.Getenv("AGENTRAY_AGENT_WORKSPACE_ROOT"),
		SandboxRequired:              envBool("AGENTRAY_SANDBOX_REQUIRED", envBool("AGENTRAY_HOSTED", false)),
		SeedDemo:                     envBool("AGENTRAY_SEED_DEMO", false),
		Hosted:                       envBool("AGENTRAY_HOSTED", false),
		CredentialsEnabled:           envBool("AGENTRAY_CREDENTIALS_ENABLED", false),
		HTTPToolEnabled:              envBool("AGENTRAY_HTTP_TOOL_ENABLED", false),
		HTTPToolAllowHosts:           os.Getenv("AGENTRAY_HTTP_TOOL_ALLOW_HOSTS"),
		HTTPToolAllowHTTP:            envBool("AGENTRAY_HTTP_TOOL_ALLOW_HTTP", false),
		AgentTraceFile:               os.Getenv("AGENTRAY_AGENT_TRACE_FILE"),
		AgentMaxContextTokens:        envInt("AGENTRAY_AGENT_MAX_CONTEXT_TOKENS", 0),
		AgentKeepRecentTokens:        envInt("AGENTRAY_AGENT_KEEP_RECENT_TOKENS", 0),
		// Hosted model: dedicated DEFAULT_* vars, then the real-provider test
		// endpoint so a local `make dev` that already has AGENTRAY_TEST_OPENAI_*
		// can answer the first ask without a second paste.
		DefaultModelProvider: env("AGENTRAY_DEFAULT_MODEL_PROVIDER", "openai"),
		DefaultModelBaseURL:  env("AGENTRAY_DEFAULT_MODEL_BASE_URL", os.Getenv("AGENTRAY_TEST_OPENAI_BASE_URL")),
		DefaultModelAPIKey:   env("AGENTRAY_DEFAULT_MODEL_API_KEY", os.Getenv("AGENTRAY_TEST_OPENAI_API_KEY")),
		DefaultModelFlash:    env("AGENTRAY_DEFAULT_MODEL_FLASH", "flash"),
		DefaultModelLite:     env("AGENTRAY_DEFAULT_MODEL_LITE", "plus"),
		DefaultModelPro:      env("AGENTRAY_DEFAULT_MODEL_PRO", "pro"),
	}
}

func envBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// envList parses a comma-separated env var into a trimmed, non-empty slice
// (empty var → nil).
func envList(key string) []string {
	raw := os.Getenv(key)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func env(key string, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
