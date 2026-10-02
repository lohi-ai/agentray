package ingestion

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"
)

// Exercise the shipped production config against another environment on a real
// shared broker. Dev is retired; the second environment is an isolation fixture.
func TestDeployedEnvironmentsKeepSeparateStreams(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "../../..")
	broker := startBroker(t)
	raw, err := os.ReadFile(filepath.Join(root, "2server/api.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	read := func(key string) string {
		matches := regexp.MustCompile(`(?m)^\s+`+key+`: (\S+)\s*$`).FindAllStringSubmatch(string(raw), -1)
		if len(matches) != 1 {
			t.Fatalf("production config must declare %s once for both colours", key)
		}
		return matches[0][1]
	}
	var colours []*colour
	for _, env := range []string{"prod", "dev"} {
		cfg := testConfig("agentray-ingestors-" + env + "-blue")
		cfg.IngestStreamName = read("INGEST_STREAM_NAME")
		cfg.IngestDLQSubject = read("INGEST_DLQ_SUBJECT")
		cfg.IngestSubject = read("INGEST_SUBJECT")
		if env == "dev" {
			cfg.IngestStreamName += "_TEST_OTHER"
			cfg.IngestDLQSubject += ".test-other"
			cfg.IngestSubject += ".test-other"
		}
		cfg.IngestConnectorSubject = cfg.IngestSubject + ".connectors"
		c := newColour(t, broker, cfg)
		c.serve(t)
		colours = append(colours, c)
	}
	waitApplied := func(c *colour) {
		// Readiness permits an in-flight write batch; wait for the data itself
		// before asserting isolation, rather than treating ready as a flush.
		deadline := time.Now().Add(20 * time.Second)
		for len(c.eventIDs(t)) != 2 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if got := len(c.eventIDs(t)); got != 2 {
			t.Fatalf("environment has %d events, want 2", got)
		}
	}
	publishParity(t, colours[0])
	waitApplied(colours[0])
	colours[0].waitReady(t, 20*time.Second)
	colours[1].waitReady(t, 20*time.Second)
	if got := len(colours[1].eventIDs(t)); got != 0 {
		t.Fatalf("dev received %d prod events", got)
	}
	publishParity(t, colours[1])
	for _, c := range colours {
		waitApplied(c)
		c.waitReady(t, 20*time.Second)
		v, err := c.ss.ReplayStatus(context.Background())
		if err != nil || !v.Ready {
			t.Fatalf("environment was disconnected: %+v %v", v, err)
		}
	}
}
