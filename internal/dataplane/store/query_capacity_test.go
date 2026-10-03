package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	"github.com/lohi-ai/agentray/internal/dataplane/querytest"
)

type capacityQuery struct {
	Name string
	SQL  func(projectIndex int) string
}

type capacitySample struct {
	Phase       string `json:"phase"`
	QueryClass  string `json:"query_class"`
	Tenant      int    `json:"tenant"`
	DurationNS  int64  `json:"duration_ns"`
	Success     bool   `json:"success"`
	RefusalKind string `json:"refusal_kind,omitempty"`
	ResultRows  int    `json:"result_rows,omitempty"`
}

type capacityPhaseStats struct {
	Phase       string  `json:"phase"`
	QueryClass  string  `json:"query_class"`
	Tenant      int     `json:"tenant"`
	Requests    int     `json:"requests"`
	Successes   int     `json:"successes"`
	Refusals    int     `json:"refusals"`
	SuccessRate float64 `json:"success_rate"`
	P95NS       int64   `json:"successful_p95_ns,omitempty"`
}

type capacityTenantProgress struct {
	Tenant            int   `json:"tenant"`
	QueryRequests     int64 `json:"query_requests"`
	QuerySuccesses    int64 `json:"query_successes"`
	QueryRefusals     int64 `json:"query_refusals"`
	AttemptedWrites   int64 `json:"attempted_writes"`
	AcceptedWrites    int64 `json:"accepted_writes"`
	WriteFailures     int64 `json:"write_failures"`
	RequiredWrites    int64 `json:"required_writes"`
	FinalEventRows    int64 `json:"final_event_rows"`
	ReconciledNewRows int64 `json:"reconciled_new_rows"`
}

type capacityResourceSnapshot struct {
	Phase           string `json:"phase"`
	At              string `json:"at"`
	ParentRSSBytes  int64  `json:"parent_rss_bytes"`
	ChildRSSBytes   int64  `json:"child_rss_bytes"`
	ChildCount      int    `json:"child_count"`
	ChildSpillBytes int64  `json:"child_spill_bytes"`
	MainDBBytes     int64  `json:"main_db_bytes"`
	MainSpillBytes  int64  `json:"main_spill_bytes"`
	Spawns          int64  `json:"spawns"`
	Reaped          int64  `json:"reaped"`
	PeakInFlight    int64  `json:"peak_in_flight"`
	CopiedRows      int64  `json:"copied_rows"`
	CopiedBytes     int64  `json:"copied_bytes"`
	AdmissionWaitNS int64  `json:"admission_wait_ns"`
	StartupCopyNS   int64  `json:"startup_copy_ns"`
	RefreshNS       int64  `json:"refresh_ns"`
	ExecutionNS     int64  `json:"execution_ns"`
}

type queryCapacityReport struct {
	Host               string                     `json:"host"`
	GOOS               string                     `json:"goos"`
	CPUs               int                        `json:"cpus"`
	Projects           int                        `json:"projects"`
	EventsPerProject   int                        `json:"events_per_project"`
	ExternalPerProject int                        `json:"external_rows_per_project"`
	Concurrency        int                        `json:"concurrency"`
	MixedDuration      time.Duration              `json:"mixed_duration_ns"`
	SeedDuration       time.Duration              `json:"seed_duration_ns"`
	QuerySHA256        map[string]string          `json:"query_sha256"`
	Samples            []capacitySample           `json:"samples"`
	PhaseStats         []capacityPhaseStats       `json:"phase_stats"`
	TenantProgress     []capacityTenantProgress   `json:"tenant_progress"`
	Resources          []capacityResourceSnapshot `json:"resources"`
	Plans              map[string][]string        `json:"plans"`
}

