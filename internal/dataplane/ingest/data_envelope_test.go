package ingestion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/config"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// This test is deliberately present in the ordinary test binary instead of
// hiding behind a build tag. That lets CI prove it compiles and lets an
// accidental opt-in fail with an actionable approval error rather than report
// "no tests to run". The expensive body remains double gated below.
const (
	envelopeRequiredHostClass  = "lohi-app-frozen-surface"
	envelopeEventsPerProject   = 10_000_000
	envelopeExternalPerProject = 1_000_000
	envelopeProjects           = 3
	envelopeInvestigations     = 4
	envelopeAcceptedPerSecond  = 100
	envelopeSteadyDuration     = 15 * time.Minute
	envelopeStagingProbeRows   = 500
)

type envelopeThresholds struct {
	WarmP95        time.Duration `json:"warm_p95_max"`
	ColdP95        time.Duration `json:"cold_p95_max"`
	PublicationP95 time.Duration `json:"publication_p95_max"`
	QueryableP95   time.Duration `json:"queryable_p95_max"`
	MaxRSSFraction float64       `json:"max_rss_fraction"`
	MaxRSSGrowth   float64       `json:"max_rss_growth_fraction"`
}

var approvedEnvelopeThresholds = envelopeThresholds{
	WarmP95: 5 * time.Second, ColdP95: 30 * time.Second,
	PublicationP95: time.Second, QueryableP95: 60 * time.Second,
	// The latency targets are parent-approved constants. These two resource
	// guards make "no OOM / no unbounded RSS" assertion-bearing: the process
	// must remain below 90% of detected RAM and its steady-state tail may not
	// climb by more than 10% of RAM over its first steady-state quartile.
	MaxRSSFraction: .90, MaxRSSGrowth: .10,
}

type envelopeGate struct {
	Enabled   bool
	Approved  bool
	HostClass string
	Report    string
}

func readEnvelopeGate(getenv func(string) string) (envelopeGate, error) {
	g := envelopeGate{
		Enabled:   strings.TrimSpace(getenv("AGENTRAY_DATA_ENVELOPE")) == "1",
		Approved:  strings.TrimSpace(getenv("AGENTRAY_DATA_ENVELOPE_APPROVED")) == "1",
		HostClass: strings.TrimSpace(getenv("AGENTRAY_DATA_ENVELOPE_HOST_CLASS")),
		Report:    strings.TrimSpace(getenv("AGENTRAY_DATA_ENVELOPE_REPORT")),
	}
	if !g.Enabled {
		return g, nil
	}
	if !g.Approved || g.HostClass != envelopeRequiredHostClass {
		return g, fmt.Errorf("AC-DATA-03 envelope refused: required approved host class %q; set AGENTRAY_DATA_ENVELOPE_APPROVED=1 and AGENTRAY_DATA_ENVELOPE_HOST_CLASS=%s only after the foreman leases that host", envelopeRequiredHostClass, envelopeRequiredHostClass)
	}
	if g.Report == "" || !filepath.IsAbs(g.Report) {
		return g, errors.New("AC-DATA-03 envelope refused: AGENTRAY_DATA_ENVELOPE_REPORT must be an absolute path outside the source tree")
	}
	return g, nil
}

type envelopeHardware struct {
	OS               string `json:"os"`
	Arch             string `json:"arch"`
	CPUs             int    `json:"cpus"`
	CPUModel         string `json:"cpu_model"`
	RAMBytes         uint64 `json:"ram_bytes"`
	CgroupLimitBytes uint64 `json:"cgroup_limit_bytes,omitempty"`
	Filesystem       string `json:"filesystem"`
	DiskTotalBytes   uint64 `json:"disk_total_bytes"`
	DiskFreeBytes    uint64 `json:"disk_free_bytes"`
}

type envelopeSample struct {
	Phase     string        `json:"phase"`
	Tenant    string        `json:"tenant,omitempty"`
	Recipe    string        `json:"recipe,omitempty"`
	StartedAt time.Time     `json:"started_at"`
	Duration  time.Duration `json:"duration"`
	Value     int64         `json:"value,omitempty"`
}

type envelopeResourceSample struct {
	Phase       string    `json:"phase"`
	At          time.Time `json:"at"`
	RSSBytes    uint64    `json:"rss_bytes"`
	DuckDBBytes uint64    `json:"duckdb_bytes"`
	WALBytes    uint64    `json:"wal_bytes"`
	SpillBytes  uint64    `json:"spill_bytes"`
	BrokerBytes uint64    `json:"broker_bytes"`
	DiskFree    uint64    `json:"disk_free_bytes"`
}

type envelopeTenantResult struct {
	ProjectID           string        `json:"project_id"`
	SeedEvents          int64         `json:"seed_events"`
	SeedExternalRows    int64         `json:"seed_external_rows"`
	AcceptedLiveEvents  int64         `json:"accepted_live_events"`
	ObservedEvents      int64         `json:"observed_events"`
	ObservedExternal    int64         `json:"observed_external_rows"`
	ObservedEventSum    int64         `json:"observed_event_seed_sum"`
	ObservedExternalSum int64         `json:"observed_external_amount_sum"`
	SeedDuration        time.Duration `json:"seed_duration"`
}

type envelopeRetention struct {
	EventRetentionDays       int           `json:"event_retention_days"`
	StagingTTL               time.Duration `json:"staging_ttl"`
	ProbeRows                int           `json:"probe_rows"`
	ProbeLogicalBytes        int64         `json:"probe_logical_bytes"`
	StagingDiskGrowthBytes   int64         `json:"staging_disk_growth_bytes"`
	PostPromotionGrowthBytes int64         `json:"post_promotion_growth_bytes"`
	CleanupVerified          bool          `json:"cleanup_verified"`
}

