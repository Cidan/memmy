package bboltstore

// Bucket and key constants for the bbolt physical layout.
//
// Logical layout (DESIGN.md §4.7):
//
//	root
//	├── tenants/                           // tenant registry
//	├── t/                                 // per-tenant subtrees
//	│   └── <tenantID>/
//	│       ├── nodes/   <nodeID>          gob(Node)
//	│       ├── msgs/    <msgID>           gob(Message)
//	│       ├── vec/     <nodeID>          raw f32 LE bytes (normalized)
//	│       ├── hnsw/
//	│       │   ├── meta                   gob(HNSWMeta)
//	│       │   └── records/<nodeID>       gob(HNSWRecord)
//	│       ├── eout/    <fromID>/<toID>   gob(MemoryEdge)
//	│       └── ein/     <toID>/<fromID>   gob(MemoryEdge) (mirror)
//	└── meta/
//	    └── schema_version                 uint32
const (
	bktTenants     = "tenants"
	bktT           = "t"
	bktMeta        = "meta"
	bktNodes       = "nodes"
	bktMsgs        = "msgs"
	bktVec         = "vec"
	bktHNSW        = "hnsw"
	bktHNSWRecords = "records"
	keyHNSWMeta    = "meta"
	bktEout        = "eout"
	bktEin         = "ein"
	keySchemaVer   = "schema_version"
	// bktCounters and keyCountersRecord are defined alongside the
	// counters helpers in counters.go.

)

// schemaVersion bumps when the on-disk layout changes.
const schemaVersion uint32 = 1
