package app

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
	"github.com/lohi-ai/agentray/internal/dataplane/alerting"
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	"github.com/lohi-ai/agentray/internal/dataplane/ingest"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/runtime"
	"github.com/lohi-ai/agentray/internal/shared/config"
	"github.com/lohi-ai/agentray/internal/shared/credential"
	"github.com/lohi-ai/agentray/sandbox"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
)

type Server struct {
	echo      *echo.Echo
	db        *storage.Store
	redis     *redis.Client
	nats      *nats.Conn
	worker    *ingestion.EventWorker
	scheduler *agentruntime.Scheduler
}

func New(ctx context.Context, cfg config.Config) (*Server, error) {
	store, err := storage.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	// Dataplane store must not import workloads; inject the pack catalog here.
	storage.SetPackCatalog(marketplacePresets, marketplacePresetBySlug)
	redisOptions, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		store.Close()
		return nil, err
	}
	redisClient := redis.NewClient(redisOptions)
	if err := redisClient.Ping(ctx).Err(); err != nil {
		store.Close()
		_ = redisClient.Close()
		return nil, err
	}
	nc, err := nats.Connect(cfg.NATSURL, nats.Name("AgentRay ingestion"))
	if err != nil {
		store.Close()
		_ = redisClient.Close()
		return nil, err
	}
	// Durable event pipeline (default): a file-backed JetStream stream makes an
	// HTTP 200 mean "durably queued" and the worker acks only after the ClickHouse
	// insert lands, so a crash / restart / ClickHouse outage redelivers instead of
	// dropping events. INGEST_JETSTREAM=false falls back to fire-and-forget core
	// NATS for a broker without JetStream (dev/tests).
	var (
		worker *ingestion.EventWorker
		queue  ingestion.EventQueue
	)
	if cfg.IngestJetStream {
		ss, err := ingestion.EnsureStreams(ctx, nc, cfg)
		if err != nil {
			store.Close()
			_ = redisClient.Close()
			nc.Close()
			return nil, err
		}
		metrics := buildPipelineMetrics(ctx, cfg, store)
		worker, err = ingestion.StartJetStreamWorker(ctx, ss, store, metrics)
		if err != nil {
			store.Close()
			_ = redisClient.Close()
			nc.Close()
			return nil, err
		}
		queue = ingestion.NewJetStreamQueue(ss.JS, cfg.IngestSubject)
	} else {
		worker, err = ingestion.StartEventWorker(nc, cfg.IngestSubject, store)
		if err != nil {
			store.Close()
			_ = redisClient.Close()
			nc.Close()
			return nil, err
		}
		queue = ingestion.NewEventQueue(nc, cfg.IngestSubject)
	}
	rateLimit := ingestion.RedisRateLimit(redisClient, cfg.RateLimitPerMinute, time.Minute)
	// Credential endpoints get a separate, much tighter per-IP limiter so the
	// generous ingest ceiling never applies to password verification.
	authRateLimit := ingestion.AuthRateLimit(redisClient, 20, time.Minute)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Use(middleware.Recover())
	// Data collection is called from the CUSTOMER's site, whose origin we cannot
	// know — a landing page on Framer, Carrd, or their own domain. Those paths
	// therefore carry their own permissive CORS, with credentials OFF: they
	// authenticate by the project API key, which is a public write-only key, and
	// they never read a cookie. Everything else keeps the strict allow-list
	// below, so the browser is still what stops a third-party page from acting
	// as a logged-in user against /api.
	//
	// This runs before the strict config and short-circuits its own preflights,
	// because two CORS middlewares on one request would emit two
	// Allow-Origin headers and the browser rejects that.
	// One set per server, filled by registerRoutes below (which runs to completion
	// before this server ever serves a request) and read-only from then on.
	collectPaths := publicCollectSet{}
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		Skipper:      func(c echo.Context) bool { return !collectPaths.has(c.Request().URL.Path) },
		AllowOrigins: []string{"*"},
		AllowMethods: []string{echo.GET, echo.POST, echo.OPTIONS},
		AllowHeaders: []string{echo.HeaderOrigin, echo.HeaderContentType, echo.HeaderAccept, "X-API-Key"},
	}))
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		Skipper:          func(c echo.Context) bool { return collectPaths.has(c.Request().URL.Path) },
		AllowOrigins:     strings.Split(cfg.AllowedOrigins, ","),
		AllowMethods:     []string{echo.GET, echo.POST, echo.PUT, echo.PATCH, echo.DELETE, echo.OPTIONS},
		AllowHeaders:     []string{echo.HeaderOrigin, echo.HeaderContentType, echo.HeaderAccept, echo.HeaderAuthorization, "X-API-Key"},
		AllowCredentials: true,
	}))

	// Build the agent isolation substrate once and thread it (as RunnerOptions)
	// into both run paths — the NATS scheduler and the HTTP chat handler. Nil when
	// disabled or Docker is unreachable, leaving agents analytics-only.
	var runnerOpts []agentruntime.RunnerOption
	sb := buildSandbox(ctx, cfg)
	if sb != nil {
		runnerOpts = append(runnerOpts, agentruntime.WithSandbox(sb))
	}
	wsBase := workspaceBase(cfg, sb != nil)
	// An operator who set AGENTRAY_SANDBOX_ENABLED asked for isolation. If Docker
	// turned out to be unreachable, `sb` is nil — and "someone forgot to wire
	// Docker" must not resolve to "run the model's commands on the host": the
	// request was explicit, so the failure fails closed.
	isolationRequired := cfg.SandboxRequired || (cfg.SandboxEnabled && sb == nil)
	runnerOpts = append(runnerOpts,
		agentruntime.WithWorkspaceBase(wsBase),
		agentruntime.WithSandboxRequired(isolationRequired),
		// A pinned folder roots the agent anywhere on the host, and is also what the
		// sandboxed tools bind-mount read-write. That is the user's call on a
		// self-hosted box they own; on the managed cloud the user is a tenant who
		// signed up, so the derived layout is the only workspace they get.
		agentruntime.WithPinnedWorkspacesDisabled(cfg.Hosted),
	)
	// The browser_use tool runs its persistent session in a dedicated Chrome image
	// (separate from the computer_use doc-toolchain image); thread it so both the
	// scheduled and HTTP-chat run paths build browser_use against it.
	if cfg.SandboxBrowserImage != "" {
		runnerOpts = append(runnerOpts, agentruntime.WithBrowserImage(cfg.SandboxBrowserImage))
	}
	// Egress allowlist (#5b): confine computer_use's network to listed hosts.
	if len(cfg.SandboxNetworkAllow) > 0 {
		runnerOpts = append(runnerOpts, agentruntime.WithNetworkAllow(cfg.SandboxNetworkAllow))
	}
	if cv := buildCredentials(cfg); cv != nil {
		runnerOpts = append(runnerOpts, agentruntime.WithCredentials(cv))
	}
	if ht := buildHTTPTool(cfg); ht != nil {
		runnerOpts = append(runnerOpts, agentruntime.WithHTTPTool(ht))
	}
	// Trace sinks fan out every per-LLM-call TraceRecord. The DB sink is always
	// on — it is the monitoring console's source of truth (one row per LLM call,
	// keyed by run → agent_id). An optional JSONL file sink is added for offline
	// debugging when AGENTRAY_AGENT_TRACE_FILE is set.
	traceSinks := observe.MultiSink{agentruntime.NewStoreTraceSink(store)}
	if cfg.AgentTraceFile != "" {
		if fs, err := observe.NewFileSink(cfg.AgentTraceFile); err != nil {
			log.Printf("agent trace file %q: %v (file tracing disabled)", cfg.AgentTraceFile, err)
		} else {
			traceSinks = append(traceSinks, fs)
		}
	}
	runnerOpts = append(runnerOpts, agentruntime.WithTraceSink(traceSinks))

	// Durable, resumable runs: a Postgres-backed append-only session log keyed on
	// the run id, so a crashed/compacted run can be reduced and replayed via the
	// resume endpoint. Always on — it is additive (an unconsumed log) and backs the
	// resume path.
	runnerOpts = append(runnerOpts, agentruntime.WithSessionStore(agentruntime.NewSessionStore(store)))
	// Durable oversized output: a tool result too large to sit inline is persisted
	// and replaced by a preview plus a locator, so the omitted middle is one
	// read_spill call away instead of gone. It shares the session log's storage
	// and lifetime — the locator lives in that log, so the artifact has to outlive
	// the process the same way the log does.
	runnerOpts = append(runnerOpts, agentruntime.WithSpillStore(agentruntime.NewSpillStore(store)))
	// Rotation-safe long runs: re-resolve each rung's BYO key before every turn.
	runnerOpts = append(runnerOpts, agentruntime.WithKeyRefresh())
	// Optional compaction-budget override (deployment/test knob); 0 keeps the 200k
	// default.
	if cfg.AgentMaxContextTokens > 0 {
		runnerOpts = append(runnerOpts, agentruntime.WithMaxContextTokens(cfg.AgentMaxContextTokens))
	}
	if cfg.AgentKeepRecentTokens > 0 {
		runnerOpts = append(runnerOpts, agentruntime.WithKeepRecentTokens(cfg.AgentKeepRecentTokens))
	}
	// Live control registry: one process-wide instance shared by every chat run
	// (via WithLiveRegistry) and the steer/follow-up HTTP handlers, so a sibling
	// request can drive an in-flight run keyed on the client conversation id.
	liveReg := agentruntime.NewLiveRegistry()
	runnerOpts = append(runnerOpts, agentruntime.WithLiveRegistry(liveReg))

	// Alerting (#1): one deliverer serves both the scheduled evaluator and the
	// send_notification agent tool. Delivery resolves {{cred:NAME}} in channel
	// config against the host vault (nil when credentials are disabled → config
	// used verbatim, which is correct for public webhook URLs).
	var alertVault *credential.Vault
	if cfg.CredentialsEnabled {
		if v := credential.LoadFromEnviron(os.Environ()); v.Len() > 0 {
			alertVault = v
		}
	}
	alertDeliverer := alerting.NewDeliverer(alertVault)
	runnerOpts = append(runnerOpts, agentruntime.WithNotifier(alertDeliverer))

	scheduler := agentruntime.NewScheduler(nc, store, runnerOpts...)
	// The evaluator and the connector sync engine ride the scheduler's minute
	// tick, sharing one clock with scheduled runs instead of standing up more
	// timers.
	alertEval := alerting.NewEvaluator(store, alertDeliverer)
	connectorEngine := connector.NewEngine(store)
	scheduler.OnTick(func(tickCtx context.Context, now time.Time) {
		alertEval.Tick(tickCtx, now)
		connectorEngine.Tick(tickCtx, now)
	})
	if err := scheduler.Start(ctx); err != nil {
		store.Close()
		_ = redisClient.Close()
		_ = worker.Stop()
		nc.Close()
		return nil, err
	}

	registerRoutes(e, store, queue, rateLimit, authRateLimit, scheduler, sb, agentruntime.ToolBuildContext{Sandbox: sb, SandboxRequired: isolationRequired, WorkspaceBase: wsBase}, liveReg, cfg.Hosted, collectPaths, runnerOpts...)
	registerOpRoutes(e, store, alertDeliverer)
	registerMcpRoutes(e, store, alertDeliverer)
	registerConnectorRoutes(e, store, connectorEngine)
	registerTeamRoutes(e, store)

	return &Server{echo: e, db: store, redis: redisClient, nats: nc, worker: worker, scheduler: scheduler}, nil
}