func capacityQueries() []capacityQuery {
	return []capacityQuery{
		{Name: "event_aggregate_62d", SQL: func(int) string {
			return `SELECT date_trunc('day', timestamp) AS date, count(*) AS value
FROM events
WHERE timestamp >= '2026-07-15T00:00:00Z' AND timestamp < '2026-09-15T00:00:00Z'
GROUP BY date ORDER BY date`
		}},
		{Name: "external_aggregate_62d", SQL: func(projectIndex int) string {
			return fmt.Sprintf(`SELECT date_trunc('day', try_cast(json_extract_string(data, '$.occurred_at') AS TIMESTAMPTZ)) AS date,
sum(try_cast(json_extract_string(data, '$.amount') AS BIGINT)) AS value
FROM external_rows
WHERE connector_id = '%s' AND table_name = 'capacity_facts'
  AND try_cast(json_extract_string(data, '$.occurred_at') AS TIMESTAMPTZ) >= '2026-07-15T00:00:00Z'
  AND try_cast(json_extract_string(data, '$.occurred_at') AS TIMESTAMPTZ) < '2026-09-15T00:00:00Z'
GROUP BY date ORDER BY date`, querytest.CapacityConnectorID(projectIndex))
		}},
		{Name: "cohort_62d", SQL: func(int) string {
			return `WITH people AS (
  SELECT canonical_id, min(timestamp) AS first_seen, max(timestamp) AS last_seen FROM events GROUP BY 1
)
SELECT date_trunc('day', first_seen) AS cohort_date,
       date_diff('day', first_seen, last_seen) AS age_days,
       count(*) AS people
FROM people
WHERE first_seen >= '2026-07-15T00:00:00Z' AND first_seen < '2026-09-15T00:00:00Z'
GROUP BY 1, 2 ORDER BY 1, 2`
		}},
		{Name: "lifetime_lookup", SQL: func(int) string {
			return `SELECT count(*) AS value, min(timestamp) AS first_seen, max(timestamp) AS last_seen
FROM events WHERE canonical_id = 'user-42'`
		}},
	}
}

func TestQueryCapacityHarnessCoversEveryWorkloadClassAndTenant(t *testing.T) {
	queries := capacityQueries()
	if len(queries) != 4 {
		t.Fatalf("capacity query classes = %d, want event/external/cohort/lifetime", len(queries))
	}
	seen := map[string]bool{}
	for tenant := range querytest.RequiredProjects {
		for _, queryClass := range queries {
			if seen[queryClass.Name] && tenant == 0 {
				t.Fatalf("duplicate query class %q", queryClass.Name)
			}
			seen[queryClass.Name] = true
			if _, _, err := scopedReadonlySQL(queryClass.SQL(tenant), querytest.CapacityProjectID(tenant), nil); err != nil {
				t.Errorf("tenant %d class %s is not valid run_sql: %v", tenant, queryClass.Name, err)
			}
		}
	}
	for _, name := range []string{"event_aggregate_62d", "external_aggregate_62d", "cohort_62d", "lifetime_lookup"} {
		if !seen[name] {
			t.Errorf("missing capacity query class %q", name)
		}
	}
}

func TestQueryCapacityStatsKeepRefusalsSeparateFromSuccessfulLatency(t *testing.T) {
	stats := summarizeCapacitySamples([]capacitySample{
		{Phase: "warm", QueryClass: "event", Tenant: 0, DurationNS: int64(time.Second), Success: true},
		{Phase: "warm", QueryClass: "event", Tenant: 0, DurationNS: int64(2 * time.Second), Success: true},
		{Phase: "mixed", QueryClass: "event", Tenant: 0, DurationNS: int64(30 * time.Second), RefusalKind: "unavailable"},
	})
	var warm, mixed capacityPhaseStats
	for _, stat := range stats {
		switch stat.Phase {
		case "warm":
			warm = stat
		case "mixed":
			mixed = stat
		}
	}
	if warm.SuccessRate != 1 || warm.P95NS != int64(2*time.Second) {
		t.Fatalf("warm success/latency corrupted by another phase's refusal: %+v", warm)
	}
	if mixed.Successes != 0 || mixed.Refusals != 1 || mixed.SuccessRate != 0 {
		t.Fatalf("mixed refusal not tracked separately: %+v", mixed)
	}
}

func TestQueryCapacityInstrumentationRecordsCopyAndPhaseCosts(t *testing.T) {
	d := openTestDuckDB(t)
	projectID := querytest.CapacityProjectID(0)
	if err := d.InsertEvents(t.Context(), []Event{{
		ProjectID: projectID, EventID: "70000000-0000-4000-8000-000000000001", DistinctID: "user-42",
		SessionID: "capacity-unit", EventName: "capacity.fact", EventType: "user", Properties: `{}`,
		Timestamp: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), VisitorClass: "human",
	}}); err != nil {
		t.Fatal(err)
	}
	pool := newSQLSandboxPool(d)
	defer pool.closeAll()
	sample := runCapacitySample(t.Context(), pool, "cold", 0, capacityQueries()[0])
	if !sample.Success {
		t.Fatalf("instrumented query refused: %+v", sample)
	}
	snapshot := capacityResources(pool, d, "unit")
	if snapshot.ChildCount != 1 || snapshot.CopiedRows == 0 || snapshot.CopiedBytes == 0 {
		t.Fatalf("copy/child evidence missing: %+v", snapshot)
	}
	if snapshot.StartupCopyNS == 0 || snapshot.RefreshNS == 0 || snapshot.ExecutionNS == 0 || snapshot.AdmissionWaitNS == 0 {
		t.Fatalf("phase timing evidence missing: %+v", snapshot)
	}
}

