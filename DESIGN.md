# memmy — Design Document

memmy is an LLM memory system written in pure Go (toolchain: **Go 1.26.2**), exposed over MCP — and, in the future, gRPC and HTTP — providing associative, decay-aware memory for one or more agents. Memory is scoped by a flexible identity tuple (e.g., `(agent)`, `(agent, user)`, or arbitrary key/value pairs).

This document is the source of truth for architecture and design rationale. Code conventions live in `CLAUDE.md`.

---

## 0. Design Principles (load-bearing)

These principles override convenience. If a section below appears to violate one of them, the section is wrong.

1. **One source of truth: the database.** Vectors, nodes, messages, memory association edges, **and the HNSW navigation graph** all live in the configured storage backend. No secondary store, no external search engine, no parallel index file. The reference backend in v1 is bbolt; the same logical data model maps to Postgres, MariaDB, Bigtable, Spanner, and other stores that satisfy the interface contracts in §9.

2. **Storage is pluggable; retrieval policy is not.** Storage backends are interchangeable behind the `Graph` and `VectorIndex` interfaces. Retrieval scoring, oversampling, memory-edge reinforcement, lazy decay, and HNSW algorithm correctness live in the Memory Service and do not change per backend. Swapping the backend must not change observable retrieval behavior — only latency and operational characteristics.

3. **Stateless service.** A memmy process holds **no in-memory data state across requests**. The only persistent in-memory state in a memmy process is:
   - connection handles (storage backend, embedder, transport sessions to clients),
   - the loaded configuration (read-only),
   - process-local backpressure primitives (semaphores, rate limiters).

   There are no caches of database content, no in-memory tenant registries, no in-memory vector indexes, no globally held weight or counter state, no shadow copies of `HNSWMeta`. Per-request transient state — priority queues, heaps, decoded vector buffers, visited-sets — is created and freed within the request's scope. This is a hard constraint, not an aspiration: it enables **horizontal scale-out via N stateless memmy nodes** against a shared multi-writer backend (Postgres, Bigtable, Spanner). The bbolt reference backend is single-process by file-lock semantics — that's a property of bbolt, not of memmy.

4. **Transport adapters wrap a single Memory Service.** All transports — MCP, gRPC, HTTP, future — call into one transport-agnostic `MemoryService` interface (§9). Transport-specific concerns (MCP tool registration, protobuf service definitions, HTTP route handlers) live in their adapters. The Memory Service does not know which transport invoked it.

5. **Decay is part of ranking, not a post-filter.** Weight-aware scoring is integrated into retrieval. A hot, recently-reinforced memory must be able to outrank a cold, never-touched memory of equal raw similarity.

6. **Two distinct graphs share the store.** The **memory graph** (Hebbian association edges, §7) and the **HNSW graph** (vector navigation links, §6) are separate concepts. They live in different collections, have different semantics, and obey different update rules. Confusing them is a correctness bug, not a quirk.

7. **Correctness over engineering effort.** When in doubt, choose the more correct option. The code is written by AI; rewriting is cheap. Bugs born of premature optimization are not.

---

## 1. Goals & Non-Goals

### Goals
- Persistent, associative memory for LLM agents.
- Multi-tenant by arbitrary identity tuple.
- **Pluggable** at every external boundary, behind Go interfaces:
  - **Storage** v1: bbolt. Future: Postgres, MariaDB, Bigtable, Spanner, badger, pebble.
  - **Transport** v1: MCP via `github.com/modelcontextprotocol/go-sdk`. Future: gRPC + HTTP.
  - **Embedder** v1: Gemini via `go-genai`.
- Memory association edges that **strengthen with use** (Hebbian) and **decay with disuse** (exponential).
- Weight-aware retrieval: decayed node weight participates in ranking.
- Disk-resident, single-store architecture: the configured storage backend holds everything.
- **Stateless server**: any memmy process is interchangeable with any other; horizontal scale-out is a backend choice, not a memmy redesign.
- Process-managed via `suture` for resilience.

### Non-Goals (initial)
- Distributed coordination *within* memmy itself (consensus, leader election, etc.). Statelessness defers all coordination to the chosen backend.
- Cross-tenant memory sharing.
- Streaming embedding providers.
- Background sweepers / cron-style decay (decay is **lazy**, computed on read).
- Precomputed semantic-similarity *memory* edges (note: distinct from HNSW links; see §7.4).
- Lexical / BM25 search in v1 (interface space reserved; see §15).

---

## 2. Architecture Overview

```
                ┌───────────────────────────────────────────┐
                │            Transport Adapters             │
                │   ┌──────┐  ┌──────┐  ┌──────┐            │
                │   │ MCP  │  │ gRPC │  │ HTTP │   …        │
                │   └──┬───┘  └──┬───┘  └──┬───┘            │
                └──────┼─────────┼─────────┼────────────────┘
                       └─────────┼─────────┘
                                 │
                       ┌─────────▼──────────┐
                       │   MemoryService    │  (input port)
                       │     interface      │
                       └─────────┬──────────┘
                                 │
                       ┌─────────▼──────────┐
                       │   Memory Service   │  (impl; stateless;
                       │  composes ports    │   suture-supervised)
                       └─────────┬──────────┘
              ┌──────────────────┼──────────────────┐
              │                  │                  │
        ┌─────▼─────┐      ┌─────▼─────┐      ┌─────▼─────┐
        │  Embedder │      │  Vector   │      │  Graph    │
        │ interface │      │  Index    │      │ interface │
        └─────┬─────┘      │ interface │      └─────┬─────┘
              │            └─────┬─────┘            │
              │                  │                  │
        ┌─────▼─────┐      ┌─────▼──────────────────▼─────┐
        │ go-genai  │      │      Storage Backend          │
        │ (Gemini)  │      │  bbolt | postgres | bigtable  │
        │           │      │  spanner | mariadb | …        │
        └───────────┘      └───────────────────────────────┘
```

**Ports IN** (inbound interface): `MemoryService`. Every transport adapter calls this interface and only this interface.

**Ports OUT** (outbound interfaces): `Embedder`, `VectorIndex`, `Graph`. The Memory Service implementation composes these and is the only component that does so.

`VectorIndex` and `Graph` share the same storage backend instance but expose different interfaces and access disjoint collection sets:
- `VectorIndex` owns the `vectors` and `hnsw_*` collections.
- `Graph` owns `nodes`, `messages`, `memory_edges_out`, `memory_edges_in`.

The Memory Service is the only place where retrieval policy (oversampling, reranking, expansion, reinforcement) lives. Storage backends are swapped by providing alternative implementations of `Graph` + `VectorIndex` rooted at the same physical store. Transports are swapped by providing alternative adapters around the same `MemoryService` instance. Multiple transports may run simultaneously (e.g., MCP + gRPC both serving the same service).

