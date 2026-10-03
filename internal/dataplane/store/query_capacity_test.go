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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	"github.com/lohi-ai/agentray/internal/dataplane/querytest"
)

type queryCapacityReport struct {
	Host               string        `json:"host"`
	GOOS               string        `json:"goos"`
	CPUs               int           `json:"cpus"`
	Projects           int           `json:"projects"`
	EventsPerProject   int           `json:"events_per_project"`
	ExternalPerProject int           `json:"external_rows_per_project"`
	Concurrency        int           `json:"concurrency"`
	MixedDuration      time.Duration `json:"mixed_duration_ns"`
	SeedDuration       time.Duration `json:"seed_duration_ns"`
	QuerySHA256        string        `json:"query_sha256"`
	Cold               []int64       `json:"cold_duration_ns"`
	Warm               []int64       `json:"warm_duration_ns"`
	Refusals           int64         `json:"refusals"`
	MixedRequests      int64         `json:"mixed_requests"`
	AcceptedWrites     int64         `json:"accepted_writes"`
	Plan               []string      `json:"plan"`
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

	const sqlText = `SELECT date_trunc('day', timestamp) AS date, count(*) AS value
FROM events
WHERE timestamp >= '2026-07-15T00:00:00Z' AND timestamp < '2026-09-15T00:00:00Z'
GROUP BY date ORDER BY date`
	digest := sha256.Sum256([]byte(sqlText))
	report := queryCapacityReport{
		Host: spec.HostDescription, GOOS: runtime.GOOS, CPUs: runtime.NumCPU(),
		Projects: spec.Projects, EventsPerProject: spec.EventsPerProject,
		ExternalPerProject: spec.RowsPerProject, Concurrency: spec.Concurrency,
		MixedDuration: spec.MixedDuration, SeedDuration: time.Since(seedStarted),
		QuerySHA256: hex.EncodeToString(digest[:]),
	}
	query, args, err := scopedReadonlySQL(sqlText, querytest.CapacityProjectID(0), nil)
	if err != nil {
		t.Fatal(err)
	}
	bounded := func(err error) bool {
		return IsSandboxUnavailable(err) || errors.Is(err, ErrSandboxTimeout) || IsEngineResourceError(err)
	}

	for range querytest.RequiredColdSamples {
		pool := newSQLSandboxPool(d)
		started := time.Now()
		_, err := pool.query(t.Context(), querytest.CapacityProjectID(0), query, args)
		elapsed := time.Since(started)
		report.Cold = append(report.Cold, elapsed.Nanoseconds())
		pool.closeAll()
		if err != nil {
			if !bounded(err) {
				t.Fatalf("unbounded cold failure: %v", err)
			}
			report.Refusals++
		} else if elapsed > 30*time.Second {
			t.Errorf("successful cold request %s exceeds 30s", elapsed)
		}
	}

	warmPool := newSQLSandboxPool(d)
	t.Cleanup(warmPool.closeAll)
	for range querytest.RequiredWarmSamples {
		started := time.Now()
		_, err := warmPool.query(t.Context(), querytest.CapacityProjectID(0), query, args)
		report.Warm = append(report.Warm, time.Since(started).Nanoseconds())
		if err != nil {
			if !bounded(err) {
				t.Fatalf("unbounded warm failure: %v", err)
			}
			report.Refusals++
		}
	}

	mixedCtx, cancel := context.WithTimeout(t.Context(), spec.MixedDuration)
	defer cancel()
	var requests, refusals, writes atomic.Int64
	var wg sync.WaitGroup
	for worker := range spec.Concurrency {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for mixedCtx.Err() == nil {
				projectID := querytest.CapacityProjectID(worker % spec.Projects)
				q, a, scopeErr := scopedReadonlySQL(sqlText, projectID, nil)
				if scopeErr != nil {
					t.Errorf("scope mixed query: %v", scopeErr)
					return
				}
				_, queryErr := warmPool.query(mixedCtx, projectID, q, a)
				requests.Add(1)
				if queryErr != nil && bounded(queryErr) {
					refusals.Add(1)
				} else if queryErr != nil && mixedCtx.Err() == nil {
					t.Errorf("mixed query: %v", queryErr)
					return
				}
			}
		}(worker)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		sequence := 0
		for {
			select {
			case <-mixedCtx.Done():
				return
			case at := <-ticker.C:
				events := make([]Event, 10)
				for i := range events {
					events[i] = Event{ProjectID: querytest.CapacityProjectID(0), EventID: fmt.Sprintf("live-%d", sequence),
						DistinctID: "live-writer", SessionID: "capacity-writer", EventName: "capacity.live",
						EventType: "system", Properties: `{}`, Timestamp: at.UTC(), VisitorClass: "human"}
					sequence++
				}
				if err := d.InsertEvents(mixedCtx, events); err == nil {
					writes.Add(int64(len(events)))
				} else if mixedCtx.Err() == nil {
					t.Errorf("concurrent writer: %v", err)
					return
				}
			}
		}
	}()
	wg.Wait()
	report.MixedRequests, report.AcceptedWrites = requests.Load(), writes.Load()
	report.Refusals += refusals.Load()
	if report.AcceptedWrites == 0 {
		t.Fatal("no concurrent ingest write committed")
	}

	// Preserve a trusted EXPLAIN of the same tenant predicate; run_sql remains
	// SELECT-only and is never relaxed to admit profiling statements.
	_ = d.Read(t.Context(), func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(t.Context(), `EXPLAIN SELECT date_trunc('day', timestamp), count(*) FROM events WHERE project_id = ? AND timestamp >= '2026-07-15T00:00:00Z' AND timestamp < '2026-09-15T00:00:00Z' GROUP BY 1`, querytest.CapacityProjectID(0))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind, plan string
			if err := rows.Scan(&kind, &plan); err != nil {
				return err
			}
			report.Plan = append(report.Plan, kind+": "+plan)
		}
		return rows.Err()
	})

	if len(report.Warm) > 0 && report.Refusals == 0 {
		sorted := append([]int64(nil), report.Warm...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		p95 := time.Duration(sorted[(len(sorted)*95-1)/100])
		if p95 > 5*time.Second {
			t.Errorf("warm p95 %s exceeds 5s", p95)
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
	t.Logf("capacity artifact: %s (refusals=%d)", path, report.Refusals)
}
