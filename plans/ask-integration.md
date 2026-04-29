# Plan — embed memmy into `ask`

Status: idea capture. Not scoped, not scheduled. This file exists so the
context behind the decision survives the conversation it came out of.

## Background

We compared memmy against [thedotmack/claude-mem](https://github.com/thedotmack/claude-mem)
and [Cidan/ask](https://github.com/Cidan/ask). claude-mem solves the
"persistent memory for Claude Code" *product* problem; memmy solves the
*memory primitive* problem. They are not competitors — claude-mem could
be a consumer of memmy.

`ask` is a much better host than a Claude Code plugin would be. It is
already a Go process that owns the stream, the tabs, the MCP bridge,
the cwd, and the agent subprocess. It is the **harness**, not the
agent. Memory is a harness responsibility, not an agent responsibility.

## Thesis

> Memory belongs to the harness, not the agent.

claude-mem puts memory in lifecycle hooks because that was the only
seam available from the outside. `ask` is the seam. Embedding memmy in
`ask` means recall/write/reinforce are first-class harness operations,
the same way `ask_user_question` and `approval_prompt` already are.

## What `ask` already has that makes this cheap

- A streaming JSON window into every Claude turn (`update.go` parses
  `stream-json` events — assistant messages, tool_use, tool_result,
  todo updates). claude-mem fights to recreate this from the outside;
  `ask` has it natively.
- Per-tab session identity: cwd, session id, MCP bridge, claude
  subprocess. Maps cleanly to a memmy tenant tuple
  `{project: <cwd>, scope: "ask"}`. memmy's optional tenant schema
  (DESIGN.md §3.1) is designed for exactly this shape.
- An embedded MCP server per tab (`mcp.go:140` — `mcpBridge`,
  Streamable-HTTP). Adding `memory.*` next to `ask_user_question` /
  `approval_prompt` is the same `mcp.AddTool(...)` pattern. Claude
  already has `--mcp-config` pointed at this endpoint, so memory tools
  become available with **zero install steps for the user**.
- An embedded-plugin pattern (`usage_plugin.go:11`, `plugins/ask-usage/`)
  that extracts a Go-embedded plugin tree to `~/.cache/ask/plugins/<name>/`
  and passes it to `claude --plugin-dir`. A second plugin
  (`plugins/ask-memory/`) drops in beside `ask-usage` with a
  `SessionStart` / `UserPromptSubmit` hook.
- Multi-provider already: Codex, ollama, etc. (`codex.go`, `ollama.go`,
  `provider.go`). Memory lives below the provider boundary, so it works
  across all of them with no extra effort.

## What memmy gets out of it

- A real ingestion pipeline. memmy is currently a passive store; the
  agent has to call `memory.write` deliberately, which means in
  practice agents under-write. `ask` already sees every turn and can
  enqueue writes for free.
- A real-world reinforcement signal. `ask` sees both the recall *and*
  the assistant response that uses it — that is the exact loop where
  `memory.reinforce` is meaningful. From inside Claude Code,
  claude-mem only sees the read side.
- A UI. Bubble Tea modal for `/memory` (browse, mark, forget, see
  weights) is straightforward in `ask`'s existing infrastructure.

## What the integration looks like (sketch, not commitment)

In `ask`:

- **`memory.go`** — owns a single `*memmy.Service`, constructed once
  at startup against SQLite at `~/.local/share/ask/memmy.db`. Each
  tab's `mcpBridge` gets a reference to it.
- **Tools registered on the existing per-tab `mcpBridge`**:
  `memory.recall`, `memory.write`, `memory.reinforce`,
  `memory.demote`, `memory.mark`, `memory.forget`, `memory.stats`
  — same surface memmy already exposes today.
- **Tenant tuple per tab**: `{project: tab.cwd, scope: "ask"}` via
  memmy's optional tenant schema, so the LLM sees the constraint in
  the tool's `inputSchema`.
- **Passive observer in `update.go`**: at turn end (already detected
  for status chips and history), enqueue a `memory.write` of the
  user prompt + assistant conclusion. **No subagent LLM. No XML
  schema. No Chroma.** This is the part that decisively differs from
  claude-mem's architecture — memmy's write path is embed → persist,
  cheap and synchronous.
- **`plugins/ask-memory/`** — embedded plugin tree (mirrors
  `ask-usage`). `SessionStart` + `UserPromptSubmit` hook calls
  `memory.recall` on the prompt and injects top-K hits as context.
- **`/memory` slash command** (later, optional) — Bubble Tea modal to
  browse recent recalls, see weights, manually mark / forget.

In memmy:

- Stay as-is. Keep `cmd/memmy/main.go`, the MCP-HTTP adapter, the
  MCP-stdio adapter — non-`ask` users still get a standalone server.
- `ask` consumes memmy as a Go module (in-process), bypassing the
  transport entirely. memmy's `MemoryService` interface (DESIGN.md
  §9.1) is the seam that makes this clean.
- One-source-of-truth, statelessness, lazy-decay, Hebbian
  reinforcement, refractory window — all unchanged. The principles
  in DESIGN.md §0 are why memmy is the right substrate to embed; do
  not erode them to make integration easier.

## Open questions

1. **Embedded library vs subprocess.** Default: embedded. memmy's
   `cmd/memmy` and stdio/HTTP adapters stay alive for non-`ask`
   users. `ask` just links memmy as a module. The SQLite file lives
   under `ask`'s data dir; if a user also runs standalone memmy they
   point it at a different DB.
2. **Auto-write granularity.** Per-turn (cheap, coarse) vs
   per-tool-result (rich, noisy). Probable answer: per-turn, with a
   heuristic for "interesting" tool results (file edits, errors, user
   corrections / re-prompts). Worth a separate design pass before
   coding.
3. **Structured fields on writes.** claude-mem's
   `(type, title, narrative, facts[], concepts[], files_*[])` shape
   is genuinely useful for filtering and rendering. Open whether to
   add optional `Node.Facets` to memmy (DESIGN-level decision) or
   keep it in `ask` only as serialized chunk text. Lean: optional
   facets on `Node`, mirror of the tenant-schema pattern, since it
   stays one source of truth and unlocks queryability.
4. **What to *not* port from claude-mem.** ChromaDB sync (violates
   one source of truth). Worker-as-state-keeper (breaks
   statelessness). Ad-hoc migration ladder. In-process LLM on the
   write path. XML observation schema. The 56KB worker-service.
5. **License.** memmy: undeclared. claude-mem: AGPL-3.0 (so do not
   copy code from it). `ask`: MIT. memmy needs an explicit license
   before embedding into `ask`; MIT is the obvious match.

## Non-goals (right now)

- Building this. This is a planning note, not a task list.
- Changing memmy's architecture to accommodate `ask`. The integration
  works *because* memmy stays principled; if it tries to push memmy
  into a non-stateless direction, the integration is wrong.
- Replacing claude-mem. Different audience. claude-mem is a polished
  Claude-Code-only product; `ask`+memmy is a multi-provider TUI with
  memory as one feature among many.