func TestQueryCapacityApprovedCorpus(t *testing.T) {
	if os.Getenv("AGENTRAY_QUERY_CAPACITY") != "1" {
		t.Skip("set AGENTRAY_QUERY_CAPACITY=1 on the declared disposable Linux host")
	}
	spec, err := querytest.CapacitySpecFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "linux" {
		t.Fatalf("capacity evidence requires Linux RLIMIT/cgroup behavior, got %s", runtime.GOOS)
	}

	d := openTestDuckDB(t)
	seedStarted := time.Now()
	const batchSize = 10_000
	for projectIndex := range spec.Projects {
		projectID := querytest.CapacityProjectID(projectIndex)
		for start := 0; start < spec.EventsPerProject; start += batchSize {
			count := min(batchSize, spec.EventsPerProject-start)
			generated := querytest.CapacityEvents(projectIndex, start, count)
			events := make([]Event, len(generated))
			for i, row := range generated {
				events[i] = Event{ProjectID: projectID, EventID: row.EventID, DistinctID: row.DistinctID,
					SessionID: "capacity", EventName: "capacity.fact", EventType: "user",
					Properties: `{"width":"representative","amount":12345}`, Timestamp: row.Timestamp, VisitorClass: "human"}
			}
			if err := d.InsertEvents(t.Context(), events); err != nil {
				t.Fatalf("seed events project=%d start=%d: %v", projectIndex, start, err)
			}
		}
		connectorID := querytest.CapacityConnectorID(projectIndex)
		for start := 0; start < spec.RowsPerProject; start += batchSize {
			count := min(batchSize, spec.RowsPerProject-start)
			generated := querytest.CapacityExternalRows(start, count)
			rows := make([]connector.LandedRow, len(generated))
			for i, row := range generated {
				rows[i] = connector.LandedRow{Key: row.Key, DataJSON: row.Data}
			}
			if err := d.InsertExternalRows(t.Context(), projectID, connectorID, "capacity_facts", rows, AppliedMark{}); err != nil {
				t.Fatalf("seed external rows project=%d start=%d: %v", projectIndex, start, err)
			}
		}
	}

	queries := capacityQueries()
	report := queryCapacityReport{
		Host: spec.HostDescription, GOOS: runtime.GOOS, CPUs: runtime.NumCPU(),
		Projects: spec.Projects, EventsPerProject: spec.EventsPerProject,
		ExternalPerProject: spec.RowsPerProject, Concurrency: spec.Concurrency,
		MixedDuration: spec.MixedDuration, SeedDuration: time.Since(seedStarted),
		QuerySHA256: map[string]string{}, Plans: map[string][]string{},
	}
	for _, queryClass := range queries {
		digest := sha256.Sum256([]byte(queryClass.SQL(0)))
		report.QuerySHA256[queryClass.Name] = hex.EncodeToString(digest[:])
	}

	// A fresh pool per sample makes cold duration include admission, process
	// startup, full tenant copy, and execution. Samples rotate across tenants.
	for _, queryClass := range queries {
		for sampleIndex := range querytest.RequiredColdSamples {
			tenant := sampleIndex % spec.Projects
			pool := newSQLSandboxPool(d)
			report.Samples = append(report.Samples, runCapacitySample(t.Context(), pool, "cold", tenant, queryClass))
			report.Resources = append(report.Resources, capacityResources(pool, d, "cold/"+queryClass.Name))
			pool.closeAll()
		}
	}

	// Keep one tenant resident at a time. A shared three-tenant pool has a
	// two-child LRU and would mislabel repeated cold copies as warm requests.
	for tenant := range spec.Projects {
		pool := newSQLSandboxPool(d)
		for _, queryClass := range queries {
			prime := runCapacitySample(t.Context(), pool, "warmup", tenant, queryClass)
			if !prime.Success {
				t.Fatalf("tenant %d %s warmup refused (%s)", tenant, queryClass.Name, prime.RefusalKind)
			}
			for range querytest.RequiredWarmSamples {
				report.Samples = append(report.Samples, runCapacitySample(t.Context(), pool, "warm", tenant, queryClass))
			}
		}
		report.Resources = append(report.Resources, capacityResources(pool, d, fmt.Sprintf("warm/tenant-%d", tenant)))
		pool.closeAll()
	}

	// Mixed load covers all classes and tenants while every tenant accepts
	// 100 events/sec. Refusals stay explicit and cannot disable warm p95 checks.
	mixedPool := newSQLSandboxPool(d)
	mixedCtx, cancel := context.WithTimeout(t.Context(), spec.MixedDuration)
	defer cancel()
	progress := make([]capacityTenantProgress, spec.Projects)
	for tenant := range progress {
		progress[tenant] = capacityTenantProgress{Tenant: tenant, RequiredWrites: int64(spec.MixedDuration/time.Second) * 100}
	}
	var sampleMu sync.Mutex
	var unexpected atomic.Int64
	var wg sync.WaitGroup
	for worker := range spec.Concurrency {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			tenant := worker % spec.Projects
			sequence := 0
			for mixedCtx.Err() == nil {
				queryClass := queries[(worker+sequence)%len(queries)]
				sample := runCapacitySample(mixedCtx, mixedPool, "mixed", tenant, queryClass)
				if mixedCtx.Err() != nil && !sample.Success {
					return
				}
				sampleMu.Lock()
				report.Samples = append(report.Samples, sample)
				progress[tenant].QueryRequests++
				if sample.Success {
					progress[tenant].QuerySuccesses++
				} else if sample.RefusalKind != "" {
					progress[tenant].QueryRefusals++
				} else {
					unexpected.Add(1)
				}
				sampleMu.Unlock()
				sequence++
			}
		}(worker)
	}
	for tenant := range spec.Projects {
		wg.Add(1)
		go func(tenant int) {
			defer wg.Done()
			runCapacityWriter(mixedCtx, d, tenant, &progress[tenant])
		}(tenant)
	}
	monitorDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		defer close(monitorDone)
		for {
			select {
			case <-mixedCtx.Done():
				return
			case <-ticker.C:
				snapshot := capacityResources(mixedPool, d, "mixed")
				sampleMu.Lock()
				report.Resources = append(report.Resources, snapshot)
				sampleMu.Unlock()
			}
		}
	}()
	wg.Wait()
	<-monitorDone
	report.Resources = append(report.Resources, capacityResources(mixedPool, d, "mixed/final"))
	mixedPool.closeAll()
	if unexpected.Load() != 0 {
		t.Errorf("mixed workload had %d unclassified query errors", unexpected.Load())
	}

	for tenant := range progress {
		progress[tenant].FinalEventRows = capacityEventCount(t, d, querytest.CapacityProjectID(tenant))
		progress[tenant].ReconciledNewRows = progress[tenant].FinalEventRows - int64(spec.EventsPerProject)
		if progress[tenant].WriteFailures != 0 {
			t.Errorf("tenant %d had %d failed ingest events", tenant, progress[tenant].WriteFailures)
		}
		if progress[tenant].AcceptedWrites < progress[tenant].RequiredWrites {
			t.Errorf("tenant %d accepted %d writes, want >= %d (100/sec)", tenant, progress[tenant].AcceptedWrites, progress[tenant].RequiredWrites)
		}
		if progress[tenant].ReconciledNewRows != progress[tenant].AcceptedWrites {
			t.Errorf("tenant %d accepted-write reconciliation = %d rows, want %d", tenant, progress[tenant].ReconciledNewRows, progress[tenant].AcceptedWrites)
		}
	}
	report.TenantProgress = progress
	report.PhaseStats = summarizeCapacitySamples(report.Samples)
	assertCapacityStats(t, report.PhaseStats, spec.Projects, queries)

	// Retain a trusted plan for every class without adding EXPLAIN to run_sql.
	for _, queryClass := range queries {
		query, args, err := scopedReadonlySQL(queryClass.SQL(0), querytest.CapacityProjectID(0), nil)
		if err != nil {
			t.Fatalf("scope %s plan query: %v", queryClass.Name, err)
		}
		if err := d.Read(t.Context(), func(conn *sql.Conn) error {
			rows, err := conn.QueryContext(t.Context(), "EXPLAIN "+query, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var kind, plan string
				if err := rows.Scan(&kind, &plan); err != nil {
					return err
				}
				report.Plans[queryClass.Name] = append(report.Plans[queryClass.Name], kind+": "+plan)
			}
			return rows.Err()
		}); err != nil {
			t.Errorf("explain %s: %v", queryClass.Name, err)
		}
	}

	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(spec.ArtifactDir, "query-capacity-store.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write capacity artifact: %v", err)
	}
	t.Logf("capacity artifact: %s (samples=%d)", path, len(report.Samples))
}

