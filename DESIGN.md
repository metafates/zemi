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
reviewed pipeline.

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
3. **Configuration is code.** `~/.config/zemi` holds Lua and markdown the
   user owns; zemi never commits to it, and versioning it is the user's
   choice. Structural changes are file edits, reviewed as diffs through the
   permission gate.
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
EXISTS` stamped with `PRAGMA user_version`; no migrations before the
first release, so no migration library came with it), `charm.land/glamour/v2`
(markdown in chat, with `alecthomas/chroma/v2` for the code block
formatter that marks glamour's own margins), `charmbracelet/x/exp/golden` (golden-file assertions
for TUI output), `aymanbagabas/go-udiff` (edit previews), `creack/pty`
(sandbox tests), `sahilm/fuzzy` (the one list filter, §6.6), and
`go-readability` with `html-to-markdown` (the `fetch` tool: the article
out of a page, as markdown the model can read). Tool input validation is a small hand-written
schema check, not a JSON Schema library. `charm.land/catwalk` is taken
for its embedded provider catalog and its client (§6.3): no network at
boot, an update only when the user asks, the user's own
`zemi.provider.add` on top. Not taken:
`charm.land/bubbles` (the composer is zemi's own editor, §6.6) and
`fsnotify` (config reload is a one-second fingerprint walk of the config
trees). Nor is ripgrep: `glob`, `grep`, `@` completion and `zemi.fs.glob`/`grep`
share one lister (`tools.ListFiles`), `git ls-files --cached --others
--exclude-standard` through the hardened `proc.Git`, so the `.gitignore`
files decide, and a Go walker that skips hidden and build directories by
name where git cannot list (no repository, no git). `grep` searches that
list in parallel with Go's RE2 `regexp`, in path order, so the same call
returns the same lines.

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
- **Learn** (`internal/learn`): the memory intake policy (caps, provenance,
  the secret scrubber), the near-duplicate check and the digest. The
  reflector and the skill manager live in `internal/hostapi`.
- **Store** (`internal/store`): SQLite (sessions, messages, memory, skills,
  checkpoints, proposals, config revisions, telemetry), one schema const; a
  schema change bumps `schemaVersion` and an older store is refused, to delete.

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
- **`wait()` suspends a task; anywhere else it raises.** A task is a
  callback zemi runs as a coroutine (`lua.Task`): a Lua tool's `execute`,
  Lua command functions, keymap functions, overlay callbacks (`on_select`,
  `on_action`, `on_close`, `keys`, `reload`), the `on_done`/`on_exit`
  callbacks of jobs, HTTP, model asks, agents, workflows and
  `zemi.ui.edit`, `tool.post` handlers, a permission mode's `decide`, and
  a chunk run through `Runtime.Eval` (the `lua` tool). A handle's `wait()`
  (and `zemi.job.wait`) yields the task: the request running it ends, the
  interpreter serves everything else, and a host goroutine waits on the
  handle, then queues the next slice as an ordinary request of the task's
  class, with the task's origin frame pushed again and its thread bound to
  that request's context. So the watchdog, the class priorities and ctx
  cancellation hold per slice, and a slow plugin cannot freeze keys or
  rendering by waiting. Everything else stays a plain call, so a per-delta
  event allocates no thread: widget renders, event handlers (vetoable ones
  included; `tool.post` aside), completion sources, health checks,
  `agent.assemble`, config watches, timer and `on_stdout`/`on_stderr`
  callbacks; `wait` raises there, and under `pcall` or a library callback
  such as `table.sort`, whose Go frame a yield would unwind, and in a
  coroutine the task made. Only the driver resumes a task: its thread
  stays registered until the task ends and `coroutine.resume` raises for
  it, so no Lua holding the thread can run the task's rest with a forged
  wait result. A task whose generation was purged or went hot
  ends at once, its wait cancelled with it: a dropped `execute` returns a
  tool error, a dropped `decide` sends the ask to the user, a dropped
  `tool.post` leaves the output as it was. Keymaps and Lua commands run in
  order on the UI's worker per slice: a suspended one lets the next key's
  handler run, and its rest reports its own failure (`-p` waits for a
  whole command). An overlay's `reload` holds the worker until it returns,
  since it hands back the items.
  NOTE: every task gets a fresh thread (~100 KB of garbage); a pool is the
  upgrade path.
- **Queue priority.** Requests carry a class: `ui` (keymaps, renders,
  events), `agent` (tool calls), `io` (timer/job/http callbacks, reloads).
  The queue drains `ui` first, then `agent`, then `io`, FIFO within a
  class. Priority orders queued work only; a running request is preempted
  only by cancellation (above), which is why handlers must be fast.
- **Watchdog.** Timers arm at execution start, not at enqueue: queued
  time is unbounded by the watchdog (a queued callback behind a long tool
  waits, it is not canceled pre-execution). A running Lua tool uses the
  tool's `timeout_ms` per slice and as its wall time; ui/io handlers get
  the ~10 ms soft budget plus a hard ceiling (1 s) per slice. On expiry
  the request's context is cancelled (the catchable error surfaces as a
  failed handler) and the async handles it
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
  in force until the replay succeeds (a failed one keeps the deny and ask
  ones; then those it did not add again are dropped), and `tool.pre` and `permission.request` vetoes wait for a
  running reload, so no tool call is checked against half a config.
- **Vetoable events never block.** `permission.request`, `app.quit`, and
  `composer.submit` handlers must answer without waiting on async work. At
  the 50 ms deadline the host cancels the handler's context (the
  catchable error fails it) and takes the event's default decision,
  logged: `permission.request` defaults to deny for asks without an
  overlay leg (fail-closed, §9),
  `app.quit` and `composer.submit` default to proceed (a wedged plugin
  must not hold the user's quit or a typed message hostage; a plugin or
  builtin `app.quit` denial is honored every time, and project handlers
  cannot deny it at all); the other
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
  holds the queue only while its Lua runs: its I/O goes through handles,
  and a `wait` on one suspends it (above). Its `timeout_ms` bounds the
  call in wall time, waits included. A stuck handler
  fails the tool call with an error and marks the runtime "hot";
  force-reload is the Go-reserved `/reload` command plus the Go-reserved
  cancel trigger, never routed through Lua (while hot, every Lua keymap is
  dead; the reserved cancel and reload triggers are not, they dispatch
  before any Lua-visible keymap and are fixed in v1). Reloading builds a
  fresh `LState`:
  new requests run normally, requests that arrived while hot got the
  "reloading" error immediately, stale timer/job callbacks are dropped and
  logged, and replay is safe because all registrations are declarative.
- **Widget render is pure.** `render(ctx)` may call only the read-only
  members (`luaReadOnly`) other than `zemi.fs.*` and a handle's `wait`;
  the sandbox guard (`guardLua`) raises on any other call, naming it
  (caught, logged, widget keeps its last-good lines). Go caches rendered
  lines keyed by `(widget id, width, height, generation)`; the generation
  is a global frame counter bumped at most once per renderer tick, so in
  v1 "invalidated" means any generation change: each visible widget
  renders once per frame and the cache only dedupes repeat `View` calls
  within a frame (per-widget dependency invalidation is a later
  optimization, §8).
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
  takes the queue back for editing; a second `esc` within 2s or `ctrl+c`
  cancels the turn and returns the queue to the composer.

Cancellation: `ctrl+c` (the cancel trigger, §8; the action is
Go-reserved, §6.2, and in v1 its trigger is fixed: no origin, the user
included, rebinds it) cancels the running
turn by cancelling the fantasy
context; in-flight Lua tool calls get the catchable "context canceled"
error at the next opcode (see the watchdog above), and one suspended in a
`wait` ends there, killing a `zemi.job.wait` command. Every
request-ctx cancellation (watchdog expiry, veto deadline, turn cancel,
hot mode) cancels the async handles that request started, inside Go,
which is what makes those terminate.

## 5. Lua runtime and plugin layout

Directories (XDG):

```
~/.config/zemi/           config
  init.lua                entry point, runs on every start
  lua/**/*.lua            user modules, require()-able
  skills/<name>/          user skills (enabled by default)
  agents/<name>.md        subagent definitions (§6.3)
  commands/<name>.md      markdown slash commands
  workflows/<name>.lua    saved workflow scripts
~/.local/share/zemi/      data
  plugins/installed/<name>/  plugins zemi cloned (git); init.lua loaded, lua/<name>/ require()-able,
                          skills|agents|commands|workflows/ as in the config dir
  plugins/local/<name>/   plugins developed in place, the same shape; zemi writes only their .luarc.json
  plugins/disabled        names turned off, one per line
  builtin-skills/<name>/  zemi's own skills, rewritten from runtime/ on sync
  drops/                  files dropped in from outside the project (@drop:<name>)
  plans/<session id>.md   plans approved from a read-only agent
~/.local/state/zemi/      state
  zemi.db                 global store (user memory, skills, kv, revisions)
  scratch/<id prefix>/    per-session scratchpad, every model command's $TMPDIR (§9 write root)
  projects/<hash>/zemi.db per-project store, hash of the trust root (sessions, ...)
  projects/<hash>/skills/<name>/SKILL.md  recipes: the project's private skills (§10.5)
  projects/<hash>/permissions  bash commands always allowed in the project (§6.1), read only when trusted
  trust.json              trust decisions by repository root
  logs/
~/.agents/skills/<name>/  user skills other agents read too, body only (§10.5)
<repo>/
  .agents/init.lua        project-local config, loaded after user config
  .agents/lua|skills|agents|commands|workflows/  the same shapes
  .agents/worktrees/      agent worktrees, ignored by its own .gitignore (§9)
  AGENTS.md, else CLAUDE.md   context files, per directory
