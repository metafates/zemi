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
`ZEMI_DATA_HOME`, `ZEMI_STATE_HOME`, `ZEMI_CACHE_HOME` (see `internal/config/dirs.go`). Most tests don't need
this — they inject `config.Dirs{...}` with `t.TempDir()` directly.

The project trust prompt and `~/.config/zemi/init.lua` only exist on a real machine;
tests boot with empty dirs, so code that requires them needs explicit setup in the test.
A test host is untrusted: call `h.SetProjectTrusted(true)` when the test needs project
instructions, language servers or settled builds.

## CLI (cli.go)

`zemi` opens the TUI; a prompt argument is sent as the first message. Flags: `-p`/`--print`
(answer on stdout and exit, with `--output text|json`), `-c` (continue the latest session),
`-r <id>`, `-m <provider/model>`, `--mode manual|plan|auto|yolo`, `--trust[=false]`, `--health`, `-v`. Piped
stdin is appended to the prompt, and is a usage error without `-p` (the TUI needs the
terminal). Flags apply after boot so they win over `init.lua`, except `--trust`, which the
trust boot step reads (`options.trust` is nil unless given, found with `fs.Visit`). `--health` and `-p` boot the
same step list and skip only the TUI: `--health` prints `HealthRows()` and exits 1 if a check
errored; `-p` runs one turn, then reports on stderr how many tool calls the gate denied
(nobody can answer an ask headless, so `deniedHint` points at `--mode auto`, at `--trust` in an
untrusted project, and at an allow rule when auto already left the call to the user or it
asked to run outside the sandbox). A headless run in a project with no saved trust decision runs untrusted
and prints one stderr line saying so (`headlessTrust`). `/health` and `--health` read the same rows: the Go
checks (store, model, git, ripgrep, sandbox, reflector, learning, skills, lua, errors,
trust, lsp) plus whatever Lua registered with `zemi.health.register`.

A first run writes a fully commented starter `init.lua` (`config.Dirs.Ensure`) because zemi
ships no providers and an empty config cannot answer a prompt. The logger keeps the newest 20
files, one per launch. Config reload is not fsnotify: `watchConfig` walks the config trees
once a second and reloads when the fingerprint (path, size, mtime) moved and no turn is
active; an untrusted project's `.zemi` is not watched. A replayed `zemi.config.set` that
writes what the last boot wrote is skipped (`Host.cfgWritten`), so `/model`, `/effort`,
the mode cycle and `-m`/`--mode` survive a reload until the config changes that key.

## Architecture and control flow

`main.go` is wiring only. Boot order (each step is a `lua.BootStep`, see `steps` in `main.go`) — order
matters:
host namespaces (`zemi.*`) -> builtin commands -> memory, reflector and skills commands -> agents (+ tasks),
workflow (+ `/review`), mcp, lsp and window commands ->
`require("zemi.defaultui")` (shipped theme/widgets/keymaps in `runtime/lua/zemi/`) ->
project trust (`trustGate.step`, before any config that can start an MCP server) ->
plugins -> user `init.lua` -> authored skill hooks -> a trusted project's `.zemi/init.lua`
(`projectInitStep`) -> markdown commands (`commands/*.md`, user then trusted project) ->
`app.ready` -> learned skill hooks (skills load last, lowest trust). Boot runs on
its own goroutine and may block on the trust prompt; results reach the UI via a hook channel.

**Project trust** (main.go `trustGate`, DESIGN §5): one decision per repository, keyed by
`Dirs.TrustRoot()` (the symlink-resolved repo root, searched no higher than below `$HOME`
so a dotfiles repo is nobody's root; a linked worktree maps to its main
checkout by reading the `.git` file, the `gitdir` back-pointer and `commondir`, never by
running git) and saved in `<state>/trust.json` as `trusted`/`untrusted`. Order: a decision
held for this process (`--trust`, an esc answer, `/trust` when saving is impossible), the
saved one, then the Go-owned prompt (`App.AskTrust`: `y` trust, `n` don't, or a click on the row; esc/enter/ctrl+c not
now). `$HOME` and `/` are never asked about or saved (`hostapi.TrustUnsaved`; `/trust` there
says the answer applies to this run only). `/trust` reopens the prompt off the UI goroutine,
releases a trust hold (`Host.ReleaseTrust`, see "Sandbox"), saves, sets `Host.SetProjectTrusted` and then reloads when the decision changed (a reload that
fails before the trust step must not leave the old decision in force); `Host.SetProjectTrusted` clears
the auto-mode approval cache on a change. `Host.ProjectTrusted()` gates project `.zemi`
(init.lua, agents, commands, workflows), instruction files and profile `context.files`
(untrusted: only `<config>/AGENTS.md`), project skills, every language server,
the Lua tool's ask, and what auto and plan mode settle (see below). `Host.git` switches off
fsmonitor, hooks and repo-local filter drivers in every repository, trusted or not: a
sandboxed command can plant a nested repo whose config zemi's own checks would run.

