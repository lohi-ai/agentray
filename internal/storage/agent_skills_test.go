package storage

import (
	"context"
	"testing"
)

func TestSkillBodiesByIDEmptyIDs(t *testing.T) {
	var s Store
	got, err := s.SkillBodiesByID(context.Background(), "scope-1", nil)
	if err != nil {
		t.Fatalf("SkillBodiesByID empty ids error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("SkillBodiesByID empty ids = %v, want empty map", got)
	}
}
