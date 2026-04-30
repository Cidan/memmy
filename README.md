# memmy

memmy is an LLM memory system written in Go (toolchain Go 1.26.2). It exposes Hebbian-reinforced, decay-aware memory to one or more agents over MCP (with gRPC and HTTP transport adapters reserved for future work). The reference storage backend is **Neo4j** via the Bolt protocol — both the Hebbian memory graph and the HNSW navigation graph (Neo4j's native vector index) live in one database, and many memmy processes can share one Neo4j instance natively. The Bolt driver (`github.com/neo4j/neo4j-go-driver/v5`) is pure-Go; memmy has no CGO dependencies.

The load-bearing design principle is **one source of truth: the database**. Vectors, the HNSW navigation graph, nodes, messages, and Hebbian memory edges all live in Neo4j — there is no in-memory index, no secondary search engine, no parallel cache. memmy itself is **stateless across requests**: only connection pools, configuration, and process-local rate limiters are kept in-memory. This is what lets N memmy instances scale out behind a multi-writer Neo4j without coordination.

## Documents

- [DESIGN.md](DESIGN.md) — architecture, data model, retrieval pipeline, decay math, and the load-bearing principles in §0.
- [CLAUDE.md](CLAUDE.md) — coding conventions and architectural rules to follow when changing the codebase.
- [IMPLEMENTATION.md](IMPLEMENTATION.md) — the running implementation checklist.

## Setup

memmy needs a running Neo4j instance reachable over Bolt. Quick options:

- Docker: `docker run -p 7687:7687 -e NEO4J_AUTH=neo4j/<your-password> neo4j:2026.04`
- macOS: `brew install neo4j && brew services start neo4j`
- Linux/desktop: download Neo4j Desktop or install the community tarball.

Default connection: `bolt://localhost:7687`, user `neo4j`. Neo4j requires the password to be at least 8 characters; tests and the example config read it from `$NEO4J_PASSWORD`.

## Build & migrate & run

```sh
go build ./cmd/memmy
cp memmy.example.yaml memmy.yaml   # then edit; password reads ${NEO4J_PASSWORD} by default
NEO4J_PASSWORD=<yours> ./memmy migrate --config memmy.yaml   # apply schema, idempotent
NEO4J_PASSWORD=<yours> ./memmy serve   --config memmy.yaml   # run the server
```

`memmy serve` is the default subcommand, so `./memmy --config memmy.yaml` is equivalent to `./memmy serve --config memmy.yaml`. The server refuses to start if the database's schema version doesn't match the binary; the fix is always `memmy migrate`.

No transport is enabled by default — `server.transports` must explicitly declare which transport(s) to run, or the config fails validation. `memmy.example.yaml` ships with the streamable MCP HTTP transport enabled on port 8765 and the stdio transport disabled. Switch `embedder.backend` to `gemini` and provide `embedder.gemini.api_key` for production use.

memmy also supports the **MCP stdio transport** for use as a child process under an MCP-aware host (editor or agent runtime). Set `server.transports.stdio.enabled: true` and disable every other transport — stdio is mutually exclusive with HTTP listeners because it owns the process's stdin/stdout. Logs always go to stderr.

An optional **tenant schema** (`tenant:` block in the config) constrains the shape of the `tenant` field on every memory.* call. The schema is rendered into the MCP tool's `inputSchema` so the LLM sees the rules during tool listing, and invalid calls return a structured corrective error. See `memmy.example.yaml` for a worked example using `project` (absolute path) and `scope: "global"` (cross-project) keys, and DESIGN.md §3.1 for semantics. Without a schema, any string-keyed tuple is accepted (today's default).

## Use as a library

memmy ships a small facade at the module root for in-process embedding. The daemon (`cmd/memmy`) and the library use the same `MemoryService` underneath; the facade just skips the transport layer.