### The concurrency rules (DESIGN §4) — violate these and things deadlock or corrupt

1. **Only the Lua actor goroutine touches the `lua.LState`.** Everything else goes through
   the queue in `internal/lua` with priority classes `ui > agent > io`. `ctx` cancellation
   preempts running Lua at the next opcode (catchable once, then re-raises).
2. **Watchdog / hot mode.** A wedged request trips the watchdog; after a grace period the
   runtime goes "hot" — every Lua request fails fast until force-reload builds a fresh
   `LState` (generation). All registrations (keymaps, widgets, events, timers, jobs,
   Lua tools) are generation-tagged (`hostapi.Gen`) and purged before replay. Never keep a
   pointer into an old generation across a reload. `Host.ForceReload` keeps the old Lua
   permission rules (`staleRules`) until the replay succeeds, and `tool.pre` and
   `permission.request` vetoes wait out a running reload (`outsideReload`), so a turn is
   never checked against half a config.
3. **UI single-writer.** All `ui.App` state mutates on the bubbletea Update goroutine.
   Work from other goroutines (host hooks, Lua callbacks) is funneled through a hook
   channel drained by a persistent cmd (`internal/ui/hooks.go`). hostapi never imports
   internal/ui — it defines `UIHooks` (internal/hostapi/host.go) and the App implements
   it; keep that dependency direction (ui -> hostapi, never the reverse).
   Corollary: never call `program.Send`/`program.Quit` from inside `Update` (or anything it
   calls) — bubbletea v2's `p.msgs` channel is unbuffered and `Send` blocks until the event
   loop receives, so the loop would wait on itself (the `/quit` hang). Wrap such calls in a
   goroutine, like `App.Quit` and `OpenModelPicker` do. Blocking hooks (`PickModel`,
   `AskPermission`, `AskTrust`) must only ever run on non-UI goroutines. Stream events only
   invalidate the frame, so at most one waits in the hook queue (`streamPending`).
4. **UI-critical Lua handlers (keymaps, widget render, event handlers) must be fast**
   (~10 ms). Long work belongs in `zemi.job` / `zemi.http` / `zemi.timer`.
5. **Vetoable events** (`permission.request`, `app.quit`, `composer.submit`, `tool.pre`) have
   a 50 ms Lua-handler deadline and then take a logged default (fail-closed for permission
   asks without an overlay leg). While the runtime is hot, registered `permission.request`
   and `tool.pre` handlers cannot run, so both deny until `/reload`.
6. **No bare goroutines in hostapi.** Start them with `h.GoSafe(where, fn)`: a panic is
   logged with its stack (source `panic`), shown in `/errors`, and zemi keeps running.
   Turn goroutines also defer `h.endTurnAfterPanic()` so the agent is not left "active".
   A panic in a plain `go func()` kills the process with the terminal still in raw mode.
7. **The store** (`internal/store`) is SQLite via modernc.org/sqlite (pure Go), WAL,
   `MaxOpenConns(1)`. Two handles: global DB (user-scope memory, kv, skills, config
   revisions) and a
   per-project DB (sessions, messages, todos, tool_calls) at
   `<state>/projects/<sha256(projectDir)[:16]>/zemi.db`. Schema is one `schema` const of
   `CREATE TABLE IF NOT EXISTS` + FTS5 triggers executed on open — add tables there; a new
   column on an existing table also needs an ALTER appended to `migrations` (store.go),
   since `IF NOT EXISTS` skips it on old databases.

### Tool pipeline (internal/tools/tools.go)

