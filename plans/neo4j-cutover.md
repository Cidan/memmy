# Neo4j Cutover — Remaining Work

End-to-end plan for finishing the SQLite → Neo4j replacement. Picks up from `neo4j-rewrite` branch HEAD `66c8b8d`. Treat this as a runbook: each step is concrete, file-level, and verifiable. The PRD lives at `.omc/prd.json`; this document expands the unfinished stories into actionable engineering work.

---

## 1. Current state (committed, do not redo)

Branch: `neo4j-rewrite`
HEAD: `66c8b8d` (3 commits past the eval-framework checkpoint at `39900e3`)
Build: `go vet ./...` clean, `go build ./...` clean. SQLite remains the live backend. The Neo4j storage package is complete and ready to wire.

| Path | Status |
|---|---|
| `internal/storage/neo4j/db.go` | ✅ Storage struct, Open, Close, session helpers |
| `internal/storage/neo4j/migrate.go` | ✅ embed.FS, Migrate, CurrentSchemaVersion, RequiredSchemaVersion |
| `internal/storage/neo4j/migrations/001_constraints.cypher` | ✅ uniqueness + lookup indexes |
| `internal/storage/neo4j/migrations/002_vector_index.cypher` | ✅ native HNSW vector index parameterized by `$dim` |
| `internal/storage/neo4j/graph.go` | ✅ Full Graph interface; bidirectional mirror dance gone |
| `internal/storage/neo4j/vectorindex.go` | ✅ Native vector index + flat-scan oracle path |
| `internal/storage/neo4j/counters.go` | ✅ Atomic per-tenant counter ops via MERGE+SET |
| `internal/storage/neo4j/scanners.go` | ✅ RecentNodeIDs, NodesForMessage, MessageIDsBefore, TenantStats |
| `internal/storage/neo4j/codec.go` | ✅ Bolt value coercion helpers |
| `internal/storage/neo4j/neo4jtest/neo4jtest.go` | ✅ Per-test tenant prefix + DETACH DELETE cleanup; skips when `NEO4J_PASSWORD` unset |
| `internal/storage/neo4j/db_test.go` | ✅ Migration smoke tests |
| `CLAUDE.md` | ✅ Stack, approved deps, testing section reflect Neo4j |
| `go.mod` | ✅ neo4j-go-driver/v5 added (mattn/go-sqlite3 still present) |

---

## 2. Connection details

The dev box has Neo4j running at:
```
URI:      bolt://localhost:7687
Username: neo4j
Password: neo4j
Database: neo4j
```

Test scaffolding reads these via env vars:
```bash
export NEO4J_URI=bolt://localhost:7687    # default if unset
export NEO4J_USER=neo4j                    # default if unset
export NEO4J_PASSWORD=neo4j                # REQUIRED — tests t.Skip without it
export NEO4J_DATABASE=neo4j                # default if unset
```

Once `NEO4J_PASSWORD` is set, `go test ./internal/storage/neo4j/...` should connect, migrate, and verify schema round-trip.

---

## 3. Remaining work, in dependency order

The remaining stories (US-NEO-006 through US-NEO-017) form a single coherent change. memmy.go's API rewrite cascades into ~30 files; the only sane way to land it is in lockstep — one focused branch, one big diff, one test-and-verify cycle. **Do not try to land memmy.go incrementally without updating every caller in the same change** — that produced ~30 build errors when I tried last session.

### 3.1 The API cutover (US-NEO-006 + US-NEO-008 + US-NEO-009 + US-NEO-011 + US-NEO-012)

The change everything else depends on. Touch all of these files in one diff:

**`memmy.go` (library facade)** — replace the SQLite-backed `Open` with a Neo4j-backed one:

```go
type Options struct {
    Neo4jURI       string         // required
    Neo4jUser      string         // required
    Neo4jPassword  string         // required
    Neo4jDatabase  string         // default "neo4j"
    ConnectTimeout time.Duration  // default 10s

    Embedder           Embedder      // required
    Clock              Clock
    ServiceConfig      *ServiceConfig
    TenantSchema       *TenantSchema
    FlatScanThreshold  int           // default 5000
    SkipMigrationCheck bool          // tests only
}

type MigrationOptions struct {
    URI, User, Password, Database string
    Dim int
    ConnectTimeout time.Duration
}

func Open(ctx context.Context, opts Options) (Service, io.Closer, error) {
    // validate, open neo4jstore.Storage, schema-version guard,
    // wire service, return.
    // on schema mismatch returns:
    //   "memmy: schema version mismatch (database is at vN, this build
    //    requires vM). Call memmy.Migrate() or run `memmy migrate` first."
}

func Migrate(ctx context.Context, opts MigrationOptions) error {
    // open temp Storage, call Storage.Migrate(ctx), close.
}
```

Drop entirely:
- `Options.DBPath`, `Options.BusyTimeout`, `Options.HNSW`, `Options.HNSWRandSeed`
- `HNSWConfig` re-export, `DefaultHNSWConfig` re-export

The full target body lived at this branch as commit-attempt-not-committed; reconstruct from the PRD's US-NEO-006 acceptance criteria. I have the file already drafted in conversation history but reverted it because the cascade broke without simultaneous caller updates.

**`internal/config/config.go`** — replace `SQLiteStorageConfig` with `Neo4jStorageConfig`:

```go
type Neo4jStorageConfig struct {
    URI            string        `yaml:"uri"`
    User           string        `yaml:"user"`
    Password       string        `yaml:"password"` // supports ${ENV_VAR}
    Database       string        `yaml:"database"`
    ConnectTimeout time.Duration `yaml:"connect_timeout"`
}

type StorageConfig struct {
    Backend string             `yaml:"backend"` // only "neo4j"
    Neo4j   Neo4jStorageConfig `yaml:"neo4j"`
}
```

Change `Default()` to return `Backend: "neo4j"` with sensible localhost defaults. Change `Validate()` to enforce `Backend == "neo4j"` and required Neo4j fields. Add `${ENV_VAR}` expansion to Password (look for the existing pattern on `gemini.api_key` if any; otherwise add a small `expandEnv(s string) string` helper).

`memmy.example.yaml` — replace the `sqlite:` block with `neo4j:`. Keep everything else.

**`cmd/memmy/main.go`** — convert to cobra (cobra is already an approved dep used by `cmd/memmy-eval`). Subcommands:

- `memmy serve --config memmy.yaml` (default if no subcommand): reads YAML, opens `memmy.Open()`, registers transports under suture supervisor, blocks on signal. **Refuses to start if schema not migrated** with message: `"memmy: database schema vN required, current vM. Run \`memmy migrate --config <path>\` first."`
- `memmy migrate --config memmy.yaml`: reads the same YAML for the neo4j block, calls `memmy.Migrate(ctx, MigrationOptions{...})`, exits 0 on success.

The existing main.go's transport wiring + suture loop stays the same; just relocate it under the `serve` cobra command.

**`internal/eval/inspect/inspect.go`** — full Neo4j rewrite. New surface:

```go
type Connection struct {
    URI, User, Password, Database string
}

func Open(conn Connection) (*Reader, error) {
    // dial Neo4j driver, return Reader holding it.
}

func (r *Reader) ListTenants(ctx) ([]Tenant, error)
func (r *Reader) ListNodes(ctx, tenant) ([]string, error)
func (r *Reader) NodeStates(ctx, tenant, ids) ([]NodeState, error)
func (r *Reader) NodeState(ctx, tenant, id) (NodeState, bool, error)
func (r *Reader) Close() error
```

`NodeState` struct stays the same (Weight, LastTouched, AccessCount, EdgeCountOut, EdgeCountIn). Cypher to compute edge counts:
```cypher
MATCH (n:Node {tenant: $tenant, id: $id})
OPTIONAL MATCH (n)-[r_out:STRUCTURAL|CORETRIEVAL|COTRAVERSAL]->()
WITH n, count(r_out) AS out_count
OPTIONAL MATCH (n)<-[r_in:STRUCTURAL|CORETRIEVAL|COTRAVERSAL]-()
RETURN n, out_count, count(r_in) AS in_count
```

**`internal/eval/harness/replay.go`** — drop `MemmyDBPath` from `ReplayOptions`. Add Neo4j connection details:

