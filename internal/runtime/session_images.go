package agentruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"github.com/lohi-ai/agentray/agentcore"
	store "github.com/lohi-ai/agentray/internal/dataplane/store"
)

const (
	// Tiny images cost less inline than another row and lookup. OMP uses the
	// same 1 KiB persistence threshold for its content-addressed blob store.
	persistedImageExternalizeThreshold = 1024
	persistedImageMaxDecodedBytes      = 768 * 1024
	persistedImageMaxBase64Chars       = 4 * ((persistedImageMaxDecodedBytes + 2) / 3)
)

type sessionImageRows interface {
	SaveAgentSpill(context.Context, store.AgentSpillArtifact) (int, error)
	AgentSpillWindowForSession(context.Context, string, string, int, int) (store.AgentSpillWindow, error)
}

// externalizeSessionEntryImages replaces large inline base64 payloads in the
// PostgreSQL representation with session-fenced, content-addressed references.
// The caller's entry is never mutated. A save failure retains the inline data:
// storage optimization must not turn a valid durability append into data loss.
func externalizeSessionEntryImages(ctx context.Context, rows sessionImageRows, sessionID string, entry agentcore.SessionEntry, saved map[string]bool) agentcore.SessionEntry {
	if !entryHasExternalizableImages(entry) {
		return entry
	}
	out := cloneEntryImageMessages(entry)
	visitEntryMessages(&out, func(message *agentcore.Message) {
		parts := message.ContentParts
		for i := range parts {
			part := &parts[i]
			if part.Type != agentcore.ContentPartImage || len(part.Data) < persistedImageExternalizeThreshold {
				continue
			}
			decoded, err := base64.StdEncoding.DecodeString(part.Data)
			if err != nil || len(decoded) == 0 || len(decoded) > persistedImageMaxDecodedBytes ||
				!validPersistedImageSignature(part.MIMEType, decoded) {
				continue
			}
			locator := persistedImageLocator(sessionID, decoded)
			if !saved[locator] {
				_, err = rows.SaveAgentSpill(ctx, store.AgentSpillArtifact{
					RunID: rootRunID(sessionID), SessionKey: sessionID,
					Locator: locator, ToolName: message.Name, CallID: message.ToolCallID,
					Label: "image", Content: part.Data,
				})
				if err != nil {
					continue
				}
				saved[locator] = true
			}
			part.Data = ""
			part.DataRef = locator
		}
	})
	return out
}

func entryHasExternalizableImages(entry agentcore.SessionEntry) bool {
	has := func(message agentcore.Message) bool {
		for _, part := range message.ContentParts {
			if part.Type == agentcore.ContentPartImage && len(part.Data) >= persistedImageExternalizeThreshold {
				return true
			}
		}
		return false
	}
	if entry.Message != nil && has(*entry.Message) {
		return true
	}
	for _, message := range entry.Retained {
		if has(message) {
			return true
		}
	}
	if entry.Outcome != nil {
		if has(entry.Outcome.Message) {
			return true
		}
		for _, message := range entry.Outcome.Extra {
			if has(message) {
				return true
			}
		}
	}
	return false
}

type hydratedSessionImage struct {
	data string
	ok   bool
}

// hydrateSessionEntryImages reverses persistence externalization before the
// log reaches agentcore. Missing/corrupt artifacts remove only their image and
// leave a visible breadcrumb; one damaged blob cannot make the session
// permanently unresumable. cache is shared across one Log/LogFrom read so a
// checkpoint and its source messages fetch a repeated image only once.
func hydrateSessionEntryImages(ctx context.Context, rows sessionImageRows, sessionID string, entry *agentcore.SessionEntry, cache map[string]hydratedSessionImage) {
	visitEntryMessages(entry, func(message *agentcore.Message) {
		if len(message.ContentParts) == 0 {
			return
		}
		parts := make([]agentcore.ContentPart, 0, len(message.ContentParts))
		missing := 0
		for _, part := range message.ContentParts {
			if part.Type != agentcore.ContentPartImage || part.Data != "" || part.DataRef == "" {
				parts = append(parts, part)
				continue
			}
			// A content-addressed row stores bytes, while MIME stays in the
			// session message. Include both in the cache identity so a damaged or
			// hand-edited log cannot validate bytes under one MIME and reuse that
			// decision for a conflicting MIME later in the same read.
			cacheKey := part.DataRef + "\x00" + part.MIMEType
			image, found := cache[cacheKey]
			if !found {
				image = loadPersistedSessionImage(ctx, rows, sessionID, part.DataRef, part.MIMEType)
				cache[cacheKey] = image
			}
			if !image.ok {
				missing++
				continue
			}
			part.Data = image.data
			part.DataRef = ""
			parts = append(parts, part)
		}
		message.ContentParts = parts
		if missing > 0 {
			note := fmt.Sprintf("[%d persisted image attachment(s) unavailable during session restore]", missing)
			if message.Content == "" {
				message.Content = note
			} else {
				message.Content += "\n" + note
			}
		}
	})
}

func loadPersistedSessionImage(ctx context.Context, rows sessionImageRows, sessionID, locator, mime string) hydratedSessionImage {
	window, err := rows.AgentSpillWindowForSession(ctx, locator, sessionID, 0, persistedImageMaxBase64Chars+1)
	if err != nil || window.Total <= 0 || window.Total > persistedImageMaxBase64Chars || len(window.Content) != window.Total {
		return hydratedSessionImage{}
	}
	data := string(window.Content)
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(decoded) > persistedImageMaxDecodedBytes || !validPersistedImageSignature(mime, decoded) {
		return hydratedSessionImage{}
	}
	return hydratedSessionImage{data: data, ok: true}
}

func persistedImageLocator(sessionID string, data []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(sessionID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(data)
	return "spillimg_" + hex.EncodeToString(h.Sum(nil)[:12])
}

func validPersistedImageSignature(mime string, data []byte) bool {
	switch mime {
	case "image/png":
		return bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n"))
	case "image/jpeg":
		return len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff
	case "image/gif":
		return bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a"))
	case "image/webp":
		return len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP"))
	default:
		return false
	}
}

func cloneEntryImageMessages(entry agentcore.SessionEntry) agentcore.SessionEntry {
	out := entry
	if entry.Message != nil {
		message := cloneRuntimeMessage(*entry.Message)
		out.Message = &message
	}
	out.Retained = cloneRuntimeMessages(entry.Retained)
	if entry.Outcome != nil {
		outcome := *entry.Outcome
		outcome.Message = cloneRuntimeMessage(entry.Outcome.Message)
		outcome.Extra = cloneRuntimeMessages(entry.Outcome.Extra)
		out.Outcome = &outcome
	}
	return out
}

func cloneRuntimeMessages(messages []agentcore.Message) []agentcore.Message {
	if messages == nil {
		return nil
	}
	out := make([]agentcore.Message, len(messages))
	for i, message := range messages {
		out[i] = cloneRuntimeMessage(message)
	}
	return out
}

func cloneRuntimeMessage(message agentcore.Message) agentcore.Message {
	out := message
	out.ContentParts = append([]agentcore.ContentPart(nil), message.ContentParts...)
	return out
}

func visitEntryMessages(entry *agentcore.SessionEntry, visit func(*agentcore.Message)) {
	if entry.Message != nil {
		visit(entry.Message)
	}
	for i := range entry.Retained {
		visit(&entry.Retained[i])
	}
	if entry.Outcome != nil {
		visit(&entry.Outcome.Message)
		for i := range entry.Outcome.Extra {
			visit(&entry.Outcome.Extra[i])
		}
	}
}