Every model tool call runs: JSON unmarshal -> schema validation -> `tool.pre` Lua events
(deny / halt / rewrite) -> permission gate -> run -> `tool.post` notification. Key
invariants: a rewritten input **re-enters at the permission gate** (per-run pipeline copy
with `RewriteLoosened`, so a non-user rewrite can't ride a pre-existing allow glob); ask
with no overlay available fails closed; a denied ask's result says who decided and the reason
the asker gave (`Asker` returns it: "denied by the user: run outside the sandbox"); errors become tool-error responses, not panics.
Host-implemented tools (`agent`, `workflow`, `window`, `Lua`, MCP and Lua tools) run
`tool.pre` through `preCheck`, which refuses a rewrite with an error result.
A command a permission mode settled runs sandboxed (per-run `Pipeline.Sandboxed`, see
"Sandbox" below).
Bash is special: `perm.Store.CheckCommand` normalizes the command first (compound
splitting, redirect gates, env-assignment gate — see internal/perm/bash.go, heavily
table-tested; extend those tables when touching it).

File tools (internal/tools/fs_tools.go): `write`, `edit` and `multiedit` refuse a file
the model has not read, or one whose mtime moved since (`Pipeline.Seen`, a per-session
map wired in `PipelineFor`; subagents get their own), and hold a per-path lock, so a
concurrent agent or the user's editor cannot lose changes. `tool.Run` resolves the path of
`read`, `write`, `edit` and `multiedit` (`perm.RealPath`) before the gate, and they open
exactly that path through `openGated` (an `openat` walk with `O_NOFOLLOW`), so a sandboxed
command cannot swap a directory for a symlink between the check and the open. Matching runs on LF text and
a CRLF file is written back as CRLF; the whitespace-tolerant fallback re-indents
`new_string` to the file's own indentation and says so. `glob` and `grep` shell out to
`rg` when installed (gitignore, `**`, `{a,b}`) and fall back to a Go walker with the same
glob rules; they are gated on their root only, so neither shows a hit that a deny or ask
`fs.read` rule covers, and grep hides secret-shaped files (rg still reads them). `read` on a
directory lists its entries under the same filter plus secret-shaped names. Long bash and fetch output is saved
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
edits **inside the jail** that are not secret-shaped, except writes under any `.git` or `.zemi`,
the git dir a project `.git` file names (`gitdirTarget`) or zemi's own config/data/state dirs (`protectedWrite`, which skip the
screener too and ask the user, as do writes outside the project and scratchpad), `tool.workflow` scripts,
`perm.SafeCommand` commands) and only then runs a one-shot screening model, caching
approvals keyed on everything it saw (subject, arg, reason, preview; the reason goes in as
escaped tagged data like the action) and falling back to the user ask with its reason
(`askHook` resolves `fs.read`/`fs.write` arguments first, and the screener and overlay see
them relative to the project, `shownArg`); `plan` denies writes and runs `SafeCommand` commands (sandboxed, the project unwritable). Network requests are
never settled locally: the URL is the model's. Known ceiling: auto settles in-jail writes and
project build/test commands locally, so the model can write code and run it with no screening;
that is the intended auto tradeoff, and manual and plan ask. In an untrusted project
(`settledCommand`) both modes settle only `perm.ReadCommand` commands (`SafeCommand` whose
every program only reads, named without a path; no builds, no git), auto settles no
writes, and auto skips the screening model and its cache, sending the rest to the user with
reason "not settled: this project is not trusted (/trust)". A trusted project with no
sandbox backend also settles only `ReadCommand` commands, so `SafeCommand` is reached
only for runs that will be sandboxed.