```go
import (
    "context"
    "os"

    "github.com/Cidan/memmy"
)

func main() {
    ctx := context.Background()

    emb, err := memmy.NewGeminiEmbedder(ctx, memmy.GeminiEmbedderOptions{
        APIKey: "...",
        Model:  "gemini-embedding-2",
        Dim:    3072,
    })
    if err != nil { /* ... */ }

    schema, err := memmy.NewTenantSchema(memmy.TenantSchemaConfig{
        Keys: map[string]memmy.TenantKeyConfig{
            "user":  {Required: true, Pattern: `^[a-zA-Z0-9_.-]+$`},
            "scope": {Enum: []string{"chat", "code"}},
        },
    })
    if err != nil { /* ... */ }

    neo4j := memmy.Neo4jOptions{
        URI:      "bolt://localhost:7687",
        User:     "neo4j",
        Password: os.Getenv("NEO4J_PASSWORD"),
        Database: "neo4j",
    }

    // Migrate once at deployment time, before Open. Idempotent.
    if err := memmy.Migrate(ctx, memmy.MigrationOptions{
        Neo4j: neo4j,
        Dim:   emb.Dim(),
    }); err != nil { /* ... */ }

    svc, closer, err := memmy.Open(ctx, memmy.Options{
        Neo4j:        neo4j,
        Embedder:     emb,
        TenantSchema: schema,
    })
    if err != nil { /* ... */ }
    defer closer.Close()

    if _, err := svc.Write(ctx, memmy.WriteRequest{
        Tenant:  map[string]string{"user": "alice", "scope": "chat"},
        Message: "Antonio prefers terse PR titles.",
    }); err != nil { /* ... */ }

    res, err := svc.Recall(ctx, memmy.RecallRequest{
        Tenant: map[string]string{"user": "alice", "scope": "chat"},
        Query:  "what does antonio like in PRs",
        K:      8,
    })
    _ = res
}
```

Notes:

- **`Embedder` is required and pluggable.** Use `memmy.NewFakeEmbedder(dim)` for tests or supply any type satisfying the `memmy.Embedder` interface.
- **`TenantSchema` is optional.** Pass `nil` (or call `NewTenantSchema` with an empty `TenantSchemaConfig`) to accept any tuple shape.
- **`closer.Close()` releases the Neo4j driver pool.** The embedder's lifecycle is the caller's; `Close` does not touch it.
- **Open enforces schema version.** If the database is older than what this binary expects, `Open` returns an error directing you to `memmy.Migrate(ctx, ...)`. Tests can pass `Options.SkipMigrationCheck: true` when they manage migrations themselves.
- **No transports start.** The facade is library-only — to expose `MemoryService` over MCP / HTTP, run `cmd/memmy` with a YAML config instead.
- **Tunable overrides use a pointer.** `Options.ServiceConfig` is `*ServiceConfig` — `nil` means "use defaults," and any non-nil value is treated as a complete config. To change one knob, start from `memmy.DefaultServiceConfig()`, mutate, and pass the address. The facade does not field-merge because some service tunables (`RefractoryPeriod`, `LogDampening`) accept zero as an intentional disable signal.

The full surface (request/result types, `EdgeKind`, `EmbedTask`, tunable `ServiceConfig`) is re-exported as type aliases on the `memmy` package; package internals stay under `internal/`.

## MCP tool surface

Seven tools, all rooted at the configured `MemoryService`:

| Tool                | When the LLM should call it                                                                                  |
|---------------------|--------------------------------------------------------------------------------------------------------------|
| `memory.write`      | Save a fact, decision, preference, or pattern worth remembering across conversations.                        |
| `memory.recall`     | Retrieve relevant memories before answering. Every call reinforces what it surfaces (Hebbian co-retrieval).  |
| `memory.reinforce`  | A specific recalled hit was actually useful in the answer.                                                   |
| `memory.demote`     | A specific recalled hit was misleading or wrong. Soft-inhibits without deleting.                             |
| `memory.mark`       | A stretch of recent context turned out to matter — retroactively boost the window.                           |
| `memory.forget`     | Erase outright. Use only for corrected misinformation, secrets, or explicit user request.                    |
| `memory.stats`      | Counts and average weights for one tenant or in aggregate.                                                   |

Reinforce/Demote/Mark go through a per-node refractory window (default 60 s) so a stuck or over-eager caller can't double-count or saturate the corpus. Demote clamps at `node_floor` and never deletes — `forget` is the hard-delete path. See DESIGN.md §8.2 for the implicit-vs-explicit reinforcement split.

## Tests

Storage and service tests connect to a real Neo4j; tenant isolation is per-test (each test gets a unique tenant prefix and a `t.Cleanup` that DETACH DELETEs everything written under it). Tests skip cleanly when `NEO4J_PASSWORD` is not set, so `go test ./...` succeeds on hosts without a Neo4j.

```sh
NEO4J_PASSWORD=<yours> go test ./...
NEO4J_PASSWORD=<yours> go test -race ./...
```

The `cmd/memmy` server is pure-Go and builds with `CGO_ENABLED=0`. The optional `cmd/memmy-eval` validation harness still depends on `mattn/go-sqlite3` for its local data stores (`corpus.sqlite`, `embedcache.sqlite`, `queries.sqlite`); that's a tooling-only dependency unrelated to memmy's runtime storage.