type envelopeReport struct {
	SchemaVersion        int                      `json:"schema_version"`
	Status               string                   `json:"status"`
	Failure              string                   `json:"failure,omitempty"`
	StartedAt            time.Time                `json:"started_at"`
	FinishedAt           time.Time                `json:"finished_at"`
	HostClass            string                   `json:"host_class"`
	Hardware             envelopeHardware         `json:"hardware"`
	Config               map[string]any           `json:"config"`
	Thresholds           envelopeThresholds       `json:"thresholds"`
	Corpus               map[string]any           `json:"corpus"`
	Tenants              []envelopeTenantResult   `json:"tenants"`
	PhaseSamples         []envelopeSample         `json:"phase_samples"`
	ColdSamples          []time.Duration          `json:"cold_samples"`
	WarmSamples          []time.Duration          `json:"warm_samples"`
	InvestigationSamples []envelopeSample         `json:"investigation_samples"`
	PublicationSamples   []envelopeSample         `json:"publication_samples"`
	QueryableSamples     []envelopeSample         `json:"queryable_samples"`
	ResourceSamples      []envelopeResourceSample `json:"resource_samples"`
	Retention            envelopeRetention        `json:"retention_and_staging"`
	OOMEventsBefore      uint64                   `json:"oom_events_before"`
	OOMEventsAfter       uint64                   `json:"oom_events_after"`
	RefusedQueries       int                      `json:"refused_queries"`
	QueryErrors          []string                 `json:"query_errors,omitempty"`
	Assertions           []string                 `json:"assertions"`
	Summary              map[string]any           `json:"summary,omitempty"`
}

type envelopeObserved struct {
	Warm, Cold, Publication, Queryable []time.Duration
	Resources                          []envelopeResourceSample
	RAMBytes                           uint64
	OOMBefore, OOMAfter                uint64
	NoCrossProjectInterference         bool
	NoDataLoss                         bool
	QueryErrors                        int
	RefusedQueries                     int
	AcceptedEvents                     int64
	ExpectedAccepted                   int64
	QueryableEvents                    int64
}

func p95(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	cpy := slices.Clone(samples)
	slices.Sort(cpy)
	idx := int(math.Ceil(float64(len(cpy))*.95)) - 1
	if idx < 0 {
		idx = 0
	}
	return cpy[idx]
}

func validateEnvelope(o envelopeObserved, limits envelopeThresholds) error {
	var failures []string
	checks := []struct {
		name string
		got  time.Duration
		max  time.Duration
	}{
		{"warm_p95", p95(o.Warm), limits.WarmP95},
		{"cold_p95", p95(o.Cold), limits.ColdP95},
		{"accepted_ingest_publication_p95", p95(o.Publication), limits.PublicationP95},
		{"queryable_p95", p95(o.Queryable), limits.QueryableP95},
	}
	for _, check := range checks {
		var count int
		switch check.name {
		case "warm_p95":
			count = len(o.Warm)
		case "cold_p95":
			count = len(o.Cold)
		case "accepted_ingest_publication_p95":
			count = len(o.Publication)
		case "queryable_p95":
			count = len(o.Queryable)
		}
		if count == 0 {
			failures = append(failures, check.name+" has no samples")
		} else if check.got > check.max {
			failures = append(failures, fmt.Sprintf("%s=%s exceeds %s", check.name, check.got, check.max))
		}
	}
	if !o.NoCrossProjectInterference {
		failures = append(failures, "cross_project_interference detected")
	}
	if !o.NoDataLoss {
		failures = append(failures, "data_loss detected")
	}
	if o.QueryErrors != 0 || o.RefusedQueries != 0 {
		failures = append(failures, fmt.Sprintf("investigations had query_errors=%d refused=%d", o.QueryErrors, o.RefusedQueries))
	}
	if o.AcceptedEvents < o.ExpectedAccepted {
		failures = append(failures, fmt.Sprintf("accepted ingest rate produced %d events, want at least %d", o.AcceptedEvents, o.ExpectedAccepted))
	}
	if o.QueryableEvents != o.AcceptedEvents {
		failures = append(failures, fmt.Sprintf("queryable sample coverage=%d, want one sample for each of %d accepted events", o.QueryableEvents, o.AcceptedEvents))
	}
	if o.OOMAfter > o.OOMBefore {
		failures = append(failures, fmt.Sprintf("cgroup OOM events increased from %d to %d", o.OOMBefore, o.OOMAfter))
	}
	steadyResources := make([]envelopeResourceSample, 0, len(o.Resources))
	for _, sample := range o.Resources {
		if sample.Phase == "steady" {
			steadyResources = append(steadyResources, sample)
		}
	}
	if len(steadyResources) == 0 {
		steadyResources = o.Resources // pure assertion tests use phase-less samples
	}
	if len(steadyResources) < 4 || o.RAMBytes == 0 {
		failures = append(failures, "RSS growth assertion lacks resource samples or detected RAM")
	} else {
		var maxRSS uint64
		for _, sample := range steadyResources {
			maxRSS = max(maxRSS, sample.RSSBytes)
		}
		if maxRSS == 0 {
			failures = append(failures, "RSS sampling returned zero")
		} else if float64(maxRSS) > float64(o.RAMBytes)*limits.MaxRSSFraction {
			failures = append(failures, fmt.Sprintf("rss_high_water=%d exceeds %.0f%% of RAM", maxRSS, limits.MaxRSSFraction*100))
		}
		quarter := max(1, len(steadyResources)/4)
		first, last := medianRSS(steadyResources[:quarter]), medianRSS(steadyResources[len(steadyResources)-quarter:])
		if last > first && float64(last-first) > float64(o.RAMBytes)*limits.MaxRSSGrowth {
			failures = append(failures, fmt.Sprintf("unbounded_rss_growth=%d exceeds %.0f%% of RAM", last-first, limits.MaxRSSGrowth*100))
		}
	}
	return errors.Join(stringErrors(failures)...)
}

func stringErrors(in []string) []error {
	out := make([]error, len(in))
	for i := range in {
		out[i] = errors.New(in[i])
	}
	return out
}

func medianRSS(in []envelopeResourceSample) uint64 {
	values := make([]uint64, len(in))
	for i := range in {
		values[i] = in[i].RSSBytes
	}
	slices.Sort(values)
	return values[len(values)/2]
}

