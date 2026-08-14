package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// Connection test uses the selected model's owning provider credentials
// (lite → B's host + key), not flash.
func TestConnectionTestUsesOwnerCredentials(t *testing.T) {
	var aAuth, bAuth []string
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aAuth = append(aAuth, r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"model-a"`) {
			t.Errorf("A received unexpected body %s", body)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bAuth = append(bAuth, r.Header.Get("x-api-key"))
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"model-b"`) {
			t.Errorf("B received unexpected body %s", body)
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`))
	}))
	defer srvB.Close()

	book := &storage.WorkspaceProviderBook{
		WorkspaceID: "ws",
		Providers: []storage.WorkspaceProviderRecord{
			{ID: "pa", Vendor: "openai", Name: "A", BaseURL: srvA.URL, APIKey: "key-a", HasKey: true},
			{ID: "pb", Vendor: "anthropic", Name: "B", BaseURL: srvB.URL, APIKey: "key-b", HasKey: true},
		},
		Sel: storage.WorkspaceTierSelection{
			FlashProviderID: "pa", FlashModel: "model-a",
			LiteProviderID: "pb", LiteModel: "model-b",
		},
	}
	ok, tiers := testBookConnections(context.Background(), book)
	if !ok {
		t.Fatalf("expected both tiers ok, got %+v", tiers)
	}
	if len(aAuth) != 1 || aAuth[0] != "Bearer key-a" {
		t.Fatalf("flash did not hit A with key-a: %v", aAuth)
	}
	if len(bAuth) != 1 || bAuth[0] != "key-b" {
		t.Fatalf("lite did not hit B with key-b: %v", bAuth)
	}
}

func TestCollectionFromBookListsStubIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"listed-only"}]}`))
	}))
	defer srv.Close()
	book := &storage.WorkspaceProviderBook{
		Providers: []storage.WorkspaceProviderRecord{
			{ID: "p1", Vendor: "openai", Name: "P", BaseURL: srv.URL, APIKey: "k"},
		},
	}
	col, err := collectionFromBook(book)
	if err != nil {
		t.Fatal(err)
	}
	// Default client can reach httptest.
	listed, err := col.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Models) != 1 || listed.Models[0].ID != "listed-only" || listed.Models[0].ProviderID != "p1" {
		t.Fatalf("listed = %+v", listed)
	}
}
