# AGENTS.md — zemi

zemi is a terminal AI agent harness whose userland (default UI, keymaps, widgets, learning
machinery) is written in embedded Lua while Go owns the runtime: the agent loop over
`charm.land/fantasy`, the bubbletea v2 TUI, the sandboxed gopher-lua interpreter, and SQLite
persistence. `DESIGN.md` is the authoritative spec; code comments cite its
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

A Makefile wraps the plain Go toolchain (module `github.com/metafates/zemi`, Go 1.27).
No CI, no lint config; `make check` is what a change has to pass:

```
make check              # go vet ./... + go test ./... + a gofmt check, all must be clean
make build / make run   # go build -o bin/zemi . / run the TUI against your real ~/.config/zemi
make test               # full suite (~20s, all pure Go, no network, CGO not needed)
make race               # go test -race ./internal/...
make golden             # regenerate UI golden files (internal/ui/testdata/*.golden), then read the diff
make fmt                # gofmt -w over the tracked Go files
```

For testing against a throwaway config tree, redirect all dirs: `ZEMI_CONFIG_HOME`,
`ZEMI_DATA_HOME`, `ZEMI_STATE_HOME` (see `internal/config/dirs.go`). Most tests don't need
this — they inject `config.Dirs{...}` with `t.TempDir()` directly.

The project-config trust prompt and `~/.config/zemi/init.lua` only exist on a real machine;
tests boot with empty dirs, so code that requires them needs explicit setup in the test.

## CLI (cli.go)

`zemi` opens the TUI; a prompt argument is sent as the first message. Flags: `-p`/`--print`
(answer on stdout and exit, with `--output text|json`), `-c` (continue the latest session),
`-r <id>`, `-m <provider/model>`, `--mode manual|plan|auto|yolo`, `--health`, `-v`. Piped
stdin is appended to the prompt, and is a usage error without `-p` (the TUI needs the
terminal). Flags apply after boot so they win over `init.lua`. `--health` and `-p` boot the
same step list and skip only the TUI: `--health` prints `HealthRows()` and exits 1 if a check
errored; `-p` runs one turn, then reports on stderr how many tool calls the gate denied
(nobody can answer an ask headless, so it points at `--mode auto`) and warns when an
untrusted project config was skipped. `/health` and `--health` read the same rows: the Go
checks (store, model, permissions, git, ripgrep, reflector, learning, skills, lua, errors,
lsp) plus whatever Lua registered with `zemi.health.register`.

A first run writes a fully commented starter `init.lua` (`config.Dirs.Ensure`) because zemi
ships no providers and an empty config cannot answer a prompt. The logger keeps the newest 20
files, one per launch. Config reload is not fsnotify: `watchConfig` walks the config trees
once a second and reloads when the fingerprint (path, size, mtime) moved and no turn is
active.

## Architecture and control flow

`main.go` is wiring only. Boot order (each step is a `lua.BootStep`, see `steps` in `main.go`) — order
matters and permissions sit early so no userland code runs before the permanent store loads:
host namespaces (`zemi.*`) -> `PermissionsLoadStep` (reads `permissions.lua`, fail-closed) ->
builtin commands -> memory, reflector and skills commands -> agents (+ tasks),
workflow (+ `/review`), mcp, lsp and window commands ->
`require("zemi.defaultui")` (shipped theme/widgets/keymaps in `runtime/lua/zemi/`) ->
plugins -> user `init.lua` -> authored skill hooks -> project `.zemi/` (`init.lua`,
`agents/`, `commands/`, `workflows/`; any of them triggers the one-time trust prompt that
hashes the config tree and refuses symlinks, and trust sets `Host.SetProjectTrusted`) ->
markdown commands (`commands/*.md`, user then trusted project) ->
`app.ready` -> learned skill hooks (skills load last, lowest trust). Boot runs on
its own goroutine and may block on the trust prompt; results reach the UI via a hook channel.
`EnsureConfigRepo()` runs in `main` before the step list is built, not after it, so the
config git repo exists before any step can commit a revision (see "Permanent rules" below).

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
   internal/ui — it defines `UIHooks` (internal/hostapi/host.go) and the App implements
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

