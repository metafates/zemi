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
- `~/Code/github/neovim` — the model for the Lua userland (`zemi.*` mirrors `vim.*`):
  `runtime/lua/vim/` for keymap/highlight/ui.select/health/secure/loader patterns,
  `src/nvim/api/` for autocmd groups and user commands

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

`main.go` is wiring only. Boot order (each step is a `lua.BootStep`, see `main.go:66`) — order
matters and permissions sit early so no userland code runs before the permanent store loads:
host namespaces (`zemi.*`) -> `PermissionsLoadStep` (reads `permissions.lua`, fail-closed) ->
builtin commands -> memory + reflector commands -> skills commands ->
`require("zemi.defaultui")` (shipped theme/widgets/keymaps in `runtime/lua/zemi/`) ->
plugins -> user `init.lua` -> authored skill hooks -> project `.zemi/init.lua` (behind a
one-time trust prompt that hashes the config tree, refuses symlinks) -> crush.json provider
import -> `app.ready` -> learned skill hooks (skills load last, lowest trust). Boot runs on
its own goroutine and may block on the trust prompt; results reach the UI via a hook channel.
After all steps, `EnsureConfigRepo()` lazily creates the config git repo (see
"Permanent rules" below).

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
   Corollary: never call `program.Send`/`program.Quit` from inside `Update` (or anything it
   calls) — bubbletea v2's `p.msgs` channel is unbuffered and `Send` blocks until the event
   loop receives, so the loop would wait on itself (the `/quit` hang). Wrap such calls in a
   goroutine, like `App.Quit` and `OpenModelPicker` do. Blocking hooks (`PickModel`,
   `AskPermission`, `AskTrust`) must only ever run on non-UI goroutines.
4. **UI-critical Lua handlers (keymaps, widget render, event handlers) must be fast**
   (~10 ms). Long work belongs in `zemi.job` / `zemi.http` / `zemi.timer`.
5. **Vetoable events** (`permission.request`, `app.quit`, `composer.submit`, `tool.pre`) have
   a 50 ms Lua-handler deadline and then take a logged default (fail-closed for permission
   asks without an overlay leg).
6. **The store** (`internal/store`) is SQLite via modernc.org/sqlite (pure Go), WAL,
   `MaxOpenConns(1)`. Two handles: global DB (user-scope memory, kv, skills, config
   revisions) and a
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

### Skills (internal/skills, internal/hostapi/skills.go)

Skills are file-based Lua units under `<config>/skills/` with YAML frontmatter (name, desc,
body file); discovery lives in `internal/skills`, hosting in `internal/hostapi/skills.go`,
rows in the global DB `skills` table. Invariants:

- **Quarantine until enabled.** A skill is inert until `zemi.skills.enable` runs, and only
  user or builtin origin may enable/disable (`memoryManageAllowed` gate at the Lua namespace
  boundary — NOT inside the Go helper; off-interpreter goroutines read `OriginHost`, which
  would defeat a Go-side gate, see the concurrency rules). Enabling runs `zemi.skill.load:<name>`
  hooks: inline if already on the interpreter goroutine (hook fns are invoked from Lua, so
  they may run `runSkillHook` directly), otherwise enqueued via `h.RT.Do` — the
  inline-vs-Do split in `enableHook` is load-bearing.
- **Token efficiency**: the agent prompt carries only the index (name + description capped at
  160 runes); full bodies load on demand via the `skill` tool after a permission check.
- **Hook registration placement**: authored skill hooks register at `SkillsAuthoredHooksStep`
  (after user init.lua), learned ones at `SkillsLearnedHooksStep` (after `app.ready`) — the
  learned step must not re-run on reload replay without its `Gen` tag.

### Permanent rules + permissions.lua (internal/hostapi/permissions.go, repo.go)

DESIGN §6.1: `permissions.lua` at `ConfigHome` is **data, not code** — Go generates it via
`permDump()` (versioned `return { version = 1, rules = {...} }`) and reads it back with
`parsePermissionsDump()`, a strict hand-written parser that never evaluates Lua and accepts
only the exact generated shape (whitelisted string fields, no function calls, no concat,
bad file = boot error with empty permanent store). Only `user` origin may create allow or
permanent rules (project may tighten only); the gate lives at the `zemi.permission.rule`
namespace closure, same off-interpreter-`OriginHost` reasoning as skills. Adding a permanent
rule: `permRegenerate()` (tmp file + rename, 0600) then `commitConfigRevision()` — a
one-line commit ("the keypress is the review", §6.1) recorded in the global-DB
`config_revisions` table (row lands BEFORE the git commit — crash leaves a pending row,
the fail-closed direction). Git is the audit trail, files + DB are the source of truth;
missing git degrades to log warnings. Git helpers shell out to the `git` binary (no git
library in go.mod), run with `GIT_CONFIG_NOSYSTEM=1` and a 15s timeout.

