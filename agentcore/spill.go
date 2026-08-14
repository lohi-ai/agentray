package agentcore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

// SpillStore persists a tool result too large to sit inline in the model's
// context and hands back a locator the model can read later.
//
// The problem it solves: without it, an oversized result is cut down to
// limits.MaxToolResultLen by truncateMiddle and the omitted bytes are GONE —
// the model is told a number and can never recover the content. A 40 MB query
// export, a long build log, a fetched page: the agent sees a head and a tail
// and has to re-run the tool (paying for it twice) to get at the middle, or
// simply reasons over a hole. With a store configured, the full text is saved
// verbatim and the inline result becomes a bounded preview plus a locator, so
// the omitted span is one read_spill call away.
//
// Implementations are consumer-supplied (agentray backs it with object storage
// keyed by session); MemorySpillStore is the in-process default used by tests
// and by runs that opt in without wiring durable storage.
//
// Ported from deepseek-harness's spill capability family (packages/spill:
// dsh-spill / dsh-spill-local / dsh-spill-policy, MIT). The seam is theirs; the
// Go shape, the fencing, and the retrieval tool are ours.
type SpillStore interface {
	// SaveText persists content verbatim and returns its locator. An error
	// makes the policy fall back to plain truncation — a spill failure must
	// never turn a successful tool call into a failed one.
	SaveText(ctx context.Context, req SpillRequest) (SpillRecord, error)
	// ReadText returns a byte slice of a previously saved artifact. offset and
	// limit are in bytes; the implementation clamps them to the artifact and
	// snaps the returned slice to UTF-8 rune boundaries.
	ReadText(ctx context.Context, locator string, offset, limit int) (SpillSlice, error)
}

// SpillRequest is one request to persist an oversized tool result.
type SpillRequest struct {
	// SessionID is the save-time storage namespace AND the fence: read_spill
	// only serves a locator minted for the reading run's session, so one agent
	// can never read another's spilled output by guessing a locator.
	SessionID string
	// ToolName and CallID identify the call that produced the text. Descriptive
	// (naming, audit) — never interpreted for access control.
	ToolName string
	CallID   string
	// Label is a short human tag for the artifact ("result").
	Label string
	// SuggestedName is a naming hint (e.g. "run_sql.txt"), never a path: the
	// backend sanitizes it to a single safe segment before use.
	SuggestedName string
	// Content is the full text to persist (UTF-8).
	Content string
}

// SpillRecord is a saved artifact's handle.
type SpillRecord struct {
	// Locator is the opaque model-facing handle passed back to read_spill.
	Locator string
	// RetrievalHint is backend-supplied prose telling the model how to get the
	// rest (appended to the notice). Empty uses the default hint.
	RetrievalHint string
	// Bytes is the exact size of the persisted content.
	Bytes int
}

// SpillSlice is a bounded read of a saved artifact.
type SpillSlice struct {
	Content string
	// Offset is the byte offset the returned content actually starts at (after
	// clamping and rune-boundary snapping).
	Offset int
	// Total is the artifact's full size in bytes.
	Total int
	// EOF reports whether this slice reaches the end of the artifact.
	EOF bool
}

// ErrSpillNotFound is returned by ReadText for an unknown or out-of-scope
// locator. It is deliberately indistinguishable from "belongs to another
// session": a locator is not an existence oracle.
var ErrSpillNotFound = errors.New("agentcore: spill artifact not found")

// SpillSettings tunes the spill policy for a run. The zero value is usable:
// MaxInlineBytes falls back to the run's limits.MaxToolResultLen.
type SpillSettings struct {
	// Store persists the oversized text. Required — a nil Store disables spill
	// entirely and the loop keeps plain truncation.
	Store SpillStore
	// MaxInlineBytes is the model-facing cap for a plain-text tool result. A
	// result above it is spilled and replaced by a preview + notice sized to
	// stay within this cap. 0 uses limits.MaxToolResultLen.
	MaxInlineBytes int
	// ExcludeTools are tool names whose results are never spilled. read_spill is
	// always excluded (a read of a spill must not spill again, which would loop);
	// add tools whose output is already bounded or must stay verbatim.
	ExcludeTools []string
}

