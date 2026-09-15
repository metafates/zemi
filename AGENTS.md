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
builtin commands -> memory + reflector + config + skills commands -> agents (+ tasks),
workflow (+ `/review`) and mcp commands ->
`require("zemi.defaultui")` (shipped theme/widgets/keymaps in `runtime/lua/zemi/`) ->
plugins -> user `init.lua` -> authored skill hooks -> project `.zemi/` (`init.lua`,
`agents/`, `commands/`, `workflows/`; any of them triggers the one-time trust prompt that
hashes the config tree and refuses symlinks, and trust sets `Host.SetProjectTrusted`) ->
markdown commands (`commands/*.md`, user then trusted project) -> crush.json provider and
MCP import -> `app.ready` -> learned skill hooks (skills load last, lowest trust). Boot runs on
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
6. **No bare goroutines in hostapi.** Start them with `h.GoSafe(where, fn)`: a panic is
   logged with its stack (source `panic`), shown in `/errors`, and zemi keeps running.
   Turn goroutines also defer `h.endTurnAfterPanic()` so the agent is not left "active".
   A panic in a plain `go func()` kills the process with the terminal still in raw mode.
7. **The store** (`internal/store`) is SQLite via modernc.org/sqlite (pure Go), WAL,
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

`permissions.mode` (internal/hostapi/permmode.go) only answers asks that survive the rules
and the `permission.request` veto: `yolo` allows, `auto` runs a one-shot screening model
and falls back to the user ask with its reason. Unknown values read as manual. Only user
config sets it; binding a key to the mode cycle is refused for other origins.

### require (internal/lua/runtime.go)

Modules are cached per generation by resolved file. Dotted names search `<root>/lua`
over the runtimepath: user, plugins, and the project's `.zemi` first but only when the
requirer is project code. `./x` and `../x` resolve against the calling function's chunk
name, which is why file chunks are compiled with their absolute path, and must classify
to the caller's own origin. `ProjectConfigDir` walks up to the `.git` root.

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

Skills are file-based units: authored under `<config>/skills/`, builtin (zemi's
self-knowledge: `runtime/skills/zemi-*`, embedded, rewritten to `<data>/builtin-skills/` on
every sync, enabled by default; keep them accurate when behavior changes), learned
(reflector drafts, body only, no Lua hook, scrubbed, start disabled) under
`<data>/skills/`, each a `SKILL.md`
with YAML frontmatter (name, description); discovery lives in `internal/skills`, hosting in
`internal/hostapi/skills.go`, rows in the global DB `skills` table. Invariants:

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

### Turns, sessions and the learning loop (internal/hostapi: agent.go, session.go, compact.go, memory.go, reflector.go)

- **One runner per send, one conversation.** `ensureRunner` rebuilds the runner (model,
  prompt, tools may change); `AgentSend` copies `History` from the previous idle runner.
  `ResumeSession` installs a bare `&agent.Runner{History: ...}` for that copy. `runTurns`
  loops the queue's follow-ups on one runner and compacts first when the last prompt passed
  80% of the window (summary message persisted, `sessions.summary_message_id` points at it).
- **Steering.** Messages sent mid-turn queue; `Runner.Steer` injects them at the next step
  boundary through fantasy's `PrepareStep`, which rebuilds step input from scratch, so
  steered messages are re-inserted every step. Leftovers become one follow-up turn, and
  `active` stays held across that handoff (`nextTurn`) so a submit cannot jump the queue.
- **Store order.** `Messages` orders by `rowid`: a turn persists several rows in one
  millisecond and ids are random. Tool telemetry rows share the table; `historyFrom` skips
  anything that does not decode as a fantasy message.
- **Proposals persist.** Memory proposals live in the scope DB's `proposals` table until
  `/memory` (a/d, decisions by row id) or `zemi.learn.decide`; `refreshPending` caches the
  statusline counts. Config proposals are still in memory (`cfgProposals`).
- **Reflector timing.** It runs on session close (`/new`, `/resume`) and at startup for
  the latest session, never on quit; `reflectDue` compares the message count with kv
  `reflector.seen.<sid>`, so a session is reflected again only after new messages.
- **Provider text shape.** Model responses carry `fantasy.TextContent`, stored messages
  `fantasy.TextPart`; `concatText` handles both. A stub that returns the wrong one hides
  real-provider bugs (this once made the reflector read empty output everywhere).
- **Jail.** `inJail` resolves symlinks on both the path and the roots; `fsDefault`,
  `resolveJailed` and `@file` attachments (`expandAttachments`) all rely on it.

### Subagents, workflows, background tasks, MCP (internal/hostapi: subagent.go, workflow.go, tasks.go, mcp.go; internal/mcp)