// buildPipelineMetrics resolves the project that ingest self-metrics are written
// to (system.pipeline.* events), so the existing alerting/dashboards observe the
// pipeline. Returns nil-project metrics (counts but never emits) when the key is
// unset or unresolvable — self-metrics are observability, never a boot blocker.
func buildPipelineMetrics(ctx context.Context, cfg config.Config, store *storage.Store) *ingestion.PipelineMetrics {
	key := strings.TrimSpace(cfg.PipelineMetricsProjectAPIKey)
	if key == "" {
		return ingestion.NewPipelineMetrics(store, "", 30*time.Second)
	}
	project, err := store.ProjectByAPIKey(ctx, key)
	if err != nil {
		log.Printf("agentray: pipeline self-metrics disabled (project for PIPELINE_METRICS_PROJECT_API_KEY not found: %v)", err)
		return ingestion.NewPipelineMetrics(store, "", 30*time.Second)
	}
	log.Printf("agentray: pipeline self-metrics enabled (project %q)", project.ID)
	return ingestion.NewPipelineMetrics(store, project.ID, 30*time.Second)
}

// buildSandbox constructs the agent isolation substrate from config. It returns
// nil — leaving agents analytics-only — when the feature is disabled, and also
// when it is enabled but Docker is unreachable (logged loudly rather than
// failing startup, so a misconfigured host degrades safely instead of crashing).
func buildSandbox(ctx context.Context, cfg config.Config) agentcore.Sandbox {
	if !cfg.SandboxEnabled {
		return nil
	}
	var opts []sandbox.Option
	if cfg.SandboxImage != "" {
		opts = append(opts, sandbox.WithImage(cfg.SandboxImage))
	}
	if cfg.SandboxCUImage != "" {
		opts = append(opts, sandbox.WithComputerUseImage(cfg.SandboxCUImage))
	}
	if cfg.SandboxDockerBin != "" {
		opts = append(opts, sandbox.WithDockerBinary(cfg.SandboxDockerBin))
	}
	sb := sandbox.NewDockerSandbox(opts...)

	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if !sb.Available(checkCtx) {
		log.Printf("agentray: AGENTRAY_SANDBOX_ENABLED is set but Docker/image %q is unavailable; run_shell, computer_use and browser_use are WITHHELD (they are not silently moved to the host — this operator asked for isolation)", sb.Image())
		return nil
	}
	log.Printf("agentray: agent sandbox enabled (image %q)", sb.Image())
	return sb
}