// defaultSpillReadLimit bounds one read_spill call so retrieving a spilled
// artifact cannot itself blow the context budget. The model pages through a
// large artifact with successive offsets.
const defaultSpillReadLimit = 8 * 1024

// readSpillToolName is the built-in retrieval tool's name. Like read_skill it
// bypasses the permission gate: it can only return text THIS session already
// produced and had truncated away, so it grants no capability the agent did not
// already exercise. The session fence in the store is what makes that true.
const readSpillToolName = "read_spill"

// spillPolicy is the per-run resolved spill configuration.
type spillPolicy struct {
	store     SpillStore
	sessionID string
	maxInline int
	exclude   map[string]bool
}

// newSpillPolicy resolves settings against the run's limits, or returns nil when
// spill is not configured.
func newSpillPolicy(s *SpillSettings, sessionID string, limits Limits) *spillPolicy {
	if s == nil || s.Store == nil {
		return nil
	}
	max := s.MaxInlineBytes
	if max <= 0 {
		max = limits.MaxToolResultLen
	}
	if max <= 0 {
		// Truncation disabled for the run — nothing is ever oversized, so
		// spilling would never fire. Keep it off rather than inventing a cap.
		return nil
	}
	exclude := map[string]bool{readSpillToolName: true}
	for _, name := range s.ExcludeTools {
		exclude[name] = true
	}
	return &spillPolicy{store: s.Store, sessionID: sessionID, maxInline: max, exclude: exclude}
}

// SpillOwner is an optional SpillStore capability: a store that can answer
// whether a locator was minted for a given session lets the loop enforce the
// fence before it ever calls ReadText. A store that does not implement it is
// trusted to scope locators itself (it was handed the session id at save time).
type SpillOwner interface {
	OwnsSpill(locator, sessionID string) bool
}

// apply bounds one successful tool result. It returns the model-facing text and
// the locator it spilled to (empty when it did not spill).
//
// Rules, in order — each one is a failure mode dsh hit and documented:
//  1. A result within the cap is returned untouched.
//  2. An excluded tool (read_spill above all) is truncated, never spilled.
//  3. A save failure returns the plainly truncated result: a spill failure
//     never turns a successful call into an error and never hides output.
//  4. The notice's byte cost is reserved OUT of the cap before the preview is
//     sized, so the replacement is always <= cap. Since the original was > cap,
//     spilling can never make a result bigger.
func (p *spillPolicy) apply(ctx context.Context, call ToolCall, out string) (string, string) {
	if len(out) <= p.maxInline || p.exclude[call.Name] {
		return truncateMiddle(out, p.maxInline), ""
	}

	rec, err := p.store.SaveText(ctx, SpillRequest{
		SessionID:     p.sessionID,
		ToolName:      call.Name,
		CallID:        call.ID,
		Label:         "result",
		SuggestedName: call.Name + ".txt",
		Content:       out,
	})
	if err != nil {
		return truncateMiddle(out, p.maxInline), ""
	}

	hint := rec.RetrievalHint
	if hint == "" {
		hint = fmt.Sprintf("Call %s with this locator (and an offset/limit in bytes) to read any part of it.", readSpillToolName)
	}
	// Reserve an upper bound on the notice: the omitted count can only have
	// fewer digits than the total, so a notice built with the total is never
	// shorter than the final one.
	reserve := len(spillNotice(len(out), len(out), rec.Locator, hint))
	budget := p.maxInline - reserve - 2 // 2 == the "\n\n" between preview and notice
	if budget < 0 {
		// The notice alone fills the cap. Emit it bare if it fits; otherwise the
		// cap is too small to say anything about the spill, so return the most
		// content that legally fits. Never emit a replacement over the cap.
		notice := spillNotice(len(out), len(out), rec.Locator, hint)
		if len(notice) <= p.maxInline {
			return notice, rec.Locator
		}
		return clampRunes(out, p.maxInline), rec.Locator
	}

	preview, omitted := headTail(out, budget)
	notice := spillNotice(omitted, len(out), rec.Locator, hint)
	if preview == "" {
		return notice, rec.Locator
	}
	return preview + "\n\n" + notice, rec.Locator
}

