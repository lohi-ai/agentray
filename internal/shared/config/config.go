package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	HTTPAddr           string
	PostgresURL        string
	// DuckDBPath is the embedded analytics database file. DuckDB owns the
	// directory: the WAL lands at <path>.wal and spill scratch at <dir>/tmp.
	// Relative paths resolve against the server working directory; the default
	// keeps a self-hosted `docker compose up` self-contained.
	DuckDBPath string
	// EventRetentionDays bounds how long events stay queryable. A sweep deletes
	// events older than this many days; 0 keeps every event forever.
	//
	// The default is not a new policy: the ClickHouse schema this store
	// replaced carried `TTL toDateTime(timestamp) + INTERVAL 1 YEAR`, so 365
	// restores the all-history semantics the product already had instead of
	// silently widening them. It is also the only bound on the per-colour
	// DuckDB file, which otherwise grows without limit on a single VM.
	EventRetentionDays int
	RedisURL             string
	NATSURL              string
	IngestSubject        string
	// IngestJetStream turns the event pipeline durable. When true (default) the
	// ingest subject is backed by a file-storage JetStream stream: publishes wait
	// for a broker ack (HTTP 200 means "durably queued") and the worker acks each
	// message only after the DuckDB insert succeeds, so a worker crash, server
	// restart, or engine outage redelivers instead of dropping events. Set
	// false to fall back to fire-and-forget core NATS (dev/tests without a JS-
	// enabled broker).
	IngestJetStream bool
	// IngestStreamName is the JetStream stream that captures IngestSubject.
	IngestStreamName string
	// IngestConnectorSubject carries connector sync batches on the SAME durable
	// stream as events, so a blue-green colour switch replays landed
	// external_rows exactly like events instead of losing them. Defaults to
	// IngestSubject + ".connectors", which inherits the per-env subject suffix
	// (dev/prod) from the one variable that already carries it.
	IngestConnectorSubject string
	// IngestDLQSubject receives batches that exhaust IngestMaxDeliver redelivery
	// attempts (poison payloads). Republish them with `agentray-server replay-dlq`.
	IngestDLQSubject string
	// IngestMaxDeliver bounds redelivery attempts before a batch is dead-lettered.
	IngestMaxDeliver int
	// IngestDurable names the JetStream durable consumer. Blue-green deploys
	// give each colour its own durable (e.g. agentray-ingestors-blue) so both
	// colours receive every message — a shared durable would split the stream
	// between them and the two DuckDB files would diverge. The durable also
	// carries the readiness contract: its ack floor is that colour's applied
	// high-water mark for events AND connector rows, which /readyz compares
	// against the stream head before a deploy switches traffic (see
	// ingest.ReplayStatus).
	IngestDurable string
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
	// still overrides this derived default in either direction.
	//
	// What it cannot do is re-open the host path once isolation has been asked
	// for and failed to wire: setting AGENTRAY_SANDBOX_ENABLED is itself an
	// explicit request, so a sandbox that does not come up withholds the tools
	// rather than moving them to the host, whatever this is set to (see
	// internal/app/app.go). "Docker when it's up, the host when it isn't" is not
	// a supported configuration — leave AGENTRAY_SANDBOX_ENABLED unset for the
	// host path.
	SandboxRequired bool
	// AgentWorkspaceRoot is the BASE for per-conversation agent workspaces
	// (<base>/<workspaceId>/<projectId>/<agentId>/<conversationId>). Empty uses
	// ~/.agentray/workspaces, so the file tools work with nothing configured.
	AgentWorkspaceRoot string
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
	// AgentMaxTurns / AgentMaxToolCalls override the hard per-run ceilings (LLM
	// calls, tool executions). 0 — the default — keeps agentcore's 24/40. They
	// exist because the right ceiling is a property of the deployment, not of the
	// library: a workspace whose agents walk an unfamiliar warehouse schema needs
	// more room than one answering off a curated mart, and hitting the ceiling
	// costs a real answer — the run wraps up honestly instead of finishing.
	AgentMaxTurns     int
	AgentMaxToolCalls int
	// The shared demo workspace — one feature, two keys.
	//
	// DemoProjectID is the project id of a real, read-only project every visitor
	// is shown, so the first thing a signed-up viewer sees is live data instead of
	// an empty state they have to fill before AgentRay means anything. Empty — the
	// default — means this instance has no demo and the surface stays hidden.
	DemoProjectID string
	// DemoAgentRunsPerUserPerDay caps how many agent runs one signed-up viewer may
	// trigger inside that demo per day. It is a spend control, not an abuse
	// control: demo runs bill the INSTANCE owner's model key, not the viewer's, so
	// an unbounded demo is an unbounded bill.
	DemoAgentRunsPerUserPerDay int
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
		DuckDBPath:                   env("DUCKDB_PATH", "./data/agentray.duckdb"),
		EventRetentionDays:           eventRetentionDays(),
		RedisURL:                     env("REDIS_URL", "redis://localhost:6389/0"),
		NATSURL:                      env("NATS_URL", "nats://localhost:4223"),
		IngestSubject:                env("INGEST_SUBJECT", "agentray.events.ingest"),
		IngestConnectorSubject:       env("INGEST_CONNECTOR_SUBJECT", env("INGEST_SUBJECT", "agentray.events.ingest")+".connectors"),
		IngestJetStream:              envBool("INGEST_JETSTREAM", true),
		IngestStreamName:             env("INGEST_STREAM_NAME", "AGENTRAY_EVENTS"),
		IngestDLQSubject:             env("INGEST_DLQ_SUBJECT", "agentray.events.dlq"),
		IngestMaxDeliver:             envInt("INGEST_MAX_DELIVER", 5),
		IngestDurable:                env("INGEST_DURABLE", "agentray-ingestors"),
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
		Hosted:                       envBool("AGENTRAY_HOSTED", false),
		CredentialsEnabled:           envBool("AGENTRAY_CREDENTIALS_ENABLED", false),
		HTTPToolEnabled:              envBool("AGENTRAY_HTTP_TOOL_ENABLED", false),
		HTTPToolAllowHosts:           os.Getenv("AGENTRAY_HTTP_TOOL_ALLOW_HOSTS"),
		HTTPToolAllowHTTP:            envBool("AGENTRAY_HTTP_TOOL_ALLOW_HTTP", false),
		AgentTraceFile:               os.Getenv("AGENTRAY_AGENT_TRACE_FILE"),
		AgentMaxContextTokens:        envInt("AGENTRAY_AGENT_MAX_CONTEXT_TOKENS", 0),
		AgentKeepRecentTokens:        envInt("AGENTRAY_AGENT_KEEP_RECENT_TOKENS", 0),
		AgentMaxTurns:                envInt("AGENTRAY_AGENT_MAX_TURNS", 0),
		AgentMaxToolCalls:            envInt("AGENTRAY_AGENT_MAX_TOOL_CALLS", 0),
		DemoProjectID:                os.Getenv("AGENTRAY_DEMO_PROJECT_ID"),
		DemoAgentRunsPerUserPerDay:   envInt("AGENTRAY_DEMO_AGENT_RUNS_PER_USER_PER_DAY", 5),
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

// eventRetentionDays reads EVENT_RETENTION_DAYS. 0 is the operator's explicit
// "keep everything"; a negative value is a typo, so it falls back to the
// default rather than quietly removing the only bound on the event log. A
// non-numeric value falls back the same way (envInt's own contract).
func eventRetentionDays() int {
	const fallback = 365
	days := envInt("EVENT_RETENTION_DAYS", fallback)
	if days < 0 {
		return fallback
	}
	return days
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
