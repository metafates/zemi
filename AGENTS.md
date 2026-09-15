# AGENTS.md — zemi

zemi is a terminal AI agent harness whose userland (default UI, keymaps, widgets, learning
machinery) is written in embedded Lua while Go owns the runtime: the agent loop over
`charm.land/fantasy`, the bubbletea v2 TUI, the sandboxed gopher-lua interpreter, and SQLite
persistence. `DESIGN.md` (1550 lines) is the authoritative spec; code comments cite its
sections (`DESIGN §4`, `§6.2`, ...). **Before changing behavior, read the DESIGN section the
code cites** — it explains the why (usually a security or concurrency invariant), and a
change that contradicts it is almost always wrong.

### Reference codebases (local checkouts, study don't copy)

`DESIGN.md`'s "prior art" all have local checkouts under `~/Code/github/` — pull ideas and
patterns from them instead of reinventing or guessing at APIs:

- `~/Code/github/crush` — the primary prior art, same Go stack (bubbletea, fantasy);
  tool descriptions, permission flow, and store patterns trace back to it
- `~/Code/github/pi` — minimal prompts and a nicely modular architecture; the best
  reference for prompt wording and package boundaries
- `~/Code/github/hermes-agent` — learning-from-user machinery, relevant to `internal/learn`
- `~/Code/github/codex`, `~/Code/github/opencode` — other coding-agent harnesses for
  cross-checking tool behavior and UX decisions

## Commands

No Makefile, no CI, no lint config. Plain Go toolchain (module `github.com/metafates/zemi`,
Go 1.27):

```
go build ./...          # build
go test ./...           # full suite (~20s, all pure Go, no network, CGO not needed)
go vet ./...            # must stay clean
go test ./internal/ui -update   # regenerate UI golden files (internal/ui/testdata/*.golden)
go run .                # run the TUI against your real ~/.config/zemi
```

For testing against a throwaway config tree, redirect all dirs: `ZEMI_CONFIG_HOME`,
`ZEMI_DATA_HOME`, `ZEMI_STATE_HOME` (see `internal/config/dirs.go`). Most tests don't need
this — they inject `config.Dirs{...}` with `t.TempDir()` directly.

The project-config trust prompt and `~/.config/zemi/init.lua` only exist on a real machine;
tests boot with empty dirs, so code that requires them needs explicit setup in the test.

## Architecture and control flow

`main.go` is wiring only. Boot order (each step is a `lua.BootStep`, see `main.go:64`):
host namespaces (`zemi.*`) -> builtin commands -> memory + reflector commands ->
`require("zemi.defaultui")` (shipped theme/widgets/keymaps in `runtime/lua/zemi/`) ->
plugins -> user `init.lua` -> project `.zemi/init.lua` (behind a one-time trust prompt that
hashes the config tree, refuses symlinks) -> crush.json provider import -> `app.ready`.
Boot runs on its own goroutine and may block on the trust prompt; results reach the UI via
a hook channel.

### The concurrency rules (DESIGN §4) — violate these and things deadlock or corrupt

1. **Only the Lua actor goroutine touches the `lua.LState`.** Everything else goes through
   the queue in `internal/lua` with priority classes `ui > agent > io`. `ctx` cancellation
   preempts running Lua at the next opcode (catchable once, then re-raises).
2. **Watchdog / hot mode.** A wedged request trips the watchdog; after a grace period the
   runtime goes "hot" — every Lua request fails fast until force-reload builds a fresh
   `LState` (generation). All registrations (keymaps, widgets, events, timers, jobs,
   Lua tools) are generation-tagged (`hostapi.Gen`) and purged before replay. Never keep a
   pointer into an old generation across a reload.
3. **UI single-writer.** All `ui.App` state mutates on the bubbletea Update goroutine.
   Work from other goroutines (host hooks, Lua callbacks) is funneled through a hook
   channel drained by a persistent cmd (`internal/ui/hooks.go`). hostapi never imports
   internal/ui — it defines `UIHooks` (internal/hostapi/host.go:26) and the App implements
   it; keep that dependency direction (ui -> hostapi, never the reverse).
4. **UI-critical Lua handlers (keymaps, widget render, event handlers) must be fast**
   (~10 ms). Long work belongs in `zemi.job` / `zemi.http` / `zemi.timer`.
5. **Vetoable events** (`permission.request`, `app.quit`, `composer.submit`, `tool.pre`) have
   a 50 ms Lua-handler deadline and then take a logged default (fail-closed for permission
   asks without an overlay leg).
6. **The store** (`internal/store`) is SQLite via modernc.org/sqlite (pure Go), WAL,
   `MaxOpenConns(1)`. Two handles: global DB (user-scope memory, kv, revisions) and a
   per-project DB (sessions, messages, todos, tool_calls) at
   `<state>/projects/<sha256(projectDir)[:16]>/zemi.db`. Schema is one `schema` const of
   `CREATE TABLE IF NOT EXISTS` + FTS5 triggers executed on open — add tables there, there
   is no migration tool.

### Tool pipeline (internal/tools/tools.go)

