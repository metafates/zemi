# zemi: a self-improving, Lua-extensible AI agent harness

Status: draft
Module: `github.com/metafates/zemi`

zemi is a terminal AI agent harness (coding agent first, general assistant by
configuration) whose entire userland, including its default UI, is written
in embedded Lua. Go owns the runtime (agent
loop, providers, I/O, rendering); Lua owns behavior. The user configures,
extends, and observes the agent the same way a neovim user configures an
editor: from `init.lua` against a stable host API.

Prior art studied: `charmbracelet/crush` (local checkout), `charm.land/fantasy`,
neovim's Lua architecture, pi, opencode, codex, hermes-agent.

## 1. Design principles

1. **Core is small, behavior is Lua.** Go code ships primitives; policies,
   keymaps, widgets, tools and prompts live in Lua. Anything users can do,
   the built-in config can do, because they use the same API. The reflector
   is Go and only proposes memories through the gated intake (§10).
2. **Every side effect is permissioned.** Filesystem, network, subprocess, and
   memory writes all flow through one gate, used by both model-invoked tools
   and Lua code. Nothing the agent learns changes a permission.
3. **Configuration is code.** `~/.config/zemi` holds Lua and markdown the
   user owns; zemi never commits to it, and versioning it is the user's
   choice. Structural changes are file edits, reviewed as diffs through the
   permission gate.
4. **Learning is typed and reviewable.** The agent stores facts (memories)
   and procedures (recipes) as distinct, inspectable objects with
   provenance.
5. **Boring concurrency model.** One Lua interpreter thread (an actor),
   bubbletea on the main goroutine, agent streams and I/O on workers. All
   Lua access funnels through one queue.

## 2. What each go.mod dependency is for

| Dependency | Role |
|---|---|
| `charm.land/fantasy` | Agent core: multi-provider LLM abstraction, agent loop, tool protocol, streaming callbacks, retries, stop conditions, provider-defined tools (web search) |
| `charm.land/bubbletea/v2` | TUI program, event loop, key/mouse input |
| `charmbracelet/ultraviolet` (+ `x/ansi`, `x/term`, `colorprofile`; `runewidth`, `uniseg`, `x/termios`, `displaywidth`, `uax29` and `terminfo` are ultraviolet's own, never imported here) | Cell-based rendering primitives under bubbletea v2; used for widget buffers and golden tests |
| `github.com/yuin/gopher-lua` | The embedded Lua 5.1 VM: interpreter thread, sandbox, `require`, coroutines |
| `github.com/goccy/go-yaml` | SKILL.md and `agents/*.md` frontmatter parsing |

Also: `modernc.org/sqlite` (pure-Go SQLite, keeps
`CGO_ENABLED=0`), `github.com/pressly/goose/v3` (the embedded SQL
migrations applied on open, §7), `charm.land/glamour/v2`
(markdown in chat, with `alecthomas/chroma/v2` for the code block
formatter that marks glamour's own margins), `charmbracelet/openai-go` (the SDK options an `OPENAI` provider's `body` sets, §6.3), `charmbracelet/x/exp/golden` (golden-file assertions
for TUI output), `aymanbagabas/go-udiff` (edit previews),
`sahilm/fuzzy` (the one list filter, §6.6),
`lucasb-eyer/go-colorful` (OKLCH for the 16-color fallback, §6.6;
already pulled in by `x/ansi`), `golang.org/x/text`
(the case folding APFS does, for paths),
`golang.org/x/sys` (file locks), `golang.org/x/mod` (plugin
release tags and the build's version), and `spf13/cobra` (the command line: subcommands, help, and bash, zsh and fish completion with values from the project store and an inert boot, `Host.Inert`; kong was weighed and lost on completion). Tool input validation is a small hand-written
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
name where git cannot list (no repository, no git) or takes over 2 s (a
huge untracked tree), the tool result saying which. `grep` searches that
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
   skills (SKILL.md)    |        ^  |                |             |
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
  the secret scrubber) and the near-duplicate check. The brain - the
  partitioned Markdown memory store with its ledger, render and lock -
  lives in `internal/brain`; the reflector and the skill manager in
  `internal/hostapi`.
- **Store** (`internal/store`): SQLite (sessions, messages, todos, tool-call
  telemetry, checkpoints, memory, proposals, skills, kv; §7), an embedded
  goose migration set applied on open; a schema change is a new numbered
  file, never a version refusal.

## 4. Concurrency model

Three kinds of goroutines, one rule: **only the Lua host goroutine touches
the `LState`.**

1. **Main goroutine**: bubbletea program. `Update` normalizes input, applies
   Go-side state, enqueues Lua work, schedules `View`.
2. **Lua host goroutine (actor)**: owns the `LState` and runs the
   requests it pops from its generation's queue, a locked heap ordered by
   priority class. Callers enqueue under the lock, and the hot flag is
   read before that, so they get the fail-fast "hot" answer without ever
   touching a wedged interpreter. Requests: UI callbacks (keymap,
   widget render, events), tool executions, config loading. Each request
   is `(fn, class, reply chan, ctx)`. Callers
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
  `zemi.ui.edit`, `tool.post` handlers, and
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
  tool error, a dropped
  `tool.post` leaves the output as it was. Keymaps and Lua commands run in
  order on the UI's worker per slice: a suspended one lets the next key's
  handler run, and its rest reports its own failure (`zemi run` waits for a
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
  dropped `LState`. `tool.pre` and `permission.request` vetoes wait for a running
  reload, so no tool call is checked against half a config.
- **Vetoable events never block.** `permission.request`, `app.quit`, and
  `composer.submit` handlers must answer without waiting on async work. At
  the 50 ms deadline the host cancels the handler's context (the
  catchable error fails it) and takes the event's default decision,
  logged: `permission.request` takes silence (a timeout, an abandoned
  handler, or no handler at all) as no veto, so the request runs unless
  the overlay leg denies it (§9); the timeout is only logged.
  `app.quit` and `composer.submit` default to proceed (a wedged plugin
  must not hold the user's quit or a typed message hostage; a plugin or
  builtin `app.quit` denial is honored every time, and project handlers
  cannot deny it at all); the other
  vetoables follow the same deadline, defaulting to proceed for
  `tool.pre` (absence of denial is the pass) and to shipped-sections-only
  for `agent.assemble`. While the runtime is hot, registered
  `permission.request` and `tool.pre` handlers cannot run, so both deny
  until `/reload` instead of passing unchecked. A model
  tool call that needs an ask has a second leg: the Go-owned overlay (§8)
  blocks the agent worker goroutine (never the interpreter) until the
  user answers, with no deadline; the veto runs first, at the gate, and
  an explicit veto deny wins over everything after it (§9). The
  fail-closed rule is for asks that have no overlay leg: an ask nobody
  can answer (no UI, a Lua call with no asker) denies as ByNobody
  (§9), and the Lua-call gate settles what a read-only agent may not
  touch.
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
  before any Lua-visible keymap and are fixed). Reloading builds a
  fresh `LState`:
  new requests run normally, requests that arrived while hot got the
  "reloading" error immediately, stale timer/job callbacks are dropped and
  logged, and replay is safe because all registrations are declarative.
- **Widget render is pure.** `render(ctx)` may call only the read-only
  members (`luaReadOnly`) other than `zemi.fs.*` and a handle's `wait`;
  the sandbox guard (`guardLua`) raises on any other call, naming it
  (caught, logged, widget keeps its last-good lines). Go caches rendered
  lines by `(widget id, width, height, generation)` (§8).
- **Deltas are not batched.** `agent.text_delta` and
  `agent.reasoning_delta` are emitted once per streamed chunk, each its own
  async request, and carry the incremental chunk only, so a subscriber
  that needs complete text reads the host-held transcript instead of
  accumulating deltas. The TUI coalesces its side only: a stream event just
  invalidates the frame, so at most one waits in its hook queue.
- **Steering.** Messages sent while a turn runs queue in arrival order
  and join the running turn as user-role messages at the next step
  boundary (before the next model call). A foreground `agent` or
  `workflow` call waiting on its run cannot hold the turn past the user's
  own message (§6.3): the message hands the run off, the call answers at
  once, and the run's report arrives as a message when it ends. Only the
  user's own line raises that signal (`Host.AgentSendUser`: the composer,
  or a command-line prompt), only when it joined a running turn - one that
  starts a turn leaves its calls waiting - and never a report, a plugin
  send or a loop; it is spent once the queue is read at a step boundary or
  taken back for editing, so a later call waits as usual.
  Whatever is still queued when
  the turn ends starts one follow-up turn; with nothing queued, a set goal
  is checked and may start the next (§6.3). `up` on an empty composer
  takes the queue back for editing; a second `esc` within 2s or `ctrl+c`
  cancels the turn and returns the queue to the composer.

Cancellation: `ctrl+c` (the cancel trigger, §8; the action is
Go-reserved, §6.2, and its trigger is fixed: no origin, the user
included, rebinds it) cancels the running
turn by cancelling the fantasy
context; in-flight Lua tool calls get the catchable "context canceled"
error at the next opcode (see the watchdog above), and one suspended in a
`wait` ends there, killing a `zemi.job.wait` command. Every
request-ctx cancellation (watchdog expiry, veto deadline, turn cancel,
hot mode) cancels the async handles that request started, inside Go,
which is what makes those terminate.

## 5. Lua runtime and plugin layout

Directories (XDG; Windows keeps the same roots under the user's home,
`%USERPROFILE%\.config\zemi`, not `%APPDATA%`):