func runCapacitySample(ctx context.Context, pool *sqlSandboxPool, phase string, tenant int, queryClass capacityQuery) capacitySample {
	sample := capacitySample{Phase: phase, QueryClass: queryClass.Name, Tenant: tenant}
	query, args, err := scopedReadonlySQL(queryClass.SQL(tenant), querytest.CapacityProjectID(tenant), nil)
	if err != nil {
		sample.RefusalKind = "invalid_harness_sql: " + err.Error()
		return sample
	}
	started := time.Now()
	rows, err := pool.query(ctx, querytest.CapacityProjectID(tenant), query, args)
	sample.DurationNS = time.Since(started).Nanoseconds()
	if err == nil {
		sample.Success = true
		sample.ResultRows = len(rows)
		return sample
	}
	sample.RefusalKind = capacityRefusalKind(err)
	return sample
}

func capacityRefusalKind(err error) string {
	switch {
	case IsSandboxUnavailable(err):
		return "unavailable"
	case errors.Is(err, ErrSandboxTimeout):
		return "timeout"
	case IsEngineResourceError(err):
		return "resource"
	default:
		return ""
	}
}

func summarizeCapacitySamples(samples []capacitySample) []capacityPhaseStats {
	type key struct {
		phase, query string
		tenant       int
	}
	grouped := map[key][]capacitySample{}
	for _, sample := range samples {
		group := key{sample.Phase, sample.QueryClass, sample.Tenant}
		grouped[group] = append(grouped[group], sample)
	}
	stats := make([]capacityPhaseStats, 0, len(grouped))
	for group, samples := range grouped {
		stat := capacityPhaseStats{Phase: group.phase, QueryClass: group.query, Tenant: group.tenant, Requests: len(samples)}
		var successful []int64
		for _, sample := range samples {
			if sample.Success {
				stat.Successes++
				successful = append(successful, sample.DurationNS)
			} else if sample.RefusalKind != "" {
				stat.Refusals++
			}
		}
		stat.SuccessRate = float64(stat.Successes) / float64(stat.Requests)
		if len(successful) > 0 {
			sort.Slice(successful, func(i, j int) bool { return successful[i] < successful[j] })
			stat.P95NS = successful[(len(successful)*95-1)/100]
		}
		stats = append(stats, stat)
	}
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].Phase != stats[j].Phase {
			return stats[i].Phase < stats[j].Phase
		}
		if stats[i].Tenant != stats[j].Tenant {
			return stats[i].Tenant < stats[j].Tenant
		}
		return stats[i].QueryClass < stats[j].QueryClass
	})
	return stats
}

