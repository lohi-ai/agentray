package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// The session-query seam gives an agent authorized retrieval over its own
// durable session log.
//
// The problem it solves: compaction is lossy on purpose. Once the loop
// summarizes an older span, the detail is gone from the model's context — but
// it is still sitting in the append-only log, fully intact. Without a way back
// in, a long run reasons from a summary of a summary and re-runs tools it
// already ran to recover facts it already had. session_query is the read path
// that makes compaction safe: shrink the context freely, because anything
// dropped is one query away.
//
// It is deliberately NOT part of compaction (a run may query without ever
// compacting, and a compacting run need not query), and the fence is the same
// one used by spill and jobs: a query is scoped to sessions the run owns.
//
// Ported from deepseek-harness's session-query capability family
// (packages/session-query, MIT), whose reference provider is SQLite FTS.

// SessionQuery is the retrieval seam. The built-in provider searches the
// current session's own log; a consumer supplies its own to search across a
// workspace's sessions.
//
// IMPORTANT for cross-session providers: a provider that searches an UNBOUNDED
// set must be index-backed and fuzzy (this repo's standing search rule —
// pg_trgm GIN over a normalized key, queried with % / similarity, never a bare
// ILIKE '%q%' and never a full scan). The built-in provider is exempt only
// because one session's log is a bounded, already-scoped set.
type SessionQuery interface {
	Search(ctx context.Context, req SessionQueryRequest) (SessionQueryResult, error)
}

// SessionQueryRequest is one retrieval request.
type SessionQueryRequest struct {
	// SessionID is the log to search. The loop always sets it to the run's own
	// session before calling a provider, so a model-supplied value can never
	// widen the scope.
	SessionID string
	// Query is free text. Empty means "return the most recent entries", which is
	// how a model browses rather than searches.
	Query string
	// Kinds filters by entry kind. Empty searches messages only — the model
	// almost always wants what was said, not bookkeeping entries.
	Kinds []SessionEntryKind
	// Limit caps the matches returned. 0 uses defaultSessionQueryLimit.
	Limit int
	// ExcerptBytes caps each match's excerpt. 0 uses defaultSessionExcerptBytes.
	ExcerptBytes int
}

// SessionQueryResult is a page of matches.
type SessionQueryResult struct {
	Matches []SessionMatch
	// Searched is how many entries were considered, so the model can tell "no
	// matches in a big log" from "the log is nearly empty".
	Searched int
}

// SessionMatch is one hit.
type SessionMatch struct {
	SessionID string           `json:"session_id"`
	EntryID   string           `json:"entry_id,omitempty"`
	Seq       int              `json:"seq"`
	Kind      SessionEntryKind `json:"kind"`
	Turn      int              `json:"turn,omitempty"`
	Role      Role             `json:"role,omitempty"`
	Excerpt   string           `json:"excerpt"`
	CreatedAt time.Time        `json:"created_at,omitempty"`
	// Score is the provider's relevance score; higher is better. The built-in
	// provider scores by matched-token count then recency.
	Score float64 `json:"score,omitempty"`
}

const (
	defaultSessionQueryLimit   = 10
	defaultSessionExcerptBytes = 600
	maxSessionQueryLimit       = 50
)

// SessionQuerySettings enables the seam for a run.
type SessionQuerySettings struct {
	// Provider performs the search. nil uses the built-in provider over the
	// run's own SessionStore (which therefore requires Session + SessionID).
	Provider SessionQuery
	// MaxLimit caps what one query may return regardless of what the model asks
	// for. 0 uses maxSessionQueryLimit.
	MaxLimit int
}

// sessionQueryPolicy is the per-run resolved configuration.
type sessionQueryPolicy struct {
	provider  SessionQuery
	sessionID string
	maxLimit  int
}

// newSessionQueryPolicy resolves settings for a run, or returns nil when the
// seam is off. Without a provider AND without a durable session there is
// nothing to search, so the seam stays off rather than registering a tool that
// can only ever answer "nothing".
func newSessionQueryPolicy(s *SessionQuerySettings, store SessionStore, sessionID string) *sessionQueryPolicy {
	if s == nil {
		return nil
	}
	provider := s.Provider
	if provider == nil {
		if store == nil || sessionID == "" {
			return nil
		}
		provider = &logSessionQuery{store: store}
	}
	maxLimit := s.MaxLimit
	if maxLimit <= 0 {
		maxLimit = maxSessionQueryLimit
	}
	return &sessionQueryPolicy{provider: provider, sessionID: sessionID, maxLimit: maxLimit}
}

// logSessionQuery is the built-in provider: a bounded scan of one session's own
// append-only log. Bounded is the operative word — this is a single session's
// entries, already scoped, so a scan is the right shape here and only here.
type logSessionQuery struct {
	store SessionStore
}

