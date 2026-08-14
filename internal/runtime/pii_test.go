package agentruntime

import (
	"strings"
	"testing"
)

// TestRedactPII checks the built-in structural detectors and the per-project
// substring denylist (§7, §13.3). PII must never survive into LLM egress or
// memory persistence.
func TestRedactPII(t *testing.T) {
	in := "contact jane.doe@example.com or +1 (415) 555-1234, host 10.0.0.5 user Jane"
	out := redactPII(in, []string{"Jane"})

	for _, leak := range []string{"jane.doe@example.com", "10.0.0.5", "555-1234", "Jane"} {
		if strings.Contains(out, leak) {
			t.Errorf("PII leaked through redaction: %q still present in %q", leak, out)
		}
	}
	if !strings.Contains(out, "[redacted-email]") || !strings.Contains(out, "[redacted-ip]") {
		t.Errorf("expected redaction markers, got %q", out)
	}
}

// TestRedactPIIDenylistCaseInsensitive confirms the extra substring list scrubs
// regardless of case and ignores empty entries.
func TestRedactPIIDenylistCaseInsensitive(t *testing.T) {
	out := redactPII("AcmeCorp signed up; acmecorp churned", []string{"acmecorp", "", "  "})
	if strings.Contains(strings.ToLower(out), "acmecorp") {
		t.Errorf("case-insensitive denylist failed: %q", out)
	}
}
