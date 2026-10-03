package engine

import (
	"strings"

	"github.com/dlclark/regexp2"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Pi creates RegExp(pattern, "u"). Keep the original pattern for diagnostics
// while enabling ECMAScript syntax and Unicode code-point matching in Go.
type argumentRegexp struct {
	compiled *regexp2.Regexp
	source   string
}

func (pattern *argumentRegexp) MatchString(value string) bool {
	matched, err := pattern.compiled.MatchString(value)
	return err == nil && matched
}

func (pattern *argumentRegexp) String() string { return pattern.source }

func compileArgumentRegexp(pattern string) (jsonschema.Regexp, error) {
	compiled, err := regexp2.Compile(argumentRegexpDots(pattern), regexp2.ECMAScript|regexp2.Unicode)
	if err != nil {
		return nil, err
	}
	return &argumentRegexp{compiled: compiled, source: pattern}, nil
}

// regexp2's ECMAScript dot excludes CR/LF but still accepts JavaScript's two
// Unicode line separators. Rewrite only unescaped dots outside character classes.
func argumentRegexpDots(pattern string) string {
	var out strings.Builder
	escaped, inClass := false, false
	for _, char := range pattern {
		if escaped {
			out.WriteRune(char)
			escaped = false
			continue
		}
		switch char {
		case '\\':
			escaped = true
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '.':
			if !inClass {
				out.WriteString(`[^\n\r\u2028\u2029]`)
				continue
			}
		}
		out.WriteRune(char)
	}
	return out.String()
}
