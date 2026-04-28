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

The default configuration enables the streamable MCP HTTP transport on port 8765 with a fake (deterministic) embedder for development. Switch `embedder.backend` to `gemini` and provide `GEMINI_API_KEY` for production use.

## Tests

```sh
go test ./...
go test -race ./...
```

Storage tests run against a real bbolt database in `t.TempDir()` — there are no storage mocks. The HNSW implementation is verified against a flat-scan oracle (recall@k floor enforced in tests).
