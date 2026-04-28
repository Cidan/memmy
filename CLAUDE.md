# memmy — Project Conventions for Claude

memmy is an LLM memory system written in pure Go (toolchain: **Go 1.26.2**), exposed over MCP and (future) gRPC + HTTP.
**`DESIGN.md` is the source of truth for architecture and design rationale.** Read it first; the design principles in §0 are load-bearing.
**`IMPLEMENTATION.md` is the running checklist** for what's built and what's left. Update it as work lands.
This file captures conventions and preferences for working in this repo.

## Stack

- **Go 1.26.2.** Pure-Go where reasonable — avoid CGO.
- **MCP server library**: `github.com/modelcontextprotocol/go-sdk` (the official Go SDK). v1 transport.
- **`suture`** — process supervision.
- **Storage backend — pluggable.** v1 reference: `bbolt`. Future targets: Postgres, MariaDB, Bigtable, Spanner, badger, pebble. All hide behind the `Graph` and `VectorIndex` interfaces (DESIGN.md §9.2).
- **`go-genai`** — Gemini embeddings provider (first impl).

No bleve. No external search engine. **The configured storage backend is the single source of truth.**

## Architectural Rules

These are restatements of the load-bearing principles in DESIGN.md §0. Do not deviate without updating DESIGN.md first and discussing with the user.

- **One source of truth: the database.** Vectors, HNSW links, and everything else live in the configured storage backend. No secondary store, no in-memory index file. The reference backend is bbolt; the same logical model maps to Postgres, Bigtable, Spanner, etc.
- **Storage is pluggable; retrieval policy is not.** Backends are interchangeable behind `Graph` + `VectorIndex`. Retrieval scoring, oversampling, reinforcement, decay, and HNSW correctness live in the Memory Service and do not change per backend.
- **Stateless service.** memmy holds NO in-memory data state across requests. Permitted: connection pools (DB, embedder, client transport sessions), config (read-only), process-local semaphores. Forbidden: caches of database content, in-memory tenant registries, in-memory `HNSWMeta` copies, accumulators, indexes. Per-request transient state (heaps, queues, visited sets) is created and freed in-request. This is what enables N-node horizontal scale-out against a multi-writer backend.
- **Transport adapters wrap a single `MemoryService`.** All transports — MCP, gRPC, HTTP, future — call into the same `MemoryService` interface (DESIGN.md §9.1). Adapters live in `internal/transport/<name>/`. Adapters NEVER touch `Embedder`, `VectorIndex`, or `Graph` directly.
- **No fully-in-memory indexes.** (Subsumed by stateless service, called out for emphasis.) Search and indexing must work on stores too large to fit in RAM. Per-query memory is O(query params), not O(corpus). Storage cursor for flat scan, storage point lookups for HNSW.
- **Two distinct graphs share the store, do not conflate them:**
  - **Memory graph** — Hebbian, decaying — `memory_edges_out` / `memory_edges_in`.
  - **HNSW graph** — vector-navigation, static — `hnsw_records`.
- **Interface ownership of collections:** `Graph` owns `nodes`, `messages`, `memory_edges_*`. `VectorIndex` owns `vectors`, `hnsw_*`. Neither reaches into the other's collections.
- **HNSW navigation by raw similarity, weight-aware reranking after.** Do not plug `weight × sim` into HNSW navigation — it breaks recall. Pull oversampled candidates and rerank.
- **Decay is always lazy** — computed on read inside the same transaction as reinforcement. No background sweeper.
- **Both flat scan and HNSW are mandatory from day 1.** Flat scan is the correctness oracle for HNSW in tests.
- **Memory-edge updates are atomic across both directions** (single edge or its mirror, depending on backend). Divergence is a correctness bug.
- **Vectors are L2-normalized at write time and stored as raw little-endian float32 bytes.** No gob, no base64. Backends with native vector types may use them; the contract (DESIGN.md §4.8) is unchanged.
- **Do not add precomputed semantic-similarity *memory* edges** without discussing with the user — see DESIGN.md §7.4.
- **The two-tier reinforcement (co-retrieval vs co-traversal) is intentional.** Don't collapse them.
- **`HNSWMeta` is never cached in memory.** Read fresh from the backend on every operation that needs it.

