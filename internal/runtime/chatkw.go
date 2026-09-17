package agentruntime

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// chatkw.go — magic keywords: standalone lowercase words in a chat message that
// opt the turn into specialized behavior (ported from oh-my-pi's
// modes/magic-keywords.ts + markdown-prose.ts).
//
// A keyword is the same mechanism as a slash command one level down: instead of
// a leading "/name" it is a word the user drops into prose — "ultrathink this"
// asks for the deepest reasoning the run tier supports. Like commands, parsing
// is server-side so the word means the same thing from any client and on replay.
//
// Matching is deliberately strict, mirroring omp: the word must be a standalone
// token in prose — never inside a code span, fenced block, XML/HTML tag,
// identifier, or path — and lowercase only ("Ultrathink" is a name, not a
// switch). Fired keywords are stripped from the message the agent sees and
// recorded in the durable log (ConvKindKeyword); the raw text the user typed
// stays in the transcript.

// magicKeyword is one trigger word and the run behavior it selects.
type magicKeyword struct {
	// Word is the exact lowercase token ("ultrathink").
	Word string
	// ReasoningEffort, when non-empty, is passed to the run as
	// RunOptions.ReasoningEffort ("low" | "medium" | "high"); providers without
	// the knob ignore it.
	ReasoningEffort string
}

// magicKeywords is the catalog. Order matters only when two words set the same
// knob — the last match wins.
var magicKeywords = []magicKeyword{
	{Word: "ultrathink", ReasoningEffort: "high"},
}

// keywordMatch is one fired keyword occurrence: its byte range in the original
// message and the behavior it selects.
type keywordMatch struct {
	start, end int
	kw         magicKeyword
}

// parseMagicKeywords scans a chat message for catalog keywords standing alone
// in prose and returns the message with every fired keyword removed plus the
// matches (in catalog order of first use — deduped per word, so "ultrathink
// ultrathink" fires once but both copies are stripped).
func parseMagicKeywords(message string) (stripped string, fired []magicKeyword) {
	masked := maskNonProse(message)
	var ranges []keywordMatch
	seen := map[string]bool{}
	for _, kw := range magicKeywords {
		for i := 0; i+len(kw.Word) <= len(message); {
			j := strings.Index(message[i:], kw.Word)
			if j < 0 {
				break
			}
			start := i + j
			end := start + len(kw.Word)
			i = end
			if overlapsMask(masked, start, end) || !isStandaloneWord(message, start, end) {
				continue
			}
			ranges = append(ranges, keywordMatch{start: start, end: end, kw: kw})
			if !seen[kw.Word] {
				seen[kw.Word] = true
				fired = append(fired, kw)
			}
		}
	}
	if len(ranges) == 0 {
		return message, nil
	}
	return stripRanges(message, ranges), fired
}

// overlapsMask reports whether any byte of [start,end) was masked as non-prose.
func overlapsMask(masked []bool, start, end int) bool {
	for p := start; p < end; p++ {
		if masked[p] {
			return true
		}
	}
	return false
}

// isStandaloneWord applies omp's magic-keyword boundary rules procedurally —
// Go's RE2 has no lookaround, so the two lookbehind/lookahead clusters become
// rune checks on the bytes flanking the match:
//
//	left:  not [\p{L}\p{N}_./\\-] and not "::" — letters/digits/underscore make
//	       it an identifier, ./\- make it a path segment, "::" a symbol ref
//	       (foo::ultrathink). A single colon is prose punctuation ("note: x").
//	right: not [\p{L}\p{N}_/\\-], not ".x" (a file extension — "ultrathink.ts"),
//	       and not "(" (a call — "ultrathink()"). A bare "." is sentence end.
func isStandaloneWord(text string, start, end int) bool {
	if start > 0 {
		if start >= 2 && text[start-2] == ':' && text[start-1] == ':' {
			return false
		}
		r, _ := utf8.DecodeLastRuneInString(text[:start])
		if keywordBoundRune(r) || r == '.' {
			return false
		}
	}
	if end < len(text) {
		r, _ := utf8.DecodeRuneInString(text[end:])
		if keywordBoundRune(r) || r == '(' {
			return false
		}
		if r == '.' {
			// "." binds only when an extension char follows it.
			if next, _ := utf8.DecodeRuneInString(text[end+1:]); end+1 < len(text) &&
				(unicode.IsLetter(next) || unicode.IsNumber(next) || next == '_' || next == '-') {
				return false
			}
		}
	}
	return true
}

