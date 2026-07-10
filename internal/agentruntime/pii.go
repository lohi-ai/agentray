package agentruntime

import (
	"regexp"
	"strings"
)

// PII redaction (§7, §13.3). v1 ships a built-in denylist of high-signal,
// structurally-detectable traits (email, phone, IPv4). Free-text names/addresses
// are not reliably detectable without NER and are out of scope for v1; projects
// extend coverage via an explicit substring denylist.
var (
	reEmail = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	rePhone = regexp.MustCompile(`(?:\+?\d[\d\-\s().]{7,}\d)`)
	reIPv4  = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
)

// redactPII replaces detectable PII in s with a [redacted] marker. extra is an
// optional list of case-insensitive substrings (per-project denylist) also
// scrubbed.
func redactPII(s string, extra []string) string {
	s = reEmail.ReplaceAllString(s, "[redacted-email]")
	s = reIPv4.ReplaceAllString(s, "[redacted-ip]")
	s = rePhone.ReplaceAllString(s, "[redacted-phone]")
	for _, term := range extra {
		t := strings.TrimSpace(term)
		if t == "" {
			continue
		}
		re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(t))
		s = re.ReplaceAllString(s, "[redacted]")
	}
	return s
}