func assertCapacityStats(t *testing.T, stats []capacityPhaseStats, tenants int, queries []capacityQuery) {
	t.Helper()
	byKey := map[string]capacityPhaseStats{}
	for _, stat := range stats {
		byKey[fmt.Sprintf("%s/%d/%s", stat.Phase, stat.Tenant, stat.QueryClass)] = stat
	}
	for _, phase := range []string{"cold", "warm", "mixed"} {
		for tenant := range tenants {
			for _, queryClass := range queries {
				stat, ok := byKey[fmt.Sprintf("%s/%d/%s", phase, tenant, queryClass.Name)]
				if !ok || stat.Requests == 0 {
					t.Errorf("missing %s samples for tenant %d class %s", phase, tenant, queryClass.Name)
					continue
				}
				if stat.Successes+stat.Refusals != stat.Requests {
					t.Errorf("%s tenant %d class %s has unclassified outcomes: %+v", phase, tenant, queryClass.Name, stat)
				}
				if phase == "cold" || phase == "warm" {
					if stat.SuccessRate != 1 {
						t.Errorf("%s tenant %d class %s success rate %.3f, want 1.0", phase, tenant, queryClass.Name, stat.SuccessRate)
					}
				} else if stat.SuccessRate <= 0 {
					t.Errorf("mixed tenant %d class %s had no successful requests (refusals=%d)", tenant, queryClass.Name, stat.Refusals)
				}
				budget := 30 * time.Second
				if phase == "warm" {
					budget = 5 * time.Second
				}
				if stat.Successes > 0 && time.Duration(stat.P95NS) > budget {
					t.Errorf("%s tenant %d class %s successful p95 %s exceeds %s", phase, tenant, queryClass.Name, time.Duration(stat.P95NS), budget)
				}
			}
		}
	}
}