// keywordBoundRune is the shared left/right boundary alphabet: word characters
// plus the path/identifier joiners.
func keywordBoundRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) ||
		r == '_' || r == '/' || r == '\\' || r == '-'
}

// stripRanges removes each matched keyword from the message. The keyword
// occupied a token slot, so the whitespace/punctuation that belonged to its
// slot goes with it: sentence punctuation immediately after ("hard.
// ultrathink." → "hard.", "ultrathink, then" → "then") is consumed along with
// the space before it, and a removal between two spaces ("a ultrathink b" →
// "a b") drops one. Everything else is left exactly as typed.
func stripRanges(text string, ranges []keywordMatch) string {
	// Ranges arrive grouped per keyword, not sorted — order them for one pass.
	sorted := append([]keywordMatch(nil), ranges...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].start < sorted[j-1].start; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	var b []byte
	prev := 0
	for _, r := range sorted {
		b = append(b, text[prev:r.start]...)
		next := r.end
		if next < len(text) && strings.IndexByte(".,;:!?", text[next]) >= 0 {
			if len(b) > 0 && b[len(b)-1] == ' ' {
				b = b[:len(b)-1]
			}
			next++
		} else if next < len(text) && text[next] == ' ' && len(b) > 0 && b[len(b)-1] == ' ' {
			next++
		}
		prev = next
	}
	b = append(b, text[prev:]...)
	return strings.TrimSpace(string(b))
}

// --- non-prose masking (port of omp's markdown-prose.ts) --------------------
//
// maskNonProse returns a per-byte mask where true marks text inside a fenced
// code block, an inline code span, or an XML/HTML construct — the regions a
// keyword occurrence must not fire in. Indices address the original text, so
// the mask never perturbs the boundary checks above.

func maskNonProse(text string) []bool {
	masked := make([]bool, len(text))
	if !strings.Contains(text, "`") && !strings.Contains(text, "<") && !strings.Contains(text, "~~~") {
		return masked
	}
	n := len(text)

	// Phase 1: fenced code blocks, line by line. A fence is up to 3 leading
	// spaces then a run of >=3 backticks or tildes; a backtick fence's info
	// string may not itself contain a backtick.
	var fenceChar byte
	fenceLen := 0
	for lineStart := 0; lineStart <= n; {
		nl := strings.IndexByte(text[lineStart:], '\n')
		if nl < 0 {
			nl = n
		} else {
			nl += lineStart
		}
		line := text[lineStart:nl]
		indent, marker := fenceMarker(line)
		if fenceChar != 0 {
			for p := lineStart; p < nl; p++ {
				masked[p] = true
			}
			// A closing fence is the same char, at least as long, alone on the line.
			if marker >= fenceLen && line[indent] == fenceChar && strings.TrimSpace(line[indent+marker:]) == "" {
				fenceChar, fenceLen = 0, 0
			}
		} else if marker >= 3 {
			if !(line[indent] == '`' && strings.Contains(line[indent+marker:], "`")) {
				fenceChar, fenceLen = line[indent], marker
				for p := lineStart; p < nl; p++ {
					masked[p] = true
				}
			}
		}
		if nl == n {
			break
		}
		lineStart = nl + 1
	}

	// Phase 2: inline code spans and HTML/XML, over not-yet-masked regions.
	for i := 0; i < n; {
		if masked[i] {
			i++
			continue
		}
		switch text[i] {
		case '`':
			runEnd := backtickRunEnd(text, i, n)
			if close := findBacktickClose(text, runEnd, n, runEnd-i, masked); close >= 0 {
				for p := i; p < close; p++ {
					masked[p] = true
				}
				i = close
			} else {
				i = runEnd // unmatched run is literal text
			}
		case '<':
			if end := maskTagAt(text, i, n, masked); end > i {
				i = end
			} else {
				i++
			}
		default:
			i++
		}
	}
	return masked
}