func TestDataEnvelopeGateRequiresApprovedHost(t *testing.T) {
	env := map[string]string{"AGENTRAY_DATA_ENVELOPE": "1"}
	_, err := readEnvelopeGate(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), envelopeRequiredHostClass) {
		t.Fatalf("missing approval error = %v, want required host class", err)
	}
	if gate, err := readEnvelopeGate(func(string) string { return "" }); err != nil || gate.Enabled {
		t.Fatalf("disabled gate = %+v err=%v", gate, err)
	}
	env["AGENTRAY_DATA_ENVELOPE_APPROVED"] = "1"
	env["AGENTRAY_DATA_ENVELOPE_HOST_CLASS"] = envelopeRequiredHostClass
	if _, err := readEnvelopeGate(func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "REPORT") {
		t.Fatalf("missing report path error = %v", err)
	}
	env["AGENTRAY_DATA_ENVELOPE_REPORT"] = filepath.Join(t.TempDir(), "report.json")
	if gate, err := readEnvelopeGate(func(k string) string { return env[k] }); err != nil || !gate.Approved {
		t.Fatalf("approved gate = %+v err=%v", gate, err)
	}
}

func TestDataEnvelopeAssertionsRejectNegativeControl(t *testing.T) {
	limits := approvedEnvelopeThresholds
	bad := envelopeObserved{
		Warm: []time.Duration{limits.WarmP95 + time.Millisecond}, Cold: []time.Duration{limits.ColdP95 + time.Millisecond},
		Publication: []time.Duration{limits.PublicationP95 + time.Millisecond}, Queryable: []time.Duration{limits.QueryableP95 + time.Millisecond},
		Resources: []envelopeResourceSample{{RSSBytes: 10}, {RSSBytes: 20}, {RSSBytes: 30}, {RSSBytes: 95}}, RAMBytes: 100,
		OOMBefore: 1, OOMAfter: 2, QueryErrors: 1, RefusedQueries: 1, AcceptedEvents: 9, ExpectedAccepted: 10, QueryableEvents: 1,
	}
	err := validateEnvelope(bad, limits)
	if err == nil {
		t.Fatal("negative control unexpectedly passed")
	}
	for _, want := range []string{"warm_p95", "cold_p95", "accepted_ingest_publication_p95", "queryable_p95", "cross_project_interference", "data_loss", "OOM", "rss_high_water", "query_errors", "accepted ingest rate", "queryable sample coverage"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("negative control error %q missing %q", err, want)
		}
	}
}

