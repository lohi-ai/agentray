package agentruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	store "github.com/lohi-ai/agentray/internal/dataplane/store"
)

type fakeSessionImageRows struct {
	artifacts map[string]store.AgentSpillArtifact
	saves     int
	failSave  bool
}

func (f *fakeSessionImageRows) SaveAgentSpill(_ context.Context, artifact store.AgentSpillArtifact) (int, error) {
	if f.failSave {
		return 0, errors.New("store unavailable")
	}
	if f.artifacts == nil {
		f.artifacts = make(map[string]store.AgentSpillArtifact)
	}
	f.saves++
	f.artifacts[artifact.Locator] = artifact
	return len(artifact.Content), nil
}

func (f *fakeSessionImageRows) AgentSpillWindowForSession(_ context.Context, locator, session string, offset, limit int) (store.AgentSpillWindow, error) {
	artifact, ok := f.artifacts[locator]
	if !ok || artifact.SessionKey != session {
		return store.AgentSpillWindow{}, store.ErrAgentSpillNotFound
	}
	data := []byte(artifact.Content)
	if offset > len(data) {
		offset = len(data)
	}
	end := min(len(data), offset+limit)
	return store.AgentSpillWindow{Content: append([]byte(nil), data[offset:end]...), Offset: offset, Total: len(data)}, nil
}

func largeSessionPNG() string {
	data := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 1024)...)
	return base64.StdEncoding.EncodeToString(data)
}

func TestSessionImagesExternalizeDeduplicateAndHydrate(t *testing.T) {
	data := largeSessionPNG()
	message := agentcore.Message{Role: agentcore.RoleTool, Name: "eval", ToolCallID: "c1", Content: "plot", ContentParts: []agentcore.ContentPart{{
		Type: agentcore.ContentPartImage, MIMEType: "image/png", Data: data,
	}}}
	original := agentcore.SessionEntry{
		Kind: agentcore.EntryCompaction, Message: &message,
		Retained: []agentcore.Message{message},
		Outcome:  &agentcore.ToolOutcomeRecord{Message: message, Extra: []agentcore.Message{message}},
	}
	rows := &fakeSessionImageRows{}
	stored := externalizeSessionEntryImages(context.Background(), rows, "session-a", original, make(map[string]bool))
	if rows.saves != 1 {
		t.Fatalf("duplicate image saves = %d, want 1", rows.saves)
	}
	if original.Message.ContentParts[0].Data != data || original.Message.ContentParts[0].DataRef != "" {
		t.Fatal("externalization mutated caller-owned entry")
	}
	if stored.Message.ContentParts[0].Data != "" || !strings.HasPrefix(stored.Message.ContentParts[0].DataRef, "spillimg_") {
		t.Fatalf("stored image was not externalized: %+v", stored.Message.ContentParts[0])
	}
	inlineJSON, _ := json.Marshal(original)
	storedJSON, _ := json.Marshal(stored)
	if len(storedJSON) >= len(inlineJSON)/2 {
		t.Fatalf("externalized checkpoint did not materially shrink: inline=%d stored=%d", len(inlineJSON), len(storedJSON))
	}

	hydrateSessionEntryImages(context.Background(), rows, "session-a", &stored, make(map[string]hydratedSessionImage))
	if !reflect.DeepEqual(stored, original) {
		t.Fatalf("hydrated entry differs from canonical input:\n got: %+v\nwant: %+v", stored, original)
	}
}

func TestSessionImageExternalizationFallsBackInlineOnSaveFailure(t *testing.T) {
	data := largeSessionPNG()
	message := agentcore.Message{Role: agentcore.RoleTool, ContentParts: []agentcore.ContentPart{{
		Type: agentcore.ContentPartImage, MIMEType: "image/png", Data: data,
	}}}
	original := agentcore.SessionEntry{Kind: agentcore.EntryMessage, Message: &message}
	stored := externalizeSessionEntryImages(context.Background(), &fakeSessionImageRows{failSave: true}, "session-a", original, make(map[string]bool))
	if stored.Message.ContentParts[0].Data != data || stored.Message.ContentParts[0].DataRef != "" {
		t.Fatalf("save failure lost inline image: %+v", stored.Message.ContentParts[0])
	}
}

func TestSessionImageExternalizationLeavesInvalidImageInline(t *testing.T) {
	data := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("not-a-png"), 128))
	message := agentcore.Message{Role: agentcore.RoleTool, ContentParts: []agentcore.ContentPart{{
		Type: agentcore.ContentPartImage, MIMEType: "image/png", Data: data,
	}}}
	rows := &fakeSessionImageRows{}
	stored := externalizeSessionEntryImages(context.Background(), rows, "session-a", agentcore.SessionEntry{
		Kind: agentcore.EntryMessage, Message: &message,
	}, make(map[string]bool))
	if rows.saves != 0 || stored.Message.ContentParts[0].Data != data || stored.Message.ContentParts[0].DataRef != "" {
		t.Fatalf("invalid image should remain inline and unsaved: saves=%d part=%+v", rows.saves, stored.Message.ContentParts[0])
	}
}

func TestSessionImageHydrationFailsClosedAcrossSessions(t *testing.T) {
	data := largeSessionPNG()
	message := agentcore.Message{Role: agentcore.RoleTool, Content: "plot", ContentParts: []agentcore.ContentPart{{
		Type: agentcore.ContentPartImage, MIMEType: "image/png", Data: data,
	}}}
	rows := &fakeSessionImageRows{}
	stored := externalizeSessionEntryImages(context.Background(), rows, "session-a", agentcore.SessionEntry{
		Kind: agentcore.EntryMessage, Message: &message,
	}, make(map[string]bool))
	hydrateSessionEntryImages(context.Background(), rows, "session-b", &stored, make(map[string]hydratedSessionImage))
	if len(stored.Message.ContentParts) != 0 || !strings.Contains(stored.Message.Content, "unavailable during session restore") {
		t.Fatalf("cross-session image reference did not fail closed: %+v", stored.Message)
	}
}

func TestSessionImageHydrationCachesByLocatorAndMIME(t *testing.T) {
	data := largeSessionPNG()
	message := agentcore.Message{Role: agentcore.RoleTool, ContentParts: []agentcore.ContentPart{{
		Type: agentcore.ContentPartImage, MIMEType: "image/png", Data: data,
	}}}
	rows := &fakeSessionImageRows{}
	stored := externalizeSessionEntryImages(context.Background(), rows, "session-a", agentcore.SessionEntry{
		Kind: agentcore.EntryMessage, Message: &message,
	}, make(map[string]bool))
	ref := stored.Message.ContentParts[0].DataRef
	stored.Message.ContentParts = append(stored.Message.ContentParts, agentcore.ContentPart{
		Type: agentcore.ContentPartImage, MIMEType: "image/jpeg", DataRef: ref,
	})

	hydrateSessionEntryImages(context.Background(), rows, "session-a", &stored, make(map[string]hydratedSessionImage))
	if len(stored.Message.ContentParts) != 1 || stored.Message.ContentParts[0].MIMEType != "image/png" ||
		!strings.Contains(stored.Message.Content, "1 persisted image attachment(s) unavailable") {
		t.Fatalf("conflicting MIME reused cached validation: %+v", stored.Message)
	}
}