**Statelessness footprint.** Every memmy process holds only: transport listeners, the service-impl, port-out implementations, connection pools, and config. No process holds anything that another process couldn't reconstruct from scratch. Deploy N memmy processes behind a load balancer; each accepts requests independently; the database serializes any necessary ordering.

---

## 3. Identity Model

Identity is an ordered tuple of string key/value pairs:

- `{"agent": "ada"}`
- `{"agent": "ada", "user": "u_42"}`
- `{"agent": "ada", "user": "u_42", "session": "s_99"}`

Internally the tuple is normalized (keys sorted, values trimmed) into a canonical form, then hashed to a stable `TenantID` (`sha256` truncated to 16 bytes, hex-encoded). Every node, edge, vector, and HNSW record is scoped by `TenantID`. Cross-tenant reads are not supported in v1.

The original tuple is also persisted alongside its `TenantID` in the `tenants` collection, so we can list known tenants for stats and admin. This registry is read fresh from the backend on demand — no in-memory tenant cache.

### 3.1 Optional tenant schema

By default any string-keyed tuple is accepted; whichever shape the caller sends defines a tenant. For deployments that want to constrain tenant shape (e.g., a Claude Code MCP server that wants every memory tagged with either `project=<abs path>` or `scope=global`), the config exposes an optional **tenant schema**:

- `tenant.description` — human prose, rendered into the MCP tool descriptions and the `tenant` property's JSON Schema description so the LLM sees it during tool listing.
- `tenant.keys` — declared keys with optional `description`, `pattern` (regex), `enum`, and `required` flags. All values are strings (the tuple is `map[string]string`). When a schema is configured, **unknown keys are rejected** (`additionalProperties: false`).
- `tenant.one_of` — list of key-sets; **exactly one** of the listed sets must be fully present in every tuple (JSON Schema `oneOf` semantics).

**Validation is shape-only and stateless.** The schema is evaluated against the incoming tuple before `TenantID` derivation; the tuple itself is not transformed. This means changing the schema does NOT migrate any stored memory — `TenantID` is purely a hash of the (validated) tuple, so:

- Memories written under schema A remain addressable under schema A.
- If schema A is replaced with schema B that rejects the same tuple, those memories become **unreachable** (no read can produce a tuple that hashes to their `TenantID`).
- If schema B is rolled back to schema A (or any schema that accepts the original tuple shape), those memories are reachable again.

There is no migration step, no forget-on-mismatch, no stored-tenant rewrites. The schema is a per-request validator that doubles as discoverable input documentation for MCP clients (see §10.2). Errors carry an `error_code`, `field`, `got` value, and the rendered JSON Schema as `expected_schema` so callers can self-correct.

---

## 4. Data Model

### 4.1 Chunks — the unit of memory

A "memory" as the user perceives it is an entire incoming message. Internally each message is split into **chunks** via a sliding window over sentences:

- Split the message into sentences (rule-based splitter; revisit with a model-based one if quality demands).
- Sliding window of **size 3, stride 2** (one sentence overlap between adjacent windows).
- Example for 10 sentences → windows `[1,2,3], [3,4,5], [5,6,7], [7,8,9], [9,10]`. The trailing window may be shorter when sentences run out.
- Each window becomes one **chunk**, gets one embedding, becomes one **node**, one vector record, and one HNSW record.
- The **original full message text** is stored once and referenced by every chunk that came from it.

### 4.2 Node (chunk metadata)

The Node record holds metadata only — **no vector**. The vector lives in the `vectors` collection.

```go
type Node struct {
    ID            string    // ULID; same ID identifies vectors[id] and hnsw_records[id]
    TenantID      string
    SourceMsgID   string    // ID of the parent message
    SentenceSpan  [2]int    // [start, end) sentence indices in source message
    Text          string    // the windowed text
    EmbeddingDim  int       // sanity check; vector lives in vectors collection
    CreatedAt     time.Time
    LastTouched   time.Time // for lazy decay (see §8)
    Weight        float64   // node strength (post-decay, lazily updated)
    AccessCount   uint64    // monotonic, never decays — for stats only
    Tombstoned    bool      // soft-delete marker; HNSW skips tombstoned nodes
}
```

### 4.3 Memory Edge (Hebbian association)

These are the **memory graph** edges — Hebbian, decaying. Distinct from HNSW links (see §4.4). Directed.

```go
type MemoryEdge struct {
    From          string
    To            string
    TenantID      string
    Kind          EdgeKind
    Weight        float64  // post-decay, lazily updated
    LastTouched   time.Time
    CreatedAt     time.Time
    AccessCount   uint64   // co-retrieval bumps
    TraverseCount uint64   // co-traversal bumps
}

type EdgeKind uint8
const (
    EdgeStructural   EdgeKind = iota // same source message / temporal adjacency
    EdgeCoRetrieval                  // appeared together in top-K seeds
    EdgeCoTraversal                  // hopped during expansion AND survived to results
)
```

`Kind` affects **initial weight and decay rate only** — retrieval branches do not switch on kind.

### 4.4 HNSW Record (vector navigation graph)

HNSW links are pure vector-space navigation — NOT decaying, NOT reinforced. Created at insert, mutated only on insert/delete.

```go
type HNSWRecord struct {
    NodeID    string
    Layer     int                 // top layer this node is present at
    Neighbors map[int][]string    // layer → neighbor node IDs (per-layer caps; see §6.3)
}

type HNSWMeta struct {
    Dim            int
    EntryPoint     string  // node ID at the top layer
    MaxLayer       int
    M              int     // target neighbors per layer (>0)
    M0             int     // target neighbors at layer 0 (typ. 2*M)
    EfConstruction int
    ML             float64 // 1 / ln(M); layer-assignment factor
    Size           int     // count of non-tombstoned nodes
}
```