```go
type ReplayOptions struct {
    CorpusStorePath  string
    EmbedCachePath   string
    Embedder         embed.Embedder
    EmbedderModelID  string
    ServiceConfig    *memmy.ServiceConfig
    FlatScanThreshold int
    TenantTuple      map[string]string
    DatasetName      string
    Neo4j            inspect.Connection // NEW
}

type ReplayResult struct {
    Service       memmy.Service
    Closer        io.Closer
    Tenant        map[string]string
    FakeClock     *memmy.FakeClock
    Neo4j         inspect.Connection // NEW — eval inspect Reader uses this
    TurnsReplayed int
    NodesWritten  int
    StartedAt     time.Time
    FinishedAt    time.Time
}
```

`OpenService` and `Replay` now call `memmy.Open(ctx, memmy.Options{Neo4jURI: opts.Neo4j.URI, ...})`. Drop `HNSW *memmy.HNSWConfig` and `HNSWRandSeed` fields entirely.

**`internal/eval/harness/runner.go`** — `RunQueriesOptions.InspectPath string` → `RunQueriesOptions.InspectConn inspect.Connection`. Update the `inspect.Open(opts.InspectConn)` call site.

**`internal/eval/sweep/apply.go`** — DELETE `ApplyHNSWOverrides` entirely (Neo4j has no exposed HNSW tunables). Update `internal/eval/sweep/sweep_test.go` and `internal/eval/sweep/sweep_e2e_test.go` to drop their HNSW assertions.

**`cmd/memmy-eval/run.go` + `cmd/memmy-eval/sweep.go`** — drop all references to `memmy.HNSWConfig`, `memmy.DefaultHNSWConfig`, `sweep.ApplyHNSWOverrides`. The baseline-cache logic in `executeRun` can stay (it caches by config hash; just hash the `ServiceConfig` only). Replace `--hnsw-seed` and `--memmy-db` flags with `--neo4j-uri`, `--neo4j-user`, `--neo4j-password`, `--neo4j-database` (or have them read from env with the same defaults as `neo4jtest`).