// workspaceBase resolves the root under which each run gets its own workspace,
// and says out loud which execution substrate the risky tools will use.
//
// The announcement is the point of doing this at startup rather than lazily per
// run. Host execution is a legitimate choice and the default one, but it must
// never be a thing an operator discovers later: "I meant to enable the sandbox"
// and "I chose host mode" produce identical behavior and very different
// intentions, and only the log can tell them apart afterwards.
func workspaceBase(cfg config.Config, sandboxWired bool) string {
	base := strings.TrimSpace(cfg.AgentWorkspaceRoot)
	if base == "" {
		def, err := sandbox.DefaultWorkspaceBase()
		if err != nil {
			log.Printf("agentray: no agent workspace root and no home directory (%v); file tools will fail closed", err)
			return ""
		}
		base = def
	}
	switch {
	case sandboxWired:
		log.Printf("agentray: agent workspaces under %q, tools run inside the sandbox", base)
	case cfg.SandboxRequired || cfg.SandboxEnabled:
		log.Printf("agentray: agent workspaces under %q; isolation is required (hosted, AGENTRAY_SANDBOX_REQUIRED, or "+
			"AGENTRAY_SANDBOX_ENABLED with no working sandbox) and none is available, "+
			"so run_shell/computer_use/browser_use are withheld", base)
	default:
		log.Printf("agentray: agent workspaces under %q; NO SANDBOX — run_shell/computer_use/browser_use execute on this "+
			"host as this process, confined to the run's workspace. Set AGENTRAY_SANDBOX_ENABLED=true to isolate them, "+
			"or AGENTRAY_SANDBOX_REQUIRED=true to withhold them instead.", base)
	}
	return base
}