**Sandbox** (internal/sandbox, internal/hostapi/sandbox.go, DESIGN §9): a `tool.bash`
verdict a mode gave (`DecidedBy` `mode:<name>`, base not yolo) makes `Pipeline.SandboxFor`
return `h.sandboxPolicy(plan)`; `tool.Run` puts it on the per-run pipeline copy
(`Sandboxed`, the `RewriteLoosened` pattern), and `runBash`, `Jobs.Start` (a `wrap` argument and the guard check;
`Adopt` takes the already wrapped command and that check, nil when unconfined) and the window tool's `win.Spec.Wrap` call
`sandbox.Wrap` before starting. A wrap error is a tool error, never an unconfined run.
Rule-allowed and user-approved runs, `!command`, `zemi.job`, MCP servers and
`Host.git` stay unconfined; language servers run under `serverPolicy` (see LSP below). `NewHost` probes the backend synchronously (`h.sandbox`; tests
swap it with `fakeSandbox`), so `--health` and the first turn's schema see it. Policy:
writes to the scratchpad, `<state>/sandbox-tmp/<session id>` (`TMPDIR`, `TMP`, `TEMP` through
`Policy.Env`; never the shared system temp dir, where macOS keeps the `xcrun_db` that picks
what `/usr/bin/make` and `git` run; swept with the scratchpads), `<state>/sandbox-cache/<hash of the trust root>/go-build` (`GOCACHE`, one per trust root: Go trusts cache entries, so a shared one lets one project poison another's build) and the project (trusted, not plan mode, not `$HOME` or above, neither holding
nor inside a zemi dir, holding no hidden path unless on seatbelt, no symlinked `.git`/`.zemi`, no relative `PATH` entry, and only with `Backend.EnforcesProtect`:
Landlock + seccomp leaves the project read-only); `.git`, `.zemi`, zemi's dirs and every `PATH` dir inside a write root read-only
(zemi runs git, sh, servers and clipboard helpers unconfined by name)
and loopback only (seatbelt: the machine's; the Linux namespace variant: its own, host
services unreachable; Landlock + seccomp blocks every inet socket); home credential paths
(`secretDirs`) and zemi's dirs (unless they hold the project) hidden. The namespace variant
binds only protected paths that exist, so `Pipeline.Guard` (`h.guardSandbox`) records the
missing ones before a sandboxed bash, job or window starts and checks them at exit (and
`CheckSandboxed` from `watchConfig` and the trust step while it runs): a created one holds
trust off (`holdTrust`, not saved, `decide` honors it) until `/trust`, and the result names it.
On backends other than seatbelt `configEntries` walks the writable project (at most
`sandboxWalkMax` entries, else it stays read-only) for nested `.git` and `.zemi`: the policy
protects the existing ones and the guard holds trust off for new ones at exit. zemi's
own temp files go to `Host.TempDir()` (`<state>/tmp`). With no backend,
auto mode sends `tool.bash` asks it cannot settle straight to the user ("not settled: no
sandbox"). While a backend is live, bash, `job_start` and `window` take `sandbox: false`
(`Request.Unsandboxed`): mode rules and non-yolo bases skip it and it asks the user with
"run outside the sandbox"; headless (`SandboxParam` needs UI hooks) neither the parameter
nor the retry line is offered, and the denial is by `unsandboxed`. A confined failure that
`sandbox.LikelyDenied` recognizes (a job: over its whole retained tail) gets
`Pipeline.sandboxHint`, which, like `tools.SandboxProp()`, describes the backend
`sandbox.Probe` returns. `/health` has a `sandbox` row.

`h.secretPath` matches the secret shapes on
the resolved path too, so a symlink cannot hide one. `SafeCommand` is an
allowlist (internal/perm/safe.go, table-tested): no shell expansion outside single quotes,
no write redirects, no program named by a path (`./x`, `/bin/cat`), and every argument must
pass the host's path check (inside the project, not secret-shaped), so a reader cannot leak
`~/.ssh` to the model. Project tools (`make`, `pytest`, `ruff`, `tsc`, the listed `go`,
`cargo` and `npm` subcommands, ...) settle with any arguments: what they write and run is
the sandbox's to confine, and plan mode stays read-only because its policy leaves the
project unwritable, not because the parser refuses a flag. Readers keep their
`refusedFlags` (`sort -o`, `find -exec`, `rg --pre`) and sed/awk/jq script checks, since
`ReadCommand` shares them and settles readers unconfined.
`perm.ArgUse` tells the check how the argument is used: code may not run from the
scratchpad, and a recursive search (`grep -r`, `rg`, `ag`, `diff`, `gofmt`) settles only over a tree
with no secret-shaped file in it. No argument may point into `.git`, and a git command that
may print file content (`grep`, `show`, `diff`, `cat-file`, `stash show`, `stash list` with
arguments, `status -v`, `log`/`reflog` with `-p`, `-U`, `--binary` or another patch flag;
`gitContentDir`) settles only in a repository
that tracks no secret-shaped file (`trackedSecret`; `repoNote` names the file in the ask
reason when it is the only reason the command did not settle). `git config` never settles
and `git remote` only bare, since `.git/config` may hold tokens. Arguments after a `cd` are
judged from every directory the shell may be in.
Known ceiling: a project tool runs code a flag or target names (`go test -exec`,
`make --eval`), and that code reads whatever the sandbox lets it. Seatbelt hides the strict
secret shapes (`.env`, `.env.*`, `.envrc`, `.netrc`, `*.pem`, `*.key`, `id_rsa` and
friends); Landlock cannot, so on Linux such a flag in a trusted project can read `.env`.
User config adds modes with `zemi.permission.mode.add`; unknown values read as manual.
Only user config sets it; binding a key to the mode cycle is refused for other origins.

The overlay's "always" answer writes session allow rules scoped to the `model` principal, and
they are prefix globs rather than the exact request: `alwaysChoice` (ask.go) gives bash
each normalized segment's two-word prefix plus `prefix *` when the program takes
subcommands (`git`, `go`, `npm`, ...; `subcommandPrograms`) and the exact segment otherwise,
`fs.read`/`fs.write` the file's resolved directory with `/*` (the `fs.write` rule has a
`not_match` of `dir*/.git*`, so `.git`, `.gitattributes` and `.gitmodules` still ask; an
approved `.gitignore` or `.github/` file also gets its own exact rule), and `net.*` both
`scheme://host` and `scheme://host/*`. A gate-forced ask (heredoc, env assignment) and a
request whose pattern text holds a glob wildcard (`*`, `?`) offer no always option at all.

