# memmy-eval — Validation Framework

Local-only harness for measuring memmy's retrieval quality and decay /
reinforcement dynamics against a controllable corpus.

The framework code ships in this repo (`cmd/memmy-eval`,
`internal/eval/...`). Datasets live OUTSIDE the repo — by default at
`~/.local/share/memmy-eval/<name>/` — and are gitignored.

## Pipeline

```
┌──────────┐  ingest   ┌────────────┐  queries  ┌──────────────┐  run / sweep   ┌───────────────┐
│ JSONL    │ ────────▶ │ corpus +   │ ────────▶ │ labeled      │ ─────────────▶ │ runs/<id>/    │
│ sessions │           │ embedcache │           │ queries      │                │ summary +     │
└──────────┘           └────────────┘           └──────────────┘                │ queries.jsonl │
                                                                                └───────────────┘
```

Each subcommand is idempotent on its inputs. Re-running `ingest` on an
unchanged session file is a no-op; re-running `queries` only adds new
queries; `run` always writes a fresh, time-stamped output directory.

## Quickstart (no API keys)

```bash
go build ./cmd/memmy-eval

# 1) Extract a few session files using the deterministic fake embedder.
./memmy-eval ingest \
  --sessions ~/.claude/projects/<project> \
  --dataset alpha \
  --embedder fake --fake-dim 32 \
  --limit 5

# 2) Generate labeled queries from the ingested corpus.
./memmy-eval queries --dataset alpha --n 10

# 3) Replay into a fresh memmy db, run the queries, write metrics.
./memmy-eval run --dataset alpha --embedder fake --fake-dim 32

# 4) See what's stored.
./memmy-eval ls
```

Output:

```
~/.local/share/memmy-eval/alpha/
  manifest.json                 # dataset provenance
  corpus.sqlite                 # turns + source-file dedup
  corpus.sqlite.embcache        # content-addressed embedding cache
  queries.sqlite                # labeled queries + judgments cache
  runs/run-XXXXXX/
    manifest.json               # per-run config + memmy git SHA
    memmy.db                    # the live memmy store from this run
    queries.jsonl               # one row per query: hits, gold flags, scores
    summary.json                # aggregate recall@k, MRR, nDCG, reinforcement
```

## With Gemini

```bash
export GEMINI_API_KEY=...
./memmy-eval ingest --sessions <path> --dataset alpha \
    --embedder gemini --gemini-model gemini-embedding-2 --gemini-dim 768
./memmy-eval queries --dataset alpha --n 50
./memmy-eval run --dataset alpha --embedder gemini --config eval/configs/baseline.yaml
```

Embeddings are cached at `<dataset>/corpus.sqlite.embcache`, keyed by
`(model, dim, sha256(text))`. Re-running ingest after adding sessions
only embeds the new chunks. The cache survives across runs and is what
makes parameter sweeps cheap.

## Parameter sweeps

```bash
./memmy-eval sweep \
  --dataset alpha \
  --matrix eval/configs/sweep.yaml \
  --embedder fake --fake-dim 32
```

The sweep YAML defines a base config plus a matrix of overrides (one
fresh memmy db per entry). Each entry produces its own
`runs/<sweep_id>/<entry_name>/` directory.

## Dataset root

Default: `~/.local/share/memmy-eval/`.
Override per command: `MEMMY_EVAL_HOME=/some/path ./memmy-eval ...`

Datasets are isolated — `--dataset alpha` and `--dataset beta` never
share files.

## Interpreting results

Two kinds of metrics live in `summary.json`:

- **IR quality** (`overall_recall_at_k`, `overall_mrr`, `overall_ndcg`)
  — how often the gold turn ranked in the top-K. `0` for distractor
  queries by design (no gold labels).
- **Dynamics** (`overall_reinforcement_mean`) — how much the top-K
  hits' weights changed across the query battery. Non-zero values
  indicate the implicit Recall reinforcement path actually fired.

Compare *deltas between runs*, not absolute values. The fake embedder
randomizes similarity, so absolute recall numbers there are not
meaningful — but the same fake corpus gives you a stable baseline for
measuring *changes* under different memmy configs.

## Visualization (out of scope of the binary)

`runs/<id>/queries.jsonl` is JSON-lines and `summary.json` is plain
JSON. Load both into Python notebooks or any plotting tool. The
framework deliberately does not ship a visualization layer — that part
lives in `eval/notebooks/` (gitignored).

## Tests

```bash
go test ./internal/eval/...
```

All tests use the deterministic `internal/embed/fake` embedder + a
fake judge in `internal/eval/queries`, so no API key is needed.
