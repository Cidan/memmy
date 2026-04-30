# memmy — Implementation Checklist

Working list of what we're building. Source-of-truth for design lives in `DESIGN.md`. Conventions in `CLAUDE.md`.

## Round 1 — v1 implementation (commits 81353df, 46c3359)

### US-001 — Project scaffold ✅
- [x] `go.mod` for Go 1.26.2 (module `github.com/Cidan/memmy`)
- [x] Directory layout: `cmd/memmy/`, `internal/{chunker,clock,config,embed,embed/fake,embed/gemini,graph,service,storage/bbolt,transport/mcp,types,vectorindex}`
- [x] `IMPLEMENTATION.md` (this file)
- [x] `CLAUDE.md` references this file
- [x] `.omc/prd.json` with task-specific stories
- [x] `go build ./...` succeeds

### US-002 — Core types (`internal/types/`) ✅
- [x] `Node`, `Message`, `MemoryEdge` (with `EdgeKind` constants), `HNSWRecord`, `HNSWMeta`, `TenantInfo`
- [x] `WriteRequest/Result`, `RecallRequest/Result`, `ForgetRequest/Result`, `StatsRequest/Result`, `RecallHit`, `ScoreBreakdown`
- [x] `TenantID` derivation: normalize tuple → sha256-truncated hex
- [x] Test: tenant-id determinism + canonicalization

### US-003 — Clock (`internal/clock/`) ✅
- [x] `Clock` interface with `Now() time.Time`
- [x] `Real` impl
- [x] `Fake` impl with `Advance(dur)` and `Set(t)`

### US-004 — Graph interface (`internal/graph/`) + bbolt impl (`internal/storage/bbolt/graph.go`) ✅
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

### US-005 — VectorIndex interface (`internal/vectorindex/`) + bbolt flat scan (`internal/storage/bbolt/vectorindex.go`) ✅
- [x] `VectorIndex` interface per DESIGN.md §9.2
- [x] Vector L2 normalization helper
- [x] Raw little-endian float32 serialization
- [x] Flat scan via bbolt cursor (streaming, bounded heap)
- [x] Real bbolt integration test: flat scan returns correct top-N for known vectors

### US-006 — HNSW algorithm (in `internal/storage/bbolt/hnsw.go`) ✅
- [x] HNSW insert: greedy descent + ef-search + neighbor selection + bidirectional pruning, all in one tx
- [x] HNSW search: greedy descent + ef-search at layer 0
- [x] Hard delete via `Delete()` repairs neighbor lists across all layers; updates entry point
- [x] Backend selection: flat scan < threshold else HNSW
- [x] HNSW oracle integration test: HNSW agrees with flat scan above the recall@k floor (see Round 2 US-101 — currently 0.95)
- [x] Tx-abort consistency test (Update + Delete failure paths)

### US-007 — Embedder (`internal/embed/`) ✅
- [x] `Embedder` interface
- [x] Fake impl: deterministic hash-to-vector (SHA-256 → []float32)
- [x] Gemini impl: real `google.golang.org/genai` client; `GEMINI_API_KEY` from env
- [x] Tests: fake determinism; Gemini live test gated behind `GEMINI_API_KEY`

### US-008 — Chunker (`internal/chunker/`) ✅
- [x] Sentence splitter (rule-based, with abbreviation list and initial detection)
- [x] Sliding window (size=3, stride=2) per DESIGN.md §4.1
- [x] Tests: 10-sentence example produces `[1,2,3], [3,4,5], [5,6,7], [7,8,9], [9,10]`; idempotence; trailing-window correctness

### US-009 — MemoryService Write (`internal/service/`) ✅
- [x] `Write` op: chunk → embed (batched, before tx) → normalize → for each window: persist Node + vector + HNSW insert; structural edges (sequential within message + recent within tenant)
- [x] Integration test (real bbolt + fake embedder): writing a 10-sentence message creates 5 nodes, 5 vectors, 5 HNSW records, sequential edges between adjacent chunks

### US-010 — MemoryService Recall ✅
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

### US-011 — MemoryService Forget + Stats ✅
- [x] `Forget` by `MessageID` (purge all chunks + vectors + HNSW records + adjacent edges)
- [x] `Forget` by `Before` timestamp (purge messages and derived data created before)
- [x] `Stats` aggregates per-tenant or globally (initially via bucket walk; later O(1) — see Round 2 US-102)
- [x] Integration tests for both

### US-012 — Lazy decay/reinforce contract ✅
- [x] Decay+reinforce closure runs inside a single backend tx (`Graph.UpdateNode`/`UpdateEdge`)
- [x] Edge with decayed weight below `edge_floor` is deleted (both mirrors) inside the same access tx
- [x] Tests using `clock.Fake` cover decay over time and reinforcement caps

### US-013 — Configuration (`internal/config/`) ✅
- [x] YAML loader matching DESIGN.md §12 schema
- [x] Validation; env-var resolution for API keys
- [x] Tests for happy path + missing required fields
- [x] Sample config at `memmy.example.yaml`

### US-014 — MCP transport adapter (`internal/transport/mcp/`) ✅
- [x] Server using `github.com/modelcontextprotocol/go-sdk` v1.5.0
- [x] Tools: `memory.write`, `memory.recall`, `memory.forget`, `memory.stats`
- [x] Streamable HTTP handler exposed via `Adapter.HTTPHandler()`
- [x] Integration tests: in-process MCP client invokes each tool against a real bbolt-backed service and verifies the result

### US-015 — Entrypoint and supervisor ✅
- [x] `cmd/memmy/main.go` loads config, constructs Embedder/VectorIndex/Graph/MemoryService, registers transports, runs them under a suture supervisor
- [x] Signal-driven graceful shutdown closes the storage backend
- [x] `go build ./cmd/memmy` succeeds; smoke-tested end-to-end (HTTP 200 on `initialize`, clean SIGTERM shutdown)

### US-016 — Final regression ✅
- [x] `go vet ./...` clean
- [x] `go build ./...` clean
- [x] `go test ./...` all green
- [x] `go test -race ./...` all green

## Round 2 — Polish (commits ceca0fe, d0bd2ca)

Architect-flagged improvements applied without changing the architectural envelope.

### US-101 — Full Malkov §4 Algorithm 4 neighbor heuristic ✅
- [x] `selectNeighborsHeuristic` admits `c` to `R` only when `dist(c, q) < dist(c, r)` for every already-chosen `r`; rejects fall to a discarded set; `keepPrunedConnections=true` fills remaining slots from discarded
- [x] `candidate` carries an optional `vec []float32` so pairwise distances are computed without re-reading vectors
- [x] `searchLayerTx` and `linkAndPruneTx` populate `vec` on every candidate
- [x] HNSW oracle test recall floor raised **0.93 → 0.95** and met (k=8, oversample=200, 50 queries on a 2000×32-dim corpus)