// buildCredentials constructs the {{cred:NAME}} secret vault from the host
// environment (governance F7). It returns nil — leaving tool arguments
// untouched — when the feature is disabled, and also when it is enabled but no
// AGENTRAY_CRED_* variables are present (logged, so a misconfiguration is
// visible rather than silently no-op).
func buildCredentials(cfg config.Config) agentcore.CredentialResolver {
	if !cfg.CredentialsEnabled {
		return nil
	}
	vault := credential.LoadFromEnviron(os.Environ())
	if vault.Len() == 0 {
		log.Printf("agentray: AGENTRAY_CREDENTIALS_ENABLED is set but no %s* variables found; credential vault disabled", credential.EnvPrefix)
		return nil
	}
	log.Printf("agentray: agent credential vault enabled (%d credential(s): %v)", vault.Len(), vault.Names())
	return vault
}

// buildHTTPTool constructs the outbound http_request tool — the worked consumer
// of the credential vault. It returns nil (no outbound HTTP surface) when the
// feature is disabled, and also when it is enabled but no host allowlist is
// configured: an outbound HTTP tool with an empty allowlist would be both
// useless and a standing SSRF risk, so it is refused (logged) rather than
// shipped open.
func buildHTTPTool(cfg config.Config) agentcore.Tool {
	if !cfg.HTTPToolEnabled {
		return nil
	}
	hosts := splitAndTrim(cfg.HTTPToolAllowHosts)
	if len(hosts) == 0 {
		log.Printf("agentray: AGENTRAY_HTTP_TOOL_ENABLED is set but AGENTRAY_HTTP_TOOL_ALLOW_HOSTS is empty; http_request tool disabled")
		return nil
	}
	// Host substrate (nil sandbox): the tool can also make its request from
	// inside a container, but that needs curl in the sandbox image and the
	// default one has none. See buildHTTPRequestTool in internal/runtime.
	tool := sandbox.NewHTTPRequestTool(nil,
		sandbox.WithHTTPAllowHosts(hosts),
		sandbox.WithHTTPAllowPlain(cfg.HTTPToolAllowHTTP),
	)
	log.Printf("agentray: agent http_request tool enabled (allow-hosts: %v)", tool.AllowHosts())
	return tool
}

// splitAndTrim parses a comma-separated list, dropping empty entries.
func splitAndTrim(csv string) []string {
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) Start(addr string) error {
	return s.echo.Start(addr)
}

func (s *Server) Shutdown(ctx context.Context) error {
	err := s.echo.Shutdown(ctx)
	if s.scheduler != nil {
		s.scheduler.Stop()
	}
	_ = s.worker.Stop()
	if s.nats != nil {
		s.nats.Close()
	}
	if s.redis != nil {
		_ = s.redis.Close()
	}
	s.db.Close()
	return err
}
