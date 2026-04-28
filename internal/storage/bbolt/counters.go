package bboltstore

import (
	"go.etcd.io/bbolt"
)

// tenantCounters holds O(1) aggregates over a tenant's nodes and memory
// edges. Maintained transactionally by every Graph mutation so the
// service layer's Stats endpoint does not have to walk buckets.
type tenantCounters struct {
	NodeCount     int
	EdgeCount     int
	SumNodeWeight float64
	SumEdgeWeight float64
}

const (
	bktCounters       = "counters"
	keyCountersRecord = "v"
)

// readCountersTx returns the current counters for tenant. If the
// counter record is absent (e.g., the tenant has never been written to)
// the zero value is returned with no error.
func readCountersTx(tx *bbolt.Tx, tenant string) (tenantCounters, error) {
	t, err := tenantBucket(tx, tenant, false)
	if err != nil || t == nil {
		return tenantCounters{}, err
	}
	cb := t.Bucket([]byte(bktCounters))
	if cb == nil {
		return tenantCounters{}, nil
	}
	raw := cb.Get([]byte(keyCountersRecord))
	if raw == nil {
		return tenantCounters{}, nil
	}
	var c tenantCounters
	if err := gobDecode(raw, &c); err != nil {
		return tenantCounters{}, err
	}
	return c, nil
}

// writeCountersTx persists the counter record. The tenant + counters
// bucket are created on demand so this is safe to call before any other
// mutation has materialized the per-tenant subtree.
func writeCountersTx(tx *bbolt.Tx, tenant string, c tenantCounters) error {
	t, err := tenantBucket(tx, tenant, true)
	if err != nil {
		return err
	}
	cb, err := subBucket(t, bktCounters, true)
	if err != nil {
		return err
	}
	buf, err := gobEncode(&c)
	if err != nil {
		return err
	}
	return cb.Put([]byte(keyCountersRecord), buf)
}

// adjustCountersTx reads the existing counters, adds delta component-wise,
// and writes the result back — all inside the supplied tx.
func adjustCountersTx(tx *bbolt.Tx, tenant string, delta tenantCounters) error {
	if delta == (tenantCounters{}) {
		return nil
	}
	cur, err := readCountersTx(tx, tenant)
	if err != nil {
		return err
	}
	cur.NodeCount += delta.NodeCount
	cur.EdgeCount += delta.EdgeCount
	cur.SumNodeWeight += delta.SumNodeWeight
	cur.SumEdgeWeight += delta.SumEdgeWeight
	return writeCountersTx(tx, tenant, cur)
}