// applySpill bounds one successful tool result for the model, spilling the
// overflow to durable storage when the run has a spill policy and falling back
// to plain head+tail truncation when it does not. The returned locator is empty
// unless an artifact was written.
func (a *Agent) applySpill(ctx context.Context, call ToolCall, out string, limits Limits) (string, string) {
	if a.spill == nil {
		return truncateMiddle(out, limits.MaxToolResultLen), ""
	}
	return a.spill.apply(ctx, call, out)
}

// spillNotice renders the omission notice appended after the preview.
func spillNotice(omitted, total int, locator, hint string) string {
	return fmt.Sprintf("(Omitted %d of %d bytes. Full result saved at: %s. %s)", omitted, total, locator, hint)
}

// spillEllipsis marks the excised middle of a preview. Its bytes are charged to
// the preview budget — the caller's cap is on the rendered string, not on the
// content inside it, and forgetting that is exactly how a "bounded" replacement
// overruns its cap.
const spillEllipsis = "\n…\n"

// headTail keeps the beginning AND end of s within budget bytes TOTAL —
// ellipsis included — returning the preview and the number of content bytes
// omitted from the middle. The end matters as much as the start (the error after
// pages of build output, a query's last rows, a stack trace's cause), so the
// content budget splits two-thirds head, one-third tail. Cuts never split a
// UTF-8 rune. A budget too small to hold the ellipsis plus anything useful
// returns an empty preview and the full length as omitted.
func headTail(s string, budget int) (string, int) {
	if len(s) <= budget {
		return s, 0
	}
	avail := budget - len(spillEllipsis)
	if avail <= 0 {
		return "", len(s)
	}
	head := avail * 2 / 3
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	start := len(s) - (avail - head)
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	if start <= head {
		return s, 0
	}
	return s[:head] + spillEllipsis + s[start:], start - head
}

// clampRunes returns at most max bytes of s, cut at a rune boundary and with no
// marker appended. Unlike truncateBytes it adds nothing, so its output is
// guaranteed to fit — the property the spill policy needs when the cap is too
// small to hold even an omission notice.
func clampRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	end := max
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

// MemorySpillStore is an in-process SpillStore: artifacts live in a map for the
// lifetime of the store. It is the default when a run enables spill without
// supplying storage, and it is what the tests use. A consumer that needs spilled
// output to survive the process (or to be visible to another node) supplies its
// own store.
type MemorySpillStore struct {
	mu    sync.Mutex
	items map[string]spillItem
}

type spillItem struct {
	sessionID string
	content   string
}

// NewMemorySpillStore builds an empty in-process store.
func NewMemorySpillStore() *MemorySpillStore {
	return &MemorySpillStore{items: make(map[string]spillItem)}
}

// SaveText stores content under a locator branded with the session, so a
// locator from another session cannot be read back through this store.
func (m *MemorySpillStore) SaveText(_ context.Context, req SpillRequest) (SpillRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.items == nil {
		m.items = make(map[string]spillItem)
	}
	locator := spillLocator(req)
	m.items[locator] = spillItem{sessionID: req.SessionID, content: req.Content}
	return SpillRecord{Locator: locator, Bytes: len(req.Content)}, nil
}

// ReadText returns a bounded, rune-aligned slice of a stored artifact.
func (m *MemorySpillStore) ReadText(_ context.Context, locator string, offset, limit int) (SpillSlice, error) {
	m.mu.Lock()
	item, ok := m.items[locator]
	m.mu.Unlock()
	if !ok {
		return SpillSlice{}, ErrSpillNotFound
	}
	return sliceSpill(item.content, offset, limit), nil
}

