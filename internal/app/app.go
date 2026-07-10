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
	"github.com/lohi-ai/agentray/internal/agentruntime"
	"github.com/lohi-ai/agentray/internal/alerting"
	"github.com/lohi-ai/agentray/internal/config"
	"github.com/lohi-ai/agentray/internal/credential"
	"github.com/lohi-ai/agentray/internal/httptool"
	"github.com/lohi-ai/agentray/internal/ingestion"
	"github.com/lohi-ai/agentray/sandbox"
	"github.com/lohi-ai/agentray/internal/storage"
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
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
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
	ws := buildWorkspace(cfg)
	if ws != nil {
		runnerOpts = append(runnerOpts, agentruntime.WithWorkspace(ws))
	}
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
	traceSinks := agentcore.MultiSink{agentruntime.NewStoreTraceSink(store)}
	if cfg.AgentTraceFile != "" {
		if fs, err := agentcore.NewFileTraceSink(cfg.AgentTraceFile); err != nil {
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
	// The evaluator rides the scheduler's minute tick, sharing one clock with
	// scheduled runs instead of standing up a second timer.
	alertEval := alerting.NewEvaluator(store, alertDeliverer)
	scheduler.OnTick(func(tickCtx context.Context, now time.Time) {
		alertEval.Tick(tickCtx, now)
	})
	if err := scheduler.Start(ctx); err != nil {
		store.Close()
		_ = redisClient.Close()
		_ = worker.Stop()
		nc.Close()
		return nil, err
	}

	registerRoutes(e, store, queue, rateLimit, authRateLimit, scheduler, sb, ws, liveReg, runnerOpts...)
	registerOpRoutes(e, store, alertDeliverer)
	registerMcpRoutes(e, store, alertDeliverer)

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
		log.Printf("agentray: AGENTRAY_SANDBOX_ENABLED is set but Docker/image %q is unavailable; sandboxed agent tools disabled", sb.Image())
		return nil
	}
	log.Printf("agentray: agent sandbox enabled (image %q)", sb.Image())
	return sb
}

func buildWorkspace(cfg config.Config) *sandbox.Workspace {
	if strings.TrimSpace(cfg.AgentWorkspaceRoot) == "" {
		return nil
	}
	ws, err := sandbox.NewWorkspace(cfg.AgentWorkspaceRoot)
	if err != nil {
		log.Printf("agentray: AGENTRAY_AGENT_WORKSPACE_ROOT is invalid; file/browser tools disabled: %v", err)
		return nil
	}
	log.Printf("agentray: agent workspace enabled (%q)", ws.Root())
	return ws
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
	tool := httptool.New(
		httptool.WithAllowHosts(hosts),
		httptool.WithAllowPlainHTTP(cfg.HTTPToolAllowHTTP),
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