Every model tool call runs: JSON unmarshal -> schema validation -> `tool.pre` Lua events
(deny / halt / rewrite) -> permission gate -> run -> `tool.post` notification. Key
invariants: a rewritten input **re-enters at the permission gate** (per-run pipeline copy
with `RewriteLoosened`, so a non-user rewrite can't ride a pre-existing allow glob); ask
with no overlay available fails closed; errors become tool-error responses, not panics.
Bash is special: `perm.Store.CheckCommand` normalizes the command first (compound
splitting, redirect gates, env-assignment gate — see internal/perm/bash.go, heavily
table-tested; extend those tables when touching it).

### Permission engine (internal/perm)

Rules `(subject, pattern, decision, not_match?, principal?)`, subjects like `tool.bash`,
`fs.read`, `fs.write`, `net.<host>`, `lua.<ns.call>`. Ordering: deny > ask > allow;
within one decision, fewest wildcards then longest literal prefix wins. A `not_match` hit
falls through and **forces ask**. The default when nothing matches is set by
`Store.Default` (host wires `fsDefault`: jail reads allow except secret-shaped paths,
writes ask) and finally ask — there is no fail-open path. New host-API side effects must
be gated with the same `lua.` subject at the Lua boundary.

### Origin model (internal/lua/origin.go, registries)

Every chunk carries an origin from its resolved path class: `user` (~/.config/zemi) >
`builtin` (embedded runtime/) > `plugin` (site/pack) > `project` (<project>/.zemi);
`learned` code is denylisted from dangerous APIs. Registries enforce: cross-origin
collision is **refused** with the higher-trust origin winning; within one origin it is
last-writer-wins. Project origin additionally cannot register providers, profiles,
`tool.pre` rewrites, builtin tool names, or emit host-owned events (only `user.*` and
catalogued events are emittable, internal/hostapi/event.go). When adding a registry,
follow `internal/hostapi/registry.go`.

## Testing patterns

- `internal/hostapi/host_test.go` has the canonical helpers: `newTestHost(t)` boots a
  fresh runtime + namespaces step with temp dirs; `runUser(t, r, luaSrc)` executes a chunk
  as user origin **with a 3s guard so an interpreter deadlock fails loudly instead of
  hanging the binary** — reuse it for any test that runs Lua.
- API surface is pinned: `TestNamespaceVersion` asserts the exact `zemi.api.version`
  ("0.1.0-m1") and namespace members. Growing the Lua API means updating that test and the
  version string.
- UI output is pinned with golden files (`charmbracelet/x/exp/golden`): commit
  `internal/ui/testdata/*.golden` changes only when the rendered change is intended;
  regenerate with `go test ./internal/ui -update` and eyeball the diff.
- Tests drive behavior through Lua (`zemi.memory.add(...)` etc.) rather than calling Go
  internals where the Lua contract is the thing being pinned; Go-internal units (perm
  normalizer, learn intake, store) get table tests.
- The sandbox, freeze, and origin-classification behavior in `internal/lua/sandbox_test.go`
  is adversarial by design (monkey-patching attempts must raise); don't weaken those
  assertions to make unrelated changes pass.

## Gotchas and non-obvious conventions

- **DESIGN.md §12 does not match reality** (it predates the code): the actual layout is
  `internal/tools` (not `internal/agent/tools`), `internal/perm` (not `internal/permission`),
  no `internal/skills` or `internal/agent/templates` yet. Trust the tree, not §12; §13
  milestones explain what exists (M0-M2 are largely in, M3+ partly).
- **`zemi.*` namespaces are frozen and integrity-checked.** Tables are metatable-locked,
  `rawset`/`rawequal` are stripped, and Go re-verifies namespace contents per request
  class. Anything that adds a runtime-mutable member to a namespace must go through the
  Go side, and each Go function exposed to Lua runs under `recover` (a panic must become a
  catchable Lua error, logged as a zemi bug).
- **Error convention (DESIGN §6.0):** misuse (bad arguments, masked mutator) raises;
  runtime failure of a sync op returns `nil, err`; async ops deliver through their handle.
  Keep this split when adding APIs.
- **Sanitize at boundaries.** Model-influenced strings pass through `internal/sanitize`
  before reaching widgets, overlays, or the log; logging is one-record-per-line and
  sanitized. Secrets (key-shaped strings) are scrubbed by the learn intake scrubber at the
  memory/store boundary — don't log raw tool output or memory text.
- **Memory intake policy** (`internal/learn`): text cap 512, fixed kind enum
  (fact/gotcha/preference/convention/decision), provenance required, `origin` is `user` or
  `learned` and gates autonomy/supersede/digest-labeling. The autonomy knob is host-owned:
  only Go may flip it (`Settings.SetHost`), never user config.
- **Config store** (`internal/config/store.go`): dotted-path keys; `SetHost` is Go-only,
  user config cannot set host-owned knobs. Providers ship none; they are registered from
  user config (or imported from the user's `~/.config/crush/crush.json`, best-effort,
  never fails boot — `providers.crush_json` key).
- **The Go-reserved triggers** (`ctrl+c` cancel, `:Reload`) are never routed through Lua
  keymaps and cannot be rebound — keep them out of any "make everything rebindable"
  refactor.
- **Handles vs snapshots**: host resources (job, http, timer, session) are handle tables
  with colon methods; everything else is a plain snapshot copy. Marshaling across the Lua
  <-> Go boundary caps string size (16 MB) and depth (64) and refuses cycles.
- `.crush/` in the repo root is this workspace's Crush CLI session data (gitignored) — not
  part of the project.
- No vendoring; the module has no generated code. Commits are small and imperative
  (`git log` shows the style: lowercase, no prefix convention).