// OwnsSpill reports whether a locator was minted for the given session. The
// loop checks this before serving a read, so the fence holds even for a store
// whose locators are guessable.
func (m *MemorySpillStore) OwnsSpill(locator, sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.items[locator]
	return ok && item.sessionID == sessionID
}

// spillLocator mints an opaque, session-branded locator. The digest covers the
// session so two sessions producing byte-identical output never collide onto one
// artifact, and the prefix makes a locator recognizable in a transcript.
func spillLocator(req SpillRequest) string {
	sum := sha256.Sum256([]byte(req.SessionID + "\x00" + req.CallID + "\x00" + req.ToolName + "\x00" + req.SuggestedName))
	return "spill_" + hex.EncodeToString(sum[:12])
}

// sliceSpill clamps offset/limit to content and snaps both ends to rune
// boundaries so a paging model never receives half a character.
func sliceSpill(content string, offset, limit int) SpillSlice {
	total := len(content)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	for offset < total && !utf8.RuneStart(content[offset]) {
		offset++
	}
	if limit <= 0 {
		limit = defaultSpillReadLimit
	}
	end := offset + limit
	if end > total {
		end = total
	}
	for end < total && !utf8.RuneStart(content[end]) {
		end++
	}
	return SpillSlice{Content: content[offset:end], Offset: offset, Total: total, EOF: end >= total}
}

// readSpillTool is the built-in retrieval tool. It is registered only when the
// run has a spill policy, and it is fenced to the run's own session: the loop
// hands it the session id at construction, and a locator minted elsewhere reads
// back as not-found.
type readSpillTool struct {
	policy *spillPolicy
}

func (t *readSpillTool) Name() string { return readSpillToolName }

func (t *readSpillTool) Schema() ToolSchema {
	return ToolSchema{
		Name: readSpillToolName,
		Description: "Read a tool result that was too large to include inline and was saved to a spill artifact. " +
			"Pass the locator from the '(Omitted … Full result saved at: …)' notice. Reads a bounded byte window; " +
			"page through a large artifact by advancing offset until eof is true.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"locator": map[string]any{
					"type":        "string",
					"description": "The spill locator from the omission notice.",
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "Byte offset to start reading from. Defaults to 0.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": fmt.Sprintf("Maximum bytes to return. Defaults to %d.", defaultSpillReadLimit),
				},
			},
			"required": []string{"locator"},
		},
	}
}

// Parallel marks the tool read-only so several spill reads in one turn run
// concurrently — it touches no shared state.
func (t *readSpillTool) Parallel() bool { return true }

func (t *readSpillTool) Run(ctx context.Context, args string) (string, error) {
	var in struct {
		Locator string `json:"locator"`
		Offset  int    `json:"offset"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	in.Locator = strings.TrimSpace(in.Locator)
	if in.Locator == "" {
		return "", errors.New("locator is required")
	}
	if in.Limit <= 0 || in.Limit > defaultSpillReadLimit {
		in.Limit = defaultSpillReadLimit
	}
	// Fence: a store that can report ownership is asked directly; one that
	// cannot is trusted to scope its own locators (an external store keys them
	// under the session prefix it was handed at save time).
	if owner, ok := t.policy.store.(SpillOwner); ok {
		if !owner.OwnsSpill(in.Locator, t.policy.sessionID) {
			return "", ErrSpillNotFound
		}
	}
	slice, err := t.policy.store.ReadText(ctx, in.Locator, in.Offset, in.Limit)
	if err != nil {
		return "", err
	}
	end := ""
	if slice.EOF {
		end = ", end of artifact"
	}
	header := fmt.Sprintf("[bytes %d-%d of %d%s]\n", slice.Offset, slice.Offset+len(slice.Content), slice.Total, end)
	return header + slice.Content, nil
}
