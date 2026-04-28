# memmy

memmy is an LLM memory system written in pure Go (toolchain Go 1.26.2). It exposes Hebbian-reinforced, decay-aware memory to one or more agents over MCP (with gRPC and HTTP transport adapters reserved for future work). The first reference storage backend is bbolt; the same logical model maps to Postgres, MariaDB, Bigtable, Spanner, and other stores that satisfy the `Graph` and `VectorIndex` interfaces.

The load-bearing design principle is **one source of truth: the database**. Vectors, the HNSW navigation graph, nodes, messages, and Hebbian memory edges all live in the configured storage backend — there is no in-memory index, no secondary search engine, no parallel cache. memmy itself is **stateless across requests**: only connection pools, configuration, and process-local rate limiters are kept in-memory. This is what lets N memmy instances scale out behind a multi-writer backend without coordination.

## Documents

- [DESIGN.md](DESIGN.md) — architecture, data model, retrieval pipeline, decay math, and the load-bearing principles in §0.
- [CLAUDE.md](CLAUDE.md) — coding conventions and architectural rules to follow when changing the codebase.
- [IMPLEMENTATION.md](IMPLEMENTATION.md) — the running implementation checklist.

## Build & run

```sh
go build ./cmd/memmy
cp memmy.example.yaml memmy.yaml   # then edit
./memmy --config memmy.yaml
```

No transport is enabled by default — `server.transports` must explicitly declare which transport(s) to run, or the config fails validation. `memmy.example.yaml` ships with the streamable MCP HTTP transport enabled on port 8765 and the stdio transport disabled. Switch `embedder.backend` to `gemini` and provide `GEMINI_API_KEY` for production use.

memmy also supports the **MCP stdio transport** for use as a child process under an MCP-aware host (editor or agent runtime). Set `server.transports.stdio.enabled: true` and disable every other transport — stdio is mutually exclusive with HTTP listeners because it owns the process's stdin/stdout. Logs always go to stderr.

An optional **tenant schema** (`tenant:` block in the config) constrains the shape of the `tenant` field on every memory.* call. The schema is rendered into the MCP tool's `inputSchema` so the LLM sees the rules during tool listing, and invalid calls return a structured corrective error. See `memmy.example.yaml` for a worked example using `project` (absolute path) and `scope: "global"` (cross-project) keys, and DESIGN.md §3.1 for semantics. Without a schema, any string-keyed tuple is accepted (today's default).

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

```sh
go test ./...
go test -race ./...
```

Storage tests run against a real bbolt database in `t.TempDir()` — there are no storage mocks. The HNSW implementation is verified against a flat-scan oracle (recall@k floor enforced in tests).