```

Everything under `<repo>/` loads only in a project the user trusts.
The decision is keyed by the repository root with symlinks resolved, and
a linked worktree maps to its main checkout by reading the `.git` file,
the `gitdir` back-pointer and `commondir` (git is never run to find it);
outside a repository the key is the working directory. The search for the
root stops below `$HOME`, so a dotfiles repository there is not the root
of every directory in home. Boot asks once per
process when no decision is saved, whether or not `.agents` exists. The
prompt shows the root `~`-shortened, what trusting loads and runs
(scrolling on a short screen), and three buttons with the cursor on the last: `y`
saves trusted, `n` saves untrusted, `l` (Later), `esc`, `ctrl+c` or an enter typed by
habit means untrusted until the next start (a reload does not ask again); up and down
move the cursor, enter confirms it, a click answers a row. `--trust` or `--trust=false`
decides one run without saving; a headless run with nothing saved is
untrusted and says so on stderr. The home directory and `/` are never
asked about or saved (their prompt says it applies to this run only).
`/trust` reopens the prompt at any time, showing the current decision (Later keeps
it), saves the answer, applies it
before the reload it triggers when the decision changed, so a revoke holds
even if that reload stops at an earlier failing step. When a sandboxed
command creates a protected path that did not exist when it started
(`.agents`, `.git`; only the Linux namespace backend lets it), trust is held
off for the process until `/trust` is answered, without saving: the tool
result and a notification name the path, and the config watcher and the
trust step check running commands before they read project config. There is no content hash:
once the user lets auto mode run a repository's commands,
re-asking about a pulled `.agents/init.lua` protects nothing, and project
origin is denylisted anyway (below).

Trusted, zemi loads the project's `.agents` config and instruction files
(and a profile's `context.files`), starts language servers (builtin and
user-added; sandboxed when a backend is live, §9), auto mode and read-only agents settle builds and tests in the sandbox, and
auto settles in-project edits (§9). Untrusted, none of that: only the user's `<config>/AGENTS.md`
is an instruction file, no language server starts, `.agents` edits trigger no reload, and auto mode and read-only agents
settle only reads and `perm.Normalized.ReadCommand` commands. Either way zemi's own
git calls (`proc.Git`) switch off `core.fsmonitor`, hooks, submodule
recursion, LFS smudging, transports other than https, ssh and file, and
repository filter drivers: a sandboxed command can plant a nested
repository whose config would otherwise run unconfined on zemi's next
`git status`. A plugin clone is someone else's repository, so its git
calls blank the user's global filter drivers too.

Load order is a list of boot steps: host `zemi.*` namespaces
-> builtin commands (main's own plus every domain
file's: memory, reflector, skills, agents, workflow, mcp, lsp) -> the
shipped Lua UI (`require("zemi.tui")`) -> the project trust
decision (the prompt above; before any config that may start an MCP
server) -> plugins (each enabled plugin's `init.lua`, sorted by name) ->
user `init.lua` -> user skills' enable hooks -> a trusted project's
`.agents/init.lua` -> markdown commands (user, then each plugin's, then a
trusted repo's `.agents`) ->
`app.ready`. Boot runs on its own goroutine, so it may block on the
trust prompt without holding the UI. Agents and workflows are read when
used: the user's, then the builtin ones, then each plugin's, then the
project's under the current decision; the first of a name wins. Project config loads late with
flagged reduced trust: its registrations are tagged `origin = "project"`
(shown in the UI and logs), its permission rules may only tighten (deny or
ask rules and `not_match` guards; allow rules are user-config-only), and
its capability denylist is mechanical: no `tool.pre` rewrite handlers, no
`zemi.tool.override` (same rewrite capability, same denial), no
`zemi.provider.add`, no `zemi.agent.profile.add` and no `agent.profile`
config write (prompt authorship is a stronger lever than any
`tool.pre` rewrite), no `agent.send`, no `agent.assemble` section
injection, no `zemi.memory.add`, no `zemi.model.set` or `zemi.config.reload`,
no `zemi.command.run` of a more trusted origin's command (its body runs
under that origin), no keymap that is a prefix or an extension of a more trusted
binding's key, no `app.quit` denial (ignored), no registration of a builtin tool name
(builtin ids are reserved; last-writer-wins applies only within one
origin; cross-origin profile id collisions are refused, §11).
`load`/`loadstring` stay available to every origin: a chunk they build
runs as the loading origin (its chunk name is re-tagged, so a forged name
does not stick) under the same denylist.

Runtimepath, in order: a trusted project's `.agents` (for project code
only), `~/.config/zemi`, then the one enabled plugin named like the
module's first segment: `require("x.y")` searches only plugin `x`'s
`lua/`, so no plugin shadows another's modules, and plugin `x`'s own code
searches its root before the config dir. The plugin roots are the ones the
plugins step loaded, fixed for the generation, so a disabled plugin's
files never resolve.

The plugins step lists `plugins/local/*` and `plugins/installed/*`, skips
the names in `disabled`, and runs each `init.lua` as plugin origin; it runs
no git. A plugin's skills, agents, commands and workflows come from the
same list, sorted by name, whether or not its `init.lua` ran; the user's
and the builtin ones win a name, a name two plugins ship goes to the first
and `/errors` names both, and a skill a plugin ships never runs a
`zemi-skill.lua`. A plugin that fails does not stop the rest; each one's error goes
to `/errors` and the `/health` plugins row counts it. A local plugin named
like an installed one does not load, nor does a symlinked plugin dir (its
files would resolve outside the plugin class). The step also drops the
`zemi.state` keys of a plugin whose directory is gone or holds another
install: `install` writes a random id into the clone's `.git`, and the key
`plugin:<name>` keeps the one that wrote them. The one-second config
watcher reads `local/` whole, the `disabled` file and each clone's git
index, so an edit, a toggle, an install, an update and a delete each
reload.

`/plugins` and `zemi plugins` manage the tree through `internal/plugins`,
which is plain git and runs no Lua: `install <git url> [name]` clones into a
dot dir, checks out the highest `vX.Y.Z` tag and renames it into place;
`update [name...]` fetches, then moves a tag within its major version, a
branch to the highest tag when that tag is ahead of it, or else a branch
fast-forward, and names only the plugins that moved; `check` fetches, so
the list names each update from local refs; `enable`, `disable` and
`delete` (an installed clone, else the local plugin; a symlink loses the
link only). `zemi plugins` runs without the Lua boot, the TUI or
trust, so a prompt that starts with "plugins" is written after `--`.
`/plugins` is a toggle list built off the key loop, local plugins first,
each row's state first (`failed`, an update), then its version and the
`init.lua` description; its detail names the source, the load status and
what the plugin registered, and the git verbs run on a goroutine that
refreshes the list when they end, unless the user closed it meanwhile. Three
verbs need the screen, so only `/plugins` takes them: `toggle <name>`,
`open <name>` (its `init.lua` in the editor, or its directory without one)
and `new [what it should do]`, the list's first row, which sends the agent
a short prompt pointing at zemi-customize, where the procedure lives.

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
- `./x` and `../x` resolve next to the calling file and stay in its tree:
  its origin class, and for plugin code its own plugin. A plugin's file
  resolves only from that plugin, so neither `..` nor a symlink reaches
  another plugin's code or a disabled one's.
- A module runs once per generation, keyed by its resolved file (a dotted
  and a relative name share it). Code below the file's origin gets its own
  instance run under its own origin, one per plugin, so it cannot plant
  functions that more trusted code or another plugin later calls. A
  builtin module for lower code, and one plugin's module for another
  plugin, is the owner's instance behind a deep read-only view: the
  requirer can neither patch it nor rerun it. What a module keeps in its
  table is readable that way; a secret stays in a local.

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
  `~/.config/zemi/**` is user, `~/.local/share/zemi/plugins/**` is
  plugin, `<repo>/.agents/**` is project, embedded `runtime/**` is
  builtin. A resolved path in no class is refused at load. Origin never follows the requirer: a
  module under `plugins/...` stays plugin origin and a project module
  stays project origin even when `init.lua` requires it, so untrusted
  code cannot ride the boot phase under a user badge.
- The code a call runs as is the least trusted of the frame the host
  entry pushed (a registration's origin) and every Lua function on the
  call stack, the threads that resumed it included; builtin functions are
  transparent, and chunks compile with no tail calls, which would drop
  their caller's frame. A metamethod runs with no frame of the code that
  set it, so `setmetatable` (tables only) lets code below builtin install
  only Lua functions of its own origin or lower as handlers, and plugin
  code only its own plugin's, copied at the call: a Go, builtin, user or
  other plugin's function behind `obj.setup`, `obj()` or `==` would run
  as the code that triggered it, and a plugin module may export no Go
  function at all (`require` refuses the module), since `obj:method()`
  would hand the module table to it as its caller. So a plugin's `setup()`
  that user `init.lua` calls runs as that plugin, a user handler that a project's `zemi.event.emit` runs
  inline runs as the project, and registrations capture the origin and
  file of the code making them. A plugin is named by its directory under
  `installed/` or `local/`: that name scopes its `zemi.state` keys
  (`plugin:<name>:<key>`), its permission principal (`plugin:<name>`) and
  its overlay badge (plugin code with no file of its plugin on the stack,
  such as a user module a plugin required, called from user code, has no
  name, and the calls that need one raise), and two plugins cannot replace each other's command,
  keymap, widget, theme, tool, completion source or health row (the
  first by name keeps it; within one plugin the last writer wins).
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
  (`zemi.fs.read`/`write`, `zemi.state.get`/`set`, `zemi.json.decode`,
  `zemi.job.start`, the memory, learn and skills calls) return
  `nil, err` instead of raising. The split is mechanical: runtime
  failures never raise; misuse (bad arguments, a call a render may not
  make, an origin that may not make the call) and a permission gate
  that does not allow always raise. One exception: `zemi.command.run`
  returns `nil, err` when the caller's origin may not run the command.
- **One verb per job**: `add` registers (`zemi.command`, `zemi.tool`,
  `zemi.provider`, `zemi.health`, `zemi.mcp`, `zemi.lsp`, `zemi.ui.widget`,
  `zemi.ui.theme`, `zemi.permission.rule`, `zemi.permission.mode`), `set`
  writes a value, `get` and `list` read back, `del` removes and reports
  whether it did. A new call takes the verb its shape already has, so the
  surface stays guessable without the type annotations open.
- **Handles vs data**: anything holding a host resource (job, HTTP call,
  model ask, timer, session) is a handle with colon-call methods (`h:wait()`,
  `t:stop()`); `wait()` suspends the task that calls it and raises outside
  one (§4). Everything else is a plain snapshot table; mutating it never
  feeds back into the host.
- **Never silently accept**: a call that can never do what it says raises
  where it is made, not three turns later in `/errors`. A color outside the
  palette, a rule subject outside the subject namespaces, a printable key
  with no `focus`, a watch on a path outside the schema and a tool spec the
  provider would reject are all misuse, not runtime failure. Where the
  wrong shape is a decision zemi must take anyway, it takes the closed one:
  a `tool.pre` or `permission.request` handler returning something that is
  not a `zemi.Veto` denies.
- **Frozen namespaces**: all `zemi.*` tables are read-only; the API only
  grows through versioned Go changes, visible in `zemi.api.version`. A
  frozen namespace yields nothing to `pairs`, so reading a member it does
  not have raises and lists the members it does.

### 6.1 Config and lifecycle

```lua
zemi.config.get("models.chat")                -- dotted path lookup
zemi.config.set("ui.theme", "whorl")
zemi.config.watch("models.chat", function(new, old) end)
zemi.env.get("EDITOR")                        -- env-read; secret-shaped
                                              -- names (§9 deny list) are denied
zemi.log("...")                               -- callable table: zemi.log(msg) = info
zemi.log.info("...", { key = "value" })
zemi.notify({ title = "hi", body = "...", level = zemi.notify.level.INFO })
```

Config values from `init.lua` merge with the defaults in Go's schema.
`zemi.config.set` is callable from user config only (past boot it is a
runtime pick, kept like one, below), and accepts only keys in the schema; anything else is refused with an
error. A reload
replays it, but a call that writes the value the last boot wrote is skipped,
so a runtime pick (`/model`, `/effort`, `/profile`, the mode cycle, `-m`) is not reverted
by an unrelated config edit; replaying `agent.profile` over a read-only agent
pick would loosen its read-only plan silently, so a pick is settled to its last value too.
A key whose `set` line was deleted returns to its saved pick, else its default, on that reload, unless a runtime pick moved it since.
`memory.autonomy` (§10.4) is one of them like `permissions.mode`: user
config may raise it, project and plugin code may not, and the
model's `lua` tool asks before any such call.

A runtime pick outlives the process: the global store keeps it per key as
`config:<path>`, one row each, so two windows never lose each other's picks,
and a start applies the rows before any Lua. The layers are schema default <
saved pick < init.lua < this run: init.lua always wins a restart, and a pick
over a key it sets lasts the run. A pick back to what the key would be without
it forgets the row. Flags, `-p` and `--health` keep nothing, and neither does a
mode whose base is yolo: a flip made for one task must not start every later
zemi, in any repository, allowing what the rules do not deny. zemi never edits
init.lua; `/config lua` shows the settings as lines to copy, and the agent edits
config files through the permission-gated file tools. zemi makes no commits.

Permission rules are data with one API and an optional principal:

```lua
zemi.permission.rule.add{ subject = "tool.bash", pattern = "make *",
                          decision = zemi.permission.decision.DENY, match = "make check" }
```

A Lua rule carries no principal and matches every principal, unless user
config scopes it to one plugin with `principal = "plugin:<name>"` (§9),
which `/permissions` shows in the row's tag; rules stored by an `always`
answer are scoped to the `model` principal, so a model tool call cannot
ride a rule meant for the user. Every rule lives
in memory until zemi exits; a rule meant to last is a line in `init.lua`,
which adds it again on every boot and a reload replays. There is no rule
file: an earlier `scope = "permanent"` dumped rules to a generated
`permissions.lua` and committed it to a config git repo, but nothing ever
removed a dumped rule, so an allow rule deleted from `init.lua` stayed in
force. That scope now raises with a pointer to `init.lua`, and a leftover
`permissions.lua` is not read. Origins: user config may add any rule;
plugin and project origins may add only deny/ask rules. Overlay `always` answers (§8) append session rules to
the same store, so rules never live in `zemi.state` or the DB, and no
Lua can forge one. Only the user's own config and `always` answers
write allow rules (the widening direction; plugin and project rules are
deny/ask only, so their globs can only tighten).

A project's `permissions` file (§5, in its state dir) is a list of bash
commands zemi always allows in that project, read as a boot step only
when the project is trusted. Each line is allowed as a whole with the
overlay's rule shape (§8): no rule is written from a wrapper or deleting
program or a line carrying a glob. Because the file lives outside the
repository (a clone cannot ship it; a gitignore line would not untrack a
committed one) and no sandbox can write it, allow rules from it are safe
where project `.agents` Lua above is not.

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

There are no editor modes, so a binding is just a key: the composer has
the keys unless a focusable layout node does (below), and commands are
slash commands typed in it. A binding with `{ focus = "<node id>" }` works
only while that node has the keys, and may be a plain letter there; it
comes before the global bindings unless a more trusted global binding
holds the key, and it may not take a key or sequence a more trusted
global binding holds, so a plugin panel cannot shadow the user's keys.
Key notation
matches bubbletea's strings (`ctrl+s`, `alt+enter`, `esc`; named keys are
lowercased); a string action in `zemi.keymap.set` resolves to a registered
command name or a dotted
host-API call, and executes under the principal of the config that
bound it (a principal is what an operation is attributed to: `model` or
the origin of the running Lua, see §9; attribution follows the
registration origin of the running handler, never the keypress).
Binding precedence across origins is `user > builtin > plugin > project`;
last-writer-wins applies only within one origin, so the project config
loading last cannot shadow a user binding (nor bind a prefix or an
extension of a more trusted key sequence), and `zemi.keymap.del` unmaps
keys set by the caller's origin or a less trusted one (user config can
remove shipped defaults; a plugin cannot remove user keys) and reports
whether there was a binding. The actions behind cancel-turn and
force-reload are Go-reserved: their triggers (the cancel key and the
exact, case-sensitive, non-abbreviable `/reload` command, checked on
composer submit before any Lua-visible command lookup) are fixed
in v1 (no origin, user included, rebinds them), so a compromised config
cannot relocate the panic button or put a decoy on it (case-variant
command names are refused by the §11 collision rules). `<leader>`
expands to `ctrl+x` (the `keymap.leader` setting); multi-key sequences arm a
pending state (shown in the statusline) until the next key or timeout; a held
prefix the next key breaks or the timeout ends replays as plain keys.
Command `nargs` follows Vim's spec strings (`?` = 0 or 1, `+` = 1 or more,
`*` = 0 or more).

Event catalogue (v1, each emitted on the interpreter thread):

- app: `app.ready`, `app.quit` (vetoable), `config.reloaded`
- session: `session.new`, `session.open`, `session.close`
- agent turn: `agent.start`, `agent.step` (after each model call, a
  subagent's included: its time, time to first token, tool calls and
  usage), `agent.finish`, `agent.error`
- subagents: `agent.subagent.start`, `agent.subagent.finish`
- prompt: `agent.assemble` (returns a section to add, §10.7; runs when the
  prompt is built, not per send, §6.3)
- stream: `agent.text_delta`, `agent.reasoning_delta`
- tools: `tool.pre` (vetoable: `deny|rewrite|halt`), `tool.post`, `tool.denied`
- permission: `permission.request` (vetoable), `permission.resolved`
- ui: `ui.resize`, `ui.theme`, `composer.change`, `composer.submit` (vetoable)
- learn: `memory.written`, `reflector.start`,
  `reflector.done` (counts, or `error` on failure)
- io: `job.exit`, `lsp.request`

A handler vetoes by returning `{ decision = "deny", reason = "..." }`; `tool.pre`
also takes `"halt"` and `"rewrite"` with `input`. Each payload is a `zemi.ev.*`
class in `runtime/types/zemi.lua`, picked by the event name through overloads
of `zemi.event.on`.

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
the user's own. A Lua emission runs its handlers inline, so a handler runs
as the less trusted of itself and its emitter (§5).

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
  (fantasy `StopTurn`). There is no `allow`
  decision: absence of denial is the only pass, so a Lua hook can never
  pre-approve a permission (crush's `allow` does pre-approve; zemi
  deliberately diverges). A `rewrite` from a non-user principal
  additionally flips an `allow` decision to `ask` (a `deny` stays `deny`:
  a rewrite never loosens an existing denial), so a rewrite can never
  ride a pre-existing allow glob. An `always` answer (key `a`, §8)
  stores session-scope allow rules scoped to the `model` principal, and
  they are prefix globs, not exact values: for `tool.bash` one rule per
  normalized segment, each the segment's program (its program and
  subcommand for `git`, `go`, `cargo` and the like, with the options
  before the subcommand kept) plus that prefix with ` *`, while a deleting program (`rm`, `mv`, `find`, `tar`, ...) or a
  segment §9 calls destructive stores its exact normalized string, since
  the rule runs unconfined; for `fs.read` and `fs.write` the file's directory
  with `/*`; for `net.*` both `scheme://host` and `scheme://host/*`;
  for anything else the exact argument. A file read is also offered a
  wider choice covering its whole repository, since in a deep tree the
  directory rule is the same ask again one package over. A call §9
  gate-forces to `ask` (heredoc/herestring, dangerous assignment), a
  pattern holding a glob wildcard, an opaque segment (an inner `sh -c`,
  a stripped assignment), a wrapper program that runs what it is
  handed (a shell, an interpreter, `sudo`, `xargs`, `busybox`, `zemi`
  itself, whose `plugins install` fetches code that runs everywhere, and the
  `npm exec`/`uv run` spellings of `npx`) offer no `always` option at
  all, so they re-ask every time: no rule written from such a segment
  says what the next call would run. A secret-shaped file is offered only
  its exact path, since a directory rule would cover the next secret
  too: a file request a `model` glob rule allows goes back to the ask when
  it is secret-shaped, and so does a reader command naming one (`cat
  .env`) under an `always` rule.
- `permission.request`, `app.quit`, `composer.submit`:
  `{ decision = "deny", reason = "..." }` or nil. Lua can deny; it can
  never grant.

`tool.post` is not vetoable, but it may rewrite what the model gets. Its
handlers run after the call, then the tool's `zemi.tool.override` `post`,
each as a task (§4) seeing in `ev.output` the full text the one before
left (`ev.ms` is the call's time from the gate, an ask included;
`ev.changed` lists the files it changed). A return of `{ output = "..." }`
replaces the output, which is then cut and spilled like any other; a
project handler's is ignored and logged, as its `tool.pre` rewrites are.
One call's handlers get 30 s of wall time, waits included, each slice
between waits the 50 ms veto deadline; esc ends them, and one that fails
or runs late leaves the output as it was, its error in `/errors`. A
rewrite touches neither the diff, `changed` nor the permission decision,
and a plugin's marks the session not local-only (§10.4). A call with no
handler runs no Lua.

### 6.3 Agents, sessions, models

```lua
-- Profiles are named agent shapes (fantasy agent + prompt + tools + model).
zemi.agent.profile.add("coder", {
  model = "anthropic/claude-sonnet-4-5",
  system = [[You are zemi, a coding agent...]],
  tools = { "read", "edit", "bash", "glob", "grep" },
  context = { files = { "AGENTS.md", "CONTRIBUTING.md" } },
  temperature = 0.2,
})
-- read_only makes a profile a planning agent: the project is read-only to it
-- and the pipeline denies writes and confines commands; "planner" ships that way.
zemi.agent.profile.add("planner", { read_only = true })

zemi.config.set("agent.profile", "coder")  -- the active one; "default" is the shipped shape,
                                           -- and what deleting this line returns to
zemi.agent.current()                 -- session handle: { id = "...", profile = "..." }
zemi.agent.send("fix the flaky test")  -- returns immediately; stream is
                                       -- async; during an active turn the
                                       -- message enqueues as steering (§4)
                                       -- (plugins: see below)
zemi.agent.cancel()
zemi.agent.run({ agent = "explore", prompt = "...",              -- subagent,
                 description = "..." },                          -- async
               function(result, err) end)    -- returns a handle: h:cancel()
zemi.agent.list()                    -- the agent definitions and their source

zemi.mcp.add("name", { ... }); zemi.mcp.list()  -- MCP servers bridged as tools
                                         -- (user origin only)

zemi.model.list()                    -- { { id = "anthropic/...", roles = { "chat" }, ... } }
zemi.model.set("anthropic/claude-sonnet-4-5")   -- writes models.chat
zemi.model.pick(zemi.model.role.TITLES)  -- open a role's picker (returns at once; none opens /model)
local h = zemi.model.ask({ prompt = "...", system = "...", model = "titles",
                           max_output = 1024, on_done = function(text, err) end })
h:wait()                             -- text, or nil, err; suspends the task (§4)
h:cancel()
-- Models are roles: models.chat runs the conversation, and models.agents,
-- models.titles, models.compact and models.memory (the reflector) follow it
-- while empty; zemi.config.set sets them. Anywhere a model can be named (a
-- profile, an agent definition, a workflow) a role name works too.

zemi.provider.add({
  id = "myllm",
  kind = zemi.provider.kind.OPENAI_COMPAT,  -- fantasy/openaicompat under the hood
  base_url = "https://llm.internal/v1",
  api_key = zemi.env.get("MYLLM_KEY"),
  body = { service_tier = "flex" },         -- extra request-body fields (not google)
  models = { { id = "main", display = "Main" } },
})
```

There is no `task` profile. A headless sub-turn is a subagent: a named
definition (builtin `explore`, `web` and `general`, plus `agents/*.md` under the
config home, an enabled plugin and a trusted repo's `.agents`) run as a fresh
runner over the same tool pipeline, in a child session recorded with
`parent_id`. Subagents never get `agent`, `workflow`, `todos`,
`question`, `plan` or `lua`, so nesting stops at depth 1, and `zemi.agent.run`
is refused to every origin but user and builtin: an agent acts with the
user's tools and permissions. The reflector is not a subagent (§10.3).
A running subagent takes messages like the main agent's steering queue
(§4): the main agent calls `agent` with `send_to` set to a background
run's id, and the user submits while the chat shows that run. A message
joins the run at its next step boundary, or starts one more turn of it
when its last step already began, and is stored in its child session. A
run that fails or is cancelled cannot read what is still queued, so its
report says how many messages it never read. A finished run is not gone:
`send_to` runs it once more on the history it ended with (its kept runner,
or its child session's rows), so a follow-up question costs no fresh
exploration. It keeps the run's model, and a run another session started
refuses it.

Builtin providers come from catwalk's embedded catalog, or the one
`/model update` fetched from catwalk.charm.land and saved in the data
dir (the catalog picks where each key is sent, so it lives where no
sandbox writes, and it reads only the variables the bundled one names;
one saved under another bundled catwalk version is ignored, so an
upgrade brings its newer bundle back): every generation
seeds the registry with the providers whose key env var is exported
(resolved at boot and on an update in Go, the one place that reads them; a
provider without its key is absent; `/model` lists them with their
variables after the usable models, and a TUI boot with no model to run
opens it once), then user config runs. `zemi.provider.add`
under a builtin id replaces it whole, so a gateway `base_url` keeps
working; `zemi.provider.del` drops one, `providers.builtin = false` all
of them. An unknown field on the spec or on a model entry raises: a
text-only model says `vision = false`, and a misspelt key there would
send it images anyway.
A `models.chat` config leaves unset runs the catalog's default model, from
the first provider the user added, else the first usable builtin in
catalog order (anthropic, openai, gemini, ...): a user typed theirs,
while a builtin's key may be exported for other tools.

`zemi.provider.add` and `zemi.provider.del` require a user-owned registration origin: Lua
from a plugin or a project cannot call them (the
mechanical check is the origin, not the caller's principal). Provider base URLs ride the §9 net
gate; a user provider's key comes from user config (`zemi.env.get` reads secret-shaped
variables for user and builtin origin only) and is never exposed back to Lua.

`zemi.model.ask` is one tool-less model call on a role's model or an id
(default `chat`), run off the interpreter and billed to the live session.
It costs money, so a plugin needs a user allow rule for `lua.model.ask`
with its principal (§9), project code may not call it, and a reload
cancels it. `zemi.agent.send` from a plugin asks `lua.agent.send` the same
way; project code may not send at all. A plugin's text reaches the model
behind `agent.Attached("plugin:<name>", ...)`, drawn as a notice row
named after the plugin, so it is never the user's words (§10.4), and the
send marks the session not local-only. A print run (`-p`) refuses a
plugin's send: nothing would bound a chain of turns, and `-p` exits after
its own.

Session/message persistence and cost/token accounting mirror crush's model
(SQLite, summary message, todos), simplified: sessions, messages (JSON parts),
tool-call telemetry.

System prompts are Go string constants in `internal/hostapi/prompt.go`;
there are no template files. Lua injects sections via the `agent.assemble`
event and a Lua tool's `prompt`, which ships only with the tool (memory
digest, skills index, project context ship as built-in sections).

The main agent's fixed prefix, the system prompt and every tool's
description and schema, is built once and sent verbatim, because anything
that varies per turn busts the provider's prompt cache. It is keyed on
what the user switches (profile, model, trust, a permission mode's
prompt) and each tool's name and input schema, since a stale schema
fails calls: a change there rebuilds it. Otherwise it is rebuilt only
where the cache is gone anyway: `/new`, `/resume`, a compaction, and
`/skills delete`. Nothing edited mid-session moves it: instruction files,
skills, saved workflows, agent definitions, memories and `agent.assemble`
handlers wait for the next rebuild, and `/reload` and the config watcher
rebuild nothing, since the watcher reloads on any config-tree write, the
model's own skill writes included. The one exception is an instruction
file the prompt holds, or one created where it would load, that the user
edited by hand: its new text rides the next user message once, in the
same `<instructions>` block a read uses, unless the model read or wrote
that version itself, and again after a rewind cuts that message. It is
checked like the prompt's own files first, so a file that became a link
out of the repository or to a secret sends nothing. Subagents build
their prompt once per run.

### 6.4 Tools

```lua
zemi.tool.add({
  name = "figlet",
  description = "Render text as ASCII art",
  input = { text = { type = "string" } },     -- the parameters, flat
  required = { "text" },
  parallel = false,
  timeout_ms = 5000,                           -- wall time, waits included
  -- execute returns a string, { output = string }, or a table sent as JSON;
  -- `nil, err` is a tool error result (§6.0); a raise is one too, its
  -- traceback kept for /errors.
  execute = function(args, run)              -- run: { name, origin, progress }
    run:progress("rendering")                 -- one line on the call's row
    -- zemi.job.wait suspends execute and frees the interpreter (§4)
    return { output = zemi.job.wait({ cmd = "figlet", args = { args.text } }).stdout }
  end,
})

zemi.tool.override("bash", { pre = function(input)          -- wrap builtin;
    if input.command:match("^rm%s") then                    -- nil = pass through
      return { decision = "deny", reason = "no rm here" }
    end
  end,
  post = function(ev) return { output = ev.output:sub(-2000) } end })  -- §6.2
zemi.tool.list()          -- introspection for UI and reflector
```

Built-ins ship in Go, modeled on crush's set (crush also ships `ls` and
`download`, omitted; `skill` is zemi's own): `bash`, `read` (crush
names it `view`; it returns images as image content, lists a directory
one level deep in place of crush's `ls`, and carries the
nearest instruction file not already in the prompt, once per session),
`write`, `edit` (incl. whitespace-tolerant variant),
`glob`, `grep` (both over `git ls-files` with a Go walker fallback, §2),
`fetch` (readability picks the article out of the
page, converted to markdown),
`todos`, `question` (1-4 questions in one overlay, options or a typed
answer), `plan` (shows the plan in its own scrollable popup for approval; for a
read-only agent the agent and mode active before it are remembered at its start,
and approval returns to them - the turn ends there because the read-only
pipeline cannot implement, and its follow-up is a fresh send as the
restored agent under the same claim; a read-only run that ends in plain
text instead of the `question` or `plan` tool is told so once, since a
question or a report is no plan to approve),
`web_search` (DuckDuckGo Lite, or the user's SearXNG at `web.search_url`),
`job_start`/`job_read`/`job_list`/`job_kill`,
`memory` (one durable line, `replaces` rewrites or drops one, §10.4),
`diagnostics` (language
server errors and warnings for any file), `lsp` (definition, implementation,
references, rename, file symbols and project-wide symbol search by name), `skill` (loads a SKILL.md body on demand), plus the
three the host adds: `agent` (delegate to a subagent), `workflow` (write
or run a workflow script), and `lua` (run Lua inside zemi, main agent only, as user origin:
full access to toggle skills and anything else user Lua can do; it asks
in an untrusted project or read-only agent, and before a call that widens what
zemi allows or starts a program outside the sandbox, §9).
Subagents never get `agent`, `workflow`, `todos`, `question`, `plan` or
`lua` (§6.3). Every tool, Go or Lua, passes through the same pipeline:

```
model call -> schema validation
           -> tool.pre events (Lua: deny | rewrite | halt)
           -> permission gate (config rules + session rules; ask = overlay)
           -> execute (Go impl or Lua callback on interpreter thread)
           -> tool.post events (Lua: may rewrite the output)
           -> result -> fantasy ToolResponse
```

A `rewrite` re-enters the pipeline at schema validation and the permission
gate; it never skips them. On re-entry the `tool.pre` handlers run again
in deny-only mode (deny decisions are re-checked against the rewritten
input; rewrite and halt do not re-fire, §6.2), so a rewrite cannot
silence another origin's deny hook and re-entry still terminates. Tools
the host runs itself (`agent`, `workflow`, `lua`, MCP and Lua
tools) refuse a rewrite with an error result instead. `zemi.tool.override(name, { pre, post })` wraps a
tool: its `pre` is a scoped handler for that tool only, running right
after the global `tool.pre` events under the same decision contract, and
its `post` runs after the global `tool.post` handlers (§6.2).
`run:progress(text)` shows one line under a Lua tool's row, sanitized and
cut to 120 characters, as a command's last output line does.

Fantasy may dispatch parallel tool calls (up to 5 concurrent, verified in
its source): Go tools genuinely run concurrently; two Lua tools never run
Lua at once (one interpreter), but one suspended in a `wait` lets the
other run, so `parallel = true` on a Lua tool overlaps their waits.

Errors are results, not exceptions: a failed permission or Lua error becomes
a `ToolResultOutputContentError`-style message so the model can recover.

### 6.5 I/O for Lua

```lua
zemi.fs.read("src/main.go")                  -- jailed to project roots + config
zemi.fs.write("notes.md", "...")             -- jailed to project roots only;
                                             -- permission-gated on top (§9)
zemi.fs.glob("**/*_test.go"); zemi.fs.grep("pattern")
local h = zemi.http.get("https://example.com")    -- returns a handle now
local res = h:wait()                              -- suspends the task until done
                                                  -- (legal only in a task, §4):
                                                  -- { status, body }
