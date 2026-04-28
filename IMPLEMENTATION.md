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
- [x] `memmy_test.go` (package `memmy_test`): 19 tests covering required-field validation (DBPath, Embedder, zero-dim), defaults applied, Write→Recall round-trip, Reinforce/Demote/Mark with refractory advancement, Stats reflects writes, Forget by MessageID, TenantSchema accept-valid + reject-invalid (three error codes via `errors.As(err, *ErrTenantInvalid)`), empty schema config returns nil, partial `ServiceConfig` and `HNSW` overrides via the pointer pattern, Close releases the bbolt file lock, Close is idempotent, and a custom user-supplied `Embedder` flowing through `Open`
- [x] All tests use real bbolt in `t.TempDir()` plus a `FakeClock`, never mocks for storage

### US-703 — Documentation ✅
- [x] `README.md`: new "Use as a library" section with full quickstart (Gemini embedder, tenant schema, Open, Write, Recall) and notes on Embedder/TenantSchema/Close/transport semantics
- [x] This file: Round 7

### US-704 — Final regression ✅
- [x] `go vet ./...` clean
- [x] `go build ./...` clean
- [x] `go test ./...` all green — **137 tests** across **14 packages** (the new top-level `memmy` package adds 19 tests; module count rises from 13 → 14)
- [x] `go test -race ./...` all green
- [x] No changes to `internal/`; the facade is purely additive
