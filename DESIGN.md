# zemi: a self-improving, Lua-extensible AI agent harness

Status: draft v1
Module: `github.com/metafates/zemi`

zemi is a terminal AI agent harness (coding agent first, general assistant by
configuration) whose entire userland, including its default UI and its own
learning machinery, is written in embedded Lua. Go owns the runtime (agent
loop, providers, I/O, rendering); Lua owns behavior. The user configures,
extends, and observes the agent the same way a neovim user configures an
editor: from `init.lua` against a stable host API.

The learning system runs through the same extensibility surface: the agent
improves by writing the same Lua that a user would write, through the same
reviewed, versioned pipeline.

Prior art studied: `charmbracelet/crush` (local checkout), `charm.land/fantasy`,
neovim's Lua architecture, pi, opencode, codex, hermes-agent.

## 1. Design principles

1. **Core is small, behavior is Lua.** Go code ships primitives; policies,
   keymaps, widgets, tools, prompts, and the learning loop live in Lua.
   Anything users can do, the built-in config can do, because they use the
   same API; the self-improvement system uses the same API too, minus the
   capabilities its origin denylist withholds (§5, §10).
2. **Every side effect is permissioned.** Filesystem, network, subprocess, and
   memory writes all flow through one gate, used by both model-invoked tools
   and Lua code. Learned rules may tighten permissions automatically but never
   loosen them without explicit approval.
3. **Configuration is code, versioned as code.** `~/.config/zemi` is a git
   repo. Structural self-modifications are commits, reviewed as diffs,
   revertible as diffs.
4. **Learning is typed and reviewable.** The agent stores facts, procedures,
   and structural changes as distinct, inspectable objects with provenance.
   No opaque "the model got better" hand-waving.
5. **Boring concurrency model.** One Lua interpreter thread (an actor),
   bubbletea on the main goroutine, agent streams and I/O on workers. All
   Lua access funnels through one queue.

## 2. What each go.mod dependency is for

| Dependency | Role |
|---|---|
| `charm.land/fantasy` | Agent core: multi-provider LLM abstraction, agent loop, tool protocol, streaming callbacks, retries, stop conditions, provider-defined tools (web search) |
| `charm.land/bubbletea/v2` | TUI program, event loop, key/mouse input |
| `charmbracelet/ultraviolet` (+ `x/ansi`, `x/term`, `x/termios`, `colorprofile`, runewidth, uniseg, terminfo, displaywidth, uax29) | Cell-based rendering primitives under bubbletea v2; used for widget buffers and golden tests |
| `github.com/yuin/gopher-lua` | The embedded Lua 5.1 VM: interpreter thread, sandbox, `require`, coroutines |
| `github.com/goccy/go-yaml` | SKILL.md and `agents/*.md` frontmatter parsing |

Added since: `modernc.org/sqlite` (pure-Go SQLite, keeps
`CGO_ENABLED=0`; the schema is one const of `CREATE TABLE IF NOT
EXISTS`, so no migration library came with it), `charm.land/glamour/v2`
(markdown in chat), `charmbracelet/x/exp/golden` (golden-file assertions
for TUI output), `aymanbagabas/go-udiff` (edit previews), `creack/pty` and
`charmbracelet/x/vt` (windows: a program on a pseudo-terminal drawn through
a Go terminal emulator). Tool input validation is a small hand-written
schema check, not a JSON Schema library. Not taken:
`charm.land/bubbles` (the composer is zemi's own editor, §6.6),
`charm.land/catwalk` (there is no model catalog; models come only from
`zemi.provider.register` in user config, §6.3), and `fsnotify` (config
reload is a one-second fingerprint walk of the config trees). Ripgrep is
not a dependency either: `glob` and `grep` exec whatever
`rg` `exec.LookPath` found at init, passing the pattern as `--regexp`
and globs as `--glob` so neither can be read as a flag, and fall back to
a Go walker with the same glob rules when `rg` is missing (Go-side and
cancellable either way).

## 3. High-level architecture

```
                        +------------------------------------------+
                        |                zemi binary               |
   ~/.config/zemi       |                                          |
   init.lua ----------->|  +-----------+     +------------------+   |
   lua/**  (user code)  |  | Lua host  |<--->| host API (zemi.*)|   |
                        |  | (1 thread)|     | internal/hostapi |   |
   plugins ------------>|  +-----------+     +--------+---------+   |
   skills (*.lua)       |        ^  |                |             |
                        |        |  v                v             |
                        |  +-----------+     +------------------+   |
                        |  | bubbletea |     | agent core       |   |
                        |  | ui core   |     | internal/agent   |   |
                        |  +-----------+     | fantasy + tools  |   |
                        |        ^           +--------+---------+   |
                        |        |                    |             |
                        |  +-----------+     +------------------+   |
                        |  | learn core|<--->| store (SQLite)   |   |
                        |  | reflector |     | internal/store   |   |
                        |  +-----------+     +------------------+   |
                        +------------------------------------------+
```

Layers and ownership:

- **Lua host** (`internal/lua`): one `lua.LState` on a dedicated goroutine,
  sandboxed stdlib, preload of all `zemi.*` namespaces, `require` resolution
  over the runtimepath.
- **Host API** (`internal/hostapi`): the `zemi.*` namespaces and the host
  services behind them: turns, sessions, prompt assembly, compaction,
  subagents, workflows, the reflector, skills, permissions (§12).
- **Agent core** (`internal/agent`): the fantasy runner (one turn, steps,
  stops, retries); built-in tools and the tool pipeline (hooks,
  permissions, execution) live in `internal/tools`.
- **UI core** (`internal/ui`): bubbletea program, region layout, widget
  registry, theme/highlight resolution, slash palette, overlay stack.
- **Learn** (`internal/learn`): the memory intake policy only (caps, the
  kind enum, provenance, the secret scrubber). The reflector and the skill
  manager live in `internal/hostapi`.
- **Store** (`internal/store`): SQLite, one schema const, no migration
  tool (sessions, messages, memory, skills, checkpoints, proposals,
  config revisions, telemetry).

## 4. Concurrency model

Three kinds of goroutines, one rule: **only the Lua host goroutine touches
the `LState`.**

1. **Main goroutine**: bubbletea program. `Update` normalizes input, applies
   Go-side state, enqueues Lua work, schedules `View`.
2. **Lua host goroutine (actor)**: owns the `LState`. A separate
   dispatcher goroutine owns the queue, the priority classes, and the hot
   flag, so callers enqueue (or get the fail-fast "hot" answer) without
   ever touching a wedged interpreter. Requests: UI callbacks (keymap,
   widget render, events), tool executions, config loading. Each request
   is `(fn, args, reply chan, ctx)`. Callers
   block on `reply`; `ctx` cancellation preempts the Lua request itself:
   the host installs it with `LState.SetContext(ctx)`, gopher-lua checks
   `ctx.Done()` before every opcode and raises a catchable Lua error
   ("context canceled"). The error is catchable once: because the context
   stays canceled, it re-raises at the next opcode, so a `pcall` catch
   block cannot run further Lua statements. Reliable cleanup is Go-side
   (defers inside host functions); the queue treats a reply arriving after
   cancellation as canceled, whatever the Lua returned.
3. **Worker goroutines**: agent stream (fantasy), jobs (subprocesses), HTTP,
   timers, store writes. They never call Lua directly; they post results to
   the queue, delivered as bubbletea messages and Lua events.

Contracts that make this safe:

- **UI-critical handlers must be fast.** Keymaps, widget renders, and event
  handlers have a soft budget (~10 ms). Long work goes through
  `zemi.job` / `zemi.http` / `zemi.timer`, which are Go-backed and async.
- **Queue priority.** Requests carry a class: `ui` (keymaps, renders,
  events), `agent` (tool calls), `io` (timer/job/http callbacks, reloads).
  The queue drains `ui` first, then `agent`, then `io`, FIFO within a
  class. Priority orders queued work only; a running request is preempted
  only by cancellation (above), which is why handlers must be fast.
- **Watchdog.** Timers arm at execution start, not at enqueue: queued
  time is unbounded by the watchdog (a queued callback behind a long tool
  waits, it is not canceled pre-execution). A running Lua tool uses the
  tool's `timeout_ms`; ui/io handlers get the ~10 ms soft budget plus a
  hard ceiling (1 s). On expiry the request's context is cancelled (the
  catchable error surfaces as a failed handler) and the async handles it
  started (jobs, HTTP) are cancelled inside Go, so a blocked Go call
  actually terminates. If the interpreter is still busy with the same
  request after a grace period (10 s), the request is abandoned hard: the
  reply is closed with an error and the runtime is marked "hot" (until
  force-reload, every Lua request fails fast with a "reload required"
  error; incoming requests are answered immediately instead of queueing;
  introspection is the one exception, and Go serves it from its own
  registries, never from the wedged interpreter). gopher-lua cannot kill a goroutine: a handler that
  swallows cancellation keeps its interpreter goroutine spinning. The
  runtime drops the abandoned `LState`, logs it, and force-reload builds a
  fresh one so service recovers; the spin lasts until process exit and is
  the accepted cost of no-kill cancellation. Reload purges every
  registration of the dead generation (keymaps, widgets, event handlers,
  overrides, timers, jobs) before replay, so no callback can fire into a
  dropped `LState`. Lua-added permission rules are the exception: they stay
  in force until the replay succeeds (then those it did not add again are
  dropped), and `tool.pre` and `permission.request` vetoes wait for a
  running reload, so no tool call is checked against half a config.
- **Vetoable events never block.** `permission.request`, `app.quit`, and
  `composer.submit` handlers must answer without waiting on async work. At
  the 50 ms deadline the host cancels the handler's context (the
  catchable error fails it) and takes the event's default decision,
  logged: `permission.request` defaults to deny for asks without an
  overlay leg (fail-closed, §9),
  `app.quit` and `composer.submit` default to proceed (a wedged plugin
  must not hold the user's quit or a typed message hostage; a non-user
  `app.quit` denial is honored once per session, further ones are logged
  and ignored; project and learned handlers cannot deny it at all); the other
  vetoables follow the same deadline, defaulting to proceed for
  `tool.pre` (absence of denial is the pass) and to shipped-sections-only
  for `agent.assemble`. While the runtime is hot, registered
  `permission.request` and `tool.pre` handlers cannot run, so both deny
  until `/reload` instead of passing unchecked. The 50 ms leg governs Lua handlers only, arms at
  handler start, and its meaning depends on the ask's shape. A model
  tool call that needs an ask has a second leg: the Go-owned overlay (§8)
  blocks the agent worker goroutine (never the interpreter) until the
  user answers, with no deadline; the legs are concurrent (the
  interpreter is free while the overlay waits). On the model path an
  explicit veto deny within the deadline is honored and wins over an
  already-given overlay answer (deny beats allow regardless of order, §9;
  the overlay stays open until both legs resolve, and a late deny is
  logged with the decision), but a veto timeout
  (or no `permission.request` handler registered at all) is
  no-objection: the overlay alone decides, so the fail-closed default
  cannot overrule an ask the user is mid-way through answering. The
  fail-closed default deny is for asks that have no overlay leg:
  host-API calls from Lua (which raise `permission ask required`, §6.5)
  and any non-model ask source.