- **Subagents** are fresh `agent.Runner`s over `PipelineFor()` (same permission gate; asks
  are prefixed with the agent name). Definitions: builtin `explore` (read-only tools) and
  `general`, plus `agents/*.md` (user over builtin over trusted project). They never get
  `agent`, `workflow`, `todos` or `question` (nesting depth 1). A run is a child session
  (`parent_id`); `ListSessions` hides children. Usage adds to the session tally, not the
  context estimate. The delegation guidance lives in `delegationPrompt` (agent.go):
  without it models rarely delegate.
- **Workflows** are Lua scripts (model-written via the `workflow` tool, saved under
  `workflows/`, builtin in `runtime/workflows/`). They run in their **own** gopher-lua
  state on a host goroutine, never on the interpreter (DESIGN §4): a coroutine per branch,
  `agent()`/`parallel()` yield and the scheduler resumes them as subagents finish.
  Invariants: the script sees only base/string/table/math + the workflow API (no fs,
  net, `zemi.*`); `agent`/`parallel` are exposed through Lua wrappers because gopher-lua
  loses resume values on a tail call (`return agent(...)`) of a yielding Go function;
  yielding inside `pcall` silently kills the coroutine, so both refuse under pcall;
  concurrency 5, 100 agents per run. The tool asks `tool.workflow` with the script as
  preview.
- **Background agents** (`agent` with `background: true`) run detached; the report comes
  back through `AgentSend` with the body behind `attachMarker`, which both the live
  transcript (`startTurn`, `deliverSteering`) and resume cut off.
- **MCP**: `internal/mcp` is a dependency-free JSON-RPC client (stdio, streamable HTTP
  with JSON or SSE replies). Servers come from `zemi.mcp.add` (user origin only) or
  crush.json; they start in the background and stop on `PurgeGeneration`. Tools are
  `mcp_<server>_<tool>`, gated per call as subject `mcp.<server>` with the tool name as
  argument. Test against `internal/mcp/mcptest` (the test binary doubles as the stdio
  server).
- **Never start a turn on the interpreter goroutine.** `systemWith` runs `agent.assemble`
  handlers through `RT.Do`, so `AgentSend` from Lua would deadlock; `zemi.agent.send`
  and `zemi.command.run` of host commands go through `GoSafe`.

### Rewind, plan mode, composer bridge

- **Rewind** (rewind.go, store/checkpoints.go): the pipeline's `Checkpoint` hook saves a
  file's pre-turn content (keyed by the turn's user message, first write wins) before
  `write`/`edit`/`multiedit` touch it. `RewindTo` restores files first and only then
  truncates messages (checkpoints cascade), so a failed restore leaves the conversation
  intact. Bash changes are not tracked.
- **Plan mode** is a permission mode enforced in `tools.Pipeline.check` (`Plan` hook):
  `fs.write` denied even under allow rules, bash and `mcp.*` allows flip to ask. The
  prompt section only saves wasted attempts.
- **Composer bridge**: Lua reads a snapshot the UI publishes (`SetComposerSnapshot` after
  key and paste handling) and edits through `UIHooks.ComposerEdit`, applied on the UI
  goroutine. Completion popups float over the chat (`floatPopup`), they never reserve
  rows.
- **Tool schemas**: fantasy wraps `ToolInfo.Parameters` as the properties map, so tools
  pass `tools.SchemaProperties(schema)` and a non-nil `Required`
  (`TestToolSchemasAreProperties`); strict providers reject anything else.

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
  ("0.7.0") and namespace members. Growing the Lua API means updating that test and the
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
- Agent-turn tests stub the model with `reflectStub` (sets `h.modelFn`; answers Generate and
  Stream with `TextContent` like real providers) and wait with `waitIdle`.
- End to end, drive the real binary in tmux against a fake OpenAI-compatible server: point
  `ZEMI_*` at temp dirs, register it in `init.lua` (`kind = "openai_compat"`, `base_url`),
  set `providers.crush_json = ""`, then `tmux send-keys` / `tmux capture-pane -p`.
  Python terminal emulators like pyte garble bubbletea's incremental renderer; tmux does not.
- The sandbox, freeze, and origin-classification behavior in `internal/lua/sandbox_test.go`
  is adversarial by design (monkey-patching attempts must raise); don't weaken those
  assertions to make unrelated changes pass.

## Gotchas and non-obvious conventions

- **DESIGN.md §12 does not match reality** (it predates the code): the actual layout is
  `internal/tools` (not `internal/agent/tools`) and `internal/perm` (not
  `internal/permission`); `internal/skills` exists, `internal/agent/templates` does not.
  Trust the tree, not §12; §13 milestones explain what exists (M0-M3 are in; from M5,
  subagents, workflows and the MCP client are in; M4 evals, LSP and the plugin manager are
  not).
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
  (`internal/ui/palette.go`). Keymaps are single keys (`zemi.keymap.set(key, action)`,
  `del`, `get`, `list`); printable keys stay with the composer, and an open palette or
  completion popup keeps its keys before keymaps are consulted.
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
