package storage

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
)

var ErrDataCapacity = errors.New("data capacity unavailable")

type DiskProbe func(path string) (uint64, error)

// DiskReserve is the admission gate for new data bytes. Receipt/hole writes
// deliberately bypass it so low space cannot prevent recording why a store is
// incomplete or prevent recovery progress.
type DiskReserve struct {
	path    string
	reserve uint64
	probe   DiskProbe
}

func NewDiskReserve(path string, reserve uint64) *DiskReserve {
	return &DiskReserve{path: filepath.Dir(path), reserve: reserve, probe: availableDiskBytes}
}

func (r *DiskReserve) Check() error {
	if r == nil || r.reserve == 0 {
		return nil
	}
	available, err := r.probe(r.path)
	if err != nil {
		return fmt.Errorf("%w: sample free space: %v", ErrDataCapacity, err)
	}
	if available < r.reserve {
		return fmt.Errorf("%w: %d bytes free is below the configured %d-byte reserve", ErrDataCapacity, available, r.reserve)
	}
	return nil
}

func availableDiskBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

func (d *DuckDB) admitDataWrite() error {
	if d == nil {
		return errDuckDBClosed
	}
	return d.diskReserve.Check()
}
