package agentruntime

import (
	"encoding/json"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
)

// This file holds the cheap-tier front-desk parsing helpers, relocated from the
// retired agentorch package. They are pure string/message utilities the chat
// classifier reuses; keeping them here removes the last dependency on a separate
// orchestrator package now that conversation lives in the general agent.

// decodeDecision extracts a {"route":...,"reply":...} pair from an LLM
// classifier's reply, tolerating code fences or stray prose around the JSON.
// Returns ("","") when no JSON object is found or it doesn't parse, leaving the
// fallback choice (which route to assume) to the caller.
func decodeDecision(raw string) (route, reply string) {
	obj := firstJSONObject(raw)
	if obj == "" {
		return "", ""
	}
	var parsed struct {
		Route string `json:"route"`
		Reply string `json:"reply"`
	}
	if err := json.Unmarshal([]byte(obj), &parsed); err != nil {
		return "", ""
	}
	return strings.TrimSpace(parsed.Route), strings.TrimSpace(parsed.Reply)
}

// firstJSONObject returns the first balanced {...} object in s, or "" if none.
// Quote/escape aware so a brace inside a string value doesn't end it early.
func firstJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// inside a string literal — ignore braces
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// recentHistory returns the last n turns (user/assistant only) so the classifier
// has light conversational context without blowing the cheap-tier budget.
func recentHistory(history []agentcore.Message, n int) []agentcore.Message {
	out := make([]agentcore.Message, 0, n)
	for _, m := range history {
		if m.Role == agentcore.RoleUser || m.Role == agentcore.RoleAssistant {
			out = append(out, agentcore.Message{Role: m.Role, Content: m.Content})
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}