### US-102 — O(1) per-tenant counters backing Stats ✅
- [x] New collection `tenant_counters` (DESIGN.md §4.6) at `t/<tenantID>/counters/v` storing `{NodeCount, EdgeCount, SumNodeWeight, SumEdgeWeight}`
- [x] Maintained transactionally by every Graph mutation: `PutNode`/`UpdateNode`/`DeleteNode` and `PutEdge`/`UpdateEdge`/`DeleteEdge` — upsert detection captures old-weight delta; brand-new paths increment the count
- [x] `TenantStats` reads the counter record in O(1); no longer walks the edges bucket
- [x] `TestCounters_MatchBruteForce` exercises 400 randomized ops (insert/upsert/weight-bump/delete) and reconciles three views: shadow ↔ direct bbolt walk ↔ counter record
- [x] `TestCounters_DeleteNonexistent` guards no-op deletes against drift

### US-103 — Recall candidate map ✅
- [x] `Service.Recall` builds `map[string]candidate` once before the visit-scoring loop; per-visit lookup is O(1) instead of O(seeds)

### US-104 — README ✅
- [x] `README.md` orients new readers (stack, load-bearing principle, links to DESIGN/CLAUDE/IMPLEMENTATION, build & run pointer)

### US-105 — Final regression ✅
- [x] `go vet ./...` clean
- [x] `go build ./...` clean
- [x] `go test ./...` all green (**64 tests across 13 packages**)
- [x] `go test -race ./...` all green

## Round 3 — MCP stdio transport

### US-201 — MCP stdio adapter ✅
- [x] Same tool surface as the streamable HTTP transport (no schema divergence)
- [x] `Adapter.RunStdio(ctx)` blocks on `Server.Run` against `mcpsdk.StdioTransport{}`
- [x] `Adapter.RunTransport(ctx, t)` exposed for tests; the integration test wires the same tool surface through `mcpsdk.NewInMemoryTransports` and round-trips `memory.write`

### US-202 — Config: stdio mutually exclusive with all other transports ✅
- [x] `internal/config` recognizes a `stdio` transport (Enabled=true, no Addr)
- [x] `Config.Validate` rejects stdio + any other enabled transport, naming both
- [x] Stdio-only validates without Addr; HTTP transports still require Addr
- [x] Tests cover every combination: stdio-only accepted; stdio + mcp/grpc/http rejected; stdio + disabled-other accepted

### US-203 — Wire stdio into entrypoint ✅
- [x] `cmd/memmy/main.go` branches on configured transport: stdio runs `Adapter.RunStdio` directly; HTTP runs under suture
- [x] Logs always written to stderr — stdout is reserved for JSON-RPC frames
- [x] Signal-driven graceful shutdown still works (ctx cancel propagates through `Server.Run`)
- [x] Smoke test: `printf '<initialize JSON-RPC>\n' | ./memmy --config stdio.yaml` returns a valid initialize response on stdout, then EOF on stdin yields exit 0

### US-204 — Documentation ✅
- [x] DESIGN.md §10.2 describes both HTTP and stdio variants with the mutual-exclusivity rule and rationale
- [x] DESIGN.md §12 sample config includes the new `stdio` key
- [x] `memmy.example.yaml` shows both options with comments
- [x] `README.md` mentions stdio
- [x] This file: Round 3 documented

### US-205 — Final regression ✅
- [x] `go vet ./...` clean
- [x] `go build ./...` clean
- [x] `go test ./...` all green
- [x] `go test -race ./...` all green

## Round 5 — Gemini task-typed embeddings (gemini-embedding-2 default, dim 3072)

### US-401 — Embedder interface carries task hint ✅
- [x] `embed.EmbedTask` enum (Unspecified, RetrievalDocument, RetrievalQuery, plus reserved values for future tasks)
- [x] `Embedder.Embed(ctx, task, texts)` is the new signature; all call sites updated

### US-402 — Service Write uses RetrievalDocument; Recall uses RetrievalQuery ✅
- [x] `internal/service/write.go` embeds chunks with `EmbedTaskRetrievalDocument`
- [x] `internal/service/recall.go` embeds the query with `EmbedTaskRetrievalQuery`
- [x] No knob exposed — task choice is hard-coded by intent

### US-403 — Gemini embedder applies the task hint per model strategy ✅
- [x] `strategyFor(model)`: gemini-embedding-001 / text-embedding-004 → `strategyParam` (sets `EmbedContentConfig.TaskType`); everything else → `strategyPrefix` (in-band prompt prefix per gemini-embedding-2 spec)
- [x] `taskTypeAPIString` maps every documented Gemini task to the API enum string
- [x] `promptPrefix` locks the gemini-embedding-2 strings: `"title: none | text: "`, `"task: search result | query: "`, `"task: sentence similarity | query: "`, `"task: classification | query: "`
- [x] `EmbedContentConfig.OutputDimensionality` is set so the model returns the configured Dim
- [x] +5 white-box unit tests in `gemini_internal_test.go` (default-strategy, known-param-models, task strings, prefix wording, doc-vs-query distinguishability)

### US-404 — Default model gemini-embedding-2 / dim 3072 ✅
- [x] `config.Default()`: `Model: "gemini-embedding-2"`, `Dim: 3072`
- [x] `memmy.example.yaml` updated; comments explain the prefix scheme is automatic
- [x] DESIGN.md §12 sample updated
- [x] Validation still requires `api_key` when `backend == "gemini"`

### US-405 — Documentation ✅
- [x] DESIGN.md §5 Indexing notes RETRIEVAL_DOCUMENT at write time
- [x] DESIGN.md §6 Retrieval notes RETRIEVAL_QUERY at recall time
- [x] DESIGN.md §15 future-work merges model rotation and task-strategy rotation into one bullet
- [x] This file (Round 5)

### US-406 — Final regression ✅
- [x] `go vet ./...` clean
- [x] `go build ./...` clean
- [x] `go test ./...` all green — **104 tests** across the **13** packages in `internal/` + `cmd/` (9 ship `*_test.go` files; the remaining 4 are interface-only or entrypoint packages with no tests of their own)
- [x] `go test -race ./...` all green

## Round 6 — Explicit reinforcement: Reinforce / Demote / Mark with refractory + log-dampening