`skills.autonomy` / `config.autonomy` knobs are currently no-ops for
skills (skill proposals are mandatory-review by design, DESIGN §10.4);
`config.autonomy = auto` lets session-scope deny/ask patches auto-apply.
The reflector (§10.3) may propose config patches as rule objects only —
`intakeConfigPatch` strict-decodes them (unknown fields refuse the patch)
and scrubs every leaf; pending patches park in the `cfgProposals` queue
and the Go-owned `/config` command opens the review card overlay (accept
applies session rules / permanent ones through the review commit with
message `learn: <title> (from <session id>)`, reject drops, esc leaves
queued). The review verdict travels as row IDs, never indices. Every
applied revision records its config-repo commit SHA (`config_revisions.
commit_sha`); `/config revisions` lists them and `/config revert <id>` does
`git revert` + a `revert:` revision row (the perm store reloads on the
next reload, not in-place).

## Testing patterns

- `internal/hostapi/host_test.go` has the canonical helpers: `newTestHost(t)` boots a
  fresh runtime + namespaces step with temp dirs; `runUser(t, r, luaSrc)` executes a chunk
  as user origin **with a 3s guard so an interpreter deadlock fails loudly instead of
  hanging the binary** — reuse it for any test that runs Lua.
- API surface is pinned: `TestNamespaceVersion` asserts the exact `zemi.api.version`
  ("0.1.0-m4") and namespace members. Growing the Lua API means updating that test and the
  version string.
- UI output is pinned with golden files (`charmbracelet/x/exp/golden`): commit
  `internal/ui/testdata/*.golden` changes only when the rendered change is intended;
  regenerate with `go test ./internal/ui -update` and eyeball the diff.
- Tests drive behavior through Lua (`zemi.memory.add(...)` etc.) rather than calling Go
  internals where the Lua contract is the thing being pinned; Go-internal units (perm
  normalizer, learn intake, store) get table tests.
- Skills/permissions tests: `newTestHostSkills(t)` boots namespaces + skills steps;
  `newTestHostSkillsNoBoot(t)` lays files down WITHOUT booting (for testing discovery of
  files that must exist first) — every `Boot` builds a fresh generation, so steps sharing
  state must be passed in ONE call. Permanent-rule tests guard git presence with
  `exec.LookPath("git")` and clean the dump via temp `ConfigHome`.
- The sandbox, freeze, and origin-classification behavior in `internal/lua/sandbox_test.go`
  is adversarial by design (monkey-patching attempts must raise); don't weaken those
  assertions to make unrelated changes pass.

## Gotchas and non-obvious conventions

- **DESIGN.md §12 does not match reality** (it predates the code): the actual layout is
  `internal/tools` (not `internal/agent/tools`) and `internal/perm` (not
  `internal/permission`); `internal/skills` exists, `internal/agent/templates` does not.
  Trust the tree, not §12; §13 milestones explain what exists (M0-M2 are in, M3 mostly).
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
- **The Go-reserved triggers** (`ctrl+c` cancel, `/Reload`) are never routed through Lua
  keymaps and cannot be rebound — `/Reload` is matched on composer submit (exact,
  case-sensitive, before the command registry is consulted), so no registered command can
  shadow it. Keep them out of any "make everything rebindable" refactor.
- **No editor modes.** The composer is always focused; commands are slash commands typed in
  the composer (`/quit`, `/help`, `/config`, ...) with the palette popup for completion
  (`internal/ui/palette.go`). `zemi.keymap.bind` still takes a mode label, but the UI routes
  `insert` bindings only (exact single keys; printable keys stay with the composer).
- **Mouse capture is load-bearing.** `App.View` sets `MouseModeCellMotion` so the wheel
  scrolls the transcript (the altscreen buffer has no native scrollback); without it wheel
  scrolling silently dies. Selection then needs shift-drag in the terminal. Composer
  editing keys (`ctrl+u`, `ctrl+k`, `ctrl+w`, `home`/`end`, ...) stay with the composer —
  scroll uses `pgup`/`pgdown`/`ctrl+home`/`ctrl+end`, bound in `defaultui.lua`.
- **Handles vs snapshots**: host resources (job, http, timer, session) are handle tables
  with colon methods; everything else is a plain snapshot copy. Marshaling across the Lua
  <-> Go boundary caps string size (16 MB) and depth (64) and refuses cycles.
- `.crush/` in the repo root is this workspace's Crush CLI session data (gitignored) — not
  part of the project.
- No vendoring; the module has no generated code. Commits are small and imperative
  (`git log` shows the style: lowercase, no prefix convention).
