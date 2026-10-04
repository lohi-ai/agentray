package ingestion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
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
	envelopeRunTimeout         = 58 * time.Minute
	envelopePostgresMemory     = 512 * 1024 * 1024
	envelopeMinMemoryLimit     = 2 * 1024 * 1024 * 1024
	envelopeMaxAggregateMemory = 3 * 1024 * 1024 * 1024
	envelopeMaxMemoryLimit     = envelopeMaxAggregateMemory - envelopePostgresMemory
	envelopeRequiredGCEProject = "lohi-dev-lohi"
	envelopeRequiredGCEZone    = "asia-southeast1-a"
	envelopeRequiredGCEName    = "lohi-app"
	envelopeMetadataBaseURL    = "http://169.254.169.254/computeMetadata/v1"
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
	GCEProject       string `json:"gce_project"`
	GCEZone          string `json:"gce_zone"`
	GCEInstance      string `json:"gce_instance"`
}

type envelopeHostIdentity struct {
	Project, Zone, Instance string
}

type envelopeRuntimeCaps struct {
	AllowedCPUs     string
	AllowedCPUCount int
	Nice            int
	MemoryMaxBytes  uint64
	InternalTimeout time.Duration
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
	RSSComplete bool      `json:"rss_complete"`
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
	CleanupCycles            int           `json:"cleanup_cycles"`
	RecoveryCycles           int           `json:"recovery_cycles"`
	CleanupRows              int           `json:"cleanup_rows"`
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
			if !sample.RSSComplete {
				failures = append(failures, "RSS process-tree sampling was incomplete")
				break
			}
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

func passingEnvelopeObserved() envelopeObserved {
	return envelopeObserved{
		Warm: []time.Duration{time.Second}, Cold: []time.Duration{time.Second},
		Publication: []time.Duration{time.Millisecond}, Queryable: []time.Duration{time.Millisecond},
		Resources: []envelopeResourceSample{
			{RSSBytes: 10, RSSComplete: true}, {RSSBytes: 10, RSSComplete: true},
			{RSSBytes: 10, RSSComplete: true}, {RSSBytes: 10, RSSComplete: true},
		},
		RAMBytes: 100, NoCrossProjectInterference: true, NoDataLoss: true,
		AcceptedEvents: 10, ExpectedAccepted: 10, QueryableEvents: 10,
	}
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
		Resources: []envelopeResourceSample{{RSSBytes: 10, RSSComplete: true}, {RSSBytes: 20, RSSComplete: true}, {RSSBytes: 30, RSSComplete: true}, {RSSBytes: 95, RSSComplete: true}}, RAMBytes: 100,
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

func TestDataEnvelopeSteadyLatencyNegativeControl(t *testing.T) {
	for _, tc := range []struct {
		name, phase, want string
		duration          time.Duration
	}{
		{name: "warm exceeds warm bound", phase: "warm", duration: approvedEnvelopeThresholds.WarmP95 + time.Nanosecond, want: "warm_p95"},
		{name: "cold excludes warm bound but exceeds cold bound", phase: "cold", duration: approvedEnvelopeThresholds.ColdP95 + time.Nanosecond, want: "cold_p95"},
		{name: "unknown phase fails closed", phase: "steady", duration: time.Second, want: "unknown investigation phase"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := &envelopeReport{
				WarmSamples: []time.Duration{time.Second}, ColdSamples: []time.Duration{time.Second},
				InvestigationSamples: []envelopeSample{{Phase: tc.phase, Duration: tc.duration}},
			}
			warm, cold, classifyErr := envelopeLatencyInputs(report)
			if classifyErr != nil {
				if !strings.Contains(classifyErr.Error(), tc.want) {
					t.Fatalf("classification error = %v, want %q", classifyErr, tc.want)
				}
				return
			}
			observed := passingEnvelopeObserved()
			observed.Warm, observed.Cold = warm, cold
			if err := validateEnvelope(observed, approvedEnvelopeThresholds); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s investigation error = %v, want %s failure", tc.phase, err, tc.want)
			}
			if tc.phase == "cold" {
				belowCold := *report
				belowCold.InvestigationSamples = []envelopeSample{{Phase: "cold", Duration: approvedEnvelopeThresholds.WarmP95 + time.Second}}
				warm, cold, err := envelopeLatencyInputs(&belowCold)
				if err != nil {
					t.Fatal(err)
				}
				observed.Warm, observed.Cold = warm, cold
				if err := validateEnvelope(observed, approvedEnvelopeThresholds); err != nil {
					t.Fatalf("valid cold refresh was subjected to warm bound: %v", err)
				}
			}
		})
	}
}

func TestDataEnvelopeHostIdentityNegativeControl(t *testing.T) {
	err := validateEnvelopeHostIdentity(envelopeHostIdentity{Project: envelopeRequiredGCEProject, Zone: envelopeRequiredGCEZone, Instance: "some-other-linux-host"})
	if err == nil || !strings.Contains(err.Error(), envelopeRequiredGCEName) {
		t.Fatalf("wrong GCE identity error = %v, want lohi-app refusal", err)
	}
}