### require (internal/lua/runtime.go)

Modules are cached per generation by resolved file and trust class: a requirer below the
file's origin gets its own instance run under its own origin (builtin modules are shared
instead, through a deep read-only view), so project, plugin or learned code cannot plant
functions into a table that higher-origin callbacks call. Dotted names
search `<root>/lua` over the runtimepath: user, plugins, and the project's `.zemi` first
but only when the requirer is project code. `./x` and `../x` resolve against the calling
function's chunk name, which is why file chunks are compiled with their absolute path, and
must stay in that file's origin tree; `load`/`loadstring` tag their chunks with the running
origin and never keep an absolute name, so neither can be forged. `ProjectConfigDir` walks up to the `.git` root and takes the nearest `.zemi` (`projectInitStep` logs which).

### Origin model (internal/lua/origin.go, registries)

Every chunk carries an origin from its resolved path class: `user` (~/.config/zemi) >
`builtin` (embedded runtime/) > `plugin` (site/pack) > `project` (<project>/.zemi);
`learned` code is denylisted from dangerous APIs. Registries enforce: cross-origin
collision is **refused** with the higher-trust origin winning; within one origin it is
last-writer-wins. Project origin additionally cannot register providers or builtin tool
names, and its `tool.pre` rewrites are ignored (logged). No origin can emit host-owned
events: `zemi.event.emit` takes only `user.*`, and `zemi.event.on` takes catalogued names
or `user.*` (internal/hostapi/event.go). A keymap that is a prefix or an
extension of a more trusted key is refused, `zemi.command.run` refuses a more trusted
origin's Lua command (its body runs under that origin), windows can be closed, focused or
moved only by user, builtin or the origin that opened them, and `app.quit` denials from
project or learned handlers are ignored. When adding a registry,
follow `internal/hostapi/registry.go`.

### Skills (internal/skills, internal/hostapi/skills.go)

Skills are file-based units: authored under `<config>/skills/`, builtin (zemi's
self-knowledge: `runtime/skills/zemi-*`, embedded, rewritten to `<data>/builtin-skills/` on
every sync, enabled by default; keep them accurate when behavior changes), learned
(reflector drafts, body only, no Lua hook, scrubbed, start disabled) under
`<data>/skills/`, each a `SKILL.md`
with YAML frontmatter (name, description); discovery lives in `internal/skills`, hosting in
`internal/hostapi/skills.go`, rows in the global DB `skills` table. A trusted project adds
its own (`.zemi/skills`, `.agents/skills`, `.claude/skills` under `Dirs.RepoRoot()`): body only,
enabled, never in the table or `skillsReg`. `SetProjectTrusted` scans them into
`Host.projectSkills`, so a skill a command writes mid-session waits for the next boot or
`/trust`, and `enabledSkills` adds them after every other skill whose name they do not take.
Invariants:

- **Quarantine until enabled.** A skill is inert until `zemi.skills.enable` runs, and only
  user or builtin origin may enable/disable (`refuseBelowUser` gate at the Lua namespace
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
  `/compact` and rewind claim `active` and a `turnScope` together (`beginScope`, released by
  `handOff`), so a cancel, a send or another claim cannot slip in between.
- **Store order.** `Messages` orders by `rowid`: a turn persists several rows in one
  millisecond and ids are random. Tool telemetry rows share the table; `historyFrom` skips
  anything that does not decode as a fantasy message.
- **Proposals persist.** Memory proposals live in the scope DB's `proposals` table until
  `/memory` (a/d/s, the edited rows come back with `Action` and `Scope`; a scope move adds
  before it deletes; `e` returns a row with `Edit`, and after the other marks apply
  `editMemory` opens its raw text through `EditFile`, re-runs intake as user origin keeping
  the id (an edited suggestion is stored), and `openMemoryReview` shows the screen again) or `zemi.learn.decide`; `refreshPending` caches the
  statusline counts.
- **Reflector timing.** It runs on session close (`/new`, `/resume`) and at startup for
  the latest session (once per process, not on a reload, when that session is the open one), never on quit; `reflectDue` compares the message count with kv
  `reflector.seen.<sid>`, so a session is reflected again only after new messages.
- **Digest and reflector input.** `digestFor(query)` ranks by the FTS score against the
  session's first message (bm25 stays near zero for common words, so they lift nothing),
  user origin x2 and recency over 30 days from the later of write and last search hit;
  pinning never bumps hits. The reflector prompt carries the instruction files
  (`instructionSections`, shared with `systemWith`, same trust gate) and a memory index of
  the newest 20 plus FTS matches against the user's lines. A reflector entry with empty
  text and `supersedes` queues a forget proposal (`queueForget`), never auto-applied.
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
the renderer untouched. The focused window's cursor is the host's own (`View.Cursor`, in
the shape and blink the program set with DECSCUSR), hidden under a modal or a float drawn
over it. OSC 10, 11 and 12 queries answer with the colors the host terminal reported
(`Manager.SetHostColor`), so an editor picks a theme that matches. Keys go the xterm way (`legacyKey`),
and as CSI u (`kittyKey`) while the program keeps kitty keyboard flag 1 on for the screen it is on
(a bool per screen like zellij, no flag stack), so nvim tells esc, ctrl+i and shift+enter apart.
OSC 52 writes reach the host clipboard (`Manager.TakeClipboard`, sent through `copyCmd` each frame)
only from a window whose `Spec.Clipboard` is set: the editor and `zemi.win.open` from user or builtin
code outside the Lua tool, never a window the model drives, which could plant a command the user
pastes; those windows also list 52 in DA1. A read is never answered. A focused window (`App.focusWin`) takes every key, ctrl+c
included, until `ui.window.unfocus`, and hears the host terminal's focus in and out; a press on a title or separator starts a `winDrag`
(move a float, resize a vsplit or split), a release without motion focuses or closes on
×, mouse events inside are forwarded (wheel becomes arrows for programs that never asked
for the mouse), and a paste goes to the program with bracketing when it asked. The model's
`window` tool gates `open` as `tool.bash` (same rules and modes as bash, and a mode-settled
open runs sandboxed through `win.Spec.Wrap`, recorded as `Window.Sandboxed`) and
`keys`/`mouse`/`send` as `tool.window`, `Unsandboxed` (so no mode settles it) unless the
window runs sandboxed; an exit lands in the next message through `shellNotes`. Subagents get
`windowToolFor(name, pipe)`: their asks carry the agent name and their windows open hidden.
The tool sees only windows it may act on (`windowTool.mine`): the main agent its own and its
subagents', a subagent its own; the user's, Lua's and editor windows answer "no window".
Editor windows (owner `editor`) are also hidden from `zemi.win.get`/`list` below user origin
and from the Lua tool (`hiddenWin`), and do not count toward the 8-window limit.
`App.EditFile` (ui/editor.go) runs the user's editor in such a window: a float kept at frame
size, `os.Environ()` (not `perm.ChildEnv`), the path as `Spec.Args`, focused until it exits,
then closed with focus given back; a non-zero exit is an error, and a cancelled ctx returns
leaving the editor running. `Host.CloseWindow` (× and `/win close`) kills a running editor
only on a second request within 3s, and `/win close all` skips it.
Input to a program queues and a `writeLoop` goroutine feeds the pty, because the emulator
writes under the screen lock and a program that does not read stdin must not wedge it.
A window's env drops the host terminal's identity (`hostTermEnv`: `TERM_PROGRAM`, `GHOSTTY_*`,
`TMUX`, ...), or image tools emit protocols x/vt drops and show nothing. Images (kitty.go) work
through kitty Unicode placeholders: the UI asks XTVERSION and `SetKitty` turns them on for
ghostty and kitty only, at 256 colors or more since a placeholder names its image by color
(windows then see `TERM_PROGRAM=ghostty`); an APC handler takes direct
base64 transmissions only (never `t=f/t/s`: the unconfined host would read the file), forwards
them under host ids 16..255 zemi owns as a virtual placement with `q=2` (host replies never
reach bubbletea), and writes placeholder cells into the emulator right after the ST
(`Window.write`). The cells are text, so moving, clipping, hiding and scrolling need nothing
image-specific; `Screen()` maps a cell's fg to the window's own host id (another window's or
an unknown id renders blank), and `Text`, scrollback, selection copy and screenshots read them
as blanks. Everywhere else `sanitize.String` blanks placeholders (`windowRows` keeps them), so
model text cannot draw a window's image in chrome. `onFrame` writes `TakeGraphics()` with `tea.Raw`; close and id eviction queue `d=I`, and quitting sends them before leaving the alternate screen (`App.leave`)
(eviction takes the sending window's own oldest image first, so one window's churn cannot blank another's).

### Subagents, workflows, background tasks, MCP (internal/hostapi: subagent.go, workflow.go, tasks.go, mcp.go; internal/mcp)

- **Subagents** are fresh `agent.Runner`s over `PipelineFor()` (same permission gate; asks
  are prefixed with the agent name). Definitions: builtin `explore` (read-only tools), `web`
  (web_search + fetch, on `agent.small_model`) and `general`, plus `agents/*.md` (user over builtin over trusted project). They never get
  `agent`, `workflow`, `todos`, `question`, `plan`, `open_in_editor` or `Lua` (nesting depth 1). A run is a child session
  (`parent_id`); `ListSessions` hides children. Usage adds to the session tally, not the
  context estimate. The delegation guidance lives in `delegationPrompt` (prompt.go):
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
  transcript (`startTurn`, `deliverSteering`) and resume cut off. `agent` with `send_to: <run id>`
  queues its prompt for that run (`MessageAgent`, error when it is not running).
- **Agent runs** (tasks.go): every `runSubagent` (tool, background, workflow, Lua) registers
  an `agentRun` whose hooks build a live transcript. `/agents view <id>` (or a click on an
  `Agent` row) sets `Host.view`, and `widgetCtxTable` renders that transcript instead of the
  session's (live stream and todos hidden, `agent.steering` holds the run's queue); esc clears it,
  and so does `setTranscript`. A submit while the run is running goes to it through
  `MessageAgent`, otherwise it clears the view and goes to the session. A run's queue is its
  `Runner.Steer` (rows join the transcript when delivered); what arrives after its last step
  boundary becomes one more `Send` (`followUp`), which closes the queue in the same lock when
  there is nothing left, so a message is never accepted and then dropped. Messages persist in
  the child session like the prompt. Runs the process no longer holds load from their child session.
  `/agents` also lists `h.jobs` (host-wide, every session) with the output tail and a Stop
  action (`/agents kill <job id>`).
- **Scratchpad** (scratch.go): `<state>/projects/<hash>/scratch/<session id>`, created when
  the pinned environment prompt is built, pads idle a week removed. `fsDefault` allows
  `fs.write` there, `Pipeline.Scratch` lets it pass plan mode, and read jails and
  `safeCommandPath` include it for plain reads (never for programs that may run code).
- **Lua tool** (luatool.go, tool name `Lua`): model Lua runs through `Runtime.Eval` as `user`
  origin, default-deny: without approval it may call only `luaReadOnly` members. Every Go
  function in a namespace passes the sandbox guard (`Sandbox.SetGuard` -> `guardModelLua`),
  and handle tables go through `lua.GuardFuncs(L, t, "win:", h.guardModelLua)` (member
  names `win:send`); the guard raises for tool code (frame path `lua-tool*` or a
  `@user:lua-tool*` function on the stack) unless the innermost tool code is an approved
  run. `luaUnlisted` tokenizes the script first: any zemi path off the list, a non-string,
  non-read handle method call, or dynamic access (`_G`, `load`, `require`, ...) asks
  `tool.lua` once, as `Request.Unsandboxed` so no mode settles it, and the approved run
  evaluates as chunk `lua-tool#<n>` (`h.luaApproved`, dropped when the run ends). In an
  untrusted project or plan mode every script asks. A new API member is guarded until
  added to `luaReadOnly`; `TestLuaToolDefaultDeny` checks every member. Not a new origin:
  the gate is a flag on the chunk name.
  It runs on the interpreter (10s ceiling), in a wrapper env whose `print` is captured.
  Main agent only.
- **Model memory edits**: `memory_update`/`memory_forget` go through `modelEditMemory`,
  which re-runs intake and asks the user (`memory.write` as `Unsandboxed`, so no mode settles it) unless `memoryAuto` holds (autonomy auto,
  a trusted project, and no `fetch`, `web_search`, `Lua` or non-builtin tool (MCP, `zemi.tool`)
  call in the session or its subagents' child sessions, whose tool calls `subagentHooks` records
  for this and `ForkSession` copies), the memory
  is learned and the text is not instruction-shaped. `gateMemory` uses the same check. Ids may be 8-char prefixes.
- **MCP**: `internal/mcp` is a dependency-free JSON-RPC client (stdio, streamable HTTP
  with JSON or SSE replies). Servers come from `zemi.mcp.add` (user origin only); they start in the background and stop on `PurgeGeneration`,
  unconfined in `DataHome` with `ZEMI_PROJECT_DIR` set, trusted project or not: `npx` and `uv`
  read config from the working directory, which settled commands and writes may change, and
  most servers need the network a sandbox refuses. Tools are
  `mcp_<server>_<tool>`, gated per call as subject `mcp.<server>` with the tool name as
  argument. Test against `internal/mcp/mcptest` (the test binary doubles as the stdio
  server).
- **LSP** (lsp.go, internal/lsp): servers start lazily on the first matching file (user
  `zemi.lsp.add` first, then an installed builtin whose marker is in the file's directory
  or above it), and only in a trusted project: servers run repo code (rust-analyzer build
  scripts), so `lspDefs` returns nothing untrusted, user-added servers included, and
  `noLSP` says why. With a live backend every server starts through `lsp.Config.Wrap` under
  `serverPolicy`: the project read-only, writes only to `<cache>/lsp/<trust key>/<server>`
  (`Dirs.CacheHome`, `ZEMI_CACHE_HOME`; `XDG_CACHE_HOME`, `GOCACHE`, `GOPLSCACHE`,
  `CARGO_TARGET_DIR` and `TMPDIR` point there), hidden paths and loopback as for commands.
  A confined cargo cannot write a missing `Cargo.lock`, so rust-analyzer reports nothing
  until one exists. Without a backend rust-analyzer (`RunsCode`) does not start;
  the other builtins and user-added servers run unconfined. typescript still gets the tsserver on PATH (skipped when that is inside
  the repo). The pipeline's `AfterWrite` appends errors and warnings to
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
  `fs.write` denied even under allow rules, `mcp.*` allows and bash allows other than
  `perm.ReadCommand` flip to ask (a rule-allowed run is unconfined). The
  prompt section only saves wasted attempts.
- **Composer bridge**: Lua reads a snapshot the UI publishes (`SetComposerSnapshot` after
  key and paste handling) and edits through `UIHooks.ComposerEdit`, applied on the UI
  goroutine. Completion popups float over the chat (`floatPopup`), they never reserve
  rows.
- **System prompt** (prompt.go): `basePrompt` (acting first, tasks, asking, care, tools,
  git, replies, security) + `delegationPrompt` for the default profile, then `zemiPrompt`
  for every profile, plan mode, the per-session pinned `environmentPrompt`, instruction
  files under `# Project instructions`, Lua sections, skills, digest. Anything that varies
  per turn must be pinned per session or it busts the provider's prompt cache.
- **Tool schemas**: fantasy wraps `ToolInfo.Parameters` as the properties map, so tools
  pass `tools.SchemaProperties(schema)` and a non-nil `Required`
  (`TestToolSchemasAreProperties`); strict providers reject anything else.

### Permission rules and config

Every rule lives in memory: `zemi.permission.rule` in config adds it again on each boot, and
an "always" answer lasts until zemi exits. There is no rule file and zemi makes no git commits
(`scope = "permanent"` raises and points at init.lua). Only `user` origin may add allow rules
(project may tighten only); the gate lives at the `zemi.permission.rule` namespace closure,
same off-interpreter-`OriginHost` reasoning as skills. Skill proposals always wait for review
(DESIGN §10.4). The reflector proposes memories and skills only; there is no config-patch
pipeline and no `/config` command (the agent edits config files directly). `Host.git` shells
out to the `git` binary (no git library in go.mod), with `GIT_CONFIG_NOSYSTEM=1` and a 15s
timeout.

## Testing patterns

- `internal/hostapi/host_test.go` has the canonical helpers: `newTestHost(t)` boots a
  fresh runtime + namespaces step with temp dirs; `runUser(t, r, luaSrc)` executes a chunk
  as user origin **with a 3s guard so an interpreter deadlock fails loudly instead of
  hanging the binary** — reuse it for any test that runs Lua.
- API surface is pinned: `TestNamespaceVersion` asserts the exact `zemi.api.version`
  ("0.23.0") and namespace members. Growing the Lua API means updating that test and the
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
  state must be passed in ONE call.
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
  print mode). Not built: M4 evals (no `zemi.eval`), the plugin manager, embeddings.
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
  `learned` and gates autonomy/supersede/digest-labeling. `memory.autonomy` is set by user
  config only (`zemi.config.set` refuses other origins).
- **Config store** (`internal/config/store.go`): dotted-path keys from a Go-owned schema;
  only user config sets them (`SetHost` is the Go path, no watches). Providers ship none; user config registers them, keys
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
  that `listRows` puts on every row of an item, prompt choices included) to act on the
  topmost chrome. Hover lights the whole item, but only the pointer over the item's
  non-blank or background-painted cells on its row makes it a target: padding, gutters,
  blank rows and section headers never are. Lists scroll at the edge and never recenter,
  so a click does not move the item under the pointer. Modal chrome is spliced after
  windows, and everything outside the top box is repainted `Host:Dim` (`dimRows`); the
  palette and completion popups are not modal. Overlays (`internal/ui/overlay.go`) are data:
  an item with `detail` or `actions` pushes a detail view (`<id>#detail`, the parent's
  origin, drawn in place of the list, esc pops it), `Body` is text above the items, `Empty` the line for none, and the
  hint line lists only keys that act on the selection (`enterVerb`); enter on an inert row
  does nothing. Helpers that rebuild segments must carry `Hit`. Composer
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
