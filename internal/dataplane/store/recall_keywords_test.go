package storage

import (
	"reflect"
	"testing"
)

// TestExtractKeywords verifies a natural-language recall query reduces to its
// distinct content terms: stopwords and sub-3-rune tokens are dropped, case is
// folded, punctuation splits tokens, and order is preserved without duplicates.
// This is the property that makes keyword recall match a memory by its terms
// instead of requiring the whole question to be a literal substring.
func TestExtractKeywords(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "question reduces to content terms",
			query: "Who is the marketing lead for Kiem Lai?",
			want:  []string{"marketing", "lead", "kiem", "lai"},
		},
		{
			name:  "vietnamese question keeps diacritics and short content words",
			query: "Trưởng nhóm marketing của Kiem Lai là ai?",
			want:  []string{"trưởng", "nhóm", "marketing", "kiem", "lai"},
		},
		{
			name:  "punctuation splits, case folds, keeps 2-rune tokens",
			query: "D7-retention: how's the SIGNUP funnel?",
			want:  []string{"d7", "retention", "signup", "funnel"},
		},
		{
			name:  "dedupes repeated terms",
			query: "traffic traffic Traffic sources",
			want:  []string{"traffic", "sources"},
		},
		{
			name:  "all-stopword query yields nothing",
			query: "what are the you have?",
			want:  []string{},
		},
		{
			name:  "empty query yields nothing",
			query: "",
			want:  []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractKeywords(tc.query)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("extractKeywords(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}
