package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore/engine"
	"github.com/lohi-ai/agentray/ai"
)

type proxyTransport func(*http.Request) (*http.Response, error)

func (f proxyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProxyPiFixtures(t *testing.T) {
	data, err := os.ReadFile("testdata/pi-proxy.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Model json.RawMessage
		Now   int64
		Cases []struct {
			Input struct {
				Name, Body, StatusText string
				Status                 int
				Options                map[string]any
			}
			Expected json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixtures.Cases {
		t.Run(test.Input.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var request any
			client := &http.Client{Transport: proxyTransport(func(r *http.Request) (*http.Response, error) {
				var body any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					return nil, err
				}
				request = map[string]any{"url": r.URL.String(), "method": r.Method, "headers": map[string]string{"Authorization": r.Header.Get("Authorization"), "Content-Type": r.Header.Get("Content-Type")}, "body": body}
				status := test.Input.Status
				if status == 0 {
					status = 200
				}
				return &http.Response{StatusCode: status, Status: statusLine(status, test.Input.StatusText), Body: io.NopCloser(strings.NewReader(test.Input.Body)), Header: make(http.Header), Request: r}, nil
			})}
			stream := engine.StreamProxy(ctx, fixtures.Model, ai.NormalizeContext(ai.Context{}), engine.ProxyStreamOptions{ProxyURL: "https://proxy.example.com", AuthToken: "test-token", Client: client, Now: func() int64 { return fixtures.Now }, Options: test.Input.Options})
			events := []ai.AssistantMessageEvent{}
			for {
				event, ok, err := stream.Next(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if !ok {
					break
				}
				events = append(events, event)
			}
			result, err := stream.Result(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.WaitForEnd(ctx); err != nil {
				t.Fatal(err)
			}
			actual, err := json.Marshal(map[string]any{"events": events, "result": result, "request": request})
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(actual, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(test.Expected, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("proxy differs from Pi\ngot: %s\nwant: %s", actual, test.Expected)
			}
			// Engine consumers use isolated snapshots. The representation must
			// retain every field of the settled live payload, including nulls.
			snapshots := make([]ai.AssistantMessageEvent, len(events))
			for i, event := range events {
				snapshots[i], err = stream.SnapshotEvent(event)
				if err != nil {
					t.Fatal(err)
				}
			}
			final, err := stream.SnapshotResult(ctx)
			if err != nil {
				t.Fatal(err)
			}
			copied, err := json.Marshal(map[string]any{"events": snapshots, "result": final, "request": request})
			if err != nil || !bytes.Equal(actual, copied) {
				t.Fatalf("snapshot changed settled payload: %v\nlive: %s\nsnapshot: %s", err, actual, copied)
			}
		})
	}
}

func statusLine(code int, text string) string {
	if text == "" {
		text = http.StatusText(code)
	}
	return fmt.Sprintf("%d %s", code, text)
}

func TestProxyCancellationAndLiveSnapshots(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"start\"}\n\ndata: {\"type\":\"text_start\",\"contentIndex\":0}\n\n")
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wait, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	stream := engine.StreamProxy(ctx, json.RawMessage(`{"id":"m","provider":"p","api":"a"}`), ai.NormalizeContext(ai.Context{}), engine.ProxyStreamOptions{ProxyURL: server.URL})
	<-entered
	first, ok, err := stream.Next(wait)
	if err != nil || !ok {
		t.Fatalf("start: %v %v", ok, err)
	}
	if first.Partial == nil {
		t.Fatal("missing live partial")
	}
	_, ok, err = stream.Next(wait)
	if err != nil || !ok {
		t.Fatalf("text_start: %v %v", ok, err)
	}
	snapshot, err := stream.SnapshotEvent(first)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := stream.WaitForEnd(wait); err != nil {
		t.Fatal(err)
	}
	result, err := stream.Result(wait)
	if err != nil {
		t.Fatal(err)
	}
	if first.Partial != result || result.StopReason != "aborted" || result.ErrorMessage == nil || *result.ErrorMessage != "Request aborted by user" {
		t.Fatalf("abort lost identity/reason: %+v", result)
	}
	if snapshot.Partial.StopReason != "pending" {
		t.Fatal("snapshot changed with live payload")
	}
}

func TestProxyConcurrentEngineSnapshots(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"start\"}\ndata: {\"type\":\"text_start\",\"contentIndex\":0}\n")
		for i := 0; i < 100; i++ {
			io.WriteString(w, "data: {\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"x\"}\n")
			w.(http.Flusher).Flush()
		}
		io.WriteString(w, "data: {\"type\":\"done\",\"reason\":\"stop\",\"usage\":{}}\n")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	var snapshots []*ai.Message
	messages, err := engine.Run(ctx, []ai.Message{{Role: "user", Content: ai.TextContent("test")}}, engine.Context{}, engine.Config{ConvertToLLM: func(messages []ai.Message) ([]ai.Message, error) { return messages, nil }, Model: json.RawMessage(`{"id":"m","provider":"p","api":"a"}`)}, func(e engine.Event) error {
		if e.Message != nil {
			mu.Lock()
			snapshots = append(snapshots, e.Message)
			mu.Unlock()
		}
		return nil
	}, func(ctx context.Context, m json.RawMessage, c ai.TranscriptContext, _ map[string]any) (*ai.AssistantMessageEventStream, error) {
		return engine.StreamProxy(ctx, m, c, engine.ProxyStreamOptions{ProxyURL: server.URL}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	last := messages[len(messages)-1]
	if last.Content.Blocks[0].Text != strings.Repeat("x", 100) {
		t.Fatalf("lost deltas: %+v", last)
	}
	// Consumer-owned snapshots are safe to retain and serialize independently.
	if _, err := json.Marshal(snapshots); err != nil {
		t.Fatal(err)
	}
}

// One-byte reads split every UTF-8 sequence and every protocol delimiter.
type proxyByteReader struct{ data []byte }

func (r *proxyByteReader) Read(dst []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	if len(dst) == 0 {
		return 0, nil
	}
	dst[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}
func (*proxyByteReader) Close() error { return nil }

func TestProxyFragmentedUTF8AndLongFrames(t *testing.T) {
	text := strings.Repeat("Chào 🌏", 10000)
	delta, _ := json.Marshal(map[string]any{"type": "text_delta", "contentIndex": 0, "delta": text})
	body := "\uFEFFdata: {\"type\":\"start\"}\r\ndata: {\"type\":\"text_start\",\"contentIndex\":0}\r\ndata: " + string(delta) + "\r\ndata: {\"type\":\"done\",\"reason\":\"stop\",\"usage\":{}}"
	client := &http.Client{Transport: proxyTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: &proxyByteReader{data: []byte(body)}, Header: make(http.Header), Request: r}, nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := engine.StreamProxy(ctx, json.RawMessage(`{"id":"m"}`), ai.NormalizeContext(ai.Context{}), engine.ProxyStreamOptions{ProxyURL: "https://proxy.example.com", Client: client})
	result, err := stream.SnapshotResult(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != "stop" || len(result.Content.Blocks) != 1 || result.Content.Blocks[0].Text != text {
		t.Fatal("fragmented/long frame corrupted")
	}
	if err := stream.WaitForEnd(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestProxyMalformedJSONSettles(t *testing.T) {
	client := &http.Client{Transport: proxyTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader("data: {invalid}\n")), Header: make(http.Header), Request: r}, nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := engine.StreamProxy(ctx, json.RawMessage(`{"id":"m"}`), ai.NormalizeContext(ai.Context{}), engine.ProxyStreamOptions{ProxyURL: "https://proxy.example.com", Client: client})
	result, err := stream.SnapshotResult(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != "error" || result.ErrorMessage == nil {
		t.Fatalf("malformed JSON did not settle: %+v", result)
	}
	if err := stream.WaitForEnd(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestProxyDrainsAfterTerminal(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"start\"}\ndata: {\"type\":\"text_start\",\"contentIndex\":0}\ndata: {\"type\":\"done\",\"reason\":\"stop\",\"usage\":{}}\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: {\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"after terminal\"}\n")
	}))
	defer server.Close()
	defer finish()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := engine.StreamProxy(ctx, json.RawMessage(`{"id":"m"}`), ai.NormalizeContext(ai.Context{}), engine.ProxyStreamOptions{ProxyURL: server.URL})
	raw, err := stream.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before, err := stream.SnapshotResult(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Content.Blocks[0].Text != "" {
		t.Fatal("server passed the gate")
	}
	finish()
	if err := stream.WaitForEnd(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := stream.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if raw != after || after.Content.Blocks[0].Text != "after terminal" || before.Content.Blocks[0].Text != "" {
		t.Fatal("live result stopped updating at terminal")
	}
	events := []string{}
	for {
		event, ok, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		events = append(events, event.Type)
	}
	if !reflect.DeepEqual(events, []string{"start", "text_start", "done"}) {
		t.Fatalf("post-terminal event leaked: %v", events)
	}
}