- **Dispatch and staleness.** Go services call into Lua without holding
  the UI hostage: UI-class requests enqueue and return; results come back
  as tea messages and Lua events. While a Lua tool occupies the
  interpreter, queued callbacks and renders wait and the screen keeps
  showing its cached lines until the queue drains; the ~10 ms handler
  budget bounds each handler, it does not buy frame latency.
- **Lua tools run on the interpreter thread.** An LLM tool implemented in Lua
  blocks the queue only for its own duration; documented, configurable
  `timeout`, and tools are required to do I/O via async APIs. A stuck handler
  fails the tool call with an error and marks the runtime "hot";
  force-reload is the Go-reserved `/reload` command plus the Go-reserved
  cancel trigger, never routed through Lua (while hot, every Lua keymap is
  dead; the reserved cancel and reload triggers are not, they dispatch
  before any Lua-visible keymap and are fixed in v1). Reloading builds a
  fresh `LState`:
  new requests run normally, requests that arrived while hot got the
  "reloading" error immediately, stale timer/job callbacks are dropped and
  logged, and replay is safe because all registrations are declarative.
- **Widget render is pure.** `render(ctx)` must not call mutating APIs; it
  runs against a capability-masked namespace where mutators raise (caught,
  logged, widget keeps its last-good lines). Go caches rendered lines keyed
  by `(widget id, width, generation)`; the generation is a global frame
  counter bumped at most once per renderer tick, so in v1 "invalidated"
  means any generation change: each visible widget renders once per frame
  and the cache only dedupes repeat `View` calls within a frame (per-widget
  dependency invalidation is a later optimization, §8).
- **Deltas are not batched.** `agent.text_delta` and
  `agent.reasoning_delta` are emitted once per streamed chunk, each its own
  async request, and carry the incremental chunk only, so a subscriber
  that needs complete text reads the host-held transcript instead of
  accumulating deltas. The TUI coalesces its side only: a stream event just
  invalidates the frame, so at most one waits in its hook queue.
- **Steering.** Messages sent while a turn runs queue in arrival order
  and join the running turn as user-role messages at the next step
  boundary (before the next model call). Whatever is still queued when
  the turn ends starts one follow-up turn. `up` on an empty composer
  takes the queue back for editing; `esc` or `ctrl+c` cancels the turn
  and returns the queue to the composer.

Cancellation: `ctrl+c` (the cancel trigger, §8; the action is
Go-reserved, §6.2, and in v1 its trigger is fixed: no origin, the user
included, rebinds it) cancels the running
turn by cancelling the fantasy
context; in-flight Lua tool calls get the catchable "context canceled"
error at the next opcode (see the watchdog above). One residual gap: a Go
call invoked from Lua that blocks mid-call (e.g. `zemi.job.out` waiting on
a subprocess) does not observe cancellation until it returns. Every
request-ctx cancellation (watchdog expiry, veto deadline, turn cancel,
hot mode) cancels the async handles that request started, inside Go,
which is what makes those terminate.

## 5. Lua runtime and plugin layout

Directories (XDG):

```
~/.config/zemi/           config (git repo)
  init.lua                entry point, runs on every start
  lua/**/*.lua            user modules, require()-able
  skills/<name>/          authored skills (enabled by default)
  agents/<name>.md        subagent definitions (§6.3)
  commands/<name>.md      markdown slash commands
  workflows/<name>.lua    saved workflow scripts
  permissions.lua         Go-generated rule dump (data, not code; §6.1)
~/.local/share/zemi/      data
  site/pack/*/start/*/    plugins: plugin/*.lua loaded, lua/ require()-able
  skills/<name>/SKILL.md  learned skills (start disabled)
  builtin-skills/<name>/  zemi's own skills, rewritten from runtime/ on sync
  drops/                  files dropped in from outside the project (@drop:<name>)
  plans/<session id>.md   plans approved in plan mode
~/.local/state/zemi/      state
  zemi.db                 global store (user memory, skills, kv, revisions)
  projects/<hash>/zemi.db per-project store (sessions, messages, todos, ...)
  projects/<hash>/scratch/<session id>/   per-session scratchpad (§9 write root)
  project_trust.json      trust hashes for project config
  logs/
<project>/
  .zemi/init.lua          project-local config, loaded after user config
  .zemi/agents|commands|workflows/  the same shapes, only once trusted
  ZEMI.md, AGENTS.md, CLAUDE.md   context files
```

Load order is a list of boot steps, and the permanent permission store
loads early so no userland code runs before it: host `zemi.*` namespaces
-> `permissions.lua` (fail-closed) -> builtin commands -> the memory,
reflector, skills, agents, workflow, mcp, lsp and window commands -> the
shipped Lua UI (`require("zemi.defaultui")`) -> plugins
(`site/pack/*/start/*/plugin/*.lua`, sorted) -> user `init.lua` ->
authored skills' enable hooks -> project `.zemi/` (`init.lua`,
`agents/`, `commands/`, `workflows/`; any of them triggers the trust
prompt below) -> markdown commands (user, then a trusted project) ->
`app.ready` -> learned skill hooks, last and least trusted. Boot runs on
its own goroutine, so it may block on the trust prompt without holding
the UI. Project config loads late with
flagged reduced trust: its registrations are tagged `origin = "project"`
(shown in the UI and logs), its permission rules may only tighten (deny or
ask rules and `not_match` guards; allow rules are user-config-only), and
its capability denylist is mechanical: no `tool.pre` rewrite handlers, no
`zemi.tool.override` (same rewrite capability, same denial), no
`tool.post` context injection (plugins get it fenced, §6.2; project
config is refused outright), no
`load`/`loadstring` (a trust hash pins files, not code built at runtime),
no `zemi.provider.register`, no `zemi.agent.profile` /
`zemi.agent.set_default` (prompt authorship is a stronger lever than any
`tool.pre` rewrite), no `agent.send`, no `agent.assemble` section
injection, no `zemi.memory.add`, no `zemi.model.set` or `zemi.config.reload`,
no `zemi.command.run` of a more trusted origin's command (its body runs
under that origin), no closing, focusing or moving a window another origin
opened, no keymap that is a prefix or an extension of a more trusted
binding's key, no `app.quit` denial (ignored), no registration of a builtin tool name
(builtin ids are reserved; last-writer-wins applies only within one
origin; cross-origin profile id collisions are refused, §11). The same
denylist binds learned-origin code (learned skill hooks, §10.5). A
one-time trust prompt gates the first project-config load and stores a
hash of the project config tree; a changed hash (a pull swapped the Lua)
re-prompts before load. The prompt answers `y` (trust this
hash), `n` (not now: ask again next start) or `d` (never for this hash:
stored as a deny, so only a changed tree asks again).

Runtimepath, in order: `~/.config/zemi`, then
`~/.local/share/zemi/site/pack/*/start/*` (sorted), then the embedded
`runtime/` (lowest precedence).

`require` rules:

- `require("zemi.foo")` always resolves to the preloaded Go namespace or the
  embedded `runtime/lua/zemi/foo.lua` stdlib. User code cannot shadow it:
  all `zemi.*` tables are frozen (read-only `__newindex` on a metatable
  locked with `__metatable`, so `setmetatable` and metatable mutation are
  refused; gopher-lua honors this protection), the global `zemi` binding
  itself is write-guarded, and every per-chunk environment has a
  raising `__newindex` for a protected-name set (`zemi`, the frozen
  stdlib tables, the base globals): an assignment like `zemi = fake`
  raises, so a later-loading origin cannot install a proxy table over the
  real namespaces and intercept, tamper with, or swallow host calls
  inside its own handlers (ordinary new globals land in the chunk's own
  environment unblocked, so `MyThing = MyThing or {}` still works),
  `rawset` and `rawequal`
  are removed from the base library (gopher-lua ships them in baselib by
  default, which would bypass the freeze; verified in its source), and Go
  re-checks namespace integrity per request class (each table still holds
  exactly the registered functions), so monkey-patching fails loudly.
- Everything else resolves over runtimepath (`lua/?.lua`), nvim-style;
  the `package` library is not opened (`package.loaded` and
  `package.loaders` would hand out the unfrozen stdlib and run files from
  `package.path`): `require` is Go-owned and resolves through a Go-side
  cache, so no Lua-side table can proxy another origin's modules.

Sandbox (what userland gets):

- Kept: `base` minus `dofile`/`loadfile` (they read and execute raw files,
  bypassing both the jail and `zemi.fs`); `load`/`loadstring` kept (Lua
  source only; gopher-lua has no bytecode loader at all, which makes the
  classic bytecode-execution hole a non-issue); `table`, `string`, `math`,
  `coroutine`, minimal `debug` (`traceback` only, with frames from other
  origins collapsed to their origin label), `os.time` / `os.date` /
  `os.clock`.