// TestDataEnvelope is AC-DATA-03's assertion-bearing capacity run. It is
// intentionally skipped unless explicitly enabled, but once enabled every
// absent prerequisite and exceeded threshold is a test failure, never a skip.
func TestDataEnvelope(t *testing.T) {
	gate, err := readEnvelopeGate(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if !gate.Enabled {
		t.Skip("set AGENTRAY_DATA_ENVELOPE=1 on the approved capacity host to run AC-DATA-03")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 88*time.Minute)
	defer cancel()
	root := t.TempDir()
	report := &envelopeReport{
		SchemaVersion: 1, Status: "running", StartedAt: time.Now().UTC(), HostClass: gate.HostClass,
		Thresholds: approvedEnvelopeThresholds,
		Corpus: map[string]any{
			"projects": envelopeProjects, "events_per_project": envelopeEventsPerProject,
			"external_rows_per_project": envelopeExternalPerProject, "investigations": envelopeInvestigations,
			"accepted_events_per_second": envelopeAcceptedPerSecond, "steady_duration": envelopeSteadyDuration.String(),
			"seed_sha256": envelopeSeedHash(), "generator": "data-envelope-v1",
		},
		Assertions: []string{"warm_p95", "cold_p95", "accepted_ingest_publication_p95", "queryable_p95", "no_oom", "bounded_rss_growth", "no_cross_project_interference", "no_data_loss", "four_investigations_without_refusal"},
	}
	defer func() {
		if report.Status == "running" {
			report.Status = "failed"
			if report.Failure == "" {
				report.Failure = "test aborted before all envelope assertions completed"
			}
		}
		report.FinishedAt = time.Now().UTC()
		if err := writeEnvelopeReport(gate.Report, report); err != nil {
			t.Errorf("write envelope report: %v", err)
		} else {
			t.Logf("AC-DATA-03 report: %s", gate.Report)
		}
	}()

	report.Hardware, err = inspectEnvelopeHardware(root)
	if err != nil {
		report.Failure = err.Error()
		t.Fatal(err)
	}
	if report.Hardware.CgroupLimitBytes > 0 && report.Hardware.CgroupLimitBytes < report.Hardware.RAMBytes {
		report.Hardware.RAMBytes = report.Hardware.CgroupLimitBytes
	}
	if runtime.GOOS != "linux" {
		report.Failure = fmt.Sprintf("AC-DATA-03 envelope refused: required host class %q is Linux; detected %s", envelopeRequiredHostClass, runtime.GOOS)
		t.Fatal(report.Failure)
	}
	report.OOMEventsBefore = cgroupOOMEvents()

	pgURL, cleanupPG, err := startEnvelopePostgres(ctx)
	if err != nil {
		report.Failure = err.Error()
		t.Fatalf("disposable PostgreSQL: %v", err)
	}
	defer func() {
		if err := cleanupPG(); err != nil {
			report.Status, report.Failure = "failed", "remove disposable PostgreSQL: "+err.Error()
			t.Errorf("remove disposable PostgreSQL: %v", err)
		}
	}()

	cfg := config.FromEnv()
	cfg.PostgresURL = pgURL
	cfg.DuckDBPath = filepath.Join(root, "analytics.duckdb")
	cfg.DefaultProjectName = "data-envelope"
	cfg.DefaultProjectAPIKey = "data_envelope_disposable"
	report.Config = map[string]any{
		"data_disk_reserve_bytes":  cfg.DataDiskReserveBytes,
		"event_retention_days":     cfg.EventRetentionDays,
		"source_staging_ttl":       cfg.SourceStagingTTL.String(),
		"source_freshness_max_age": cfg.SourceFreshnessMaxAge.String(),
		"ingest_stream_max_bytes":  cfg.IngestStreamMaxBytes,
		"ingest_max_deliver":       cfg.IngestMaxDeliver,
	}
	store, err := storage.Open(ctx, cfg)
	if err != nil {
		report.Failure = err.Error()
		t.Fatal(err)
	}
	defer store.Close()

	projects := make([]storage.Project, envelopeProjects)
	report.Tenants = make([]envelopeTenantResult, envelopeProjects)
	for i := range projects {
		projects[i], err = store.CreateProject(ctx, fmt.Sprintf("envelope-%d", i+1))
		if err != nil {
			report.Failure = err.Error()
			t.Fatal(err)
		}
		report.Tenants[i].ProjectID = projects[i].ID
	}

	brokerDir := filepath.Join(root, "jetstream")
	brokerURL, stopBroker, err := startEnvelopeBroker(brokerDir)
	if err != nil {
		report.Failure = err.Error()
		t.Fatal(err)
	}
	defer stopBroker()
	nc, err := nats.Connect(brokerURL, nats.Name("data-envelope"))
	if err != nil {
		report.Failure = err.Error()
		t.Fatal(err)
	}
	defer nc.Close()
	ingestCfg := testConfig("data-envelope")
	ingestCfg.DuckDBPath = cfg.DuckDBPath
	ingestCfg.IngestStreamMaxBytes = cfg.IngestStreamMaxBytes
	ingestCfg.IngestMaxDeliver = cfg.IngestMaxDeliver
	report.Config["ingest_stream_max_bytes"] = ingestCfg.IngestStreamMaxBytes
	report.Config["ingest_max_deliver"] = ingestCfg.IngestMaxDeliver
	streams, err := EnsureStreams(ctx, nc, ingestCfg)
	if err != nil {
		report.Failure = err.Error()
		t.Fatal(err)
	}
	worker, err := StartJetStreamWorker(ctx, streams, store, nil)
	if err != nil {
		report.Failure = err.Error()
		t.Fatal(err)
	}
	defer func() {
		if worker != nil {
			_ = worker.Stop()
		}
	}()
	queue := NewJetStreamQueue(streams.JS, streams.Subject, streams.ConnectorSubject)

	stopSampling := make(chan struct{})
	var sampleWG sync.WaitGroup
	var stopSamplingOnce sync.Once
	var reportMu sync.Mutex
	samplePhase := "seed"
	sampleWG.Add(1)
	go func() {
		defer sampleWG.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopSampling:
				return
			case at := <-ticker.C:
				reportMu.Lock()
				sample := resourceEnvelopeSample(at, cfg.DuckDBPath, brokerDir)
				sample.Phase = samplePhase
				report.ResourceSamples = append(report.ResourceSamples, sample)
				reportMu.Unlock()
			}
		}
	}()
	stopSampler := func() { stopSamplingOnce.Do(func() { close(stopSampling); sampleWG.Wait() }) }
	defer stopSampler()

	for i, project := range projects {
		started := time.Now()
		if err := seedEnvelopeTenant(ctx, store, project.ID); err != nil {
			report.Failure = fmt.Sprintf("seed tenant %d: %v", i+1, err)
			t.Fatal(report.Failure)
		}
		report.Tenants[i].SeedEvents = envelopeEventsPerProject
		report.Tenants[i].SeedExternalRows = envelopeExternalPerProject
		report.Tenants[i].SeedDuration = time.Since(started)
		report.PhaseSamples = append(report.PhaseSamples, envelopeSample{Phase: "seed", Tenant: project.ID, StartedAt: started.UTC(), Duration: time.Since(started)})
	}

	report.Retention = runStagingProbe(ctx, t, store, cfg, projects[0].ID, cfg.DuckDBPath)
	if !report.Retention.CleanupVerified {
		report.Failure = "snapshot staging promotion did not clean its disposable staging rows"
		t.Fatal(report.Failure)
	}

	recipes := envelopeRecipes()
	reportMu.Lock()
	samplePhase = "cold_warm"
	reportMu.Unlock()
	for _, project := range projects {
		for recipeIndex, recipe := range recipes {
			started := time.Now()
			if _, _, err := store.RunSQLWithMeta(ctx, project.ID, recipe.SQL); err != nil {
				report.Failure = fmt.Sprintf("cold query %s/%s: %v", project.ID, recipe.Name, err)
				t.Fatal(report.Failure)
			}
			d := time.Since(started)
			if recipeIndex == 0 {
				report.ColdSamples = append(report.ColdSamples, d)
			} else {
				report.WarmSamples = append(report.WarmSamples, d)
			}
			report.PhaseSamples = append(report.PhaseSamples, envelopeSample{Phase: map[bool]string{true: "cold", false: "warm"}[recipeIndex == 0], Tenant: project.ID, Recipe: recipe.Name, StartedAt: started.UTC(), Duration: d})
		}
	}

	steadyCtx, stopSteady := context.WithTimeout(ctx, envelopeSteadyDuration)
	reportMu.Lock()
	samplePhase = "steady"
	reportMu.Unlock()
	accepted := newEnvelopeAcceptances(envelopeProjects)
	var liveWG sync.WaitGroup
	for publisher := 0; publisher < 4; publisher++ {
		liveWG.Add(1)
		go func(workerID int) {
			defer liveWG.Done()
			publishEnvelopeEvents(steadyCtx, queue, projects, workerID, accepted, report, &reportMu)
		}(publisher)
	}
	for investigation := 0; investigation < envelopeInvestigations; investigation++ {
		liveWG.Add(1)
		go func(workerID int) {
			defer liveWG.Done()
			runEnvelopeInvestigation(steadyCtx, store, projects, recipes[workerID], workerID, accepted, report, &reportMu)
		}(investigation)
	}
	<-steadyCtx.Done()
	stopSteady()
	liveWG.Wait()

	catchupCtx, catchupCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer catchupCancel()
	for !accepted.complete() && catchupCtx.Err() == nil {
		for i, project := range projects {
			rows, _, queryErr := store.RunSQLWithMeta(catchupCtx, project.ID, recipes[3].SQL)
			if queryErr == nil {
				accepted.observe(i, project.ID, numericCell(rows, "max_seq"), time.Now(), report, &reportMu)
			}
		}
	}
	if err := waitEnvelopeCaughtUp(ctx, streams, 2*time.Minute); err != nil {
		report.Failure = err.Error()
		t.Fatal(err)
	}
	if err := worker.Stop(); err != nil {
		report.Failure = err.Error()
		t.Fatal(err)
	}
	worker = nil
	reportMu.Lock()
	samplePhase = "verification"
	reportMu.Unlock()

	noLoss, isolated, err := verifyEnvelopeTotals(ctx, store, projects, accepted, report)
	if err != nil {
		report.Failure = err.Error()
		t.Fatal(err)
	}
	report.OOMEventsAfter = cgroupOOMEvents()
	reportMu.Lock()
	finalResource := resourceEnvelopeSample(time.Now(), cfg.DuckDBPath, brokerDir)
	finalResource.Phase = samplePhase
	report.ResourceSamples = append(report.ResourceSamples, finalResource)
	reportMu.Unlock()
	stopSampler()
	observed := envelopeObserved{
		Warm: report.WarmSamples, Cold: report.ColdSamples, Publication: envelopeDurations(report.PublicationSamples),
		Queryable: envelopeDurations(report.QueryableSamples), Resources: report.ResourceSamples, RAMBytes: report.Hardware.RAMBytes,
		OOMBefore: report.OOMEventsBefore, OOMAfter: report.OOMEventsAfter,
		NoCrossProjectInterference: isolated, NoDataLoss: noLoss, QueryErrors: len(report.QueryErrors),
		RefusedQueries: report.RefusedQueries, AcceptedEvents: accepted.total(),
		ExpectedAccepted: int64(envelopeAcceptedPerSecond) * int64(envelopeSteadyDuration/time.Second) * 99 / 100,
		QueryableEvents:  int64(len(report.QueryableSamples)),
	}
	report.Summary = map[string]any{
		"warm_p95": p95(observed.Warm), "cold_p95": p95(observed.Cold),
		"publication_p95": p95(observed.Publication), "queryable_p95": p95(observed.Queryable),
		"accepted_events":            observed.AcceptedEvents,
		"accepted_events_per_second": float64(observed.AcceptedEvents) / envelopeSteadyDuration.Seconds(),
		"queryable_samples":          observed.QueryableEvents,
	}
	if err := validateEnvelope(observed, approvedEnvelopeThresholds); err != nil {
		report.Status, report.Failure = "failed", err.Error()
		t.Fatal(err)
	}
	report.Status = "passed"
}