File tools (internal/tools/fs_tools.go): `write`, `edit` and `multiedit` refuse a file
the model has not read, or one whose mtime moved since (`Pipeline.Seen`, a per-session
map wired in `PipelineFor`; subagents get their own), and hold a per-path lock, so a
concurrent agent or the user's editor cannot lose changes. Matching runs on LF text and
a CRLF file is written back as CRLF; the whitespace-tolerant fallback re-indents
`new_string` to the file's own indentation and says so. `glob` and `grep` shell out to
`rg` when installed (gitignore, `**`, `{a,b}`) and fall back to a Go walker with the same
glob rules; grep never reads secret-shaped files. Long bash and fetch output is saved
whole to the session scratchpad and the result names the file. A bash command still
running at its timeout is handed to `Jobs.Adopt` instead of being killed, so a slow build
keeps going under a job id (which is why the command is not bound to the turn's context;
an interrupt still kills it). `read` carries the nearest instruction file not already in
the prompt (`nearestInstructions`), once per session, so a monorepo package's own
AGENTS.md reaches the model.

### Permission engine (internal/perm)

Rules `(subject, pattern, decision, not_match?, principal?)`, subjects like `tool.bash`,
`fs.read`, `fs.write`, `net.<host>`, `lua.<ns.call>`. Ordering: deny > ask > allow;
within one decision, fewest wildcards then longest literal prefix wins. A `not_match` hit
falls through and **forces ask**. The default when nothing matches is set by
`Store.Default` (host wires `fsDefault`: jail reads allow except secret-shaped paths,
writes ask) and finally ask — there is no fail-open path. New host-API side effects must
be gated with the same `lua.` subject at the Lua boundary.

`permissions.mode` (internal/hostapi/permmode.go) only answers asks that survive the rules
and the `permission.request` veto: `yolo` allows; `auto` first settles locally (reads and
edits **inside the jail** that are not secret-shaped, `perm.SafeCommand` commands) and only
then runs a one-shot screening model, caching approvals and falling back to the user ask
with its reason; `plan` denies writes and runs `SafeCommand` commands. Network requests are
never settled locally: the URL is the model's. `h.secretPath` matches the secret shapes on
the resolved path too, so a symlink cannot hide one. `SafeCommand` is an
allowlist (internal/perm/safe.go, table-tested): no shell expansion outside single quotes,
no write redirects, and every argument must pass the host's path check (inside the
project, not secret-shaped), so a read-only program cannot leak `~/.ssh` to the model.
User config adds modes with `zemi.permission.mode.add`; unknown values read as manual.
Only user config sets it; binding a key to the mode cycle is refused for other origins.

The overlay's "always" answer writes session allow rules scoped to the `model` principal, and
they are prefix globs rather than the exact request: `alwaysChoice` (agent.go) gives bash
each normalized segment's two-word command prefix plus `prefix *`, `fs.read`/`fs.write` the
file's directory with `/*`, and `net.*` both `scheme://host` and `scheme://host/*`. A
gate-forced ask (heredoc, env assignment) offers no always option at all.

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
last-writer-wins. Project origin additionally cannot register providers,
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
  would defeat a Go-side gate, see the concurrency rules). Enabling a learned skill runs its
  `zemi-skill.lua` (`runSkillHook`): inline when `zemi.skills.enable` calls it (already on
  the interpreter goroutine), otherwise enqueued via `h.RT.Do` (`/skills enable`) — the
  inline-vs-Do split in `enableHook` is load-bearing.
- **Token efficiency**: the agent prompt carries only the index (name + description, at most
  512 runes); full bodies load on demand via the `skill` tool after a permission check.
- **Hook registration placement**: authored skill hooks register at `SkillsAuthoredHooksStep`
  (after user init.lua), learned ones at `SkillsLearnedHooksStep` (after `app.ready`) — the
  learned step must not re-run on reload replay without its `Gen` tag.

### Type annotations (internal/hostapi/types.go, runtime/types/zemi.lua)