`HNSWMeta` is **never cached in memory** across requests. Every operation that needs it reads it fresh from the backend (within the operation's transaction). Multiple memmy nodes may insert concurrently against multi-writer backends; the backend's transactional semantics serialize entry-point/maxLayer changes.

### 4.5 Message

```go
type Message struct {
    ID        string
    TenantID  string
    Text      string
    Metadata  map[string]string
    CreatedAt time.Time
}
```

Persisted once per message. Returned alongside chunks during retrieval so callers see the full source context, not just a 3-sentence window.

### 4.6 Logical Data Model (collections)

The data model is described in terms of **collections** keyed by tenant + identifier(s). Each storage backend maps these to its native primitives — bbolt buckets, SQL tables, Bigtable column families, Spanner interleaved tables, etc. The collections themselves and the keying scheme are part of the design contract; the physical layout is the backend's concern.

| Collection            | Key                          | Value                          | Owner       |
|-----------------------|------------------------------|--------------------------------|-------------|
| `tenants`             | `TenantID`                   | `TenantInfo`                   | (registry)  |
| `nodes`               | `(TenantID, NodeID)`         | `Node` (metadata; no vector)   | Graph       |
| `messages`            | `(TenantID, MessageID)`      | `Message`                      | Graph       |
| `vectors`             | `(TenantID, NodeID)`         | raw f32 LE bytes (normalized)  | VectorIndex |
| `hnsw_records`        | `(TenantID, NodeID)`         | `HNSWRecord`                   | VectorIndex |
| `hnsw_meta`           | `TenantID`                   | `HNSWMeta`                     | VectorIndex |
| `memory_edges_out`    | `(TenantID, FromID, ToID)`   | `MemoryEdge`                   | Graph       |
| `memory_edges_in` ¹   | `(TenantID, ToID, FromID)`   | `MemoryEdge` (mirror)          | Graph       |
| `tenant_counters` ²   | `TenantID`                   | `{NodeCount, EdgeCount, SumNodeWeight, SumEdgeWeight}` | Graph |

¹ The `_in` mirror is required only for backends that don't support efficient lookups in both directions natively. KV backends (bbolt, badger, pebble, Bigtable) need the mirror because prefix scans only work on a single key prefix. SQL backends with proper secondary indexes can collapse `out` and `in` into one `memory_edges` table. The interface contract is "neighbor lookup is efficient in either direction"; how the backend gets there is its problem.

² `tenant_counters` backs the `Stats` operation in O(1) per tenant. Maintained transactionally by every Graph mutation (`PutNode`, `UpdateNode`, `DeleteNode`, `PutEdge`, `UpdateEdge`, `DeleteEdge`) — upsert paths capture an old-weight delta; brand-new paths increment the count. Backends MAY skip this collection if they can answer Stats without walking buckets via native counts/aggregates (e.g., SQL `COUNT(*)` + `SUM(weight)` with appropriate indexes).

**Operations the backend must support:**

- Point get / put / delete by full key.
- Prefix scan within a tenant subtree (to cursor through `vectors`, `nodes`, or outbound edges from a given source).
- Atomic multi-record write transactions across the collections owned by one interface (e.g., one HNSW insert touches `hnsw_records` of multiple nodes plus `hnsw_meta`, all within one transaction).

**Interface-ownership rule.** `Graph` writes only to the collections it owns (`nodes`, `messages`, `memory_edges_*`). `VectorIndex` writes only to its collections (`vectors`, `hnsw_*`). The Memory Service may compose calls across both, but neither implementation may write the other's collections.

### 4.7 Reference Layout: bbolt

```
root
├── tenants/                                            # tenants registry
│   └── <tenantID> → gob(TenantInfo)
├── t/                                                  # per-tenant data
│   └── <tenantID>/
│       ├── nodes/    <nodeID>          → gob(Node)
│       ├── msgs/     <msgID>           → gob(Message)
│       ├── vec/      <nodeID>          → []byte                # raw f32 LE, normalized
│       ├── hnsw/
│       │   ├── meta                    → gob(HNSWMeta)
│       │   └── records/<nodeID>        → gob(HNSWRecord)
│       ├── eout/     <fromID>/<toID>   → gob(MemoryEdge)       # outbound
│       ├── ein/      <toID>/<fromID>   → gob(MemoryEdge)       # mirror
│       └── counters/ v                 → gob(tenantCounters)   # O(1) Stats backing
└── meta/
    └── schema_version → uint32
```

bbolt's nested buckets express the logical model directly. Sketches for other backends:

- **Postgres / MariaDB**: each collection becomes a table with a composite primary key matching the logical key. `memory_edges` collapses into one table with two indexes on `(tenant, from)` and `(tenant, to)`. Vectors stored as `bytea` (or `vector` via pgvector if used purely as opaque bytes for our distance computation).
- **Bigtable**: row key `tenant#kind#id` (e.g., `t01#vec#01J...`), with one column family per logical category. `hnsw_records` and `vectors` can share a row but live in distinct families.
- **Spanner**: parent table `Tenants(tenant_id)`, with `Nodes`, `Vectors`, `HnswRecords`, `MemoryEdges` interleaved under it for locality.
- **badger / pebble**: same pattern as bbolt; key prefixing replaces nested buckets.

Implementations live in `internal/storage/<backend>/` and must pass the storage compatibility test suite (§14).

### 4.8 Vector Serialization (logical contract)

Logical contract: a vector is a contiguous binary blob of `Dim × 4` bytes (little-endian float32). Vectors are L2-normalized **at write time**, so dot product equals cosine similarity at search time. No header, no gob, no base64 — these are wasteful and lossy. Backends with native vector types (e.g., pgvector) MAY use them; the contract is "given a `(tenant, NodeID)`, produce its normalized vector for a distance computation that returns the same number to within float32 precision."

`Dim` is enforced by `HNSWMeta.Dim` and validated at every read.

---

## 5. Indexing Pipeline

```
Write(tenantTuple, messageText, metadata) →
  1. Normalize tuple → tenantID; upsert into tenants.
  2. Persist Message{...} to messages.
  3. Split text → sentences.
  4. Generate windows (size=3, stride=2).
  5. Embed all windows in one batched call to Embedder.
  6. L2-normalize each vector.
  7. For each window:
       a. Create Node{...}; persist to nodes.
       b. Persist vector bytes to vectors.
       c. VectorIndex.Insert(tenantID, nodeID, vec)  →  HNSW insert (see §5.1).
  8. Create memory association edges (Hebbian, write-time):
       a. Sequential within message: chunk_i ↔ chunk_{i+1}, kind=Structural, w=1.0.
       b. Recent within tenant (last N chunks within Δt minutes): kind=Structural, w=0.3.
  9. Return {message_id, node_ids[]}.
```

Embedding is the only externally-blocking step; it happens **before** any storage write transaction. Storage transactions are short.

### 5.1 HNSW insert (per node, inside one storage write tx)

```
Insert(tenantID, nodeID, qVec):
  meta := read hnsw_meta (or initialize)
  L    := sample insertion layer  # floor(-ln(rand) * meta.ML)
  ep   := meta.EntryPoint

  if meta is empty:
    write hnsw_records[(tenant, nodeID)] = {Layer: L, Neighbors: {0..L: []}}
    meta.EntryPoint = nodeID; meta.MaxLayer = L; meta.Size = 1
    write hnsw_meta
    return

  # Phase 1: greedy descent from top layer to L+1
  cur := ep
  for ℓ := meta.MaxLayer; ℓ > L; ℓ--:
    cur = greedySearchLayer(qVec, cur, ef=1, layer=ℓ)

  # Phase 2: at each layer L..0, ef-search and link
  for ℓ := min(L, meta.MaxLayer); ℓ >= 0; ℓ--:
    W := efSearchLayer(qVec, cur, ef=meta.EfConstruction, layer=ℓ)
    M_target := meta.M if ℓ > 0 else meta.M0
    chosen   := selectNeighborsHeuristic(qVec, W, M_target)  # Malkov §4 Alg.4
    for n in chosen:
      add bidirectional link nodeID ↔ n at layer ℓ
      if len(neighbors[n][ℓ]) > M_target:
        prune neighbors[n][ℓ] to M_target  (same heuristic)
    cur := chosen[0]

  # Phase 3: extend top if needed
  if L > meta.MaxLayer:
    meta.EntryPoint = nodeID; meta.MaxLayer = L

  meta.Size++
  write hnsw_records[nodeID], updated neighbor records of all touched nodes,
  hnsw_meta.
```

All writes (HNSW records, neighbor mutations, meta) commit in **one** storage transaction. Atomicity per insert is required. `meta` is read from the backend at the start of the tx and written back at the end of the tx; no in-memory copy is retained.

---

## 6. Retrieval Pipeline

The retrieval pipeline operates entirely against the storage backend — never loading the index into memory. Per-query memory is bounded by query parameters (`oversampleN`, `efSearch`, `hops`), not by corpus size.

### 6.1 Backend selection

Per tenant, by `HNSWMeta.Size`:
- `< flatScanThreshold` (default 5,000): **flat scan**.
- `≥ flatScanThreshold`: **HNSW**.

The threshold is configurable. **Both modes are implemented from day 1.** HNSW correctness is validated against flat scan in tests (§14).

`HNSWMeta.Size` is read fresh per query (one point lookup); no per-process cache.

### 6.2 Flat scan (storage cursor, streaming)

```
FlatScan(tenantID, qVec, oversampleN):
  q := qVec  (already normalized)
  heap := bounded-min-heap of size oversampleN  (key = sim)
  cur  := storage cursor over `vectors` prefixed by tenantID

  for nodeID, vecBytes := cur.Next(); nodeID != ""; ...:
    vec := decodeF32LE(vecBytes, dim)
    s   := dot(q, vec)              # cosine, since both normalized
    heap.PushIfBetter(s, nodeID)

  filter heap survivors against nodes.Tombstoned (one Node read per survivor)
  return heap.Sorted()
```

Memory footprint = O(oversampleN) for the heap. Vectors are decoded into a **reusable** buffer (one per scan, not per vector) and discarded as the cursor advances. The corpus is never materialized.

Tombstone filtering happens once at the end on heap survivors (≤ oversampleN reads), not per-visit, to avoid 1M extra Node reads on a 1M-node scan.

### 6.3 HNSW search (storage point lookups)

```
HNSWSearch(tenantID, qVec, oversampleN):
  q    := qVec  (already normalized)
  meta := read hnsw_meta

  cur := meta.EntryPoint
  for ℓ := meta.MaxLayer; ℓ > 0; ℓ--:
    cur = greedySearchLayer(q, cur, ef=1, layer=ℓ)

  ef := max(meta.EfSearch, oversampleN)
  candidates := efSearchLayer(q, cur, ef=ef, layer=0)
  filter out tombstoned
  return top oversampleN by sim
```

`greedySearchLayer` and `efSearchLayer`:
- Maintain a candidate priority queue and a result priority queue (both bounded).
- Each visit reads `hnsw_records[nodeID]` (neighbor list) + `vectors[nodeID]` (vector). Two storage point lookups per visited node.
- Visited-set is `map[string]struct{}` bounded by `O(ef × MaxLayer)` per query — small, per-query, freed at return.

The backend's native cache (OS page cache for bbolt, buffer pool for SQL, internal cache for Bigtable/Spanner) keeps hot regions of the graph warm; cold regions live on disk until needed. memmy never explicitly caches HNSW records or vectors at the application layer — that would violate §0 #3.

### 6.4 Weight-aware reranking (both modes)

```
Recall(tenantTuple, query, k=8, hops=2, oversampleN=300) →
  1. tenantTuple → tenantID.
  2. q_vec := Embedder.Embed(query); L2-normalize.
  3. candidates := VectorIndex.Search(tenantID, q_vec, oversampleN)
       (flat scan or HNSW, chosen per §6.1)
  4. For each candidate c:
       node := graph.UpdateNode(c.id, lazy-decay-then-reinforce)   # see §8
       c.score := (c.sim ^ α) * (node.weight ^ β)                  # default α=1.0, β=0.5
  5. seeds := top-K by reranked score.
  6. Hebbian update: for every unordered pair (a,b) in seeds:
       memory edge a↔b, kind=CoRetrieval, lazy-decay-then-reinforce.
  7. Graph expansion via memory edges, BFS up to `hops` hops:
       For each frontier edge (cur → next):
         eff_w := edge.weight * exp(-λ_kind * (now - edge.LastTouched))
         if eff_w < edge_floor: prune; skip
         path_score(next) += seed_score(seed_origin) * eff_w / depthPenalty(depth)
       Multiply by node.eff_weight at each visited node.
  8. Merge seeds ∪ expanded; rank by combined score.
  9. Co-traversal reinforcement: for every node in the FINAL returned set
     reached via a memory edge (not a seed), reinforce that edge as CoTraversal.
 10. Return ranked results with provenance (see §6.6).
```

### 6.5 Why navigate by raw similarity, rerank by weight

HNSW correctness depends on monotonic distance comparisons during greedy descent. If we plug `weight × sim` directly into navigation, we can skip high-similarity bridge nodes that lead to high-similarity-AND-high-weight regions — recall collapses unpredictably.

Mitigation:
- HNSW navigation uses pure cosine.
- Pull a wide candidate set (`oversampleN`, default 300 for target k=8).
- Weight-aware scoring is applied during reranking on the candidate set.

The oversample factor is the knob that trades query latency for recall of "hot but moderately similar" memories. Empirically, 300 candidates for k=8 closes the recall gap to negligible. Tunable.

For flat scan, `oversampleN` is also used (the heap is bounded), but recall is exact-by-construction up to the heap size.

### 6.6 Provenance returned to caller

```json
{
  "results": [
    {
      "node_id":        "01J...",
      "text":           "<chunk text>",
      "source_msg_id":  "01J...",
      "source_text":    "<full original message>",
      "score":          0.84,
      "score_breakdown":{"sim":0.71,"node_weight":1.42,"graph_mult":1.18,"depth":1},
      "path":           ["01J_seed", "01J_hop1", "01J_self"]
    }
  ]
}
```

Provenance is non-negotiable. Without it, debugging why a memory surfaced (or didn't) is impossible.

---

## 7. Memory-Edge Formation — Mechanisms

Three mechanisms create **memory edges** (Hebbian association graph). Only the first runs at write time; the other two are read-time learning signals.

### 7.1 Structural (write-time, free)

- Sequential within message — `chunk_i ↔ chunk_{i+1}`, initial weight 1.0.
- Recent within tenant — link new chunks to last N chunks of same tenant within Δt (default 5 min), initial weight 0.3.

### 7.2 Hebbian Co-Retrieval (read-time)

Whenever two nodes appear in the same top-K seed set (post-rerank), increment the mutual edge:

```
δ_edge_coretrieval = base_coret * min(sim(q, a), sim(q, b))
```

`base_coret = 0.5`. Pairs that are *both* highly relevant to the query reinforce each other more.

### 7.3 Co-Traversal (read-time, second-tier)

When graph expansion brings a node into the **final** returned set via an edge, that edge gets a larger bump than co-retrieval:

```
δ_edge_cotraversal = 1.5 × δ_edge_coretrieval
```

Co-traversal rewards edges that *worked*, not edges that merely activated.

### 7.4 Why no precomputed semantic-similarity *memory* edges

(Note: HNSW links are a different graph; see §4.4 / §7.5. This subsection is about the memory graph only.)

Tempting and rejected for v1:

- The HNSW graph already encodes vector-space neighborhood. Duplicating it as memory edges adds nothing.
- Static similarity edges dilute the meaning of memory-edge weight — every plausible edge already exists, so reinforcement loses its discriminative power.
- The memory graph should encode **usage history**, not **content similarity**. The vector index handles content; the memory graph handles association by use.

If cold-start retrieval feels too thin in practice, revisit with `EdgeColdSemantic` (low initial weight, fast decay, seeded from HNSW neighbors at write time). Do not add without explicit discussion.

### 7.5 Memory edges vs HNSW links — disambiguation

| Aspect              | HNSW link                       | Memory edge                          |
|---------------------|---------------------------------|--------------------------------------|
| Purpose             | vector-space navigation         | semantic association by usage        |
| Created             | at insert time, by HNSW algo    | structural rule + Hebbian + co-trav  |
| Strengthens?        | no                              | yes, with use                        |
| Decays?             | no                              | yes, with time                       |
| Used during search? | yes — find candidates           | yes — graph expansion of seeds       |
| Collection          | `hnsw_records`                  | `memory_edges_out` (+ `_in`)         |
| Owner interface     | `VectorIndex`                   | `Graph`                              |

Cross-confusing them between subsystems is a correctness bug.

---

## 8. Strengthening & Decay (memory graph only)

HNSW links do not decay. This section applies to nodes and memory edges only.

### 8.1 Lazy decay — computed on read

Decay is never run by a sweeper. Every node and memory edge stores `LastTouched` and current `Weight`. On any access:

```go
now := clock.Now()
dt  := now.Sub(stored.LastTouched).Seconds()

decayed := stored.Weight * math.Exp(-lambda * dt)
boosted := math.Min(decayed + delta, weightCap)

stored.Weight      = boosted
stored.LastTouched = now
```

Read-modify-write happens inside one storage transaction via `Graph.UpdateNode` / `Graph.UpdateEdge` (closure form, see §9).

### 8.2 Reinforcement — capped

Default: cap-only with `weightCap = 100.0`. Combined with decay, steady-state weight is proportional to access frequency, bounded by `weightCap`. Move to log-dampening if the cap proves too coarse.

### 8.3 Decay rates

| Entity                    | λ (per second) | Half-life    |
|---------------------------|----------------|--------------|
| Node                      | 8.0e-8         | ~100 days    |
| Memory edge — Structural  | 4.0e-8         | ~200 days    |
| Memory edge — CoRetrieval | 2.7e-7         | ~30 days     |
| Memory edge — CoTraversal | 1.3e-7         | ~60 days     |

All λ values configurable. Tune from observed weight/age distributions.

### 8.4 Pruning

When `decayed_weight < floor` on read:
- **Memory edges**: delete on the spot (and the mirror, if present, atomically), inside the same transaction as the decay calculation.
- **Nodes**: keep (they still hold content), but exclude from graph expansion. Hard delete only via explicit `Forget` operation (see §10).

Hard-deleting a node hard-deletes its `vectors` and `hnsw_records` entries too. HNSW deletion uses a tombstone-then-compact strategy (§15 — compaction is future work; tombstones + filtered search work for v1).

### 8.5 Worked example

A new CoRetrieval edge created with weight 1.0, λ = 2.7e-7:

| Time      | Decayed weight if no further access |
|-----------|-------------------------------------|
| 0         | 1.000                               |
| 1 day     | 0.977                               |
| 7 days    | 0.848                               |
| 30 days   | 0.500                               |
| 90 days   | 0.125                               |
| 180 days  | 0.016 → likely below floor, pruned  |

Accessed once per week with `δ ≈ 0.35`, weight stabilizes around `0.35 / (1 − exp(−λ · week)) ≈ 2.3`. Bounded, intuitive.

### 8.6 Two access counters — node and edge

- **Node.AccessCount** — bumped on direct retrieval. Monotonic, never decays. Analytics only.
- **Node.Weight** — decaying, used for ranking.
- **MemoryEdge.AccessCount** — bumped on co-retrieval. Monotonic. Analytics.
- **MemoryEdge.TraverseCount** — bumped on co-traversal. Monotonic. Analytics.
- **MemoryEdge.Weight** — decaying, used for graph expansion ranking.

Counts answer "what happened over time"; weights answer "what is salient now."

### 8.7 Multi-node race semantics

Under multi-writer backends, two memmy nodes may concurrently observe the same node/edge during lazy-decay-and-reinforce. Both compute the new weight; the second writer wins. The first writer's reinforcement is silently dropped. This is **acceptable**: losing one reinforcement event is not a correctness violation (the access still happened; another reinforcement will fire on the next access). Steady-state behavior is unaffected. The only visible impact is a slight under-counting of reinforcement under high concurrency — bounded by the fraction of races, which is small in practice.

---

## 9. Go Interfaces

These are the abstraction surface. Three port-out interfaces (Embedder, VectorIndex, Graph) and one port-in interface (MemoryService). Do not grow the surface speculatively.

### 9.1 Port IN

```go
// MemoryService is the application's input port. All transport adapters
// (MCP, gRPC, HTTP, ...) call into this interface. The implementation
// MUST be stateless across calls — no in-memory data state, no caches.
//
// Lives in `internal/service/`. Adapters live in `internal/transport/<name>/`.
type MemoryService interface {
    Write(ctx context.Context, req WriteRequest) (WriteResult, error)
    Recall(ctx context.Context, req RecallRequest) (RecallResult, error)
    Forget(ctx context.Context, req ForgetRequest) (ForgetResult, error)
    Stats(ctx context.Context, req StatsRequest) (StatsResult, error)
}

type WriteRequest struct {
    Tenant   map[string]string
    Message  string
    Metadata map[string]string
}
type WriteResult struct {
    MessageID string
    NodeIDs   []string
}

type RecallRequest struct {
    Tenant       map[string]string
    Query        string
    K            int  // target results; 0 → default 8
    Hops         int  // graph expansion hops; 0 → default 2
    OversampleN  int  // candidates pulled before rerank; 0 → default 300
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
type ScoreBreakdown struct {
    Sim        float64
    NodeWeight float64
    GraphMult  float64
    Depth      int
}

type ForgetRequest struct {
    Tenant    map[string]string
    MessageID string    // optional; if "", uses Before
    Before    time.Time // optional; zero value means "no time bound"
}
type ForgetResult struct {
    DeletedNodes   int
    DeletedEdges   int
    DeletedVectors int
}

type StatsRequest struct {
    Tenant map[string]string // optional; nil → aggregate across tenants
}
type StatsResult struct {
    NodeCount       int
    MemoryEdgeCount int
    HNSWSize        int
    AvgNodeWeight   float64
    AvgEdgeWeight   float64
}
```

These request/result types are transport-agnostic Go values. Each adapter is responsible for marshalling its wire format to/from these types.

### 9.2 Ports OUT

```go
// Embedder produces vectors for text. Returned vectors are NOT normalized;
// callers normalize at index/query boundaries.
type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
    Dim() int
}

// VectorIndex stores vectors and provides top-N similarity search.
//
// Implementations MUST NOT load the entire index into memory. Per-query
// memory must be bounded by query parameters, not corpus size.
//
// Implementations own the `vectors` and `hnsw_*` collections within the
// configured storage backend. Vectors passed to Insert MUST be L2-normalized.
type VectorIndex interface {
    Insert(ctx context.Context, tenant, nodeID string, vec []float32) error
    Delete(ctx context.Context, tenant, nodeID string) error              // tombstones
    Search(ctx context.Context, tenant string, qVec []float32, n int) ([]VectorHit, error)
    Size(ctx context.Context, tenant string) (int, error)                 // for backend selection
    Close() error
}

type VectorHit struct {
    NodeID string
    Sim    float64 // cosine, in [-1, 1]; with normalized vectors typically [0, 1]
}

// Graph stores nodes, messages, and memory association edges (Hebbian).
//
// Implementations own the `nodes`, `messages`, `memory_edges_out`, and
// `memory_edges_in` collections. They MUST NOT touch `vectors` or
// `hnsw_*` — those belong to VectorIndex.
type Graph interface {
    PutNode(ctx context.Context, n Node) error
    GetNode(ctx context.Context, tenant, id string) (Node, error)
    UpdateNode(ctx context.Context, tenant, id string, fn func(*Node) error) error
    DeleteNode(ctx context.Context, tenant, id string) error

    PutMessage(ctx context.Context, m Message) error
    GetMessage(ctx context.Context, tenant, id string) (Message, error)

    PutEdge(ctx context.Context, e MemoryEdge) error                        // upserts both directions atomically
    GetEdge(ctx context.Context, tenant, from, to string) (MemoryEdge, bool, error)
    UpdateEdge(ctx context.Context, tenant, from, to string, fn func(*MemoryEdge) error) error
    DeleteEdge(ctx context.Context, tenant, from, to string) error          // both directions atomically

    Neighbors(ctx context.Context, tenant, id string) ([]MemoryEdge, error)         // outbound
    InboundNeighbors(ctx context.Context, tenant, id string) ([]MemoryEdge, error)  // inbound

    Close() error
}

// LexicalIndex — RESERVED. Not implemented in v1. Future addition for
// BM25 / sparse retrieval if dense retrieval underperforms on rare tokens
// (proper nouns, IDs, codes). Do NOT introduce until a real failure case
// motivates it.
//
// type LexicalIndex interface { ... }
```

`UpdateNode` / `UpdateEdge` take a closure so the read-modify-write happens inside a single backend transaction. This is essential for atomic decay+reinforce.

Storage implementations live in `internal/storage/<backend>/` (e.g., `internal/storage/bbolt/`). Each backend implements both `Graph` and `VectorIndex` against the same physical store.

---

## 10. Service Surface and Transport Adapters

### 10.1 Service surface (transport-neutral)

The four operations on `MemoryService` (§9.1):

- **Write** — see §5.
- **Recall** — see §6.
- **Forget** — see §8.4.
- **Stats** — see §15 (observability).

These operations are the contract. Every transport adapter must expose all four; semantics must match.

### 10.2 MCP Adapter (v1)

Implements an MCP server using `github.com/modelcontextprotocol/go-sdk`. Lives in `internal/transport/mcp/`. The same tool surface is exposed over **two interchangeable wire transports**:

- **`mcp` (streamable HTTP)** — long-lived process, listens on a configured address, accepts many concurrent client sessions. Use this for the typical server deployment.
- **`stdio`** — the process is launched as a child by an MCP-aware host (e.g., editor or agent runtime). Reads JSON-RPC frames from stdin, writes responses to stdout. Logs MUST go to stderr so the JSON-RPC stream stays clean.

These two transports are **mutually exclusive** at the config level. `stdio` owns the process's stdin/stdout exclusively; running an HTTP listener alongside makes no sense (and would drift logs onto the JSON-RPC stream). The config validator rejects any configuration in which `stdio` is enabled together with another transport.

Tools registered (identical schema across both transports):

| MCP tool         | Args (JSON)                                                  | Result (JSON)                                                  | Maps to       |
|------------------|--------------------------------------------------------------|----------------------------------------------------------------|---------------|
| `memory.write`   | `{tenant, message, metadata?}`                               | `{message_id, node_ids[]}`                                     | `Write`       |
| `memory.recall`  | `{tenant, query, k?, hops?, oversample?}`                    | `{results[]}` (see §6.6)                                       | `Recall`      |
| `memory.forget`  | `{tenant, message_id?, before?}`                             | `{deleted_nodes, deleted_edges, deleted_vectors}`              | `Forget`      |
| `memory.stats`   | `{tenant?}`                                                  | `{node_count, memory_edge_count, hnsw_size, avg_*_weight}`     | `Stats`       |

The streamable transport allows long retrievals to yield partial results progressively (seeds first, then expanded set). Optional in v1; can return unary initially without an API break since results are already shaped as a list.

When the optional **tenant schema** (§3.1) is configured, each tool's auto-derived `inputSchema` has its `tenant` property replaced with the schema-rendered JSON Schema, including the schema's `description`, per-key descriptions, patterns, enums, `additionalProperties: false`, and `oneOf` constraints. The MCP SDK validates incoming arguments against this schema before dispatching to the handler. As a defense-in-depth fallback, handlers also catch `*service.ErrTenantInvalid` and return a `CallToolResult{IsError: true}` whose `TextContent` carries a structured payload (`error_code`, `field`, `got`, `message`, `expected_schema`) so the LLM can retry with corrected input.

### 10.3 gRPC Adapter (future, reserved)

Lives in `internal/transport/grpc/`. Service definitions in protobuf; service stubs implement `MemoryService` with proto ↔ Go-type marshalling. Designed to run on a separate listener alongside the MCP adapter. Authentication and authorization layers mount here.

### 10.4 HTTP Adapter (future, reserved)

Lives in `internal/transport/http/`. JSON over HTTP, REST-ish (`POST /v1/memory/write` etc.). Useful for browser clients, simple curl integration, and gateway-side authn/authz.

### 10.5 Adapter rules

- Adapters NEVER touch `Embedder`, `VectorIndex`, or `Graph` directly. Only `MemoryService`.
- Adapters NEVER hold per-call data state in memory beyond what the wire protocol mandates (e.g., a streaming cursor for the duration of one streamed response).
- Each adapter is independently enable-able via config. Multiple adapters may run in one process simultaneously, **except** for `stdio`, which is mutually exclusive with all other transports.

---

## 11. Process Management (suture)

Top-level supervisor tree:

```
RootSupervisor
├── StorageService             // owns the storage backend handle
├── EmbedderService            // owns embedder client
├── MemorySvcCore              // wires Embedder + VectorIndex + Graph; exposes MemoryService
├── MCPTransportService        // adapter; wraps MemoryService; v1 transport
├── GRPCTransportService       // FUTURE; reserved
├── HTTPTransportService       // FUTURE; reserved
└── MaintenanceService         // periodic stats; writes only to the storage backend
```

Each service implements `suture.Service`. `StorageService` is the lone owner of the storage backend handle (e.g., bbolt `*DB`, SQL `*sql.DB`, Bigtable `*Client`); `VectorIndex` and `Graph` borrow it. Each transport adapter is a separate `suture.Service`; multiple may run simultaneously based on config.

`MaintenanceService` is permitted because anything it computes is written back to the database — it holds no state, only periodically reads the database, computes stats, and writes them back to a `tenants/<id>/stats` record (or equivalent). Killing and restarting it has no consequences.

Failures are isolated and restarted with backoff. Restarting `StorageService` cascades — configure long backoff and conservative thresholds to avoid thrash if the backend is unhealthy.

---

## 12. Configuration

Single YAML config at startup; no global state, no env-var overrides except for secrets.

```yaml
server:
  transports:
    mcp:
      enabled: true
      addr: "0.0.0.0:8765"
    # stdio is mutually exclusive with every other transport.
    # Enable EITHER mcp/grpc/http (one or more) OR stdio — not both.
    stdio:
      enabled: false
    grpc:
      enabled: false
      addr: "0.0.0.0:8766"
    http:
      enabled: false
      addr: "0.0.0.0:8767"

storage:
  backend: bbolt              # bbolt | postgres | mariadb | bigtable | spanner | ...
  bbolt:
    path: "./data/memmy.db"
  # postgres:
  #   dsn_env: "MEMMY_PG_DSN"
  # bigtable:
  #   project: "..."
  #   instance: "..."
  #   table: "memmy"

embedder:
  backend: gemini
  gemini:
    model: "text-embedding-004"
    api_key_env: "GEMINI_API_KEY"
    concurrency: 8           # process-local semaphore limit

vector_index:
  flat_scan_threshold: 5000
  hnsw:
    m:                16
    m0:               32
    ef_construction:  200
    ef_search:        100
    ml:               0.36

memory:
  chunk_window_size: 3
  chunk_stride:      2
  retrieval_k:        8
  retrieval_hops:     2
  retrieval_oversample: 300

  scoring:
    sim_alpha:    1.0
    weight_beta:  0.5

  decay:
    node_lambda:             8.0e-8
    edge_structural_lambda:  4.0e-8
    edge_coretrieval_lambda: 2.7e-7
    edge_cotraversal_lambda: 1.3e-7

  reinforce:
    node_delta:                   1.0
    edge_coretrieval_base:        0.5
    edge_cotraversal_multiplier:  1.5

  prune:
    edge_floor: 0.05
    node_floor: 0.01

  weight_cap: 100.0

# Optional tenant schema (§3.1). When unset, any tuple is accepted.
tenant:
  description: |
    Identity for this memory. Use `project` (absolute path) for
    project-scoped memories. Use `scope: "global"` for cross-project.
  keys:
    project:
      description: "Absolute path of the working directory."
      pattern: "^/"
    scope:
      description: "Memory scope; 'global' for cross-project."
      enum: ["global"]
  one_of:
    - [project]
    - [scope]
```

---

## 13. Concurrency & Statelessness

### 13.1 Backend-dependent concurrency

Concurrency semantics depend on the chosen storage backend. The Memory Service is written against the interface contracts; backend implementations are responsible for their own concurrency control.

- **bbolt** is single-writer at the file level AND single-process by file lock. The bbolt implementation serializes writes through one goroutine; reads are concurrent.
- **Postgres / MariaDB / Spanner / Bigtable** support concurrent writers natively and across multiple processes. Their implementations exploit that; the Memory Service does not need to know.
- **HNSW insert** is a multi-record write within ONE storage transaction. Atomicity is required by all backends.
- **Embedder**: shared HTTP client; concurrency limited by a process-local semaphore. Embedding happens **outside** any storage tx.
- **Search**: read-mostly. Each search runs in a single read tx for the duration of the `VectorIndex.Search` call (snapshot isolation across visited HNSW records and vectors). Reranking + reinforcement happens **after** the read tx closes — it opens a fresh write tx batched per query.
- **Lazy decay-and-reinforce on read**: a single batched write tx per query updates all touched nodes and edges. If the write fails, results are still returned (we lose one reinforcement event; not a correctness violation).

### 13.2 Statelessness contract

memmy is stateless across requests by design (§0 #3). This subsection enumerates the contract.

**Permitted in-memory state:**
- Connection pools to the storage backend, the embedder, and active client transport sessions.
- Configuration (loaded once at startup, read-only).
- Process-local concurrency primitives (semaphores, rate limiters). These constrain the local process; they are not coordinated across nodes.
- Per-request transient state: heaps, priority queues, visited sets, decoded vector buffers, request bodies. Created and freed within request scope.

**Forbidden in-memory state:**
- Caches of database content (Node, Edge, HNSWRecord, Vector, HNSWMeta, etc.).
- In-memory tenant registries, schema caches, weight or counter accumulators.
- Inverted indexes, in-memory shards of HNSW, or any index data not fetched fresh from the backend on demand.
- Background-aggregated counters or stats that aren't backed by the database.
- Per-tenant locks held across multiple requests.

**Implications:**
- `HNSWMeta` is read fresh per operation that needs it. No "warm copy" survives between requests.
- Tenant existence checks are storage queries, not map lookups.
- `MaintenanceService` is allowed only because everything it produces is written to the database.

### 13.3 Multi-node deployment

When N memmy processes share a multi-writer backend (Postgres, Bigtable, Spanner), the application code is identical to the single-process case. Operational considerations:

- **Race on lazy reinforcement** — handled per §8.7.
- **Race on HNSW insert** — backend's transactional guarantees serialize entry-point/maxLayer changes; algorithm correctness is preserved.
- **Embedder rate limits** — each node manages its own quota with a process-local semaphore. If a global quota matters, coordinate at an embedder gateway, not in memmy.
- **Configuration** — every node loads the same config. Config rollouts are deploy events, not runtime concerns.
- **Sticky sessions** — none required. Any node can serve any request.

bbolt deployments are single-node by file-lock semantics; the application is the same code, just with one process.

---

## 14. Testing Strategy

- **Real storage backend in tests** — `t.TempDir()` for embedded backends, test container or shared dev instance for networked backends. **No mocks for storage.** v1 ships with bbolt; the test suite runs against bbolt. As additional backends are added, the same test suite must run against each.
- **Storage compatibility test suite** — a portable suite verifies any backend implementing `Graph` + `VectorIndex` against the contract: CRUD correctness, prefix-scan ordering, transaction atomicity (including aborted txs leaving consistent state), bidirectional neighbor lookup, and tombstone semantics. Every new backend must pass before merge.
- **HNSW oracle test** — property-based: for each random (corpus, query) pair, top-K from HNSW agrees with top-K from flat scan above a configured recall floor (e.g., recall@k ≥ 0.95 for k=8 with `oversampleN=300`). Run on multiple corpus sizes spanning the threshold.
- **Service-level tests** — target `MemoryService` directly without any transport adapter. Proves the service is transport-agnostic and that adapters can be swapped without re-validating service logic.
- **Transport adapter tests** — for each adapter, drive the wire protocol with an in-process client and verify it lands on the right `MemoryService` calls with the right arguments. The service is stubbed only here, where the test is about marshalling.
- **Embedder mock**: deterministic hash-to-vector implementation in `internal/embed/fake/`.
- **Live Gemini smoke test**: gated behind `GEMINI_API_KEY`, skipped otherwise.
- **Property tests** for chunking (idempotence, span correctness), decay (monotone in `dt`, bounded by `weightCap`, time-symmetric within ε under reinforcement), and HNSW invariants (every link is bidirectional; entry point exists; layer histogram approximately exponential).
- **Statelessness check** — a test that runs N concurrent requests against a process and asserts no module under `internal/` accumulates persistent state visible from a runtime introspection point. Easier in practice: code review + a periodic `pprof heap` snapshot in CI that fails if non-connection allocations grow with request count.
- **Multi-node simulation (post-bbolt)** — once a multi-writer backend lands, run two memmy instances against the same backend in tests, hammer concurrent reads/writes, and assert correctness invariants hold.
- **Clock injection**: a `Clock` interface (`Now() time.Time`) plumbed wherever decay math runs.

---

## 15. Open Questions / Future Work

- **Additional storage backends.** Beyond bbolt: Postgres, MariaDB, Bigtable, Spanner, badger, pebble. Each implements `Graph` + `VectorIndex` and passes the storage compatibility suite (§14).
- **Additional transports.** gRPC (with proto stubs, mTLS, structured authn) and HTTP (REST-ish, JSON, gateway-friendly). Both wrap the existing `MemoryService` — no service changes required.
- **Multi-node memmy.** Statelessness (§0 #3) means N memmy processes can run against the same multi-writer backend with no coordination. Operational concerns: load balancer, embedder rate-limit coordination if needed.
- **Sentence splitter quality.** Rule-based first. Revisit with model-based if quality is poor.
- **Cross-tenant search.** Out of scope for v1; would require an ACL model.
- **Embedding model upgrades.** Strategy for rotating models without invalidating the entire index. Likely: namespace `vectors` and `hnsw_*` by `(model_name, version)` and migrate lazily on read.
- **Multi-modal memory.** Schema can extend; embedder grows accordingly.
- **Active forgetting.** Explicit "forget topic X" RPC.
- **HNSW deletion compaction.** Tombstones + filtered search work for v1. If churn is high, add a periodic compaction pass that hard-deletes and rebuilds affected neighbor lists.
- **Lexical / sparse retrieval.** Reserved interface in §9. Add `LexicalIndex` (BM25 or sparse-vector) if dense retrieval underperforms on rare tokens, IDs, or proper nouns. Do not pre-build.
- **Cold-start memory edges (`EdgeColdSemantic`).** If usage-only edges leave early UX too thin (§7.4).
- **Native vector types.** For SQL backends, consider whether to use pgvector / equivalent or stay with opaque `bytea`. Decision made per backend; the contract (§4.8) is unchanged.
- **Observability.** Prometheus metrics on retrieval latency (split flat-scan vs HNSW; split per-transport), edge-creation rate, HNSW recall vs flat-scan oracle on a sampled query, decay distribution, average graph degree, weight histograms.

---

## 16. Glossary

- **Chunk** — a 3-sentence sliding window over a message; the unit of embedding and unit of memory.
- **Node** — graph vertex backed by one chunk. Decaying `Weight`, monotonic `AccessCount`.
- **Memory edge** — directed Hebbian association between two nodes. Decaying `Weight`, kind, monotonic `AccessCount`/`TraverseCount`. Lives in `memory_edges_out` (and mirror `memory_edges_in` for KV backends).
- **HNSW link / HNSW record** — vector-space navigation link in the HNSW graph. Static (created at insert, mutated only at insert/delete). Lives in `hnsw_records`. Distinct from memory edges.
- **Tenant** — identity scope; normalized tuple hashed to `TenantID`.
- **Seed** — node returned by `VectorIndex.Search` post-rerank; entry point for memory-graph expansion.
- **Hebbian co-retrieval** — strengthening a memory edge between two nodes that appeared together in a top-K seed set.
- **Co-traversal** — strengthening a memory edge that delivered a node into the final returned result set.
- **Lazy decay** — computing exponential decay on read inside the same transaction as reinforcement. No sweeper.
- **HNSW** — Hierarchical Navigable Small World; the disk-resident ANN index over the `vectors` collection. Layered graph, navigation by greedy descent + ef-search.
- **Oversample** — pulling top-N candidates with N ≫ K so weight-aware reranking has room to lift hot-but-moderately-similar memories.
- **Flat scan** — storage cursor over `vectors`, computing similarity per visit, top-N heap. Memory O(N) per query, regardless of corpus size.
- **Storage backend** — the concrete implementation of `Graph` + `VectorIndex` rooted at a physical store. v1: bbolt. Future: Postgres, MariaDB, Bigtable, Spanner, badger, pebble.
- **Collection** — a logical grouping of records with a shared key shape (e.g., `vectors`, `nodes`). Each backend maps collections to its native primitive (bucket, table, column family).
- **Port IN / Port OUT** — ports-and-adapters terminology. Port IN (`MemoryService`) defines what the application accepts; ports OUT (`Embedder`, `VectorIndex`, `Graph`) define what the application requires.
- **Transport adapter** — implementation that translates a wire protocol (MCP, gRPC, HTTP) to/from `MemoryService`.
- **Stateless** — no in-memory data state across requests; see §13.2 statelessness contract.