- Removed: `io`, `os.execute`, `os.remove`/`rename`/`exit`, `os.getenv`
  (use `zemi.env.get`), `os.setenv` (env mutation subverts the §9 env
  gates),
  `os.tmpname` (bypasses the jail), the `package` library, `setfenv`/`getfenv`
  (environment swapping is how freezes get dodged), and the `channel`
  library (`channel.receive` would block the interpreter outside the
  watchdog's reach).
- No shared mutable stdlib: the `string`/`table`/`math`/`coroutine` tables
  and base globals are frozen with the same `__metatable` lock as the
  `zemi.*` namespaces (`setmetatable` on them is refused; Go re-checks
  their contents per request class), and each chunk runs in an environment
  whose `__index` reads that frozen surface; the environment itself is
  `__metatable`-locked, and `_G` resolves to the chunk environment, so
  `_G.x = y` hits the chunk's own raising `__newindex`. A patch of
  `pcall` or `string.format` by one origin cannot intercept calls made by
  Lua of another origin (assignment into the shared tables raises, like
  the `zemi` binding).
- `print` routes to the log and, when called from a handler, to `zemi.log`.
- All filesystem/net/subprocess access is via `zemi.fs` / `zemi.http` /
  `zemi.job`, each permissioned and rooted (see §9).
- Origin is per call, not per file: chunks created by `load`/`loadstring`
  inherit the origin and the environment of the loading chunk (the same
  raising `__newindex` and `__index` chain; they never receive the
  interpreter's global table), and the embedded
  `runtime/lua/**` stdlib runs as a distinct `builtin` origin, so a
  static-scan denylist cannot be dodged by building source at runtime.
  A file's origin comes from a directory-class map Go applies to its
  `EvalSymlinks`-resolved path (a symlink is classified by what it
  resolves to, so an alias cannot borrow another class):
  `~/.config/zemi/**` is user, `~/.local/share/zemi/site/pack/**` is
  plugin, `<project>/.zemi/**` is project,
  `~/.local/share/zemi/skills/**` is learned, embedded `runtime/**` is
  builtin. A resolved path in no class is refused at load. Origin never follows the requirer: a
  module under `site/pack/...` stays plugin origin and a project module
  stays project origin even when `init.lua` requires it, so untrusted
  code cannot ride the boot phase under a user badge.
- Boundary caps: values crossing the Lua <-> Go marshal are capped (string
  16 MB, table depth 64; configurable), cycles are refused with an error,
  and every Go function exposed to Lua runs under `recover`, so a Go panic
  becomes a catchable Lua error logged as a zemi bug.

Rationale for gopher-lua over JS/Starlark/WASM plugins: single small
interpreter, real coroutines, nvim muscle-memory, config-as-code with the same
language as plugins, mature Go embedding. Accepted limitations: one
instruction stream (no parallel Lua execution; mitigated by async I/O APIs
and cancellation, §4; Lua tools default to `parallel = false`), Lua 5.1
(byte strings, no integer division), and per-instruction cancellation that
cannot reach into a blocked Go call (covered by the watchdog, §4).

## 6. The host API (`zemi.*`)

All namespaces are preloaded Go-backed tables (plus a small Lua stdlib).
This is the full v1 surface (v1 = M0-M4; M5-only APIs are marked); the
binary version is `zemi.api.version`, and §13 says which namespaces land
in which milestone.

### 6.0 Conventions

- **Errors**: calling an API wrongly (bad argument types, unknown member,
  invalid enum) raises a Lua error, catchable with `pcall`. Operations that
  can fail at runtime split two ways: async ones (jobs, HTTP, provider
  calls) return a handle and deliver failure through it; synchronous ones
  (`zemi.fs.read`/`write`, `zemi.state.put`, `zemi.json.decode`,
  `zemi.job.start`, the memory, learn and skills calls) return
  `nil, err` instead of raising. The split is mechanical: runtime
  failures never raise; misuse (bad arguments, a masked mutator in a
  render, an origin that may not make the call) and a permission gate
  that does not allow always raise.
- **Handles vs data**: anything holding a host resource (job, HTTP call,
  timer, session, window) is a handle with colon-call methods (`h:wait()`,
  `t:stop()`). Everything else is a plain snapshot table; mutating it never
  feeds back into the host.
- **Frozen namespaces**: all `zemi.*` tables are read-only; the API only
  grows through versioned Go changes, visible in `zemi.api.version`.

### 6.1 Config and lifecycle

```lua
zemi.config.get("agent.model")                -- dotted path lookup
zemi.config.set("ui.theme", "default")
zemi.config.watch("agent.model", function(new, old) end)
zemi.env.get("EDITOR")                        -- env-read; secret-shaped
                                              -- names (§9 deny list) are denied
zemi.log("...")                               -- callable table: zemi.log(msg) = info
zemi.log.info("...", { key = "value" })
zemi.notify({ title = "hi", body = "...", level = "info" })
```

Config values from `init.lua` merge with the defaults in Go's schema.
`zemi.config.set` is in-memory for the session, callable from user config
only, and accepts only the keys the schema marks user-writable; everything
else is refused with an error (an allowlist, not a denylist). The
autonomy knobs (§10.8) are not in the user-writable set: `config.set`
refuses them from every origin at every time, boot included; only Go
writes them (`Settings.SetHost`). Persistent config changes are edits to
files in the config repo (the user's, or the agent's through the
permission-gated file tools); the one commit zemi makes itself is the
permanent-rule commit below.

Permission rules are data with one API, two scopes, and an optional
principal:

```lua
zemi.permission.rule{ subject = "tool.bash", pattern = "make *",
                      decision = "deny", match = "make check",
                      scope = "session" }
```

Lua rules carry no principal, so they match every principal (§9); rules
stored by an `always` answer are scoped to the `model` principal, so a
model tool call cannot ride a rule meant for the user. `scope =
"session"` lives in memory until the session ends; `scope = "permanent"`
is added to the rule store and committed by Go as a one-line commit (the
user's own config line is the review). `permissions.lua` is not code: Go generates it from the rule
store on every change (a stable, versioned serialization of the permanent
rules only; the store is the source of truth and the file is its dump;
session rules live in memory and never touch the file), loads it
at boot as data, and refuses file content that is not rule data with a
boot error that leaves the store empty (fail-closed; the user fixes or
deletes the file), so nothing can smuggle executable Lua
under the guise of a permission patch. Origins: user config may add any
rule; plugin, project, and learned origins may add only session-scope
deny/ask rules. Overlay `always` answers (§8) append session rules to
the same store, so rules never live in `zemi.state` or the DB, and no
Lua can forge one. Only the user's own config and `always` answers
write allow rules (the widening direction; plugin, project, and learned
rules are deny/ask only, so their globs can only tighten).

### 6.2 Keys, commands, events

```lua
zemi.keymap.set("ctrl+t", function() zemi.notify({ body = "hi" }) end)
zemi.keymap.set("ctrl+e", "zemi.ui.help", { desc = "help overlay" })
zemi.keymap.del("tab")                 -- unbind (also a shipped default)
zemi.keymap.get("ctrl+e")              -- { key, action, desc, origin } or nil
zemi.keymap.list()

zemi.command.add("Todo", function(args)
    zemi.notify({ body = "todo: " .. (args.args or "") })
  end, { desc = "show todos", nargs = "?", complete = function(prefix) return {} end })

zemi.event.on("tool.pre", function(ev)
    if ev.tool == "bash" and ev.input.command:match("^rm%s") then
      return { decision = "deny", reason = "no rm from the model" }
    end
  end, { id = "safety.no-rm" })
zemi.event.emit("user.my-thing", { any = "payload" })
```

There are no editor modes, so a binding is just a key: the composer is
always focused and commands are slash commands typed in it. Key notation
matches bubbletea's strings (`ctrl+s`, `alt+enter`, `esc`; named keys are
lowercased); a string action in `zemi.keymap.set` resolves to a registered
command name or a dotted
host-API call, and executes under the principal of the config that
bound it (a principal is what an operation is attributed to: `model`,
`user`, `plugin:<path>`, `project`, `learned`, or `builtin` (§5); §9
has the full list including `mcp:<server>` (M5) and attributes by the
registration origin of the running handler, never the keypress).
Binding precedence across origins is `user > builtin > plugin > project`;
last-writer-wins applies only within one origin, so the project config
loading last cannot shadow a user binding (nor bind a prefix or an
extension of a more trusted key sequence), and `zemi.keymap.del` unmaps
keys set by the caller's origin or a less trusted one (user config can
remove shipped defaults; a plugin cannot remove user keys). The actions behind cancel-turn and
force-reload are Go-reserved: their triggers (the cancel key and the
exact, case-sensitive, non-abbreviable `/reload` command, checked on
composer submit before any Lua-visible command lookup) are fixed
in v1 (no origin, user included, rebinds them), so a compromised config
cannot relocate the panic button or put a decoy on it (case-variant
command names are refused by the §11 collision rules). `<leader>`
expands to `ctrl+g` (the `keymap.leader` setting); multi-key sequences arm a
pending state (shown in the statusline) until the next key or timeout.
Command `nargs` follows Vim's spec strings (`?` = 0 or 1, `+` = 1 or more,
`*` = 0 or more).

Event catalogue (v1, each emitted on the interpreter thread):

- app: `app.ready`, `app.quit` (vetoable), `config.reloaded`
- session: `session.new`, `session.open`, `session.close` (with summary stats)
- agent turn: `agent.start`, `agent.finish`, `agent.error`
- subagents: `agent.subagent.start`, `agent.subagent.finish`
- prompt: `agent.assemble` (returns a section to add, §10.7)
- stream: `agent.text_delta`, `agent.reasoning_delta`
- tools: `tool.pre` (vetoable: `deny|rewrite|halt`), `tool.post`, `tool.denied`
- permission: `permission.request` (vetoable), `permission.resolved`
- ui: `ui.resize`, `ui.theme`, `overlay.open`, `overlay.close`,
  `composer.change`, `composer.submit` (vetoable)
- learn: `memory.written`, `skill.learned`, `reflector.start`,
  `reflector.done` (counts, or `error` on failure)
- io: `job.exit`, `win.open`, `win.exit`, `win.close`, `lsp.request`

Every emission is recorded whether or not a handler exists: `zemi.event.log()` holds the
last 500 (deltas excluded), `zemi.event.stats()` counts per name, and `zemi.notify.list()`
holds the last 100 notifications, so a widget can show what the session did.

The `tool.pre` event is the generalization of crush's `hookedTool` +
PreToolUse hook: it runs in-process in Lua, can rewrite JSON input, and its
aggregation rules match crush's (deny wins, patches merge in order). Lua
may emit only `user.*` event names; `zemi.event.emit` refuses host-owned
prefixes, and every emitted event carries its emitter's origin in
`ev.from` (Lua emissions carry the emitting origin; host-emitted events
carry `from = "host"`), so a handler can tell a plugin's emission from
the user's own.

Vetoable events share one return contract. Return `nil` for "no objection";
otherwise return a decision table:

- `tool.pre`: `{ decision = "deny", reason = "..." }` blocks the call (the
  reason becomes the tool's error result); `{ decision = "rewrite", input =
  <patched input> }` shallow-merges over the original JSON and the rewritten
  input re-enters the pipeline at schema validation and the permission gate;
  on re-entry the `tool.pre` handlers run again in deny-only mode (deny
  decisions are re-checked against the rewritten input; rewrite and halt
  do not re-fire), so a rewrite cannot silence another origin's deny hook
  and re-entry still terminates (§6.4);
  `{ decision = "halt", reason = "..." }` ends the turn after this tool call
  (fantasy `StopTurn`). `tool.post` is a notification: its return value is
  ignored. There is no `allow`
  decision: absence of denial is the only pass, so a Lua hook can never
  pre-approve a permission (crush's `allow` does pre-approve; zemi
  deliberately diverges). A `rewrite` from a non-user principal
  additionally flips an `allow` decision to `ask` (a `deny` stays `deny`:
  a rewrite never loosens an existing denial), so a rewrite can never
  ride a pre-existing allow glob. An `always` answer (key `a`, §8)
  stores session-scope allow rules scoped to the `model` principal, and
  they are prefix globs, not exact values: for `tool.bash` one rule per
  normalized segment, each the segment's two-word command prefix plus
  that prefix with ` *` (a segment with no such prefix stores its exact
  normalized string); for `fs.read` and `fs.write` the file's directory
  with `/*`; for `net.*` both `scheme://host` and `scheme://host/*`;
  for anything else the exact argument. A call §9 gate-forces to `ask`
  (heredoc/herestring, dangerous assignment) offers no `always` option
  at all, so it re-asks every time.
- `permission.request`, `app.quit`, `composer.submit`:
  `{ decision = "deny", reason = "..." }` or nil. Lua can deny; it can
  never grant.

### 6.3 Agents, sessions, models

```lua
-- Profiles are named agent shapes (fantasy agent + prompt + tools + model).
zemi.agent.profile("coder", {
  model = "anthropic/claude-sonnet-4-5",
  system = [[You are zemi, a coding agent...]],
  tools = { "read", "edit", "bash", "glob", "grep" },
  context = { files = { "ZEMI.md", "AGENTS.md" } },
  temperature = 0.2,
})

zemi.agent.set_default("coder")
zemi.agent.current()                 -- session handle: { id = "...", profile = "..." }
zemi.agent.send("fix the flaky test")  -- returns immediately; stream is
                                       -- async; during an active turn the
                                       -- message enqueues as steering (§4)
zemi.agent.cancel()
zemi.agent.run({ agent = "explore", prompt = "...",              -- subagent,
                 description = "..." },                          -- async
               function(result, err) end)    -- returns a handle: h:cancel()
zemi.agent.list()                    -- the agent definitions and their source

zemi.mcp.add("name", { ... }); zemi.mcp.list()  -- MCP servers bridged as tools
                                         -- (user origin only)

zemi.model.list()                    -- { { id = "anthropic/...", ... } }
zemi.model.set("anthropic/claude-sonnet-4-5")
zemi.model.pick()                    -- picker over the registered models

zemi.provider.register({
  id = "myllm",
  kind = zemi.provider.kind.OPENAI_COMPAT,  -- fantasy/openaicompat under the hood
  base_url = "https://llm.internal/v1",
  api_key = zemi.env.get("MYLLM_KEY"),
  models = { { id = "main", display = "Main" } },
})
```

There is no `task` profile. A headless sub-turn is a subagent: a named
definition (builtin `explore`, `web` and `general`, plus `agents/*.md` under the
config home, zemi's own, and a trusted project's `.zemi`) run as a fresh
runner over the same tool pipeline, in a child session recorded with
`parent_id`. Subagents never get `agent`, `workflow`, `todos`,
`question`, `plan`, `open_in_editor` or `Lua`, so nesting stops at depth 1, and `zemi.agent.run`
is refused to every origin but user and builtin: an agent acts with the
user's tools and permissions. The reflector is not a subagent (§10.3).

`zemi.provider.register` requires a user-owned registration origin: Lua
from a plugin, a project, or a learned source cannot call it (the
mechanical check is the origin, not the caller's principal). Provider base URLs ride the §9 net
gate; the key comes from user config (`zemi.env.get` reads secret-shaped
variables for user and builtin origin only) and is never exposed back to Lua.

Session/message persistence and cost/token accounting mirror crush's model
(SQLite, summary message, todos), simplified: sessions, messages (JSON parts),
tool-call telemetry.

System prompts are Go string constants in `internal/hostapi/agent.go`;
there are no template files. They are assembled per session and pinned
there, because anything that varies per turn busts the provider's prompt
cache. Lua injects sections via the `agent.assemble` event (memory
digest, skills index, project context ship as built-in sections).

### 6.4 Tools

```lua
zemi.tool.register({
  name = "figlet",
  description = "Render text as ASCII art",
  input = {                                  -- JSON Schema
    type = "object",
    properties = { text = { type = "string" } },
    required = { "text" },
  },
  parallel = false,
  timeout_ms = 5000,
  -- execute returns a string, { output = string }, or a table sent as JSON;
  -- errors: `error(msg)` is caught and becomes a tool error result.
  execute = function(args, call)             -- call: { name, origin }
    -- zemi.job.out blocks the queue; timeout_ms bounds it
    return { output = zemi.job.out({ cmd = "figlet", args = { args.text } }).stdout }
  end,
})

zemi.tool.override("bash", { pre = function(input)          -- wrap builtin;
    if input.command:match("^rm%s") then                    -- nil = pass through
      return { decision = "deny", reason = "no rm here" }
    end
  end })
zemi.tool.list()          -- introspection for UI and reflector
```

Built-ins ship in Go, modeled on crush's set (crush also ships `ls` and
`download`, omitted; `skill` is zemi's own): `bash`, `read` (crush
names it `view`; it returns images as image content, and carries the
nearest instruction file not already in the prompt, once per session),
`write`, `edit` (incl. whitespace-tolerant variant),
`multiedit`, `glob`, `grep` (both Go-exec'd async against `rg` with a Go
walker fallback, §2), `fetch`,
`todos`, `question` (1-4 questions in one overlay, options or a typed
answer), `plan` (shows a plan and asks for approval; in plan mode the
approval picks the mode to implement in), `open_in_editor` (the user
edits a file in their own editor; the result is the diff),
`web_search` (DuckDuckGo Lite, or the user's SearXNG at `web.search_url`),
`job_start`/`job_read`/`job_list`/`job_kill`,
`memory_add`/`memory_search`/`memory_update`/`memory_forget` (model
memory edits re-run intake and ask, §10.4), `diagnostics` (language
server errors for any file), `lsp` (definition, references, rename and
symbols by name), `skill` (loads a SKILL.md body on demand), plus the
four the host adds: `agent` (delegate to a subagent), `workflow` (write
or run a workflow script), `window` (a program in a terminal drawn inside
zemi) and `Lua` (run Lua inside zemi, main agent only, as user origin:
full access to toggle skills and anything else user Lua can do).
Subagents never get `agent`, `workflow`, `todos`, `question`, `plan`,
`open_in_editor` or `Lua` (§6.3). Every tool, Go or Lua, passes through the same pipeline:

```
model call -> schema validation
           -> tool.pre events (Lua: deny | rewrite | halt)
           -> permission gate (config rules + learned rules; ask = overlay)
           -> execute (Go impl or Lua callback on interpreter thread)
           -> tool.post events
           -> result -> fantasy ToolResponse
```

A `rewrite` re-enters the pipeline at schema validation and the permission
gate; it never skips them. On re-entry the `tool.pre` handlers run again
in deny-only mode (deny decisions are re-checked against the rewritten
input; rewrite and halt do not re-fire, §6.2), so a rewrite cannot
silence another origin's deny hook and re-entry still terminates. Tools
the host runs itself (`agent`, `workflow`, `window`, `Lua`, MCP and Lua
tools) refuse a rewrite with an error result instead. `zemi.tool.override(name, { pre })` wraps a
builtin: its `pre` is a scoped handler for that tool only, running right
after the global `tool.pre` events under the same decision contract.

Fantasy may dispatch parallel tool calls (up to 5 concurrent, verified in
its source): Go tools genuinely run concurrently; two Lua tools never
overlap (one interpreter), so `parallel = true` on a Lua tool only means
other Go tools may proceed while it waits for the queue.

Errors are results, not exceptions: a failed permission or Lua error becomes
a `ToolResultOutputContentError`-style message so the model can recover.

### 6.5 I/O for Lua

```lua
zemi.fs.read("src/main.go")                  -- jailed to project roots + config
zemi.fs.write("notes.md", "...")             -- jailed to project roots only;
                                             -- permission-gated on top (§9)
zemi.fs.glob("**/*_test.go"); zemi.fs.grep("pattern")
local h = zemi.http.get("https://example.com")    -- returns a handle now
local res = h:wait()                              -- blocks the queue until done
                                                  -- (legal only inside a tool
                                                  -- handler, see below; the UI
                                                  -- shows cached lines; Go
                                                  -- chrome still renders):
                                                  -- { status, body }
zemi.http.get(url, { on_done = function(r) end }) -- or callback style
zemi.job.start({ cmd = "go", args = { "test", "./..." },
                 on_stdout = nil, on_exit = function(ev) end })
zemi.job.out({ cmd = "figlet", args = { "hi" } }) -- blocking convenience: run,
                                                  -- wait, { stdout, stderr, code }
local t = zemi.timer.set(1000, function() end)    -- one-shot; also zemi.timer.every
t:stop()                                          -- cancels a pending timer
zemi.state.put("recent.model", "anthropic/x")      -- persisted kv, global scope
zemi.state.get("recent.model")
```

Async I/O returns handles (§6.0 convention): block on `h:wait()` or pass
callbacks; the watchdog cancels handles a tool left running. Blocking
waits (`h:wait()`, `zemi.job.out`) are not refused anywhere, but they
hold the interpreter until they return, so they belong in tool handlers
and keymaps and event handlers use the callback form. Host-API calls
from Lua never open the interactive overlay: when the matched decision
is `ask`, the call raises `permission ask required` (a
`permission.request` event is emitted, post-hoc and informational: the
outcome is already decided and the event carries no veto leg); only model tool calls surface the overlay, on
the agent worker (§4). Keymap- and command-bound use of an ask-default
API therefore needs a pre-existing allow rule from the user's own config,
or it fails closed every time. URLs go through the permission gate like tools do
(§9 covers redirects and IP rules). Subprocess environments are filtered:
a child inherits the parent env minus the §9 secret denylist; extra vars
are opt-in per job (`env = { ... }`, Lua-supplied values only, never
looked up from the parent by name); every `env`-extras key runs the §9
environment-affecting gate (a dangerous name forces `ask` for the job
regardless of allow rules, and env extras render in the ask overlay's
preview for model `job_*` calls; for Lua-originated jobs a gate-forced
ask is a refusal); jobs never inherit the controlling
tty (each runs in its own session through `internal/proc`; stdin is
closed unless the job asks for a pipe with `stdin = true`).

### 6.6 UI

```lua
-- A statusline segment
zemi.ui.widget.register({
  id = "clock", region = "statusline.right", priority = 10,
  render = function(ctx)   -- ctx: { width, session, agent, time, ... }
    return { { { text = ctx.time, hl = "Comment" } } }  -- lines of segments
  end,
})

-- Overlays (modals/pickers) with their own keymaps
zemi.ui.overlay.open({
  id = "model-picker", title = "Model",
  items = zemi.model.list(),
  render_item = function(m) return m.display end,
  on_select = function(m) zemi.model.set(m.id) end,
})

-- Pickers in the shape of vim.ui.select: on_choice(item), nil on cancel
zemi.ui.select(zemi.model.list(), {
  prompt = "Model",
  format_item = function(m) return m.display end,
}, function(m) if m then zemi.model.set(m.id) end end)