`runtime/types/zemi.lua` is the LuaLS (`---@meta`) description of the whole `zemi.*` API;
`syncTypes` (called from `installNamespaces`, next to `syncSkills`) rewrites it plus a copy
of `runtime/lua/` to `<data>/types/` on every start, and writes `<config>/.luarc.json` only
when there is none. It is never loaded by the interpreter — a description, not code. Three
tests keep it honest: `TestTypesCoverAPI` (every member `Sandbox.NamespaceMembers` reports
is annotated), `TestTypesDeclareNothingExtra` (no annotation for a member zemi dropped) and
`TestTypesParse`. Growing the Lua API means updating it, `TestNamespaceVersion` and the
version string together.

### UI screenshots

`compose` publishes each finished frame under `App.frameMu`; `UIHooks.Screenshot` strips the
styling and the trailing padding and hands it back, so `zemi.ui.screenshot()` works from the
interpreter goroutine without either side waiting on the other. It is how the agent debugs
its own widgets, themes and layout (see the zemi-debug skill) instead of asking the user
what the screen says.

### Turns, sessions and the learning loop (internal/hostapi: agent.go, session.go, compact.go, memory.go, reflector.go)

- **One runner per send, one conversation.** `ensureRunner` rebuilds the runner (model,
  prompt, tools may change); `AgentSend` copies `History` from the previous idle runner.
  `ResumeSession` installs a bare `&agent.Runner{History: ...}` for that copy. `runTurns`
  loops the queue's follow-ups on one runner through `sendCompacting` (compact.go): compaction
  runs before a send, when `Runner.Full` cuts a turn at a step boundary (then `Send(ctx, "")`
  continues), and once after a context-too-long refusal. The size is `agent.ContextUsed` of the
  last step, never a sum; an unknown window means no auto compaction. The summary row is
  followed by copies of the verbatim tail stored with row role `tail`, which resume reads and
  every transcript-like reader skips.
- **A turn's work survives the turn.** `Runner.Persist` writes each step's messages from
  `OnStepFinish`, so a cancelled or crashed turn keeps what it already did; the next send
  runs `closeToolCalls` over the history first, inserting an error result after any tool call
  left unanswered (providers refuse a dangling call). The stops are `maxSteps` 200
  (`ErrStepLimit`) and a block of up to three tool steps repeating over six steps or three
  cycles (`ErrToolLoop`; `job_read` and `job_list` steps are left out, polling is not
  looping), with `maxRetries` 5 per step and `MaxOutputTokens` turning a length-finished reply into `ErrReplyCut`. Cache tokens are
  priced apart (`CostPer1MInCached`/`CostPer1MOutCached`), and the session tally counts cache
  reads and creations as input.
- **Steering.** Messages sent mid-turn queue; `Runner.Steer` injects them at the next step
  boundary through fantasy's `PrepareStep`, which rebuilds step input from scratch, so
  steered messages are re-inserted every step. Leftovers become one follow-up turn, and
  `active` stays held across that handoff (`nextTurn`) so a submit cannot jump the queue.
- **Store order.** `Messages` orders by `rowid`: a turn persists several rows in one
  millisecond and ids are random. Tool telemetry rows share the table; `historyFrom` skips
  anything that does not decode as a fantasy message.
- **Proposals persist.** Memory proposals live in the scope DB's `proposals` table until
  `/memory` (a/d, decisions by row id) or `zemi.learn.decide`; `refreshPending` caches the
  statusline counts.
- **Reflector timing.** It runs on session close (`/new`, `/resume`) and at startup for
  the latest session, never on quit; `reflectDue` compares the message count with kv
  `reflector.seen.<sid>`, so a session is reflected again only after new messages.
- **Provider text shape.** Model responses carry `fantasy.TextContent`, stored messages
  `fantasy.TextPart`; `concatText` handles both. A stub that returns the wrong one hides
  real-provider bugs (this once made the reflector read empty output everywhere).
- **Jail.** `inJail` resolves symlinks on both the path and the roots; `fsDefault`,
  `resolveJailed` and `@file` attachments (`expandAttachments`) all rely on it.

### Windows (internal/win, internal/hostapi/win.go, internal/ui/win.go)