### US-601 — Service-layer Reinforce/Demote/Mark + refractory + log-dampening ✅
- [x] `internal/types/types.go` adds `ReinforceRequest/Result`, `DemoteRequest/Result`, `MarkRequest/Result`
- [x] `internal/service/service.go` Config gains `RefractoryPeriod` (default 60s), `LogDampening` (default true), `MarkMaxNodes` (default 256); `MemoryService` interface declares the three new ops
- [x] `internal/service/decay.go` adds `applyExplicitNodeBump` — explicit path with refractory gate + log-dampening — alongside existing `applyNodeDecayReinforce` (implicit Recall path, unchanged)
- [x] Refractory: when `now - LastTouched < RefractoryPeriod`, the closure drops the delta but still updates `LastTouched` and `AccessCount`. `skipped` is reset at the top of the closure so MVCC retries report the final invocation's outcome
- [x] Log-dampening: positive deltas scale by `1 - decayed/WeightCap` (clamped ≥ 0); negative deltas (Demote) bypass dampening
- [x] `internal/service/reinforce.go` (new) implements `Reinforce`, `Demote`, `Mark`. Mark walks `RecentNodeIDs`, applies recency-scaled `Strength * NodeDelta` per node through the same explicit-bump helper; clamps weight at `NodeFloor` for Demote

### US-602 — MCP tool surface ✅
- [x] `memory.reinforce`, `memory.demote`, `memory.mark` registered via the existing `registerToolWithTenantSchema` helper; tenant property rendered through any configured `TenantSchema`
- [x] Each handler honours `tenantErrorResult` for structured corrective payloads
- [x] Tool descriptions are LLM-facing — they describe WHEN to call and WHAT effect the user gains, with no mention of weight, decay, refractory, log-dampening, NodeDelta, or WeightCap

### US-603 — Service-level tests ✅
- [x] `internal/service/reinforce_test.go` (new): 10 tests covering Reinforce bump, refractory blocking + expiry, log-dampening near cap, log-dampening off matches NodeDelta, Demote clamp at NodeFloor, Mark window scoping, Mark refractory interaction, unknown-node error paths, Mark input validation
- [x] All tests use real bbolt (`t.TempDir()`) + `clock.Fake` + fake embedder

### US-604 — MCP integration tests ✅
- [x] `TestMCP_ToolList` asserts the three new tools register
- [x] Round-trip tests for `memory.reinforce`, `memory.demote`, `memory.mark` against an in-process MCP client over real bbolt
- [x] `TestMCP_Reinforce_TenantValidationError` covers the schema-error surface

### US-605 — DESIGN.md updates ✅
- [x] §8.2 rewritten: documents implicit (Recall) vs explicit (caller-driven) reinforcement paths, refractory period, log-dampening formula, Demote semantics, Mark synaptic-tag-capture analog
- [x] §9.1 `MemoryService` interface lists `Reinforce`, `Demote`, `Mark` with request/result shapes
- [x] §10.2 MCP adapter table extends to seven tools
- [x] §12 sample config block adds `refractory_period`, `log_dampening`, `mark_max_nodes`

### US-606 — Final regression ✅
- [x] `go vet ./...` clean
- [x] `go build ./...` clean
- [x] `go test ./...` all green — **118 tests** across **13 packages**
- [x] `go test -race ./...` all green
- [x] `memmy.example.yaml` updated with the three new memory.* keys + comments

## Round 4 — Optional tenant schema (single-server, MCP-rendered)

### US-301 — Friendly YAML tenant schema in config ✅
- [x] `internal/config` exposes a `tenant` section: top-level description, keys map (description / pattern / enum / required), optional `one_of` constraint
- [x] Empty tenant section preserves today's free-form behavior
- [x] `Config.Validate` compiles regex patterns at load time and rejects unknown `one_of` references / empty `one_of` sets
- [x] +4 config tests covering empty-section, valid-parse, invalid-regex, unknown-one_of-key, empty-one_of-set

### US-302 — Service-layer tenant validation + JSON Schema renderer ✅
- [x] `service.TenantSchema` with `Validate(map[string]string) error` and `JSONSchema() *jsonschema.Schema`
- [x] Validate enforces: unknown-key rejection, pattern, enum, required, `one_of` (exactly-one semantics)
- [x] Errors are typed `*service.ErrTenantInvalid` with `Code`, `Field`, `Got`, `Message`, and a `Payload(expected)` method that emits a JSON envelope with `error_code` / `field` / `got` / `message` / `expected_schema`
- [x] `Service.New` accepts an optional schema; nil = accept any tuple (unchanged path)
- [x] `Service.Write/Recall/Forget/Stats` validate tenant before deriving `TenantID`
- [x] No stored-tenant migration: `TenantID` is purely a hash of the validated tuple, so changing the schema and changing back leaves prior memories addressable
- [x] +12 unit tests in tenant_schema_test.go covering every validation rule, JSON schema shape, and payload round-trip