zemi.http.get(url, { on_done = function(r) end }) -- or callback style
zemi.job.start({ cmd = "go", args = { "test", "./..." },
                 on_stdout = nil, on_exit = function(ev) end })
zemi.job.wait({ cmd = "figlet", args = { "hi" } }) -- start and wait in one call:
                                                  -- { stdout, stderr, code }
local t = zemi.timer.after(1000, function() end)  -- one-shot; also zemi.timer.every
t:stop()                                          -- cancels a pending timer
zemi.state.set("recent.model", "anthropic/x")     -- persisted kv, global scope
zemi.state.get("recent.model")                    -- also del(key), keys(prefix)
```

`zemi.state` keys from plugin and project code are stored under a
`plugin:<name>:` or `project:` prefix the chunk never sees, so each plugin
and the project have their own space and cannot read or overwrite another's,
user or builtin keys; user and builtin code see the whole store, the others'
keys included. A plugin's keys go when its directory does (§5).

Async I/O returns handles (§6.0 convention): `h:wait()` in a task or pass
callbacks; a reload stops the jobs, timers, HTTP requests and model asks
of the generation it drops. A wait (`h:wait()`, `zemi.job.wait`) suspends the
task and frees the interpreter until the handle is done, its timeout
passes (`zemi.job.wait` then kills the command) or the task ends; it
raises outside a task (§4), so an event handler or a render uses the
callback form. Host-API calls
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
environment gates: a secret-shaped or environment-affecting name is
refused when the job starts (`perm.ChildEnv`); model `job_start` takes
none, since its shell line sets them (`FOO=1 cmd`); jobs never inherit the
controlling tty (each runs in its own session through `internal/proc`;
stdin is closed unless the job asks for a pipe with `stdin = true`).

### 6.6 UI

```lua
-- A statusline segment
zemi.ui.widget.add({
  id = "clock", region = "statusline.right", priority = 10,
  render = function(ctx)   -- ctx: { width, session, agent, time, ... }
    return { { { text = ctx.time, hl = "Comment" } } }  -- lines of segments
  end,
})