A window is a program on a pseudo-terminal (creack/pty, no cgo) feeding a pure Go
emulator (charmbracelet/x/vt). `win.Manager` on the host owns them; they outlive turns and
reloads (only Lua `on_exit` callbacks are generation-tagged). Each window carries a
`Layout` (kind, hidden, frac, cols/rows, x/y) that the UI reads per frame: vsplits are
full-height columns beside the main column (compose lays the main column out at `mainW`
and joins the columns at the end), splits take rows at the bottom of the chat area,
floats go over everything at their position or centered; `shares` divides an axis so the
main area keeps its minimum. `Screen()` turns cells into `sgr:` segments so colors reach
the renderer untouched. A focused window (`App.focusWin`) takes every key, ctrl+c
included, until `ui.window.unfocus`; a press on a title or separator starts a `winDrag`
(move a float, resize a vsplit or split), a release without motion focuses or closes on
×, mouse events inside are forwarded (wheel becomes arrows for programs that never asked
for the mouse), and a paste goes to the program with bracketing when it asked. The model's
`window` tool gates `open` as `tool.bash` (same rules and modes as bash) and `keys`/`mouse`/`send`
as `tool.window`; an exit lands in the next message through `shellNotes`. Subagents get
`windowToolFor(name, pipe)`: their asks carry the agent name and their windows open hidden.

### Subagents, workflows, background tasks, MCP (internal/hostapi: subagent.go, workflow.go, tasks.go, mcp.go; internal/mcp)

- **Subagents** are fresh `agent.Runner`s over `PipelineFor()` (same permission gate; asks
  are prefixed with the agent name). Definitions: builtin `explore` (read-only tools), `web`
  (web_search + fetch, on `agent.small_model`) and `general`, plus `agents/*.md` (user over builtin over trusted project). They never get
  `agent`, `workflow`, `todos`, `question`, `plan`, `open_in_editor` or `Lua` (nesting depth 1). A run is a child session
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
- **Agent runs** (tasks.go): every `runSubagent` (tool, background, workflow, Lua) registers
  an `agentRun` whose hooks build a live transcript. `/agents view <id>` (or a click on an
  `Agent` row) sets `Host.view`, and `widgetCtxTable` renders that transcript instead of the
  session's (live stream and todos hidden); esc or a submit clears it, and so does
  `setTranscript`. Runs the process no longer holds load from their child session.
- **Scratchpad** (scratch.go): `<state>/projects/<hash>/scratch/<session id>`, created when
  the pinned environment prompt is built, pads idle a week removed. `fsDefault` allows
  `fs.write` there, `Pipeline.Scratch` lets it pass plan mode, and read jails and
  `safeCommandPath` include it.
- **Lua tool** (luatool.go, tool name `Lua`): model Lua runs through `Runtime.Eval` as `user`
  origin and the tool never asks (only `tool.pre` hooks can deny it; gated `zemi.fs`,
  `zemi.http` and `zemi.job` calls inside still raise on an ask), on the interpreter (10s
  ceiling), in a wrapper env whose `print` is captured. Main agent only.
- **Model memory edits**: `memory_update`/`memory_forget` go through `modelEditMemory`,
  which re-runs intake and asks (`memory.write`) unless autonomy is auto, the memory is
  learned and the text is not instruction-shaped. Ids may be 8-char prefixes.
- **MCP**: `internal/mcp` is a dependency-free JSON-RPC client (stdio, streamable HTTP
  with JSON or SSE replies). Servers come from `zemi.mcp.add` (user origin only); they start in the background and stop on `PurgeGeneration`. Tools are
  `mcp_<server>_<tool>`, gated per call as subject `mcp.<server>` with the tool name as
  argument. Test against `internal/mcp/mcptest` (the test binary doubles as the stdio
  server).