// fenceMarker reports the leading-space count and the length of a fence run
// (>=3 identical backticks or tildes) opening a line, or marker 0 when the line
// is not a fence.
func fenceMarker(line string) (indent, marker int) {
	for indent < len(line) && line[indent] == ' ' && indent < 3 {
		indent++
	}
	if indent >= len(line) || (line[indent] != '`' && line[indent] != '~') {
		return indent, 0
	}
	c := line[indent]
	for indent+marker < len(line) && line[indent+marker] == c {
		marker++
	}
	return indent, marker
}

// backtickRunEnd is the index just past the run of backticks beginning at i.
func backtickRunEnd(text string, i, n int) int {
	for i < n && text[i] == '`' {
		i++
	}
	return i
}

// findBacktickClose finds the closing backtick run matching an opening run of
// runLen, scanning from `from` and skipping already-masked positions. Returns
// the index just past it, or -1 — an unmatched run is literal text, not a span.
func findBacktickClose(text string, from, n, runLen int, masked []bool) int {
	for k := from; k < n; {
		if masked[k] {
			k++
			continue
		}
		if text[k] == '`' {
			if e := backtickRunEnd(text, k, n); e-k == runLen {
				return e
			} else {
				k = e
			}
			continue
		}
		k++
	}
	return -1
}

// tagNameAt reads an HTML/XML tag name ([A-Za-z][A-Za-z0-9-]*) at i, returning
// its end index or i when none starts there.
func tagNameAt(text string, i, n int) int {
	if i >= n || text[i] < 'A' || (text[i] > 'Z' && text[i] < 'a') || text[i] > 'z' {
		return i
	}
	j := i + 1
	for j < n {
		c := text[j]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			j++
			continue
		}
		break
	}
	return j
}

// findTagEnd is the index of the ">" closing a tag whose attributes begin at j,
// honoring quoted values. -1 when the tag is malformed — a new "<" first, or no
// ">" at all — so callers treat the "<" as literal.
func findTagEnd(text string, j, n int) int {
	var quote byte
	for k := j; k < n; k++ {
		c := text[k]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '>':
			return k
		case '<':
			return -1
		}
	}
	return -1
}

// findMatchingClose locates the "</name>" balancing an opening "<name>",
// counting nested same-name tags. -1 when the section never closes, so callers
// mask only the opening tag rather than swallowing the rest of the document.
func findMatchingClose(text string, start, n int, name string, masked []bool) int {
	lname := strings.ToLower(name)
	depth := 1
	for k := start; k < n; {
		if masked[k] || text[k] != '<' {
			k++
			continue
		}
		m := k + 1
		isClose := false
		if m < n && text[m] == '/' {
			isClose = true
			m++
		}
		nameEnd := tagNameAt(text, m, n)
		if nameEnd == m {
			k++
			continue
		}
		gt := findTagEnd(text, nameEnd, n)
		if gt < 0 {
			k++
			continue
		}
		if strings.ToLower(text[m:nameEnd]) == lname {
			if isClose {
				depth--
				if depth == 0 {
					return gt + 1
				}
			} else if text[gt-1] != '/' {
				depth++
			}
		}
		k = gt + 1
	}
	return -1
}

// maskTagAt masks the HTML/XML construct beginning at "<" (index i): a comment,
// a self-closing/closing tag (the tag alone), or an opening tag together with
// the content through its matching close tag. Returns the index just past the
// masked region, or i when the "<" begins no tag (a stray less-than).
func maskTagAt(text string, i, n int, masked []bool) int {
	if strings.HasPrefix(text[i:], "<!--") {
		stop := n
		if end := strings.Index(text[i+4:], "-->"); end >= 0 {
			stop = i + 4 + end + 3
		}
		for p := i; p < stop; p++ {
			masked[p] = true
		}
		return stop
	}
	j := i + 1
	closing := false
	if j < n && text[j] == '/' {
		closing = true
		j++
	}
	nameEnd := tagNameAt(text, j, n)
	if nameEnd == j {
		return i
	}
	gt := findTagEnd(text, nameEnd, n)
	if gt < 0 {
		return i
	}
	tagEnd := gt + 1
	selfClosing := text[gt-1] == '/'
	for p := i; p < tagEnd; p++ {
		masked[p] = true
	}
	if closing || selfClosing {
		return tagEnd
	}
	close := findMatchingClose(text, tagEnd, n, text[j:nameEnd], masked)
	if close < 0 {
		return tagEnd
	}
	for p := tagEnd; p < close; p++ {
		masked[p] = true
	}
	return close
}