func envelopeDurations(samples []envelopeSample) []time.Duration {
	out := make([]time.Duration, len(samples))
	for i := range samples {
		out[i] = samples[i].Duration
	}
	return out
}

type envelopeRecipe struct{ Name, SQL string }

func envelopeRecipes() []envelopeRecipe {
	return []envelopeRecipe{
		{"62_day_activity", `SELECT date_trunc('day', "timestamp") AS day, count(*) AS events FROM events WHERE "timestamp" >= current_timestamp - INTERVAL '62 days' GROUP BY day ORDER BY day`},
		{"62_day_purchases", `SELECT date_trunc('day', "timestamp") AS day, count(*) AS purchases FROM events WHERE event_name='purchase' AND "timestamp" >= current_timestamp - INTERVAL '62 days' GROUP BY day ORDER BY day`},
		{"lifetime_first_payer", `SELECT min(first_paid_at) AS first_paid_at FROM (SELECT canonical_id, min("timestamp") AS first_paid_at FROM events WHERE event_name='purchase' GROUP BY canonical_id) payers`},
		{"queryable_watermark", `SELECT coalesce(max(CAST(json_extract_string(properties, '$.envelope_seq') AS BIGINT)), 0) AS max_seq FROM events WHERE event_name='envelope.live'`},
	}
}