-- Overlays (modals/pickers) with their own keymaps
zemi.ui.overlay.open({
  id = "model-picker", title = "Model",
  items = zemi.model.list(),
  render_item = function(m) return m.display end,  -- or a line of segments
  selected = 2, anchor = "bottom-right",           -- 1-based; by its statusline pill
  on_select = function(m) zemi.model.set(m.id) end,
})

-- Pickers in the shape of vim.ui.select: on_choice(item), nil on cancel
zemi.ui.select(zemi.model.list(), {
  prompt = "Model",
  format_item = function(m) return m.display end,
}, function(m) if m then zemi.model.set(m.id) end end)

-- Themes: palettes + highlight groups resolved by Go (colorprofile-aware)
zemi.ui.theme.add("tokyonight", {
  palette = { bg = "#1a1b26", fg = "#a9b1d6" },
  hl = { Comment = { fg = "fg", italic = true },
         StatusLine = { bg = "bg", fg = "fg" },
         DiffAdd = { fg = "#9ece6a" } },
})
zemi.ui.theme.set("tokyonight")
```

UI extensibility contract:

- The screen is a layout tree of regions (`header`, `chat`, `working`, `composer`,
  `statusline`, or any name) that Lua sets with `zemi.ui.layout.set`
  and Go solves and paints every frame (§8), plus an overlay stack. The shipped chat and statusline are default Lua registrations
  in `runtime/lua/zemi/tui/` (one module per part), replaceable by id. The module is
  `zemi.tui`, since `require("zemi.ui")` returns the frozen host namespace, whose `theme`
  and `layout` members would also shadow `theme.lua` and `layout.lua`. The composer is
  Go-owned and zemi's own editor (`internal/ui/composer.go`; there is no
  bubbles dependency): visual wrapping, a large paste folded into a
  token, submitted-message history, `ctrl+z` undo, and a
  `$VISUAL`/`$EDITOR` handoff for the draft. Lua reacts to
  `composer.change`/`composer.submit`, reads and edits the draft through
  `zemi.composer` (§6.7), and registers decoration widgets around it,
  but cannot replace it in v1.
- Widgets return lines of `{ text, hl, click, effect, copy, soft_wrap }` segments; Go measures, wraps
  (opt-in per widget), styles, and diffs via bubbletea v2. `copy = false`
  marks decoration (gutter bars, bullets, borders, line numbers): a
  selection neither lights nor copies those cells. Wrapping ends a row that
  continues on the next in a `soft_wrap` segment, so a copy rejoins the
  line and skips what follows it on the row (padding, another column). No lipgloss in
  Lua; highlight names only, and `zemi.ui.width(text)` measures cells the
  way the renderer does, so Lua clips and pads wide glyphs correctly. Rich text goes through
  `zemi.ui.markdown(text, width)`, a Go bridge to glamour returning
  styled segment lines (how the default chat widget renders assistant
  text). Glamour renders unwrapped and zemi wraps the result, so the rows
  carry `soft_wrap`; its decorations (list bullets, task marks, quote bars,
  code block margins) come back `copy = false` in `Dim`, so what a
  selection copies is the text the model wrote. A `<` that would open raw
  HTML outside code is escaped first, since glamour drops inline HTML and a
  `Vec<String>` in prose would vanish. A table renders at the
  width instead: its rows are layout, not prose. Text the user typed is not
  markdown: `zemi.ui.wrap(text, width)` wraps it plain, with the same
  `soft_wrap` marks, and `zemi.ui.diff(text, width)` draws a unified diff in
  the `Diff*` groups, the renderer the permission prompt draws in its own
  chrome. `zemi.ui.highlight(text, lang, width?)` lexes code with chroma
  into group names from the same table as the code blocks' chroma style
  (how the chat draws a bash command), plain rows when no lexer matches.
  Glamour's styles come from the active theme's groups (`Normal`,
  `Markdown*`, `Rule`, `Dim`, `Accent` for code keywords and `DiffAdd`/`DiffDel`
  for a diff fence's lines), the theme's
  `-light` variant on a light terminal; its output is parsed back into segments whose `hl`
  is `sgr:<params>`, an inline SGR style that only ever decodes to
  colors and attributes, never other escapes. `effect = "shimmer"` asks Go
  to sweep a bright band across the text every frame (widgets render at
  about 8/s, the effect needs 30), blending `hl` toward `Normal`;
  `ui.animations = false` keeps it static.
- Clicks: a segment's `click` is a plain string id and the widget spec
  carries one `on_click(ev)` (`ev = { id, button, clicks, mods, x, y }`),
  the shape of neovim's statusline `%N@Func@`. Render stays pure data, so
  cached frames hold no closures from an old generation; the handler is
  looked up by widget id at click time, dropped after a reload, and runs
  on the UI worker under the widget's origin like a keymap, so a plugin
  widget can open nothing a plugin keymap could not. An id without a
  handler draws no hover and does nothing. Ids from model text must not
  become command arguments: use host ids.
- Terminal title: the tab is named after the session ("<title> · zemi");
  `zemi.ui.title(text)` from user config replaces it until `title(nil)` or a
  reload that does not set it again. Go strips escapes from it, so it cannot
  smuggle a sequence into the terminal.
- Themes: palettes map names to colors; the `Normal` group's `bg` is the
  app's canvas, which every group without a fill of its own sits on and
  which OSC 11 gives the terminal too (reset on every exit), so the host's
  chrome and the terminal's own background are one; `hl` groups reference palette
  names or raw hex, or `link` to another group (neovim's `link`, followed
  8 deep, a cycle draws unstyled); a group takes `fg`, `bg`, `sp` (underline
  color), `bold`, `italic`, `reverse` and `underline` (`true` or a kind: `double`,
  `curl`, `dotted`, `dashed`). Hover draws the theme's `Hover` (widget click
  segments, statusline pills) or `HoverItem` (list rows,
  menus, memory items) over the text's own style: keys a hover group leaves out
  keep that style, `false` and `"NONE"` clear it, `brighten` lifts the text's fg
  (toward black on a lighter fill) and `replace` draws the group alone. A theme
  without the group underlines. A segment marked `hover = false` takes neither
  group: a clickable body row (a diff line, a tool's detail, reasoning) says a
  click acts with the pointer shape alone, so hovering one restyles nothing. The trust and permission prompts' choices keep
  the fixed `Host:Hover` (§8); `Host:` is Go's own, so a Lua segment naming
  one draws unstyled and cannot forge prompt chrome. Go resolves to the terminal's colorprofile
  (truecolor/256/16, downgrading as needed). `zemi.ui.hl.get(group)` reads
  a group back resolved, and `zemi.ui.widget.del(id)` removes a widget
  (same or less trusted origin), shipped ones included. `zemi.ui.hl.set` and
  `zemi.ui.theme.set` are user or shipped code only: an override and the
  active theme sit above every other theme, the user's own included.
- Key routing: bubbletea key -> normalized string -> Go-reserved cancel
  trigger -> Go-owned chrome (trust prompt, permission asks,
  question/memory overlays) -> the topmost overlay (Lua, or Go-built
  lists like the mode and model pickers) -> a focused layout node (its scoped keymaps, then the global ones; an
  unbound key does nothing, an unbound esc gives the keys back) ->
  open composer popup -> keymap (keys and key sequences; printable keys
  stay with the composer) ->
  composer (there are no modes). Overlays capture first,
  user maps override defaults, `zemi.keymap.del` unmaps.
- Focus: a layout region or float with an id and `focusable = true` can
  take the keys. `zemi.ui.focus.set(id)` (nil for the composer), `get`,
  `next` and `prev` (drawn focusable nodes in tree then float order, the
  composer counting as one), the `zemi.ui.focus.*` host actions and a
  click on the node move it; a click on the composer, a
  layout without the node, or esc give it back. The focus lives in Go and
  every key still goes through normal keymap dispatch, so a panel costs no
  Lua per key beyond its bindings. Widgets see it as `ctx.focus`, and the
  composer hides its cursor and popups while a node has the keys. The
  `zemi.ui.focus.*` calls and the keys that bind them are user or shipped
  code only, like setting the layout: a focused node takes every key, so a
  lower origin could route the user's typing to its own keymaps. A key the user presses still runs
  whatever bound it.
- Slash palette: typing `/` in the composer opens a popup listing registered
  commands, matched fuzzily as you type and ranked best first, a name the
  typed one begins leading; `up`/`down`
  navigate, `tab` accepts (fills `/name ` and lists its arguments, or the
  next argument's), `enter` submits the typed
  line, so `/name args` runs the command instead of sending to the
  model; a typed name that is no command runs the highlighted one only
  when it begins that name or the user moved to it, so a typo toasts and
  does not send; `esc` closes and
  `ctrl+space` reopens whatever popup the cursor's word takes. Past
  `/name ` the list holds the command's argument candidates, a Go
  completer's matched and ranked before the cap of 50, a Lua one's as it
  returned them, none past the last argument its `nargs` takes. The
  exact, case-sensitive `/reload` is Go-reserved: it is checked on
  composer submit before any Lua-visible command lookup (§4).
- Completion sources: `zemi.composer.complete({ id, trigger | command,
  items })` adds rows to the popup after a punctuation character that
  starts a word (a `#` issue list; `@` rows go ahead of the files) or to a
  command's argument candidates. `items(lead, line)` runs on the UI worker
  after each edit, like a Lua command completer, and its answer is dropped
  when the draft moved on, so the key loop never waits on it; Go sanitizes
  the rows and caps them at 50, and shows them as the source matched them
  (by meaning, maybe), narrowing only the rows it keeps while an answer is
  pending. A trigger popup with nothing to offer is
  not drawn and takes no keys, so prose with a `#` in it still submits.