-- Themes: palettes + highlight groups resolved by Go (colorprofile-aware)
zemi.ui.theme.register("tokyonight", {
  palette = { bg = "#1a1b26", fg = "#a9b1d6" },
  hl = { Comment = { fg = "fg", italic = true },
         StatusLine = { bg = "bg", fg = "fg" },
         DiffAdd = { fg = "#9ece6a" } },
})
zemi.ui.theme.set("tokyonight")
```

UI extensibility contract:

- The screen is a stack: regions (`header`, `chat`, `composer`, `statusline`)
  composed by Go with ultraviolet constraints, plus an overlay
  stack. The shipped chat and statusline are default Lua registrations
  in `runtime/lua/zemi/defaultui.lua`, replaceable by id. The composer is
  Go-owned and zemi's own editor (`internal/ui/composer.go`; there is no
  bubbles dependency): visual wrapping, a large paste folded into a
  token, submitted-message history, `ctrl+z` undo, and a
  `$VISUAL`/`$EDITOR` handoff for the draft. Lua reacts to
  `composer.change`/`composer.submit`, reads and edits the draft through
  `zemi.composer` (§6.7), and registers decoration widgets around it,
  but cannot replace it in v1.
- Widgets return lines of `{ text, hl }` segments; Go measures, wraps
  (opt-in per widget), styles, and diffs via bubbletea v2. No lipgloss in
  Lua; highlight names only. Rich text goes through
  `zemi.ui.markdown(text, width)`, a Go bridge to glamour returning
  styled segment lines (how the default chat widget renders assistant
  text). Glamour's styles come from the active theme's `Normal` and
  `Markdown*` groups; its output is parsed back into segments whose `hl`
  is `sgr:<params>`, an inline SGR style that only ever decodes to
  colors and attributes, never other escapes.
- Themes: palettes map names to colors; `hl` groups reference palette
  names or raw hex; Go resolves to the terminal's colorprofile
  (truecolor/256/16, downgrading as needed).
- Key routing: bubbletea key -> normalized string -> Go-reserved cancel
  trigger -> Go-owned chrome (trust prompt, permission asks, model
  picker, question/memory overlays) -> active Lua overlay ->
  open composer popup -> keymap (keys and key sequences; printable keys
  stay with the composer) ->
  composer (always focused; there are no modes). Overlays capture first,
  user maps override defaults, `zemi.keymap.del` unmaps.
- Slash palette: typing `/` in the composer opens a popup listing registered
  commands, filtered case-insensitively as you type; `up`/`down`
  navigate, `tab` accepts (fills `/name `), `enter` submits the typed
  line, so `/name args` runs the command instead of sending to the
  model; an unknown name toasts and does not send; `esc` closes. The
  exact, case-sensitive `/reload` is Go-reserved: it is checked on
  composer submit before any Lua-visible command lookup (§4).

### 6.7 Learning, session, skills

```lua
zemi.session.id(); zemi.session.title(); zemi.session.set_title("...")
zemi.session.messages()                -- { id, role, json } per stored message
zemi.session.todos()                   -- this session's todo list
zemi.session.list(); zemi.session.new(); zemi.session.open(id)
zemi.session.rewind(message_id)        -- restore files, then truncate (§8)
                                       -- new/open/rewind: user config only
zemi.memory.search("flaky", { scope = "project", limit = 10 })
zemi.memory.add({ kind = "gotcha", text = "...", tags = { "pg" } })
                                       -- same host-side intake policy as §10.4
zemi.learn.proposals()                 -- pending proposal cards
zemi.learn.decide(id, "accept")        -- user principal only (keymap/command)
zemi.skills.list(); zemi.skills.enable("local-integration")
```

The rest of the installed surface (`internal/hostapi/host.go` holds the
list): `zemi.json` (`encode`, `decode`), `zemi.composer` (`get`, `set`,
`insert`, `submit`, over a snapshot the UI publishes and edits it
applies on its own goroutine; edits from user config only, since a
submitted line reaches slash commands and `!shell`), `zemi.health` (`register(name, fn)`,
whose rows join the Go checks in `/health` and `zemi --health`),
`zemi.workflow` (`run`, `list`), `zemi.lsp` (`add`, `disable`, `list`),
`zemi.mcp` (`add`, `list`), `zemi.win` (windows), `zemi.errors` (`list`),
and `zemi.permission.mode` (`add`, `remove`, `list`; user config only, §9).
There is no `zemi.eval`: the eval harness was never built (§10.6).

`zemi.learn.decide` and `zemi.skills.enable` require a user or builtin
registration origin: the model can propose, but a
plugin-registered keymap or command invoking them is refused; the trust
anchor is a human keypress on a binding the user's own config registered.
They gate like the registration APIs in §9 (refusal, never an ask) for
origins that may not register.

## 7. Storage (SQLite)

```
sessions(id, parent_id, title, profile, created_at, updated_at, summary_message_id,
         prompt_tokens, completion_tokens, cost)
messages(id, session_id, role, parts_json, created_at)          -- fantasy messages
todos(id, session_id, content, status, active_form, ord, created_at)
tool_calls(id, message_id, tool, input_json, ok, error_kind, latency_ms, denied,
           decided_by, created_at)                              -- decided_by: rule|hook|user|overlay|default
memory(id, scope, kind, text, tags, provenance_session_id, confidence,
       hit_count, created_at, updated_at, last_hit_at, superseded_by,
       origin)                                                  -- FTS5(text)
checkpoints(message_id, session_id, path, existed, content, created_at)
                                                                -- pre-turn file content for rewind (§8)
proposals(id, kind, payload, created_at)                        -- pending, until reviewed
skills(name, path, source, enabled, created_at)                 -- source: authored|builtin|learned
config_revisions(id, created_at, title, changes_json, paths_json, status, applied_by,
                 eval_ok, commit_sha)
                                                                -- paths_json: paths the revision wrote
                                                                -- commit_sha: its config-repo commit
                                                                -- eval_ok: dead column, evals never shipped
