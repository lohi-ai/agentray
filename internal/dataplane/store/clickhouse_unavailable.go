package storage

import (
	"context"
	"errors"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// errClickHouseRemoved is what every surviving ClickHouse-facing call returns.
// The engine connection and schema bootstrap were replaced by the embedded
// DuckDB runtime; the read paths that still emit ClickHouse SQL are ported to
// DuckDB in ticket 007 and this stub is deleted with the driver in 008.
var errClickHouseRemoved = errors.New("storage: clickhouse engine removed; analytics reads pending duckdb port (ticket 007)")

// unavailableCH satisfies chConn so the unported query code compiles and fails
// fast with a typed error instead of panicking on a nil interface. It is not a
// compatibility shim — nothing ever connects; it exists only to keep the
// compiler honest until the read paths are ported.
type unavailableCH struct{}

func (unavailableCH) Exec(context.Context, string, ...any) error {
	return errClickHouseRemoved
}

func (unavailableCH) Query(context.Context, string, ...any) (driver.Rows, error) {
	return nil, errClickHouseRemoved
}

func (unavailableCH) QueryRow(context.Context, string, ...any) driver.Row {
	return unavailableRow{}
}

func (unavailableCH) PrepareBatch(context.Context, string, ...driver.PrepareBatchOption) (driver.Batch, error) {
	return nil, errClickHouseRemoved
}

func (unavailableCH) Ping(context.Context) error { return errClickHouseRemoved }

func (unavailableCH) Close() error { return nil }

// unavailableRow is the driver.Row counterpart: every read fails with the
// same typed error.
type unavailableRow struct{}

func (unavailableRow) Err() error           { return errClickHouseRemoved }
func (unavailableRow) Scan(...any) error    { return errClickHouseRemoved }
func (unavailableRow) ScanStruct(any) error { return errClickHouseRemoved }