func runCapacityWriter(ctx context.Context, d *DuckDB, tenant int, progress *capacityTenantProgress) {
	const eventsPerTick = 100
	sequence := 0
	write := func(at time.Time) bool {
		events := make([]Event, eventsPerTick)
		for i := range events {
			events[i] = Event{ProjectID: querytest.CapacityProjectID(tenant), EventID: fmt.Sprintf("live-%d-%d", tenant, sequence),
				DistinctID: fmt.Sprintf("live-writer-%d", tenant), SessionID: "capacity-writer", EventName: "capacity.live",
				EventType: "system", Properties: `{}`, Timestamp: at.UTC(), VisitorClass: "human"}
			sequence++
		}
		atomic.AddInt64(&progress.AttemptedWrites, eventsPerTick)
		if err := d.InsertEvents(ctx, events); err != nil {
			if ctx.Err() == nil {
				atomic.AddInt64(&progress.WriteFailures, eventsPerTick)
			}
			return false
		}
		atomic.AddInt64(&progress.AcceptedWrites, eventsPerTick)
		return true
	}
	if !write(time.Now()) {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-ticker.C:
			if !write(at) {
				return
			}
		}
	}
}

func capacityEventCount(t *testing.T, d *DuckDB, projectID string) int64 {
	t.Helper()
	var count int64
	if err := d.Read(t.Context(), func(conn *sql.Conn) error {
		return conn.QueryRowContext(t.Context(), `SELECT count(*) FROM events WHERE project_id = ?`, projectID).Scan(&count)
	}); err != nil {
		t.Fatalf("count tenant events: %v", err)
	}
	return count
}

func capacityResources(pool *sqlSandboxPool, d *DuckDB, phase string) capacityResourceSnapshot {
	snapshot := capacityResourceSnapshot{
		Phase: phase, At: time.Now().UTC().Format(time.RFC3339Nano), ParentRSSBytes: procRSSBytes(os.Getpid()),
		Spawns: pool.spawns.Load(), Reaped: pool.reaped.Load(), PeakInFlight: pool.peakInFlight.Load(),
		CopiedRows: pool.copiedRows.Load(), CopiedBytes: pool.copiedBytes.Load(),
		AdmissionWaitNS: pool.admissionWaitNanos.Load(), StartupCopyNS: pool.startupNanos.Load(),
		RefreshNS: pool.refreshNanos.Load(), ExecutionNS: pool.executionNanos.Load(),
	}
	pool.mu.Lock()
	sandboxes := make([]*sqlSandbox, 0, len(pool.sandboxes))
	for _, sandbox := range pool.sandboxes {
		sandboxes = append(sandboxes, sandbox)
	}
	pool.mu.Unlock()
	snapshot.ChildCount = len(sandboxes)
	for _, sandbox := range sandboxes {
		if sandbox.cmd != nil && sandbox.cmd.Process != nil {
			snapshot.ChildRSSBytes += procRSSBytes(sandbox.cmd.Process.Pid)
		}
		snapshot.ChildSpillBytes += treeBytes(sandbox.tmpDir)
	}
	snapshot.MainDBBytes = fileBytes(d.path) + fileBytes(d.path+".wal")
	snapshot.MainSpillBytes = treeBytes(filepath.Join(filepath.Dir(d.path), "tmp"))
	return snapshot
}

func procRSSBytes(pid int) int64 {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		var kib int64
		if _, err := fmt.Sscanf(line, "VmRSS: %d kB", &kib); err == nil {
			return kib * 1024
		}
	}
	return 0
}

func fileBytes(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func treeBytes(root string) int64 {
	if root == "" {
		return 0
	}
	var total int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}