- One matcher (`hostapi.Match`, over `sahilm/fuzzy`) filters every list a
  human narrows by typing: the palette, argument candidates, `@` files,
  overlays and `/memory`. Every word of the query must match a row: its
  label fuzzily when the label is one token (a name, an id, a path, a
  dotted key), or as a substring when the label is prose (it holds a
  space), since a subsequence finds nearly every line of prose; any word
  may instead appear in the row's other text. Popups under the composer
  rank best first (`hostapi.Rank`); overlays and `/memory` keep list
  order, so sections stay whole and `/sessions` keeps recency. `@` files
  rank a file name the token begins first, then by the score of the name,
  or of the path when only the path matches. Every list draws the matched
  runes of a label in the accent (`listRowsW`, `matchSegs`); the selected
  row stays the bold accent whatever matched, and its matched runes take the
  bold text color, so each state has one look in every list. It changes only
  colors, never a row's width, and never underlines: that means a click
  acts. Model-facing lookups
  (`tool_search`, a mistyped path's siblings), command dispatch and every
  permission or path check stay exact.

### 6.7 Learning, session, skills

```lua
zemi.session.id(); zemi.session.title(); zemi.session.rename("...")
zemi.session.messages()                -- { id, role, json } per stored message
zemi.session.todos()                   -- this session's todo list
zemi.session.list(); zemi.session.new(); zemi.session.resume(id)
zemi.session.rewind(message_id)        -- restore files, then truncate (§8)
                                       -- new/resume/rewind: user config only
zemi.memory.add("...", { scope = zemi.memory.scope.PROJECT, quote = "...", replaces = id })
                                       -- same host-side intake policy as §10.4
zemi.memory.list(); zemi.memory.del(id)
zemi.learn.list()                      -- pending proposal cards
zemi.learn.decide(id, "accept")        -- user principal only (keymap/command)
zemi.skills.list(); zemi.skills.enable("local-integration")
```

The rest of the installed surface (`internal/hostapi/host.go` holds the
list): `zemi.json` (`encode`, `decode`), `zemi.composer` (`get`, `set`,
`insert`, `submit`, `history_prev`, `history_next`, `paste`, `edit`, over
a snapshot the UI publishes and edits it applies on its own goroutine;
edits from user config only, since a submitted line reaches slash
commands and `!shell` and a paste reads the clipboard; the same actions are
`zemi.composer.*` keymap strings only user config may bind), `zemi.health` (`add(name, fn)`,
whose rows join the Go checks in `/health` and `zemi --health`, and `list`, which reads
those rows back from Lua),
`zemi.workflow` (`run`, `list`), `zemi.lsp` (`add`, `disable`, `list`),
`zemi.mcp` (`add`, `list`), `zemi.errors` (`list`), `zemi.project` (`trusted`, `root`),
and `zemi.permission.mode` (`add`, `del`, `set`, `list`; user config only, §9).
There is no `zemi.eval`: the eval harness was never built (§10.6).

`zemi.learn.decide` and `zemi.skills.enable` require a user or builtin
registration origin: the model can propose, but a
plugin-registered keymap or command invoking them is refused; the trust
anchor is a human keypress on a binding the user's own config registered.
They gate like the registration APIs in §9 (refusal, never an ask) for
origins that may not register.

## 7. Storage (SQLite)

```
sessions(id, parent_id, title, profile, created_at, updated_at,
         prompt_tokens, completion_tokens, cost)
messages(id, session_id, role, parts_json, created_at)          -- fantasy messages
todos(id, session_id, content, status, active_form, ord, created_at)
tool_calls(id, message_id, tool, ok, created_at)                -- per-tool counts for the reflector
memory(id, scope, text, quote, provenance_session_id,
       created_at, updated_at, superseded_by, origin)
checkpoints(message_id, session_id, path, existed, link, content, created_at)
                                                                -- pre-turn file content (a link's target) for rewind (§8)
proposals(id, payload, created_at)                              -- pending, until reviewed
skills(name, path, source, enabled, created_at)                 -- source: user|builtin
kv(key, value_json, updated_at)                                 -- zemi.state
```

One file per scope: global DB at `~/.local/state/zemi/zemi.db`, per-project
DBs at `~/.local/state/zemi/projects/<hash of trust root>/zemi.db`, the same
repository identity trust uses, so a subdirectory or a linked worktree opens
the same sessions and project memory rather than a blank store. The
global DB holds user-scope memory, skills and kv; a project DB
holds its
sessions, messages, todos, tool_calls, checkpoints, proposals, and
project-scope memory. Opening a store sweeps checkpoints older than 30
days; sessions are never swept (the session TTL is 0, and zemi deletes a
session only when the user says to: Delete in `/sessions` removes it and its subagent
runs, then VACUUMs and truncates the WAL so the text leaves the disk).
Indexes: `messages(session_id)`,
`tool_calls(message_id, created_at)`,
`memory(scope, updated_at)`, `sessions(parent_id)`,
`sessions(updated_at)`, `todos(session_id)`, `checkpoints(session_id)`.
There is no memory search and no ranking: every live memory
(`superseded_by IS NULL`) reaches the prompt, newest first per scope,
under the per-scope byte budget the write gate enforces (§10.7).
`memory.quote` holds the user's own words a memory about them rests on.
`memory.origin` is `user` or `learned`: `/memory <note>` writes `user`,
everything else writes `learned`; the autonomy gate, the supersede guard
(§10.4), and the digest labels (§10.7) read this column, not the
provenance string.

## 8. UI core detail

- **Layout**: header (no default widget; the region stays for Lua), chat
  (flex), then the composer (1..8 rows plus a "… n more lines" row) inside a
  square frame two rows taller, whose rules carry regions: the `working`
  one on top, which gets the whole rule between the corners (the shipped
  `working` widget: the snail mascot fixed in the left corner on a plain
  rule, the state in its eyes alone: swapping while the model thinks or
  tools run, `??` while the user is asked, then for a few seconds `^^`
  when the turn finished ok, `><` when it failed and idle eyes when it was
  cancelled; centered on the rule a shimmering
  label naming the current activity, interrupt key, queue depth, elapsed
  time, the ticking part keeping the cells it grows to, so the block is
  centered once and the label never slides; it reads `ctx.frame`, `ctx.ended`,
  `ctx.cancelling`, `interrupt_armed`, `view`, `editing`, `waiting` and
  `agent.retry`/`compacting`),
  the statusline below. The mascot is always drawn and the frame's height
  never moves with agent state, so no turn adds or removes a row.
  Statusline
  (`statusline.left` / `statusline.right` widgets ordered by
  `priority` ascending from each side's outer edge, the right one from the
  right edge, Go joins the ones that draw with a dim " · ",
  overflow drops the highest priority first; the
  shipped left widgets also show an armed quit (`ctx.quit_armed`), a
  pending key sequence (`key_seq`) and the rows below a scrolled chat
  (`rows_below`); only the boot failed, stuck and errors pills are Go-drawn; the
  shipped right widgets (`statusline.profile`, `model`, `context`, `percent`,
  `cost`) show the model, the session token tally, the
  session cost when the model spec carries prices (`cost_per_1m_in` /
  `cost_per_1m_out`), and
  the context-window percent, warning colors past 80%/95%).
  Overlays float, centered or anchored (`anchor = "bottom-left"` or
  `"bottom-right"`: over the statusline, the border in line with the
  composer frame on the side of the pill that opened it: `/mode` left,
  `/profile`, `/model` and `/usage` right). A Lua overlay has
  every field a Go-built menu uses (`anchor`, `selected`, a `body` of
  styled lines or `markdown`, `render_item`
  rows of `{ text, hl }` segments; a `click` or `effect` there is dropped,
  an item's `hotkey`, one character, acts like enter on that row outside a filter,
  an item's `copy` is the row's value, which `y` copies in place of the row
  (`/model`'s full model id, the init.lua lines of a `/permissions` always
  answer) and enter copies, keeping the list open, when the row does nothing else,
  an item's `section` starts a header row where it changes, and the filter
  matches it and the value with the row, so a provider name narrows `/model`
  to its group and a full model id finds its row,
  and the toggle-list `reload` `/skills` uses: a function returning the
  items, run with `on_select` on the UI worker after enter or space while
  the list stays open, and again when an overlay stacked on it closes, a
  detail view only after it ran something, so a picker opened from it shows its pick at once,
  and after a reload; a row that fills the composer closes the list, which would keep
  the keys); Go still draws the box, the origin badge, the
  filter and the hints, where a list's own `keys` are named. Opening the id
  of an open overlay replaces it in place, keeping selection and filter, when
  the opener may close it (user, builtin or its own origin). Go parses each
  item once (`hostapi.ParseOverlayItem`), and an item or action field of the
  wrong type raises from `open` (§6.0) instead of drawing a row that ignores it
  (a list item's `id` and `hl` are its own data). A detail view's rows are actions
  (`fill`, `run`, or an `id` handed to the list's `on_action`), so they draw as
  the prompts' buttons ("[r] Read it"), keyed by their `hotkey` or their
  position. Go screens key every action by its verb's initial, one verb per
  letter on a screen: `r` restart, resume or run, `a` ask or run with
  arguments, `s` sign in or stop, `e` enable, `d` disable, `o` open, `u`
  update or use now, `c` change, and `x`, in the danger color, for what
  cannot be undone (delete, or `/mcp`'s sign out, which deletes the saved
  token); `/health`'s one action per check takes its label's initial. A
  screen that manages things (`/skills`, `/mcp`, `/lsp`, `/permissions`,
  `/plugins`) is a toggle list, so an action returns to it rebuilt, and
  `/mcp`, `/lsp` and `/plugins` also rebuild in place when a server or a
  slow action changes their state off the key loop (`Overlay.Refresh`);
  `/sessions`, `/agents` and `/workflows` close on an action instead: resume,
  a run and an agent's transcript move to another screen, which a rebuilt
  list would cover. A row's binary state (a skill or plugin on or off, a
  picked option of a multi-select) is one 5-cell switch in the key column,
  `■ on ` in Ok, `□ off` or a locked `  on ` in Hint, so a toggle never
  moves text; a tag there (`current`, `follows chat`) names, in the accent.
  A row may name its own enter verb (`verb`, "open" on a
  row that opens another screen): the hint says it, space, which toggles
  a toggle list's rows, passes it by, and an "open" row leaves its list
  under the screen it opens, so esc comes back to it (`/model`'s "Choose a
  model per role"). A list's action rows are sentence-case verb phrases above
  its sections, with no section of their own and their own verb where enter
  does something other than the list's ("Install a plugin", "Follow the chat
  model"); the values sit under section headers, so an action never reads as
  one of them. A Go screen's detail opens with
  `key: value` lines set as a table (prose after them is text as written), and its
  `status:` line colors the status words every screen shares: `starting`,
  `ready`, `failed`, `stopped`, `idle` (a language server waiting for its
  first file) and `off` (disabled). A list grouped by source
  (`/skills`, `/agents`, `/workflows`) puts the user's first,
  then builtin, plugin and project, by `lua.Origin.Rank` (`sortBySource`).
- **Layout tree** (`zemi.ui.layout.set/get/rects`, `internal/ui/layout.go`): Lua hands
  over plain data, Go validates it once and solves it every frame on the Update
  goroutine, so the screen never waits on the interpreter. Boxes (row or col, optional
  separators) hold leaves: a widget region (optionally scrolled, by node id), the
  composer (exactly one, never hidden, optionally framed with a region in each rule),
  the statusline bar, a spacer.
  Each child claims cells, a percent, its content rows or a weighted fill share,
  clamped to min and max; a box too small takes cells from fill first, then percent,
  content and cells, the last child first, down to min and then to 0, and hides its last
  `collapse` child while the mins do not fit. The solver is integer arithmetic and never
  fails. Floats place a region like a neovim float (relative node, anchor corner,
  row/col, size) in list order. The shipped `tui/layout.lua` sets the default;
  user config may replace it, plugin and project code may not (a layout can
  hide what they show). `/reload` keeps drawing the old layout until the replay sets
  one; a replay that sets none draws the Go copy of the default (`hostapi.DefaultLayout`).
  Go keeps the §8 chrome whatever the tree says: prompts and overlays center over the
  stage node, or the whole screen when it is under 40 by 12; toasts stack in the stage;
  completion popups go over the stage's bottom when it is above the composer, else beside
  the composer on the side with room; Lua floats never cover the composer or the alarm
  pills, and without a bar the pills float at the bottom-left. A scrolled region
  keeps its last column for a thin scrollbar and a blank before it, and its widgets
  render two cells narrower, so the bar appearing covers and rewraps nothing and chat
  text lines up with the composer's; every other scrolling box (overlay
  bodies and lists, prompts, popups) scrolls through the same viewport and bar, and every
  bar drags, found by its cells in the last frame.
  Widgets render at their node's width and height; a content-sized node gets
  the most rows it could take (its max or the box's cells), not its last height, which
  would never let it shrink.
- **Rendering loop**: the state generation is bumped at most once per
  renderer tick, not per tea message (a refresh from any goroutine waits in
  the hook queue at most once); keymaps, widget clicks, one-shot timers,
  async Lua callbacks (job, http, agent) and async event handlers
  bump it when they return, and `zemi.ui.redraw()` bumps it on demand (a
  repeating timer calls it itself, so an idle tick costs nothing). A
  widget a message resized renders on that message instead, so no frame
  draws lines rendered for another size. A render's ctx builds `transcript`, `agent`, `todos`, `recent` and
  `included` on first read: over a 500-row transcript a widget that reads only scalars renders
  in under 20 µs instead of 1 ms (`BenchmarkWidgetCtx`). A line table a
  widget returned is never changed by it: Go converts one it saw in the last
  render only once and wraps it only once per width, so a frame of a
  streaming reply over 500 turns costs 1.5 ms, not 40
  (`BenchmarkStreamingFrame`). A render returning `true` as a second result
  runs again next frame, that widget alone: the chat fills a long resumed
  session that way, latest messages first, 30 ms at a time, and a region
  scrolled up keeps the last whole render until the new one is whole, so
  the reader's rows do not move; `View`
  asks each visible region for its cached widget lines; Lua `render` runs
  only when invalidated: v1 re-renders on every generation change (cheap:
  render is pure and cached by `(id, width, height, generation)`), plus resize
  dependency-declared invalidation is opt-in later.
- **Chat transcript**: messages are stored as content blocks (text, tool
  call, tool result, reasoning); tool output is sanitized before render
  (see below); the default chat widget maps blocks to
  lines, applies markdown rendering via glamour for assistant text
  (cached per block id and width; a frame never re-runs glamour). The
  view word-wraps (the widget sets `wrap`) and is bottom-anchored: long
  lines stay visible instead of truncating. Scrollback (`pgup`/`pgdown`,
  `shift+up`/`shift+down`, `ctrl+home`/`ctrl+end`, mouse wheel) moves the viewport
  off the tail;
  a scrolled view stays anchored while new lines fill underneath, and
  sending a message (or a `!command`) snaps back to the tail, a slash
  command does not. Reasoning blocks
  collapse to one `✻ Thought for Xs` line unless shown
  (default keymaps below); a finished tool call collapses the same way and a
  click on its row opens that one row, command and output, with the rest of
  the transcript untouched.
  The rendered view is derived from host-held blocks: the raw message JSON
  is always in the store (and in the log), reachable via
  `zemi.session.messages` and `/copy` (the last reply as the model wrote it;
  with code blocks a builtin picker over them, whose rows run `/copy code
  <n>`, `/copy code` and `/copy reply`), so no widget can hide what the
  model actually received.
- **Composer**: zemi's own Go editor (`internal/ui/composer.go`), not a
  bubbles textarea; `enter` sends, `shift+enter`, `alt+enter` and `ctrl+j` newline
  (configurable); a large paste folds into a token that expands on submit.
  A prompt glyph marks the first row and a gutter aligns the rest, so a
  multi-line draft reads as one block. The composer stays editable during
  a turn: a submit while the turn runs queues as steering (§4) instead of
  spawning a second turn, and the queued messages show above it.
- **Default keymaps** (all rebindable by user config except the
  Go-reserved cancel and reload triggers, §6.2): `enter` sends, `esc`
  twice within 2s or `ctrl+c` once interrupts the turn (`esc` in an agent
  view goes back; idle, `ctrl+c` moves a draft into the history, and on an
  empty composer twice within 2s quits), `ctrl+o` toggle reasoning,
  `pgup`/`pgdown`, `shift+up`/`shift+down` and `ctrl+home`/`ctrl+end` scroll the
  transcript (the wheel too: mouse cell motion is captured because the altscreen has no
  native scrollback). A scrolled-back viewport is anchored on the rows it
  shows, not on a row number, so a resize, an expanded thought or a new
  turn leaves the same content in view.
  Commands are slash commands: `/quit`, `/help`, `/model`, `/mode`,
  `/profile [name]` (the agent profile the next send runs as; a picker without a name;
  `ctrl+t`/`ctrl+shift+t` cycle the agent; `ctrl+y` flips to yolo and back),
  `/permissions` (the mode and the rules in force; a session rule an "always"
  answer wrote can be deleted there, from its detail view, and `y` copies it as
  init.lua lines, which are wider: a config allow skips the secret re-ask the
  answer keeps, so the detail says so, and a network answer copies none),
  `/new` (close the session: reflector trigger, clean slate), `/sessions`
  (list; enter offers Resume and Delete), `/resume [id]` (latest without an id; history and
  transcript rebuilt from the store), `/compact`, `/btw <question>` (a side question
  answered in an overlay, on stdout under `-p`, from the conversation so far,
  never stored), `/health` (one row per check, problems first: store, model
  key, context window, git,
  reflector, pending reviews, skills, recent errors, project trust, MCP
  servers, language servers, Lua runtime),
  `/errors` (recent widget, keymap and event-handler errors behind the
  statusline badge), plus the ones the host steps register (`/memory`,
  `/skills`, `/reflect`, `/agents`, `/workflow`, `/review`, `/mcp`,
  `/lsp`, `/plugins`, `/win`) and `/trust` (§5). Enter on a help command row runs it, or
  fills the composer when the command needs an argument.
  Under `-p` a command that opens a screen fails with "/<name> needs the TUI".
  The menus share one list: a row that only a closer look should act on
  (an error, a server, a skill, a workflow, a rewind) opens a detail view
  in place of the list with its full text and the actions as a list, so a restart, a
  multi-agent run or a rewind is never one stray enter; esc goes back.
- **Mouse**: one hit test (`App.target`) serves clicks and hover, so a
  highlight always means a click acts. Go chrome items carry a Go-only
  item index; widget segments carry their `click` id (§6.6) and are
  targets only while their widget has `on_click` and no modal chrome is
  open, which takes every click. The shipped statusline is clickable from
  `tui/statusline.lua`: the mode and the always-visible agent pill open their
  pickers, the model the model
  picker, the context and cost `/usage`, pending reviews `/memory`,
  running agents `/agents`. The chat widget marks a reasoning line
  (`zemi.ui.reasoning.toggle()`), an agent tool row (`agent:<run id>`,
  from the row's `run` field, opens that run's transcript) and a tool row
  (`tool:<idx>`, expands that row). The boot
  failed, stuck and errors pills stay Go-drawn, since they report a broken
  Lua layer that could not draw its own alarm; a click on boot failed or
  errors opens `/errors`, on stuck reloads.
- **Permission overlays**: ask-style tool calls freeze the turn, render an
  overlay (diff preview for edits, command preview for bash, command
  and env preview for job asks), and resume
  the pipeline. Concurrent ask-needing calls wait in the host: one overlay
  at a time, in arrival order, each waiter judged again (rules, mode,
  trust) before its overlay opens, so an answer that covers it settles it
  with no prompt; the answered box stays drawn until the next replaces it
  in place. The title asks what the call would do ("Run this
  command?"), and the model's description sits quoted on its own line,
  never in the title. The choices are a vertical list with their keys:
  `Yes` (`y`, or `o`), `Yes, and allow <what> for this session` (`a`, only
  when the request has an always choice; a command's `<what>` ends in where the
  rule runs it, `outside the sandbox` or `with the network`; plus a wider read choice `r` and,
  for a trusted project's safe bash command, a `for this project` choice
  `p`), `No` (`n` or `esc`), and `No, and tell zemi what to do instead`
  (`t`, which denies it and every waiting ask, stops the turn and hands the composer back); up and
  down move, enter answers the highlighted one. Every Go-owned prompt
  (permission, trust, plan, a question's options keyed `1`-`9` with the typed answer
  last, a yes/no one's `y` and `n`, an open memory, an overlay's detail view; a multi-select stays a list of
  switches) draws its buttons through one renderer, which puts
  each answer's key in brackets beside its label, the selected answer marked `›` in the accent like a selected list row. The session `always` and `wider`
  answers write allow rules to the rule store (§6.1),
  scoped to the `model` principal, with the prefix-glob patterns §6.2 lists; they last
  until zemi exits. Lasting rules normally come only from `zemi.permission.rule.add`
  lines in the user's own config. The one exception is the `for this project`
  answer: it appends the command to the project's `permissions` file in its state
  dir (§5) and registers its rules un-scoped, so a trusted project re-reads them on every
  boot. The overlay
  is Go-owned and non-replaceable: a plugin cannot whitelist itself; Lua
  may restyle the frame via theme groups only. The security-relevant
  chrome (answer keys, subject, badge) renders in fixed host colors no
  theme group can change; Go draws it itself and Lua text never renders
  inside that chrome, so a widget cannot fake a permission prompt. Modal
  chrome (overlays and prompts) draws over everything,
  as do the completion and palette popups, which are not modal; while a
  modal is open everything outside the top box, plugin overlays and widget
  regions too, is dimmed in a fixed host color; at any other time a plugin
  overlay carries a Go-stamped origin badge (`plugin:<name>`, `project:<file>`). While a box
  stays open its width and height only grow (a filter narrowing a menu,
  a question batch's tabs, a detail view, which opens at its list's size,
  the palette as you type), and a click outside it closes it.
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
- **Golden tests**: headless mode renders the frame's segment rows to a string;
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
  Before the split, leading literal assignments (`D=/long/path; ls $D`)
  are inlined into the command so gates and rules see the real path; an
  assignment to a name already in the environment or to a variable the
  shell itself reads (`CDPATH`, `HOME`, `PWD`, `OLDPWD`, `GLOBIGNORE`,
  `TMPDIR`, ...) or on the assignment gate's list is never inlined, and
  inlining stops at it.
  Every segment is matched on its own: the call is allowed only if each
  segment matches an allow rule, so a compound payload (including
  `cmd1\nrm -rf x` and `cmd1 & cmd2`) cannot ride a broad allow. A
  segment with a word that climbs (`docs/../x`) is matched as written and
  again with that word cleaned, and the stricter verdict holds, so
  `cat docs/*` does not allow `cat docs/../.env`; the cleaning is lexical,
  the working directory being unknown to the rules, so a path word the
  shell may still expand into a climb (`docs/{..,a}`, `docs/$X`,
  `docs/.*`) turns an allow into an ask. The
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
  session add session rules. Evaluation order: `deny`
  beats `ask` beats `allow` regardless of registration order; within one
  decision the most specific pattern wins (fewest wildcards, then the
  longest literal prefix). `not_match` is a runtime exclusion guard on
  its own rule: a segment matching the pattern but also matching the
  rule's `not_match` value does not match that rule and evaluation
  falls through; every fall-through is logged with its decision.
  Lua-authored rules must carry `match` / `not_match`
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
  `model` (every tool call, MCP tools included) or, for Lua, the running
  origin: `user`, `builtin`, `plugin:<name>`, `project` (§5).
  Attribution follows the registration origin of
  the running handler and the least trusted code on the call stack, never
  the trigger: an operation inside a plugin-registered keymap acts as that
  plugin, not `user`, no matter whose finger pressed the key, and so does
  a plugin's `setup()` that user config calls; only user code acts as
  `user`. A refused Lua call names the rule to paste, a plugin's with
  `principal = "plugin:<name>"`: a principal scopes a rule to one plugin,
  and only user config writes one. Rules may be scoped per principal, so a
  prompt-injected tool call (`model`) cannot ride rules meant for the
  user.
- Defaults when no rule matches: pure reads (`zemi.config.get`,
  `zemi.state.get`, `zemi.log`, list/introspection calls) and
  UI-shaping host APIs (`zemi.state.set`, `zemi.notify`, `zemi.timer`,
  `zemi.ui.*`, `zemi.keymap`, `zemi.command`,
  `zemi.model.pick`) are allow; `fs.read`
  inside the jail is allow except secret-shaped paths (matched
  case-insensitively on the resolved path: `.env`, `*.env`, `.env.*`, `.envrc`,
  `*.pem`, `*.key`, `*.p12`, `*.pfx`, `*.jks`, `*.keystore`, `id_rsa*`, `id_dsa*`,
  `id_ecdsa*`, `id_ed25519*` but not their `.pub`, `*credential*`, `*secret*`,
  `.netrc`, `.npmrc`, `.yarnrc.yml`, `.pgpass`, `.pypirc`, `.htpasswd`, `.my.cnf`;
  folded like APFS),
  which ask by default and whose contents are
  scrubbed at the memory store boundary with the memory-intake scrubber
  (§10.4); the side-effectful set (`fs.write`,
  `net.*`, `tool.*`, `zemi.job` as `lua.job.start`, `zemi.http` as
  `lua.http.<method>`, a plugin's `zemi.agent.send` and `zemi.model.ask` as
  `lua.agent.send` and `lua.model.ask`) is `ask`; user and builtin code
  send and ask ungated, `zemi.model.set` is refused below builtin origin,
  project code may neither send nor ask, and `zemi.memory.add` is
  refused for project code (§5); a Lua-originated call to one of these
  APIs checks the same capability under its `lua.` subject (`lua.fs.read`
  mirrors `fs.read`, subject namespace above), so these defaults cover
  both origins without extra rules. Registration and config APIs
  (`zemi.permission.rule.add`, `zemi.config.set`, profile/provider
  registration) follow their origin rules in §6 and refuse outright, as
  errors, never as asks. Anything unlisted defaults to `ask`; there is no
  fail-open path.
- Permission modes answer what is left: a mode sees only the asks that
  survive the rules above and the `permission.request` veto (§6.2), and
  it can never overrule a `deny`. An ask a rule asked for (an `ask` rule, a
  `not_match` exclusion; `Request.Explicit`) is the user's under `auto`,
  so a project or plugin rule can tighten it. Two ship: `auto` and `yolo` (every surviving ask allows, an explicit one too). A read-only agent
  (the shipped `planner` profile, `Profile.ReadOnly`) is the old `plan` in the agent axis:
  `fs.write` is denied even under an allow rule, bash and `mcp.*` allows flip to `ask` unless
  the bash command is a `perm.Normalized.ReadCommand`, and commands run sandboxed with the project
  unwritable, which is what keeps a planning session read-only. `auto` settles
  locally first, without a model: reads and writes inside the jail that
  are not secret-shaped (writes under any `.git`, to the Lua in any `.agents`
  (`init.lua`, `lua/`), to a `.gitattributes` or `.gitmodules`, the git dir a project `.git` file
  names, or zemi's own config, data and state dirs excepted: those ask
  whatever rule allows them, a mode's own included), workflow scripts (the
  script is sandboxed and each agent it starts is gated), and every bash
  command, unparsed, since it runs sandboxed (below). Two kinds still ask
  (`Host.unsettled`): the short `perm.Normalized.Destructive` list (forced or
  recursive `rm`, or one with a glob operand, `git clean`, `find -delete`,
  `shred`, `truncate`, `dd`, `rsync --delete`, also behind `xargs`,
  `find -exec`, `sh -c`, `eval` or a shell keyword, and a bare `rm` in a
  loop body or a shell body `xargs` or `find -exec` repeats; an `rm` whose
  every operand is below the scratchpad, as `$TMPDIR/...` or its literal
  path with no climb, settles while nothing in the command can have
  changed `$TMPDIR`), a git command that writes `.git` (`perm.Normalized.GitWrites`:
  the sandbox keeps `.git` read-only, so a confined commit could only
  fail and the ask comes first), and on a backend that cannot hide files
  a command naming an existing secret-shaped file. Anything a
  mode cannot settle goes to the user's ask carrying the reason it did
  not: no model screens permission requests.
  A file request is judged and shown by its resolved path (relative to
  the project when inside it), so a symlink a sandboxed command planted
  cannot dress a write to zemi's config as a project file, and a write
  under any `.git`, to the Lua in any `.agents` or zemi's own dirs, or outside the project
  and the scratchpad (`~/.gitconfig`, a shell rc file: the sandbox cannot
  confine the write tool), goes to the user.
  Network is never settled locally, because the URL is the model's.
  Known ceiling: in-jail writes and commands both settle, so the model
  can write code and run it unattended, rewrite any project file and read
  what the sandbox does not hide, outside the project too; that is the
  auto-mode tradeoff, and the sandbox policy (below) is its only limit.
  In a project the user has not trusted (§5) both modes settle less:
  writes and every command but `perm.Normalized.ReadCommand` (an allowlist of
  programs that only read, named without a path, with per-program flag
  and script checks, arguments inside the project and not secret-shaped,
  recursive searches only over trees with no secret-shaped file and, unless
  the searcher skips hidden entries, no `.git` directory; no build
  tool and no git, which runs programs `.git/config` names) go to the user.
  With no sandbox backend a trusted project also settles only
  `ReadCommand` commands, which run unconfined, and
  auto mode sends every other command to the user ("no
  sandbox"), since an approved one would run unconfined.
  A mode added from config may carry a `decide` function that answers one
  ask from Lua ("allow", "deny", anything else leaves it to the user); it
  runs as a task (§4) for at most 60s, long enough for a model call of its
  own, and like the mode's rules may not allow an unsandboxed or
  network request, an ask a rule asked for or a protected write.
  A script the model's Lua tool gets by `path` must be a `.lua` file and is
  read through the `fs.read` gate the read tool uses, since a Lua error
  echoes tokens of what it ran.
  The model's Lua tool asks (`tool.lua`, no always option) in an untrusted
  project and a read-only agent. Everywhere else it is default-deny: without
  the user's approval a run may call only the read-only members in
  `luaReadOnly` (hostapi `luatool.go`). Every Go function a namespace
  exposes, and every handle method (`lua.GuardFuncs`), runs the sandbox
  guard (`Sandbox.SetGuard`, `guardLua`), which raises for any other
  member when the tool's code is calling (its chunk, functions it defined,
  callbacks it registered, found by frame path and call stack) and the
  innermost tool code on the stack is not an approved run. Before running,
  the tool tokenizes the script (`luaUnlisted`): a zemi path off the list,
  a method call other than string and read-only handle methods, or `_G`,
  `load`, `require` and the like ask the user once for the whole script
  with no mode settling it (`Request.Unsandboxed`; headless it is denied),
  since what it changes or starts runs unconfined. An approved run gets a
  chunk name of its own, which the guard passes until the run ends, so a
  callback it registered keeps only the read-only calls afterwards. A new
  member is guarded until it is added to the list. Only user config may add modes (`zemi.permission.mode.add`) or set the
  current one; `tab` cycles them, and an unknown value reads as
  `auto`, the default. A mode's `decide` that answers neither `allow` nor
  `deny` sends the ask to the user whatever its base settles, so a mode
  whose `decide` answers nothing is how a user gets every ask.
- Sandbox (`internal/sandbox`, hostapi `sandbox.go`): a `tool.bash`
  or `job_start` call whose verdict a mode gave
  (`DecidedBy` `mode:<name>`, any base but `yolo`) or a read-only agent
  made runs confined, and so does one a session rule written from a
  `network: true` ask allows (`Rule.Network`: the ask said the command
  stays sandboxed, so the rule keeps it and adds sockets); other
  rule-allowed and user-approved runs, `!command`, `zemi.job`, MCP
  servers and zemi's own git stay unconfined. Stdio MCP servers start in the data dir
  with `ZEMI_PROJECT_DIR` in their environment, trusted project or not:
  `npx` and `uv` read `.npmrc`, `node_modules/.bin` and uv config from the
  working directory, which settled commands and writes may change, and
  most servers need the network and home caches a sandbox refuses.
  Language servers run project code (build scripts, proc macros), so with
  a live backend each starts under `serverPolicy`: readable and hidden as
  for commands, loopback, the project read-only, and writable only
  `<cache>/lsp/<trust root hash>/<server>` (`ZEMI_CACHE_HOME`, default
  `~/.cache/zemi`), which `XDG_CACHE_HOME`, `GOCACHE`, `GOPLSCACHE`,
  `CARGO_TARGET_DIR` and `TMPDIR` point at. It is not under the state dir:
  seatbelt hides that dir's own metadata, which cargo stats. A confined
  cargo cannot create a missing `Cargo.lock`, so rust-analyzer reports
  nothing until one exists. Without a backend rust-analyzer does not
  start; the other builtins and user-added servers run unconfined. The backend (seatbelt through
  `/usr/bin/sandbox-exec` on macOS; on Linux the `zemi __sandbox` helper
  with Landlock plus user, network and mount namespaces where the kernel
  allows unprivileged user namespaces, else Landlock plus seccomp) is
  probed once in `NewHost`, before
  boot (the probe is fast), so `--health` and the first turn's tool schema
  see the result; `/health` has a `sandbox` row. `sandboxPolicy`: everything
  readable but the home credential paths (`~/.ssh`, `~/.aws`, `~/.config/gh`,
  `~/.npmrc`, `~/.git-credentials`, shell histories, ...), zemi's config,
  data and state dirs (sessions, logs, keys in `init.lua`; not when they
  hold the project, and the write roots inside them stay usable) and, on
  macOS, the strict secret file shapes (in the scratchpad too, so a hint
  names another file name as the way through); writable are the pipeline's scratchpad, also
  its temp dir (`TMPDIR`, `TMP` and `TEMP`, which every model command gets, confined or not, so
  the file tools read what a command left there without asking; the shared system temp dir and `/tmp`
  are not writable, since macOS keeps the `xcrun_db` there that picks what
  `/usr/bin/make` or `git` runs, and a denial naming one gets a hint
  pointing at `$TMPDIR` before `sandbox: false`), a Go
  build cache only sandboxed runs use, one per trust root
  (`<state>/sandbox-cache/<hash of the trust root>/go-build`, set as
  `GOCACHE` through `Policy.Env`: Go fails a build it cannot cache, and Go
  trusts its cache entries, so the user's own cache written from here could
  poison a later unconfined build and a shared one another project's build) and the project when it is trusted, not read-only, not `$HOME`
  or above it, neither holds nor sits in one of zemi's config, data and
  state dirs, holds no hidden path unless the backend is seatbelt (a
  Landlock rule on a write root opens its whole subtree), has no symlinked `.git` or `.agents` (the backend protects the
  resolved target, not the link), no relative `PATH` entry makes every
  directory a `PATH` dir, and the backend keeps protected paths
  read-only inside it (`Backend.EnforcesProtect`); `.git` (and a `gitdir:`
  target), `.agents` (its `worktrees` aside, a write root of its own), zemi's dirs and every `PATH` dir inside a write root
  (an activated `.venv/bin`: zemi runs git, sh, language servers and
  clipboard helpers unconfined by name) stay read-only (the scratchpad and
  cache re-allowed); localhost network only. Seatbelt's localhost is the
  machine's; the namespace variant has a loopback of its own, so servers
  already running on the machine are unreachable (the model is told so),
  and it binds only protected paths that exist, which is why a created one
  holds trust off (§5). It cannot protect by name either, so zemi walks the
  writable project for `.git` and `.agents` entries at any depth: the ones
  there are bound read-only, one a command created holds trust off at exit,
  and past 100,000 entries the project stays read-only (or, when the command
  grew it past that, trust is held off). Landlock plus seccomp enforces neither the
  read-only paths nor loopback: with it the project is read-only to
  sandboxed runs (a build that writes it gets the hint and escalates) and
  every inet socket is refused. Known ceilings: other tools' caches
  (`~/.cache/uv`, `~/.npm`) stay read-only, so uv and npm fail confined
  until each gets a per-session cache variable, and seatbelt denies
  binding a unix socket. zemi's own temp files (the ctrl+g draft, a pasted image)
  live in `<state>/tmp`. A
  wrap that fails is a tool error, never an unconfined run.
  While a backend is live, bash and `job_start` take two escapes,
  fixed per process so the prompt cache holds. `network: true`
  (`Request.Network`) keeps the sandbox and adds IP sockets, no unix one (`Policy.Network`),
  which is all a dependency fetch or a `gh` call needs, and it stays confined
  whichever verdict allowed it, the user's ask included; `sandbox: false`
  (`Request.Unsandboxed`) drops the policy whole, credential hiding included.
  Neither is settled by mode rules or by a base other than `yolo`: they go to
  the user with the reason "needs the network, staying sandboxed" or "run
  outside the sandbox" (headless nobody can answer, so the parameters and the
  retry line are left out and the denial is by `network` or `unsandboxed`). A
  confined run that fails with output that looks like a denial, or with
  none and an exit status above 1, like a silent `curl -s`
  (`sandbox.LikelyDenied`, a heuristic, over a job's whole retained tail)
  gets one line naming the live backend's limits and the retry, `network: true`
  first when the failure looks like a network one (`sandbox.LikelyNetwork`);
  a timed-out bash run adopted as a job keeps it. Known
  ceiling: on Linux, where Landlock cannot hide `.env`, a sandboxed
  command in a trusted project can read it when it names it by a glob or
  a variable, or runs code that opens it.
- Filesystem: the jail and fs rules are checked on `perm.CanonPath`: the
  path resolved one component at a time, so `..` after a symlink lands
  where the kernel lands, case-folded on darwin and windows. Paths
  outside the jail ask (model tools) or are refused (`zemi.fs`). The write root is the launch
  directory, which is also all a sandboxed command may write (`zemi.ui.edit` alone reaches the
  whole trusted repository: the user does that writing); the per-session scratchpad
  (`<state>/scratch/<id prefix>`, §5, every model command's `$TMPDIR`) is outside it but
  is the one path the model's file tools read and write by default, secret-shaped names aside
  (`Pipeline.Check` on the pad the pipeline was bound to, so a run that outlives its session
  keeps it; `zemi.fs` cannot write there), and user config's `zemi.fs` also
  reaches `<data>/lua` (`zemi.fs.data_dir(name)`, outside the watched config
  tree, never a root of the model's file tools or of a Lua tool script the
  user did not approve); the read jail is wider: project
  roots, the config dir, the project's `.agents`, both skills dirs, that
  scratchpad and `<data>/types` (the API annotations
  and shipped Lua UI) and, for the model's file tools alone, the plugins
  minus `.git` (`<data>/plugins`, whose skills the skill tool loads; a
  clone's remote URL may carry a token; not a `zemi.fs`
  root, so no plugin reads another's source). The plugins and the rest of the data and state dirs are
  never write roots: a write to a local plugin asks every time with no
  always answer, and no mode settles it (yolo aside), since the code loads
  in every project, trusted or not; an installed plugin changes only
  through git. The one exception is the plugin being created: the user's
  yes (or yolo) to the write that creates `local/<name>/` lets auto edit
  that directory, its `.git` aside, in any project until zemi exits,
  and a directory that already existed is never granted. So an approved
  `fs.write` in one project cannot silently plant code in a plugin the user
  did not just create. The config dir is read-only to Lua: of
  zemi's own code only Go writes there, so `zemi.fs.write` cannot
  author `init.lua`. The model's `read`, `write` and `edit`, `zemi.fs`,
  `@file` attachments and the `lua` tool's `path` get their path resolved
  once before the check into a `perm.Path` and open exactly that path,
  walking it with `openat` and `O_NOFOLLOW` (`Path.Open`), so a directory
  a sandboxed command swaps for a symlink after the check fails the open.
  `write` and `edit` refuse a path whose last component is a
  symbolic link and name the target to edit instead.
  Residual race: `zemi.fs.stat` and `exists` and the roots `glob` and
  `grep` walk go by name, so a component swapped between check and use
  is followed there (the Linux endgame is `openat2` with
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
  deny (the localhost development opt-in); project rules do not. `fetch`
  refuses a host private as written (an IP literal in the set, `localhost`,
  `*.localhost`) before its ask, since no answer to it could permit one. Userinfo in URLs is a hard
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
   own behavior. Scoped, provenance-tracked. Cheap to write, cheap
   to supersede.
2. **Procedures (skills)**: reusable recipes. A SKILL.md plus optional
   `zemi-skill.lua` that registers tools/commands when enabled. User
   skills live in the config dir (`~/.config/zemi/skills/`) and start
   enabled, tracked by their `skills` row and file; nothing model-written
   is a skill: a repository procedure is a recipe.
3. **Structure (config)**: profiles, prompt sections, permission rules,
   keymaps, widgets. The learning loop proposes none of it: these are
   edits to files in the config dir, made by the user or by the agent
   through the permission-gated file tools (§6.1).

Memory enters through the intake policy below (its review queue is the verify
leg; supersede and delete via `/memory` are its rollback).

### 10.2 Capture

Everything is already captured by the store: transcripts, tool calls with
outcomes, denials, user edits to the agent's output, permission answers,
session summaries. Additional signals: explicit user teaching
(`/memory this project uses make check`), `tool.post` outcomes, and
"corrections" (user rejects a permission or reverts an edit; both recorded).

### 10.3 Distillation: the reflector

The reflector is Go (`internal/hostapi/reflector.go`), not a Lua plugin and
not a profile: one tool-less model call over a closed session. With no tools
it cannot act at all, so its only output is the proposal document below.
Triggered on `session.close` (`/new`, `/resume`) and by a startup sweep
(once per process, not on a reload) over the newest sessions nothing has
touched for ten minutes, at most three, so quitting never waits on a model
call and a session a quit, a crash or a `-p` run left behind is still read;
`/reflect` runs it by hand. It reads the delta since the seen count (kv
`reflector.seen.<sid>`, clamped when a rewind cuts below it, a fork starting
past its copied history), and only when that delta holds two user messages
(compaction summaries do not count) and a tool call, or one failed tool
call; `/reflect` skips that floor. One run per session across windows: an
atomic kv claim keyed by the session, leased for the run's timeout plus a
minute, so a second window's delta waits for its next close or sweep instead
of a run over the same rows. A reply that is not the JSON asked for marks
the delta seen, as does a second failed try (kv `reflector.tries.<sid>`), so
a delta a provider refuses does not cost a call at every close; `/health`
shows the last outcome.

Input, in this order: the delta since the seen count (its last 120 user
and assistant messages, each truncated to 400 runes, a compaction summary
labeled `summary:`, every line through the shape scrubber before it leaves
the machine) + the newest ten failed tool results, first line each, read
from the history rows since the telemetry rows keep no text + tool-call
stats + the instruction files the agent's prompt carries (the same trust
gate, 16 KiB), so the reflector can see what it must not repeat + this
session's memories awaiting review (another session's may hold outside
text, and this session's autonomy would store a copy) + the whole memory
index with each scope's budget use and the quote under a memory that has
one, so an older near-duplicate is replaced rather than added again + the
project's skills and recipes by name and description, so a repository's
own procedure is left to the live agent's recipe. The prompt asks for
memories phrased as facts, never as instructions, which the intake
heuristic would flag, and for the user's own words under anything about
the user.

Output: a JSON proposal document (schema-validated):

```json
{
  "memories": [
    { "scope": "project",
      "text": "UpsertLabels must stay read-then-insert; ON CONFLICT burns the sequence",
      "quote": "",
      "replaces": "" }
  ]
}
```

Each entry goes through the §10.4 intake and gate like one the live
agent's `memory` tool writes: `quote` is checked against the session's
user rows and a miss drops the entry, a user-scope entry without one
queues, a near-duplicate of a live or pending memory is skipped, and
`replaces` supersedes the memory it names once the new one is stored. An
entry with `replaces` set and an empty `text` retires that memory: the
reflector read in the session that it stopped being true. Retirements
always queue as forget proposals (a copy of the memory, shown as `forget:
<text>` in `/memory` and with `forget = true` in `zemi.learn.list()`),
whatever the autonomy, since deleting loses data; accepting one deletes the
memory, rejecting keeps it.

The reflector is constrained by prompt and by pipeline: it can only emit
proposals; application is a separate, reviewed step. Memories go through
the intake policy (§10.4) into the store; there is no reflector scratch
directory and no path into the config dir, and it drafts no skills: a
procedure is a recipe the live agent writes under the user's eyes. The
reflector runs detached with a timeout; a run cut off by exit is retried
by the next startup sweep, since the session still has unseen messages.

### 10.4 Proposal, review, apply

- Pending memory proposals persist in their scope's store (`proposals`
  table) until accepted or rejected, so restarts never lose them; the
  statusline shows how many wait for `/memory`. In `/memory`, `a` accepts
  a proposal, `d` rejects one or deletes a stored memory, `s` moves one
  between this project and everywhere (a stored memory is added to the
  other store before it is deleted from its own), `e` edits one in the
  user's editor (the marks apply first; the saved text passes intake again
  as user origin with the same scope and quote and keeps the id, an
  edited suggestion is stored, empty or unchanged text changes nothing,
  and the screen reopens on that row), `u` undoes the last mark, enter
  opens a row with those keys as its buttons, and esc closes the screen and
  applies the marks. Stored rows show their
  age.
- **Memory intake is host-side**, regardless of autonomy: a length cap (300
  chars, and 160 for the quote), provenance required, the quote scrubbed
  like the text, and intake-time secret scrubbing (a span matching a key
  shape or above an entropy threshold in the quote is redacted with a
  `[redacted n chars]` marker; one in the text refuses the memory, since a
  redacted row would never load, and the error says to name the env var
  instead). Text phrased as instructions ("always run X", "ignore previous
  instructions") is flagged by a recall-biased heuristic pass (imperative
  verbs, second person, instruction-override phrasings) and routes to the
  review queue even under `memory.autonomy = auto`; false positives only
  cost a review click. Auto-applied memories are capped per session. A
  near-duplicate of a live memory or a pending proposal in either scope
  (`learn.Similar`: 75% of the content tokens shared) is refused with the id
  and text it repeats, so the model merges the two through `replaces` (a
  pending one is named as queued; the user's own text is refused only word
  for word). Anything about the user carries `quote`, the user's own words:
  a learned user-scope memory without one waits for review, and a quote that
  is not in what the user typed in the session (compaction summaries,
  attachments and a plugin's `zemi.agent.send` text aside) drops the
  memory. Each scope has a 3 KB prompt budget (§10.7): a write that would
  pass it queues for review with the oldest ids to replace, and `/memory
  <note>`, accepting a queued one, a move into that scope and an edit that
  grows past it refuse and point at the oldest in `/memory`, since the user
  is there. The digest (§10.7) tells the model to follow a preference the
  user stated and to check what any other line names before relying on it.
  Residual risk is stated plainly: a learned line can still sway a credulous
  model; the cap, provenance, quote check, scrubbing and the review queue
  narrow that window. `replaces` is the one edit: text with it supersedes
  the old memory, empty text with it forgets. `/memory forget <id>` is the
  other review action: model proposals may never silently drop a
  user-attested (`/memory <note>`) memory, and a reflector retirement
  (§10.3) never drops any memory without review.
- A recipe (§10.5) is written with the file tools, and every write asks:
  the dir is under zemi's state, so auto mode never settles it, no
  "always" is offered, and the ask says when the session read text from
  outside the machine. A recipe is instructions a later unattended
  session follows, which is worth a look each time. The secret scrub
  runs after the write, on any file in the dir, and redacts it in
  place, telling the model which lines it cut. It keeps the key shapes
  and drops the entropy pass, which would cut the long paths and
  `NAME=url` values a procedure is made of. After, not before: the session store already holds the text, and scrubbing
  first would make the diff the user approved differ from the `edit`
  the model made. The same note says when the recipe will not load (a
  frontmatter name that is not the directory's, a taken name, a body
  over the `skill` tool's cap). Rewind (§8) reverts a recipe written
  in the rewound turn like any other file.

Autonomy is one knob that only user config sets:

```
memory.autonomy = "auto"      -- memories the model or the reflector propose
                              -- store at once when policy-clean; "ask" queues them
                              -- for /memory review instead
```

`/memory <note>` is not governed by the knob: it is the user's own
keystroke, stamped `origin = user` (§7), and writes immediately in both
modes. The knob covers model-proposed memories only: `ask` queues them;
`auto` applies them without review except instruction-shaped text
("project convention: run `curl ... | sh` first" is exactly the shape the
heuristic exists for), which queues anyway, a replacement of a `user`
memory or of one in the other scope (a user-scope memory superseded from
a project is hidden in every project), a retirement (§10.3), and everything from an untrusted project
(§5) or from a session that it or one of its subagents fed text from
outside the machine (`fetch`, `web_search`, the `lua` tool, any tool
zemi does not ship: MCP and Lua-registered ones, and a plugin's send or
`tool.post` rewrite, which stores a `plugin:<name>` row; all read from the
session's `tool_calls`), since injected memories come from there.
The same conditions decide whether the `memory` tool's forget of a learned
memory applies at once or waits for review; one the user saved asks,
whatever the permission mode. Known ceiling: a command the user let run
unconfined can fetch too, and that is not seen.

### 10.5 Skills

Skill format (agentskills.io compatible, crush parity):

```
skills/
  local-integration/
    SKILL.md            # yaml frontmatter: name, description; body = instructions
    zemi-skill.lua      # optional: zemi.tool.add / command.add on enable
```

Discovery merges three sources, the higher-trust one winning a name
clash: user (config dir, then `~/.agents/skills`, the dir other agents
share, whose `zemi-skill.lua` never runs since installers write there)
over builtin (zemi's own, embedded
under `runtime/skills/`, rewritten to `<data>/builtin-skills/` on every
sync) over plugin (each enabled plugin's `skills/`, by plugin name, body
only: its `zemi-skill.lua` never runs). All start enabled and toggle; a
plugin skill's flag goes with the plugin when it is turned off. The model sees name +
description in context and the full body on demand through the `skill`
tool; there is no `/skill` command, only `/skills` to browse, toggle and
use them (a row's detail carries the whole body and opens it in the
editor). Discovery lists only `enabled` skills to the model, and enable
goes through the host API (`zemi.skills.enable`, user or builtin origin).

A trusted project's skills (`.agents/skills` at the repository root, the
dir other agents read) join
the index after those and lose every name clash. They are
body only (a `zemi-skill.lua` there never runs), enabled with no toggle,
and kept out of the `skills` table, which is keyed by name across
projects. They are scanned when the trust decision is set (boot, reload,
`/trust`), never per turn, and the pinned prompt (§6.3) keeps the index
a session was built with: a sandboxed command may write one. An untrusted project adds
none, like its instruction files (§5).

Recipes are the third source: a trusted project's private skills under
`<state>/projects/<hash>/skills`, for exact procedures too long for a
memory. They never enter the repository, no sandbox policy makes the dir
writable, and they behave like project skills (body only, no toggle, no
row), losing a name clash to those too. The index lists the recipes found
when the prompt was pinned (§6.3), while the `skill` tool and `/skills` read the dir on every call: a recipe
written mid-session loads at once and never moves the prompt. Loading a
skill counts as reading its file, so a correction is one `edit`. Only a
recipe has a Delete action in `/skills` (`/skills delete <name>`), which
rebuilds the prompt: the index moves once, by the user's own act. The
reflector's prompt lists the project's skills and recipes by name and
leaves a repository's own procedure to the live agent's recipe.

### 10.6 Evals

None of this is built. There is no `zemi.eval`, no mock provider, no
headless eval run and no CI gate; the auto-apply paths that read an eval verdict below never
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
  regression case"), but eval files are code in the config dir: same review
  path.

### 10.7 Context assembly (how learning reaches the model)

Per session, the system prompt is assembled most stable first, so a
provider that caches any shared prefix shares it across sessions: the
base prompt + context files (AGENTS.md, else CLAUDE.md, injected
verbatim) + **skills index** (name + description only) + Lua sections
(§6.3) + the environment + **memory digest** (every live memory, this project then
every project, newest first, each scope cut at a 3 KB budget the write
gate keeps it under (§10.4), each line prefixed with id, origin and age in
days; no relevance ranking). The whole prompt, tool descriptions included,
is pinned at session start (§6.3) and rebuilt with a fresh environment and
digest at compaction (triggered before a turn
or at a step boundary once the context the last step used, cache
included, is within a 20k reply reserve plus 13k of the window but at
least half of it, never when the window is unknown; once after a
provider's context-too-long refusal; or by `/compact [focus]`: Go clears
old tool output and, when that is not enough, summarizes all but a
verbatim tail (at least the latest user message on, when a turn came
before it) with one model call (asked after the runner's own prompt and
unpruned history when the compact role runs the conversation's model, so
it reads the provider's cache; a flat transcript to a separate compact
model, or when that call gives no summary), stores the summary (an unfinished todo
list follows it, outside what the next compaction merges) and a copy of the
tail after it (`/resume` scans for the last summary row and starts from
it), and re-runs digest assembly, §7, with the environment section and the
rest of the prompt (§6.3) pinned again; three failed or
ineffective auto compactions in a row stop it for the session), which keeps
provider prompt caches warm; under "# Memory" a preference the user
stated holds unless the conversation or project instructions say
otherwise, and any other line carries the cue that it held when it was
written, so a file, command or flag it names is checked before the model
relies on it. Digest lines are
labeled `user` or `learned` from `memory.origin`, so users can audit
what the model was told and which of it the agent wrote itself. Context
files are injected verbatim, and only in a trusted project (§5); an
untrusted one gets the user's `<config>/AGENTS.md` alone. A pull can
still change a trusted repository's files, which the trust decision
accepts like its build scripts. `agent.assemble` handlers
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
- Only user config raises `memory.autonomy`; no Lua path edits the env
  deny-list or widens the fs jail. Auto autonomy never applies a memory
  from an untrusted project or a session that read outside text (§10.4).

Each line is enforced in code, not by prompt: the intake policy (§10.4),
the host-only proposal channel (§10.3), the user-only config.set and allow rules
(§6.1), and the reflector's tool-less model call (§10.3).

## 11. Error handling and observability

- Every Go->Lua boundary is a `pcall`; errors are logged and show as a
  statusline flag; a `/errors` overlay lists recent handler failures with
  file:line (from Lua `debug.traceback`). A failed reflector run is
  recorded there instead of toasting. Lua code and runtime failures are
  recorded by their callers under a slug source (`command boom`, `keymap
  ctrl+t`); a command error that is how it was asked (`ErrUsage`: a bad
  argument, nothing to act on) is a warn toast only. Toasts show
  at most three body rows and never a stack trace; a cut body ends in
  "… N more lines", and "/errors" after it when the error is recorded
  there. With no UI a warning or an error prints to stderr as
  "zemi: <title>: <body>".
- Registration conflicts (duplicate widget id, keymap collision) warn at
  load with origin path, last-writer-wins within one origin, like nvim's
  plugin loading noise but structured. Cross-origin id collisions are
  refused with the lower-trust origin losing, and one plugin can neither
  replace nor remove another's: event-handler ids, profile
  ids, builtin tool names, command names, and widget ids, so project
  config, loaded last, cannot replace a user safety hook, a user
  profile, a user command, or the shipped chat widget. Command names
  collide case-insensitively: a registration whose name differs from an
  existing command only by case is refused, so `/reload`/`/RELOAD`
  cannot shadow the reserved `/reload`. MCP tools (M5) are named
  `mcp_<server>_<tool>` (64 chars max) and gated per call as
  `mcp.<server>`; servers come from user config only. When two tools map
  to one name, the first server's tool is kept and the rest are skipped
  with a logged warning. Past ten MCP tools in a run's set, every one stays
  registered but a step offers only the `tool_search` tool (its
  description a catalog of names by server) and the tools a search loaded,
  at most five per search, kept per session and per subagent run.
- Logs: `zemi.log` to file + optional in-app pane; tool calls (each with
  its decision's source: rule, hook, user, overlay, or default), decisions,
  memory writes, revisions all logged with IDs so the reflector and the user
  can audit the loop end to end.

## 12. Repository layout

```
main.go, cli.go             wiring, boot steps, and the command line
internal/lua/               interpreter thread, Lua sandbox, origins, marshal, require
internal/hostapi/           zemi.* namespaces and the host services behind them:
                            turns, sessions, compaction, subagents, workflows,
                            agent runs, memory, reflector, skills hosting,
                            permissions + modes, mcp and
                            lsp wiring, rewind, scratchpads, health, export
internal/agent/             the fantasy runner: one turn, steps, stops, retries
internal/tools/             builtin tools (fs, bash, jobs, net, misc) and the tool
                            pipeline (validate, tool.pre, permission gate, run)
internal/perm/              rules engine, bash normalizer, ReadCommand, env gates
internal/sandbox/           OS confinement for settled commands: seatbelt, Landlock +
                            seccomp, the namespace variant and the helper re-exec
internal/ui/                program, regions, widgets, themes, palette, overlays,
                            composer, mouse
internal/learn/             memory intake policy: caps, scrubber, near-duplicate check, digest
internal/skills/            SKILL.md discovery and parsing
internal/store/             sqlite schema, its version stamp and queries
internal/config/            dirs, settings store, defaults
internal/mcp/, internal/lsp/   the two clients, each with its own test server
internal/proc/              every child process (own session and process group) and hardened git
internal/plugins/           plugin install, update, check and delete over plain git, and on/off
internal/glob/              rg-style glob matching for the file tools and LSP
internal/sanitize/          escape stripping at the render and log boundary
internal/logging/           log files
runtime/lua/                embedded Lua userland (tui: widgets, themes)
runtime/skills/zemi-*/      zemi's own skills; runtime/workflows/, the builtin ones
runtime/types/              the LuaLS `---@meta` description of the whole zemi.* API
```

## 13. Milestones

Status: M0-M3 shipped, and M3's config revision pipeline was later dropped (the
reflector proposes memories only). M4 was never built: no eval harness, no mock
provider, no eval CI, no `zemi.eval`. M3's permanent rule scope, its
`permissions.lua` dump and the config git repo were dropped too (§6.1). M5
shipped in part: subagents, workflows, the MCP client, the LSP client and the
plugin manager (§5) are in; embeddings-backed search is not. Shipped outside
every milestone: permission modes (§9), workflows, rewind and checkpoints,
per-session scratchpads, background agents with the `/agents` transcript view,
the `lua` tool, markdown commands and `agents/*.md`, headless print mode (`-p`)
and `--health`, and the `zemi.health`, `zemi.composer` and `zemi.json`
namespaces.

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
- **M1, chat that codes**: providers (anthropic, openai, openai_compat, google),
  `zemi.model.pick` over the registered models, sessions + messages in
  SQLite, streaming into chat widget, composer,
  bash/read/write/edit/glob/grep tools, permission overlays,
  `:` commands, themes in Lua.
  Namespaces: `zemi.env`, `zemi.state`, `zemi.timer`, `zemi.fs`,
  `zemi.http`, `zemi.job`, `zemi.tool`, `zemi.model`, `zemi.provider`,
  `zemi.agent` (profile/send/current/cancel), `zemi.session`,
  `zemi.permission` (session-scope rules; overlays need them).
  Concurrency-core exit tests: a stuck Lua tool trips the watchdog and
  marks the runtime hot; force-reload recovers; a veto deadline is
  honored under load; a rewrite re-entry terminates.
- **M2, remember**: context files, memory store + digest injection,
  reflector v1 (memory only, Go-driven), `/remember`,
  `/reflect`, session summaries; remaining built-in tools (`fetch`,
  `todos`, `question`, `web_search`, `job_*`, `skill`). Namespace:
  `zemi.memory`.
- **M3, learn to act**: skills (user), config
  revision pipeline with diff review + git, validation (smoke-load),
  autonomy knobs, permanent-scope rules + `permissions.lua` (session
  rules ship in M1). Namespaces: `zemi.skills`, `zemi.learn`.
- **M4, prove it**: eval harness + mock provider + headless CI, auto-apply
  path with eval gate, rollback UX, telemetry dashboards (local, reading
  the `tool_calls`/`messages` tables).
  Namespace: `zemi.eval`.
- **M5, platform**: subagents (`zemi.agent.run` and the `agent` tool,
  §6.3), LSP client + tools, MCP client (bridged as tools and as
  `zemi.mcp`, tools named as in §11: their tool descriptions render to the model as fenced
  untrusted data in the §6.2 style, and the server process itself sits
  outside the §9 net gate like any user-installed binary), plugin
  manager (`/plugins`, `zemi plugins install <git url>`, §5),
  embeddings-backed memory search.

## 14. Open questions

- Two projects side by side in one zemi: per-project Lua states?
  Defer, but keep session/store APIs project-scoped from day one.
- Credential storage and auth flow (keys read with `zemi.env.get` today).
- Streaming render cost: a long transcript re-renders per generation
  bump; needs a per-widget invalidation design and a measured budget.
- Platform support statement: the doc assumes Unix throughout (XDG
  paths, termios); v1 scope for Windows is undecided.
