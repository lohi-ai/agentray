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
	AgentWorkspaceRoot  string   // optional root for read_file/write_file; empty disables file tools
	SeedDemo            bool     // AGENTRAY_SEED_DEMO: seed ~2 days of synthetic events into the default project on first boot (compose quickstart only, #3b)
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
	// context size above which old turns are summarized). 0 — the default — keeps
	// agentcore's 200k. A deployment/test knob to tune or exercise compaction.
	AgentMaxContextTokens int
	// AgentKeepRecentTokens overrides how much recent context compaction keeps
	// verbatim (the rest of the older span is summarized). 0 — the default —
	// keeps agentcore's 20k. Must be below AgentMaxContextTokens for the LLM
	// summary path to engage; a deployment/test knob paired with the budget above.
	AgentKeepRecentTokens int
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
		SeedDemo:                     envBool("AGENTRAY_SEED_DEMO", false),
		CredentialsEnabled:           envBool("AGENTRAY_CREDENTIALS_ENABLED", false),
		HTTPToolEnabled:              envBool("AGENTRAY_HTTP_TOOL_ENABLED", false),
		HTTPToolAllowHosts:           os.Getenv("AGENTRAY_HTTP_TOOL_ALLOW_HOSTS"),
		HTTPToolAllowHTTP:            envBool("AGENTRAY_HTTP_TOOL_ALLOW_HTTP", false),
		AgentTraceFile:               os.Getenv("AGENTRAY_AGENT_TRACE_FILE"),
		AgentMaxContextTokens:        envInt("AGENTRAY_AGENT_MAX_CONTEXT_TOKENS", 0),
		AgentKeepRecentTokens:        envInt("AGENTRAY_AGENT_KEEP_RECENT_TOKENS", 0),
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