- **LSP** (lsp.go, internal/lsp): servers start lazily on the first matching file (user
  `zemi.lsp.add` first, then an installed builtin whose marker is in the file's directory
  or above it); the pipeline's `AfterWrite` appends errors and warnings to
  write/edit/multiedit results plus errors in other files, the `diagnostics` tool reads
  any file and the `lsp` tool does definition, references, rename and symbols by name.
  A server answers from the text the client sent for an open document and from its own
  copy of every other file, which it re-reads only when told: so before every touch or
  query `lspSync` diffs two `git status` listings and sends `workspace/didChangeWatchedFiles`
  for what changed (bash, formatters, git, the user's editor), the client re-sends open
  documents whose mtime moved and closes deleted ones, and a rename refuses a versioned
  edit planned against other text. A slow or failed server only means no diagnostics,
  never a failed edit; three silent touches in a row restart it. Test against
  `internal/lsp/lsptest`, whose fake registers a `**/*.txt` watcher like gopls does.
- **Never start a turn on the interpreter goroutine.** `systemWith` runs `agent.assemble`
  handlers through `RT.Do`, so `AgentSend` from Lua would deadlock; `zemi.agent.send`
  and `zemi.command.run` of host commands go through `GoSafe`.

### Rewind, plan mode, composer bridge

- **Rewind** (rewind.go, store/checkpoints.go): the pipeline's `Checkpoint` hook saves a
  file's pre-turn content (keyed by the turn's user message, first write wins) before
  `write`/`edit`/`multiedit` touch it. `RewindTo` restores files first and only then
  truncates messages (checkpoints cascade), so a failed restore leaves the conversation
  intact. Bash changes are not tracked.
- **Plan mode** is a permission mode enforced in `tools.Pipeline.Check` (`Plan` hook):
  `fs.write` denied even under allow rules, bash and `mcp.*` allows flip to ask. The
  prompt section only saves wasted attempts.
- **Composer bridge**: Lua reads a snapshot the UI publishes (`SetComposerSnapshot` after
  key and paste handling) and edits through `UIHooks.ComposerEdit`, applied on the UI
  goroutine. Completion popups float over the chat (`floatPopup`), they never reserve
  rows.
- **System prompt** (agent.go): `basePrompt` (task, care, tools, commands that do not exit,
  git, replies, security) + `delegationPrompt` for the default profile, then `zemiPrompt`
  for every profile, plan mode, the per-session pinned `environmentPrompt`, instruction
  files under `# Project instructions`, Lua sections, skills, digest. Anything that varies
  per turn must be pinned per session or it busts the provider's prompt cache.
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

Skill proposals always wait for review (DESIGN §10.4). The reflector
proposes memories and skills only; there is no config-patch pipeline and
no `/config` command (the agent edits config files directly). The overlay's
"always" answer adds session rules only; permanent rules come only from
`zemi.permission.rule{ scope = "permanent" }` in user config.
Every applied revision records its config-repo commit SHA
(`config_revisions.commit_sha`); undoing one is `git revert` in the
config directory. `sweepPendingRevisions` runs in `PermissionsLoadStep`.

## Testing patterns

- `internal/hostapi/host_test.go` has the canonical helpers: `newTestHost(t)` boots a
  fresh runtime + namespaces step with temp dirs; `runUser(t, r, luaSrc)` executes a chunk
  as user origin **with a 3s guard so an interpreter deadlock fails loudly instead of
  hanging the binary** — reuse it for any test that runs Lua.
- API surface is pinned: `TestNamespaceVersion` asserts the exact `zemi.api.version`
  ("0.19.0") and namespace members. Growing the Lua API means updating that test and the
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
  `ZEMI_*` at temp dirs, register it in `init.lua` (`kind = "openai_compat"`, `base_url`), then `tmux send-keys` / `tmux capture-pane -p`.
  Python terminal emulators like pyte garble bubbletea's incremental renderer; tmux does not.
- The sandbox, freeze, and origin-classification behavior in `internal/lua/sandbox_test.go`
  is adversarial by design (monkey-patching attempts must raise); don't weaken those
  assertions to make unrelated changes pass.

## Gotchas and non-obvious conventions

- **DESIGN.md is the spec; the tree is the truth.** §12's layout and §13's milestone status
  were last squared with the code, so they read correctly today; when they drift again,
  trust the tree. Shipped: M0-M3, plus subagents, workflows, MCP and LSP out of M5, and a
  pile of things no milestone named (permission modes, rewind, scratchpads, the `Lua` tool,
  print mode). Not built: M4 evals (no `zemi.eval`, and `config_revisions.eval_ok` is a dead
  column), the plugin manager, embeddings.
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
- **Memory intake policy** (`internal/learn`): text cap 500, fixed kind enum
  (fact/gotcha/preference/convention/decision), provenance required, `origin` is `user` or
  `learned` and gates autonomy/supersede/digest-labeling. The autonomy knob is host-owned:
  only Go may flip it (`Settings.SetHost`), never user config.