func TestDataEnvelopeSandboxPhaseNegativeControl(t *testing.T) {
	proc := t.TempDir()
	writeCmdline := func(pid int, projectID string) {
		t.Helper()
		dir := filepath.Join(proc, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "test-binary\x00--duckdb-sandbox-worker\x00--tmp-dir=/tmp/sandbox-" + projectID + "-123\x00"
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeCmdline(101, "tenant-a")
	warmBefore := envelopeSandboxProcesses(proc, "tenant-a")
	warmAfter := envelopeSandboxProcesses(proc, "tenant-a")
	if phase := envelopeInvestigationPhase(warmBefore, warmAfter); phase != "warm" {
		t.Fatalf("surviving sandbox phase = %q, want warm", phase)
	}
	writeCmdline(102, "tenant-b")
	if phase := envelopeInvestigationPhase(warmBefore, envelopeSandboxProcesses(proc, "tenant-b")); phase != "cold" {
		t.Fatalf("replaced sandbox phase = %q, want cold", phase)
	}
}

func TestDataEnvelopeAcceptedRateNegativeControl(t *testing.T) {
	observed := passingEnvelopeObserved()
	observed.ExpectedAccepted = int64(envelopeAcceptedPerSecond) * int64(envelopeSteadyDuration/time.Second)
	observed.AcceptedEvents = observed.ExpectedAccepted - 1
	observed.QueryableEvents = observed.AcceptedEvents
	if err := validateEnvelope(observed, approvedEnvelopeThresholds); err == nil || !strings.Contains(err.Error(), "accepted ingest rate") {
		t.Fatalf("one-event rate shortfall error = %v, want inclusive 100 events/second failure", err)
	}
}

func TestDataEnvelopeRuntimeCapsNegativeControl(t *testing.T) {
	err := validateEnvelopeRuntimeCaps(envelopeRuntimeCaps{AllowedCPUs: "0-1", AllowedCPUCount: 2, Nice: 0, MemoryMaxBytes: 0, InternalTimeout: 61 * time.Minute})
	if err == nil {
		t.Fatal("uncapped runtime unexpectedly passed")
	}
	for _, want := range []string{"one allowed CPU", "nice=19", "MemoryMax", "timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("uncapped runtime error %q missing %q", err, want)
		}
	}
	tooLarge := envelopeRuntimeCaps{AllowedCPUs: "0", AllowedCPUCount: 1, Nice: 19, MemoryMaxBytes: envelopeMaxMemoryLimit + 1, InternalTimeout: envelopeRunTimeout}
	if err := validateEnvelopeRuntimeCaps(tooLarge); err == nil || !strings.Contains(err.Error(), "aggregate") {
		t.Fatalf("aggregate memory ceiling violation error = %v, want refusal", err)
	}
	if err := validateEnvelopeRunWindow(time.Date(2026, time.October, 4, 6, 0, 0, 0, time.FixedZone("UTC+7", 7*60*60))); err == nil {
		t.Fatal("out-of-window capacity run unexpectedly passed")
	}
}

func TestDataEnvelopePostgresWatchdogNegativeControl(t *testing.T) {
	if os.Getenv("AGENTRAY_ENVELOPE_WATCHDOG_CHILD") == "1" {
		watchdog, err := startEnvelopeDockerWatchdog(os.Getpid(), "agentray-watchdog-negative-control", []string{"run", "--rm", "--name", "agentray-watchdog-negative-control", "fake-postgres"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv("AGENTRAY_ENVELOPE_WATCHDOG_READY"), []byte(strconv.Itoa(watchdog.cmd.Process.Pid)), 0o644); err != nil {
			t.Fatal(err)
		}
		select {}
	}

	dir := t.TempDir()
	runPID, removed, ready := filepath.Join(dir, "run.pid"), filepath.Join(dir, "removed"), filepath.Join(dir, "ready")
	docker := filepath.Join(dir, "docker")
	script := `#!/bin/sh
case "$1" in
run)
  # Model an attached client blocked on PostgreSQL smart shutdown while an
  # active query is still running: TERM alone must not let cleanup proceed.
  trap '' TERM INT HUP
  mkfifo "$AGENTRAY_ENVELOPE_FAKE_FIFO"
  echo $$ > "$AGENTRAY_ENVELOPE_FAKE_RUN_PID"
  IFS= read -r ignored < "$AGENTRAY_ENVELOPE_FAKE_FIFO"
  ;;
rm)
  if [ ! -e "$AGENTRAY_ENVELOPE_FAKE_REMOVED" ]; then
    : > "$AGENTRAY_ENVELOPE_FAKE_REMOVED"
    kill -KILL "$(cat "$AGENTRAY_ENVELOPE_FAKE_RUN_PID")" >/dev/null 2>&1 || true
  fi
  ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDataEnvelopePostgresWatchdogNegativeControl$", "-test.v")
	cmd.Env = append(os.Environ(),
		"AGENTRAY_ENVELOPE_WATCHDOG_CHILD=1",
		"AGENTRAY_ENVELOPE_WATCHDOG_READY="+ready,
		"AGENTRAY_ENVELOPE_FAKE_RUN_PID="+runPID,
		"AGENTRAY_ENVELOPE_FAKE_REMOVED="+removed,
		"AGENTRAY_ENVELOPE_FAKE_FIFO="+filepath.Join(dir, "blocked-query"),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	watchdogPID := waitEnvelopePIDFile(t, ready, 5*time.Second)
	dockerPID := waitEnvelopePIDFile(t, runPID, 5*time.Second)
	parentPGID, parentPGErr := syscall.Getpgid(cmd.Process.Pid)
	watchdogPGID, watchdogPGErr := syscall.Getpgid(watchdogPID)
	if parentPGErr != nil || watchdogPGErr != nil || parentPGID == watchdogPGID {
		t.Fatalf("watchdog process-group isolation: parent=%d err=%v watchdog=%d err=%v", parentPGID, parentPGErr, watchdogPGID, watchdogPGErr)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed watchdog parent unexpectedly exited successfully")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (!pathExists(removed) || envelopeProcessExists(watchdogPID) || envelopeProcessExists(dockerPID)) {
		time.Sleep(25 * time.Millisecond)
	}
	if !pathExists(removed) {
		t.Fatal("watchdog did not remove the owned container after its parent was killed")
	}
	if envelopeProcessExists(watchdogPID) || envelopeProcessExists(dockerPID) {
		t.Fatalf("orphan remained after parent death: watchdog_alive=%t docker_alive=%t", envelopeProcessExists(watchdogPID), envelopeProcessExists(dockerPID))
	}
}

type envelopeRealWatchdogReady struct {
	Container   string `json:"container"`
	WatchdogPID int    `json:"watchdog_pid"`
	DockerPID   int    `json:"docker_pid"`
}

// TestDataEnvelopePostgresWatchdogActiveQueryNegativeControl exercises the
// actual Docker/PostgreSQL shutdown behavior that a fake client cannot fully
// model. It is opt-in so ordinary unit runs do not pull images or require a
// Docker daemon; the AC-DATA-03 repair verification enables it explicitly.
func TestDataEnvelopePostgresWatchdogActiveQueryNegativeControl(t *testing.T) {
	if readyPath := os.Getenv("AGENTRAY_ENVELOPE_REAL_WATCHDOG_CHILD"); readyPath != "" {
		ctx := context.Background()
		url, container, cleanup, err := startEnvelopePostgres(ctx, "0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cleanup() }()
		dockerPID := waitEnvelopePIDFile(t, os.Getenv("AGENTRAY_ENVELOPE_WATCHDOG_DOCKER_PID_FILE"), 5*time.Second)
		pool, err := pgxpool.New(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		// The watchdog is the direct parent of the attached Docker client. Record
		// its real PID instead of relying on adjacent PID allocation.
		watchdogPID, err := envelopeParentPID(dockerPID)
		if err != nil {
			t.Fatal(err)
		}
		ready, err := json.Marshal(envelopeRealWatchdogReady{Container: container, WatchdogPID: watchdogPID, DockerPID: dockerPID})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(readyPath, ready, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = pool.Exec(ctx, "SELECT pg_sleep(90)")
		if err != nil {
			t.Fatal(err)
		}
		return
	}

	if os.Getenv("AGENTRAY_ENVELOPE_REAL_WATCHDOG") != "1" {
		t.Skip("set AGENTRAY_ENVELOPE_REAL_WATCHDOG=1 to run the real active-query watchdog control")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("AGENTRAY_ENVELOPE_REAL_WATCHDOG=1 requires docker")
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Fatalf("AGENTRAY_ENVELOPE_REAL_WATCHDOG=1 requires a running Docker daemon: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready.json")
	dockerPIDPath := filepath.Join(dir, "docker.pid")
	cmd := exec.Command(os.Args[0], "-test.run=^TestDataEnvelopePostgresWatchdogActiveQueryNegativeControl$", "-test.timeout=2m", "-test.v")
	cmd.Env = append(os.Environ(),
		"AGENTRAY_ENVELOPE_REAL_WATCHDOG_CHILD="+readyPath,
		"AGENTRAY_ENVELOPE_WATCHDOG_DOCKER_PID_FILE="+dockerPIDPath,
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var ready envelopeRealWatchdogReady
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		if ready.Container != "" {
			_ = exec.Command("docker", "rm", "--force", ready.Container).Run()
		}
	})
	deadline := time.Now().Add(50 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(readyPath)
		if err == nil {
			if err := json.Unmarshal(body, &ready); err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ready.Container == "" || ready.WatchdogPID <= 0 || ready.DockerPID <= 0 {
		t.Fatal("real PostgreSQL watchdog child did not become ready within 50s")
	}

	deadline = time.Now().Add(5 * time.Second)
	for {
		out, err := exec.Command("docker", "exec", ready.Container, "psql", "-U", "envelope", "-d", "envelope", "-Atc", "SELECT count(*) FROM pg_stat_activity WHERE query='SELECT pg_sleep(90)' AND state='active'").Output()
		if err == nil && strings.TrimSpace(string(out)) == "1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("SELECT pg_sleep(90) did not become active within 5s")
		}
		time.Sleep(50 * time.Millisecond)
	}

	started := time.Now()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed real PostgreSQL watchdog parent unexpectedly exited successfully")
	}
	deadline = time.Now().Add(10 * time.Second)
	containerAbsent := false
	for time.Now().Before(deadline) {
		_, inspectErr := exec.Command("docker", "inspect", ready.Container).Output()
		containerAbsent = inspectErr != nil
		if containerAbsent && !envelopeProcessExists(ready.WatchdogPID) && !envelopeProcessExists(ready.DockerPID) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("active-query cleanup elapsed=%s container_absent=%t watchdog_absent=%t docker_absent=%t", time.Since(started), containerAbsent, !envelopeProcessExists(ready.WatchdogPID), !envelopeProcessExists(ready.DockerPID))
	if !containerAbsent || envelopeProcessExists(ready.WatchdogPID) || envelopeProcessExists(ready.DockerPID) {
		t.Fatal("orphan remained after killing the real active-query parent")
	}
}

func envelopeParentPID(pid int) (int, error) {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	parent, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || parent <= 0 {
		return 0, fmt.Errorf("invalid parent PID %q for process %d", strings.TrimSpace(string(out)), pid)
	}
	return parent, nil
}

func waitEnvelopePIDFile(t *testing.T, path string, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
			if err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PID file %s was not written within %s", path, within)
	return 0
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func envelopeProcessExists(pid int) bool {
	process, err := os.FindProcess(pid)
	return err == nil && process.Signal(syscall.Signal(0)) == nil
}

func TestDataEnvelopeProcessTreeRSSNegativeControl(t *testing.T) {
	proc := t.TempDir()
	writeStatus := func(pid, ppid, rssKiB int) {
		t.Helper()
		dir := filepath.Join(proc, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf("Name:\ttest\nPPid:\t%d\nVmRSS:\t%d kB\n", ppid, rssKiB)
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeStatus(100, 1, 10)
	writeStatus(101, 100, 40)
	writeStatus(102, 101, 45)
	writeStatus(999, 1, 500)
	rss, err := processTreeRSSBytes(proc, []int{100})
	if err != nil || rss != 95*1024 {
		t.Fatalf("process-tree RSS = %d err=%v, want parent+children 97280", rss, err)
	}
	observed := passingEnvelopeObserved()
	for i := range observed.Resources {
		observed.Resources[i].RSSBytes = rss
	}
	observed.RAMBytes = 100 * 1024
	if err := validateEnvelope(observed, approvedEnvelopeThresholds); err == nil || !strings.Contains(err.Error(), "rss_high_water") {
		t.Fatalf("child-heavy RSS error = %v, want aggregate high-water failure", err)
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

	ctx, cancel := context.WithTimeout(context.Background(), envelopeRunTimeout)
	defer cancel()
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

	if runtime.GOOS != "linux" {
		report.Failure = fmt.Sprintf("AC-DATA-03 envelope refused: required host class %q is Linux; detected %s", envelopeRequiredHostClass, runtime.GOOS)
		t.Fatal(report.Failure)
	}
	identity, err := inspectEnvelopeHostIdentity(ctx, &http.Client{Timeout: 2 * time.Second}, envelopeMetadataBaseURL)
	if err != nil {
		report.Failure = err.Error()
		t.Fatal(err)
	}
	if err := validateEnvelopeHostIdentity(identity); err != nil {
		report.Failure = err.Error()
		t.Fatal(err)
	}
	caps, err := inspectEnvelopeRuntimeCaps()
	if err != nil {
		report.Failure = err.Error()
		t.Fatal(err)
	}
	if err := validateEnvelopeRuntimeCaps(caps); err != nil {
		report.Failure = err.Error()
		t.Fatal(err)
	}
	if err := validateEnvelopeRunWindow(time.Now()); err != nil {
		report.Failure = err.Error()
		t.Fatal(err)
	}

	root := t.TempDir()
	report.Hardware, err = inspectEnvelopeHardware(root)
	if err != nil {
		report.Failure = err.Error()
		t.Fatal(err)
	}
	report.Hardware.GCEProject, report.Hardware.GCEZone, report.Hardware.GCEInstance = identity.Project, identity.Zone, identity.Instance
	if report.Hardware.CgroupLimitBytes > 0 && report.Hardware.CgroupLimitBytes < report.Hardware.RAMBytes {
		report.Hardware.RAMBytes = report.Hardware.CgroupLimitBytes
	}
	report.OOMEventsBefore = cgroupOOMEvents()

	pgURL, postgresContainer, cleanupPG, err := startEnvelopePostgres(ctx, caps.AllowedCPUs)
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
		"allowed_cpus":             caps.AllowedCPUs,
		"nice":                     caps.Nice,
		"memory_max_bytes":         caps.MemoryMaxBytes,
		"internal_timeout":         caps.InternalTimeout.String(),
		"postgres_memory_bytes":    envelopePostgresMemory,
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
				sample := resourceEnvelopeSample(at, cfg.DuckDBPath, brokerDir, postgresContainer)
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
			candidates := accepted.pendingBatch(i, 1_000)
			rows, _, queryErr := store.RunSQLWithMeta(catchupCtx, project.ID, envelopeQueryableSQL(candidates))
			if queryErr == nil {
				accepted.observeVisibleBatch(i, project.ID, candidates, numericCell(rows, "visible"), time.Now(), report, &reportMu)
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
	finalResource := resourceEnvelopeSample(time.Now(), cfg.DuckDBPath, brokerDir, postgresContainer)
	finalResource.Phase = samplePhase
	report.ResourceSamples = append(report.ResourceSamples, finalResource)
	reportMu.Unlock()
	stopSampler()
	warm, cold, err := envelopeLatencyInputs(report)
	if err != nil {
		report.Status, report.Failure = "failed", err.Error()
		t.Fatal(err)
	}
	observed := envelopeObserved{
		Warm: warm, Cold: cold, Publication: envelopeDurations(report.PublicationSamples),
		Queryable: envelopeDurations(report.QueryableSamples), Resources: report.ResourceSamples, RAMBytes: report.Hardware.RAMBytes,
		OOMBefore: report.OOMEventsBefore, OOMAfter: report.OOMEventsAfter,
		NoCrossProjectInterference: isolated, NoDataLoss: noLoss, QueryErrors: len(report.QueryErrors),
		RefusedQueries: report.RefusedQueries, AcceptedEvents: accepted.total(),
		ExpectedAccepted: int64(envelopeAcceptedPerSecond) * int64(envelopeSteadyDuration/time.Second),
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

func envelopeLatencyInputs(report *envelopeReport) (warm, cold []time.Duration, err error) {
	warm = slices.Clone(report.WarmSamples)
	cold = slices.Clone(report.ColdSamples)
	for _, sample := range report.InvestigationSamples {
		switch sample.Phase {
		case "warm":
			warm = append(warm, sample.Duration)
		case "cold":
			cold = append(cold, sample.Duration)
		default:
			return nil, nil, fmt.Errorf("unknown investigation phase %q for tenant=%q recipe=%q", sample.Phase, sample.Tenant, sample.Recipe)
		}
	}
	return warm, cold, nil
}

type envelopeRecipe struct{ Name, SQL string }

func envelopeRecipes() []envelopeRecipe {
	return []envelopeRecipe{
		{"62_day_activity", `SELECT date_trunc('day', "timestamp") AS day, count(*) AS events FROM events WHERE "timestamp" >= current_timestamp - INTERVAL '62 days' GROUP BY day ORDER BY day`},
		{"62_day_purchases", `SELECT date_trunc('day', "timestamp") AS day, count(*) AS purchases FROM events WHERE event_name='purchase' AND "timestamp" >= current_timestamp - INTERVAL '62 days' GROUP BY day ORDER BY day`},
		{"lifetime_first_payer", `SELECT min(first_paid_at) AS first_paid_at FROM (SELECT canonical_id, min("timestamp") AS first_paid_at FROM events WHERE event_name='purchase' GROUP BY canonical_id) payers`},
		{"queryable_identities", `SELECT CAST(0 AS BIGINT) AS visible`},
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
	pg, err := pgxpool.New(ctx, cfg.PostgresURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	if _, err := pg.Exec(ctx, `INSERT INTO data_connectors(id,project_id,name,kind)
VALUES($1,$2,'capacity-staging','postgres') ON CONFLICT(id) DO NOTHING`, connectorID, projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(ctx, `INSERT INTO connector_syncs(id,connector_id,project_id,source_table,key_column,sync_mode)
VALUES($1,$2,$3,'envelope_staging_probe','id','snapshot') ON CONFLICT(id) DO NOTHING`, syncID, connectorID, projectID); err != nil {
		t.Fatal(err)
	}
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
	manifest, err := connector.SnapshotManifestDigest([]connector.SnapshotManifestEntry{{Index: 0, BatchID: "probe-0", PayloadSHA256: digest, RowCount: len(rows)}})
	if err != nil {
		t.Fatal(err)
	}
	var previous connector.SnapshotEnvelope
	cleanupCycles, recoveryCycles, cleanupRows := 0, 0, 0
	staged := before
	for cycle := 1; cycle <= 3; cycle++ {
		started := time.Now().UTC().Add(-time.Minute)
		generation := uuid.NewSHA1(namespace, []byte(fmt.Sprintf("staging-generation-%d", cycle))).String()
		runID := uuid.NewSHA1(namespace, []byte(fmt.Sprintf("staging-run-%d", cycle))).String()
		batch := connector.SnapshotEnvelope{Protocol: connector.SnapshotProtocolV1, ProjectID: projectID, ConnectorID: connectorID, Table: "envelope_staging_probe", SyncID: syncID, RunID: runID, Generation: generation, GenerationSeq: int64(cycle), BindingDigest: strings.Repeat("a", 64), Kind: connector.SnapshotKindBatch, BatchID: "probe-0", BatchIndex: 0, PayloadSHA256: digest, CaptureStartedAt: started, Rows: rows}
		if _, err := store.ApplySnapshotEnvelope(ctx, batch, storage.AppliedMark{}); err != nil {
			t.Fatal(err)
		}
		if cycle == 1 {
			staged = fileSize(duckPath)
		}
		finished := time.Now().UTC()
		complete := batch
		complete.Kind, complete.BatchID, complete.BatchIndex, complete.PayloadSHA256, complete.Rows = connector.SnapshotKindComplete, "", 0, "", nil
		complete.ExpectedBatches, complete.ExpectedRows, complete.BatchManifestSHA256, complete.CaptureFinishedAt = 1, int64(len(rows)), manifest, &finished
		promotion, err := store.ApplySnapshotEnvelope(ctx, complete, storage.AppliedMark{})
		if err != nil || promotion == nil {
			t.Fatalf("staging promotion cycle %d: promotion=%+v err=%v", cycle, promotion, err)
		}
		if _, err := pg.Exec(ctx, `INSERT INTO connector_snapshot_generations
(project_id,connector_id,table_name,sync_id,generation,generation_seq,binding_digest,state,capture_started_at,capture_finished_at,terminal_at,run_id,owner,lease_epoch)
VALUES($1,$2,$3,$4,$5,$6,$7,'sealed',$8,$9,$9,$10,'capacity',1)`, projectID, connectorID, batch.Table, syncID,
			generation, cycle, batch.BindingDigest, started, finished, runID); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.Exec(ctx, `INSERT INTO connector_snapshot_outbox
(project_id,connector_id,table_name,generation,batch_id,batch_index,kind,payload,published,published_at)
VALUES($1,$2,$3,$4,'probe-0',0,'batch',$5,true,now())`, projectID, connectorID, batch.Table, generation, []byte("capacity-staging-payload")); err != nil {
			t.Fatal(err)
		}
		if cycle > 1 {
			deleted, eligible, err := store.DeleteEligibleStagingChunk(ctx, previous.Generation, time.Now().UTC().Add(time.Hour), 50_000)
			if err != nil || !eligible || deleted != len(rows) {
				t.Fatalf("staging cleanup cycle %d: deleted=%d eligible=%v err=%v", cycle-1, deleted, eligible, err)
			}
			cleanupCycles++
			cleanupRows += deleted
			if _, err := store.ApplySnapshotEnvelope(ctx, previous, storage.AppliedMark{}); err != nil {
				t.Fatalf("staging recovery cycle %d: %v", cycle-1, err)
			}
			recoveryCycles++
			var outboxRows int
			if err := pg.QueryRow(ctx, `SELECT count(*) FROM connector_snapshot_outbox WHERE generation=$1`, previous.Generation).Scan(&outboxRows); err != nil || outboxRows != 0 {
				t.Fatalf("staging cleanup cycle %d retained outbox=%d err=%v", cycle-1, outboxRows, err)
			}
		}
		previous = batch
	}
	after := fileSize(duckPath)
	return envelopeRetention{EventRetentionDays: cfg.EventRetentionDays, StagingTTL: cfg.SourceStagingTTL,
		ProbeRows: len(rows), ProbeLogicalBytes: logical, StagingDiskGrowthBytes: staged - before,
		PostPromotionGrowthBytes: after - before, CleanupCycles: cleanupCycles, RecoveryCycles: recoveryCycles,
		CleanupRows: cleanupRows, CleanupVerified: cleanupCycles == 2 && recoveryCycles == 2 && cleanupRows == 2*len(rows)}
}

type envelopeAcceptance struct {
	Seq     int64
	EventID string
	At      time.Time
}
type envelopeAcceptances struct {
	mu        sync.Mutex
	next      []int64
	successes []int64
	pending   [][]envelopeAcceptance
}

func newEnvelopeAcceptances(projects int) *envelopeAcceptances {
	return &envelopeAcceptances{next: make([]int64, projects), successes: make([]int64, projects), pending: make([][]envelopeAcceptance, projects)}
}

func TestDataEnvelopeAcceptedIdentityNegativeControl(t *testing.T) {
	accepted := newEnvelopeAcceptances(1)
	first, second := accepted.reserve(0), accepted.reserve(0)
	accepted.accepted(0, first, "event-one", time.Now())
	accepted.accepted(0, second, "event-two", time.Now())
	report := &envelopeReport{}
	var reportMu sync.Mutex

	// Seeing only the event with the maximum reserved sequence must not imply
	// that the earlier accepted identity is queryable.
	accepted.observeVisibleBatch(0, "tenant", []envelopeAcceptance{{Seq: second, EventID: "event-two"}}, 1, time.Now(), report, &reportMu)
	if accepted.complete() {
		t.Fatal("maximum accepted sequence incorrectly proved every accepted identity queryable")
	}
	if len(report.QueryableSamples) != 1 || report.QueryableSamples[0].Value != second {
		t.Fatalf("identity samples = %+v, want only event-two", report.QueryableSamples)
	}
}

func (a *envelopeAcceptances) reserve(project int) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.next[project]++
	return a.next[project]
}
func (a *envelopeAcceptances) accepted(project int, seq int64, eventID string, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.successes[project]++
	a.pending[project] = append(a.pending[project], envelopeAcceptance{Seq: seq, EventID: eventID, At: at})
}

func (a *envelopeAcceptances) pendingBatch(project, limit int) []envelopeAcceptance {
	a.mu.Lock()
	defer a.mu.Unlock()
	if limit <= 0 || limit > len(a.pending[project]) {
		limit = len(a.pending[project])
	}
	return slices.Clone(a.pending[project][:limit])
}

func (a *envelopeAcceptances) observeVisibleBatch(project int, tenant string, candidates []envelopeAcceptance, visible int64, at time.Time, report *envelopeReport, reportMu *sync.Mutex) {
	if len(candidates) == 0 || visible != int64(len(candidates)) {
		return
	}
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		seen[candidate.EventID] = struct{}{}
	}
	a.mu.Lock()
	remaining := a.pending[project][:0]
	samples := make([]envelopeSample, 0, len(candidates))
	for _, pending := range a.pending[project] {
		if _, ok := seen[pending.EventID]; ok {
			samples = append(samples, envelopeSample{Phase: "queryable", Tenant: tenant, StartedAt: pending.At.UTC(), Duration: at.Sub(pending.At), Value: pending.Seq})
			delete(seen, pending.EventID)
			continue
		}
		remaining = append(remaining, pending)
	}
	a.pending[project] = remaining
	a.mu.Unlock()
	reportMu.Lock()
	report.QueryableSamples = append(report.QueryableSamples, samples...)
	reportMu.Unlock()
}
func (a *envelopeAcceptances) complete() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.pending {
		if len(a.pending[i]) != 0 {
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
	publish := func(at time.Time) {
		projectIndex := index % len(projects)
		index += 4
		seq := accepted.reserve(projectIndex)
		event := storage.Event{ProjectID: projects[projectIndex].ID, EventID: uuid.NewString(), DistinctID: fmt.Sprintf("live-%d-%d", workerID, seq%10_000), SessionID: fmt.Sprintf("live-session-%d", seq/5), EventName: "envelope.live", EventType: "user", Timestamp: at.UTC(), Platform: "web", Properties: fmt.Sprintf(`{"envelope_seq":%d,"publisher":%d}`, seq, workerID)}
		started := time.Now()
		err := queue.InsertEvents(ctx, []storage.Event{event})
		d := time.Since(started)
		reportMu.Lock()
		report.PublicationSamples = append(report.PublicationSamples, envelopeSample{Phase: "publication", Tenant: projects[projectIndex].ID, StartedAt: started.UTC(), Duration: d, Value: seq})
		if err != nil && ctx.Err() == nil {
			report.QueryErrors = append(report.QueryErrors, "publish: "+err.Error())
		}
		reportMu.Unlock()
		if err == nil {
			accepted.accepted(projectIndex, seq, event.EventID, time.Now())
		}
	}
	// Include the left edge of the 15-minute window. The following ticks cover
	// [40ms, 15m), yielding exactly 25 attempts/s per publisher without relying
	// on a timer firing at the cancellation boundary.
	publish(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-ticker.C:
			publish(at)
		}
	}
}

func runEnvelopeInvestigation(ctx context.Context, store *storage.Store, projects []storage.Project, recipe envelopeRecipe, workerID int, accepted *envelopeAcceptances, report *envelopeReport, reportMu *sync.Mutex) {
	iteration := 0
	for ctx.Err() == nil {
		projectIndex := (workerID + iteration) % len(projects)
		iteration++
		query := recipe.SQL
		var candidates []envelopeAcceptance
		if recipe.Name == "queryable_identities" {
			candidates = accepted.pendingBatch(projectIndex, 1_000)
			query = envelopeQueryableSQL(candidates)
		}
		before := envelopeSandboxProcesses("/proc", projects[projectIndex].ID)
		started := time.Now()
		rows, _, err := store.RunSQLWithMeta(ctx, projects[projectIndex].ID, query)
		d := time.Since(started)
		after := envelopeSandboxProcesses("/proc", projects[projectIndex].ID)
		phase := envelopeInvestigationPhase(before, after)
		reportMu.Lock()
		report.InvestigationSamples = append(report.InvestigationSamples, envelopeSample{Phase: phase, Tenant: projects[projectIndex].ID, Recipe: recipe.Name, StartedAt: started.UTC(), Duration: d})
		if err != nil && ctx.Err() == nil {
			report.QueryErrors = append(report.QueryErrors, recipe.Name+": "+err.Error())
			if strings.Contains(strings.ToLower(err.Error()), "busy") || strings.Contains(strings.ToLower(err.Error()), "capacity") {
				report.RefusedQueries++
			}
		}
		reportMu.Unlock()
		if err == nil && recipe.Name == "queryable_identities" {
			accepted.observeVisibleBatch(projectIndex, projects[projectIndex].ID, candidates, numericCell(rows, "visible"), time.Now(), report, reportMu)
		}
	}
}

func envelopeSandboxProcesses(procRoot, projectID string) map[int]struct{} {
	found := make(map[int]struct{})
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return found
	}
	prefix := "sandbox-" + projectID + "-"
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		for _, arg := range strings.Split(string(body), "\x00") {
			if value, ok := strings.CutPrefix(arg, "--tmp-dir="); ok && strings.HasPrefix(filepath.Base(value), prefix) {
				found[pid] = struct{}{}
				break
			}
		}
	}
	return found
}

func envelopeInvestigationPhase(before, after map[int]struct{}) string {
	for pid := range before {
		if _, sameWorker := after[pid]; sameWorker {
			return "warm"
		}
	}
	// No same project worker survived across the timed call: the call either
	// spawned/refreshed a new worker or raced with an eviction immediately after
	// completion. Both must use the more permissive cold-refresh budget; neither
	// may contaminate warm p95.
	return "cold"
}

func envelopeQueryableSQL(candidates []envelopeAcceptance) string {
	if len(candidates) == 0 {
		return `SELECT CAST(0 AS BIGINT) AS visible`
	}
	ids := make([]string, len(candidates))
	for i, candidate := range candidates {
		ids[i] = "'" + strings.ReplaceAll(candidate.EventID, "'", "''") + "'"
	}
	return `SELECT count(DISTINCT event_id) AS visible FROM events WHERE event_name='envelope.live' AND event_id IN (` + strings.Join(ids, ",") + `)`
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

type envelopeDockerWatchdog struct {
	cmd      *exec.Cmd
	done     chan struct{}
	waitErr  error
	stopOnce sync.Once
}

const envelopeDockerWatchdogScript = `
parent_pid=$1
container=$2
shift 2
docker_pid=
wait_bounded() {
  child_pid=$1
  attempts=0
  while kill -0 "$child_pid" >/dev/null 2>&1; do
    child_state=$(ps -o stat= -p "$child_pid" 2>/dev/null) || child_state=
    case "$child_state" in
      *Z*) break ;;
    esac
    if [ "$attempts" -ge 50 ]; then
      kill -KILL "$child_pid" >/dev/null 2>&1 || true
      break
    fi
    attempts=$((attempts + 1))
    sleep 0.1
  done
  wait "$child_pid" >/dev/null 2>&1 || true
}
force_remove() {
  docker rm --force "$container" >/dev/null 2>&1 &
  remove_pid=$!
  wait_bounded "$remove_pid"
}
cleanup() {
  trap - EXIT HUP INT TERM
  # Force removal before signalling or waiting on the attached client.
  # PostgreSQL interprets TERM as smart shutdown and can otherwise keep that
  # client (and this watchdog) alive for the duration of an active query.
  force_remove
  if [ -n "$docker_pid" ]; then
    kill "$docker_pid" >/dev/null 2>&1 || true
    wait_bounded "$docker_pid"
  fi
  # Close the startup race where the first removal ran before docker created
  # the named container but the attached client had already been launched.
  force_remove
}
trap cleanup EXIT HUP INT TERM
docker "$@" >/dev/null 2>&1 &
docker_pid=$!
if [ -n "$AGENTRAY_ENVELOPE_WATCHDOG_DOCKER_PID_FILE" ]; then
  printf '%s\n' "$docker_pid" > "$AGENTRAY_ENVELOPE_WATCHDOG_DOCKER_PID_FILE"
fi
while kill -0 "$parent_pid" >/dev/null 2>&1; do
  if ! kill -0 "$docker_pid" >/dev/null 2>&1; then
    wait "$docker_pid"
    exit $?
  fi
  sleep 0.1
done
exit 0
`

// startEnvelopeDockerWatchdog owns an attached docker client for the lifetime
// of parentPID. It deliberately is not CommandContext-owned: if Go times out,
// panics, is signalled, or is killed, this small independent process observes
// the parent death, stops the client, forcibly removes the named container,
// and exits. The outer timeout's TERM is also caught by the same cleanup trap.
func startEnvelopeDockerWatchdog(parentPID int, container string, dockerArgs []string) (*envelopeDockerWatchdog, error) {
	args := append([]string{"-c", envelopeDockerWatchdogScript, "agentray-postgres-watchdog", strconv.Itoa(parentPID), container}, dockerArgs...)
	cmd := exec.Command("sh", args...)
	// Put the watchdog in its own session, not merely another process group.
	// Parent/process-group death and terminal hangup must not remove the only
	// process still able to tear down Docker and reap the attached client.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	watchdog := &envelopeDockerWatchdog{cmd: cmd, done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start disposable PostgreSQL watchdog: %w", err)
	}
	go func() {
		watchdog.waitErr = cmd.Wait()
		close(watchdog.done)
	}()
	return watchdog, nil
}

func (w *envelopeDockerWatchdog) exited() (bool, error) {
	select {
	case <-w.done:
		return true, w.waitErr
	default:
		return false, nil
	}
}

func (w *envelopeDockerWatchdog) stop(container string) error {
	var result error
	w.stopOnce.Do(func() {
		_ = w.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-w.done:
		case <-time.After(10 * time.Second):
			_ = w.cmd.Process.Kill()
			<-w.done
			result = errors.New("disposable PostgreSQL watchdog did not exit within 10s")
		}
		out, err := exec.Command("docker", "rm", "--force", container).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "No such container") {
			result = errors.Join(result, fmt.Errorf("docker rm %s: %w (%s)", container, err, strings.TrimSpace(string(out))))
		}
	})
	return result
}

func startEnvelopePostgres(ctx context.Context, allowedCPU string) (string, string, func() error, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "", "", nil, errors.New("docker is required to own and clean up the disposable PostgreSQL resource")
	}
	name := "agentray-data-envelope-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	password := uuid.NewString()
	dockerArgs := []string{"run", "--rm", "--name", name,
		"--cpuset-cpus", allowedCPU, "--memory", strconv.FormatInt(envelopePostgresMemory, 10), "--memory-swap", strconv.FormatInt(envelopePostgresMemory, 10), "--pids-limit", "128", "--cpu-shares", "2",
		"--publish", "127.0.0.1::5432", "--env", "POSTGRES_USER=envelope", "--env", "POSTGRES_PASSWORD=" + password, "--env", "POSTGRES_DB=envelope", "postgres:16-alpine"}
	watchdog, err := startEnvelopeDockerWatchdog(os.Getpid(), name, dockerArgs)
	if err != nil {
		return "", "", nil, err
	}
	cleanup := func() error { return watchdog.stop(name) }
	fail := func(err error) (string, string, func() error, error) { _ = cleanup(); return "", "", nil, err }
	var port string
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if exited, err := watchdog.exited(); exited {
			if err == nil {
				err = errors.New("watchdog exited")
			}
			return fail(fmt.Errorf("docker run postgres with required caps exited before readiness: %w", err))
		}
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
					return url, name, cleanup, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
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

func inspectEnvelopeHostIdentity(ctx context.Context, client *http.Client, baseURL string) (envelopeHostIdentity, error) {
	read := func(path string) (string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Metadata-Flavor", "Google")
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("read GCE metadata %s: %w", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Metadata-Flavor") != "Google" {
			return "", fmt.Errorf("read GCE metadata %s: status=%s metadata_flavor=%q", path, resp.Status, resp.Header.Get("Metadata-Flavor"))
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(body)), nil
	}
	project, err := read("/project/project-id")
	if err != nil {
		return envelopeHostIdentity{}, err
	}
	instance, err := read("/instance/name")
	if err != nil {
		return envelopeHostIdentity{}, err
	}
	zone, err := read("/instance/zone")
	if err != nil {
		return envelopeHostIdentity{}, err
	}
	if cut := strings.LastIndex(zone, "/"); cut >= 0 {
		zone = zone[cut+1:]
	}
	return envelopeHostIdentity{Project: project, Zone: zone, Instance: instance}, nil
}

func validateEnvelopeHostIdentity(identity envelopeHostIdentity) error {
	if identity.Project != envelopeRequiredGCEProject || identity.Zone != envelopeRequiredGCEZone || identity.Instance != envelopeRequiredGCEName {
		return fmt.Errorf("AC-DATA-03 envelope refused: GCE identity project=%q zone=%q instance=%q; require project=%q zone=%q instance=%q", identity.Project, identity.Zone, identity.Instance, envelopeRequiredGCEProject, envelopeRequiredGCEZone, envelopeRequiredGCEName)
	}
	return nil
}

func inspectEnvelopeRuntimeCaps() (envelopeRuntimeCaps, error) {
	body, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return envelopeRuntimeCaps{}, fmt.Errorf("inspect CPU affinity: %w", err)
	}
	var allowed string
	for _, line := range strings.Split(string(body), "\n") {
		if value, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
			allowed = strings.TrimSpace(value)
			break
		}
	}
	count, err := countCPUList(allowed)
	if err != nil {
		return envelopeRuntimeCaps{}, fmt.Errorf("inspect CPU affinity %q: %w", allowed, err)
	}
	nice, err := currentNiceValue("/proc/self/stat")
	if err != nil {
		return envelopeRuntimeCaps{}, err
	}
	return envelopeRuntimeCaps{AllowedCPUs: allowed, AllowedCPUCount: count, Nice: nice, MemoryMaxBytes: cgroupMemoryLimit(), InternalTimeout: envelopeRunTimeout}, nil
}

func validateEnvelopeRuntimeCaps(caps envelopeRuntimeCaps) error {
	var failures []string
	if caps.AllowedCPUCount != 1 {
		failures = append(failures, fmt.Sprintf("one allowed CPU required, got %q (%d CPUs)", caps.AllowedCPUs, caps.AllowedCPUCount))
	}
	if caps.Nice != 19 {
		failures = append(failures, fmt.Sprintf("nice=19 required, got %d", caps.Nice))
	}
	if caps.MemoryMaxBytes < envelopeMinMemoryLimit || caps.MemoryMaxBytes > envelopeMaxMemoryLimit {
		failures = append(failures, fmt.Sprintf("parent MemoryMax must be 2-2.5 GiB so parent plus %d-byte PostgreSQL stays within the 3 GiB aggregate ceiling; got parent=%d aggregate=%d bytes", envelopePostgresMemory, caps.MemoryMaxBytes, caps.MemoryMaxBytes+envelopePostgresMemory))
	}
	if caps.InternalTimeout > 60*time.Minute {
		failures = append(failures, fmt.Sprintf("timeout must be <=60m, got %s", caps.InternalTimeout))
	}
	return errors.Join(stringErrors(failures)...)
}

func validateEnvelopeRunWindow(now time.Time) error {
	// Ho Chi Minh City does not observe daylight saving time. A fixed zone keeps
	// this safety check independent of host timezone-database installation.
	hcm := now.In(time.FixedZone("Asia/Ho_Chi_Minh", 7*60*60))
	if hcm.Hour() < 1 || hcm.Hour() >= 6 {
		return fmt.Errorf("AC-DATA-03 envelope refused outside the authorized 01:00-06:00 HCM window; current HCM time is %s", hcm.Format(time.RFC3339))
	}
	return nil
}

func countCPUList(value string) (int, error) {
	if value == "" {
		return 0, errors.New("empty CPU list")
	}
	total := 0
	for _, part := range strings.Split(value, ",") {
		bounds := strings.SplitN(strings.TrimSpace(part), "-", 2)
		start, err := strconv.Atoi(bounds[0])
		if err != nil {
			return 0, err
		}
		end := start
		if len(bounds) == 2 {
			end, err = strconv.Atoi(bounds[1])
			if err != nil {
				return 0, err
			}
		}
		if start < 0 || end < start {
			return 0, fmt.Errorf("invalid range %q", part)
		}
		total += end - start + 1
	}
	return total, nil
}

func currentNiceValue(statPath string) (int, error) {
	body, err := os.ReadFile(statPath)
	if err != nil {
		return 0, fmt.Errorf("inspect nice value: %w", err)
	}
	end := strings.LastIndex(string(body), ") ")
	if end < 0 {
		return 0, errors.New("inspect nice value: malformed /proc stat")
	}
	fields := strings.Fields(string(body)[end+2:]) // starts at proc stat field 3
	if len(fields) <= 16 {
		return 0, errors.New("inspect nice value: incomplete /proc stat")
	}
	nice, err := strconv.Atoi(fields[16]) // field 19
	if err != nil {
		return 0, fmt.Errorf("inspect nice value: %w", err)
	}
	return nice, nil
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

func resourceEnvelopeSample(at time.Time, duckPath, brokerDir, postgresContainer string) envelopeResourceSample {
	rss, complete := currentOwnedRSSBytes(postgresContainer)
	return envelopeResourceSample{At: at.UTC(), RSSBytes: rss, RSSComplete: complete, DuckDBBytes: uint64(max(0, fileSize(duckPath))), WALBytes: uint64(max(0, fileSize(duckPath+".wal"))), SpillBytes: treeSize(filepath.Join(filepath.Dir(duckPath), "tmp")), BrokerBytes: treeSize(brokerDir), DiskFree: diskFree(filepath.Dir(duckPath))}
}

func currentOwnedRSSBytes(postgresContainer string) (uint64, bool) {
	out, err := exec.Command("docker", "inspect", "--format", "{{.State.Pid}}", postgresContainer).Output()
	if err != nil {
		return 0, false
	}
	postgresPID, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || postgresPID <= 0 {
		return 0, false
	}
	rss, err := processTreeRSSBytes("/proc", []int{os.Getpid(), postgresPID})
	return rss, err == nil
}

type envelopeProcessStat struct {
	PID, PPID int
	RSSBytes  uint64
}

func processTreeRSSBytes(procRoot string, roots []int) (uint64, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, err
	}
	stats := make(map[int]envelopeProcessStat)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		stat, err := readEnvelopeProcessStat(filepath.Join(procRoot, entry.Name(), "status"), pid)
		if err == nil {
			stats[pid] = stat
		}
	}
	owned := make(map[int]struct{}, len(roots))
	for _, root := range roots {
		if _, ok := stats[root]; !ok {
			return 0, fmt.Errorf("RSS root process %d was not readable", root)
		}
		owned[root] = struct{}{}
	}
	for changed := true; changed; {
		changed = false
		for pid, stat := range stats {
			if _, already := owned[pid]; already {
				continue
			}
			if _, parentOwned := owned[stat.PPID]; parentOwned {
				owned[pid] = struct{}{}
				changed = true
			}
		}
	}
	var total uint64
	for pid := range owned {
		total += stats[pid].RSSBytes
	}
	return total, nil
}

func readEnvelopeProcessStat(path string, pid int) (envelopeProcessStat, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return envelopeProcessStat{}, err
	}
	stat := envelopeProcessStat{PID: pid}
	var havePPID, haveRSS bool
	for _, line := range strings.Split(string(body), "\n") {
		if value, ok := strings.CutPrefix(line, "PPid:"); ok {
			stat.PPID, err = strconv.Atoi(strings.TrimSpace(value))
			havePPID = err == nil
		}
		if value, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			fields := strings.Fields(value)
			if len(fields) > 0 {
				var kib uint64
				kib, err = strconv.ParseUint(fields[0], 10, 64)
				if err == nil {
					stat.RSSBytes, haveRSS = kib*1024, true
				}
			}
		}
	}
	if !havePPID || !haveRSS {
		return envelopeProcessStat{}, fmt.Errorf("incomplete status for process %d", pid)
	}
	return stat, nil
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
	body, err := os.ReadFile(currentCgroupV2File("memory.max"))
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
	body, _ := os.ReadFile(currentCgroupV2File("memory.events"))
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

func currentCgroupV2File(name string) string {
	body, err := os.ReadFile("/proc/self/cgroup")
	if err == nil {
		for _, line := range strings.Split(string(body), "\n") {
			if path, ok := strings.CutPrefix(line, "0::"); ok {
				return filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(strings.TrimSpace(path), "/"), name)
			}
		}
	}
	return filepath.Join("/sys/fs/cgroup", name)
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
