package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
)

// tenantCounters holds O(1) aggregates over a tenant's nodes and memory
// edges. Maintained transactionally by every Graph mutation so the
// service layer's Stats endpoint does not have to walk the tables.
type tenantCounters struct {
	NodeCount     int
	EdgeCount     int
	SumNodeWeight float64
	SumEdgeWeight float64
}

// readCountersTx returns the current counters for tenant. If the row
// is absent (tenant never written) the zero value is returned.
func readCountersTx(ctx context.Context, tx *sql.Tx, tenant string) (tenantCounters, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT blob FROM counters WHERE tenant = ?`, tenant).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return tenantCounters{}, nil
	}
	if err != nil {
		return tenantCounters{}, err
	}
	var c tenantCounters
	if err := gobDecode(raw, &c); err != nil {
		return tenantCounters{}, err
	}
	return c, nil
}

// writeCountersTx upserts the counter row.
func writeCountersTx(ctx context.Context, tx *sql.Tx, tenant string, c tenantCounters) error {
	buf, err := gobEncode(&c)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO counters(tenant, blob) VALUES(?, ?)
		ON CONFLICT(tenant) DO UPDATE SET blob = excluded.blob
	`, tenant, buf)
	return err
}

// adjustCountersTx reads the existing counters, adds delta component-wise,
// and writes the result back — all inside the supplied tx.
func adjustCountersTx(ctx context.Context, tx *sql.Tx, tenant string, delta tenantCounters) error {
	if delta == (tenantCounters{}) {
		return nil
	}
	cur, err := readCountersTx(ctx, tx, tenant)
	if err != nil {
		return err
	}
	cur.NodeCount += delta.NodeCount
	cur.EdgeCount += delta.EdgeCount
	cur.SumNodeWeight += delta.SumNodeWeight
	cur.SumEdgeWeight += delta.SumEdgeWeight
	return writeCountersTx(ctx, tx, tenant, cur)
}
