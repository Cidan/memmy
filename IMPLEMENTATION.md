# memmy — Implementation Checklist

Working list of what we're building. Source-of-truth for design lives in `DESIGN.md`. Conventions in `CLAUDE.md`.

## US-001 — Project scaffold ✅
- [x] `go.mod` for Go 1.26.2 (module `github.com/Cidan/memmy`)
- [x] Directory layout: `cmd/memmy/`, `internal/{chunker,clock,config,embed,embed/fake,embed/gemini,graph,service,storage/bbolt,transport/mcp,types,vectorindex}`
- [x] `IMPLEMENTATION.md` (this file)
- [x] `CLAUDE.md` references this file
- [x] `.omc/prd.json` with task-specific stories
- [x] `go build ./...` succeeds

## US-002 — Core types (`internal/types/`) ✅
- [x] `Node`, `Message`, `MemoryEdge` (with `EdgeKind` constants), `HNSWRecord`, `HNSWMeta`, `TenantInfo`
- [x] `WriteRequest/Result`, `RecallRequest/Result`, `ForgetRequest/Result`, `StatsRequest/Result`, `RecallHit`, `ScoreBreakdown`
- [x] `TenantID` derivation: normalize tuple → sha256-truncated hex
- [x] Test: tenant-id determinism + canonicalization

## US-003 — Clock (`internal/clock/`) ✅
- [x] `Clock` interface with `Now() time.Time`
- [x] `Real` impl
- [x] `Fake` impl with `Advance(dur)` and `Set(t)`

## US-004 — Graph interface (`internal/graph/`) + bbolt impl (`internal/storage/bbolt/graph.go`) ✅
- [x] `Graph` interface per DESIGN.md §9.2
- [x] bbolt impl: nested buckets per DESIGN.md §4.7
- [x] Both `eout/` and `ein/` mirrors written atomically in one tx
- [x] `UpdateNode`/`UpdateEdge` closure API
- [x] Tombstone semantics on Node
- [x] Real bbolt integration tests in `t.TempDir()`:
  - CRUD for Node/Message/Edge
  - Bidirectional neighbor lookup
  - Atomic dual-mirror edge updates
  - Aborted tx leaves consistent state

## US-005 — VectorIndex interface (`internal/vectorindex/`) + bbolt flat scan (`internal/storage/bbolt/vectorindex.go`) ✅
- [x] `VectorIndex` interface per DESIGN.md §9.2
- [x] Vector L2 normalization helper
- [x] Raw little-endian float32 serialization
- [x] Flat scan via bbolt cursor (streaming, bounded heap)
- [x] Real bbolt integration test: flat scan returns correct top-N for known vectors

## US-006 — HNSW algorithm (in `internal/storage/bbolt/hnsw.go`) ✅
- [x] HNSW insert: greedy descent + ef-search + neighbor selection + bidirectional pruning, all in one tx
- [x] HNSW search: greedy descent + ef-search at layer 0
- [x] Hard delete via `Delete()` repairs neighbor lists across all layers; updates entry point
- [x] Backend selection: flat scan < threshold else HNSW
- [x] HNSW oracle integration test: HNSW agrees with flat scan above recall@k floor (≥0.93 with oversample=200, k=8 across 50 queries on a 2000-vector corpus)
- [x] Tx-abort consistency test (Update + Delete failure paths)

## US-007 — Embedder (`internal/embed/`) ✅
- [x] `Embedder` interface
- [x] Fake impl: deterministic hash-to-vector (SHA-256 → []float32)
- [x] Gemini impl: real `google.golang.org/genai` client; `GEMINI_API_KEY` from env
- [x] Tests: fake determinism; Gemini live test gated behind `GEMINI_API_KEY`

## US-008 — Chunker (`internal/chunker/`) ✅
- [x] Sentence splitter (rule-based, with abbreviation list and initial detection)
- [x] Sliding window (size=3, stride=2) per DESIGN.md §4.1
- [x] Tests: 10-sentence example produces `[1,2,3], [3,4,5], [5,6,7], [7,8,9], [9,10]`; idempotence; trailing-window correctness

## US-009 — MemoryService Write (`internal/service/`) ✅
- [x] `Write` op: chunk → embed (batched, before tx) → normalize → for each window: persist Node + vector + HNSW insert; structural edges (sequential within message + recent within tenant)
- [x] Integration test (real bbolt + fake embedder): writing a 10-sentence message creates 5 nodes, 5 vectors, 5 HNSW records, sequential edges between adjacent chunks

## US-010 — MemoryService Recall ✅
- [x] Vector search (flat or HNSW per tenant size) with oversample
- [x] Weight-aware rerank by `sim_normalized^α × weight^β`
- [x] Hebbian co-retrieval edge updates between seed pairs
- [x] BFS expansion via memory edges with edge-floor pruning + depth penalty
- [x] Co-traversal reinforcement on edges that delivered nodes into final result set
- [x] Provenance returned (score breakdown + path)
- [x] Integration tests:
  - Hot memory ranks above stale memory of equal raw similarity
  - Co-retrieval edges form between two memories returned together
  - Memory reachable only via expansion is included in result set
  - Edge whose decayed weight falls below `edge_floor` is removed on its next access

## US-011 — MemoryService Forget + Stats ✅
- [x] `Forget` by `MessageID` (purge all chunks + vectors + HNSW records + adjacent edges)
- [x] `Forget` by `Before` timestamp (purge messages and derived data created before)
- [x] `Stats` aggregates per-tenant or globally
- [x] Integration tests for both

## US-012 — Lazy decay/reinforce contract ✅
- [x] Decay+reinforce closure runs inside a single backend tx (`Graph.UpdateNode`/`UpdateEdge`)
- [x] Edge with decayed weight below `edge_floor` is deleted (both mirrors) inside the same access tx
- [x] Tests using `clock.Fake` cover decay over time and reinforcement caps

## US-013 — Configuration (`internal/config/`) ✅
- [x] YAML loader matching DESIGN.md §12 schema
- [x] Validation; env-var resolution for API keys
- [x] Tests for happy path + missing required fields
- [x] Sample config at `memmy.yaml.example`

## US-014 — MCP transport adapter (`internal/transport/mcp/`) ✅
- [x] Server using `github.com/modelcontextprotocol/go-sdk` v1.5.0
- [x] Tools: `memory.write`, `memory.recall`, `memory.forget`, `memory.stats`
- [x] Streamable HTTP handler exposed via `Adapter.HTTPHandler()`
- [x] Integration tests: in-process MCP client invokes each tool against a real bbolt-backed service and verifies the result

## US-015 — Entrypoint and supervisor ✅
- [x] `cmd/memmy/main.go` loads config, constructs Embedder/VectorIndex/Graph/MemoryService, registers transports, runs them under a suture supervisor
- [x] Signal-driven graceful shutdown closes the storage backend
- [x] `go build ./cmd/memmy` succeeds; smoke-tested end-to-end (HTTP 200 on `initialize`, clean SIGTERM shutdown)

## US-016 — Final regression ✅
- [x] `go vet ./...` clean
- [x] `go build ./...` clean
- [x] `go test ./...` all green (62 tests across 13 packages)
- [x] `go test -race ./...` all green
