package sqlitestore

// Logical layout (DESIGN.md §4.7):
//
//	tenants(id PK, info BLOB)                              # tenant registry
//	nodes(tenant, id, blob, PK(tenant, id))                # gob(Node)
//	messages(tenant, id, blob, PK(tenant, id))             # gob(Message)
//	vectors(tenant, node_id, vec, PK(tenant, node_id))     # raw f32 LE bytes (normalized)
//	hnsw_meta(tenant PK, blob)                             # gob(HNSWMeta)
//	hnsw_records(tenant, node_id, blob, PK(tenant, node_id))  # gob(HNSWRecord)
//	edges_out(tenant, from_id, to_id, blob,
//	          PK(tenant, from_id, to_id))                  # gob(MemoryEdge); outbound mirror
//	edges_in (tenant, to_id, from_id, blob,
//	          PK(tenant, to_id, from_id))                  # gob(MemoryEdge); inbound mirror
//	counters(tenant PK, blob)                              # gob(tenantCounters)
//	meta(key PK, value)                                    # schema_version + future markers

const schemaVersion uint32 = 1

const keySchemaVer = "schema_version"

// schemaSQL creates every table the backend needs. Idempotent.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value BLOB
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS tenants (
  id   TEXT PRIMARY KEY,
  info BLOB NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS nodes (
  tenant TEXT NOT NULL,
  id     TEXT NOT NULL,
  blob   BLOB NOT NULL,
  PRIMARY KEY (tenant, id)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS messages (
  tenant TEXT NOT NULL,
  id     TEXT NOT NULL,
  blob   BLOB NOT NULL,
  PRIMARY KEY (tenant, id)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS vectors (
  tenant  TEXT NOT NULL,
  node_id TEXT NOT NULL,
  vec     BLOB NOT NULL,
  PRIMARY KEY (tenant, node_id)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS hnsw_meta (
  tenant TEXT PRIMARY KEY,
  blob   BLOB NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS hnsw_records (
  tenant  TEXT NOT NULL,
  node_id TEXT NOT NULL,
  blob    BLOB NOT NULL,
  PRIMARY KEY (tenant, node_id)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS edges_out (
  tenant  TEXT NOT NULL,
  from_id TEXT NOT NULL,
  to_id   TEXT NOT NULL,
  blob    BLOB NOT NULL,
  PRIMARY KEY (tenant, from_id, to_id)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS edges_in (
  tenant  TEXT NOT NULL,
  to_id   TEXT NOT NULL,
  from_id TEXT NOT NULL,
  blob    BLOB NOT NULL,
  PRIMARY KEY (tenant, to_id, from_id)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS counters (
  tenant TEXT PRIMARY KEY,
  blob   BLOB NOT NULL
) WITHOUT ROWID;
`