```
~/.config/zemi/           config
  init.lua                entry point, runs on every start
  lua/**/*.lua            user modules, require()-able
  skills/<name>/          user skills (enabled by default)
  agents/<name>.md        subagent definitions (§6.3)
  commands/<name>.md      markdown slash commands
  workflows/<name>.lua    saved workflow scripts
~/.local/share/zemi/      data
  plugins/installed/<name>/  plugins installed from a marketplace entry: a copy of a relative
                          source, or a clone of a git one; their code under .zemi/: init.lua loaded,
                          lua/<name>/ require()-able, tests/, workflows/, and plugin.json, the
                          manifest with its optional version, description and uses (§9);
                          skills|agents|commands/ as in the config dir
  plugins/local/<name>/   plugins developed in place, the same shape; zemi writes only their .luarc.json
                          and .emmyrc.json
  plugins/marketplaces/<name>/  marketplaces zemi registered: one git clone each, shared by the
                          entries installed from it; a clone's root holds a marketplace manifest,
                          `.zemi/marketplace.json` or Claude's `.claude-plugin/marketplace.json`,
                          the zemi one read when a repo holds both
  plugins/marketplaces.json  the registered marketplaces and, per installed plugin, the entry
                          it was installed from, with the install's kind (copy or git) and id
  plugins/data/<name>/    a Claude-layout plugin's ${CLAUDE_PLUGIN_DATA}, where its hooks and
                          servers may keep state
  plugins/disabled        names turned off, one per line
  builtin-skills/<name>/  zemi's own skills, rewritten from runtime/ on sync
  drops/                  copies of the files attached from outside the project
  plans/<session id>.md   plans approved from a read-only agent
  types/                  the LuaLS annotations and a copy of runtime/lua, rewritten every start
~/.local/state/zemi/      state
  zemi.db                 global store (proposals, the user partition's memory ledger, skills, kv)
  scratch/<session id>/   per-session scratchpad, every model command's $TMPDIR (§9 write root)
  projects/<hash>/zemi.db per-project store, hash of the trust root (sessions, ...)
  projects/<hash>/root    the trust root the dir belongs to; its mtime is the last use there
  projects/<hash>/skills/<name>/SKILL.md  recipes: the project's private skills (§10.5)
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
move the cursor, enter picks it, a click answers a row. `--trust` or `--trust=false`
decides one run without saving; a headless run with nothing saved is
untrusted and says so on stderr. The home directory and `/` are never
asked about or saved (their prompt says it applies to this run only), and
the TUI does not ask at start in a directory outside a repository where the
splash, its first screen with no prompt argument and no resume, offers other projects (`Host.OffersProjects`): the user may be there
only to pick one, so it runs untrusted as after Later and the statusline
keeps `/trust`.
`/projects` lists every root zemi keeps state for, switches to one by
restarting zemi there (trust, origins and stores are bound at
start, so one process runs one project), forgets another
project's decision (the next start there asks again) and deletes its data; it
never grants trust. Its ages are the last use: the root file is touched when
zemi starts there, closes a session and quits. This project's row names the
running decision when it differs from the saved one (`untrusted this run`),
as the statusline does.
`/trust` reopens the prompt at any time, showing the current decision (Later keeps
it), saves the answer, applies it
before the reload it triggers when the decision changed, so a revoke holds
even if that reload stops at an earlier failing step.

Trusted, zemi loads the project's `.agents` config and instruction files
(and a profile's `context.files`) and starts language servers (builtin and
user-added; the Lua of zemi's own trees gets its server either way).
Untrusted, none of that: only the user's `<config>/AGENTS.md`
is an instruction file, no project language server starts (zemi's own Lua still gets its own), and `.agents` edits trigger no reload.
Either way zemi's own
git calls (`proc.Git`) switch off `core.fsmonitor`, hooks, submodule
recursion, LFS smudging, log's signature checks (`gpg.program`),
transports other than https, ssh and file, and repository filter drivers: a command can plant a nested
repository whose config would otherwise run on zemi's next
`git status`. A plugin clone is someone else's repository, so its git
calls blank the user's global filter drivers too.

Load order is a list of boot steps: host `zemi.*` namespaces
-> builtin commands (main's own plus every domain
file's: memory, reflector, skills, agents, workflow, mcp, lsp) -> the
shipped Lua UI (`require("zemi.tui")`) -> the project trust
decision (the prompt above; before any config that may start an MCP
server) -> plugins (each enabled plugin's `.zemi/init.lua`, sorted by name) ->
user `init.lua` -> user skills' enable hooks -> a trusted project's
`.agents/init.lua` -> markdown commands (user, then each plugin's, then a
trusted repo's `.agents`, then the skills that ask for one, §10.5) ->
`app.ready`. Boot runs on its own goroutine, so it may block on the
trust prompt without holding the UI. The host's own steps (namespaces, builtin
commands, the shipped UI, markdown commands, `app.ready`) are hard: the first
failure stops the boot, the runtime goes hot and the screen is the Go-drawn
boot box (§8). Everything a user owns (trust, plugins,
`init.lua`, skill hooks, project `init.lua`) is a soft step: its error is
recorded for `/errors` and the boot goes on. Agents and workflows are read when
used: the user's, then the builtin ones, then each plugin's, then the
project's under the current decision; the first of a name wins. Project config loads late with
its registrations tagged `origin = "project"` (shown in the UI and logs).
Origin is a label, never a privilege: any origin calls the same host API with
the same rights, and a collision goes to the last registration (one plugin
still cannot replace another's, §11). What stays is the trust mechanism and
the manifest: a plugin may call only what its `uses` declares, a project's
files load only when it is trusted, and a builtin tool name stays reserved
(builtin ids are names, not privilege).
`load`/`loadstring` stay available to every origin: a chunk they build
runs as the loading origin (its chunk name is re-tagged, so a forged name
does not stick).

Runtimepath, in order: a trusted project's `.agents`, `~/.config/zemi`, then
the one enabled plugin named like the module's first segment: `require("x.y")`
searches plugin `x`'s `.zemi/lua/` after the config dir, so a plugin shadows
no another's modules. Any origin may require from any of these trees. The
plugin roots are the ones the plugins step loaded, fixed for the generation,
so a disabled plugin's files never resolve.

The plugins step lists `plugins/local/*` and `plugins/installed/*`, skips
the names in `disabled`, and runs each `.zemi/init.lua` as plugin origin; it runs
no git and reads each manifest only for its declared `uses`, which a name
outside §9's vocabulary refuses at load. A plugin whose `init.lua`, `lua/`,
`tests/` or `workflows/` still sits at its root is refused too, with where
its Lua goes: that layout is what zemi no longer reads, and loading nothing
in its place would be silent. A plugin's skills, agents, commands and
workflows come from the same list, sorted by name, whether or not its
`init.lua` ran; the user's
and the builtin ones win a name, a name two plugins ship goes to the first
and `/errors` names both, and a skill a plugin ships never runs a
`zemi-skill.lua`. A plugin that fails does not stop the rest; each one's error goes
to `/errors` and the `/health` plugins row counts it. A local plugin named
like an installed one does not load, nor does a symlinked plugin dir (its
files would resolve outside the plugin class). The step also drops the
`zemi.state` keys of a plugin whose directory is gone or holds another
install: the install's id comes from its `marketplaces.json` row, else from
the clone's `.git`, and the key `plugin:<name>` keeps the one that wrote
them. The one-second config
watcher reads `local/` whole, `.zemi` included, the `disabled` file,
`marketplaces.json` and each installed clone's git index, so an edit, a
toggle, an install, an update and a delete each reload; a marketplace
clone's index is unwatched, since updating it changes no installed plugin,
whose reload the `marketplaces.json` write carries.

`/plugins` and `zemi plugins` manage the tree through `internal/plugin`,
which is plain git and runs no Lua. Every install comes from a marketplace:
`marketplace add <url>` clones a repo holding a marketplace manifest into
`marketplaces/<name>/`, named by the manifest - `.zemi/marketplace.json`, or
Claude's `.claude-plugin/marketplace.json`, the zemi one read when a repo
holds both; a repo holding neither is refused with both files named, the
same name and source re-adds by re-cloning the cache, and a different
source under a taken name is refused naming both. `marketplace list` shows each
marketplace with its source and entries; `marketplace update [name]` pulls
one or all clones, naming what moved and the installs now behind it;
`marketplace remove <name>` is refused while a copy install still updates
from it, and drops the rows of git-kind installs, which keep working from
their own clones. `install <name>[@<marketplace>]` installs one entry: a
relative source is copied into `installed/` as a plain dir, a git one is
cloned into a dot dir, checked out at the highest `vX.Y.Z` tag and renamed
into place, and the install's row in `plugins/marketplaces.json` keeps the
marketplace, the entry, the version, the kind and the id; a bare name
searches every registered marketplace, installing the one match, listing
the candidates when several carry it and hinting `marketplace add` when
none does, and a URL or a path is refused with the same hint - there is no
install from a git URL. `update [name...]` routes by the row's kind: a
git-kind install fetches, then moves a tag within its major version, a
branch to the highest tag when that tag is ahead of it, or else a branch
fast-forward, and refreshes its recorded version; a copy pulls its
marketplace clone and re-copies, marketplaces grouped so one update pulls
each once, the re-copy and the save skipped when nothing moved; an install
with no row is the legacy plain clone, updated by its own git. A git-kind
entry's ref or sha pin is install-time: update floats within the repo's own
tags, and a marketplace repin needs delete and install. `check` fetches the
clones and, for copies, fetches each marketplace holding copy installs
without resetting its clone, reading the manifest off the fetched ref, so
the list names each update from local refs: a version move as
`<ver> -> <ver>`, a versionless entry's movement as `updated in the
marketplace (<name>)`; `enable`, `disable` and `delete` (an installed
plugin, else the local plugin; a symlink loses the
link only; `zemi plugins delete` asks first, and needs `--yes` off a
terminal). `zemi plugins` runs without the TUI or trust, and without the
Lua boot but for `test`; a prompt whose first word names a command goes
after `--`. A plugin may carry a manifest, `.zemi/plugin.json` in its code
dir:
`{ "version": "1.2.0", "description": "...", "uses": ["model", "http"] }`,
every field optional, which is what a list shows in place of the
checked-out tag (else the short commit) and `.zemi/init.lua`'s first line; `uses`
is the set of host capabilities the plugin's code calls, and what §9 makes
the switch grant. It never names the plugin: the directory does,
in the settings keys, the principals and every verb.

A plugin laid out for Claude Code (`.claude-plugin/plugin.json`, no `.zemi`)
loads too, with no Lua of its own. Its manifest, `.mcp.json` and hooks file
are read by `internal/plugin`; its skills, agents and commands load through
the same layered steps under the plugin's sanitized name as a prefix
(`<plugin>-<name>`, so two plugins cannot take one name), with the skill
frontmatter's `name` and `description` optional (the directory and the
body's first line stand in), every body carrying `${CLAUDE_PLUGIN_ROOT}`
and `${CLAUDE_PLUGIN_DATA}`, and each command also a model-invocable skill
row that is no `/<name>` - the command is. Enabling the plugin is the
consent for all of it, as for an MCP stdio child: nothing prompts past the
enable.

Its MCP servers register through the same `addMCP` as user config, named
`<plugin>-<server>`, stdio children carrying the two plugin variables in
their environment; `sse` and `ws` types are logged and skipped. Its hooks
build a generation-tagged registry beside the other ones, rebuilt by the
plugins step from the plugin dirs and purged with the generation. The
mapped events (`PostToolUse`, `PermissionDenied`, `Stop`, `SessionEnd`,
`SubagentStart`, `SubagentStop`) fire from the events' emit sites, each
entry a shell child run off the caller's goroutine, the plugin root as its
directory, the Claude payload on stdin and its output only logged, never
interpreted (a `PostToolUse` hook loses Claude's exit-2 feedback path with
it). Refused at load, the plugin otherwise loading: `PreToolUse` and
`PermissionRequest` (a hook's output is logged, never interpreted, so it
neither denies nor asks, and a subprocess cannot sit inside the 50 ms veto budget), `UserPromptSubmit` (it would tax every
submit), `Setup`, `Notification`, every unmapped event, hook type but
`command`, and an entry with an `if` condition. A `SessionStart` hook is
session context instead of an event: the capture starts off the
session-creation path, holds each hook to 10 s and stores its
`additionalContext`, else its stdout, for that session alone, dropping a
pinned prompt it lands after so the next send rebuilds with it. A reload
never re-runs it, a message sent within the capture window gets it on the
next send, a headless one-shot may never see it, and a `SessionEnd` hook
does not fire on process quit (nothing calls closeSession there).
`/plugins` is a toggle list built off the key loop, local plugins first,
each row's state first (`failed`, an update), then its version, the
marketplace it came from (none for a local plugin or a legacy clone) and its
description; its detail names the source, the load status, the manifest's
`uses` and what the plugin registered, and the git and marketplace verbs run
on a goroutine that refreshes the list when they end, unless the user closed
it meanwhile. Under the plugin rows a Marketplaces section lists each
registered marketplace, its source and entry count in the row, the clone
manifest's entries in its detail, `u` updating the clone and `x` removing
the marketplace, refused with the plugins whose copies still update from
it; none registered, the section's one row fills
`/plugins marketplace add `. Four
verbs need the screen, so only `/plugins` takes them: `toggle <name>`,
`open <name>` (its `.zemi/init.lua` in the editor, or its directory without one),
`new [what it should do]`, the list's first row, and `modify <name> [what
to change]`, which a local plugin's detail fills; the last two send the
agent a short prompt pointing at the zemi skill's plugins.md, where the
procedure lives. `modify` also lets auto mode edit that plugin until zemi
exits, as an approved creating write does (§9); an installed plugin is
refused, since the agent never writes there.

`zemi plugins test <name|dir> [pattern]` runs a plugin's
`.zemi/tests/*.lua` so an agent writing one tests it without a user's session.
Each top-level file there is a test file, and one that declares no case
fails: code the files share goes in a subdirectory (`.zemi/tests/lib/`, loaded
with `require("./lib/<name>")`), which the runner does not glob. Each case
whose "file: name" holds the pattern runs in a zemi of
its own, booted in the process from temp dirs holding a filtered copy of
the plugin (its code and its manifest, `.zemi` whole, nothing of the root
but the layer dirs, no `node_modules`
or link leading out) and nothing else: no user or project config, no trust
step, no other plugin, no `~/.agents`. The file's top level runs where
`init.lua` would, so it is
the case's setup, and a first boot collects the case names. `zemi.test`,
registered only there, declares cases and drives what the plugin
registered the way zemi runs it: `run`, `keys`, `tool` (through the
adapter the model's call runs), `render`, `click`, `emit` (to the plugin's
own handlers, `tool.post` as tasks and vetoable events with their
deadline), `complete` (a command's completer and the plugin's
`zemi.composer.complete` sources, as the palette asks them), `overlay`,
`answer` and `set` (one of the plugin's settings by its bare key, as
`/config` writes it),
each refusing another origin's target, a Go command and a host
action, and a driver at a file's top level raises. `eq`, `neq`,
`contains` and `raises` assert, `eq` naming the first difference (the line,
in a multi-line string). The zemi does
no IO by construction: the process starts no process (`proc.Refuse`), and
a job, HTTP request, model ask, agent send or `zemi.fs` write passes the
gate only when a `test.stub` answers it, its reply standing in for the
transport; a call no stub answers raises naming the stub line to add, and
a `false` reply refuses it. Time is a fake clock at 2026-01-01 UTC
that only `test.advance` moves, driving timers, `os.time`/`os.date`/`os.clock`,
a render's `ctx.time` and log times; every driver returns once zemi is
quiet (no host goroutine running, no timer due). An error zemi records in
`/errors` while a driver runs fails the case. Output is the failures,
paths relative to the plugin, each fact once, and one summary line; the exit status is 1
on any failure or a pattern matching no case, 2 on a usage error.
NOTE: one widget renders at a time (no screen layout), `keys` reaches no
`{ focus = id }` binding, `answer` no list key or toggle list (a row
it picks runs its `run` when no callback takes it, only a command of the
plugin), a stub answers at once (a timeout is untested), an assertion in a
`tests/lib/` helper names the helper's line (the case's is in the FAIL
heading), and the plugin's
skills, agents and workflows are not exercised.

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
  cache, so no Lua-side table can stand in for a module's table.
- `./x` and `../x` resolve next to the calling file and stay in its tree,
  and for plugin code in its own plugin. A plugin's file resolves only from
  that plugin, so neither `..` nor a symlink reaches another plugin's code
  or a disabled one's.
- A module runs once per generation, keyed by its resolved file (a dotted
  and a relative name share it), whatever requires it, so the table it
  returns is the one every caller reads, and what it keeps in that table is
  readable and writable by all of them; a secret stays in a local.

Sandbox (what userland gets):

- Kept: `base` minus `dofile`/`loadfile` (they read and execute raw files,
  bypassing the `zemi.fs` gate); `load`/`loadstring` kept (Lua
  source only; gopher-lua has no bytecode loader at all, which makes the
  classic bytecode-execution hole a non-issue); `table`, `string`, `math`,
  `coroutine`, minimal `debug` (`traceback` only, every frame named),
  `os.time` / `os.date` / `os.clock`.
- Removed: `io`, `os.execute`, `os.remove`/`rename`/`exit`, `os.getenv`
  (use `zemi.env.get`), `os.setenv` (env mutation subverts the §9 env
  gates),
  `os.tmpname` (creates files past every gate), the `package` library, `setfenv`/`getfenv`
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
  `pcall` or `string.format` by loaded code fails the same way
  (assignment into the shared tables raises, like the `zemi` binding).
- `print` routes to the log and, when called from a handler, to `zemi.log`.
- All filesystem/net/subprocess access is via `zemi.fs` / `zemi.http` /
  `zemi.job`, each permissioned at its call (see §9).
- Origin is per call, not per file: chunks created by `load`/`loadstring`
  inherit the origin and the environment of the loading chunk (the same
  raising `__newindex` and `__index` chain; they never receive the
  interpreter's global table), and the embedded
  `runtime/lua/**` stdlib runs as a distinct `builtin` origin. A file's
  origin comes from a directory-class map Go applies to its
  `EvalSymlinks`-resolved path (a symlink is classified by what it
  resolves to, so an alias cannot borrow another class):
  `~/.config/zemi/**` is user, `~/.local/share/zemi/plugins/**` is
  plugin, `<repo>/.agents/**` is project, embedded `runtime/**` is
  builtin. Origin never follows the requirer: a module under `plugins/...`
  stays plugin origin and a project module stays project origin even when
  `init.lua` requires it. Origin is a label (the overlay badge, the error
  and traceback chunk names, `zemi.state` keys): it grants nothing.
- The code a call runs as is the innermost Lua file on the call stack, the
  threads that resumed it included, so a plugin's `setup()` that user
  `init.lua` calls runs as that plugin and a user callback the plugin calls
  runs as user; a chunk with no file of its own (a task's scratch source, a
  `loadstring` chunk) takes the frame the host entry pushed. Chunks compile
  with no tail calls, which would drop their caller's frame. A metamethod
  leaves no frame of the code that set it, so a handler behind `obj.setup`,
  `obj()` or `==` runs as the code that triggers it; `setmetatable` (tables
  only) installs it as given, and a Go function handed out as a value runs
  as its caller, with the module table as its first argument when it is
  called as a method. Registrations capture the origin and file of the code
  making them. A plugin is named by its directory under
  `installed/` or `local/`: that name scopes its `zemi.state` keys
  (`plugin:<name>:<key>`), its permission principal (`plugin:<name>`) and
  its overlay badge (plugin code with no file of its plugin on the stack,
  such as a user module a plugin required, called from user code, has no
  name, and the calls that need one raise), and one plugin cannot replace another's
  command, keymap, widget, theme, event handler, tool, completion source,
  health row or config watch (the first by name keeps it; everything else
  is the last writer's).
- Boundary caps: values crossing the Lua <-> Go marshal are capped (string
  16 MB, table depth 64), cycles are refused with an error,
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

All namespaces are preloaded Go-backed tables (plus a small Lua stdlib);
the binary's API version is `zemi.api.version`.

### 6.0 Conventions

- **Errors**: calling an API wrongly (bad argument types, an option field
  of the wrong type, unknown member, invalid enum) raises a Lua error,
  catchable with `pcall`; an option field raises naming its key
  (`nargs must be a string, not a number`, `opt` in helpers.go). Operations that
  can fail at runtime split two ways: async ones (jobs, HTTP, provider
  calls) return a handle and deliver failure through it; synchronous ones
  (`zemi.fs.read`/`write`, `zemi.state.get`/`set`, `zemi.json.decode`,
  `zemi.job.start`, the memory, learn and skills calls) return
  `nil, err` instead of raising. The split is mechanical: runtime
  failures never raise; misuse (bad arguments, a call a render may not
  make) and a permission gate that does not allow always raise.
- **One verb per job**: `add` registers (`zemi.command`, `zemi.tool`,
  `zemi.provider`, `zemi.health`, `zemi.mcp`, `zemi.lsp`, `zemi.ui.widget`,
  `zemi.ui.theme`, `zemi.config`), `set`
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
  palette, a printable key
  with no `focus`, a watch on a path outside the schema and a tool spec the
  provider would reject are all misuse, not runtime failure. Where the
  wrong shape is a decision zemi must take anyway, it takes the closed one:
  a `tool.pre` or `permission.request` handler returning something that is
  not a `zemi.Veto` denies.
- **Frozen namespaces**: all `zemi.*` tables are read-only; the API only
  changes through versioned Go changes, visible in `zemi.api.version`.
  `pairs` lists a namespace's members through an iterator Go owns (the
  table Lua holds is an empty proxy, and the real one never reaches Lua),
  and reading a member it does not have raises and lists the members it does.

### 6.1 Config and lifecycle

```lua
zemi.config.get("models.chat")                -- dotted path lookup
zemi.config.set("ui.theme", "whorl")
zemi.config.watch("models.chat", function(new, old) end)
zemi.env.get("EDITOR")                        -- env-read
zemi.log("...")                               -- callable table: zemi.log(msg) = info
zemi.log.info("...", { key = "value" })
zemi.notify({ title = "hi", body = "...", level = zemi.notify.level.INFO })
```

Config values from `init.lua` merge with the defaults in Go's schema.
`zemi.config.set` is callable from user config only (past boot it is a
runtime pick, kept like one, below), and accepts only keys in the schema; anything else is refused with an
error. A reload
replays it, but a call that writes the value the last boot wrote is skipped,
so a runtime pick (`/model`, `/effort`, `/profile`, `-m`) is not reverted
by an unrelated config edit; replaying `agent.profile` over a read-only agent
pick would loosen its read-only plan silently, so a pick is settled to its last value too.
A key whose `set` line was deleted returns to its saved pick, else its default, on that reload, unless a runtime pick moved it since.
`memory.autonomy` (§10.4) is one of them, and a call that raises it from
Lua passes the `lua.config.set` ask like any other.

A runtime pick outlives the process: the global store keeps it per key as
`config:<path>`, one row each, so two windows never lose each other's picks,
and a start applies the rows before any Lua. The layers are schema default <
saved pick < init.lua < this run: init.lua always wins a restart, and a pick
over a key it sets lasts the run. A pick back to what the key would be without
it forgets the row. Flags, `zemi run` and `zemi health` keep nothing. zemi never edits
init.lua; `/config lua` shows the settings as lines to copy, and the agent edits
config files through the gated file tools. zemi makes no commits.

A plugin's options are settings of its own: `zemi.config.add(key, spec)`
registers a key (a string, a number, a bool, or one of a list) with its
default, from the plugin's init.lua while it loads. The plugin names it by
the bare key; zemi derives the plugin from the calling file, as it does for
`zemi.state`, and stores it as `plugins.<name>.<key>`, the name the user sets
and /config shows. The plugin's `get` and `watch` of a bare key resolve the
same way, and since every builtin key has a section, a bare name is never a
builtin one. They are schema rows like the builtin ones, so `/config` lists
them under a section per plugin and every layer above applies unchanged; a kept pick waits in
the store until its key registers (checked then, forgotten when the new spec
refuses it). A replay that registers a key again with the same spec keeps its
value; a key the replay did not register (the plugin is off, gone, or stopped
registering it) leaves after it. A `set` or `watch` of a key its plugin did
not register raises as a typo when the plugin loaded and does nothing when it
is off or failed, so turning a plugin off never breaks init.lua, and a
deleted or reinstalled plugin's picks go with its `zemi.state`. A plugin
reads its settings and never writes them (its write would be kept as the
user's pick), and a secret never goes in one: /config shows it.

A `zemi.tool.override` no tool answers to is an `/errors` entry once boot
is done naming the tools there are: the Lua tool it names may come from a
project or a plugin, so the call that adds it cannot tell.

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
		if ev.tool == "shell" and ev.input.command:match("^rm%s") then
			return { decision = "deny", reason = "no rm from the model" }
		end
	end, { id = "safety.no-rm" })
zemi.event.emit("user.my-thing", { any = "payload" })
```

There are no editor modes, so a binding is just a key: the composer has
the keys unless a focusable layout node does (below), and commands are
slash commands typed in it. A binding with `{ focus = "<node id>" }` works
only while that node has the keys, and may be a plain letter there; it
comes before the global bindings while that node has the keys.
A binding with `{ list = true }` is a list alias: while a list, a
composer popup or a prompt has the keys, its key makes the move its action
names (`down`, `ctrl+d`, ...), swapped in Go before the list sees it, so a
list never waits on Lua; while a line types (the draft under a popup, a
filter, a typed answer) a move the line edits with, `home` or
`end`, is not swapped and the key keeps its own meaning. It is one
non-printable key other than the ones lists answer with (enter, esc, tab,
shift+tab, space, backspace). This is neovim's `pumvisible()` mapping as
data.
Key notation
matches bubbletea's strings (`ctrl+s`, `alt+enter`, `esc`; named keys are
lowercased); a string action in `zemi.keymap.set` resolves to a registered
command name or a dotted
host-API call, and executes under the principal of the config that
bound it (a principal is what an operation is attributed to: `model` or
the origin of the running Lua, see §9; attribution follows the
registration origin of the running handler, never the keypress).
Any origin binds any key: the last registration wins, so the project
config loading last shadows a user binding, and a key that starts a longer
sequence shadows or is dead, whichever came later. `zemi.keymap.del`
unmaps a key whatever bound it (user config can remove shipped defaults,
and a plugin can remove a user key) and reports whether there was a
binding. The actions behind cancel-turn and
force-reload are Go-reserved: their triggers (the cancel key and the
exact, case-sensitive, non-abbreviable `/reload` command, checked on
composer submit before any Lua-visible command lookup) are fixed
(no origin, user included, rebinds them), so a compromised config
cannot relocate the panic button or put a decoy on it (case-variant
command names are refused by the §11 collision rules). `<leader>`
expands to `ctrl+x` (the `keymap.leader` setting); multi-key sequences arm a
pending state (shown in the statusline) until the next key or timeout; a held
prefix the next key breaks or the timeout ends replays as plain keys.
Command `nargs` follows Vim's spec strings (`?` = 0 or 1, `+` = 1 or more,
`*` = 0 or more).

Event catalogue (each emitted on the interpreter thread):

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
- ui: `ui.resize`, `composer.change`, `composer.submit` (vetoable)
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
as its own file (§5).

Vetoable events share one return contract. Return `nil` for "no objection";
otherwise return a decision table:

- `tool.pre`: `{ decision = "deny", reason = "..." }` blocks the call (the
  reason becomes the tool's error result); `{ decision = "rewrite", input =
  <patched input> }` shallow-merges over the original JSON and re-enters the
  pipeline (§6.4);
  `{ decision = "halt", reason = "..." }` ends the turn after this tool call
  (fantasy `StopTurn`). There is no `allow`
  decision: absence of denial is the only pass, so a Lua hook can never
  pre-approve a permission (crush's `allow` does pre-approve; zemi
  deliberately diverges).
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
rewrite touches neither the diff, `changed` nor the permission decision.
A call with no
handler runs no Lua.

### 6.3 Agents, sessions, models

```lua
-- Profiles are named agent shapes (fantasy agent + prompt + tools + model).
zemi.agent.profile.add("coder", {
	model = "anthropic/claude-sonnet-4-5",
	system = [[You are zemi, a coding agent...]],
	tools = { "read", "edit", "shell", "glob", "grep" },
	context = { files = { "AGENTS.md", "CONTRIBUTING.md" } },
	temperature = 0.2,
})
-- read_only makes a profile a planning agent: the project is read-only to it
-- and the pipeline denies writes and confines commands; its turn ends in a
-- plan the user approves, asks changes to, or saves to start later. A plugin
-- or a project registers profiles the same way, so a planner can ship as a
-- plugin; the switch stays the user's (a /profile pick, ctrl+t, or their own
-- command or keymap):
zemi.agent.profile.add("planner", { read_only = true })
zemi.command.add("plan", function() zemi.config.set("agent.profile", "planner") end)

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

zemi.model.list()                    -- { { id = "anthropic/...", roles = { "chat" }, ... } }
zemi.model.set("anthropic/claude-sonnet-4-5")   -- writes the conversation's slot:
                                                 -- the active profile's, else models.chat
zemi.model.pick(zemi.model.role.TITLES)  -- open a target's picker (returns at once; none opens /model)
zemi.model.add("classifier", { desc = "the permission classifier", model = "anthropic/claude-haiku-4-5" })
zemi.model.del("classifier")         -- a slot of its own, for a job rather than a conversation
local h = zemi.model.ask({ prompt = "...", system = "...", model = "titles",
                           max_output = 1024, on_done = function(text, err) end })
h:wait()                             -- text, or nil, err; suspends the task (§4)
h:cancel()
-- Models are targets: models.chat runs the conversation, and models.agents,
-- models.titles, models.compact and models.memory (the reflector) follow it
-- while empty; zemi.config.set sets them. A profile is a target too, its
-- definition's model the slot's default that models.<name> overrides.
-- zemi.model.add registers a slot with a model of its own, for code that runs
-- a model on a job. Anywhere a model can be named (a profile, an agent
-- definition, a workflow) a target name works too.

zemi.provider.add({
	id = "myllm",
	kind = zemi.provider.kind.OPENAI_COMPAT,  -- fantasy/openaicompat under the hood
	base_url = "https://llm.internal/v1",
	api_key = zemi.env.get("MYLLM_KEY"),
	body = { service_tier = "flex" },         -- extra request-body fields (not google)
	models = { { id = "main", display = "Main" } },
})
```

A session is named at creation from the first line of the user's first
message, which the store row, the tab, the exit banner and
`zemi.session.title()` read at once; `session.auto_title` then has the `titles`
role's model replace that name in the background, never waiting for the first
reply, and a failed or empty answer keeps it. A `zemi run` makes no title call.
Every title surface - the tab, `/sessions`, `/projects`, the splash's recent
rows and the exit banner - leads the name with the session's marker emoji,
derived from its id and picked from a pool of one-codepoint, East Asian Wide
glyphs of Emoji 11.0 or older, so it is stable across resume and restart
without storage; subagent runs, in child sessions, never surface one.

There is no `task` profile. A headless sub-turn is a subagent: a named
definition (builtin `explore`, `general` and `plan`, plus `agents/*.md`
under the config home, an enabled plugin and a trusted repo's `.agents`,
whose `read_only: true` makes a read-only agent like `explore`) run as a fresh
runner over the same tool pipeline, in a child session recorded with
`parent_id`. A subagent never gets `agent`, `workflow`, `todos`, `question`,
`plan` or `lua`, so it starts no subagent of its own. `zemi.agent.run`
from Lua starts one with the same tools and permissions the caller's turn
has. The reflector is not a subagent (§10.3).
Nesting stays bounded because only a live turn starts a fork, and a fork
cannot fork again: a run it delegates to is a leaf like any other.

A fork (`agent = "fork"`, subagent.go) continues the caller's conversation
instead of starting fresh. Its first request is the caller's last request byte
for byte - the same tools in the same order with the same schemas, the same
system prompt and context message, the same messages - plus one task message
holding a preamble and the caller's prompt, and its calls carry the caller's
own provider cache key (`prompt_cache_key`, `x-session-affinity`, §10.7), so
the provider reads that prefix out of its cache instead of writing it again.
Everything else is an ordinary subagent run: an `agentRun` in `taskReg`, a child
session whose `profile` is `fork`, `send_to`, `/agents`, `/export`, the same
permission pipeline, the same billing. It is no definition: `forkAgent` holds a
name and a description only, so the tool's enum, its agent list and
`zemi.agent.list()` name it, while a user's `agents/fork.md` is ignored with an
error and `/agents open fork` says there is nothing to edit. A fork gets its own
`tools.Pipeline` (its own pad, the conversation's read log and MCP loads,
`TodosRead`/`TodosWrite`/`Question`/`ApprovePlan` nil) and rebuilds its tools
over it, keeping the caller's names, order and `Info` (`pinnedTool`): another
array would write the cache it exists to read, so a name the rebuild lacks
refuses the fork. That array is also the caller's, so a fork lists `agent`,
`workflow`, `todos`, `question`, `plan` and `lua`, which a subagent never gets,
and not `update`, which a subagent does (the harness still streams a fork's
progress into the caller's tool row): todos, question and plan refuse through
the nils, while `agent` and `workflow` work, since a fork is the main agent
continued. A fork continues the live conversation, so `forkSetup` refuses one
whose call is already a run of it, one asked after the turn ended, one the tool
asked for with `background` (its context is the turn that is running), and one
this process forgot: its stored rows are its own work, never the conversation it
opened with. Its task says what it is, so a model does not spend steps asking
for what only a live turn can give it.
`/agents new` has the main agent write a definition: it sends the typed
line with a short prompt pointing at the New agent steps in the zemi skill.
Each ready prompt (`/agents new`, `/workflows new`, `/plugins new` and
`modify`, `/errors ask`)
names its zemi skill files by path (`zemiSkillDir`), SKILL.md, whose rules
every change follows, then the task's own, which the read tool opens
with no ask, so it works with the skill switched off or no skill tool.
A running subagent takes messages like the main agent's steering queue
(§4): the main agent calls `agent` with `send_to` set to a background
run's id, and the user submits while the chat shows that run. A message
joins the run at its next step boundary, or starts one more turn of it
when its last step already began, and is stored in its child session. It
talks back the same way: the run's `update` tool sends its caller a line
mid-run, which joins the caller's turn at its next step boundary and
draws a notice row naming the run, so a caller waiting on the run reads
it there and can steer on it (the user's own message hands the run off
instead, §4). A run that fails or is cancelled cannot
read what is still queued, so its report says how many messages it never
read. A finished run is not gone:
`send_to` runs it once more on the history it ended with (its kept runner,
or its child session's rows), so a follow-up question costs no fresh
exploration. It keeps the run's model, and a run another session started
refuses it. `send_to` with `stop` cancels a running run, the model's one
stop where `/agents stop` is the user's: the run ends cancelled and
reports nothing, and `wait` and `stop` refuse each other.

A workflow is a Lua script that starts subagents: `zemi.workflow.agent`,
`parallel`, `pipeline` (Lua over `parallel`), `phase`, `log` and `args`,
nothing else of zemi but `zemi.json`; a bare `agent` or `print` raises
naming its `zemi.workflow` spelling. The `workflow` tool takes one of
`script`, `name` or `run` (`script` joins `run` for action `rerun`), and
`action list` on its own names this session's runs, newest first, with the id
to act on: a run `/workflows` started is drawn in the chat, which the model
never reads, so the list is the only way it learns one is running. `run`
with an action reports its status, waits on it, pauses it (no new dispatch,
running agents finish), resumes or stops it (cancels the run), and `concurrency`
with `run` sets a running one's limit (1-16, at the next dispatch);
`/workflows restart` cancels one running agent and queues the same call
again, counting toward the 100-agent cap. `background` delivers the result
like a background agent's report, and a failed run's result says how to
rerun it. A call without `background` waits for its run, and the user's own
message hands the run off: the call answers with the background text and the
result arrives as a message like that one, since a call cannot hold the turn
past a message the user sent (§4). A handed-off run keeps its row, revised
when the run ends - a run that finished with a failed agent inside it draws
warn, a stopped one stop, a run that died err, and the notice row its report
arrives as leads with the same mark - and the outcome is stored with
the session, so a restart draws the mark it ended with, not done. A run
already under way can be waited on instead: the `workflow` tool's `wait`
action and the `agent` tool's `wait` beside `send_to` block until that run
ends, esc, or the user types, the row naming the run waited on, and the report
still arrives as a message rather than in the call's result. `agent`'s `schema`
asks in the task for a JSON object and the reply's outermost `{...}` is
validated, asking again at most twice; the script gets a table. A run
writes its script and a journal (a header, one line per agent that ended,
an end line) into the pad of the session that started it; a rerun runs
that script file, edited or not, and answers each call identical to a
finished one (agent, model, schema, prompt) from the journal in order,
starting no agent. `/workflows` heads with making one
(`new <what it should do>` sends the line with a prompt pointing at the New
workflow steps in the zemi skill, and the list's first row fills the command;
a run in flight is what its keys act on by default), then the runs, live ones
of every session
first, then this session's earlier ones (another session's live run ends
its desc with that session's title), then the saved workflows; enter on a
run opens its view, at the one width every workflow screen takes: its
fields, its agents under their phases (each run and agent row, in
`/agents` too, its status field and each phase header lead with the
chat's mark for its state: a phase is failed once one agent failed, else
running while one runs, done when all are; an agent the run never started
reads cancelled once it ended), and keys acting on the run, `p`,
`+` and `-` (relative, so a second press before the view rebuilds still
counts) and `x` (stop, on its second press) while it runs, `r` (rerun,
whose view takes the ended one's place) once it ended, and `o` (open its
script) for a run of this session, with `s` (save it to `workflows/` under
its description's name, never one another workflow has) when the model
wrote the script and this process still holds it as it ran. An agent's
detail views its transcript, from which esc comes back, restarts or stops
it; its stop has no hotkey, since `x` there stops the run. While any run lives
one ticker rebuilds whichever of the two is open once a second. The user
starts a saved one with `/workflows run` or `/review`, with no model call
before the work: detached in the TUI with a live notice row, and headless
on the claimed agent, as `/btw`.

Builtin providers come from catwalk's embedded catalog, or the one
`/model update` or `zemi models update` fetched from catwalk.charm.land and saved in the data
dir (the catalog picks where each key is sent, so it lives in zemi's own
data dir, and it reads only the variables the bundled one names;
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

`zemi.provider.add` and `zemi.provider.del` take any caller: a provider's key
comes from `zemi.env.get` and is never exposed back to Lua.

`zemi.model.ask` is one tool-less model call on a role's model or an id
(default `chat`), run off the interpreter and billed to the live session.
It costs money, and the manifest's `uses` naming `model` is what consents to it (§9);
a reload cancels it. `zemi.agent.send` from a plugin settles the same way. A plugin's text reaches the model
behind `agent.Attached("plugin:<name>", ...)`, drawn as a notice row
named after the plugin, so it is never the user's words (§10.4).
`zemi run` refuses a
plugin's send: nothing would bound a chain of turns, and `zemi run` exits after
its own.

A `zemi.model.ask` with a `schema` asks for a value instead of text. The
schema is a JSON schema in the fields a tool's input has (`type`,
`properties`, `required`, `items`, `enum`, `description`, the numeric and
length bounds), and it goes to the provider as one: whichever route the
kind's ladder takes - native JSON mode, a forced tool call, or the schema
written into the prompt - fantasy decodes the reply against it, so zemi
returns the object itself, a table for an object schema and a number,
string or boolean for a scalar, with the raw reply as the second return.
A model that answers with a sentence is pinned per provider: `object_mode`
(`auto`, `json`, `tool`, `text`) is refused for the kinds that always pick
their own route. A schema zemi cannot read raises at the call, and a reply
that arrived and did not fit the schema comes back as `nil` and the error,
whose text carries the reply.

Classifying a state is its own call. `zemi.classify.ask({ model, state,
questions, timeout })` (`zemi.ClassifySpec`) judges `state` and answers one
typed answer per question - a bool with its probability, a choice with its
label probabilities, a score - and nothing parses a sentence. `model` is any
target or `"provider/model"` id, the conversation's model by default. The
call answers through the first of three strategies that can:

1. `protocol`: the model is a `decisions` model of a `system_one` provider, so
   the state and the questions go to `{base_url}/systemone` with the key as a
   bearer token and the answers come back with real probabilities. A decisions
   model is never resolved as a chat model: `/model` offers it to the slots
   `zemi.model.add(name, { classify = true })` made for classification, and
   neither a turn nor a chat role takes one.
2. `logprobs`: a chat model whose provider returns token probabilities. Each
   question becomes one prompt whose answer is a single label (`Yes`/`No`, a
   letter, a digit) and the answer is the softmax over the label tokens among
   the returned top-k, at a deeper readout when a label is missing. The
   temperature is left alone here: the provider reports the distribution its
   own sampling produced.
3. `schema`: the structured ask above, with a schema per question type, so the
   provider decodes the answer and zemi validates it. It has no probability to
   report, so its answer carries no confidence.

A strategy the model cannot answer through is skipped, one that fails does not
stop the next, and the call is an error when the last one fails: a caller that
cannot get a verdict asks the user rather than guessing. Every answer is
validated against its question - an unknown label, a probability outside
0..1, a question the response left out, a score outside its levels and a body
that is not JSON are errors, never a nearest match. The call is gated as
`lua.classify.ask` (a plugin needs `model` in its uses) and billed to the
live session like `zemi.model.ask`, and the provider's key is read in Go.

`/goal <condition>` gives a session a finish line. The message carries abrief naming the condition, and storing it sets the goal (a `goal` row after
it, with the session's spend then); only the brief `/goal` just sent counts,
so a plugin's text, a file or a report quoting one sets nothing. After each turn that ended cleanly with
nothing queued and no background run of the session going, the model judges
the goal as a side question on the cached conversation (`Host.aside`, the
call `/btw` uses): `MET` clears it, `NOT YET` sends the next turn with the
reason, `BLOCKED` stops and keeps it, as do a reply with no verdict and a check
that failed, whose reason the paused row names. The
check and the turns it starts run under the claim the first turn took, so a
submit queues and `zemi run` waits for the whole chain; a message queued
during the check goes instead of the note, an interrupt or a failed turn
ends the chain with the goal kept, and at 20 turns in a row the goal
started itself it pauses; a loop run does not reset that count. Each verdict is a `goal` row, drawn as a notice
line live and on resume, so a rewind, a fork and a delete handle it like the
rest of the conversation. A read-only agent is never checked, and `zemi
run` exits 1 when the chain leaves the goal set. Lua reads it (`ctx.goal`,
`ctx.agent.checking_goal`) and does not drive it: the check must hold the
claim, and a plugin-driven chain of turns is what `zemi run` refuses.

`/loop [interval] <prompt>` sends a prompt again while the session stays
open in this zemi: every interval (1m to 24h), or, with none, when the model
says in its reply's last line (`next: 10m`, 1m to 1h, 20m when it names none;
`next: done` ends the loop). The first run goes at once. A run is a message
with the prompt in a note, or a command with the note attached, sent only
when the agent is idle (a busy one delays it 30s), and each is a notice row.
One loop per session; it is never stored, so a resumed session starts
nothing on its own, and it stops on `/loop stop`, a session change, a
failed run, an interrupt of any turn nobody typed (a run, a goal's turn or
check, a report's turn: esc means stop working on your own), and after 72h.
A chain of turns a run began arms no idle recap and sends no `ui.notify`
alert, since nobody waits on it; the loop's end alerts. `zemi run` refuses
it: a process that stayed up for it would be a daemon nobody asked for.

Session/message persistence and cost/token accounting mirror crush's model
(SQLite, summary message, todos), simplified: sessions, messages (JSON parts),
tool-call telemetry.

System prompts are Go string constants in `internal/hostapi/prompt.go`;
there are no template files. Lua injects sections via the `agent.assemble`
event and a Lua tool's `prompt`, which ships only with the tool (the brain
section, skills index, project context ship as built-in sections).

The main agent's fixed prefix, the system prompt and every tool's
description and schema, is built once and sent verbatim, because anything
that varies per turn busts the provider's prompt cache. It is keyed on
what the user switches (profile, model, trust) and each tool's name and
input schema, since a stale schema
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
their prompt once per run; a fork sends the caller's own text and tools
instead, which is the cache it reads (§10.7).

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

zemi.tool.override("shell", { pre = function(input)          -- wrap builtin;
		if input.command:match("^rm%s") then                    -- nil = pass through
			return { decision = "deny", reason = "no rm here" }
		end
	end,
	post = function(ev) return { output = ev.output:sub(-2000) } end })  -- §6.2
zemi.tool.list()          -- introspection for UI and reflector
```

Built-ins ship in Go, modeled on crush's set (crush also ships `ls` and
`download`, omitted; `skill` is zemi's own): `shell` (crush names it `bash`), `read` (crush
names it `view`; it returns images as image content, lists a directory
one level deep in place of crush's `ls`, and carries the
nearest instruction file not already in the prompt, once per session),
`write`, `edit`,
`glob`, `grep` (both over `git ls-files` with a Go walker fallback, §2),
`todos` (a list the user watches: an item left `in_progress` when the claim ends
goes back to `pending`, so nothing reads as running while the agent is idle), `question` (1-4 questions in one overlay, options or a typed
answer, and a note the user may add to a pick; the result is one JSON
document, each answer a string, a multi-select's array, or null when left
open, which its `open` field tells the model to decide), `plan` (shows the plan in its own scrollable popup for approval; for a
read-only agent the agent active before it is remembered at its start,
and approval returns to it - the turn ends there because the read-only
pipeline cannot implement, and its follow-up is a fresh send as the
restored agent under the same claim; Save, don't start approves it with no
handoff, saving the plan and leaving the turn ended until the user says when; a
read-only run that ends in plain
text instead of the `question` or `plan` tool is told so once, since a
question or a report is no plan to approve),
`job_start`/`job_read`/`job_list`/`job_kill`,
`memory` (one durable line, or a private-partition brain file, `replaces` rewrites or drops one, §10.4),
`diagnostics` (language
server errors and warnings for any file), `lsp` (definition, implementation,
references, rename, file symbols, project-wide symbol search by name),
`skill` (loads a SKILL.md body on demand), plus the
five the host adds: `agent` (delegate to a subagent), `workflow` (run a
workflow script inline or by name, in the background too, rerun an edited one,
and pause, resume or stop a background one), `lua` (run Lua inside zemi, main agent only, as user origin:
full access to toggle skills and anything else user Lua can do), `tool_search`
(past ten MCP tools a step offers only it and what a search loaded, its
description a catalog of names by server, §11), and `update` (what
a subagent tells its caller mid-run, §6.3; only a subagent holds it).
Subagents never get `agent`, `workflow`, `todos`, `question`, `plan` or
`lua` (§6.3); `update` joins their set after the `tools` allowlist is read,
so no agent file can take away the way back to its caller. A fork holds the
caller's own set instead, `lua` included and `update` not, since its cache
requires the same array. Every tool, Go or Lua, passes through the
same pipeline:

```
model call -> schema validation
           -> tool.pre events (Lua: deny | rewrite | halt)
           -> permission gate (veto; ask decision = overlay)
           -> execute (Go impl or Lua callback on interpreter thread)
           -> tool.post events (Lua: may rewrite the output)
           -> result -> fantasy ToolResponse
```

A lua script also drives the model's tools: `zemi.tool.call(name, args)`
starts one of the run's own tools off the interpreter, through that same
pipeline, one gate per call, and returns a handle whose `wait()` suspends the
script. Only the script's print and return values reach the model, so a script
batches, parallelizes and filters what would otherwise be a step per call
(the tool result carries the whole output, `Pipeline.Script`, not the cut a
direct call gets). Only the running script reaches it, the set is the run's own
(profile filter included), and a script that only drives tools needs no
approval of its own: `zemi.tool.call` is the one member `luaDelegated` lets an
unapproved run call, since what it reaches is gated anyway. `zemi.tool.list()`
names that set - an MCP tool a step is holding back included - and
`zemi.tool.describe(name)` gives any tool's parameters, so one script composes
calls the model itself would have to load first.

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
zemi.fs.read("src/main.go")                  -- relative to the project dir
zemi.fs.write("notes.md", "...")             -- any path; both gated (§9)
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

`zemi.state` keys from plugin code are stored under a
`plugin:<name>:` prefix the chunk never sees, so each plugin has its own
space and cannot read or overwrite another plugin's; every other origin
shares one store, so the user's config, the project's and the model's Lua
read and write the same keys. A plugin's keys go when its directory does
(§5).

Async I/O returns handles (§6.0 convention): `h:wait()` in a task or pass
callbacks; a reload stops the jobs, timers, HTTP requests, model asks,
agent runs and workflow runs of the generation it drops. A wait (`h:wait()`, `zemi.job.wait`) suspends the
task and frees the interpreter until the handle is done, its timeout
passes (`zemi.job.wait` then kills the command) or the task ends; it
raises outside a task (§4), so an event handler or a render uses the
callback form. Host-API calls
from Lua never open the interactive overlay: the gate settles them (§9) -
a read-only agent's writes deny, everything else runs; only model tool
calls surface the overlay, on the agent worker (§4). URLs go through the
permission gate like tools do; a redirect is re-checked against the gate
before it is followed. Subprocess
environments are the parent's whole environ: a command the model starts
adds the non-interactive vars and points `TMPDIR` at the scratchpad;
`zemi.job.start` appends the job's opt-in extras (`env = { ... }`,
Lua-supplied values only, never looked up from the parent by name) as
given; model `job_start` takes
none, since its shell line sets them (`FOO=1 cmd`); jobs never inherit the
controlling tty (each runs in its own session through `internal/proc`;
stdin is closed unless the job asks for a pipe with `stdin = true`). Windows
has no process groups here: a killed shell only kills itself, and its own
children keep running until a job object is used.

### 6.6 UI

```lua
-- A statusline segment
zemi.ui.widget.add({
	id = "clock", region = "statusline.right", priority = 10,
	render = function(ctx)   -- ctx: { width, session, agent, time, ... }
		return { { { text = ctx.time, hl = "Dim" } } }  -- lines of segments
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

-- Themes: a palette of roles, the rest derived; groups name roles and are
-- defined once for every theme (Go resolves them, colorprofile-aware)
zemi.ui.theme.add("mine", { bg = "#1a1b26", fg = "#c0caf5", ansi = { --[[ 16 colors ]] } })
zemi.ui.hl.set("Dim", { fg = "dim", italic = true })
zemi.config.set("ui.theme", "mine")
```

UI extensibility contract:

- The screen is a layout tree of regions (`header`, `chat`, `working`, `composer`,
  `statusline`, or any name) that Lua sets with `zemi.ui.layout.set`
  and Go solves and paints every frame (§8), plus an overlay stack. No layout draws the
  `exit` region: its widgets print once to the shell after zemi quits, foreground colors
  only (the shipped `exit` widget prints the session, the command that resumes it and the cost). The shipped chat and statusline are default Lua registrations
  in `runtime/lua/zemi/tui/` (one module per part), replaceable by id. The module is
  `zemi.tui`, since `require("zemi.ui")` returns the frozen host namespace, whose `theme`
  and `layout` members would also shadow `theme.lua` and `layout.lua`. The composer is
  Go-owned and zemi's own editor (`internal/ui/composer.go`; there is no
  bubbles dependency): visual wrapping, a large paste folded into a
  token, submitted-message history, `ctrl+z` undo, and a
  `$VISUAL`/`$EDITOR` handoff for the draft. Lua reacts to
  `composer.change`/`composer.submit`, reads and edits the draft through
  `zemi.composer` (§6.7), and registers decoration widgets around it,
  but cannot replace it.
- Widgets return lines of `{ text, hl, click, effect, copy, soft_wrap, anchor }` segments; Go measures, wraps
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
  (how the chat draws a shell command), plain rows when no lexer matches.
  Glamour's styles come from the active theme's groups (`Normal`,
  `Markdown*`, `Rule`, `Dim`, and for code `Accent` keywords, `Literal` strings
  and numbers, `DiffAdd`/`DiffDel` a diff fence's lines), the theme's
  `-light` variant on a light terminal; its output is parsed back into segments whose `hl`
  is `sgr:<params>`, an inline SGR style that only ever decodes to
  colors and attributes, never other escapes. `effect = "shimmer"` asks Go
  to sweep a bright band across the text every frame (Go applies it when
  it paints, so the band moves every 50 ms with no render), blending `hl`
  toward `Normal`, and
  `effect = "spin"` steps a segment's first glyph through the running
  spinner. The state marks
  (`zemi.ui.marks`: running, done, warn, failed, stopped, waiting, ...) are
  one Go table the chat's rows and the host's lists draw alike.
- Clicks: a segment's `click` is a plain string id and the widget spec
  carries one `on_click(ev)` (`ev = { id, button, clicks, mods, x, y }`),
  the shape of neovim's statusline `%N@Func@`. Render stays pure data, so
  cached frames hold no closures from an old generation; the handler is
  looked up by widget id at click time, dropped after a reload, and runs
  on the UI worker under the widget's origin like a keymap, so a plugin
  widget can open nothing a plugin keymap could not. An id without a
  handler draws no hover and does nothing. Ids from model text must not
  become command arguments: use host ids.
- Terminal title: the tab is named after the session ("<mark> <title> · zemi",
  the mark the session's id derives); `zemi.ui.title(text)` from
  user config replaces it until `title(nil)` or a
  reload that does not set it again. Go strips escapes from it, so it cannot
  smuggle a sequence into the terminal. Either way a mark leads it: a braille
  spinner stepping every 200 ms while a turn runs, `?` while a prompt waits on
  the user, `✓` or `✗` after a turn that ended while the window was unfocused,
  until it is focused again. An idle zemi writes no title.
- Themes: a theme is a palette of fixed roles (`bg`, `fg`, `accent`,
  `literal`, `ok`, `warn`, `danger`, `dim`, `rule`, `panel`, `band`, `addbg`,
  `delbg`); `zemi.ui.theme.add` takes `bg`, `fg` and the four hues, or a
  terminal's 16 `ansi` colors in their place, and derives the rest once in Go
  (OkLab mixes: dim is fg toward bg, rule and panel bg toward fg, band the
  panel toward the accent, the diff tints bg toward ok and danger; the literal
  the OKLCH midpoint of danger and warn, each scheme's own orange; and a
  palette that came from `ansi` takes the hue and a little of the chroma of
  the scheme's own bright black into dim, rule and panel, a surface taking
  less than a line of text, so a generated theme's greys and boxes belong to
  the terminal it came from).
  `panel` and `band`, a spec's own included, are kept within 0.13 of bg's
  OKLCH lightness, and every text role is then moved along OKLCH lightness,
  hue kept, to a WCAG floor on the least contrasting of bg, panel and band
  (fg 7, warn 3.5, the rest 4.5), so no theme, a plugin's included, can hide
  prompt text. The shipped ones are whorl (and whorl-light) plus ports of
  Ghostty's catppuccin, tokyonight, gruvbox, nord, dracula, solarized,
  rose-pine, kanagawa, one, everforest and github, most with a `-light`
  twin, which draws on a light terminal when it is registered, its theme
  itself included;
  `ui.theme` names one: `zemi.config.set` takes any name, since a plugin's
  or the project's theme may be added after it, and a name no theme answers
  to once boot is done draws whorl and is an `/errors` entry; `/config`
  refuses one, and `/theme` picks one with the list open, enter on a row
  repainting the screen in it as the preview. The groups are defined once, for every theme, by the shipped
  `theme.lua` through `zemi.ui.hl.set`, and any later `hl.set` replaces
  one. The palette's `bg` is the app's canvas, which every group without a
  fill of its own sits on and which OSC 11 gives the terminal too
  (reset on every exit), while a `bg` on `Normal` recolors its text alone; every box
  (overlays, prompts, toasts, popups) and the composer sit on `panel`, a
  box's selected row on `band`. A group names a role, hex or an ANSI color,
  or `link`s to another group (neovim's `link`, followed
  8 deep, a cycle draws unstyled); a group takes `fg`, `bg`, `sp` (underline
  color), `bold`, `italic`, `reverse` and `underline` (`true` or a kind: `double`,
  `curl`, `dotted`, `dashed`). Hover draws the `Hover` group (widget click
  segments, statusline pills) or `HoverItem` (list rows,
  menus, memory items; the shipped one underlines, so a hovered row keeps
  its selection's and matches' accent apart) over the text's own style: keys a hover group leaves out
  keep that style, `false` and `"NONE"` clear it, `brighten` lifts the text's fg
  (toward black on a lighter fill) and `replace` draws the group alone. With neither
  group defined, a target underlines. A segment marked `hover = false` takes neither
  group: a clickable body row (a diff line, a tool's detail, reasoning) says a
  click acts with the pointer shape alone, so hovering one restyles nothing. The trust and permission prompts' choices keep
  `Host:Hover` (§8); `Host:` is Go's own, so a Lua segment naming
  one draws unstyled and cannot forge prompt chrome. Go resolves to the terminal's colorprofile
  (truecolor/256/16) itself, since the stock tables tint a near grey: at 256
  colors a color of OKLCH chroma under 0.03 takes the grey nearest in
  lightness and any other the entry nearest in OkLab; at 16 a background
  takes the grey of its lightness and a foreground the ANSI color its
  lightness (a grey) or hue falls in, bright when light, stepping to the
  grey past its background when the two would be one color. `zemi.ui.hl.get(group)` reads
  a group back resolved, and `zemi.ui.widget.del(id)` removes a widget
  shipped ones included. `zemi.ui.hl.set` restyles a group for every theme
  and `ui.theme` picks one, from any origin: the last write stands.
- Key routing: bubbletea key -> normalized string -> Go-reserved cancel
  trigger -> Go-owned chrome (trust prompt, permission asks,
  question and plan prompts) -> the topmost overlay (Lua, or Go-built
  lists like the model picker) -> a focused layout node (its scoped keymaps, then the global ones; an
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
  `zemi.ui.focus.*` calls and the keys that bind them are open to any
  origin, like setting the layout, and a key the user presses runs whatever
  bound it.
- Slash palette: typing `/` in the composer opens a popup listing registered
  commands, matched fuzzily as you type and ranked best first, a name the
  typed one begins leading; `up`/`down`
  navigate, `tab` accepts (fills `/name ` and lists its arguments, or the
  next argument's), `enter` submits the typed
  line, so `/name args` runs the command instead of sending to the
  model; the highlighted command runs in place of a typed name that is
  none or that the user moved off, so what is highlighted is what runs;
  once arguments follow the name the list holds only the names it begins,
  and an unknown name with no row toasts and does not send; `esc` closes and
  `ctrl+space` reopens whatever popup the cursor's word takes. Past
  `/name ` the list holds the command's argument candidates, its Go or Lua
  completer's matched and ranked in one place (`CompleteCommand`) before the
  cap of 50, a completion source's (below) as it returned them, none past
  the last argument its `nargs` takes. The
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
  not drawn and takes no keys, so prose with a `#` in it still submits. A
  popup's top border names what it lists where zemi knows the name: `Files`
  for `@`, `Commands` for the palette; a source's own trigger titles nothing,
  since zemi cannot know what it means.
- One matcher (`hostapi.Match`, over `sahilm/fuzzy`) filters every list a
  human narrows by typing: the palette, argument candidates, `@` files
  and overlays. Every word of the query must match a row: its
  label fuzzily when the label is one token (a name, an id, a path, a
  dotted key), or as a substring when the label is prose (it holds a
  space), since a subsequence finds nearly every line of prose; any word
  may instead appear in the row's other text. Popups under the composer
  rank best first (`hostapi.Rank`); overlays keep list
  order, so sections stay whole and `/sessions` keeps recency. `@` files
  rank a file name the token begins first, then by the score of the name,
  or of the path when only the path matches. Every list draws the matched
  runes of a label in the accent (`listRowsW`, `matchSegs`), bold on the
  selected row. The selection is its own channel: a band (`Segment.Fill`,
  the palette's `band`) under the row, the accent `›` and bold text, in every list
  and every prompt's buttons, so a match and the selection never share a
  color. It changes only colors, never a row's width, and never underlines:
  that means a click acts. Model-facing lookups
  (`tool_search`, a mistyped path's siblings), command dispatch and every
  permission or path check stay exact.

### 6.7 Learning, session, skills

```lua
zemi.session.id(); zemi.session.title(); zemi.session.rename("...")
zemi.session.messages()                -- { id, role, text, json } per stored message
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
`zemi.composer.*` keymap strings), `zemi.health` (`add(name, fn)`,
whose rows join the Go checks in `/health` and `zemi health`, and `list`, which reads
those rows back from Lua),
`zemi.workflow` (`run`, `list`; inside a workflow script `agent`, `parallel`, `pipeline`,
`phase`, `log` and `args` instead), `zemi.lsp` (`add`, `del`, `list`),
`zemi.mcp` (`add`, `list`), `zemi.errors` (`list`), `zemi.project` (`trusted`, `root`),
and `zemi.perm` (`ask`, a plugin needs `perm` in its uses, §9).

`zemi.learn.decide` and `zemi.skills.enable` are open to any origin: the
proposal queue and the enabled set are the user's to change, and the
capability model that matters is the manifest's `uses`, not who calls.

## 7. Storage (SQLite)

```
sessions(id, parent_id, title, profile, created_at, updated_at,
         prompt_tokens, completion_tokens, cache_read_tokens,
         cache_billed_tokens, cost, last_in, run_status)           -- run_status: how the session's last run ended;
                                                                  -- last_in: the context its latest step used
messages(id, session_id, role, parts_json, created_at)          -- fantasy messages, tool and turn telemetry,
                                                                -- compaction tails, goal events and chat rows
                                                                -- (role "chat": a notice, an error, a !command)
todos(id, session_id, content, status, active_form, ord, created_at)
tool_calls(id, message_id, tool, ok, created_at)                -- per-tool counts for the reflector
memory(id, scope, text, quote, provenance_session_id,
       created_at, updated_at, superseded_by, origin)          -- the frozen pre-migration trail (below)
memory_ops(op, id, ts, text, prior_text, quote, session_id, hash,
           superseded_by, scope, origin, prior_bytes,
           prior_truncated, prior_existed, created_at, updated_at)
                                                               -- the ledger: one per scope's DB, part of the
                                                               -- store's schema and migrations
checkpoints(message_id, session_id, path, existed, link, content, created_at)
                                                                -- pre-turn file content (a link's target) for rewind (§8)
proposals(id, payload, created_at)                              -- pending, until reviewed
skills(name, path, source, enabled, created_at)                 -- source: user|builtin|plugin
kv(key, value_json, updated_at)                                 -- zemi.state, saved config picks, reflector marks
```

Ids people and the model type (sessions, forks, agent and workflow runs,
memories and their proposals) are 8 lowercase Crockford base32
characters, 40 random bits (`store.ShortID`); a subagent run's id is its
child session's id, so `send_to` and `/agents` reach a run the process
forgot by an exact lookup. Message ids are 128-bit hex, since nobody types
them.

One file per scope: global DB at `~/.local/state/zemi/zemi.db`, per-project
DBs at `~/.local/state/zemi/projects/<hash of trust root>/zemi.db`, named in
the dir's `root` file (read only when it hashes to the dir's name), the same
repository identity trust uses, so a subdirectory or a linked worktree opens
the same sessions and project memory rather than a blank store. The
global DB holds the user partition's ledger and frozen memory rows,
proposals, skills and kv; a project DB holds its sessions, messages, todos,
tool_calls, checkpoints, the private partition's ledger and frozen rows,
proposals, and the reflector's kv marks. Opening a store sweeps checkpoints older than 30
days; sessions are never swept (zemi deletes a
session only when the user says to: Delete in `/sessions` removes it and its subagent
runs, then VACUUMs and truncates the WAL so the text leaves the disk).

A resume rebuilds the session from the store alone and must read exactly as
the session did when it was left: the transcript rows a model message does
not hold (a notice, a failed turn, a `!command`) are stored rows of their
own, `role = "chat"`, and the context the latest step used is the session's
`last_in`, since only the provider counts tokens and an estimate at 4 bytes
each would read low. A loop is the one thing that does not come back: it is
a running process, not session state (`/loop`).

Indexes: `messages(session_id)`,
`tool_calls(message_id, created_at)`,
`memory(scope, updated_at)`, `sessions(parent_id)`,
`sessions(updated_at)`, `todos(session_id)`, `checkpoints(session_id)`.

The schema is embedded goose migrations under `internal/store/migrations`,
one transaction each, applied on open under a `zemi.db.lock` file lock so
two zemis never run one twice. A change is a new numbered file: it reaches
every store at its next open, with no version stamp and no reset to do.
The baseline is all `CREATE ... IF NOT EXISTS`, so a store made before
goose only gets the version table and keeps its rows.

The memory store itself is not a table but Markdown on disk, entirely
inside zemi's own state, in two partitions:
`~/.local/state/zemi/projects/<hash of trust root>/brain/` beside the
project DB, and `~/.local/share/zemi/brain/user/` beside the global one.
One-liner memories are lines in each partition's reserved `memories.md`
(`- [id origin YYYY-MM-DD] text`, newest first by last write); every other
`.md` is freeform knowledge whose required frontmatter (`description`,
`read_if`, an optional `paths:` glob) derives the injected catalog. No brain
byte is ever created in a repository, so nothing of it can be written by a
pull; committed team knowledge stays in the repo's own files. Every write to
a partition - a memory-tool apply, the reflector, `/memory`, migration,
rewind, the post-write scrub - holds an exclusive lock (flock, or LockFileEx
on windows) on the partition's
`memories.lock` for the whole operation (a blocked writer waits 2s, then
fails; a cross-partition move takes both locks, user then private), inserts
its ledger entries before the file replace - except the reconcile-on-open
migration, which writes the bytes first and the import entries after, so a
crash between re-runs to identical bytes - and replaces files atomically.

The `memory_ops` ledger is append-only and never compacted; nothing reads it
to build the prompt. Line ops (add, supersede, forget, edit, archive, move,
import) carry the line's text, hash, quote and provenance; a write op - the
memory tool's file mode - carries the replaced file's prior bytes, capped at
32 KiB with a truncation flag, so a rewind restores them. Import entries are
permanent tombstones: the marker that keeps a deleted or rewound-away line
from re-importing.

The `memory` table froze at the swap: never written again, no migration
drops it. Its superseded rows keep their text, quote and provenance
forever, its live rows compose into `memories.md` by the reconcile-on-open
migration, and the read path stays as the trail resolver -
`/memory`'s merged list and collapsed trail group, and the ids a stored
context row or a pending `replaces` still names. There is no memory search
and no ranking: every live line reaches the prompt, newest first per scope,
under the per-scope byte budget the write gate keeps it under (§10.4).
A memory about the user carries the user's own words (`quote`), and
`origin` is `user` or `learned` - `/memory <note>` writes `user`, everything
else writes `learned` - which the autonomy gate, the supersede guard
(§10.4), and the injected labels (§10.7) read; since the swap the origin
rides the `memories.md` line and its ledger entries.

## 8. UI core detail

- **Layout**: header (no default widget; the region stays for Lua), chat
  (flex), then the composer (1..8 rows, those past either edge fading toward
  the panel) inside a square frame two rows taller, whose rules carry
  regions: the `working`
  one on top, which gets the whole rule between the corners (the shipped
  `working` widget: the snail mascot a cell in from the left corner, a cell of
  rule either side of it, the state in its eyes alone: swapping while the model thinks or
  tools run, lowered while a reading call, a compaction or a goal check runs,
  `??` while the user is asked, then for a few seconds `^^`
  when the turn finished ok, `><` when it failed and idle eyes when it was
  cancelled; at the right end of the rule the interrupt key
  with the elapsed time, and while the chat shows an agent run's transcript
  that run's agent with `esc back` in its place, both on an idle
  rule too, and that run's own clock while it runs. The block ends a cell off
  the rule's end, a cell of rule either side of it, so nothing to its
  right ever moves and the cells a part grows into come out of the rule on its
  left; a narrow rule sheds the least worth first, sized by the cells a part
  can grow to; the running call names
  no cell of it, which the chat row already says. It reads `ctx.frame`,
  `ctx.ended`, `ctx.cancelling`, `interrupt_armed`, `view`, `editing`,
  `waiting`, `headless`, `busy` (its newest entry, the job the user waits on)
  and `agent.retry`/`compacting`/`checking_goal`),
  the statusline below. The mascot is always drawn and the frame's height
  never moves with agent state, so no turn adds or removes a row.
  Statusline
  (`statusline.left` / `statusline.right` widgets ordered by
  `priority` ascending from each side's outer edge, the right one from the
  right edge, Go joins the ones that draw with a dim " · ",
  overflow drops the highest priority first; the
  shipped left widgets also show an armed quit (`ctx.quit_armed`), a
  pending key sequence (`key_seq`) and the rows below a scrolled chat
  (`rows_below`); only the stuck and errors pills
  are Go-drawn. A boot that
  failed leaves the runtime hot, so no widget, theme, config text or composer
  exists at all: the whole screen left is Go's, and the boot box states the
  error, the log that holds the whole traceback, and the two keys that work
  there, `c` to copy the error and `q` to quit. The
  shipped right widgets (`statusline.profile`, `model`, `effort`,
  `context`, `cost`, `hints`) show the model, the session token tally, the
  session cost when the model spec carries prices (`cost_per_1m_in` /
  `cost_per_1m_out`),
  the reasoning effort, and, innermost
  until the first message, the `/` and `@` keys).
  Overlays float, centered or anchored (`anchor = "bottom-left"` or
  `"bottom-right"`: over the statusline, the border in line with the
  composer frame on the side of the pill that opened it: a Lua picker
  left, `/profile`, `/model`, `/effort` and `/usage` right; the box stands on
  the frame, whose top rule is its bottom border, the label that rule carries
  giving way to it cell for cell). A box takes its width on the frame it
  opens in and keeps it: a menu that resized as the selection moved, or as a
  reload brought longer text, would shift under the reader, so its rows, body
  and hints fit that width and a longer title is cut instead. A Lua overlay has
  every field a Go-built menu uses (`anchor`, `selected`, `follow` and an
  item's `anchor`: moving the selection then scrolls the chat to the
  transcript row the item names and bands that message, opening on the row
  the reader is at, with enter keeping that place and esc putting the chat
  back; the topmost following list answers while a screen a row opened sits
  over it, so the chat keeps previewing the row that screen is about, and a
  row's second key on one letter raises, since a key runs one action; a `body` of styled lines or `markdown`, `render_item`
  rows of `{ text, hl }` segments; a `click` or `effect` there is dropped,
  an item's `keys`, one character each, act on that row from the list and are
  named in the footer of its view,
  an item's `preview` shows whole in a pane under the list while its row is
  selected, the pane as tall as the longest preview (4 rows at most, and the
  list 72 cells wide, like text), so moving never resizes the box, and the
  filter matches it (`/config` puts each key's description there),
  an item's `copy` is the row's value, which `y` copies in place of the row
  (`/model`'s full model id, `/mcp`'s sign-in URL) and enter copies, keeping the list open, when the row does nothing else,
  an item's `section` starts a header row where it changes, and the filter
  matches it and the value with the row, so a provider name narrows `/model`
  to its group and a full model id finds its row,
  and the toggle-list `reload` `/skills` uses: a function returning the
  items, run with `on_select` on the UI worker after enter or space while
  the list stays open, and again when an overlay stacked on it closes, a
  detail view only after it ran something, so a picker opened from it shows its pick at once,
  as does a Go screen's one-line prompt (`/config edit`), whose command runs with the typed
  text before it closes,
  and after a reload; a row that fills the composer closes the list, which would keep
  the keys); Go still draws the box, the origin badge, the
  filter and the hints, where a list's own `keys` are named. Opening the id
  of an open overlay replaces it in place, keeping the filter and the
  selection on its row wherever the rebuild moved it, whoever opens it. Go parses each
  item once (`hostapi.ParseOverlayItem`), and an item or action field of the
  wrong type raises from `open` (§6.0) instead of drawing a row that ignores it
  (a list item's `id` is its own data). A row has one view: its fields and
  text, never a second one of buttons. What acts on it is its own `keys`, one
  character each (`fill`, `run`, or an `id` handed to the list's `on_action`),
  which act on the selected row from the list and are named in its view's
  footer; a key whose `run` opens a screen says `verb = "open"` and leaves the
  list under it, so esc comes back. Go screens key every operation by its
  verb's initial, one letter per act on a screen: `r` restart, resume or run,
  `a` ask or run with arguments, or `/mcp`'s sign in to all, `s` sign in or
  stop, `e` enable, `d` disable, `n` rename, `o` open, `u` update or use now,
  `c` change, and `x` for what cannot be undone (delete, or `/mcp`'s sign
  out, which deletes the saved token); `/health`'s one key per check takes its
  label's initial. A row or key with `danger` (those `x` keys, `/rewind`'s
  `r`; not `/memory`'s keys, which `u` undoes) draws in the danger color, and
  its first press only arms it: the footer turns to `x again to delete · esc
  cancel`, the verb in that color and the keys drawn as keys, the same key,
  enter or space runs it, esc disarms and stays, and any other key, or a click
  or wheel that moves the selection, disarms and then acts; a screen's own
  danger key (a workflow view's `x`) waits the same way, and any click or
  wheel drops it. A destructive command typed without the `--yes` its key's
  run ends in (`/plugins delete x`, `/skills delete x`, `/sessions delete
  <id>`, `/mcp sign-out x`) asks the same way: its screen opens on that row
  with that key armed (`hostapi.AskFirst`, `Overlay.Confirm`), or, for a row
  the list does not hold (a session past the 50 it shows), a toast names the
  `--yes` form; `/memory forget <id>` opens `/memory` on that memory, its
  Forget key armed. In `zemi run` such a command refuses, naming the command
  that runs it without asking (`zemi plugins delete x --yes`). `j`, `k`, `/`,
  `y` and `Y` are the list's own and cannot be keys. A
  screen that manages things (`/skills`, `/mcp`, `/lsp`,
  `/plugins`, `/memory`, `/workflows` and a workflow's view) is a toggle list, so an
  action returns to it rebuilt, and `/mcp`, `/lsp`, `/plugins`, `/workflows`
  and a workflow's view also rebuild in place when a server, a run or a
  slow action changes their state off the key loop (`Overlay.Refresh`, for
  a run a 1 s ticker's `RefreshOverlay` that lives only as long as some run
  does and builds only an open screen), a detail open over them too; `/sessions` rebuilds the same way, so a deleted
  one goes without the list closing. A row's binary state (a skill or plugin on or off, a
  picked option of a multi-select) is one 5-cell switch in the key column,
  `◆ on ` in Ok, `◇ off` or a locked `  on ` in Hint, so a toggle never
  moves text; a tag there (`current`, `follows chat`) names, in the accent.
  Enter's hint names its act in one word shared by every list: `open` a
  detail or another screen, `pick` a value (a picker, `zemi.ui.select`, the
  `@` popup), `run` a command (the palette, `/help`), `toggle` a toggle
  list's row, `fill` the composer, `copy`, and `go` for a following list's
  row with no level under it, whose enter closes the menu keeping the place
  it scrolled the chat to while its esc says `back` and puts the chat where
  it was; a prompt's footer says `pick`
  for its buttons, whichever is highlighted, and `send` for typed text
  (`zemi.ui.input`, a typed answer or note, a multi-select's picks, a plan's
  changes, a question batch's Submit tab, whose digits `change` a question):
  four letters each, so starting to type or moving the highlight never
  shifts the footer. The footer's pairs sit in one order: moving keys,
  enter, space, copy, the screen's own keys (a Lua list's `keys`,
  `/memory`'s `u`, a workflow view's `p`, `+/-`, `x`, `r`, `o`, `s`, and the
  selected row's own keys), esc,
  then scrolling; the screen's keys come and go with its state, so their
  going moves only esc, and the scroll hint comes and goes with the text,
  so it sits last and its cells are trailing blanks while it is away. A
  toggle list, which can grow past its view, keeps scrolling's cells blank
  until it does, so the hint coming moves nothing. A narrow footer drops
  scrolling and copy before the screen's keys. Every hint's label is a
  lowercase word or phrase, the verbs' grammar too: `h back`,
  `o open the view`, `y copy`, `esc close`. A row names its own (`verb`)
  where the list's default would say another: "open" on a row that runs a
  command opening a screen, "run" on `/model`'s "Update the model catalog".
  Space, which toggles a toggle list's rows outside an open filter (in one
  it types, so a query of two words flips nothing), passes a row with a verb
  by, and an "open" row leaves its list under the screen it opens, so esc
  comes back to it (`/model`'s "Choose a model per role"). A list's action rows are
  sentence-case verb phrases above its sections, with no section of their
  own ("Install a plugin", "Follow the chat model"); the values sit under
  section headers, so an action never reads as one of them. `/` starts a
  filter on a list that opened with five rows or more; a shorter one reads at
  a glance and shows no filter row, and a rebuild keeps what it opened with,
  so a row going or coming adds or drops no filter row. While it types, every one-character key types,
  so the footer says only `enter done · esc clear`; enter keeps the query and
  gives the keys back to the list (j and k move in the narrowed rows, `/`
  types on), and esc clears it, then closes. A detail view opens with its
  item's `fields` set as a table (names in a dim column, a field with no value
  left out), then its `detail` prose, and lists nothing: its keys are the
  list's and the row's, and the footer names them; `y` copies the fields as
  `name: value` lines above the prose. A Go screen's body may come in
  parts, each drawn the way its text is written - markdown, a unified diff,
  or a language to highlight it as - and a body its work rewrites (`Live`)
  keeps the reader's place, or follows its tail while they never scrolled.
  A part that names a surface covers its own rows alone, with a blank row
  above and below it, so a call's command (the canvas, sunk into the box)
  and what it answered (the well, a step above the box) read as two bands
  of one block with air between them, and they differ in nothing but their
  surface, as `/jobs view` draws it too. A `status` field colors the status
  words every screen shares: `starting`,
  `ready`, `failed`, `stopped`, `idle` (a language server waiting for its
  first file) and `off` (disabled). A list grouped by source
  (`/skills`, `/agents`, `/workflows`) puts the user's first,
  then builtin, plugin and project, by `lua.Origin.Rank` (`sortBySource`).
- **Layout tree** (`zemi.ui.layout.set/get/rects`, `internal/ui/layout.go`): Lua hands
  over plain data, Go validates it once and solves it every frame on the Update
  goroutine, so the screen never waits on the interpreter. Boxes (row or col, optional
  separators) hold leaves: a widget region (optionally scrolled, by node id), the
  composer (exactly one, never hidden, optionally framed with a region in each rule,
  with a `placeholder` an empty draft shows dim under the cursor),
  the statusline bar, a spacer.
  Each child claims cells, a percent, its content rows or a weighted fill share,
  clamped to min and max; a box too small takes cells from fill first, then percent,
  content and cells, the last child first, down to min and then to 0, and hides its last
  `collapse` child while the mins do not fit. A hidden or collapsed child takes no
  separator. The solver is integer arithmetic and never
  fails. Floats place a region like a neovim float (relative node, anchor corner,
  row/col, size) in list order. A render's `ctx.regions` names the regions this
  frame's solve draws, so a widget knows whether another is on screen. The shipped
  `tui/layout.lua` sets the default: the main column, the composer's frame, centered
  past 124 cells, and in it the header, then the stage, the chat column, centered
  past 100 cells and a row above the
  composer's frame, the composer's rules running wider than the stage so their
  widgets fit; the working rule's key and clock sit at its right end. Any origin may replace the layout, the last one
  set winning. `/reload` keeps drawing the old layout until the replay sets
  one; a replay that sets none draws the Go copy of the default as it boots
  (`hostapi.DefaultLayout`).
  Go keeps the §8 chrome whatever the tree says: prompts and overlays center over the
  stage node, or the whole screen when it is under 40 columns or 8 rows; the live toasts (five at
  most) are one tray of a fixed width at the stage's top, hugging its right edge (three
  cells over it, the rest in the margin, inside the screen), newest first, a section
  per level and title with one row per toast, each leaving when its own time runs out
  (4 s for info, 8 s for a warning or an error), drawn after every other layer: a
  notification is the one thing that never sits behind a box, and it never joins a border
  beside it; completion popups stand in line with
  the composer, ending at the stage's bottom when the stage is above the composer,
  on the composer frame's top rule when it has one, in the draft's rule tint, else go
  beside the composer on the side with room; Lua floats never cover the composer or the alarm
  pills, and without a bar the pills float at the bottom-left. A scrolled region
  keeps its last column for a thin scrollbar and a blank before it, or draws the bar in the
  separator column beside it, its own last cell left blank; its widgets
  render two cells narrower, so the bar appearing covers and rewraps nothing and chat
  text lines up with the composer's; every other scrolling box (overlay
  bodies and lists, prompts, popups) scrolls through the same viewport, fades
  its edge rows toward the panel it draws them on and draws the bar
  in its right border, the track the border line itself, so its text keeps every column
  and never rewraps when the bar appears; the band of a selected row stops at the cell
  before it, never running into the border column. Every bar drags, found by its cells in the last
  frame. Go's chrome lines (box borders, the
  composer frame and its plain rule runs, box separators) join where they touch once
  everything is placed (`App.join`): a line end facing a line becomes a T and a crossing a
  `┼`, in the cell's own color; text, Lua content and the snail art never join. Under a
  modal the backdrop joins before it fades toward the canvas, and a faded line joins
  nothing placed over it, so the top box's border meets only its own lines - and the rule
  it stands on, whose cells under it become that border whatever the rule carried there.
  The rows of the message a following list picked keep their color through that fade:
  the map dims the chat around what it is looking at, so the band reads.
  Widgets render at their node's width and height; a content-sized node gets
  the most rows it could take (its max or the box's cells), not its last height, which
  would never let it shrink. A widget that pads to the whole region, the splash,
  needs the region to itself: while it shows, the chat yields to it
  (`zemi.tui.text.empty_chat`) and the splash draws the pending row in its own
  block, on a row it keeps either way - a row of the chat's there would overflow
  the region and the viewport would scroll the splash up as the boot's work
  comes and goes.
- **Rendering loop**: the state generation is bumped at most once per
  renderer tick, not per tea message (a refresh from any goroutine waits in
  the hook queue at most once); keymaps, widget clicks, one-shot timers,
  async Lua callbacks (job, http, agent) and async event handlers
  bump it when they return, and `zemi.ui.redraw()` bumps it on demand (a
  repeating timer calls it itself, so an idle tick costs nothing). A
  widget a message resized renders on that message instead, so no frame
  draws lines rendered for another size. A render's ctx builds `transcript`, `agent`, `todos`, `recent`,
  `projects` and `instructions` on first read: over a 500-row transcript a widget that reads only scalars renders
  in under 20 µs instead of 1 ms (`BenchmarkWidgetCtx`). A line table a
  widget returned is never changed by it: Go converts one it saw in the last
  render only once and wraps it only once per width, so a frame of a
  streaming reply over 500 turns costs 1.5 ms, not 40
  (`BenchmarkStreamingFrame`). A render returning `true` as a second result
  runs again next frame, that widget alone: the chat fills a long resumed
  session that way, latest messages first, 30 ms at a time but never less
  than a full view, topped with the prompt it pins even when that prompt
  sits hundreds of rows further back, and a region
  scrolled up keeps the last whole render until the new one is whole, so
  the reader's rows do not move. `View`
  asks each visible region for its cached widget lines; a generation change
  renders every shown widget again, a resize the ones whose size moved.
- **Chat transcript**: messages are stored as content blocks (text, tool
  call, tool result, reasoning); tool output is sanitized before render
  (see below); the default chat widget maps blocks to
  lines, applies markdown rendering via glamour for assistant text
  (cached per block id and width; a frame never re-runs glamour). The
  view word-wraps (the widget sets `wrap`), so long lines stay visible
  instead of truncating, and starts at its top row: a transcript shorter
  than the view leaves the blank under it. Scrollback (the scroll keys in Default
  keymaps below, and the mouse wheel) moves the viewport off the tail;
  a scrolled view stays anchored while new lines fill underneath, and a
  click on a row leaves the tail too: a digest opening over its rail
  redraws the rows under the reader's place, never
  scrolling them away. Sending a message (or a `!command`) snaps back to the
  tail, a slash command does not. While a turn's prompt is above the view its first row
  stays pinned over the chat's top row (a segment's `sticky`) as chrome,
  dimmed (`Pinned`) on the panel, ending in `…` when the prompt has more rows,
  and an opened run's digest of that turn (`sticky = "section"`) pins on the
  row under it once it scrolls past that row, until its run's last row
  (`sticky = "end"`) reaches it: the pins stack as one header block, a turn at
  a time, and the next prompt starts the stack anew. A click on a pin scrolls
  back to its row, which lands under the pins above it, and `alt+up`/`alt+down`
  move between prompts and opened runs. Every row of a unit also carries its
  transcript index as an `anchor`, which nothing draws: `/messages` names the
  same numbers, so the picker's selection scrolls the chat to that unit's
  first row and bands all of its rows (`Overlay.Follow`), while the picker
  opens on the unit the reader is at. A banded message is the one thing a
  picker leaves undimmed: the map sinks the chat around what it looks at.
  A settled turn folds each run of activity into one dim digest row, `· 6
  files, 8 commands · thought 8s` or, holding thinking alone, `· thought 3.3s`,
  and nothing else: the rail of steps under
  it, its thinking, and the edits, failures, questions, plans, background jobs
  and commands with a diff it holds are a click
  away, drawn as they drew live. The digest exists only when the run holds a
  call a family counts (a read, a search, a command that changed
  nothing, gone well, an agent run, a workflow, a script, an MCP call under its
  server) or a thought to name: a run with neither keeps
  its whole rail. A run a reply has closed folds this
  way at once, while the reply still streams and after it settles, so only the
  run the turn is working in, the one it thinks and calls in, stays open, and an
  agent run's view folds it the same way, so what a run shows behaves like the
  session's chat. Consecutive notice rows (a background report's head, an
  agent's update line, a plugin's or a loop's send) draw as one block with no
  blank row between them; a burst past four keeps its three newest, the older
  ones behind one dim `… N earlier` row a click opens and folds back, and a
  printed run keeps them all, so a fleet of agents reporting at once never
  takes the chat over; an error or compaction row is a block of its own. The
  live turn and a printed run draw the whole rail, one
  row per step, a continuous bar in column 0, every row's own state mark in
  column 2 and its text from column 4
  (`│ ✓ edit`, `│ ⠋ running label`, `│ ⠋ waiting on workflow review` while the
  turn waits on a call whose work is another agent loop, `│ ✻ thought 8s`),
  a row nested under its step hanging by its `├` or `└` branch in the mark's
  place, the step's state standing for it: a step's unlabelled counted
  calls, two or more of them, as one row saying what they did, marked with
  their own state, each behind a `├` or `└`, the bar kept under a `├` for the
  rows that follow it
  and a gap under the last, its output behind the `⎿` under that, a lone one
  as its own row, a labelled call a row of its own, its label (its command shows
  once opened), a thought a row of its own. The rail itself opens and closes on
  a bar-only row, and another stands between two entries of different kinds or
  on either side of a call that failed or that the user stopped; entries of one
  kind, running or settled, stand together.
  Text a row cuts ends in an `…` with a few
  cells of air after it, so the cut reads as the row's own and never runs into
  the window's edge, while what a row hangs at its right, a diff's counts or a
  running call's clock, keeps the measure's edge. While the model thinks its
  last lines show
  under a `│ ⠋ thinking` row, their top sinking toward the canvas while it
  hides lines above them, and only while its own reasoning block is open, so a
  thought that ended does not go on spinning over the calls that follow it.
  The row opens with the turn itself, before any output, after the same 300ms
  that guards the working rule's block: its label breathes (the shimmer
  effect) for as long as the row lives, and the first reasoning, call or reply
  arrives into it - reasoning flows in under it, a first call replaces it, a
  reply takes its block. A retry's backoff, a compaction, a cancel winding down
  and an ask holding the turn say so through the working rule instead, and the
  row stands down.
  A click on a
  digest opens its run's rail under it, the digest staying as the run's header,
  whose second click folds it back, with the
  rest of the transcript untouched; a run of one thought opens as its text,
  the digest above it already naming the thought. A step row's click opens
  its calls
  nested two cells under it,
  the step's row staying as their head, and closes them again. A rail of eight or
  more rows keeps its last six and folds the earlier ones behind one dim
  row, `… 2 earlier`, whose click shows the whole run and then folds it back
  (a printed run lists everything). A call's and a thought's row never expand
  in the rail: a click opens a view of that row alone, over the chat -
  `/call <row>`, which the row's id names - a box with the call's mark and
  facts (its state, how long it has run, the file or pattern it was given)
  over its content in the form it is written: a command highlighted as bash
  with what it answered apart under it, a file change as the numbered diff,
  a thought as markdown, a question as the answers it got. `o` opens the job
  a command went on as, `y` copies the whole view, and one of a call that
  still runs refreshes as it runs. The reply that ends an
  exchange carries its readout: how long its turn ran, its tokens out and the
  clock; a sentence the model says between runs carries none of it, and each
  call counts its own change on its row (`+4 -1`).
  Under the transcript the chat draws what is still pending as one row whose
  labels breathe, with no spinner and no clock of its own, so what a job is
  never widens the row as it runs. It
  names every handed-off agent and workflow and background job, and the
  `Host.Busy` entries (a compaction, the idle recap, a plugin install, a boot's
  MCP starts) as one part: the oldest's label, `, and N more` when more run. An empty transcript
  has no chat rows at all: the splash draws that row, on the row its block
  reserves for it. The todo block
  follows it, in the order the items were written: the header's meter and a
  window of at most 12 rows (a third of the chat) follow the item in progress,
  saying `… N more todos` under the window and `… N more todos above` when it
  starts past it.
  The rendered view is derived from host-held blocks: the raw message JSON
  is always in the store (and in the log), reachable via
  `zemi.session.messages` and `/copy` (the last reply as the model wrote it),
  so no widget can hide what the model actually received. `/messages` is the
  map of it (`alt+m`): a builtin list of turns, each opening on the rows it
  held (a prompt, reply, notice, error or command) with `l`, and its whole
  activity as one row saying how many steps it took, as the chat draws that
  rail folded. `l` on that row opens the rail's own list, one row per call and
  thought, and the chat draws that run whole while it is up: no row of it
  stays behind the rail's own `… N earlier`, each call keeps its body folded,
  and the call the list names is the one banded, the run this turn is still
  adding to included: its calls are open whole while the list is up and go
  back to the short tail a live rail keeps once it closes. It opens where the
  reader is, the way muffin opens on the symbol under the cursor, not on its
  first row: the levels down to the row the view's middle draws come up
  together, turns then the turn then its rail, and `h` walks back up, each
  level on the row it was left on, while `esc` closes the whole menu. `l` is
  the one way down a level: a row's own view or the run its keys open. `enter`
  on a row of any level is the place itself - the menu closes and the chat
  stays there, the run folding under it - so enter answers "open in chat"
  everywhere. What a row offers past that is its flat keys, `r` to rewind a
  prompt; a call's job opens from its view, never by a second key on the
  list. The list is
  a window on the transcript, not a copy of it: a row that lands while it is
  open shows there at once, and a level the transcript no longer holds (a
  rewind, a compaction) closes. Prompt and reply rows lead with the
  speaker's kind glyph, `▌` in the accent and `◆` in dim, and the state rows
  keep their marks, a marked row drawing no glyph of its own. It follows the
  chat: moving the selection scrolls the transcript to the row the item
  names, centers the message in the view when the whole of it fits (else its
  first row just under the pinned header) and bands every row of it, the
  scrollbar column and a reply's readout aside; `y` copies the row, and `esc`
  closes the menu with the chat back where it was found, the way muffin's
  popup cancels. A view a key opens sits over the level it came from, so esc
  there returns to it as it was. A jump records the row
  it placed the view at, because the viewport alone cannot say which message
  it is when a centered placement clamps at the top or the tail and many share
  one top row: the list opens on that row again while the view still sits
  where the jump left it, and so does closing the menu on a row, which lands
  the reader there. A scroll landing the view at the transcript's end drops
  it, because the reader is at the newest content then, and so do a submit
  and a click. Without it the list opens on the message whose rows
  hold the view's middle row, else the last one above it: a wheel line or two
  keeps the message a jump centered, scrolling on moves to the message the
  middle passes, and a blank line or a run of calls hands over to a message
  still on screen rather than an off-screen one.
- **Composer**: zemi's own Go editor (`internal/ui/composer.go`), not a
  bubbles textarea; `enter` sends, `shift+enter`, `alt+enter` and `ctrl+enter` newline
  (configurable); a large paste folds into a token that expands on submit.
  An input-mode draft starts at the frame's inner edge and runs the width of
  it, so a multi-line draft reads as one block; a `!` shell or `/` command
  draft keeps its glyph on the first row, the rows under it holding the two
  cells it took. A draft taller than the window keeps the cursor row in view
  with a row of headroom below it, and the rows past either edge fade, so
  neither edge needs a row of its own to say there is more. The composer
  stays editable during
  a turn: a submit while the turn runs queues as steering (§4) instead of
  spawning a second turn, and hands off any foreground agent or workflow call
  waiting on its run, so the turn reads the message instead of waiting for it.
  The queued messages the user typed show above it, dim and without a tag,
  where a sent one carries the accent bar: the bar says what went.
  A background report or a plugin send waiting in that queue draws a notice
  row naming it rather than the user's own words, since the whole of it is the
  model's.
- **Default keymaps** (all rebindable by user config except the
  Go-reserved cancel and reload triggers, §6.2; a keymap bound to plain
  `esc` shadows the esc chords, so they are a default, never a guarantee):
  `enter` sends, `esc` twice within 2s or `ctrl+c` once interrupts the turn
  (`esc` in an agent view goes back; the esc chord stops a busy job, and
  when idle - a draft in the composer included - opens /rewind; `ctrl+c`
  moves a draft into the history, and on an empty composer twice within 2s
  quits), `pgup`/`pgdown`,
  `shift+up`/`shift+down`, `ctrl+k`/`ctrl+j` and `ctrl+home`/`ctrl+end`
  scroll the transcript (the wheel too: mouse cell motion is captured because the
  altscreen has no native scrollback), and `alt+up`/`alt+down` jump to the previous or
  next prompt or opened run; `alt+m` opens `/messages`, the map of the
  conversation, which scrolls the chat as its selection moves; `ctrl+d`/`ctrl+u` scroll it half a view, the one step
  a list moves on them and a box's text takes as it overflows, which the footer
  names as `ctrl+d/u scroll`. The pair is never the typed line's, so a prompt's
  line keeps its text and the pair scrolls the text above it, the draft under a
  popup included, and only the
  composer popups keep `home` and `end` for the draft, so a line that types still
  names the scroll keys. A scrolled-back viewport is anchored on the rows it
  shows, not on a row number, so a resize, an expanded thought or a new
  turn leaves the same content in view.
  Commands are slash commands: `/quit`, `/help`, `/model`,
  `/profile [name]` (the agent profile the next send runs as; a picker without a name;
  `ctrl+t`/`ctrl+shift+t` cycle the agent),
  `/new` (close the session: reflector trigger, clean slate), `/sessions`
  (list; `search <words>` finds them in every project, `r` resumes the
  selected session, `n` renames it, `x` opens its Delete, enter offers both),
  `/rename [title]` (names the session; without a
  title, the same one-line input the picker's `n` opens), `/resume [id]` (latest without an id; history and
  transcript rebuilt from the store), `/compact`, `/goal <condition>` (work until
  it holds, §6.3; alone, what it spent; `stop` drops it), `/loop [interval] <prompt>`
  (§6.3; alone, what it ran; `stop` stops it), `/btw <question>` (a side question
  answered in an overlay, on stdout in `zemi run`, from the conversation so far,
  never stored), `/copy [reply|code [n]]` (the last reply or its code blocks as
  written), `/messages` (the map of the conversation, `alt+m`), `/logs` (recent
  `print` and `zemi.log` lines), `/health` (one row per check, problems first: store,
  providers, catalog, model, context window, git, shell, notify,
  project trust, skills, plugins, MCP servers, language servers, reflector,
  pending reviews, recent errors, layout, Lua runtime, and the rows Lua adds),
  `/errors` (recent widget, keymap and event-handler errors behind the
  statusline badge), plus the ones the host steps register (`/memory`,
  `/skills`, `/reflect`, `/agents`, `/jobs`, `/workflows`, `/review`, `/mcp`,
  `/lsp`, `/plugins`, `/projects` (the projects zemi keeps state for; switch restarts zemi there))
  and `/trust` (§5). A hidden `/call <row>` opens the view of a chat row that
  its own click opens, listed in /help and out of the palette's default list.
  `/jobs` is the background processes on a screen of their own, apart from
  `/agents`: running first, each row its state and the clock it has run, `s`
  stopping the selected one without closing the list (so they go one at a
  time) and `x` stopping every one on its second press; enter opens the job's
  output, which follows the tail while it runs unless the reader scrolled up.
  Enter on a help command row runs it, or
  fills the composer when the command needs an argument.
  In `zemi run` a command that opens a screen fails with "/<name> needs the TUI".
  The menus share one list: a row that only a closer look should act on
  (an error, a server, a skill, a workflow, a rewind) opens a detail view
  in place of the list with its full text and the actions as a list, so a restart, a
  multi-agent run or a rewind is never one stray enter; esc goes back.
- **Mouse**: one hit test (`App.target`) serves clicks and hover, so a
  highlight always means a click acts. Go chrome items carry a Go-only
  item index; widget segments carry their `click` id (§6.6) and are
  targets only while their widget has `on_click` and no modal chrome is
  open, which takes every click. The shipped statusline is clickable from
  `tui/statusline.lua`: the profile opens its
  picker, the project path `/projects`, the model the model
  picker, the context and cost `/usage`, pending reviews `/memory`,
  running agents `/agents`, running jobs `/jobs`. The chat widget marks a
  digest (`turn:<first
  index>`, opens or folds the run under it), a rail's or a notice burst's fold
  row (`more:<first index>`, shows what it hides and folds it back), a step row
  (`step:<idx>`, opens its calls), a call's or a thought's row (`call:<idx>`,
  opens a view of that row alone), an agent tool
  row (`agent:<run id>`, from the row's `run` field, opens that run's
  transcript), a workflow row (`workflow:<run id>`, its view) and the job a
  call handed off to (`job:<id>`, its output); the splash's rows resume a session or
  switch to a project, and its tip's keys run their command or fill the composer (a `fill
  <text>` click id, which `zemi.tui.text`'s `run_click` puts there). The stuck
  and errors pills stay Go-drawn, since they report a broken
  Lua layer that could not draw its own alarm; a click on
  errors opens `/errors`, on stuck reloads. A drag selects inside the pane
  or box it starts in: a modal's text, a popup's, a layout leaf or a
  float; pulling past an edge selects to it, and a drag outside a modal
  selects nothing.
- **Permission overlays**: ask-style tool calls freeze the turn, render an
  overlay (diff preview for edits, command preview for shell, command
  and env preview for job asks), and resume
  the pipeline. Concurrent ask-needing calls wait in the host: one overlay
  at a time, in arrival order; the answered box stays drawn until the next
  replaces it in place. The title asks what the call would do, built from the
  request's typed fields ("Run this command?", "Write this file?",
  "Use <tool> from <server>?"). The badge names the subagent asking (`explore agent`, a
  workflow's by its run id, never the model's descriptions), whose code a
  Lua tool runs (`plugin:<name>`) and the queue ("1 of 3"), in what the
  title leaves. Under the title a facts table,
  dim names beside their values like a detail view's fields: the target (`command`,
  `file`, `url`, `workflow`), `why` zemi asked, and `says`, the
  model's own description, quoted and last since nothing checked it.
  Then the command, diff or input, then the choices, a vertical list with their
  keys: `Yes` (`y`, or `o`), `No` (`n` or `esc`), and `No, and tell zemi`
  (`t`, which denies it and every waiting ask, stops the turn and hands the composer back); up and
  down move, enter picks the highlighted one. Every Go-owned prompt
  (permission, trust, plan, a question's options keyed `1`-`9` with the typed answer
  last, and under them its one field, a note on the pick that `n` focuses or the typed
  answer, drawn always so focusing it moves nothing; a batch's Submit tab, whose one
  button stays pinned under the scrolling summary; an overlay's detail view; a
  multi-select stays a list of switches) draws its buttons through one renderer, which puts
  each answer's key in brackets beside its label, the selected answer marked like a selected list row, the band under the accent `›`. The overlay
  is Go-owned and non-replaceable: a plugin cannot whitelist itself. Its
  chrome (frame, answer keys, subject, badge) renders in the active
  palette's roles through `Host:` styles no group or `hl.set` reaches, and
  every palette meets its contrast floors when it registers (§6.6), so a
  theme can recolor the prompt but not hide it; Go draws it itself and Lua text never renders
  inside that chrome, so a widget cannot fake a permission prompt. Modal
  chrome (overlays and prompts) draws over everything but the toast tray,
  as do the completion and palette popups, which are not modal; while a
  modal is open everything outside the top box, plugin overlays and widget
  regions too, keeps its own styles sunk toward the canvas to the same
  ambient depth (`modalFade`), and a box covers its own cells and nothing
  else: the backdrop behind it keeps its text and style, sunk with the rest
  of the screen, so no word of it is cut short and no stripe of the
  terminal's background digs into what the box stands over; at any other time a plugin
  overlay carries a Go-stamped origin badge (`plugin:<name>`, `project:<file>`). While a box
  stays open its width and height only grow (a filter narrowing a menu,
  a question batch's tabs, a detail view, the palette as you type), and a
  click outside it closes it. Opening a detail view is navigation: it keeps
  its list's width, widened to show its breadcrumb title where the screen has
  room, and takes its own height; a field repeating the title's item (a
  project's path) shows only when the title is cut.
- **Sanitization**: every model-influenced string is sanitized by Go at
  the cell boundary before it reaches any widget: tool output, assistant
  and reasoning text (before glamour), overlay diff, command, and env
  previews,
  hook reasons/context, `zemi.notify` bodies and titles, and `/errors` strings.
  Terminal escape sequences (CSI, OSC, and other
  C1 controls) are stripped and C0 controls other than newline and tab
  are collapsed at the same boundary, so streamed content cannot restyle the
  frame, set titles, or fake host-drawn chrome. Plugin `render` output is
  sanitized the same way, and the log's slog text handler quotes every key
  and value holding a control byte, a newline or another unprintable rune,
  so a `tail -f` of the log cannot execute escape sequences or forge entries.
- **Printed runs** (`zemi run`; internal/cli/run.go, ui/run.go): one observer, two
  outputs. stdout is the answer and nothing else: the run's last message
  (the headless prompt tells the model to put the whole answer there, after
  its last call), or with `--json` one event per line, `done` last (the
  lines are specified below). stderr is the run view: the chat and working
  widgets rendered over the run's rows with `UIState.Printed`, which widgets
  read as `ctx.headless` (no click or key hints, the splash stays out, the
  exit region drops its banner). Rows print once they settle, and on a
  terminal the pending rows, the live turn and the working rule are a tail
  redrawn in place under them (`Host.RunFrame`: while a turn runs, a row
  settles only below the running step's start, which a provider retry cuts
  back to). The chat regions render at the stage node's max, the width the
  TUI centers the chat inside (the terminal's width when the layout has no
  stage or it sets none); the working rule draws at the terminal's width.
  The gap between two printed batches is the chat's own block rule, seeded
  with the last row printed above (`Printed.Prev`), and the live tail's
  render (`Printed.Tail`) leaves the pending row to the tail alone. Off a
  terminal it is plain lines; `-q` and `--json` leave only
  diagnostics, which print above the tail since the view is `Host.Stderr`;
  the view renders with no lock held, so a diagnostic the Lua actor writes
  never waits on a render queued behind it.
  Where stdout and stderr are both terminals and the view drew the answer,
  stdout stays empty. An ask nobody can answer is denied (`tools.ByNobody`),
  recorded with its tool, argument and reason (`Host.HeadlessDenials`, any
  agent's), and listed on stderr at the end with the change that would let
  it through; the model reads "no one can approve in a headless run".
- **JSON lines** (internal/cli/run.go, cli.go): `zemi run --json` prints one object per line
  in the order the run drew them, `done` last; a field the event's type does
  not carry is left out, every string on a line passes the same sanitize
  pass at the wire's chokepoint (`writeJSON`), the nested rows included, and
  a run that failed before its turn prints that `done` line alone, no
  events before it.

  | `type` | its fields |
  |---|---|
  | `start` | `session`, `model`, `profile`, `mode`, `trusted`, `cwd` |
  | `message` | `text` |
  | `reasoning` | `text`, `ms` |
  | `notice`, `compact`, `error` | `text` |
  | `tool_start` | `id`, `tool`, `input`, `label` |
  | `tool_end` | the start's fields, `status`, `output`, `diff`, `detail`, and `error` when the call failed or was stopped |
  | `retry` | `attempt`, `in_ms`, `error` |
  | `ask` | `tool`, `subject`, `arg`, `reason`, `why` |

  The `done` line carries `result` (the run's last message), `is_error` and
  `error`, `session`, `duration_ms`, the run's own `usage`
  (`input_tokens`, `output_tokens`, `cache_read_tokens`, `cost_usd`),
  `compactions`, `denied` (each `tool`, `subject`, `arg`, `reason`, `why`) and
  `tools` (`id`, `tool`, `input`, `status` and `error` per call that ended).
  `zemi sessions` (`-a` for every project, `search <words>` across all of
  them), `zemi projects`, `zemi plugins` and `zemi health` take `--json` too,
  one object per row:

  | command | fields per row |
  |---|---|
  | `zemi sessions` | `id`, `title`, `updated`, `cost_usd`; `-a` adds `project` |
  | `zemi sessions search` | `id`, `title`, `updated`, `project`, `hits`, `match` |
  | `zemi projects` | `root`, `trust`, `used`, `sessions`, `memories` |
  | `zemi plugins` | `name`, `version`, `local`, `enabled`, `source`, `uses` |
  | `zemi health` | `level`, `name`, `detail`; an `error` level makes the exit status 1 |
- **Golden tests**: headless mode renders the frame's segment rows to a string;
  golden files per widget and per full frame via `charmbracelet/x/exp/golden`
  (`go test ./internal/ui/... -update` regenerates).
  A scrolled view sinks the rows at an edge content lies past toward the
  surface it sits on: a region's toward the canvas, a box's and the
  composer's toward the panel, which the fill a box puts its rows on carries.
  A region fades the `fade` rows its layout node sets (the shipped chat uses
  4), a box and the composer two, and none more than a third of the view;
  deepest at the edge - under the pins while rows are scrolled above, and at
  the bottom while scrolled up - so each edge says there is more, easing off
  as the last rows come into view. The
  pins, the bar, the composer's cursor row and the rows a window left empty
  never fade, and a list reveals its selection a fade's rows clear of the
  window's edge, so the ramp never covers the selected row. A widget may sink
  a block it clips itself by setting `fade` on those rows (a segment field):
  the deeper of the two sinks holds.

## 9. Permissions

- Model: everything runs; the user stays aware through the ask overlay,
  which only a plugin's gate-steering opens. The permission system is a
  UX feature: it keeps the user aware of what the agent is doing, it is
  not security isolation, and it never parses a command to judge it. A
  run that must not touch the machine runs in a container or VM.
- An action is everything but a read: `fs.write`, `tool.shell`,
  `tool.lua`, `tool.workflow`, `net.<host>`, a Lua tool (`tool.<name>`),
  an MCP tool its server does not mark read-only (`Read` on the request),
  `memory.write`. Reads (`fs.read`) settle on their own.
- Subjects label the ask and the `tool.denied` event.
  A request carries tool, subject, arg, reason, preview, description,
  agent, owner, script; the pipeline fills read-only and the script
  name.
- The gate (tools.Pipeline.Check): a read-only agent's action denies,
  except a write in its scratchpad; then the veto settles: a denial
  refuses, an ask decision runs the ask path; what survives runs.
- The vetoable `permission.request` event runs at the gate, once per
  action request; a handler denies, or returns `decision = "ask"` to
  run the ask path. 50 ms, silence on timeout (§4). A rewritten tool input
  re-enters at the gate. Registering a handler and `zemi.perm.ask` both
  need the manifest's `perm` word: steering the gate is a declared
  capability, and enabling the plugin is the consent. A plugin recipe
  that restores a nervous gate is a `permission.request` handler
  returning `{ decision = "ask" }`.
- Asks reach the user one at a time (`Host.askMu`); no Lua runs under
  the ask lock. An ask no UI can answer (zemi run) fails closed
  (`ByNobody`) and the denial is recorded for the run's summary.
- `zemi.perm.ask({subject=, arg=, reason=, preview=})` is a plugin's own
  ask from a task: the shipped overlay, `permission.resolved`, headless
  failing closed. `:wait()` answers allow, deny or told (the user denied
  and said what to do instead).
- Host-API calls from Lua never open the overlay (§6.5): the gate
  settles them, a read-only agent's writes deny, everything else runs.
  A plugin's declared capability (`uses`) is checked before the gate -
  the manifest is the consent - and a call it does not declare is
  refused there, naming the line to add.
- A Lua tool run asks as one action; once approved, its inner `zemi.*`
  calls ride the approval (`luaApproved`), and each `zemi.tool.call` it
  drives gates like the model's own call.
- Path handling (perm): the file tools resolve once (`perm.Resolve`)
  and the tool opens that resolved path, never input's again. NOTE: only
  the file itself is what the check proved; a parent swapped for a link
  after the resolve still redirects the open (grepFile's NOTE says the
  same).
- read-before-write stays: write refuses a file the session has not read
  unchanged (an edit proves its text with one exact match instead), so a
  command the model ran or the user's editor is never silently dropped.
- Safety beyond the gate: the sandboxed Lua interpreter (§5), project
  trust gating what a repository may load (§5), and the child-process
  discipline of `internal/proc`. Nothing else stands between the model
  and the machine; that is the deal.
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
3. **Structure (config)**: profiles, prompt sections,
   keymaps, widgets. The learning loop proposes none of it: these are
   edits to files in the config dir, made by the user or by the agent
   through the permission-gated file tools (§6.1).

Memory enters through the intake policy below (under `ask` its queue is the
verify leg; supersede and delete via `/memory` are its rollback).

### 10.2 Capture

Everything is already captured by the store: transcripts, tool calls with
outcomes, denials, user edits to the agent's output, permission answers,
session summaries. Additional signals: explicit user teaching
(`/memory this project uses make check`), `tool.post` outcomes, and
"corrections" (user rejects a permission or reverts an edit; both recorded).

### 10.3 Distillation: the reflector

The reflector is Go (`internal/hostapi/reflector.go`), not a Lua plugin and
not a profile: one tool-less model call over a closed or idle session. With no
tools it cannot act at all, so its only output is the proposal document below.
Triggered on `session.close` (`/new`, `/resume`), once an open zemi's session
has had no turn for ten minutes (the same idle the sweep waits for, so the
close finds the delta seen), and by a startup sweep
(once per process, not on a reload) over the newest sessions nothing has
touched for ten minutes, one model call, so quitting never waits on a model
call and a session a quit, a crash or a `zemi run` left behind is still read;
`/reflect` runs it by hand. The sweep records every session it listed with
the message count it held then, and a close skips a session still at that
count: one process reads a session's rows once, so a sweep plus a `/resume`
and a `/new` that added nothing cost one round rather than two, while a
session that grew still reflects at its close.
It reads the delta since the seen count (kv
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
text, and this session's autonomy would store a copy) + the memory index
the partitions' files and ledgers hold (§7), each scope's budget use and
the quote under a memory that has one, so an older near-duplicate is
replaced rather than added again + the
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
user rows and a miss drops the entry, a near-duplicate of a live or pending
memory is skipped, and `replaces` supersedes the memory it names once the
new one is stored. An entry with `replaces` set and an empty `text` retires
that memory: the reflector read in the session that it stopped being true.
Under `ask` a retirement queues as a forget proposal (a copy of the memory,
shown as `forget: <text>` in `/memory` and with `forget = true` in
`zemi.learn.list()`), since deleting loses data; under `auto` it applies at
once, except a memory the user saved, which the reflector never touches -
accepting a queued forget deletes the memory, rejecting keeps it.

The reflector is constrained by prompt and by pipeline: it can only emit
proposals; application happens in the host. Its view of live
memories - the dedup and the retirement targets - is the partitions'
`memories.md` lines plus the ledger (§7), and each entry it applies goes
through the same gate and lock as a memory-tool write, ledger first. An
entry the gate passes applies, one it refuses under `ask` queues as a
pending proposal, and a file apply that failed (the partition lock held, a
rename error) queues too: only when every entry has applied or queued is
the delta marked seen, so nothing a run proposed is lost. Memories go
through
the intake policy (§10.4) into the brain; there is no reflector scratch
directory and no path into the config dir, and it drafts no skills: a
procedure is a recipe the live agent writes under the user's eyes. The
reflector runs detached with a timeout; a run cut off by exit is retried
by the next startup sweep, since the session still has unseen messages.

### 10.4 Proposal, review, apply

- Pending memory proposals persist in their scope's store (`proposals`
  table) until accepted or rejected, so restarts never lose them; the
  statusline shows how many wait for `/memory`. `/memory` is a toggle list
  (§8): the suggestions, then this project's memories and everywhere's -
  the lines the partitions' `memories.md` hold, quote and provenance read
  from the ledger (§7) - each section title counting its rows and a scope's
  naming its share of the budget; a row is one line, and the preview pane
  and the detail hold the whole text, its quote, origin and age. The ids
  the budget cut or a supersede removed, and the frozen table's superseded
  rows, are one collapsed trail group at the end, origin- and date-labeled,
  the ledger keeping their text; their ids still resolve for `replaces`,
  forget and the fork deltas. A row's keys act at once: `a`
  accepts a proposal, `d` drops one, `f` forgets a stored memory, `s` moves
  one between this project and everywhere (deleted from its own store
  before the move-in entry and the other store's write; a crash between
  leaves the move-out entry explaining the absence; a copy already there
  merges, keeping the mover's attestation when the mover is the user), `o` edits one in the
  user's editor (the saved text passes intake again as user origin with
  the same scope and quote and keeps the id, an edited suggestion is
  stored, empty or unchanged text changes nothing), and `u`, named only
  while there is something to undo, undoes the last of them, a saved edit
  included, newest first and to the age a memory had, until the list opens
  again. Since `u` undoes them, no key there arms; only a typed `/memory
  forget <id>` opens with its Forget armed. The row keys run `/memory
  <verb> <id> --yes`, a form no note takes, so `/memory` with any other
  text keeps that text as a note.
- **Memory intake is host-side**, regardless of autonomy: a length cap (300
  chars, and 160 for the quote), provenance required, the quote scrubbed
  like the text, and intake-time secret scrubbing (a span matching a key
  shape or above an entropy threshold in the quote is redacted with a
  `[redacted n chars]` marker; one in the text refuses the memory, since the
  fact it named is gone with the secret, and the error says to name the env var
  instead). Text phrased as instructions ("always run X", "ignore previous
  instructions") is flagged by a recall-biased heuristic pass (imperative
  verbs, second person, instruction-override phrasings) and, under `ask`,
  routes to the review queue; brain lint flags a shaped line wherever it
  landed. A
  near-duplicate of a live memory or a pending proposal in either scope
  (`learn.Similar`: 75% of the content tokens shared) is refused with the id
  and text it repeats, so the model merges the two through `replaces` (a
  pending one is named as queued; the user's own text is refused only word
  for word). Anything about the user carries `quote`, the user's own words:
  a quote that
  is not in what the user typed in the session (compaction summaries,
  attachments and a plugin's `zemi.agent.send` text aside) drops the
  memory. Each scope has a 3 KB prompt budget (§10.7), and the write gate
  enforces it by cutting, not refusing: a model write that would push its
  scope past the budget applies with its oldest learned lines cut from
  `memories.md` into the ledger - the cut skips user-attested lines, the
  tool result names what it took, and the ids still resolve for `replaces`
  and forget. A learned write that cannot fit even then lives in the ledger
  trail alone (an add and an archive entry, `memories.md` unchanged): query
  recalls it, the trail group in `/memory` lists it, promote graduates it
  into a brain file. The user's own writes refuse rather than cut or trail:
  `/memory <note>`, accepting a user-origin one, a move into that scope and
  an edit that grows past it refuse and point at the oldest in `/memory`,
  since the user is there; nothing silently drops a user-attested memory.
  The `## Memory` header (§10.7)
  tells the model to follow a preference the user stated and to check what
  any other line names before relying on it.
  Residual risk is stated plainly: a learned line can still sway a credulous
  model; provenance, quote check, scrubbing and `/memory` narrow that
  window. `replaces` is the one edit: text with it supersedes
  the old memory, empty text with it forgets. `/memory forget <id>` is the
  other review action: model proposals may never silently drop a
  user-attested (`/memory <note>`) memory - the model's forget of one asks
  at the permission gate, and under `auto` the reflector never retires one
  (§10.3).
- The `memory` tool's file mode writes a private-partition brain file -
  `path` inside the partition's dir plus `content`; the reserved
  `memories.md` refuses ("the memory tool owns this file"), and the user
  partition refuses whole ("user-brain files are written with the file
  tools; those writes ask"). The injected surfaces get the one-liner
  treatment: `description` and `read_if` are stripped and capped like a
  one-liner's text (shaped or oversized queues under `ask`), the secret
  scan and scrub
  run on the content, and the body itself is exempt - it is read lazily,
  never injected. Under `auto` a write applies, replacing what is there;
  under `ask` an overwrite of a file this session
  did not write queues, and so does one whose bytes changed since - the
  gate holds the session's write set, every apply and every approved
  file-tool write recording the file's hash and clearing on rewind. A
  settled overwrite keeps the replaced file's prior bytes in its ledger
  write op, capped with a truncation flag, so rewind restores what
  checkpoints never saw.
- The `memory` tool's promote mode moves a stored line into a brain file in
  its own partition - `promote` is the id, `path` the bare .md file name,
  and `description` and `read_if` both required (a catalog line without
  either renders the self-repair note and never triggers): the body is the
  line's text byte-for-byte, the frontmatter composed from the two fields.
  The frontmatter passes the file-mode intake at propose time, before
  anything queues or applies. The pre-flight - the trail holds the id with
  neither a forget nor a serving promotion on it, the name is free, the
  catalog would still render the file (`CatalogAdmits`, the render check at
  its sorted position) - refuses with nothing written at propose and again
  at accept, `brain.Promote` the one entry point. No new op kind: the
  supersede entry's `superseded_by` holds the file name and a paired write
  op records the creation, ledger first under the partition lock (§7), so
  rewind, lint and the trail replay it unchanged; the id still resolves for
  replaces and forget through its trail, and a forget of a promoted id
  refuses while the file exists ("promoted to <name>; drop the file"), so
  the catalog line cannot outlive the trail's explainer. Under `ask` a line
  the user saved, or one in the user scope, queues - the accept is the
  approval the homes line (§10.8) asks for - while `auto` promotes any
  line. Accept pushes its own undo,
  not the /memory forget's: the file goes only while its bytes still match
  what promotion wrote, a live line is re-prepended with no ledger entry,
  and a ledger-only undo journals an archive entry carrying the text under
  the literal session id "user", which no rewind queries, so rewinding the
  accepting session never resurrects a cut line live past the budget. The
  tool's query mode searches the live lines and the ledger trail (newest op
  an archive or a supersede carrying text) in both scopes by text or id; a
  forgotten id and the frozen table's superseded rows stay out, so every
  hit id the result's footer names is actionable. Both modes ride tool
  results, never the pinned prefix (§10.7), and a replace of a promoted id
  proceeds, its result naming the file the text moved into.
- A recipe (§10.5) is written with the file tools. A recipe is instructions a later unattended
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
- A brain file the file tools write - write, edit, an LSP rename's
  secondary targets - is an ordinary write, and so is Lua's
  `zemi.fs.write`: both pass the same gates. The reserved `memories.md`
  carries its own reason, "the memory tool owns this file", with the file's open lint
  findings named in the ask. Once approved the write gets the recipe
  treatment: the secret scrub runs after the write, the instruction-shape
  check is a lint finding, not a rewrite (dropping content the user
  approved is the review queue's job), and a scrubbed `memories.md` writes
  its edit entries under the partition lock, so the ledger's hash trail
  stays honest.

Autonomy is one knob:

```
memory.autonomy = "auto"      -- memories the model or the reflector propose
                              -- store at once when policy-clean; "ask" queues them
                              -- for review in /memory instead
```

`/memory <note>` is not governed by the knob: it is the user's own
keystroke, stamped `origin = user` (§7), and writes immediately in both
modes. The knob covers model-proposed memories only: `ask` queues them;
`auto` stores and applies them - one-liners, brain-file writes, promotions
and forgets of learned memories alike. The quote check (§10.4) and the
near-duplicate refusal still apply, and a memory the user saved is never
dropped automatically: the model's forget of one asks at the permission
gate, and the reflector never retires one (§10.3).

### 10.5 Skills

Skill format (agentskills.io compatible, crush parity):

```
skills/
  local-integration/
    SKILL.md            # yaml frontmatter: name, description, user-invocable,
                        # argument-hint; body = instructions
    zemi-skill.lua      # optional: zemi.tool.add / command.add on enable
```

Discovery merges three sources, the higher-trust one winning a name
clash: user (config dir, then `~/.agents/skills`, the dir other agents
share, whose `zemi-skill.lua` never runs since installers write there)
over builtin (zemi's own three: `zemi`, whose SKILL.md routes to
reference files beside it that the model reads on demand, `sessions`,
which finds, reads and resumes a past conversation through the `zemi
sessions` commands, and `codescan`, the recipes for answering a
many-file question in one lua script; embedded under `runtime/skills/`, rewritten to
`<data>/builtin-skills/` on every sync, so `/skills` offers them no
editor) over plugin (each enabled plugin's `skills/`, by plugin name, body
only: its `zemi-skill.lua` never runs). All start enabled and toggle; a
plugin skill's flag goes with the plugin when it is turned off. The model sees name +
description in context and the full body on demand through the `skill`
tool. A skill whose frontmatter says `user-invocable: true` is also the
command `/<name>`, registered after every other command under the
skill's origin, never over a builtin command or one of its own origin,
and over any other by the registry's last-writer rule. Every enabled
skill is in the index and the skill tool: Claude's
`disable-model-invocation` is accepted and ignored, and the command fields
are the skill's own. `/skills` browses, toggles (toggling a skill with a command
reloads config, which rebuilds the commands) and uses them (a row's
detail carries the whole body and opens it in the editor). Discovery lists only `enabled` skills to the model, and enable
goes through the host API (`zemi.skills.enable`, any origin).

A trusted project's skills (`.agents/skills` at the repository root, the
dir other agents read) join
the index after those and lose every name clash. They are
body only (a `zemi-skill.lua` there never runs), enabled with no toggle,
and kept out of the `skills` table, which is keyed by name across
projects. They are scanned when the trust decision is set (boot, reload,
`/trust`), never per turn, and the pinned prompt (§6.3) keeps the index
a session was built with. An untrusted project adds
none, like its instruction files (§5).

Recipes are the third source: a trusted project's private skills under
`<state>/projects/<hash>/skills`, for exact procedures too long for a
memory. They never enter the repository, and they behave like project
skills (body only, no toggle, no
row), losing a name clash to those too. The index lists the recipes found
when the prompt was pinned (§6.3), while the `skill` tool and `/skills` read the dir on every call: a recipe
written mid-session loads at once and never moves the prompt. Loading a
skill counts as reading its file, so a correction is one `edit`. Only a
recipe has a Delete action in `/skills` (`/skills delete <name>`), which
rebuilds the prompt: the index moves once, by the user's own act. The
reflector's prompt lists the project's skills and recipes by name and
leaves a repository's own procedure to the live agent's recipe.

### 10.6 Evals

There are none: no eval harness, mock provider or CI gate.

### 10.7 Context assembly (how learning reaches the model)

The system prompt is the same for every session of a user in projects
of the same trust, so a provider that caches a shared prefix keeps it and
the tool definitions after it across sessions: the base prompt + **skills
index** (name + description only; the user's, builtin and plugin skills) +
the user's `<config>/AGENTS.md` + Lua sections (§6.3; a trusted project's
own Lua can add one). What varies by project or session follows it as the
context message, a user-role message before the conversation that the
session stores and a resume sends again byte for byte while the brain is
unchanged: the repository's context files (AGENTS.md, else CLAUDE.md,
injected verbatim) + the project's skills and recipes + the environment +
**# Brain** (§7), one section in the slot the memory digest held:

- the loop text, static, naming the partition roots that exist;
- the derived catalogs, one line per freeform brain file from its
  frontmatter, `### user` in every project (user-owned state a pull cannot
  write), `### private` in trusted projects only, each cut at 4 KiB;
- `## Memory`, the two contract lines, then the live lines, this
  project then every project, newest first, each scope cut at a 3 KB budget
  the write gate keeps it under (§10.4), each line prefixed with id, origin
  and age in days; no relevance ranking.

The whole section is built once at session open and rebuilt only at
compaction and at a mid-session trust flip, never per send; the context
key hashes the render, and the stored context row replays its bytes while
it matches, so a memory written mid-session surfaces at the next rebuild
and no cache is broken before then. Each send keys its calls on the session
(`prompt_cache_key`, `x-session-affinity`). A fork (§6.3) sends the caller's
own prefix under the caller's own key, tools by name and schema, system
prompt and context message included, so the provider reads that conversation
out of its cache instead of writing it again; that pinning is what makes a
fork cheap to start. The whole prompt, tool descriptions and the
context message included, is pinned at session start (§6.3) and rebuilt
with a fresh environment and brain at compaction (triggered before a turn
or at a step boundary once the context the last step used, cache
included, reaches `agent.compact_at` percent of the window, at least half
of it, never when the window is unknown; once after a
provider's context-too-long refusal; or by `/compact [focus]`: Go clears
old tool output and, when that is not enough, summarizes all but a
verbatim tail (at least the latest user message on, when a turn came
before it) with one model call (asked after the runner's own prompt and
unpruned history when the compact role runs the conversation's model, so
it reads the provider's cache; a flat transcript to a separate compact
model, or when that call gives no summary), stores the summary (an unfinished todo
list follows it, outside what the next compaction merges) and a copy of the
tail after it (`/resume` scans for the last summary row and starts from
it), and re-runs the brain assembly, §7, with the environment section and the
rest of the prompt (§6.3) pinned again; three failed or
ineffective auto compactions in a row stop it for the session), which keeps
provider prompt caches warm; under "## Memory" a preference the user
stated holds unless the conversation or project instructions say
otherwise, and any other line carries the cue that it held when it was
written, so a file, command or flag it names is checked before the model
relies on it. Memory lines are
labeled `user` or `learned` from their origin, so users can audit
what the model was told and which of it the agent wrote itself. A brain
file whose `paths:` globs (rg-style, as the file tools match them: `**` crosses
directories) match a file the session reads or edits earns a
one-line pointer as tool-result text, at most three per result, once per
conversation - outside the pinned prefix, so recall never moves the cache.
The memory tool's promote and query answers ride the same surface: a
promotion's result is tool text, a search's hits are tool text, and the
query hint a budget cut names rides the tool result only.
Context
files are injected verbatim, and only in a trusted project (§5); an
untrusted one gets the user's `<config>/AGENTS.md` alone. A pull can
still change a trusted repository's files, which the trust decision
accepts like its build scripts. `agent.assemble` handlers add sections:
each returns a string or
`{ title, text }`, capped at 16 KiB per section and 64 KiB in all, rendered
below the built-in sections; no handler can drop the brain section, the
skills index, or the untrusted-content framing.

### 10.8 What the loop cannot do (hard lines)

- Never modifies Go behavior; if something needs Go, that is a zemi feature
  request, not a self-modification.
- Never loosens what a prompt or gate asks, never disables
  telemetry, never edits its own proposals after review begins.
- Never writes outside its homes - the memory store, the brain partitions
  (private via the memory tool's file mode or an approved file-tool write,
  user via the file tools only when the user approves; a promotion
  relocates an attested line into a brain file of its own partition), the
  skills dir.
  Anything else is a normal tool call under normal permissions.
- Only user config changes `memory.autonomy`; no Lua path skips a gate.
  `ask` queues what the model or the reflector propose for /memory;
  `auto` stores it, and never drops a memory the user saved (§10.4).

Most lines are enforced in code, not by prompt: the intake policy (§10.4),
the host-only proposal channel (§10.3), the user-only config.set
(§6.1), and the reflector's tool-less model call (§10.3). The homes line
rests on the memory tool's channel plus the model's instructions: a brain
write is the memory tool's file mode or an approved file-tool write, and
the AfterWrite hook scrubs what the user approves.

## 11. Error handling and observability

- Every Go->Lua boundary is a `pcall`; errors are logged and show as a
  statusline flag; a `/errors` overlay lists recent handler failures with
  file:line (from Lua `debug.traceback`). A failed reflector run is
  recorded there instead of toasting. Lua code and runtime failures are
  recorded by their callers under a slug source (`command boom`, `keymap
  ctrl+t`); a command error that is how it was asked (`ErrUsage`: a bad
  argument, nothing to act on) is a warn toast only. A toast shows
  at most three body rows and never a stack trace; a cut body ends in
  "… N more lines", and "/errors" after it when the error is recorded
  there. With no UI a warning or an error prints to stderr as
  "zemi: <title>: <body>".
- Registration conflicts (duplicate widget id, keymap collision) log at
  load with origin path, the last registration winning, like nvim's plugin
  loading noise but structured. One plugin can neither
  replace nor remove another's: event-handler ids, profile
  ids, builtin tool names, command names, and widget ids. Builtin tool
  names and command names are reserved, so no later registration shadows
  them and `/reload`/`/RELOAD` cannot diverge. Command names
  collide case-insensitively: a registration whose name differs from an
  existing command only by case is refused, so `/reload`/`/RELOAD`
  cannot shadow the reserved `/reload`. MCP tools are named
  `mcp_<server>_<tool>` (64 chars max) and gated per call as
  `mcp.<server>`; servers come from user config only. When two tools map
  to one name, the first server's tool is kept and the rest are skipped
  with a logged warning. Past ten MCP tools in a run's set, every one stays
  registered but a step offers only the `tool_search` tool (its
  description a catalog of names by server) and the tools a search loaded,
  at most five per search, kept per session and per subagent run. A lua
  script reaches any of them by name, deferred or not, through the same gate,
  and `zemi.tool.list()` names the run's MCP tools beside its own.
- Logs: `zemi.log` to file + optional in-app pane; tool calls (each with
  its decision's source: a plugin's declared capability, a hook's veto,
  the user, or a fail-closed default), decisions
  and memory writes are all logged with IDs, so the reflector and the user
  can audit the loop end to end.

## 12. Repository layout

```
main.go                     the entry: one os.Exit
internal/cli/               the front end: wiring, boot steps and the TUI launch
                            (boot.go), the command line (cli.go, cobra), zemi run:
                            the loop, the answer or the JSON events (run.go), zemi
                            plugins test: the copy, a boot per case, the report
                            (plugintest.go)
internal/lua/               interpreter thread, Lua sandbox, origins, marshal, require
internal/hostapi/           zemi.* namespaces and the host services behind them:
                            turns, sessions, compaction, subagents, workflows,
                            agent runs, memory, reflector, skills hosting,
                            permissions, mcp and
                            lsp wiring, rewind, scratchpads, health, export,
                            zemi.test
internal/agent/             the fantasy runner: one turn, steps, stops, retries
internal/classify/          judging data with a model: protocol, logprobs, schema
internal/tools/             builtin tools (fs, shell, jobs, net, misc) and the tool
                            pipeline (validate, tool.pre, permission gate, run)
internal/perm/              path resolution, the pad, glob and subjects
internal/ui/                program, regions, widgets, themes, palette, overlays,
                            composer, mouse, and zemi run's view of a run
internal/learn/             memory intake policy: caps, scrubber, near-duplicate check
internal/brain/             the memory store: partitions, memories.md, the ops ledger,
                            render, lint, lock, migration
internal/skill/             SKILL.md discovery and parsing
internal/store/             goose migrations (embedded SQL) and queries
internal/config/            dirs, settings store, defaults
internal/mcp/, internal/lsp/   the two clients, each with its own test server
internal/proc/              every child process (own session and process group) and hardened git
internal/filelock/          exclusive advisory file locks: brain writes, store migrations
internal/plugin/            plugin install, update, check and delete over plain git, and on/off
internal/glob/              rg-style glob matching for the file tools, LSP and brain paths
internal/sanitize/          escape stripping at the render boundary (the log's slog handler quotes)
internal/logging/           the slog log file, one per launch
runtime/lua/                embedded Lua userland (tui: widgets, themes)
runtime/skills/             the builtin skills (zemi, sessions, codescan); runtime/workflows/, the builtin ones
runtime/types/              the LuaLS `---@meta` description of the whole zemi.* API
```

## 13. Open questions

- Credential storage and auth flow (keys read with `zemi.env.get` today).