func envelopeSeedHash() string {
	s := fmt.Sprintf("v1/projects=%d/events=%d/external=%d/staging=%d", envelopeProjects, envelopeEventsPerProject, envelopeExternalPerProject, envelopeStagingProbeRows)
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func seedEnvelopeTenant(ctx context.Context, store *storage.Store, projectID string) error {
	namespace := uuid.MustParse(projectID)
	now := time.Now().UTC()
	for start := 0; start < envelopeEventsPerProject; start += 1_000 {
		end := min(start+1_000, envelopeEventsPerProject)
		batch := make([]storage.Event, 0, end-start)
		for i := start; i < end; i++ {
			at := now.Add(-time.Duration(i%int((90*24*time.Hour)/time.Second)) * time.Second)
			name := "product.viewed"
			if i%10 == 0 {
				name = "purchase"
			}
			batch = append(batch, storage.Event{
				ProjectID: projectID, EventID: uuid.NewSHA1(namespace, []byte(strconv.Itoa(i))).String(),
				DistinctID: fmt.Sprintf("user-%06d", i%200_000), SessionID: fmt.Sprintf("session-%08d", i/5),
				EventName: name, EventType: "user", Timestamp: at, Platform: []string{"web", "ios", "android"}[i%3],
				Properties: fmt.Sprintf(`{"amount":%d,"plan":"pro","campaign":"capacity","seed_index":%d,"seed_project":"%s"}`, 100+i%9900, i, projectID),
			})
		}
		if err := store.SinkEvents(ctx, batch, storage.AppliedMark{}); err != nil {
			return err
		}
	}
	connectorID := uuid.NewSHA1(namespace, []byte("external-connector")).String()
	for start := 0; start < envelopeExternalPerProject; start += 500 {
		end := min(start+500, envelopeExternalPerProject)
		rows := make([]connector.LandedRow, 0, end-start)
		for i := start; i < end; i++ {
			rows = append(rows, connector.LandedRow{Key: strconv.Itoa(i), Cursor: strconv.Itoa(i), DataJSON: fmt.Sprintf(`{"order_id":%d,"amount":%d,"status":"paid","source":"capacity","seed_project":"%s"}`, i, 100+i%9900, projectID)})
		}
		if err := store.InsertExternalRows(ctx, projectID, connectorID, "orders", rows, storage.AppliedMark{}); err != nil {
			return err
		}
	}
	return nil
}

func runStagingProbe(ctx context.Context, t *testing.T, store *storage.Store, cfg config.Config, projectID, duckPath string) envelopeRetention {
	t.Helper()
	namespace := uuid.MustParse(projectID)
	connectorID, syncID := uuid.NewSHA1(namespace, []byte("staging-connector")).String(), uuid.NewSHA1(namespace, []byte("staging-sync")).String()
	generation, runID := uuid.NewSHA1(namespace, []byte("staging-generation")).String(), uuid.NewSHA1(namespace, []byte("staging-run")).String()
	started := time.Now().UTC().Add(-time.Minute)
	rows := make([]connector.SnapshotRow, envelopeStagingProbeRows)
	var logical int64
	for i := range rows {
		rows[i] = connector.SnapshotRow{Key: strconv.Itoa(i), Data: json.RawMessage(fmt.Sprintf(`{"probe":%d,"payload":"staging-capacity"}`, i))}
		logical += int64(len(rows[i].Key) + len(rows[i].Data))
	}
	digest, err := connector.SnapshotPayloadDigest(rows)
	if err != nil {
		t.Fatal(err)
	}
	before := fileSize(duckPath)
	batch := connector.SnapshotEnvelope{Protocol: connector.SnapshotProtocolV1, ProjectID: projectID, ConnectorID: connectorID, Table: "envelope_staging_probe", SyncID: syncID, RunID: runID, Generation: generation, GenerationSeq: 1, BindingDigest: strings.Repeat("a", 64), Kind: connector.SnapshotKindBatch, BatchID: "probe-0", BatchIndex: 0, PayloadSHA256: digest, CaptureStartedAt: started, Rows: rows}
	if _, err := store.ApplySnapshotEnvelope(ctx, batch, storage.AppliedMark{}); err != nil {
		t.Fatal(err)
	}
	staged := fileSize(duckPath)
	manifest, err := connector.SnapshotManifestDigest([]connector.SnapshotManifestEntry{{Index: 0, BatchID: batch.BatchID, PayloadSHA256: digest, RowCount: len(rows)}})
	if err != nil {
		t.Fatal(err)
	}
	finished := time.Now().UTC()
	complete := batch
	complete.Kind, complete.BatchID, complete.BatchIndex, complete.PayloadSHA256, complete.Rows = connector.SnapshotKindComplete, "", 0, "", nil
	complete.ExpectedBatches, complete.ExpectedRows, complete.BatchManifestSHA256, complete.CaptureFinishedAt = 1, int64(len(rows)), manifest, &finished
	promotion, err := store.ApplySnapshotEnvelope(ctx, complete, storage.AppliedMark{})
	if err != nil {
		t.Fatal(err)
	}
	after := fileSize(duckPath)
	return envelopeRetention{EventRetentionDays: cfg.EventRetentionDays, StagingTTL: cfg.SourceStagingTTL,
		ProbeRows: len(rows), ProbeLogicalBytes: logical, StagingDiskGrowthBytes: staged - before,
		PostPromotionGrowthBytes: after - before, CleanupVerified: promotion != nil && promotion.ExpectedRows == int64(len(rows))}
}

type envelopeAcceptance struct {
	Seq int64
	At  time.Time
}
type envelopeAcceptances struct {
	mu             sync.Mutex
	next, observed []int64
	lastAccepted   []int64
	successes      []int64
	pending        [][]envelopeAcceptance
}

func newEnvelopeAcceptances(projects int) *envelopeAcceptances {
	return &envelopeAcceptances{next: make([]int64, projects), observed: make([]int64, projects), lastAccepted: make([]int64, projects), successes: make([]int64, projects), pending: make([][]envelopeAcceptance, projects)}
}

func (a *envelopeAcceptances) reserve(project int) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.next[project]++
	return a.next[project]
}
func (a *envelopeAcceptances) accepted(project int, seq int64, at time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.successes[project]++
	a.lastAccepted[project] = max(a.lastAccepted[project], seq)
	if seq <= a.observed[project] {
		return true
	}
	a.pending[project] = append(a.pending[project], envelopeAcceptance{Seq: seq, At: at})
	return false
}
func (a *envelopeAcceptances) observe(project int, tenant string, seq int64, at time.Time, report *envelopeReport, reportMu *sync.Mutex) {
	a.mu.Lock()
	if seq <= a.observed[project] {
		a.mu.Unlock()
		return
	}
	a.observed[project] = seq
	var samples []envelopeSample
	cut := 0
	for cut < len(a.pending[project]) && a.pending[project][cut].Seq <= seq {
		samples = append(samples, envelopeSample{Phase: "queryable", Tenant: tenant, StartedAt: a.pending[project][cut].At.UTC(), Duration: at.Sub(a.pending[project][cut].At), Value: a.pending[project][cut].Seq})
		cut++
	}
	a.pending[project] = append([]envelopeAcceptance(nil), a.pending[project][cut:]...)
	a.mu.Unlock()
	reportMu.Lock()
	report.QueryableSamples = append(report.QueryableSamples, samples...)
	reportMu.Unlock()
}
func (a *envelopeAcceptances) complete() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.next {
		if a.observed[i] < a.lastAccepted[i] {
			return false
		}
	}
	return true
}
func (a *envelopeAcceptances) total() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	var n int64
	for _, v := range a.successes {
		n += v
	}
	return n
}
func (a *envelopeAcceptances) counts() []int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.successes)
}