// Search folds the log into ranked matches. Matching is token-AND over
// accent-folded, case-folded text: a query for "doanh thu" finds "Doanh Thu"
// and "doanh-thu", because a model that half-remembers a phrase should still
// find it. Exact substring matching would miss all of those.
func (q *logSessionQuery) Search(ctx context.Context, req SessionQueryRequest) (SessionQueryResult, error) {
	log, err := q.store.Log(ctx, req.SessionID)
	if err != nil {
		return SessionQueryResult{}, err
	}
	kinds := map[SessionEntryKind]bool{}
	for _, k := range req.Kinds {
		kinds[k] = true
	}
	tokens := foldTokens(req.Query)
	excerpt := req.ExcerptBytes
	if excerpt <= 0 {
		excerpt = defaultSessionExcerptBytes
	}

	var out []SessionMatch
	searched := 0
	for _, e := range log {
		if len(kinds) == 0 {
			// Default scope: what was actually said. Bookkeeping entries
			// (model changes, leaves, tool-disable records) are noise to a model
			// trying to remember a fact.
			if e.Kind != EntryMessage {
				continue
			}
		} else if !kinds[e.Kind] {
			continue
		}
		text := entryText(e)
		if text == "" {
			continue
		}
		searched++
		score, ok := scoreTokens(text, tokens)
		if !ok {
			continue
		}
		m := SessionMatch{
			SessionID: req.SessionID,
			EntryID:   e.ID,
			Seq:       e.Seq,
			Kind:      e.Kind,
			Turn:      e.Turn,
			Excerpt:   truncateMiddle(text, excerpt),
			CreatedAt: e.CreatedAt,
			Score:     score,
		}
		if e.Message != nil {
			m.Role = e.Message.Role
		}
		out = append(out, m)
	}

	// Rank by score, then most recent first — a tie between two equally relevant
	// entries should surface the one the model saw last.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Seq > out[j].Seq
	})
	limit := req.Limit
	if limit <= 0 {
		limit = defaultSessionQueryLimit
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return SessionQueryResult{Matches: out, Searched: searched}, nil
}

// entryText renders the searchable text of an entry.
func entryText(e SessionEntry) string {
	switch {
	case e.Message != nil && e.Message.Content != "":
		return e.Message.Content
	case e.Summary != "":
		return e.Summary
	case e.Goal != "":
		return e.Goal
	default:
		return ""
	}
}

// scoreTokens reports whether text matches every query token, and how strongly.
// An empty token list matches everything with score 0, which is how a
// query-less request degrades to "most recent entries".
func scoreTokens(text string, tokens []string) (float64, bool) {
	if len(tokens) == 0 {
		return 0, true
	}
	folded := foldText(text)
	score := 0.0
	for _, tok := range tokens {
		n := strings.Count(folded, tok)
		if n == 0 {
			return 0, false
		}
		// Diminishing returns: an entry mentioning a term twenty times is not
		// twenty times more relevant than one mentioning it twice.
		score += 1 + float64(n-1)/10
	}
	return score, true
}

// foldTokens splits a query into folded, deduplicated tokens.
func foldTokens(q string) []string {
	fields := strings.FieldsFunc(foldText(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// foldText lowercases and strips combining marks, so accented and unaccented
// spellings compare equal (Vietnamese content is the motivating case: a model
// recalling "doanh thu" must match "doanh thu" written with tones).
func foldText(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	folded, _, err := transform.String(t, s)
	if err != nil {
		folded = s
	}
	return strings.ToLower(folded)
}

// sessionQueryToolName is the built-in tool. Like read_skill / read_spill /
// job_* it bypasses the permission gate: the loop pins the request to the run's
// OWN session id, so it can only return text this session already produced.
const sessionQueryToolName = "session_query"

// sessionQueryTool is the model-facing retrieval tool.
type sessionQueryTool struct {
	policy *sessionQueryPolicy
}

func (t *sessionQueryTool) Name() string { return sessionQueryToolName }

// Parallel: a pure read, safe to run alongside other parallel-eligible calls.
func (t *sessionQueryTool) Parallel() bool { return true }

func (t *sessionQueryTool) Schema() ToolSchema {
	return ToolSchema{
		Name: sessionQueryToolName,
		Description: "Search this session's own history, including parts that were summarized away by compaction. " +
			"Use it to recover a detail you no longer see in context — an earlier query result, a number, a decision — " +
			"instead of re-running the tool that produced it. Matching ignores case and accents and requires every " +
			"word in the query to appear. Omit 'query' to list the most recent entries.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Words that must all appear in the entry. Omit to browse the most recent entries.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": fmt.Sprintf("Maximum matches to return (default %d).", defaultSessionQueryLimit),
				},
			},
		},
	}
}

func (t *sessionQueryTool) Run(ctx context.Context, args string) (string, error) {
	if t.policy == nil || t.policy.provider == nil {
		return "", errNoSessionQuery
	}
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultSessionQueryLimit
	}
	if limit > t.policy.maxLimit {
		limit = t.policy.maxLimit
	}
	// The fence: the session id comes from the run, never from the model.
	res, err := t.policy.provider.Search(ctx, SessionQueryRequest{
		SessionID: t.policy.sessionID,
		Query:     in.Query,
		Limit:     limit,
	})
	if err != nil {
		return "", err
	}
	return renderSessionMatches(in.Query, res), nil
}

// renderSessionMatches formats a result page for the model.
func renderSessionMatches(query string, res SessionQueryResult) string {
	if len(res.Matches) == 0 {
		if res.Searched == 0 {
			return "No searchable history yet in this session."
		}
		return fmt.Sprintf("No entries matched %q (searched %d entries).", query, res.Searched)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d match(es) of %d searched entries:\n", len(res.Matches), res.Searched)
	for _, m := range res.Matches {
		fmt.Fprintf(&b, "\n--- turn %d, %s", m.Turn, m.Kind)
		if m.Role != "" {
			fmt.Fprintf(&b, " (%s)", m.Role)
		}
		if !m.CreatedAt.IsZero() {
			fmt.Fprintf(&b, " at %s", m.CreatedAt.UTC().Format(time.RFC3339))
		}
		b.WriteString("\n")
		b.WriteString(m.Excerpt)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// errNoSessionQuery is returned when the tool runs without a resolved provider,
// which should be impossible (the loop only registers it with one).
var errNoSessionQuery = errors.New("agentcore: session query is not configured for this run")