### US-303 — MCP adapter renders schema into the tool surface ✅
- [x] `mcpadapter.New` takes an optional `*service.TenantSchema`; when set, every tool's auto-derived InputSchema has its `tenant` property replaced with the rendered JSON Schema
- [x] Helper `registerToolWithTenantSchema` patches the tenant property uniformly across all four tools
- [x] Each handler catches `*service.ErrTenantInvalid` and returns `CallToolResult{IsError: true}` carrying the structured payload (defense in depth — the SDK pre-validates against InputSchema, so this fires only for cases the schema doesn't fully express)
- [x] +5 MCP integration tests: valid project tuple accepted; valid scope tuple accepted; invalid pattern surfaces an error (SDK or handler path); unknown tenant key rejected; schema description visible in InputSchema's tool listing

### US-304 — Documentation + sample yaml ✅
- [x] `memmy.example.yaml` shows the `tenant:` section using ONLY `project` (`pattern: ^/`) and `scope` (`enum: [global]`), with `one_of: [[project], [scope]]`
- [x] DESIGN.md §3.1 added: schema semantics, no-migration guarantee, error envelope shape
- [x] DESIGN.md §10.2 notes that the MCP adapter renders the schema into each tool's InputSchema and produces structured corrective errors
- [x] DESIGN.md §12 sample includes the tenant block
- [x] `README.md` mentions the schema feature with a pointer to the example
- [x] This file: Round 4 documented

### US-305 — Final regression ✅
- [x] `go vet ./...` clean
- [x] `go build ./...` clean
- [x] `go test ./...` all green (**94 tests across 13 packages**)
- [x] `go test -race ./...` all green

## Round 7 — Library facade for in-process embedding

memmy is now embeddable directly via `import "github.com/Cidan/memmy"`. The daemon (`cmd/memmy`) and the library share the same `MemoryService`; the facade simply skips the transport layer.

### US-701 — Facade package at module root ✅
- [x] `memmy.go` defines `package memmy` at the module root
- [x] Re-exports `Service` (= `service.MemoryService`) plus every request/result value type from `internal/types` via Go type aliases (`WriteRequest`, `RecallRequest`, `RecallHit`, `ScoreBreakdown`, `Forget*`, `Stats*`, `Reinforce*`, `Demote*`, `Mark*`, `EdgeKind` and its three constants)
- [x] Re-exports `Embedder` and `EmbedTask` (with all task constants), `Clock` / `RealClock` / `FakeClock`, `ServiceConfig` (= `service.Config`), `TenantSchema` / `TenantSchemaConfig` / `TenantKeyConfig`, `ErrTenantInvalid`, `HNSWConfig`
- [x] Constructors: `NewFakeEmbedder(dim)`, `NewGeminiEmbedder(ctx, GeminiEmbedderOptions)`, `NewFakeClock(t)`, `NewTenantSchema(TenantSchemaConfig)` (returns nil for empty configs), `DefaultServiceConfig()`, `DefaultHNSWConfig()`
- [x] `Options` struct: `DBPath` and `Embedder` required; optional `Clock`, `ServiceConfig` (`*ServiceConfig`), `TenantSchema`, `HNSW` (`*HNSWConfig`), `FlatScanThreshold`, `OpenTimeout`, `HNSWRandSeed`. Embedder dim drives storage dim — no separate `Dim` knob to mismatch
- [x] `Open(Options) (Service, io.Closer, error)`: opens bbolt, wires the service, returns the storage handle as `io.Closer` so callers `defer closer.Close()`. Embedder lifecycle is the caller's
- [x] No transports start — library mode is daemon-free. To run a transport, use `cmd/memmy` with a YAML config
- [x] Pointer-typed tunable overrides eliminate the partial-zero footgun: `Options.ServiceConfig` and `Options.HNSW` are pointers (`nil` → defaults, non-nil → caller's complete config). Field-by-field merge would silently override intentional zero values for `RefractoryPeriod` / `LogDampening`. Doc comments tell callers to start from `DefaultServiceConfig()` / `DefaultHNSWConfig()`, mutate, then take the address
- [x] `HNSWConfig` re-export is annotated as bbolt-specific so a future second backend triggers an explicit revisit

### US-702 — Library tests ✅
- [x] `memmy_test.go` (package `memmy_test`): 19 tests covering required-field validation (DBPath, Embedder, zero-dim), defaults applied, Write→Recall round-trip, Reinforce/Demote/Mark with refractory advancement, Stats reflects writes, Forget by MessageID, TenantSchema accept-valid + reject-invalid (three error codes via `errors.As(err, *ErrTenantInvalid)`), empty schema config returns nil, partial `ServiceConfig` and `HNSW` overrides via the pointer pattern, Close releases the storage handle, Close is idempotent, and a custom user-supplied `Embedder` flowing through `Open`
- [x] All tests use the real reference backend in `t.TempDir()` plus a `FakeClock`, never mocks for storage (the underlying backend was migrated to SQLite in Round 8 — same test surface, same `t.TempDir()` pattern)

### US-703 — Documentation ✅
- [x] `README.md`: new "Use as a library" section with full quickstart (Gemini embedder, tenant schema, Open, Write, Recall) and notes on Embedder/TenantSchema/Close/transport semantics
- [x] This file: Round 7

### US-704 — Final regression ✅
- [x] `go vet ./...` clean
- [x] `go build ./...` clean
- [x] `go test ./...` all green — **137 tests** across **14 packages** (the new top-level `memmy` package adds 19 tests; module count rises from 13 → 14)
- [x] `go test -race ./...` all green
- [x] No changes to `internal/`; the facade is purely additive

## Round 8 — SQLite (WAL) replaces bbolt as the v1 reference backend

bbolt's single-process file-lock semantics blocked the use case of
multiple host processes (e.g. several `ask` CLI agents) embedding memmy
against one shared corpus. SQLite in WAL mode coordinates many readers
+ one writer across processes through file locks, with the same
single-tx-write atomicity the existing code already assumes.

### US-801 — sqlite storage package replaces internal/storage/bbolt ✅
- [x] `internal/storage/bbolt/` deleted wholesale (no migration — memories blown away)
- [x] `internal/storage/sqlite/` ships `db.go`, `keys.go`, `codec.go`, `counters.go`, `graph.go`, `scanners.go`, `hnsw.go`, `vectorindex.go` plus the test suite
- [x] Two `*sql.DB` handles per Storage: writer DSN with `_txlock=immediate` capped at one connection (RESERVED lock upfront prevents SQLITE_BUSY upgrade races); reader DSN with deferred mode for concurrent snapshot reads
- [x] Pragmas applied at open: `_journal_mode=WAL`, `_synchronous=NORMAL`, `_foreign_keys=ON`, `_busy_timeout` (default 5s, configurable)
- [x] Schema (DESIGN.md §4.7) bootstrapped idempotently on Open: `tenants`, `nodes`, `messages`, `vectors`, `hnsw_meta`, `hnsw_records`, `edges_out`, `edges_in`, `counters`, `meta` — all `WITHOUT ROWID`
- [x] gob blob format for structured records and raw little-endian float32 bytes for vectors are unchanged from the bbolt era

### US-802 — Graph + VectorIndex parity ✅
- [x] Every Graph method ports to SQL with the same atomicity guarantees: `PutNode`/`UpdateNode`/`DeleteNode` adjust counters in the same write tx; `PutEdge`/`UpdateEdge`/`DeleteEdge` write both `edges_out` (tenant, from, to) and `edges_in` (tenant, to, from) inside one tx
- [x] HNSW algorithms (`hnswInsertTx`, `hnswDeleteTx`, `hnswDetachTx`, `searchLayerTx`, `greedyDescentTx`, `linkAndPruneTx`, `pickEntryPointTx`, neighbor-selection heuristic, candidate PQs, layer sampling) ported verbatim, swapping the four `*bbolt.Tx`-bucket helpers for SQL row helpers
- [x] Counters (gob-encoded `tenantCounters` blob per tenant) maintained transactionally; `TenantStats` reads counters + `hnsw_meta` in O(1)
- [x] Scanners (`RecentNodeIDs`, `NodesForMessage`, `MessageIDsBefore`) translate ULID-prefix reverse cursor scans into `WHERE tenant=? AND id >= ? ORDER BY id DESC LIMIT N`
- [x] MVCC closure-retry safety: `UpdateNode` / `UpdateEdge` keep the "reset closure-captured state at top of body" discipline (architect note 2026-04-27); under SQLite WAL with BEGIN IMMEDIATE the retry case shouldn't fire, but the discipline costs nothing and protects future Postgres/Spanner ports

### US-803 — Wiring and library facade ✅
- [x] `internal/config/config.go`: `StorageConfig.Backend` defaults to `"sqlite"`; `SQLite SQLiteStorageConfig` (path + busy_timeout) replaces `BBolt`; validation error message updated
- [x] `cmd/memmy/main.go` opens via `sqlitestore.Open` and rejects `bbolt` with a clear error
- [x] `memmy.go` library facade: bbolt import removed; `HNSWConfig` re-exported from `sqlitestore`; `Options.OpenTimeout` renamed `Options.BusyTimeout` (the SQLite busy-timeout pragma window)
- [x] `memmy_test.go`: `TestClose_ReleasesDBLock` → `TestClose_ReleasesDBHandle`; `OpenTimeout` references renamed
- [x] `internal/transport/mcp/mcp_test.go` and `internal/service/service_test.go` switched to sqlitestore; identical test surface
- [x] `internal/service/{write,forget}.go` doc comments updated to reference SQLite

### US-804 — Tests ported and extended ✅
- [x] Storage suite ported file-for-file: `db_test.go`, `graph_test.go`, `counters_test.go`, `hnsw_test.go`, `vectorindex_test.go`, `codec_export_test.go`, `storage_testhelp_test.go`. Real SQLite in `t.TempDir()`; no mocks
- [x] HNSW oracle test recall@8 ≥ 0.95 with `oversample=200` over a 2000×32-dim corpus — same floor that bbolt was meeting
- [x] New: `TestOpen_WALMode` PRAGMA-checks `journal_mode=wal` post-open; `TestOpen_SchemaVersionRecorded` confirms the schema marker; `TestGraph_Edge_UpdateEdge_ClosureErrorAborts` proves edges_in + edges_out both roll back on closure error
- [x] New: `TestMultiHandle_ConcurrentReadWrite` opens two `*Storage` handles against the same DB file simultaneously, writes through one, and reads through the other — the property bbolt could not satisfy

### US-805 — bbolt fully removed ✅
- [x] `internal/storage/bbolt/` directory deleted
- [x] `go.etcd.io/bbolt` dropped from `go.mod` / `go.sum`
- [x] `github.com/mattn/go-sqlite3` added to approved-deps list (CLAUDE.md)
- [x] No live code references to bbolt anywhere in the repo

### US-806 — Documentation ✅
- [x] DESIGN.md §0 #1 / §0 #3 / §1 / §2 / §4.7 / §13.1 / §13.3 / §14 / §15 updated to reference SQLite as the v1 reference backend with the new schema sketch
- [x] CLAUDE.md Stack section, Approved-deps list, Architectural rules, and Testing section all reference SQLite
- [x] README.md describes SQLite WAL multi-process semantics and the CGO build requirement
- [x] `memmy.example.yaml` ships `backend: sqlite` with `sqlite.path` and a documented `busy_timeout` knob

### US-807 — Final regression ✅
- [x] `go vet ./...` clean
- [x] `go build ./...` clean
- [x] `go test ./...` all green
- [x] `go test -race ./...` all green

## Round 9 — Validation framework (`memmy-eval`)

Local-only CLI for measuring memmy's retrieval quality and decay /
reinforcement dynamics against a controllable corpus extracted from
Claude Code session JSONL. Datasets live OUTSIDE the repo at
`$MEMMY_EVAL_HOME` (default `~/.local/share/memmy-eval/<name>/`); only
the framework code ships in the repo.

Approved deps added: `github.com/spf13/cobra`,
`github.com/schollz/progressbar/v3`. Updated CLAUDE.md approved-deps
list.

### US-EVAL-001 — Approved deps + CLAUDE.md ✅
- [x] `go.mod` declares `cobra` v1.10.2 and `schollz/progressbar/v3` v3.19.0
- [x] CLAUDE.md approved-deps list updated

### US-EVAL-002 — `internal/eval/dataset` ✅
- [x] `Open(root, name)` resolves `$MEMMY_EVAL_HOME` (or
      `~/.local/share/memmy-eval`), creates `<name>/runs/`, idempotent
- [x] Typed paths for `corpus.sqlite`, `queries.sqlite`, `manifest.json`,
      `runs/<id>/`
- [x] `ListDatasets(root)` walks the root, returns per-dataset stats
      from manifest + best-effort directory walks; missing root is silent
- [x] Tests cover idempotent re-Open, run-dir creation, env-override
      resolution, name-validation rejection, and listing

### US-EVAL-003 — `internal/eval/corpus` ✅
- [x] `Extract(path, fn)` streams turns from a single .jsonl OR a
      directory of them; honors `Turn` callback errors
- [x] Decodes user/assistant turns; skips file-history-snapshot, system,
      queue-operation, sidechain
- [x] Polymorphic `message.content` decode: string OR array of
      typed blocks (text only; thinking, tool_use, tool_result skipped)
- [x] `OpenStore` materializes corpus.sqlite with `source_files` (dedup)
      + `turns` tables; chronological iteration; stable `SnapshotHash`
- [x] Tests cover filter, polymorphic decode, dir lex-ordering, callback
      error propagation, store dedup, hash stability, file-hash helper

### US-EVAL-004 — `internal/eval/embedcache` ✅
- [x] Content-addressed sqlite cache keyed by
      `(model_id, dim, sha256(text))`, raw little-endian f32 vectors
- [x] `EmbedBatch(ctx, embedder, modelID, task, texts)` returns input-
      ordered vectors; cache hits skip embedder calls; misses are
      embedded then cached
- [x] Tests cover round-trip, miss reporting, dedup of repeated calls,
      mixed hit/miss order preservation, and count

### US-EVAL-005 — `internal/eval/queries` ✅
- [x] `Generator` + `Judge` interfaces; `FakeGenerator` (paraphrase +
      distractor + tagged stubs for other categories) and `FakeJudge`
      (token-overlap scoring) ship in the same package
- [x] `OpenStore` materializes queries.sqlite with `queries` table
      (per-query embedding blob) + `judgments` cache table
- [x] `Put` is idempotent; `CountForGeneration(version, snapshot,
      category)` powers the dedup key
- [x] `PutEmbedding` / `Embedding(dim)` round-trip cached query vectors
- [x] Tests cover generator output shape, fake-judge token overlap,
      store dedup, generation counting, embedding round-trip

### US-EVAL-006 — `internal/eval/inspect` ✅
- [x] Read-only sqlite reader (`mode=ro`, `_query_only=1`) opens the
      same db file the live service writes; no shared connection pool
- [x] `ListTenants`, `ListNodes(tenant)`, `NodeState(tenant, id)`,
      `NodeStates(tenant, ids)` decode gob-encoded Node records
- [x] Test writes via the memmy facade then reads back via inspect;
      weights, edge counts, last-touched all round-trip

### US-EVAL-007 — `internal/eval/manifest` ✅
- [x] `DatasetManifest` (sessions source, embedder, chunk count, snapshot
      hash, timestamps) and `RunManifest` (run id, memmy git SHA,
      service+HNSW config blobs, queries executed)
- [x] Atomic JSON write via tmp+rename; SchemaVersion field
- [x] `MemmyGitSHA()` reads `runtime/debug.ReadBuildInfo` `vcs.revision`,
      falls back to "unknown" under `go test`
- [x] Tests cover round-trip for both manifest types and SHA non-empty

### US-EVAL-008 — `internal/eval/harness` ✅
- [x] `Ingest` walks JSONL via `corpus.Extract`, persists turns via
      `corpus.Store`, chunks via `internal/chunker.Default`, embeds via
      `embedcache.EmbedBatch`. Idempotent per source file
      (path+mtime+sha256). Optional `Progress` callback for cobra bars.
- [x] `Replay` reads turns chronologically, drives a `memmy.FakeClock`
      to each turn's timestamp, calls `svc.Write` under tenant
      `{agent: memmy-eval, dataset: <name>}`. The wrapped embedder
      consults the embedcache so re-replay never re-embeds. Returns a
      live `memmy.Service` handle for the query battery.
- [x] `RunQueries` snapshots per-tenant node state pre-Recall (via
      inspect), executes Recall, captures hits + score breakdowns +
      post-state for the top-K. Optional `AdvanceClock` between queries
      defeats the default 60s explicit-bump refractory window.
- [x] `harness_test.go` runs the full ingest → replay → queries → metrics
      pipeline against synthetic JSONL in `t.TempDir()` using fake
      embedder + fake judge. Verifies dedup, clock advancement to last
      turn timestamp, summary.json existence, recall in [0, 1].

### US-EVAL-009 — `internal/eval/metrics` ✅
- [x] `Compute(QueryResult, turnUUIDForNode)` produces a flat `QueryRow`:
      hit IDs, gold flags, recall@1/3/5/8, MRR, nDCG, plus
      `ReinforcementSum`/`Max` from pre/post `inspect.NodeState` deltas
- [x] `Aggregate(runID, dataset, rows)` rolls up overall + per-category
      averages
- [x] `WriteRun(outDir, rows, summary)` emits `queries.jsonl` (one row
      per query) and `summary.json` (aggregate)
- [x] Tests cover recall@k boundary cases, MRR / NDCG known-input
      values, no-gold zero credit, reinforcement-from-pre/post, and
      aggregate averaging

### US-EVAL-010 — `internal/eval/sweep` ✅
- [x] `Load(path)` parses sweep YAML: `base` config (optional) + `matrix`
      of `{name, overrides, hnsw}` entries
- [x] `ApplyServiceOverrides` and `ApplyHNSWOverrides` JSON-marshal the
      base config, splice the override map in by key, unmarshal back
      into the typed config — no per-field knowledge needed in YAML
- [x] Tests cover load shape, single-field override survives untouched
      siblings, missing-matrix rejection

### US-EVAL-011 — `cmd/memmy-eval` cobra binary ✅
- [x] Subcommands: `ingest`, `queries`, `run`, `sweep`, `ls`
- [x] `ingest --sessions PATH --dataset NAME --embedder fake|gemini`;
      progress bar via `schollz/progressbar/v3` for files + chunks;
      idempotent re-runs print `skipped(dup)=N`; updates dataset
      manifest with chunk count + snapshot hash
- [x] `queries --dataset NAME --n N --categories paraphrase,distractor,...`
      generates with the fake generator; updates dataset manifest
      QueryCount on success
- [x] `run --dataset NAME --config PATH --embedder ... --k K --hops H`
      replays into a fresh per-run memmy db under `runs/<run_id>/`,
      executes the query battery, writes `queries.jsonl` +
      `summary.json` + `manifest.json`
- [x] `sweep --dataset NAME --matrix PATH` runs each matrix entry as
      a fresh `executeRun`, one db per entry; sequential v1 (parallel
      hook reserved for a follow-up)
- [x] `ls` prints datasets + chunk + query + run counts via tabwriter
- [x] `--help` renders cleanly on root and every subcommand
- [x] Smoke run on `~/.claude/projects/-home-antonio-git-nanomite`
      (3 files, 274 turns, 1798 chunks, 10 queries, 3-entry sweep) all
      pass

### US-EVAL-012 — End-to-end test ✅
- [x] `internal/eval/harness/harness_test.go` (synthetic JSONL → ingest
      → queries → run → metrics) under `t.TempDir()`. Uses fake
      embedder + fake judge — no API key required
- [x] Test runs in <2s on a developer laptop

### US-EVAL-013 — Sample configs + README ✅
- [x] `eval/README.md` orients new readers (pipeline diagram, fake
      quickstart, Gemini quickstart, sweep example, dataset root,
      result interpretation, deltas-not-absolutes note)
- [x] `eval/configs/baseline.yaml` consumable by `run --config`
- [x] `eval/configs/sweep.yaml` consumable by `sweep --matrix` (3 entries)
- [x] `.gitignore` excludes `eval/fixtures/`, `eval/notebooks/outputs/`,
      `/memmy-eval`, `/memmy`

### US-EVAL-014 — Smoke run on real session dir ✅
- [x] `MEMMY_EVAL_HOME=/tmp/memmy-eval-smoke memmy-eval ingest
      --sessions /home/antonio/.claude/projects/-home-antonio-git-nanomite
      --dataset nanomite-smoke --embedder fake --fake-dim 32 --limit 3`
      — files=3 turns=274 chunks=1798
- [x] Re-ingest = 0 turns added, `skipped(dup)=3`
- [x] `memmy-eval queries --dataset nanomite-smoke --n 5` produced 10
      queries; `memmy-eval ls` reflects the manifest counts
- [x] `memmy-eval run --dataset nanomite-smoke --embedder fake
      --fake-dim 32 --k 5 --hops 1` produced
      `runs/run-XXX/{summary.json,queries.jsonl,manifest.json,memmy.db}`
      with non-zero `overall_reinforcement_mean`
- [x] `memmy-eval sweep --dataset nanomite-smoke --matrix
      eval/configs/sweep.yaml` produced 3 per-entry summary files;
      `high-reinforce` shows reinforce_mean ≈ 12.5 vs baseline ≈ 5.0
      (the framework's whole point — measuring config-induced dynamics
      deltas)

### US-EVAL-015 — Final regression ✅
- [x] `go vet ./...` clean
- [x] `go build ./...` clean (binary at `cmd/memmy-eval`)
- [x] `go test ./...` all green
- [x] DESIGN.md untouched (the framework is tooling, not core architecture)

## Round 9b — Validation framework: test-coverage gap fill

Verifier of Round 9 flagged two PARTIAL-coverage areas (sweep e2e and
non-zero-reinforcement assertion). User asked to "heavily test this
code" with two explicit exclusions: Gemini embedder (no API key in
tests) and the cobra binary (per user direction). This round adds 8
new test files covering 36 new tests; one production-code defect fix
fell out of the manifest concurrent-writer test.

### US-EVAL-T01 — Sweep multi-entry e2e ✅
- [x] `internal/eval/sweep/sweep_e2e_test.go::TestSweep_TwoEntriesEndToEnd`
      drives a 2-entry matrix through harness.Replay + RunQueries +
      metrics.WriteRun for each entry; asserts both summary.json files
      exist AND that NodeDelta=5 produces strictly larger reinforce_mean
      than NodeDelta=0.1 (proves the override actually flowed through).

### US-EVAL-T02 — Corpus extractor edge cases ✅
- [x] `internal/eval/corpus/edge_cases_test.go` (8 tests): malformed
      JSON propagates file+line, embedded `\n`+`\t` round-trip through
      extract+store, thinking/tool_use-only assistant message skipped,
      empty-content user message skipped, missing timestamp parses to
      zero time, HashFile on 0-byte file matches sha256 of empty input,
      HashFile missing-file wraps `os.ErrNotExist`,
      ListJSONLFiles missing-path errors.

### US-EVAL-T03 — Embedcache concurrency + defensive validation ✅
- [x] `internal/eval/embedcache/edge_cases_test.go` (5 tests): 8
      goroutines × 5 texts with overlap → quiescent re-call asserts
      zero new embedder calls; Put with mismatched dim returns error
      mentioning both numbers; corrupted vector row (manually written)
      returns error rather than silently truncating; Open empty path
      errors; Close+re-Open survives with rows readable.

### US-EVAL-T04 — Queries top-up dedup ✅
- [x] `internal/eval/queries/topup_test.go` (3 tests): N1=3 then N2=5
      with 2 collisions yields total=5 and original GeneratedAt +
      GoldTurnUUIDs preserved; different corpus_snapshot still bound to
      query_id PK; ByCategory filters cleanly.

### US-EVAL-T05 — Manifest atomic write + schema-version skew ✅
- [x] `internal/eval/manifest/edge_cases_test.go` (6 tests): empty path
      errors; missing file wraps `os.ErrNotExist`; **16 concurrent
      racers leave a valid manifest with no .tmp leftovers**; future
      SchemaVersion=99 + unknown field still decodes; WriteRun empty
      path errors; ReadRun missing-file errors.
- [x] **Production-code fix exposed by this round:** `manifest.writeJSON`
      now uses `os.CreateTemp(dir, base+".*.tmp")` instead of a fixed
      `path + ".tmp"`. Old code lost rename races under concurrent
      writers (the test originally failed with "rename: ... no such
      file or directory" for ~5 of 16 goroutines). New code is
      atomic-per-call; the 16-racer test passes.

### US-EVAL-T06 — Replay edge cases ✅
- [x] `internal/eval/harness/replay_edge_test.go` (3 tests): Replay
      against an empty corpus succeeds with TurnsReplayed=0 and a
      usable Service handle; second Replay against the cache-primed
      corpus calls the underlying embedder ZERO times (counting
      wrapper); custom TenantTuple flows through and Recall under it
      returns hits.

### US-EVAL-T07 — Inspect read-during-write semantics ✅
- [x] `internal/eval/inspect/concurrency_test.go` (3 tests): inspect
      Reader stays open while 4 concurrent Service.Write goroutines
      land new nodes; subsequent NodeStates returns all of them;
      NodeStates silently omits unknown IDs (mixed real/unknown list);
      ListNodes against an unknown tenant returns empty.

### US-EVAL-T08 — Metrics boundary conditions ✅
- [x] `internal/eval/metrics/boundaries_test.go` (5 tests): zero hits
      yields all-zero metrics; gold at rank 8 yields Recall@8=1 +
      MRR=1/8; multiple gold hits (ranks 1,3) yields NDCG ∈ (0,1] and
      MRR=1; WriteRun with empty rows produces a valid summary.json +
      0-byte queries.jsonl; Aggregate with empty-string Category bucket
      survives without panic.

### US-EVAL-T09 — Final regression ✅
- [x] `go vet ./...` clean
- [x] `go build ./...` clean
- [x] `go test ./...` 217 tests pass across 24 packages (vs 182 baseline
      = +35 new tests post-deslop). The pre-existing storage / service /
      mcp / types test counts are unchanged — no regressions.
- [x] Verifier agent: APPROVE
- [x] ai-slop-cleaner pass: removed a misleading "in case future tests"
      comment on a test helper, deleted a duplicate clock-progression
      test (`TestReplay_FakeClockMonotonicProgression` overlapped
      `TestReplay_AdvancesClockToTurnTimestamps` from the prior round),
      cleaned up the now-unused `time` import that fell out of that
      deletion. Post-deslop regression re-run: clean.

## Round 10 — Neo4j replaces SQLite end-to-end

memmy migrates from SQLite (with hand-rolled HNSW) to Neo4j (Bolt protocol, native vector index + first-class graph). Schema bundled in the binary via `embed.FS`; migrations are explicit (`memmy migrate` subcommand; library exposes `memmy.Migrate(ctx, MigrationOptions{...})`). The `cmd/memmy` server is pure-Go (`CGO_ENABLED=0` works). The optional `cmd/memmy-eval` validation harness still imports `mattn/go-sqlite3` for its local data stores (`corpus.sqlite`, `embedcache.sqlite`, `queries.sqlite`); that's a tooling-only dependency, unrelated to memmy's runtime storage.

### US-NEO-001 — Storage scaffold ✅
- [x] `internal/storage/neo4j/{db,migrate,graph,vectorindex,counters,scanners,codec}.go` with the full Graph + VectorIndex port over the Bolt driver.
- [x] `internal/storage/neo4j/migrations/{001_constraints,002_vector_index}.cypher` embedded via `embed.FS`. Migration system runs each statement in its own auto-commit (Neo4j forbids schema + data ops in one tx) and records applied versions on `:Migration` nodes.
- [x] Two distinct graphs share the store: Hebbian memory edges (`STRUCTURAL` / `CORETRIEVAL` / `COTRAVERSAL` relationship types) and the native vector index (`node_embedding_idx` on `:Node(embedding)` with `cosine` similarity).

### US-NEO-002 — Single-edge-per-pair invariant ✅
- [x] memmy's contract is one edge per `(from, to)` regardless of `Kind`. Cypher relationship types are part of the rel identity, so `putEdgeTx` deletes any existing different-typed rel before MERGEing the requested type. `UpdateEdge` recreates the relationship with a new type when the closure promotes Kind (e.g. STRUCTURAL → CORETRIEVAL during Phase 5 of recall).

### US-NEO-003 — Cosine semantics ✅
- [x] Neo4j's `vector.similarity.cosine` returns `(1 + cos)/2 ∈ [0, 1]`; the VectorIndex contract is the standard cosine in `[-1, 1]`. The flat-scan and native-index paths both apply the inverse mapping (`2*sim - 1`) before returning, so the service layer's `simScore` math is unchanged from the SQLite era.

### US-NEO-004 — Counter accuracy under DETACH DELETE ✅
- [x] `DeleteNode` reads the count and weight-sum of every attached edge (undirected pattern, self-loops are forbidden so no double-count) and adjusts the per-tenant counter for them before the DETACH DELETE silently drops the rels.

### US-NEO-005 — Test scaffolding ✅
- [x] `internal/storage/neo4j/neo4jtest` exposes `Open(t, dim, opts...)`, `OpenSharedTenant(t, dim, prefix, opts...)`, `WithFlatScanThreshold(n)`, and `SkipIfUnset(t)`. Per-test tenant prefix; `t.Cleanup` DETACH DELETEs every Node/TenantInfo created under that prefix, matching both literal-prefix tenants (storage tests) and SHA-derived TenantIDs whose `tuple_json` mentions the prefix (service / harness tests). Tests skip cleanly when `NEO4J_PASSWORD` is unset.

### US-NEO-006 — Library facade rewrite ✅
- [x] `memmy.go` rewritten with `Options{Neo4jURI, Neo4jUser, Neo4jPassword, Neo4jDatabase, ConnectTimeout, Embedder, Clock, ServiceConfig, TenantSchema, FlatScanThreshold, SkipMigrationCheck}`. Schema-version guard at `Open` returns a remediation message pointing to `memmy.Migrate()`. New `MigrationOptions` + `Migrate(ctx, ...)` entry point.

### US-NEO-007 — Config Neo4j-only ✅
- [x] `internal/config/config.go` drops `SQLiteStorageConfig` and `HNSWParams`; `StorageConfig` now nests `Neo4jStorageConfig{URI, User, Password, Database, ConnectTimeout}`. `Default()` ships localhost defaults; `Validate()` enforces `Backend == "neo4j"` and required Neo4j fields. Password supports `${ENV_VAR}` expansion (also applied to `embedder.gemini.api_key`). `memmy.example.yaml` updated to the new shape.

### US-NEO-008 — Cobra-driven CLI ✅
- [x] `cmd/memmy/main.go` converted to cobra. `memmy serve --config <path>` (default if no subcommand) opens the service, schema-version-guards, registers transports under suture. `memmy migrate --config <path>` applies pending Cypher migrations and exits 0 on success. Removed the `bbolt` legacy-error branch — all backends except `neo4j` now error from `Validate`.

### US-NEO-009 — Eval framework wiring ✅
- [x] `internal/eval/inspect/inspect.go` rewritten as a Bolt-backed read-only window. New `inspect.Connection{URI, User, Password, Database}` is the unit threaded through `harness.ReplayOptions.Neo4j` and `harness.RunQueriesOptions.InspectConn`. `MemmyDBPath`, `HNSW`, and `HNSWRandSeed` fields are gone. The per-run baseline cache in `cmd/memmy-eval/run.go` is gone — Neo4j replay is fast enough that it's not a perf cliff.

### US-NEO-010 — Sweep + manifest cleanup ✅
- [x] `internal/eval/sweep/apply.go` drops `ApplyHNSWOverrides`. `Entry` drops the `HNSW` field. `cmd/memmy-eval/{run,sweep}.go` drop every HNSW reference and the `--hnsw-seed` / `--memmy-db` flags; Neo4j connection now reads from `NEO4J_*` env vars. `internal/eval/manifest/manifest.go` drops `HNSWConfigJSON` and `HNSWRandSeed` from `RunManifest`.

### US-NEO-011 — Storage tests ✅
- [x] `internal/storage/neo4j/{graph,counters,vectorindex,oracle,multiprocess}_test.go` ported from the SQLite suite. The oracle test enforces native-index recall@8 ≥ 0.85 vs flat-scan ground truth over 500×32-dim synthetic vectors (Neo4j's vector index is approximate — the recall floor is what matters, not the SQLite-HNSW exact numbers). Counters test runs 200 randomized ops against a brute-force shadow.

### US-NEO-012 — Service / transport / library / eval tests ✅
- [x] `internal/service/{service,reinforce}_test.go`, `internal/transport/mcp/mcp_test.go`, `memmy_test.go`, `internal/eval/inspect/{inspect,concurrency}_test.go`, `internal/eval/harness/{harness,replay_edge}_test.go`, `internal/eval/sweep/sweep_e2e_test.go` all migrated to `neo4jtest.Open`. Tenant tuples bake in the per-test prefix so the cleanup hook catches every row.

### US-NEO-013 — SQLite removal (memmy storage layer) ✅
- [x] `internal/storage/sqlite/` deleted. `mattn/go-sqlite3` is now imported only by the eval framework's local data stores (`internal/eval/{corpus,embedcache,queries}/`). The `cmd/memmy` server has no SQLite or CGO dependency.

### US-NEO-014 — Documentation ✅
- [x] `README.md`: setup section now documents Neo4j install (Docker / brew / Desktop), `memmy migrate` workflow, `NEO4J_*` env vars for tests, pure-Go build (no CGO).
- [x] `memmy.example.yaml`: `storage.neo4j.{uri,user,password,database,connect_timeout}` block; `vector_index.hnsw` knobs gone (Neo4j has no exposed HNSW tunables).
- [x] `IMPLEMENTATION.md`: this Round 10 entry.

### US-NEO-015 — Final regression ✅
- [x] `NEO4J_PASSWORD=… go vet ./...` clean
- [x] `NEO4J_PASSWORD=… go build ./...` clean
- [x] `NEO4J_PASSWORD=… go test ./...` 214 tests pass across 25 packages.
- [x] End-to-end smoke: `memmy migrate --config memmy.example.yaml` then `memmy serve --config memmy.example.yaml` against the local Neo4j succeeds.