func publishEnvelopeEvents(ctx context.Context, queue EventQueue, projects []storage.Project, workerID int, accepted *envelopeAcceptances, report *envelopeReport, reportMu *sync.Mutex) {
	ticker := time.NewTicker(40 * time.Millisecond) // four publishers = 100 accepted events/s
	defer ticker.Stop()
	index := workerID
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-ticker.C:
			projectIndex := index % len(projects)
			index += 4
			seq := accepted.reserve(projectIndex)
			event := storage.Event{ProjectID: projects[projectIndex].ID, EventID: uuid.NewString(), DistinctID: fmt.Sprintf("live-%d-%d", workerID, seq%10_000), SessionID: fmt.Sprintf("live-session-%d", seq/5), EventName: "envelope.live", EventType: "user", Timestamp: at.UTC(), Platform: "web", Properties: fmt.Sprintf(`{"envelope_seq":%d,"publisher":%d}`, seq, workerID)}
			started := time.Now()
			err := queue.InsertEvents(ctx, []storage.Event{event})
			d := time.Since(started)
			reportMu.Lock()
			report.PublicationSamples = append(report.PublicationSamples, envelopeSample{Phase: "publication", Tenant: projects[projectIndex].ID, StartedAt: started.UTC(), Duration: d, Value: seq})
			if err != nil {
				report.QueryErrors = append(report.QueryErrors, "publish: "+err.Error())
			}
			reportMu.Unlock()
			if err == nil {
				acceptedAt := time.Now()
				if accepted.accepted(projectIndex, seq, acceptedAt) {
					reportMu.Lock()
					report.QueryableSamples = append(report.QueryableSamples, envelopeSample{Phase: "queryable", Tenant: projects[projectIndex].ID, StartedAt: acceptedAt.UTC(), Duration: 0, Value: seq})
					reportMu.Unlock()
				}
			}
		}
	}
}

func runEnvelopeInvestigation(ctx context.Context, store *storage.Store, projects []storage.Project, recipe envelopeRecipe, workerID int, accepted *envelopeAcceptances, report *envelopeReport, reportMu *sync.Mutex) {
	iteration := 0
	for ctx.Err() == nil {
		projectIndex := (workerID + iteration) % len(projects)
		iteration++
		started := time.Now()
		rows, _, err := store.RunSQLWithMeta(ctx, projects[projectIndex].ID, recipe.SQL)
		d := time.Since(started)
		reportMu.Lock()
		report.InvestigationSamples = append(report.InvestigationSamples, envelopeSample{Phase: "steady", Tenant: projects[projectIndex].ID, Recipe: recipe.Name, StartedAt: started.UTC(), Duration: d})
		if err != nil && ctx.Err() == nil {
			report.QueryErrors = append(report.QueryErrors, recipe.Name+": "+err.Error())
			if strings.Contains(strings.ToLower(err.Error()), "busy") || strings.Contains(strings.ToLower(err.Error()), "capacity") {
				report.RefusedQueries++
			}
		}
		reportMu.Unlock()
		if err == nil && recipe.Name == "queryable_watermark" {
			accepted.observe(projectIndex, projects[projectIndex].ID, numericCell(rows, "max_seq"), time.Now(), report, reportMu)
		}
	}
}

func numericCell(rows []map[string]any, key string) int64 {
	if len(rows) == 0 {
		return 0
	}
	switch v := rows[0][key].(type) {
	case int64:
		return v
	case int32:
		return int64(v)
	case uint64:
		return int64(v)
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	default:
		return 0
	}
}

