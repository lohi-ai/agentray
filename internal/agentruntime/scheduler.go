package agentruntime

import (
	"context"
	"encoding/json"
	"time"

	"github.com/lohi-ai/agentray/internal/cronx"
	"github.com/lohi-ai/agentray/internal/storage"
	"github.com/nats-io/nats.go"
)

// DefaultRunSubject is the NATS subject autonomous runs are published on, kept
// off the HTTP path the same way ingestion decouples via NATS (§8).
const DefaultRunSubject = "agentray.agent.run"

// staleRunDeadline is how long a run may sit in 'running' before the sweeper
// marks it errored. Generously past the detached-run ceiling (10m) so a healthy
// long run is never swept, only one whose process actually died.
const staleRunDeadline = 15 * time.Minute

// MonitorPrompt is the canned task for a scheduled watchdog run.
const MonitorPrompt = `Perform your scheduled check. Inspect recent activity and data quality for anomalies:
ingestion gaps, volume/error spikes, latency or cost drift, and malformed or missing event properties.
Summarize what you find. If a scope you hold permits it, open a recommendation for anything material.`

// runMessage is the NATS payload for one autonomous run. AgentID selects which
// agent runs (empty = the project's default agent); Trigger records what started
// it (empty defaults to "scheduled"), so webhook and schedule producers share one
// payload shape and one consumer.
type runMessage struct {
	ProjectID string `json:"project_id"`
	AgentID   string `json:"agent_id,omitempty"`
	Trigger   string `json:"trigger,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
}

// Scheduler triggers autonomous runs. A ticker scans scheduled projects each
// minute and publishes due ones to NATS; a subscriber consumes them and drives
// a run through the Runner. Splitting publish/consume over NATS keeps a single
// process from blocking on long runs and lets runs scale horizontally later.
type Scheduler struct {
	nc      *nats.Conn
	runner  *Runner
	store   *storage.Store
	subject string
	sub     *nats.Subscription
	stop    chan struct{}
	// onTick, if set, runs each minute alongside the schedule scan (the alert
	// evaluator hooks here so alerting shares the one clock instead of a second
	// timer). Failures are the callback's own concern — the ticker never blocks.
	onTick func(ctx context.Context, now time.Time)
}

// OnTick registers a callback fired every minute with the tick time (UTC). Used
// to drive the alert evaluator off the same clock as scheduled runs.
func (s *Scheduler) OnTick(fn func(ctx context.Context, now time.Time)) {
	s.onTick = fn
}

// NewScheduler wires a scheduler over NATS + storage. RunnerOptions (e.g.
// WithSandbox) are forwarded to the Runner so scheduled runs share the same
// isolation substrate as HTTP-chat runs.
func NewScheduler(nc *nats.Conn, store *storage.Store, runnerOpts ...RunnerOption) *Scheduler {
	return &Scheduler{
		nc:      nc,
		runner:  NewRunner(store, runnerOpts...),
		store:   store,
		subject: DefaultRunSubject,
		stop:    make(chan struct{}),
	}
}

// Start subscribes to the run subject and launches the minute ticker. It is
// non-blocking; call Stop to tear down.
func (s *Scheduler) Start(ctx context.Context) error {
	sub, err := s.nc.Subscribe(s.subject, func(msg *nats.Msg) {
		var m runMessage
		if err := json.Unmarshal(msg.Data, &m); err != nil || m.ProjectID == "" {
			return
		}
		prompt := m.Prompt
		if prompt == "" {
			prompt = MonitorPrompt
		}
		trigger := m.Trigger
		if trigger == "" {
			trigger = "scheduled"
		}
		// Reflect (the self-improvement pass) belongs to the autonomous watchdog,
		// not to an externally-driven webhook; a webhook run is one-shot work.
		reflect := trigger == "scheduled"
		// Each run gets a bounded context independent of the publish path.
		runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		_, _, _ = s.runner.Run(runCtx, RunOptions{
			ProjectID: m.ProjectID, AgentID: m.AgentID, Trigger: trigger, Prompt: prompt, Reflect: reflect,
		})
	})
	if err != nil {
		return err
	}
	s.sub = sub

	go s.tickLoop(ctx)
	return nil
}

// Stop unsubscribes and halts the ticker.
func (s *Scheduler) Stop() {
	close(s.stop)
	if s.sub != nil {
		_ = s.sub.Unsubscribe()
	}
}

// Publish enqueues an immediate scheduled-style run for a project's default
// agent (used by a manual "run now" trigger as well as the ticker).
func (s *Scheduler) Publish(projectID, prompt string) error {
	return s.publishRun(runMessage{ProjectID: projectID, Trigger: "scheduled", Prompt: prompt})
}

// PublishWebhook enqueues a webhook-triggered run for a specific agent (the
// webhook ingress producer). It shares the scheduler's NATS run path, so a
// webhook is just a second producer — no new run engine.
func (s *Scheduler) PublishWebhook(projectID, agentID, prompt string) error {
	return s.publishRun(runMessage{ProjectID: projectID, AgentID: agentID, Trigger: "webhook", Prompt: prompt})
}

// publishRun marshals and publishes one run message.
func (s *Scheduler) publishRun(m runMessage) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.nc.Publish(s.subject, data)
}

func (s *Scheduler) tickLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case now := <-t.C:
			s.publishDue(ctx, now.UTC())
			s.sweepStaleRuns(ctx)
			if s.onTick != nil {
				s.onTick(ctx, now.UTC())
			}
		}
	}
}

// sweepStaleRuns marks runs stuck in 'running' past the deadline as errored, so a
// run whose process died never lingers in the UI. Best-effort: a sweep failure is
// retried on the next tick.
func (s *Scheduler) sweepStaleRuns(ctx context.Context) {
	_, _ = s.store.SweepStaleRuns(ctx, staleRunDeadline)
}

// publishDue publishes a run for every due schedule. Two sources are scanned:
// the legacy project-level schedule (agent_configs.schedule_cron → the default
// agent) and the per-agent agent_triggers (AgentGarden §7), so existing
// scheduled projects keep firing unchanged while non-default agents gain their
// own schedules.
func (s *Scheduler) publishDue(ctx context.Context, now time.Time) {
	if projects, err := s.store.ScheduledAgentProjects(ctx); err == nil {
		for projectID, cron := range projects {
			if cronMatches(cron, now) {
				_ = s.Publish(projectID, MonitorPrompt)
			}
		}
	}
	if triggers, err := s.store.ScheduledAgentTriggers(ctx); err == nil {
		for _, t := range triggers {
			if !cronMatches(t.Cron, now) {
				continue
			}
			prompt := t.PromptTemplate
			if prompt == "" {
				prompt = MonitorPrompt
			}
			_ = s.publishRun(runMessage{ProjectID: t.ProjectID, AgentID: t.AgentID, Trigger: "scheduled", Prompt: prompt})
		}
	}
}

// cronMatches reports whether a 5-field cron expression (minute hour dom month
// dow) matches t. The matcher itself lives in internal/cronx so the connector
// sync engine shares the same semantics.
func cronMatches(expr string, t time.Time) bool {
	return cronx.Matches(expr, t)
}
