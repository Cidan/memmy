// Package types holds the domain entities used throughout memmy.
//
// Storage backends serialize these values into their native primitives
// (bbolt buckets, SQL tables, Bigtable column families). The shapes
// defined here are part of the design contract — see DESIGN.md §4.
package types

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// EdgeKind classifies a memory association edge. Affects initial weight and
// decay rate; retrieval logic does not branch on Kind.
type EdgeKind uint8

const (
	// EdgeStructural — same source message or temporal adjacency. Created
	// at write time. Slow decay.
	EdgeStructural EdgeKind = iota
	// EdgeCoRetrieval — appeared together in the top-K seed set after
	// reranking. Created or reinforced at read time.
	EdgeCoRetrieval
	// EdgeCoTraversal — graph expansion hopped this edge AND the target
	// node ended up in the final returned result set.
	EdgeCoTraversal
)

func (k EdgeKind) String() string {
	switch k {
	case EdgeStructural:
		return "structural"
	case EdgeCoRetrieval:
		return "coretrieval"
	case EdgeCoTraversal:
		return "cotraversal"
	default:
		return fmt.Sprintf("kind(%d)", k)
	}
}

// Node is a chunk's metadata. The vector itself lives in the VectorIndex's
// `vectors` collection. See DESIGN.md §4.2.
type Node struct {
	ID           string
	TenantID     string
	SourceMsgID  string
	SentenceSpan [2]int
	Text         string
	EmbeddingDim int
	CreatedAt    time.Time
	LastTouched  time.Time
	Weight       float64
	AccessCount  uint64
	Tombstoned   bool
}

// Message is the original full text the caller wrote, persisted once and
// referenced by every chunk that came from it. See DESIGN.md §4.5.
type Message struct {
	ID        string
	TenantID  string
	Text      string
	Metadata  map[string]string
	CreatedAt time.Time
}

// MemoryEdge is a directed Hebbian association between two nodes. Distinct
// from HNSW navigation links. See DESIGN.md §4.3 / §7.5.
type MemoryEdge struct {
	From          string
	To            string
	TenantID      string
	Kind          EdgeKind
	Weight        float64
	LastTouched   time.Time
	CreatedAt     time.Time
	AccessCount   uint64
	TraverseCount uint64
}

// HNSWRecord stores the per-node neighbor lists across all layers it
// participates in. Static after insert. See DESIGN.md §4.4.
type HNSWRecord struct {
	NodeID    string
	Layer     int
	Neighbors map[int][]string
}

// HNSWMeta is per-tenant index metadata. Read fresh from the backend on
// every operation that needs it (no in-memory cache — DESIGN.md §0 #3).
type HNSWMeta struct {
	Dim            int
	EntryPoint     string
	MaxLayer       int
	M              int
	M0             int
	EfConstruction int
	EfSearch       int
	ML             float64
	Size           int
}

// TenantInfo is the entry registered in the `tenants` collection.
type TenantInfo struct {
	ID        string
	Tuple     map[string]string
	CreatedAt time.Time
}

// CanonicalTenant returns the canonical string form of a tenant tuple:
// keys and values trimmed, keys sorted, separated by NUL bytes. Stable
// across runs and processes.
func CanonicalTenant(tuple map[string]string) string {
	if len(tuple) == 0 {
		return ""
	}
	normalized := make(map[string]string, len(tuple))
	keys := make([]string, 0, len(tuple))
	for k, v := range tuple {
		nk := strings.TrimSpace(k)
		normalized[nk] = strings.TrimSpace(v)
		keys = append(keys, nk)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(0)
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(normalized[k])
	}
	return b.String()
}

// TenantID derives a stable 16-byte hex tenant identifier from a tuple.
// SHA-256 of the canonical form, truncated to 16 bytes (32 hex chars).
func TenantID(tuple map[string]string) string {
	canon := CanonicalTenant(tuple)
	sum := sha256.Sum256([]byte(canon))
	return hex.EncodeToString(sum[:16])
}

// ScoreBreakdown is returned alongside RecallHit for explainability.
type ScoreBreakdown struct {
	Sim        float64
	NodeWeight float64
	GraphMult  float64
	Depth      int
}

// WriteRequest / WriteResult — see DESIGN.md §9.1.
type WriteRequest struct {
	Tenant   map[string]string
	Message  string
	Metadata map[string]string
}

type WriteResult struct {
	MessageID string
	NodeIDs   []string
}

// RecallRequest / RecallResult / RecallHit — see DESIGN.md §6, §9.1.
type RecallRequest struct {
	Tenant      map[string]string
	Query       string
	K           int
	Hops        int
	OversampleN int
}

type RecallResult struct {
	Results []RecallHit
}

type RecallHit struct {
	NodeID         string
	Text           string
	SourceMsgID    string
	SourceText     string
	Score          float64
	ScoreBreakdown ScoreBreakdown
	Path           []string
}

// ForgetRequest / ForgetResult — see DESIGN.md §9.1.
type ForgetRequest struct {
	Tenant    map[string]string
	MessageID string
	Before    time.Time
}

type ForgetResult struct {
	DeletedNodes   int
	DeletedEdges   int
	DeletedVectors int
}

// StatsRequest / StatsResult — see DESIGN.md §9.1.
type StatsRequest struct {
	Tenant map[string]string
}

type StatsResult struct {
	NodeCount       int
	MemoryEdgeCount int
	HNSWSize        int
	AvgNodeWeight   float64
	AvgEdgeWeight   float64
}