func waitEnvelopeCaughtUp(ctx context.Context, streams *StreamSet, within time.Duration) error {
	deadline := time.Now().Add(within)
	var last ReplayVerdict
	for time.Now().Before(deadline) {
		verdict, err := streams.ReplayStatus(ctx)
		if err == nil {
			last = verdict
			if verdict.Ready {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("ingest did not catch up within %s: %+v", within, last)
}

func verifyEnvelopeTotals(ctx context.Context, store *storage.Store, projects []storage.Project, accepted *envelopeAcceptances, report *envelopeReport) (bool, bool, error) {
	counts := accepted.counts()
	noLoss, isolated := true, true
	for i, project := range projects {
		rows, _, err := store.RunSQLWithMeta(ctx, project.ID, `SELECT count(*) AS n, CAST(coalesce(sum(CAST(json_extract_string(properties, '$.seed_index') AS BIGINT)), 0) AS BIGINT) AS seed_sum, count(*) FILTER (WHERE json_extract_string(properties, '$.seed_project') IS NOT NULL AND json_extract_string(properties, '$.seed_project') != '`+project.ID+`') AS foreign_rows FROM events`)
		if err != nil {
			return false, false, err
		}
		events := numericCell(rows, "n")
		eventSum, foreignEvents := numericCell(rows, "seed_sum"), numericCell(rows, "foreign_rows")
		rows, _, err = store.RunSQLWithMeta(ctx, project.ID, `SELECT count(*) AS n, CAST(coalesce(sum(CAST(json_extract_string(data, '$.amount') AS BIGINT)), 0) AS BIGINT) AS amount_sum, count(*) FILTER (WHERE json_extract_string(data, '$.seed_project') != '`+project.ID+`') AS foreign_rows FROM external_rows WHERE table_name='orders'`)
		if err != nil {
			return false, false, err
		}
		external := numericCell(rows, "n")
		externalSum, foreignExternal := numericCell(rows, "amount_sum"), numericCell(rows, "foreign_rows")
		report.Tenants[i].AcceptedLiveEvents, report.Tenants[i].ObservedEvents, report.Tenants[i].ObservedExternal = counts[i], events, external
		report.Tenants[i].ObservedEventSum, report.Tenants[i].ObservedExternalSum = eventSum, externalSum
		wantEvents := int64(envelopeEventsPerProject) + counts[i]
		if events != wantEvents || external != envelopeExternalPerProject || eventSum != envelopeSeedIndexSum(envelopeEventsPerProject) || externalSum != envelopeAmountSum(envelopeExternalPerProject) {
			noLoss = false
		}
		// Exact per-tenant totals prove the sandbox did not leak another tenant's
		// 10M-row corpus into this query result.
		if events > wantEvents || external > envelopeExternalPerProject || foreignEvents != 0 || foreignExternal != 0 {
			isolated = false
		}
	}
	return noLoss, isolated, nil
}

func envelopeSeedIndexSum(rows int) int64 { return int64(rows) * int64(rows-1) / 2 }
func envelopeAmountSum(rows int) int64 {
	cycles, remainder := rows/9900, rows%9900
	// Values repeat from 100 through 9999 inclusive.
	return int64(cycles)*int64(100+9999)*9900/2 + int64(remainder)*int64(2*100+remainder-1)/2
}

func startEnvelopeBroker(dir string) (string, func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	srv, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: dir, NoLog: true, NoSigs: true})
	if err != nil {
		return "", nil, err
	}
	go srv.Start()
	if !srv.ReadyForConnections(15 * time.Second) {
		srv.Shutdown()
		return "", nil, errors.New("embedded JetStream did not become ready")
	}
	return srv.ClientURL(), srv.Shutdown, nil
}

func startEnvelopePostgres(ctx context.Context) (string, func() error, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "", nil, errors.New("docker is required to own and clean up the disposable PostgreSQL resource")
	}
	name := "agentray-data-envelope-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	password := uuid.NewString()
	cmd := exec.CommandContext(ctx, "docker", "run", "--detach", "--rm", "--name", name, "--publish", "127.0.0.1::5432", "--env", "POSTGRES_USER=envelope", "--env", "POSTGRES_PASSWORD="+password, "--env", "POSTGRES_DB=envelope", "postgres:16-alpine")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("docker run postgres: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	cleanup := func() error {
		out, err := exec.Command("docker", "rm", "--force", name).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "No such container") {
			return fmt.Errorf("docker rm %s: %w (%s)", name, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	fail := func(err error) (string, func() error, error) { _ = cleanup(); return "", nil, err }
	var port string
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.CommandContext(ctx, "docker", "port", name, "5432/tcp").Output()
		if err == nil {
			line := strings.TrimSpace(string(out))
			if cut := strings.LastIndex(line, ":"); cut >= 0 {
				port = line[cut+1:]
			}
		}
		if port != "" {
			url := fmt.Sprintf("postgres://envelope:%s@127.0.0.1:%s/envelope?sslmode=disable", password, port)
			pool, err := pgxpool.New(ctx, url)
			if err == nil {
				pingCtx, cancel := context.WithTimeout(ctx, time.Second)
				err = pool.Ping(pingCtx)
				cancel()
				pool.Close()
				if err == nil {
					return url, cleanup, nil
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fail(errors.New("disposable PostgreSQL did not become ready within 45s"))
}

func writeEnvelopeReport(path string, report *envelopeReport) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func inspectEnvelopeHardware(path string) (envelopeHardware, error) {
	h := envelopeHardware{OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), CPUModel: cpuModel(), RAMBytes: physicalMemoryBytes(), CgroupLimitBytes: cgroupMemoryLimit(), Filesystem: filesystemType(path)}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return h, err
	}
	h.DiskTotalBytes, h.DiskFreeBytes = uint64(stat.Blocks)*uint64(stat.Bsize), uint64(stat.Bavail)*uint64(stat.Bsize)
	if h.RAMBytes == 0 || h.DiskTotalBytes == 0 {
		return h, errors.New("could not detect RAM or disk capacity on approved host")
	}
	return h, nil
}

func resourceEnvelopeSample(at time.Time, duckPath, brokerDir string) envelopeResourceSample {
	return envelopeResourceSample{At: at.UTC(), RSSBytes: currentRSSBytes(), DuckDBBytes: uint64(max(0, fileSize(duckPath))), WALBytes: uint64(max(0, fileSize(duckPath+".wal"))), SpillBytes: treeSize(filepath.Join(filepath.Dir(duckPath), "tmp")), BrokerBytes: treeSize(brokerDir), DiskFree: diskFree(filepath.Dir(duckPath))}
}

func currentRSSBytes() uint64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0
	}
	kib, _ := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	return kib * 1024
}

func physicalMemoryBytes() uint64 {
	if runtime.GOOS == "darwin" {
		out, _ := exec.Command("sysctl", "-n", "hw.memsize").Output()
		n, _ := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
		return n
	}
	body, _ := os.ReadFile("/proc/meminfo")
	for _, line := range strings.Split(string(body), "\n") {
		var kib uint64
		if _, err := fmt.Sscanf(line, "MemTotal: %d kB", &kib); err == nil {
			return kib * 1024
		}
	}
	return 0
}

func cpuModel() string {
	if runtime.GOOS == "darwin" {
		out, _ := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
		return strings.TrimSpace(string(out))
	}
	body, _ := os.ReadFile("/proc/cpuinfo")
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "model name") {
			if _, value, ok := strings.Cut(line, ":"); ok {
				return strings.TrimSpace(value)
			}
		}
	}
	return "unknown"
}

func filesystemType(path string) string {
	args := []string{"-f", "%T", path}
	if runtime.GOOS == "linux" {
		args = []string{"-f", "-c", "%T", path}
	}
	out, _ := exec.Command("stat", args...).Output()
	return strings.TrimSpace(string(out))
}

func cgroupMemoryLimit() uint64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	body, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil || strings.TrimSpace(string(body)) == "max" {
		return 0
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(string(body)), 10, 64)
	return n
}

func cgroupOOMEvents() uint64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	body, _ := os.ReadFile("/sys/fs/cgroup/memory.events")
	var total uint64
	for _, line := range strings.Split(string(body), "\n") {
		var name string
		var n uint64
		if _, err := fmt.Sscanf(line, "%s %d", &name, &n); err == nil && (name == "oom" || name == "oom_kill") {
			total += n
		}
	}
	return total
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
func treeSize(root string) uint64 {
	var total uint64
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, statErr := d.Info(); statErr == nil {
				total += uint64(info.Size())
			}
		}
		return nil
	})
	return total
}
func diskFree(path string) uint64 {
	var stat syscall.Statfs_t
	if syscall.Statfs(path, &stat) != nil {
		return 0
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize)
}
