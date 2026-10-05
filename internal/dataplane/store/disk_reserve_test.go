package storage

import (
	"errors"
	"testing"
)

func TestDiskReserveRefusesUnknownAndLowSpace(t *testing.T) {
	r := &DiskReserve{path: t.TempDir(), reserve: 100, probe: func(string) (uint64, error) { return 99, nil }}
	if err := r.Check(); !errors.Is(err, ErrDataCapacity) {
		t.Fatalf("low space = %v", err)
	}
	r.probe = func(string) (uint64, error) { return 0, errors.New("probe failed") }
	if err := r.Check(); !errors.Is(err, ErrDataCapacity) {
		t.Fatalf("unknown space = %v", err)
	}
	r.probe = func(string) (uint64, error) { return 100, nil }
	if err := r.Check(); err != nil {
		t.Fatalf("at reserve = %v", err)
	}
}
