package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	"github.com/lohi-ai/agentray/internal/dataplane/querytest"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/config"
)

func TestQueryCapacityHTTPRefusalEnvelope(t *testing.T) {
	if os.Getenv("AGENTRAY_QUERY_CAPACITY") != "1" {
		t.Skip("set AGENTRAY_QUERY_CAPACITY=1 on the declared disposable Linux host")
	}
	spec, err := querytest.CapacitySpecFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "linux" {
		t.Fatalf("capacity evidence requires Linux, got %s", runtime.GOOS)
	}
	pgURL := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if pgURL == "" {
		t.Fatal("AGENTRAY_TEST_DATABASE_URL is required when AGENTRAY_QUERY_CAPACITY=1")
	}
	s, err := storage.Open(t.Context(), config.Config{
		PostgresURL: pgURL, DuckDBPath: filepath.Join(t.TempDir(), "capacity.duckdb"),
		DefaultProjectName: "capacity", DefaultProjectAPIKey: "capacity-default",
	})
	if err != nil {
		t.Fatalf("open capacity store: %v", err)
	}
	t.Cleanup(s.Close)

	type caller struct {
		project storage.Project
		secret  string
	}
	callers := make([]caller, 0, spec.Concurrency)
	const batchSize = 10_000
	seedStarted := time.Now()
	for projectIndex := range spec.Projects {
		account, err := s.CreateAccount(t.Context(), fmt.Sprintf("capacity-%d-%d@test.local", time.Now().UnixNano(), projectIndex), "Capacity", "password-123", "capacity", fmt.Sprintf("project-%d", projectIndex))
		if err != nil {
			t.Fatal(err)
		}
		_, secret, err := s.CreateProjectCredential(t.Context(), account.User.ID, account.Project.ID, "investigator", []string{"analytics:read", "sources:read"})
		if err != nil {
			t.Fatal(err)
		}
		callers = append(callers, caller{project: account.Project, secret: secret})
		if projectIndex == 0 {
			_, extra, err := s.CreateProjectCredential(t.Context(), account.User.ID, account.Project.ID, "investigator-2", []string{"analytics:read", "sources:read"})
			if err != nil {
				t.Fatal(err)
			}
			callers = append(callers, caller{project: account.Project, secret: extra})
		}
		for start := 0; start < spec.EventsPerProject; start += batchSize {
			generated := querytest.CapacityEvents(projectIndex, start, min(batchSize, spec.EventsPerProject-start))
			events := make([]storage.Event, len(generated))
			for i, row := range generated {
				events[i] = storage.Event{ProjectID: account.Project.ID, EventID: row.EventID, DistinctID: row.DistinctID,
					SessionID: "capacity", EventName: "capacity.fact", EventType: "user",
					Properties: `{"width":"representative","amount":12345}`, Timestamp: row.Timestamp, VisitorClass: "human"}
			}
			if err := s.InsertEvents(t.Context(), events); err != nil {
				t.Fatalf("seed events project=%d start=%d: %v", projectIndex, start, err)
			}
		}
		for start := 0; start < spec.RowsPerProject; start += batchSize {
			generated := querytest.CapacityExternalRows(start, min(batchSize, spec.RowsPerProject-start))
			rows := make([]connector.LandedRow, len(generated))
			for i, row := range generated {
				rows[i] = connector.LandedRow{Key: row.Key, DataJSON: row.Data}
			}
			if err := s.InsertExternalRows(t.Context(), account.Project.ID, querytest.CapacityConnectorID(projectIndex), "capacity_facts", rows, storage.AppliedMark{}); err != nil {
				t.Fatalf("seed external project=%d start=%d: %v", projectIndex, start, err)
			}
		}
	}

	e := mountServerRoutes(t, s)
	registerOpRoutes(e, s, nil, nil)
	const query = `SELECT date_trunc('day', timestamp) AS date, count(*) AS value FROM events WHERE timestamp >= '2026-07-15T00:00:00Z' AND timestamp < '2026-09-15T00:00:00Z' GROUP BY date ORDER BY date`
	type outcome struct {
		Status     int           `json:"status"`
		Duration   time.Duration `json:"duration_ns"`
		RetryAfter string        `json:"retry_after,omitempty"`
	}
	outcomes := make([]outcome, spec.Concurrency)
	var wg sync.WaitGroup
	for i := range spec.Concurrency {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			caller := callers[index%len(callers)]
			req := httptest.NewRequest(http.MethodPost, "/api/sql/run?project_id="+caller.project.ID, strings.NewReader(fmt.Sprintf(`{"sql":%q}`, query)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+caller.secret)
			rec := httptest.NewRecorder()
			started := time.Now()
			e.ServeHTTP(rec, req)
			outcomes[index] = outcome{Status: rec.Code, Duration: time.Since(started), RetryAfter: rec.Header().Get("Retry-After")}
		}(i)
	}
	wg.Wait()
	for _, outcome := range outcomes {
		switch outcome.Status {
		case http.StatusOK:
			if outcome.Duration > 30*time.Second {
				t.Errorf("successful HTTP query exceeded cold budget: %s", outcome.Duration)
			}
		case http.StatusServiceUnavailable:
			if outcome.RetryAfter == "" {
				t.Error("bounded capacity refusal omitted Retry-After")
			}
		default:
			t.Errorf("HTTP query returned unclassified status %d", outcome.Status)
		}
	}
	report := map[string]any{
		"host": spec.HostDescription, "goos": runtime.GOOS, "projects": spec.Projects,
		"events_per_project": spec.EventsPerProject, "external_rows_per_project": spec.RowsPerProject,
		"concurrency": spec.Concurrency, "seed_duration_ns": time.Since(seedStarted).Nanoseconds(), "outcomes": outcomes,
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(spec.ArtifactDir, "query-capacity-http.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("capacity HTTP artifact: %s", path)
}