**`cmd/memmy-eval/ingest.go`** — no changes needed (it doesn't open memmy directly; only embedcache + corpus.OpenStore).

After all of this lands together, `go build ./...` should be clean.

### 3.2 Test scaffolding (US-NEO-010) — already done

`internal/storage/neo4j/neo4jtest/neo4jtest.go` exists. The `Open(t, dim)` helper opens a Storage, applies migrations, registers DETACH DELETE cleanup. The `Connection` type is what `inspect.Open` and `harness.Replay` consume.

### 3.3 Storage tests (US-NEO-011)

Port from `internal/storage/sqlite/*_test.go` to `internal/storage/neo4j/*_test.go`. Use the neo4jtest helper. Each test gets its unique tenant prefix; concurrent tests don't interfere. Files to write:

- `graph_test.go` — Node CRUD, UpdateNode closure-error rolls back, tombstone, Message CRUD, Edge CRUD with directional traversal (Neighbors out + InboundNeighbors in agree), UpsertTenant + GetTenant + ListTenants
- `counters_test.go` — port the 400-randomized-op brute-force reconciliation test from `sqlite/counters_test.go`. Sanity that counters match a shadow walk after random Insert/Upsert/WeightBump/Delete sequences.
- `vectorindex_test.go` — Insert/Delete + Search top-K correctness + Size.
- `oracle_test.go` — recall floor: native vector index recall@8 ≥ 0.95 vs flat-scan baseline over a 2000×32-dim synthetic corpus. Ports from `sqlite/hnsw_test.go`. Note: Neo4j's vector index is approximate, so the recall floor target is what matters; you don't need to match SQLite-HNSW's exact numbers.
- `multiprocess_test.go` — two `neo4jstore.Storage` handles against the same database concurrently, writer in one + reader in the other, see each other's writes. The bolt driver handles this naturally.

### 3.4 Service / transport / library / eval tests (US-NEO-012)

For each test file, replace the SQLite open call with `neo4jtest.Open(t, dim)`:

- `internal/service/service_test.go`
- `internal/service/recall_test.go`
- `internal/service/reinforce_test.go`
- `internal/service/forget_test.go` (if separate)
- `internal/service/stats_test.go`
- `internal/service/write_test.go` (if separate)
- `internal/transport/mcp/mcp_test.go`
- `memmy_test.go` (top-level facade)
- `internal/eval/harness/harness_test.go`
- `internal/eval/harness/replay_edge_test.go`
- `internal/eval/sweep/sweep_e2e_test.go`
- `internal/eval/inspect/inspect_test.go`
- `internal/eval/inspect/concurrency_test.go`

Pattern (typical service test):
```go
func TestX(t *testing.T) {
    storage, _, _ := neo4jtest.Open(t, 32)  // dim 32 for fake embedder
    g := storage.Graph()
    v := storage.VectorIndex()
    cl := clock.NewFake(time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC))
    svc, err := service.New(g, v, fake.New(32), cl, service.DefaultConfig(), nil)
    // ... rest unchanged ...
}
```

Pattern (eval inspect test):
```go
func TestInspect(t *testing.T) {
    storage, conn, _ := neo4jtest.Open(t, 32)
    // write something via storage
    r, _ := inspect.Open(conn)
    defer r.Close()
    // ... rest as today ...
}
```

Pattern (eval harness test): Open a neo4jtest storage, capture the `Connection`, pass it as `harness.ReplayOptions.Neo4j`. The harness opens its own service via `memmy.Open(ctx, memmy.Options{Neo4jURI: conn.URI, ...})`.

### 3.5 SQLite removal (US-NEO-013)

Once all callers compile and tests pass against Neo4j:

```bash
rm -rf internal/storage/sqlite/
go mod tidy   # drops mattn/go-sqlite3 from go.mod and go.sum
```

Verify no remaining references:
```bash
grep -r "mattn/go-sqlite3" .
grep -r "sqlitestore" .
grep -r "internal/storage/sqlite" .
```
All three should return nothing (excluding this doc + git history).

### 3.6 Documentation (US-NEO-014)

**DESIGN.md** — replace SQLite with Neo4j in:
- §0 #1 (single source of truth = Neo4j)
- §1 (stack: Neo4j via bolt, native vector index)
- §2 (storage model: graph DB native; nodes + relationships + counters; no dual-mirror edge maintenance — Cypher relationships are single-direction at the storage layer but indexed both ways for free)
- §4.7 (full Cypher schema sketch — labels, constraints, vector index)
- §13.1 / §13.2 (operability — Neo4j multi-process semantics, schema migrations as explicit step)
- §13.3 (deployment topology — one Neo4j instance per memmy deployment; multi-tenant via property filtering today, optional multi-database in Enterprise)
- §14 (future work — Aura cloud, multi-database tenant isolation)

**README.md** — Setup section now requires Neo4j running. Document:
- Local Neo4j install (`brew install neo4j` / docker / Neo4j Desktop)
- Default connection bolt://localhost:7687 with neo4j/neo4j (rotate in production)
- `memmy migrate --config memmy.yaml` as the explicit schema-application step
- `NEO4J_USER` / `NEO4J_PASSWORD` env vars for tests
- Note that `mattn/go-sqlite3` and CGO are no longer required; pure-Go build

**memmy.example.yaml** — `sqlite:` block → `neo4j:` block:
```yaml
storage:
  backend: neo4j
  neo4j:
    uri: bolt://localhost:7687
    user: neo4j
    password: ${NEO4J_PASSWORD}
    database: neo4j
    connect_timeout: 10s
```

**IMPLEMENTATION.md** — append:
```markdown
## Round 10 — Neo4j replaces SQLite end-to-end

memmy migrates from SQLite (with manual HNSW) to Neo4j (with native
vector index + first-class graph). Schema bundled in the binary via
embed.FS; migrations are explicit (`memmy migrate` subcommand;
library exposes memmy.Migrate()).

### US-NEO-001 through US-NEO-017 — see PRD
```

with a one-line summary per story matching what landed.

### 3.7 Final regression + verifier + deslop (US-NEO-015 / 016 / 017)

```bash
NEO4J_PASSWORD=neo4j go vet ./...
NEO4J_PASSWORD=neo4j go build ./...
NEO4J_PASSWORD=neo4j go test ./...
```

Expected: all tests pass against the live Neo4j. No tests skip (since the user wanted everything tested). The previous suite was 217 tests; the Neo4j port may add or remove a few — confirm the count is in the same ballpark.

End-to-end smoke:
```bash
NEO4J_PASSWORD=neo4j ./memmy migrate --config memmy.example.yaml
NEO4J_PASSWORD=neo4j ./memmy serve --config memmy.example.yaml &
# ... in another shell, hit the MCP endpoint or run memmy-eval ...
```

Verifier sign-off via the `verifier` agent in thorough mode.

ai-slop-cleaner pass on the changed-file set, then re-run regression.

---

## 4. Critical gotchas (the things I learned the hard way)

### 4.1 The bolt driver returns variant types

Numeric properties come back as `int64` even when stored as `int`. Float properties may come back as `float64`. The `codec.go` helpers (`asInt`, `asInt64`, `asFloat`, `asString`, `asBool`) handle this — use them everywhere you decode a `rec.Get(name)` result. The pattern:

```go
raw, _ := rec.Get("weight")
w := asFloat(raw)  // robust to int64 / float32 / float64
```

### 4.2 Schema operations cannot share a tx with data writes

Neo4j errors out if you try to `CREATE CONSTRAINT ... ; MATCH ... SET ...` in the same tx. The migration system handles this by running each statement in its own auto-commit (`s.execAutoCommit`). Don't try to wrap migrations in a managed write tx.

### 4.3 Vector index dim must match the configured embedder dim

The vector index is created with a fixed dim at migration time (`$dim` parameter). If the embedder dim changes (e.g., switching from gemini-embedding-2 dim 3072 to fake-32 for tests), the index needs to be dropped + recreated. Two reasonable strategies:

- **Per-test fresh index**: `neo4jtest.Open` could DROP and CREATE the vector index per test invocation (slow). Skip for now — tests use dim=32 and the migrated index is whatever dim the first test ran with. Document the assumption.
- **Multi-database** (Enterprise only): one db per dim. Out of scope until the project goes Enterprise.

For now, document that the test dim is fixed at `32` and changing it requires manually dropping the vector index in the dev Neo4j (`DROP INDEX node_embedding_idx`) and re-running `memmy migrate`.

### 4.4 Per-tenant counter race

`MERGE (c:Counter {tenant: $tenant}) ON CREATE SET ... SET c.node_count = c.node_count + $delta` is atomic per row, but in Neo4j the MERGE+SET pattern does NOT prevent concurrent transactions from both creating two Counter rows for the same tenant if they race past the constraint check. The constraint `Counter.tenant IS UNIQUE` (in 001_constraints.cypher) prevents this — the second writer hits a constraint violation, the bolt driver retries the managed tx, the second attempt sees the row created by the first and falls into the SET branch.

If you see flaky counter tests under contention, the fix is to confirm the unique constraint is in place (`SHOW CONSTRAINTS` in cypher-shell).

### 4.5 The eval framework's `MemmyDBPath` field is dead weight

`internal/eval/harness/replay.go::ReplayOptions.MemmyDBPath` made sense when memmy was a SQLite file; it's meaningless for Neo4j. The harness opens a memmy service against a Neo4j db; the test isolation comes from `TenantTuple` (per-test prefix), not from a per-test file. Drop the field; rename it `Neo4j inspect.Connection`. The `cmd/memmy-eval/run.go::executeRun` baseline-cache is more questionable — it copies a SQLite file to skip the HNSW rebuild. Neo4j has no equivalent (you can't easily snapshot a Neo4j db while it's open). Two options:

- Remove the baseline cache entirely. With Neo4j, replay is cheap enough that it's not a perf cliff.
- Implement a Neo4j-native baseline by exporting via `apoc.export.cypher` (Enterprise / requires APOC plugin) or by tracking the test tenant's last replay state. Probably not worth it for the eval framework.

Recommend: rip the baseline cache when porting `executeRun`.

### 4.6 Test naming for tenant prefix

`neo4jtest.TenantPrefix(t)` returns `Test_<8-byte-random-hex>`. Always use the returned prefix as the tenant value (or as a prefix of the tenant value) so `t.Cleanup`'s DETACH DELETE catches everything. If a test creates multiple tenants, derive each from the same prefix:

```go
prefix := neo4jtest.TenantPrefix(t)
tenant1 := prefix + "_a"
tenant2 := prefix + "_b"
```

The cleanup query `MATCH (n) WHERE n.tenant STARTS WITH $prefix DETACH DELETE n` then catches both.

### 4.7 The `:Migration` node label

The schema check (`Storage.CurrentSchemaVersion`) reads `MATCH (m:Migration) RETURN max(m.version)`. The DETACH DELETE cleanup query in neo4jtest doesn't have a tenant filter for `:Migration` (migrations are global). So tests don't pollute it; the migration node persists across runs. That's intentional — once migrated, stay migrated.

If you ever need to reset the migration state (e.g., to test the migrate-from-scratch path), do it manually in cypher-shell:
```cypher
MATCH (m:Migration) DETACH DELETE m;
DROP CONSTRAINT node_tenant_id_unique IF EXISTS;
DROP CONSTRAINT message_tenant_id_unique IF EXISTS;
DROP CONSTRAINT tenant_id_unique IF EXISTS;
DROP CONSTRAINT counter_tenant_unique IF EXISTS;
DROP INDEX node_tenant_idx IF EXISTS;
DROP INDEX message_tenant_idx IF EXISTS;
DROP INDEX node_tenant_source_idx IF EXISTS;
DROP INDEX node_embedding_idx IF EXISTS;
```

---

## 5. Suggested ordering inside the next session

1. Set `NEO4J_PASSWORD=neo4j` in the env. Verify `go test ./internal/storage/neo4j/...` connects, migrates, and passes.
2. Write the storage tests (US-NEO-011) — graph, counters, vectorindex, oracle, multiprocess. Verify they pass.
3. Update `internal/config/config.go` to Neo4j-only.
4. Rewrite `memmy.go` with the new Options + Migrate API.
5. Rewrite `internal/eval/inspect/inspect.go` to Neo4j.
6. Update `internal/eval/harness/replay.go` to use Neo4j connection.
7. Drop `ApplyHNSWOverrides` from `internal/eval/sweep/apply.go`; update `cmd/memmy-eval/run.go` and `sweep.go`.
8. Convert `cmd/memmy/main.go` to cobra with `serve` and `migrate` subcommands.
9. Build everything; fix fallout. `go vet ./...` + `go build ./...` clean.
10. Port every test (US-NEO-012). Run them all green against the live Neo4j.
11. Delete `internal/storage/sqlite/`. `go mod tidy`. Verify no references remain.
12. Update DESIGN.md + README.md + memmy.example.yaml + IMPLEMENTATION.md.
13. Run `go test ./...` end-to-end. All green.
14. End-to-end smoke: `memmy migrate` then `memmy serve` then a manual MCP check or memmy-eval roundtrip.
15. Verifier agent (thorough). Apply any rejections.
16. ai-slop-cleaner on the changed-file set; re-run regression.

Estimated effort: 4-8 hours of focused work, depending on how cleanly the test ports go. The interfaces are already in shape; this is mostly a translation pass with a clear oracle (the SQLite tests) for what the right behavior is.

---

## 6. After this lands

- `internal/storage/sqlite/` is gone. The pluggable storage abstraction in DESIGN.md §9.2 is now backed by exactly one implementation. If we want to add Postgres+pgvector or Aura later, the interface is already shaped for it.
- The `hnsw` codepath in memmy is gone — no more layer assignment, no more bidirectional pruning, no more recall@k oracle test against our own implementation. Neo4j's vector index does that work.
- The dual-mirror edge dance (`edges_out` + `edges_in` in SQLite) is gone — Cypher relationships are first-class, indexed both ways for free.
- ~3,000 lines of SQLite-specific code deleted. ~1,800 lines of Neo4j-specific code added (already committed). Net code reduction.
- CGO build dependency goes away (was for `mattn/go-sqlite3`). Pure-Go binary.
- The eval framework's per-run baseline cache hack disappears (Neo4j replay is fast enough that it's not a perf cliff).
- Tests now require a running Neo4j; document the install step in README.

The existing eval framework's measurement story stays unchanged — same metrics, same dynamics, same labeled-query workflow. Just the storage backing memmy is different.