- **Config store** (`internal/config/store.go`): dotted-path keys; `SetHost` is Go-only,
  user config cannot set host-owned knobs. Providers ship none; user config registers them, keys
  included (`api_key = zemi.env.get("NAME")`; `zemi.env.get` reads secret-shaped names for
  user and builtin origin only). Config fields hold values, never names of env vars.
- **The Go-reserved triggers** (`ctrl+c` cancel, `/reload`) are never routed through Lua
  keymaps and cannot be rebound — `/reload` is matched on composer submit (exact,
  case-sensitive, before the command registry is consulted), so no registered command can
  shadow it. Keep them out of any "make everything rebindable" refactor.
- **No editor modes.** The composer is always focused; commands are slash commands typed in
  the composer (`/quit`, `/help`, `/memory`, ...) with the palette popup for completion
  (`internal/ui/palette.go`). Keymaps are keys or key sequences
  (`zemi.keymap.set("<leader>m", action)`, `del`, `get`, `list`; `<leader>` is the
  `keymap.leader` setting); printable keys stay with the composer, and an open palette or
  completion popup keeps its keys before keymaps are consulted.
- **Mouse capture is load-bearing.** `App.View` sets `MouseModeAllMotion` (unless
  `ui.mouse = false`) so the wheel scrolls the transcript (the altscreen buffer has no
  native scrollback) and plain pointer motion drives hover. `App.target` is the one hit
  test for clicks and hover (a highlight always means a click acts); motion redraws only
  when the target changes, and OSC 22 turns the pointer into a hand where supported. Drag selection and clicks are zemi's own (`internal/ui/mouse.go`):
  compose keeps the last frame's segments, a drag copies their text through OSC 52 plus
  pbcopy/wl-copy/xclip, and a click reads `Segment.Hit` (a Go-only 1-based item index
  that list rows, prompt buttons and memory rows set) to act on the topmost chrome.
  Helpers that rebuild segments must carry `Hit`. Composer
  editing keys (`ctrl+u`, `ctrl+k`, `ctrl+w`, `home`/`end`, ...) stay with the composer —
  scroll uses `pgup`/`pgdown`/`ctrl+home`/`ctrl+end`, bound in `defaultui.lua`.
- **Every child process goes through `internal/proc`** (bash tool, `!command`, jobs, Lua
  jobs, MCP and LSP servers, git). A child runs in its own session: no controlling terminal
  (nothing can read the TUI's keys or hang on a password prompt) and its own process group,
  which cancel, timeout, `proc.Wait` and `proc.KillAll` on exit kill whole. `WaitDelay`
  keeps an orphan holding stdout from blocking `Wait`; `proc.Buffer` caps output. A plain
  `exec.Command` here is a bug: a cancelled `sh -c` pipeline once hung the turn forever.
- **The key loop never waits on Lua.** Keymap right-hand sides and Lua commands run in
  order on one worker goroutine (`App.background`); Go commands stay inline. A turn's
  `turnScope` context (host.go) covers compaction, the model, tools and auto-mode
  screening, so esc stops all of them; a second ctrl+c while "Cancelling" quits.
- **Drag and drop is a paste.** Terminals paste a dropped file's path (escaped, quoted or
  `file://`); `ui/drop.go` turns a paste made only of existing paths into tokens via
  `Host.AttachPath`. Files from outside the project are copied to `<data>/drops` and
  referenced as `@drop:<name>`; `expandAttachments` resolves `drop:` only inside that
  directory, so a forged reference cannot attach other files outside the project.
- **gopher-lua multiple assignment**: `a[#a + 1], b = b, x` stores `x` in `a`. Split any
  assignment whose right side names a left-side target (this once garbled wrapped text).
- **Handles vs snapshots**: host resources (job, http, timer, session, window) are handle tables
  with colon methods; everything else is a plain snapshot copy. Marshaling across the Lua
  <-> Go boundary caps string size (16 MB) and depth (64) and refuses cycles.
- `.crush/` in the repo root is this workspace's Crush CLI session data (gitignored) — not
  part of the project.
- No vendoring; the module has no generated code. Commits are small and imperative
  (`git log` shows the style: lowercase, no prefix convention).