kv(key, value_json, updated_at)                                 -- zemi.state
```

One file per scope: global DB at `~/.local/state/zemi/zemi.db`, per-project
DBs at `~/.local/state/zemi/projects/<hash of project root>/zemi.db`. The
global DB holds user-scope memory, skills, kv and all `config_revisions`
(the permanent-rule commits to the one global config repo); a project DB
holds its
sessions, messages, todos, tool_calls, checkpoints, proposals, and
project-scope memory. Opening a store sweeps checkpoints older than 30
days; sessions are never swept (the session TTL is 0, and zemi deletes a
session only when the user says to).
Indexes: `messages(session_id)`,
`tool_calls(message_id, created_at)`, `memory(scope, last_hit_at)`,
`memory(scope, updated_at)`, `sessions(parent_id)`,
`sessions(updated_at)`, `todos(session_id)`, `checkpoints(session_id)`. `memory_fts` is an external-content FTS5 table over
`memory.text`, synced by triggers (insert, delete, and a
delete-then-reinsert on text update) (FTS5 confirmed working in
modernc.org/sqlite by a compile-and-query oracle). `zemi.memory.search`
filters `superseded_by IS NULL` (dead memories never rank) and
bumps `hit_count` and `last_hit_at` on hits; the digest ranks by FTS5
score x recency x `log(1 + hit_count)`, the log keeping an inflatable
count (the model can drive hits by asking for searches) from dominating.
`memory.origin` is `user` or `learned`: `/memory <note>` writes `user`,
everything else writes `learned`; the autonomy gate, the supersede guard
(§10.4), and the digest labels (§10.7) read this column, not the
provenance string. No embeddings in v1; embeddings via a
configured provider later (`zemi.memory.search` signature does not change).

## 8. UI core detail

- **Layout**: header (no default widget; the region stays for Lua), chat
  (flex), a Go-drawn working row while a turn runs (pulsing bullet,
  shimmering label naming the current activity, elapsed time, interrupt
  key, queue depth; `ui.animations = false` keeps it static), composer (3..8 rows, textarea grows), statusline
  (1 row: `statusline.left` / `statusline.right` widgets ordered by
  `priority` ascending, overflow truncates the lowest-priority ones; the
  shipped `statusline.info` widget shows the model, the session token tally, the
  session cost when the model spec carries prices (`cost_per_1m_in` /
  `cost_per_1m_out`), and
  the context-window percent, warning colors past 80%/95%).
  Overlays float, centered or anchored.
- **Rendering loop**: the state generation is bumped at most once per
  renderer tick, not per tea message; `View`
  asks each visible region for its cached widget lines; Lua `render` runs
  only when invalidated: v1 re-renders on every generation change (cheap:
  render is pure and cached by `(id, width, generation)`), plus resize
  dependency-declared invalidation is opt-in later.
- **Chat transcript**: messages are stored as content blocks (text, tool
  call, tool result, reasoning); tool output is sanitized before render
  (see below); the default chat widget maps blocks to
  lines, applies markdown rendering via glamour for assistant text
  (cached per block id and width; a frame never re-runs glamour). The
  view word-wraps (the widget sets `wrap`) and is bottom-anchored: long
  lines stay visible instead of truncating. Scrollback (`pgup`/`pgdown`,
  `ctrl+home`/`ctrl+end`, mouse wheel) moves the viewport off the tail;
  a scrolled view stays anchored while new lines fill underneath, and
  sending a message snaps back to the tail. Persisted reasoning blocks
  collapse to one `✻ thought Xs (ctrl+o to expand)` line unless shown
  (default keymaps below).
  The rendered view is derived from host-held blocks: the raw message JSON
  is always in the store (and in the log), reachable via
  `zemi.session.messages` and a copy-raw action, so no widget can hide what
  the model actually received.
- **Composer**: zemi's own Go editor (`internal/ui/composer.go`), not a
  bubbles textarea; `enter` sends, `alt+enter`
  newline (configurable); a large paste folds into a token that expands
  on submit. The composer stays editable
  during a turn: a submit while the turn runs queues as steering (§4)
  instead of spawning a second turn.
- **Default keymaps** (all rebindable by user config except the
  Go-reserved cancel and reload triggers, §6.2): `enter`/`alt+enter`
  send/newline, `esc`/`ctrl+c` interrupt the turn (idle, `ctrl+c`
  twice within 2s quits), `ctrl+o` toggle reasoning,
  `pgup`/`pgdown` and `ctrl+home`/`ctrl+end` scroll the transcript (the
  wheel too: mouse cell motion is captured because the altscreen has no
  native scrollback).
  Commands are slash commands: `/quit`, `/help`, `/model`,
  `/new` (close the session: reflector trigger, clean slate), `/sessions`
  (list; enter resumes), `/resume [id]` (latest without an id; history and
  transcript rebuilt from the store), `/compact`, `/health` (one row per
  check: store, model key, permissions.lua, git, ripgrep, reflector,
  pending reviews, skills, recent errors, language servers, Lua runtime),
  `/errors` (recent widget, keymap and event-handler errors behind the
  statusline badge), plus the ones the host steps register (`/memory`,
  `/skills`, `/reflect`, `/agents`, `/workflow`, `/review`, `/mcp`,
  `/lsp`, `/win`). Enter on a help command row fills the composer.
- **Permission overlays**: ask-style tool calls freeze the turn, render an
  overlay (diff preview for edits, command preview for bash, command
  and env preview for job asks), and resume
  the pipeline. Concurrent ask-needing calls queue: one active overlay at
  a time, FIFO. The buttons are `Yes`, `Yes, <always label>` (only when
  the request has an always choice), `No`, and `No, and say why`; `y` and `o` answer once, `a` takes the always option, `n` and
  `esc` deny, `t` denies and hands the composer back so the user can say
  what to do instead. The overlay writes session rules only: `always`
  adds allow rules to the rule store (§6.1), scoped to the
  `model` principal, with the prefix-glob patterns §6.2 lists. Permanent
  rules never come from the overlay; they are written by
  `zemi.permission.rule{ scope = "permanent" }` in the user's own
  config. The overlay
  is Go-owned and non-replaceable: a plugin cannot whitelist itself; Lua
  may restyle the frame via theme groups only. The security-relevant
  chrome (answer keys, subject, badge) renders in fixed host colors no
  theme group can change; Go draws it itself and Lua text never renders
  inside that chrome, so a widget cannot fake a permission prompt. While
  a permission overlay is active, plugin overlays and widget regions are
  hidden until it resolves; at any other time a plugin overlay carries a
  Go-stamped `plugin:<path>` origin badge.
- **Sanitization**: every model-influenced string is sanitized by Go at
  the cell boundary before it reaches any widget: tool output, assistant
  and reasoning text (before glamour), overlay diff, command, and env
  previews,
  hook reasons/context, `zemi.notify` bodies and titles, and `/errors` strings.
  Terminal escape sequences (CSI, OSC, and other
  C1 controls) are stripped and C0 controls other than newline and tab
  are collapsed at the same boundary, so streamed content cannot restyle the
  frame, set titles, or fake host-drawn chrome. Plugin `render` output is
  sanitized the same way, and the log sink collapses C0 control bytes
  (newlines included: one record, one line) and strips C1, so a `tail -f`
  of the log cannot execute escape sequences or forge entries.
- **Golden tests**: headless mode renders regions into ultraviolet buffers;
  golden files per widget and per full frame via `charmbracelet/x/exp/golden`
  (`go test ./internal/ui/... -update` regenerates).

## 9. Permissions and safety

- Model: rules `(subject, pattern, decision, principal?)`; `principal` is
  optional and unscoped by default (it matches every principal); rules
  stored by an `always` answer are scoped to the triggering principal
  (§6.2). One subject namespace:
  `tool.<name>` (model tool calls), `fs.read`, `fs.write`, `net.<host>`,
  `lua.<ns.call>` for host-API calls from Lua (`lua.fs.read` checks the
  same jail as `fs.read`, again at the Lua boundary). Decisions
  `allow|ask|deny`; patterns are globs over the operation argument (path,
  url; commands match a normalized form: the raw string is first split on
  the shell's full separator set (`;`, `&`, `&&`, `||`, `|`, newline,
  backticks, `$(`, `<(`, `>(`; redirection and heredoc tokens
  (`[n]>&[m]`, `[n]<&[m]`, `&>`, `&>>`, `>|`, `<>`, `>&-`, `<&-`, `<<`,
  `<<-`, `<<<`) are recognized before the split so a redirect is never
  cut by a separator; fail-closed on unbalanced quotes or parens), then
  each segment is collapsed (whitespace, quote-aware argv split, `VAR=x`
  prefixes unwrapped from the matched form) and a `sh -c`/`bash -c`/`zsh -c`
  unwrap recurses into the inner string with the same split, re-applying
  the collapse and the assignment gate below to the inner segments).
  Every segment is matched on its own: the call is allowed only if each
  segment matches an allow rule, so a compound payload (including
  `cmd1\nrm -rf x` and `cmd1 & cmd2`) cannot ride a broad allow. The
  assignment gate is a token scan: every argv token of every collapsed
  segment, dequoted with full shell word semantics (embedded quotes
  removed, so `PA'TH'=/tmp/x` is the name `PATH`), shaped `NAME=value`
  or `NAME+=value` (at
  prefix position, mid-segment, a whole-segment assignment, or as an
  argument to `export`/`declare`/`env`/`sudo`) naming an
  environment-affecting variable forces `ask` for the whole call
  regardless of allow rules: the name list (`PATH`, `LD_*`, `DYLD_*`,
  `BASH_ENV`, `ENV`, `IFS`, `PYTHONPATH`, `PYTHONHOME`,
  `PYTHONSTARTUP`, `NODE_OPTIONS`, `NODE_PATH`, `RUBYOPT`, `RUBYLIB`,
  `PERL5OPT`, `PERL5LIB`, `PERLLIB`, `LUA_PATH`, `LUA_INIT`,
  `BASH_FUNC_*`, `SHELLOPTS`, `BASH_XTRACEFD`, `EDITOR`, `VISUAL`,
  `PAGER`, `GIT_SSH_COMMAND`, `GIT_EDITOR`, `GIT_PAGER`,
  `GIT_CONFIG_*`; case-insensitive) is a curated denylist, not a proof:
  `PATH=.:$PATH make`, `export PATH=.:$PATH; make`, and
  `GIT_SSH_COMMAND=/tmp/x git fetch` cannot ride stored allows. The
  accepted residual risk is exec-steering by any spelling the scan
  does not see (an unrecognized env name, or the same effect via flag
  syntax like `git -c core.sshCommand=/tmp/x fetch`), the same residue
  the mounts/hardlinks note accepts, caught by the ask defaults for
  everything the user never allowed. Redirections:
  fd-dups and fd closes (`[n]>&[m]`, `[n]<&[m]`, `>&-`, `<&-`) gate
  nothing, but those forms require a digit or `-` target: `>&word` with
  a non-digit word is bash's both-streams-to-file form and its target
  gates as `fs.write`, `<&word` with a non-digit word gates as
  `fs.read`; `&>`, `&>>`, and `>|` are writes whose target gates as
  `fs.write`; `<>` opens read-write, so its target gates as both
  `fs.read` (secret-shaped paths ask, below) and `fs.write`; a redirect
  whose target is emptied by the split (e.g. around process
  substitution) fails the whole call; heredoc and herestring bodies
  (`<<`, `<<-`, `<<<`) are shell-executed input, not inert text, so any
  call using them forces `ask` for the whole call; other input targets
  gate as `fs.read` (secret-shaped paths ask, below), other output
  targets gate as `fs.write`, and only the inert character devices
  `/dev/null` and `/dev/zero` are exempt from both (`/dev/tty` and
  every other device file gates like any outside-jail path). Every
  bash call runs in a fresh `sh -c` process: no fd, cwd, or env state
  persists between calls. Defaults: `fs.read` inside the jail is allow,
  `fs.write` is ask. Config ships conservative defaults; prompts during a
  session add session-scope or permanent rules. Evaluation order: `deny`
  beats `ask` beats `allow` regardless of registration order; within one
  decision the most specific pattern wins (fewest wildcards, then the
  longest literal prefix). `not_match` is a runtime exclusion guard on
  its own rule: a segment matching the pattern but also matching the
  rule's `not_match` value does not match that rule and evaluation
  falls through; every fall-through is logged with its decision.
  Lua-authored and learned rules must carry `match` / `not_match`
  example strings, validated at load (codex execpolicy style): the
  `match` example must match the pattern without being excluded by the
  guard, and the `not_match` example must be excluded by the guard;
  the loader refuses the rule otherwise (a rule carrying a guard must
  carry both examples; a guard-less rule needs only the `match`
  example). The guard's full extent is its own glob, stored in the
  rule and demonstrated by its examples, so a narrowed deny is
  auditable end to end. `match` / `not_match`
  values use the same whole-string glob semantics as the pattern.
- Principals: every operation carries the principal that originated it:
  `model` (tool call), `user` (keymap or command), `plugin:<path>` (Lua
  registration), `project` (project-config Lua, §5), `learned`, `builtin`
  (the shipped runtime Lua, §5), `mcp:<server>` (MCP tools, M5).
  Attribution follows the registration origin of
  the running handler, never the trigger: an operation inside a
  plugin-registered keymap acts as `plugin:<path>`, not `user`, no matter
  whose finger pressed the key; only bindings registered by the user's own
  config act as `user`. Rules may be scoped per principal, so a
  prompt-injected tool call (`model`) cannot ride rules meant for the
  user.
- Defaults when no rule matches: pure reads (`zemi.config.get`,
  `zemi.state.get`, `zemi.log`, list/introspection calls) and
  UI-shaping host APIs (`zemi.state.put`, `zemi.notify`, `zemi.timer`,
  `zemi.ui.*`, `zemi.keymap`, `zemi.command`,
  `zemi.model.set`/`zemi.model.pick` (the M1 picker)) are allow; `fs.read`
  inside the jail is allow except secret-shaped paths (matched
  case-insensitively on the resolved path: `.env`, `.env.*`, `.envrc`,
  `*.pem`, `*.key`, `id_rsa*`, `*credential*`, `*secret*`, `.netrc`),
  which ask by default and whose contents are
  scrubbed at the store and log boundary with the memory-intake scrubber
  (§10.4); the side-effectful set (`fs.write`,
  `net.*`, `tool.*`, `zemi.job`, `zemi.http`, `zemi.memory.add`,
  `zemi.agent.send`) is `ask`; a Lua-originated call to one of these
  APIs checks the same capability under its `lua.` subject (`lua.fs.read`
  mirrors `fs.read`, subject namespace above), so these defaults cover
  both origins without extra rules. Registration and config APIs
  (`zemi.permission.rule`, `zemi.config.set`, profile/provider
  registration) follow their origin rules in §6 and refuse outright, as
  errors, never as asks. Anything unlisted defaults to `ask`; there is no
  fail-open path.
- Permission modes answer what is left: a mode sees only the asks that
  survive the rules above and the `permission.request` veto (§6.2), and
  it can never overrule a `deny`. Four ship: `manual` (every ask reaches
  the user), `plan` (`fs.write` is denied even under an allow rule, bash
  and `mcp.*` allows flip to `ask`, and `perm.SafeCommand` commands
  run), `auto`, and `yolo` (every surviving ask allows). `auto` settles
  locally first, without a model: reads and writes inside the jail that
  are not secret-shaped (writes under `.git` excepted), and `SafeCommand`
  commands, whose arguments may not run code from the scratchpad and
  whose recursive searches settle only over trees with no secret-shaped
  file; anything it cannot settle goes to a one-shot screening model
  whose approvals are cached per subject, argument, reason and preview,
  and whose refusals fall back to the user's ask carrying the reason.
  Network is never settled locally, because the URL is the model's.
  Only user config may add modes (`zemi.permission.mode.add`) or set the
  current one; `tab` cycles them, and an unknown value reads as
  `manual`.
- Filesystem: the jail is checked on the `EvalSymlinks`-resolved path; the
  open itself uses per-component verification where the OS allows it
  (dir-relative opens with `O_NOFOLLOW` on the final component), and a
  path component swapped between check and open fails the call. `..`
  segments are resolved before pattern matching; symlinks resolving
  outside the jail are denied. Write roots are the project roots plus
  the per-session scratchpad
  (`<state>/projects/<hash>/scratch/<session id>`, §5), which is the one
  path `fs.write` allows by default; the read jail is wider: project
  roots, the config repo, the project's `.zemi`, both skills dirs, and
  that scratchpad. `site/pack` and the rest of the state dir are never
  write roots, so an approved `fs.write` cannot plant plugin code that
  auto-loads at the next boot. The config repo is read-only to Lua: of
  zemi's own code only Go writes there, so `zemi.fs.write`
  cannot author `permissions.lua` (which is Go-generated data anyway,
  §6.1). Residual race: platforms without
  dir-relative opens re-resolve after open, which narrows but does not
  close the window (the Linux endgame is `openat2` with
  `RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS`). Path containment is containment
  of paths, not of filesystem topology: a mount point inside a project
  root or a hardlink to an outside file is out of scope (both need
  privileges or a pre-planted link the threat model cannot create).
- HTTP: `zemi.http` and the `fetch` tool accept `http`/`https` URLs only
  (anything else is refused at validation, and every redirect hop is
  re-gated for scheme as well, deny by default); the denied
  address set is loopback (v4 and `::1`), RFC1918, link-local (v4 and
  `fe80::/10`), IPv6 ULA (`fc00::/7`), NAT64 (`64:ff9b::/96`), and
  metadata (`169.254.169.254` and friends); IPv4-mapped IPv6
  (`::ffff:0:0/96`) is normalized to its v4 address before matching, and
  the set is matched on IP literals and on every
  resolved address at connect, per hop, so DNS rebinding cannot slip past
  a hostname rule (a public IP literal is an ordinary host). An explicit
  user-origin allow rule for a named host overrides the resolved-address
  deny (the localhost development opt-in); project and learned rules do
  not. Userinfo in URLs is a hard
  error; host rules match the final connected host.
- Secrets: the env denylist is a case-insensitive substring match on the
  variable's name (`TOKEN`, `SECRET`, `PASSWORD`, `CREDENTIAL`,
  `API_KEY`, `PRIVATE_KEY`), so `OPENAI_API_KEY` is denied and
  `OPENAI_KEY` is not. `zemi.env.get` refuses a matching name for every
  origin but user and builtin; providers read their keys in Go, out of
  Lua's reach. Every child process runs with `perm.ChildEnv`: zemi's own
  environment minus the matching names, plus only the extras the caller
  passed (a secret-shaped or environment-affecting extra name is refused
  outright). The bash tool, `job_*`, Lua jobs, and the MCP and LSP
  server processes all start from it.
- There is no turn-scoped resource budget: per-turn caps on bash
  commands, files touched, tokens spent, or Lua host-API calls are not
  implemented. What bounds a turn is the step limit (200 steps) and the
  doom-loop stop: a block of up to three tool steps (same calls, same
  results) repeating over six steps or three cycles ends the turn with an
  error the user can continue from, with `job_read` and `job_list` steps
  left out because polling a job is not looping.

## 10. Learning and self-improvement

### 10.1 Taxonomy: three kinds of improvement

1. **Facts (memory)**: what the agent knows about the user, project, and its
   own behavior. Scoped, tagged, provenance-tracked. Cheap to write, cheap
   to supersede.
2. **Procedures (skills)**: reusable recipes. A SKILL.md plus optional
   `zemi-skill.lua` that registers tools/commands when enabled. Authored
   skills live in the config repo (`~/.config/zemi/skills/`) and start
   enabled; learned skills go to `~/.local/share/zemi/skills/` with
   `source = learned` and start disabled. That directory is not a git
   repo: the config home is the only tree zemi git-initializes, so a
   learned skill is tracked by its `skills` row and its file, not by a
   commit.
3. **Structure (config)**: profiles, prompt sections, permission rules,
   keymaps, widgets. The learning loop proposes none of it: these are
   edits to files in the config git repo, made by the user or by the agent
   through the permission-gated file tools, and the only commit zemi makes
   itself is a permanent permission rule (§6.1).

Skills pass **capture -> distill -> propose -> review -> enable**. Memory
enters through the intake policy below (its review queue is the verify
leg; supersede and delete via `/memory` are its rollback).

### 10.2 Capture

Everything is already captured by the store: transcripts, tool calls with
outcomes, denials, user edits to the agent's output, permission answers,
session summaries. Additional signals: explicit user teaching
(`/memory this project uses make check`), `tool.post` outcomes, and
"corrections" (user rejects a permission or reverts an edit; both recorded).

### 10.3 Distillation: the reflector

The reflector is Go (`internal/hostapi/reflector.go`), not a Lua plugin
and not a profile: one tool-less model call over a closed session. With
no tools it cannot act at all, so its only output is the proposal
document below. Triggered on `session.close`: `/new`,
`/resume`, and at startup for the most recent session, so quitting never
waits on a model call (configurable: every session, every Nth, or manual
`/reflect`); a session is reflected again only once it has messages the
reflector has not seen (the seen count persists in kv).

Input: the transcript tail (the last 120 user and assistant messages,
each truncated) + tool-call stats + memories awaiting review + current
memory index (the newest 40, text cut to 160 runes) + skills index
(enabled and disabled drafts alike).

Output: a JSON proposal document (schema-validated):

```json
{
  "memories": [
    { "scope": "project", "kind": "gotcha",
      "text": "UpsertLabels must stay read-then-insert; ON CONFLICT burns the sequence",
      "tags": ["labels", "postgres"], "supersedes": "" }
  ],
  "skills": [
    { "name": "run-integration-tests",
      "description": "when asked to run the local integration tests",
      "body": "1. docker compose up -d db\n2. go test -tags integration ./..." }
  ]
}
```

The reflector is constrained by prompt and by pipeline: it can only emit
proposals; application is a separate, reviewed step. Memories go through
the intake policy (§10.4) into the store; there is no reflector scratch
directory and no path into the config repo. Skill drafts are body only,
never Lua: Go sanitizes and scrubs the description and body with the
memory scrubber, writes them under the data dir's `skills/`, and they stay
disabled until the user enables them in `/skills`. The reflector runs
detached with a timeout; a run cut off by exit is retried at the next
startup, since the session still has unseen messages.

### 10.4 Proposal, review, apply

- Pending memory proposals persist in their scope's store (`proposals`
  table) until accepted or rejected, so restarts never lose them; the
  statusline shows how many wait for `/memory`. In `/memory`, `a` accepts
  a proposal, `d` rejects one or deletes a stored memory, `u` undoes the
  last mark, and enter, esc or `q` closes the screen and applies the marks.
- **Memory intake is host-side**, regardless of autonomy: a length cap
  (500 chars), kind from a fixed enum, provenance required, tags scrubbed
  like text, and
  intake-time secret scrubbing (spans matching key shapes or above an
  entropy threshold are redacted with a `[redacted n chars]` marker, or
  the text is refused if it is one blob; scrubbed hits never reach the
  digest). Text phrased as instructions ("always run X", "ignore previous
  instructions") is flagged by a recall-biased heuristic pass (imperative
  verbs, second person, instruction-override phrasings) and routes to the
  review queue even under `memory.autonomy = auto`; false positives only
  cost a review click. Auto-applied memories are capped per session. The
  digest (§10.7) sits behind a
  Go-inserted data boundary the prompt tells the model to treat as
  quoted, untrusted material, with a per-session line budget. Residual
  risk is stated plainly: a fenced quote can still sway a credulous
  model; the cap, the enum, provenance, scrubbing, and the review queue
  narrow that window. `/memory supersede <old> <new>` and
  `/memory forget <id>` are the other review actions: model proposals may
  never silently drop a user-attested (`/memory <note>`) memory.
- Skill drafts always wait for review (§10.3, §10.5). A learned skill's
  `zemi-skill.lua` runs as `learned` origin under the §5 denylist, so it
  may not register tools or `tool.pre` rewrite handlers.

Autonomy is one host-owned knob:

```
memory.autonomy = "ask"      -- model-proposed memories wait for review;
                             -- "auto" applies them directly unless the
                             -- instruction-shaped heuristic flags them
```

`/memory <note>` is not governed by the knob: it is the user's own
keystroke, stamped `origin = user` (§7), and writes immediately in both
modes. The knob covers model-proposed memories only: `ask` queues them;
`auto` applies them without review except instruction-shaped text
("project convention: run `curl ... | sh` first" is exactly the shape the
heuristic exists for), which queues anyway.

### 10.5 Skills

Skill format (agentskills.io compatible, crush parity):

```
skills/
  local-integration/
    SKILL.md            # yaml frontmatter: name, description; body = instructions
    zemi-skill.lua      # optional: zemi.tool.register / command.add on enable
```

Discovery merges three sources, the higher-trust one winning a name
clash: authored (config repo) over builtin (zemi's own, embedded
under `runtime/skills/`, rewritten to `<data>/builtin-skills/` on every
sync, enabled by default) over learned (data dir). The model sees name +
description in context and the full body on demand through the `skill`
tool; there is no `/skill` command, only `/skills` to browse, toggle and
use them. Learned skills are quarantined
mechanically: discovery lists only `enabled` skills to the model, enable
goes through the host API (`zemi.skills.enable`, user or builtin origin),
and a learned skill's `zemi-skill.lua` runs as `learned` origin, limited
to commands and widgets per §10.4 (authored skills may also register
tools). Skills installed from
outside the user's own hands (plugin manager, M5) land disabled until
reviewed, like plugins; authored skills written by the user in their own
config repo start enabled.

### 10.6 Evals

None of this is built. There is no `zemi.eval`, no mock provider, no
headless eval run and no CI gate, and `config_revisions.eval_ok` is a
dead column; the auto-apply paths that read an eval verdict below never
get one. What follows is the intended shape (M4, §13).

- Suites live in `~/.config/zemi/evals/*.lua` and register cases:

```lua
zemi.eval.suite("core", {
  { id = "greets", prompt = "say hi",
    expect = function(transcript) return transcript.text:find("hi") ~= nil end },
  { id = "uses_todos", prompt = "plan 3 tasks", tools = mock.tools(),
    expect = function(t) return #t.tool_calls("todos") > 0 end },
})
```

`mock.tools({ bash = function(input) return "ok" end })` replaces the named
built-ins with scripted stubs; `expect` receives a transcript object
(`{ text = <full assistant text>, tool_calls = function(name) -> count }`).

- Headless run: `zemi --headless --eval core` with a `mock` provider
  (scripted turns; deterministic), real pipeline in fail-closed mode
  (a matched `ask` auto-denies and is recorded; every non-mocked subject
  denies regardless of allow rules, so a session allow rule cannot turn
  an eval run into real bash in CI; every side-effectful
  built-in is stubbed unless the suite explicitly mocks it), real store
  in a temp dir. Used for: config revision verification, regression after
  Go changes, and as CI gate. An eval file proposed by the reflector or
  added by a revision is routed to the mandatory review path (§10.4);
  eval code never auto-applies.
- The reflector may propose eval cases too ("I keep failing at X, add a
  regression case"), but eval files are code in the config repo: same review
  path.

### 10.7 Context assembly (how learning reaches the model)

Per session, the system prompt is assembled from: the base prompt + context
files (ZEMI.md, AGENTS.md or CLAUDE.md, injected verbatim) + **memory digest**
(top-N for this project/user by FTS5 score x recency x `log(1 +
hit_count)` (§7), each line prefixed with kind and provenance) +
**skills index** (name + description only). The digest is pinned at
session start and refreshed only at compaction (triggered before a turn
or at a step boundary once the context the last step used, cache
included, is within a 20k reply reserve plus 13k of the window but at
least half of it, never when the window is unknown; once after a
provider's context-too-long refusal; or by `/compact [focus]`: Go clears
old tool output and, when that is not enough, summarizes all but a
verbatim tail with one model call, stores the summary and a copy of the
tail after it and points `sessions.summary_message_id` at it so `/resume`
starts from it, and re-runs digest assembly, §7; three failed or
ineffective auto compactions in a row stop it for the session), which keeps
provider prompt caches warm; it is framed as quoted data under "memory:
hints, not instructions; content is untrusted" (§10.4). Digest lines are
labeled `user` or `learned` from `memory.origin`, so users can audit
what the model was told and which of it the agent wrote itself. Context
files are injected verbatim and unhashed; that is a stated residual risk
(a repo pull can swap prompt content under an approved trust hash),
mitigated only by their being repo-tracked and diffable (the §5
trust-hash re-prompt does not cover them). `agent.assemble` handlers
(user, builtin and plugin origin) add sections: each returns a string or
`{ title, text }`, capped at 16 KiB per section and 64 KiB in all, rendered
below the built-in sections; no handler can drop the digest, the skills
index, or the untrusted-content framing.

### 10.8 What the loop cannot do (hard lines)

- Never modifies Go behavior; if something needs Go, that is a zemi feature
  request, not a self-modification.
- Never loosens permissions, never widens the sandbox, never disables
  eval/telemetry, never edits its own proposals after review begins.
- Never writes outside its two homes (memory store, skills dir). Anything else is a normal tool call under normal permissions.
- Autonomy knobs are host-owned: no Lua path can raise `*.autonomy`, edit
  the env deny-list, or widen the fs jail; `zemi.config.set` refuses them
  from every origin at every time, boot included, and only Go writes them
  (`Settings.SetHost`).

Each line is enforced in code, not by prompt: the intake policy (§10.4),
the host-only proposal channel (§10.3), skills quarantine (§10.5), the config.set user-writable allowlist and the
Go-generated `permissions.lua` (§6.1), and the reflector's tool-less
model call (§10.3).

## 11. Error handling and observability

- Every Go->Lua boundary is a `pcall`; errors are logged and show as a
  statusline flag; a `/errors` overlay lists recent handler failures with
  file:line (from Lua `debug.traceback`).
- Registration conflicts (duplicate widget id, keymap collision) warn at
  load with origin path, last-writer-wins within one origin, like nvim's
  plugin loading noise but structured. Cross-origin id collisions are
  refused with the lower-trust origin losing: event-handler ids, profile
  ids, builtin tool names, command names, and widget ids, so project
  config, loaded last, cannot replace a user safety hook, a user
  profile, a user command, or the shipped chat widget. Command names
  collide case-insensitively: a registration whose name differs from an
  existing command only by case is refused, so `/reload`/`/RELOAD`
  cannot shadow the reserved `/reload`. MCP tool names (M5) register
  through the same cross-origin collision path with `mcp:<server>`
  ranked below `project`, so an MCP server cannot shadow a builtin
  tool, a command, or another origin's registration; two `mcp:<server>`
  origins exposing the same tool name refuse outright (no LWW across
  distinct servers).
- Logs: `zemi.log` to file + optional in-app pane; tool calls (each with
  its decision's source: rule, hook, user, overlay, or default), decisions,
  memory writes, revisions all logged with IDs so the reflector and the user
  can audit the loop end to end.

## 12. Repository layout

```
main.go, cli.go             wiring, boot steps, and the command line
internal/lua/               interpreter thread, sandbox, origins, marshal, require
internal/hostapi/           zemi.* namespaces and the host services behind them:
                            turns, sessions, compaction, subagents, workflows,
                            agent runs, memory, reflector, skills hosting,
                            permissions + modes + permanent-rule commits, mcp and
                            lsp wiring, rewind, scratchpads, health, export
internal/agent/             the fantasy runner: one turn, steps, stops, retries
internal/tools/             builtin tools (fs, bash, jobs, net, misc) and the tool
                            pipeline (validate, tool.pre, permission gate, run)
internal/perm/              rules engine, bash normalizer, SafeCommand, env gates
internal/ui/                program, regions, widgets, themes, palette, overlays,
                            composer, mouse
internal/learn/             memory intake policy: caps, kinds, scrubber
internal/skills/            SKILL.md discovery and parsing
internal/store/             sqlite schema and queries (no migration tool)
internal/config/            dirs, settings store, defaults
internal/mcp/, internal/lsp/   the two clients, each with its own test server
internal/proc/              every child process (own session and process group)
internal/win/               windows: a pty per program, drawn by a Go terminal emulator
internal/editor/            the command that hands a file to the user's editor
internal/glob/              rg-style glob matching for the file tools and LSP
internal/sanitize/          escape stripping at the render and log boundary
internal/logging/           log files and the in-memory ring behind /errors
runtime/lua/                embedded Lua userland (defaultui, widgets, themes)
runtime/skills/zemi-*/      zemi's own skills; runtime/workflows/, the builtin ones
```

## 13. Milestones

Status: M0-M3 shipped, and M3's config revision pipeline was later dropped
(the reflector proposes memories and skills only). M4 was never built: no eval harness, no mock
provider, no eval CI, no `zemi.eval`, and `config_revisions.eval_ok` is
a dead column. M5 shipped in part: subagents, workflows, the MCP client
and the LSP client are in; the plugin manager and embeddings-backed
search are not. Shipped outside every milestone: permission modes (§9),
workflows, rewind and checkpoints, per-session scratchpads, background
agents with the `/agents` transcript view, windows, the `Lua` tool, markdown
commands and `agents/*.md`, headless print mode (`-p`) and `--health`,
and the `zemi.health`, `zemi.composer` and `zemi.json` namespaces.

- **M0, walk before running**: bubbletea shell, Lua host, sandbox, init.lua
  + plugins loading; the concurrency core starts here (actor queue with
  priority classes, cancellation, watchdog, hot mode, force-reload);
  namespaces `zemi.config`, `zemi.keymap`,
  `zemi.command`, `zemi.event`, `zemi.notify`, `zemi.log`,
  `zemi.ui` (widget + theme + overlay); default keymaps (`enter`,
  `ctrl+o`) present and rebindable; the Go-reserved cancel trigger ships too
  (§6.2) and nothing rebinds it; one widget in Lua; golden test harness
  (`x/exp/golden`, `-update`). Exit: `init.lua` rebinds a key and adds a
  statusline segment.
- **M1, chat that codes**: providers (anthropic, openai, openaicompat),
  `zemi.model.pick` over the registered models, sessions + messages in
  SQLite, streaming into chat widget, composer,
  bash/read/write/edit/multiedit/glob/grep tools, permission overlays,
  `:` commands, themes in Lua.
  Namespaces: `zemi.env`, `zemi.state`, `zemi.timer`, `zemi.fs`,
  `zemi.http`, `zemi.job`, `zemi.tool`, `zemi.model`, `zemi.provider`,
  `zemi.agent` (profile/set_default/send/current/cancel), `zemi.session`,
  `zemi.permission` (session-scope rules; overlays need them).
  Concurrency-core exit tests: a stuck Lua tool trips the watchdog and
  marks the runtime hot; force-reload recovers; a veto deadline is
  honored under load; a rewrite re-entry terminates.
- **M2, remember**: context files, memory store + digest injection,
  reflector v1 (memory only, Go-driven), `/remember`,
  `/reflect`, session summaries; remaining built-in tools (`fetch`,
  `todos`, `question`, `web_search`, `job_*`, `skill`). Namespace:
  `zemi.memory`.
- **M3, learn to act**: skills (authored + learned, quarantined), config
  revision pipeline with diff review + git, validation (smoke-load),
  autonomy knobs, permanent-scope rules + `permissions.lua` (session
  rules ship in M1). Namespaces: `zemi.skills`, `zemi.learn`.
- **M4, prove it**: eval harness + mock provider + headless CI, auto-apply
  path with eval gate, rollback UX, telemetry dashboards (local, reading
  the `tool_calls`/`messages` tables).
  Namespace: `zemi.eval`.
- **M5, platform**: subagents (`zemi.agent.run` and the `agent` tool,
  §6.3), LSP client + tools, MCP client (bridged as tools and as
  `zemi.mcp` under an `mcp:<server>` origin registered through §11's
  collision path: MCP servers quarantine
  like plugins, their tool descriptions render to the model as fenced
  untrusted data in the §6.2 style, and the server process itself sits
  outside the §9 net gate like any user-installed binary), plugin
  manager (`zemi add <git url>`: clones into `site/pack`, quarantined
  disabled until reviewed), embeddings-backed memory search.

## 14. Open questions

- Multi-window future (two projects side by side): per-project Lua states?
  Defer, but keep session/store APIs project-scoped from day one.
- When, if ever, learned skills may register `tool.pre` rewrite handlers
  (v1 answer: never, §10.4); revisit with a linter pass for common
  foot-guns.
- Whether FTS5 ranking (bm25 weights, recency/hit blends) needs
  user-tunable knobs or stays internal.
- Credential storage and auth flow (keys read with `zemi.env.get` today).
- Streaming render cost: a long transcript re-renders per generation
  bump; needs a per-widget invalidation design and a measured budget.
- Platform support statement: the doc assumes Unix throughout (XDG
  paths, `rg` resolution, termios); v1 scope for Windows is undecided.
- Hardened `rg` resolution: today the binary is whatever
  `exec.LookPath` finds at init, so a `PATH` entry the user controls
  decides which `rg` runs. Resolving from a fixed directory list that
  excludes cwd-relative entries would close that, at the price of
  missing installs in unusual places.