## Coding Conventions

- Standard `gofmt` + `goimports`. No bespoke style.
- Errors: wrap with `fmt.Errorf("...: %w", err)`. Sentinel errors via `errors.Is`.
- Logging: `log/slog`, structured key/value. No printf-style.
- Config: a single Config struct loaded from YAML at startup. No global state.
- Time: never call `time.Now()` deep in business logic. Plumb a `Clock` interface (`Now() time.Time`) wherever decay math runs.
- Context: every interface method takes `ctx context.Context` first.
- IDs: ULIDs for nodes and messages.
- Layout: `internal/service/` (MemoryService impl), `internal/transport/<name>/` (adapters), `internal/storage/<backend>/` (backend impls), `internal/embed/<provider>/` (embedder impls), `internal/embed/fake/` (test embedder), `internal/graph/`, `internal/vectorindex/`, `internal/types/` (Node, MemoryEdge, etc.), `internal/chunker/` (sentence splitter + sliding window), `internal/clock/` (Clock interface + Real/Fake), `internal/config/` (YAML loader).

## Testing

- **Real storage backend in tests**, in `t.TempDir()` for embedded backends, test containers / shared dev instances for networked backends. **No mocks for storage.** v1 runs against bbolt; new backends must pass the same suite.
- **Storage compatibility suite**: a portable test suite that runs against any `Graph + VectorIndex` implementation — verifies CRUD, prefix-scan ordering, transaction atomicity (including aborts leaving consistent state), bidirectional neighbor lookup, and tombstone semantics.
- **HNSW oracle test**: HNSW search results must agree with flat-scan results above a recall floor (e.g., recall@k ≥ 0.95 with `oversampleN=300` for `k=8`). Property-based across corpus sizes.
- **Service-level tests** target `MemoryService` directly without any transport adapter — proves the service is transport-agnostic.
- **Transport adapter tests** drive the wire protocol with an in-process client; the service is stubbed only there, where the test is about marshalling.
- **Statelessness check**: heap-allocation snapshots in CI fail if non-connection allocations grow with request count. Code review enforces the rest.
- Embedder is mocked at the interface level — deterministic hash-to-vector in `internal/embed/fake/`.
- Live Gemini smoke tests are gated behind `GEMINI_API_KEY` and skipped otherwise.
- Property-based tests for chunking, decay, and HNSW invariants (bidirectional links, entry-point validity, exponential layer histogram).
- Failure-injection tests: aborted writer txs must leave the index consistent.
- End-to-end tests for MCP tools via an in-process MCP client.

## Workflow

- Consult official docs first when integrating an SDK/library (delegate to `document-specialist` agent or use Context Hub).
- **Ask the user before adding a new third-party dependency.** Approved deps so far: `github.com/modelcontextprotocol/go-sdk`, `suture`, `bbolt`, `go-genai`.
- Update `DESIGN.md` *before* the code when changing design.
- Don't refactor opportunistically during a feature change; keep diffs reviewable.

## Scope Discipline

- No premature abstraction. The interfaces in `DESIGN.md` §9 are the abstraction surface; do not grow them speculatively.
- No backwards-compatibility shims yet — the project has no users.
- No feature flags for unreleased features.
- No comments explaining *what* the code does. Comments only when *why* is non-obvious.
- The `LexicalIndex` interface is RESERVED, not implemented. Don't bake it in until a real failure case motivates it.
- gRPC and HTTP transport adapters are FUTURE WORK. Reserve directories (`internal/transport/grpc/`, `internal/transport/http/`) but don't implement until requested.
