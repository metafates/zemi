# AGENTS.md - zemi

zemi is a terminal AI agent harness whose userland (default UI, keymaps, widgets, learning
machinery) is written in embedded Lua while Go owns the runtime: the agent loop over
`charm.land/fantasy`, the bubbletea v2 TUI, the sandboxed gopher-lua interpreter, and SQLite
persistence. `DESIGN.md` is the spec; code comments cite its sections (`DESIGN §4`, `§6.2`).
**Before changing behavior, read the DESIGN section the code cites** - it explains the why
(usually a security or concurrency invariant), and a change that contradicts it is almost
always wrong. When spec and tree disagree, the tree is the truth.

Prior art with local checkouts under `~/Code/github/` (study, don't copy): `crush` (same Go
stack; tool descriptions, permission flow and store patterns trace back to it), `pi` (prompt
wording), `hermes-agent` (learning machinery), `codex` and `opencode` (tool behavior), `neovim`
(the model for the Lua userland: `runtime/lua/vim/` for keymap, highlight, health and loader
patterns).

## House rules

These are the owner's rules for anyone changing this repo, human or agent.

Every new line must be deserved, and the best change deletes code. Before adding anything: does
it need to exist, does the tree already have it, does the stdlib or an installed dependency do
it. No abstractions nobody asked for, no speculative options, fewest files. Fix a root cause in
the shared primitive, never per screen or per caller.

Breaking changes are welcome, architecture and the Lua API included, when they make the result
simpler. No compatibility shims, no deprecated aliases, no "the old key is still read": remove
the old thing, then update `runtime/types/zemi.lua`, `TestNamespaceVersion` and the
`zemi.api.version` string together.

Parse at the boundary: a check returns the checked value in the shape inner code needs (a
struct, a `perm.Path`) and inner code takes that type, never the raw string; a check that
returns only an error, followed by `x, _ :=` on the same input, is a bug. A type gets an
unexported field only when it carries a proof something acts on, like a path safe to open.

Comments say only what the code cannot: a why, an invariant, a ceiling, in one or two lines,
describing the code as it is and never the diff. A deliberate shortcut gets a `NOTE:` naming
the ceiling. Tests are tables and one runner, a case another case covers is deleted,
non-trivial logic leaves exactly one runnable check, and security and sandbox assertions are
never weakened.

Fast is a feature. Nothing slow sits between submit and the provider request, an idle zemi
redraws nothing, and there are no per-frame Lua or allocation hot spots. Cosmetics never cost a
model call: the model supplies its own labels, like a bash call's `about`, as it streams the
call.

Safety comes from the sandbox, not from nagging. Auto mode does not ask for what the sandbox
already confines, only for what is destructive or leaves it. Never fail open.

The UI has one identity: greys, one periwinkle accent, square corners, and the `@` snail whose
state lives in its eyes, plain eye color in every state. Widgets use the semantic theme groups
(`Accent`, `Dim`, `Rule`, `Title`, `Selected`, `Button`, `Danger`, `Warn`, `Ok`). No state change may
add or remove a row or shift text. Hover in the chat is minimal: body rows get none, a header
only changes foreground. A popup covers only its own cells. Polished UI must be easy to build
from Lua and ugly UI hard, so what Go draws comes from the layout and widget API rather than
hardcoded strings.

Docs follow the code in the same change: the AGENTS.md, DESIGN.md and `runtime/skills/zemi-*`
lines a change makes wrong are fixed with it. `make check` must be clean, and goldens are
regenerated with `make golden` and the diff read. Commits are small, one lowercase line saying
what changed, no prefixes, no body unless the why is not obvious. Never push unless asked.

Workers in git worktrees: worktree isolation cuts from main, so land prior work on main first.
Goldens conflict on every UI merge; take either side and run `make golden`.

## Commands

A Makefile wraps the plain Go toolchain (module `github.com/metafates/zemi`, Go 1.27). No CI;
`make check` is what a change has to pass:

```
make check              # go vet + go test + make luals + make stylua + make lint + a gofmt check, all must be clean
make luals              # lua-language-server over runtime/; prints a skip line without the binary
                        # not on PATH? make check LUALS=/path/to/lua-language-server
make stylua             # stylua --check over runtime/ (stylua.toml); a skip line without the binary
make lint               # golangci-lint run (.golangci.yml, curated linters); a skip line without the binary
make build / make run   # go build -o bin/zemi . / run the TUI against your real ~/.config/zemi
make test               # full suite (~20s, pure Go, no CGO; network only for one macOS TLS check)
make race               # go test -race ./internal/...
make golden             # regenerate internal/ui/testdata/*.golden, then read the diff
make fmt                # gofmt -w over the tracked Go files, and stylua over runtime/ Lua
make reset-db           # delete the stores a schema bump refuses (ZEMI_STATE=<dir> for another tree)
```

git commits are run unsandboxed (`sandbox: false`): `.git` is read-only inside the
sandbox, so a confined `git commit` fails on `index.lock`.

For a throwaway config tree, redirect all dirs: `ZEMI_CONFIG_HOME`, `ZEMI_DATA_HOME`,
`ZEMI_STATE_HOME`, `ZEMI_CACHE_HOME` (internal/config/dirs.go), and `HOME` too when the
skills in `~/.agents/skills`, which no override moves, must stay out; most tests inject
`config.Dirs{...}` with `t.TempDir()` instead. Tests boot with empty dirs and an untrusted
project: call `h.SetProjectTrusted(true)` when one needs project instructions, language servers
or settled builds.

## CLI (cli.go)

`zemi` opens the TUI; a prompt argument is sent as the first message, or run as a command when
it names one (`sendPrompt`, TUI and `-p` alike, so `/review` works from a git hook). `--help`
lists the flags. Piped stdin (1 MiB at most) is attached behind the typed prompt and is a usage
error without `-p`. Flags apply after boot so they win over `init.lua`, except `--trust`, which
the trust boot step reads. `-m`, `/model <provider/model>` and `zemi.model.set` write
`models.chat`, and refuse while the active profile sets its own model (`Host.ProfileModelErr`;
`--profile` applies before `-m`), since the key would change and the conversation would not.
`Host.SetModel` parses the rest of `/model` (`Host.CompleteModel` offers the same grammar in
the palette): `roles` opens the role list (`OpenModelRoles`, a `Reload` list that rebuilds
when the role's model list over it closes, and that opens over `/model` from its "Choose a
model per role" row, so esc goes back), `update` fetches the catalog (below; the row under
it), `<role>` that role's model list (`chat` too, titled like the others), `<role> <id>` or
`<role> chat` sets the role or makes it follow chat (`chat chat` returns `models.chat` to the
default).
`--profile` and `/profile` (`Host.SetProfile`; no argument opens the picker, which the
statusline's profile segment clicks into) write `agent.profile` for the next send: a switch
changes the prompt and the tool set, so it busts the provider's prompt cache once; ctrl+t and
ctrl+shift+t cycle it.

`--health` and `-p` boot the same step list and skip only the TUI. `-p` runs one turn, then
reports on stderr how many asks nobody could approve (`Host.HeadlessDenials`, subagents' too;
`deniedHint` points at `--trust` or at an allow rule) and, on its own line, how many calls a
rule, the mode or the profile denied. It also drops what needs a user (`headlessExcluded`), the
agent tool's `background` and `send_to` (a detached report would outlive the run), and the
prompt lines naming them, and a plugin's `zemi.agent.send` raises there (nothing would bound
its chain of turns). A headless run with no saved trust decision runs untrusted and says
so. With no UI `Host.Notify` prints a warning or an error to `Host.Stderr` as "zemi: <title>:
<body>", so `-p` never swallows one. A command that opens a screen gets `Host.Screen`'s
`ErrNeedsTUI` there, which `RunCommand` names ("/help needs the TUI", status 1; `/trust`
points at `--trust`); `/btw` claims the agent like `/compact`, so `-p` waits on it and ctrl+c
cancels it, and appends its answer as a transcript row, which `-p` prints.

`zemi plugins <verb>` (`parseArgs`, when `plugins` is the first word) runs `plugins.Run` and
exits: no Lua boot, TUI, trust or store, so it works beside a running TUI, whose config watcher
reloads on the change. `/plugins` (hostapi/pluginscmd.go) takes the same verbs plus `toggle`,
`open` and `new` (the list's first row, which sends `newPluginPrompt` to the agent: the
procedure lives in zemi-customize). Its list is built off the key loop from `plugins.List` and the last boot's
`pluginLoads`; install, update, check and delete run on `GoSafe`, toast, and refresh the list
(`Overlay.Refresh`: it replaces an open one in place and opens nothing once esc closed it).
`open` edits the plugin's `init.lua`, or its directory when it has none. Under `-p` it points at
`zemi plugins`.

A first run writes a fully commented starter `init.lua` (`config.Dirs.Ensure`), and a TUI
boot no model can run opens the `/model` picker once (`Host.OfferModel`, after the flags
apply, never on a reload or after a failed init.lua). Builtin
providers come from the catwalk catalog (`agent.LoadCatalog`, converted at boot by
`agent.Builtin` with the key env vars resolved in Go, never stored in config, into the
`Host.Builtin` pointer): the embedded one, or the one `/model update` (`agent.UpdateCatalog`,
a busy job that reloads config; `-p` only saves) fetched from catwalk.charm.land into
`<data>/catwalk.json`, never `<cache>`, which a sandbox can write, since the catalog picks
where each key is sent. A fetched one reads only the variables the bundled one names
(`bundledRef`), so the feed cannot pick which secret goes. No network at boot; a saved catalog from another bundled catwalk
version is ignored, so an upgrade brings its newer bundle back, and one that does not load is
the `/health` catalog row's warning. Each builtin is on only while its key is
exported, one without is in `Registry.Missing` (minus the ids config deleted or added) so the
no-model hint (`noModelHint`, one sentence everywhere) can name the variable and `/model` lists it, copy only, after the usable models, `Registry.Reset` seeds every generation
before `init.lua`, a `zemi.provider.add` under a builtin id replaces it
whole, `zemi.provider.del` drops one and `providers.builtin = false` (read once in `readyStep`)
all of them. A `models.chat` config leaves unset runs `Registry.Default`: the first user-added provider's
default model, else the first builtin's in catalog order (anthropic, openai, gemini first).
Config reload is not fsnotify:
`watchConfig` walks the config trees once a second (past dot-directories, `node_modules` and `.luarc.json`),
plus the local plugins, the plugins' `disabled` file and each clone's git index (`pluginRoots`),
and reloads when a fingerprint moved and no turn is active; an untrusted project's `.agents` is not watched (a trusted one's is, `worktrees` aside). A replay that writes a key the
same way as the last boot is skipped (`cfgReplayed`, which `zemi.config.set` and the setters with
a key of their own, `zemi.ui.theme.set`, `zemi.permission.mode.set` and `zemi.model.set`, all
run a valid value through) and a key the new config writes fewer
times is settled to its last value (`cfgSettle`), so `/model`, `/effort`, `/profile`, the mode
cycle and the flags survive a reload until the config changes that key. A key the config stopped
writing returns to its schema default, unless a runtime pick moved it since.

`/config` (hostapi/configcmd.go) is a `Reload` list of every schema key by section, each
description led by its `cfgSource` (`default`; `on disk` when the live value is config's last
write; `unsaved` when a runtime pick moved it off that or off the default; one width, so a
toggle or a save moves no text) and then the value, which a bool draws as its switch instead. Space toggles a bool; every other row opens its
detail, whose actions pick or fill `/config set`. `/config` and `/effort` write through
`Host.SetConfig`, which sends `models.*`, `permissions.mode`, `agent.profile` and `ui.theme`
through their own setters; for any other key a refused value is a usage hint. A key's
`Choices` (the effort levels and "", `memory.autonomy`'s auto or ask) refuse any other value in
`config.checkKind`, which also takes `default` for a "" choice, `zemi.config.set` included; every runtime write,
those setters included, fires the key's watches through `setWatched`. `config lua`
shows the keys off their default as `zemi.config.set` lines; `config save` (`saveInit`) writes
the unsaved ones: the literal of a key's one top-level set line rewritten in place, a key no
line names appended to the `-- zemi: saved from /config` block at the end, one set by code or
twice left alone, and every key left alone when the result would not parse (`planInit`),
through a temp file renamed over init.lua (its target when it is a link) and refused when the
file changed since it was read. `config lua` tags a key save would leave alone `-- unsaved,
save leaves it` and, when a model can run, offers `config ask`, which sends those keys,
recomputed at the press, to the agent (`saveRestPrompt`). Under `-p` only `config set` runs.

## Architecture and control flow

`main.go` is wiring only. Boot order (each step is a `lua.BootStep`, see `steps` in main.go),
and the order matters: host namespaces (`zemi.*`) -> builtin commands ->
`require("zemi.tui")` (shipped theme, widgets and keymaps, one module each) -> project
trust (`trustGate.step`, before any config that can start an MCP server) -> plugins -> user
`init.lua` -> user skill hooks -> a trusted project's `.agents/init.lua` -> markdown commands
(`commands/*.md`: user, each plugin's, then a trusted repo's `.agents`) -> `app.ready`.
Boot runs on its own goroutine and may block on the trust prompt.

**Project trust** (main.go `trustGate`, DESIGN §5): one decision per repository, keyed by
`Dirs.TrustRoot()` (the symlink-resolved repo root, searched no higher than below `$HOME` so a
dotfiles repo is nobody's root; a linked worktree maps to its main checkout by reading the
`.git` file, never by running git) and saved in `<state>/trust.json`. Order: a decision held
for this process (`--trust`, a Later answer, `/trust` where saving is impossible), the saved
one, then the Go-owned prompt. `$HOME` and `/` are never asked about or saved
(`hostapi.TrustUnsaved`). `/trust` reopens the prompt off the UI goroutine, saves, calls
`Host.SetProjectTrusted` and reloads when the decision changed (a reload that fails before the
trust step must not leave the old decision in force). `Host.ProjectTrusted()` gates project
`.agents` (`Dirs.ProjectConfigDir`, at the repo root only: init.lua, lua, skills, agents,
commands, workflows), instruction files (AGENTS.md, else CLAUDE.md, per directory) and profile `context.files`,
project skills, every language server, the Lua tool's ask, the read jail reaching past the
launch dir to `Dirs.RepoRoot()`, and what auto mode and read-only agents settle. `Host.git` and
`internal/plugins` shell out to the `git` binary (no git library in go.mod) through `proc.Git`,
with `GIT_CONFIG_NOSYSTEM=1`, and switch off fsmonitor, hooks, submodule recursion, LFS
smudging, every transport but https, ssh and file, and repo-local filter drivers (a plugin
clone's global ones too) in every repository, trusted or not: a sandboxed command can plant a
nested repo whose config zemi would run.

### The concurrency rules (DESIGN §4) - violate these and things deadlock or corrupt

1. **Only the Lua actor goroutine touches the `lua.LState`.** Everything else goes through the
   queue in `internal/lua` with priority classes `ui > agent > io`. `ctx` cancellation preempts
   running Lua at the next opcode (catchable once, then re-raises). A task (`lua.Task`,
   internal/lua/task.go) runs a callback as a coroutine: a handle's `wait()` yields it, a host
   goroutine waits, and the next slice is an ordinary request, so a resume never touches the
   `LState` off the actor. The task's thread stays in `Generation.tasks` until it ends, and the
   sandbox's `coroutine.resume` raises for it: only the driver resumes a task.
2. **Watchdog / hot mode.** A wedged request trips the watchdog; after a grace period the
   runtime goes "hot" - every Lua request fails fast until force-reload builds a fresh `LState`
   (generation). All registrations (keymaps, widgets, events, timers, jobs, Lua tools, a
   permission mode's `decide`) are generation-tagged (`hostapi.Gen`) and purged before replay,
   so never keep a pointer into an old generation. `ForceReload` refuses an init.lua that does
   not parse and keeps the old Lua permission rules (`staleRules`) until the replay succeeds (a
   failed one, an init.lua error included, keeps the deny and ask ones and skips `cfgSettle`),
   and `tool.pre` and `permission.request` vetoes wait out a reload, so a turn is never checked
   against half a config.
3. **UI single-writer.** All `ui.App` state mutates on the bubbletea Update goroutine; work
   from other goroutines is funneled through a hook channel (internal/ui/hooks.go). hostapi
   never imports internal/ui - it defines `UIHooks` and the App implements it; keep that
   direction. Corollary: never call `program.Send`/`program.Quit` from inside `Update` (or
   anything it calls) - bubbletea v2's `p.msgs` channel is unbuffered, so the loop would wait
   on itself (the `/quit` hang). Wrap such calls in a goroutine, like `App.Quit` does. Blocking
   hooks (`AskPermission`, `AskTrust`, `AskQuestion`, `ReviewMemories`) must only run on non-UI
   goroutines.
4. **UI-critical Lua handlers (keymaps, widget render, event handlers) must be fast** (~10 ms).
   Long work belongs in `zemi.job` / `zemi.http` / `zemi.timer`. `wait()` suspends a task (a
   Lua tool's `execute`, a Lua command, a keymap function, an overlay callback, an
   `on_done`/`on_exit`, a `tool.post` handler, a mode's `decide`, `Runtime.Eval`) and raises
   anywhere else: a render, any other event handler, a timer, under `pcall` or a library
   callback (`lua.InGoCall`). A handle's `wait` is a `lua.Suspending` wrapper, since gopher-lua
   drops the resume values of a Go function reached by a tail call. A keymap or command returns
   once it suspends (`StartTask`), so the next key runs; a purged generation ends every waiting
   task at once.
5. **Vetoable events** (`permission.request`, `app.quit`, `composer.submit`, `tool.pre`) have a
   50 ms Lua-handler deadline and then take a logged default (fail-closed for permission asks).
   While the runtime is hot those handlers cannot run, so both deny until `/reload`.
6. **No bare goroutines in hostapi.** Start them with `h.GoSafe(where, fn)`: a panic is logged
   with its stack, shown in `/errors`, and zemi keeps running. Turn goroutines also defer
   `h.endTurnAfterPanic()` so the agent is not left "active". A panic in a plain `go func()`
   kills the process with the terminal still in raw mode.
7. **The store** (`internal/store`) is SQLite via modernc.org/sqlite (pure Go), WAL,
   `MaxOpenConns(1)`. Two handles: a global DB (memory, kv, skills) and a per-project one under
   `<state>/projects/<hash of Dirs.TrustRoot()>/`, so a subdirectory and a worktree share the
   repository's sessions and memories. Schema is one `schema` const of `CREATE TABLE IF NOT
   EXISTS` run on open - add tables and columns there. There are no migrations before the
   first release: a schema change bumps `schemaVersion`, and a store stamped with another one
   is refused with its path, to delete (the owner is the only user).

### Tool pipeline (internal/tools/tools.go)

Every model tool call runs: JSON unmarshal -> schema validation -> `tool.pre` Lua events (deny
/ halt / rewrite) -> permission gate -> run -> `tool.post` handlers, then an override's `post`
(`Host.toolPost`, each a task seeing the output the last left; a `{ output }` return rewrites
what the model gets, never the diff, `changed` or the decision, and a project one is ignored;
no handler, no Lua). Invariants: a
rewritten input **re-enters at the permission gate** (per-run pipeline copy with
`RewriteLoosened`, so a non-user rewrite can't ride a pre-existing allow glob); an ask with no
overlay available fails closed; a denied result says who decided and why; errors become
tool-error responses, not panics. Host-implemented tools (`agent`, `workflow`, `lua`,
MCP and Lua tools) call the gate themselves and run the events through `preCheck` (which
refuses a rewrite with an error result), `postNotify` and `Pipeline.CheckDenied`. A command a
permission mode settled runs sandboxed (per-run `Pipeline.Sandboxed`).

Bash is special: `Pipeline.Check` normalizes the command once (`perm.Normalize`: compound splitting,
redirect and env-assignment gates - internal/perm/bash.go, heavily table-tested; extend those
tables when touching it), and `perm.Store.CheckCommandFor`, the predicates on `perm.Normalized`,
the ask hook and `alwaysChoice` all judge that one parse (`Request.Command`); a nested
`sh -c` body or `eval` text is parsed again, and so is each segment `alwaysChoice` offers a rule for. An assignment prefix the normalizer strips downgrades an allow
verdict to ask, since the rules never see it and `GIT_EXTERNAL_DIFF=./x git diff` runs `./x`.
A segment with a word that climbs (`docs/../x`) is judged as written and with the word cleaned,
and the stricter verdict holds (`cleanDotDot`), so `cat docs/*` does not allow it; a path word
the shell could still expand into a climb (`{`, `$`, a glob after a leading dot) turns an allow
into an ask (`expandsToDotDot`). A deny, ask or not_match rule also meets the segment's `perm.canonical` form
(reserved words and runner prefixes like `env`, `sudo`, `timeout 5` stepped past, the program
by base name, its leading options dropped), so `git -C . push` meets a `git push*` rule; an
allow is judged as written, and a git alias still slips past (NOTE in the code).

File tools (internal/tools/fs_tools.go): `write` and `edit` refuse a file the
model has not read, or one whose mtime moved since (`Pipeline.Seen`, per session; subagents get
their own), and hold a per-path lock, so a concurrent agent or the user's editor cannot lose
changes; within one step the lock is taken in call order (`tools.CallStart` from fantasy's
`OnToolCall`, which fires for every call before any is dispatched; `tools.CallDone` from
`OnToolResult`, which fires for a call fantasy refused to run too, and once more for the
runner's ids when its turn ends, so a call nothing ran never holds the file), so two edits to
one file emitted together apply as written. A live call's row waits (`wait`, drawn with no
timer) until fantasy starts it (`agent.runningTool`): a serial call (bash, lsp) holds the rest
of its step. A mistyped path names up to three siblings it could have meant (`Pipeline.notFound`), for read and edit alike, before any ask.
`ResumeSession` rebuilds that map from the stored rows (`seedReads`), leaving out
files touched after the last message. `tool.Run` runs the same check before the gate
(`tool.edits`), so nobody approves a change the tool would then refuse, and resolves the path
once (`perm.Resolve`) before the gate into the per-run `Pipeline.Path`; the tools open exactly
that `perm.Path` (`Path.Open`, an `openat` walk with `O_NOFOLLOW`) and never the input string,
so a sandboxed command cannot swap a directory for a symlink between check and open.
`zemi.fs`, `@file` attachments and the `lua` tool's `path` open through a `perm.Path` too
(`resolveJailed`). Both writers refuse a path whose last component is a symbolic link
and name the target to edit instead. `glob` and `grep` are gated on their root only, so neither
shows a hit a deny or ask `fs.read` rule covers, and walk the root as written (NOTE in the
code). grep and a directory listing leave out the strict secret shapes
(`perm.PathIsStrictSecret`: not `*secret*`, `*credential*` or a `.env.example`): a listing names what it hid, and grep's answer never depends on them, since a
count of their hits would leak a key one bit per call. Output too long for a result is cut
head and tail at line breaks, the marker naming the cut lines, and saved to the scratchpad
(`tools.Spill`); command output, `!command` included, goes through `tools.CmdOut`, which also
counts what the 1 MB buffer dropped. A bash command still running at its timeout is handed to `Jobs.Adopt` instead of being killed
(which is why it is not bound to the turn's context; an interrupt still kills it). A bash row
shows what the command changed (hostapi/snapshot.go, `Pipeline.Changes`): a git tree of the
repository before and after, written through a warm temp index and object directory so the
user's index and object store are never touched; it leaves out the secret shapes the sandbox
hides. The two are diffed into the result's `Metadata`; the model gets one line of paths and `tool.post`
every path as `ev.changed`, next to the file tools' own. `read` carries the nearest
instruction file not already in the prompt, once per session, so a monorepo package's own
AGENTS.md reaches the model.

### Permission engine (internal/perm)

Rules `(subject, pattern, decision, not_match?, principal?)`, subjects like `tool.bash`,
`fs.read`, `fs.write`, `net.<host>`, `lua.<ns.call>`. Ordering: deny > ask > allow; within one
decision, fewest wildcards then longest literal prefix wins. A `not_match` hit falls through
and **forces ask**. The default when nothing matches is `Store.Default` (host wires
`fsDefault`: jail reads and the model's reads of the plugins, minus any `.git`, allow except
secret-shaped paths, writes ask) and finally ask - there
is no fail-open path. New host-API side effects need the same `lua.` subject.

`permissions.mode` (internal/hostapi/permmode.go) only answers asks that survive the rules and
the `permission.request` veto, and it is deterministic: no model screens anything. `yolo`
allows, an ask a rule asked for included (an `ask` rule, a `not_match` exclusion:
`Request.Explicit`, which `auto` and a mode's own rules leave to the user). `auto` settles what `autoLocal` covers - reads and edits **inside the jail** that are
not secret-shaped (reads of the local plugins too), `tool.workflow` scripts (each agent they start is gated), bash commands
(`unsettled`), and MCP tools a server annotates
read-only - and hands the rest to the user with a reason naming why (`askHook`; writes under
`.git` or zemi's own dirs (a local plugin included: it loads in every project, so like the
recipes it asks every time, with no always answer, except one whose creating write the user or
yolo allowed in this process (`madePlugins`, recorded by `askHook`), which auto then edits in any
project), to the Lua in any `.agents` (`init.lua`, `lua/`) and to a
`.gitattributes` or `.gitmodules` are `protectedWrite`, checkouts in `.agents/worktrees` aside,
which no rule settles, a mode's own included; the rest of `.agents` (skills, agents, commands,
workflows) loads nothing into the interpreter and is an ordinary project write).
A read-only agent (`Profile.ReadOnly`, see below) denies writes and runs commands with the
project unwritable in the sandbox, whatever the mode; without trust or a sandbox, a changing
command asks (under yolo, only without a sandbox), as does an MCP tool not marked read-only.
Network requests are never settled: the URL is the model's. A mode from config may carry
`decide`, a Lua function answering one ask ("allow", "deny", anything else sends it to the user
whatever the base would settle, which is how a user gets every ask); it runs as a task for at
most 60s, long enough for a model call of its own, and, like the mode's own rules,
cannot allow an `Unsandboxed` or `Network` request, an `Explicit` one or a `protectedWrite`. In a trusted project with a live sandbox `unsettled` parses nothing:
every command runs confined, except the short `perm.Normalized.Destructive` list (forced or recursive
`rm`, or one with a glob operand, `git clean`, `find -delete`, `shred`, `truncate`, `dd`,
`rsync --delete`, also behind `xargs`, `find -exec`, `sh -c`, `eval` or a shell keyword, and a
bare `rm` in a loop body or in a shell body `xargs` or `find -exec` repeats), a git command that writes `.git`
(`perm.Normalized.GitWrites`: commit, checkout, stash and the like, which the sandbox's read-only `.git`
could only fail, so the ask comes before a wasted run) and, on a backend that cannot hide
files, one naming an existing secret-shaped file. Known ceiling: the sandbox policy is the only limit, so the model
can write code and run it unattended, rewrite any project file and read what the policy does
not hide, outside the project too. In an untrusted project both modes settle only `ReadCommand`
commands (every program only reads, named without a path; no builds, no git) and auto settles
no project writes (a `madePlugins` plugin is in no project). In a trusted project with no sandbox backend auto still settles jail writes, but
commands settle only when `ReadCommand` holds, since an approved one would run unconfined.
Asks reach the user one at a time (`Host.askMu`, held over the prompt stage only, never over
the veto or `decide`), and a waiter is judged again once it holds it (`Request.Recheck`, then
the mode and trust with no Lua), so an answer, a switch to yolo or new trust settles the asks
behind it, a rule's verdict returned whole (`tools.ByRecheck`) so a network rule keeps the sandbox.

**Sandbox** (internal/sandbox, internal/hostapi/sandbox.go, DESIGN §9): a `tool.bash` verdict a
mode gave (`DecidedBy` `mode:<name>`, base not yolo) makes `Pipeline.SandboxFor` return
`h.sandboxPolicy(readOnly)`; `tool.Run` puts it on the per-run pipeline copy, and `runBash`,
`Jobs.Start`/`Adopt` call `sandbox.Wrap` before starting. A wrap error is a
tool error, never an unconfined run. Rule-allowed and user-approved runs, `!command`,
`zemi.job`, MCP servers and `Host.git` stay unconfined, except under a read-only agent, whose
every command runs confined whoever allowed it (`Request.ReadOnly`; `unsettled` then skips the
`Destructive` and `GitWrites` asks, since the confined run fails on its own); language servers
run under `serverPolicy`. `NewHost` probes the backend synchronously, so `--health` and the first turn's
schema see it.

`sandboxPolicy` writes the policy; the whys behind it: the sandbox temp dir (`TMPDIR`) is the
session's scratchpad, so the file tools read what a command left there without an ask, and
never the shared system one (macOS keeps the `xcrun_db` there that picks what `/usr/bin/make`
and `git` run; a denial naming `/tmp` gets a hint pointing at `$TMPDIR` before `sandbox: false`),
`GOCACHE` is one per trust root (Go trusts cache entries, so a shared one lets one project
poison another's build), the project is writable only when it is trusted, outside zemi's own
dirs and on a backend that can protect paths inside it (`Backend.EnforcesProtect`), `.git`,
`.agents` and every `PATH` dir under a write root stay read-only because zemi runs git, sh and
servers unconfined by name, and home credential paths are hidden. The exception is the repo's
`.agents/worktrees` (`Dirs.WorktreesDir`), where the agent keeps git worktrees: a write root of
its own, ignored by the `/*` in its own `.gitignore` (`Dirs.WorktreesIgnore`, written only
when missing, by the trust step of a trusted boot and by `sandboxPolicy`), with
each checkout's own `.git` and `.agents` protected and nothing loaded from it. The namespace
backend binds only protected paths that exist, so `Pipeline.Guard` records the missing ones
before a run and checks them at exit: a created `.git` or `.agents` holds trust off until
`/trust`. Bash and `job_start` take two escapes no mode settles and each of which
asks: `network: true` only adds sockets, `sandbox: false` drops the whole policy. A confined
failure `sandbox.LikelyDenied` recognizes gets `Pipeline.sandboxHint`, which names the writable
dirs and offers `network: true` first.

`ReadCommand` is an allowlist of readers (internal/perm/safe.go, table-tested), which settle
unconfined: no shell expansion outside quotes (`$` and backticks inside double quotes count),
no write redirects (`2>/dev/null` and `2>&1` are none), no program named by a path, and every
argument must pass `safeCommandPath` (inside the project and not secret-shaped - `h.secretPath`
matches the resolved path too, so a symlink cannot hide one, and every home path the sandbox
hides (`secretDirs`: `~/.kube`, `~/.config/gh`, shell histories), whose names say nothing, so
one list drives the sandbox, the re-ask and auto's read check; a plain read may also reach the
scratchpad, zemi's config tree and the local plugins, but `cd` may not enter them). Readers keep their
`refusedFlags` and script checks, and `perm.ArgUse` says how an argument is used: a search
pattern is no path, and a recursive search settles only over a tree with no secret-shaped file
in it. Nothing may point into `.git`, since `.git/config` may hold tokens.

Every rule lives in memory: `zemi.permission.rule.add` in config adds it again on each boot,
and an "always" answer lasts until zemi exits. There is no rule file and zemi makes no git
commits (`scope = "permanent"` raises and points at init.lua). Untouched by that "no rule
file": a trusted project's `permissions` file in its state dir (`<state>/projects/<hash>/`,
next to the recipes, where no repository can ship it and no sandbox can write it;
`ProjectPermissionsStep`, internal/hostapi/projectperm.go) lists bash commands always allowed in that project, each by
the `alwaysChoice` rule shape (a wrapper or deleting program or a glob line offers no rule);
it is read as a Go boot step, so it never goes through the Lua origin gate below, and a
permission overlay's `for this project` answer appends the command to it, offered only when
the file would load exactly the rules the label names (`projectChoice`). Only `user`
origin may add allow rules (project may tighten only), scope a rule to one plugin
(`principal = "plugin:<name>"`, which a plugin's refused call puts in the rule line it names;
a plugin's `zemi.agent.send` and `zemi.model.ask` ask `lua.agent.send` and `lua.model.ask`)
or switch the mode
(`zemi.permission.mode.set`, which the shipped ctrl+y yolo toggle uses); the gate lives at the
namespace closure, same off-interpreter-`OriginHost` reasoning as skills.

The overlay's "always" answer writes session allow rules scoped to the `model` principal
(`Rule.Network` when the ask was a `network: true` command, so `SandboxFor` keeps the policy and
adds sockets instead of running the next call unconfined; `projectChoice` offers no file line
for one), as prefix globs rather than the exact request (`alwaysChoice`, ask.go: a command's program, or
program and subcommand for `git`, `go`, `cargo` and the other `subcommandPrograms`, leading
options kept (`git -C sub status`, `perm.Subcommand`) and the exact segment when options leave
no subcommand (`git --version`) or one not for sure (`npm -w web test`: `perm.LeadKnown`); a file's directory, or the file alone when that is `$HOME`, `/` or between,
which hold every dotfile; a URL's host). A deleting program (`rm`, `mv`, `find`, `tar`, ...) or a segment
`perm.Normalized.Destructive` names stays exact, since its rule would run unconfined. A file read is also
offered the wider `repoChoice` ("allow reads anywhere in <repo>", answered as `user:wider`),
because in a deep tree the directory rule is the same ask again one package over. No always
option is offered at all for a gate-forced ask (heredoc, env assignment), a pattern holding a
glob wildcard, an opaque segment (an inner `sh -c`, a stripped assignment) or a wrapper program
that runs what it is handed (`perm.WrapperPrograms`: shells, interpreters, `sudo`, `xargs`,
`busybox`, `awk`, `zemi` itself, ..., the list `perm.Normalized.Destructive` and `perm.Normalized.GitWrites` look
past too, and `wrapperSubcommands` for the `npm exec`/`uv run` spellings of `npx`) - no rule
written from such a segment says what the next call would run - nor for a `protectedWrite`,
which `Pipeline.Check` asks about whatever rule allows it. A secret-shaped file is offered only
its exact path: a directory glob cannot exclude the secret shapes, so `Pipeline.Check`'s loosen
sends a file request a `model` glob rule allowed back to the ask when `Secret` matches, and a command whose
reader names one (`perm.Normalized.ReadsSecret`: `cat .env`, `cat .e*`) when a `model` rule or a
permissions file line allowed it (`perm.Rule.Source`); a user-config rule has no `Source` and
no `model` principal, stays authoritative, and shows read-only in `/permissions` with its
source (init.lua, the permissions file, a project's `.agents/init.lua`) and, when it scopes one
plugin, that `plugin:<name>` principal as its tag, while each always answer is one row (its
bare and wildcard rules, `answerKey`) whose Delete drops both; the row has no `run`, so
space deletes nothing.
`sandbox: false` is denied outright for a read-only agent (`Pipeline.Check`, before any mode
could settle it), and its sandbox hint offers only `network: true`.

### require and the origin model (internal/lua/runtime.go, origin.go, registries)

Modules are cached per generation by resolved file and requirer: a requirer below the file's
origin gets its own instance run under its own origin, one per plugin, so project or plugin code
cannot plant functions into a table that higher-origin callbacks or another plugin call; a
builtin module for lower code and one plugin's module for another plugin are the owner's
instance behind a deep read-only view. Dotted names search `<root>/lua` over the runtimepath:
the project's `.agents` first but only when the requirer is project code, user, then the one
plugin the plugins step loaded (`lua.Generation.Plugins`, resolved, fixed per generation) named
like the first segment, which that plugin's own code searches first: a plugin answers only its
own names, so none shadows another's. `./x` and `../x` resolve against the calling function's
chunk name, which is why file chunks are compiled with their absolute path, and must stay in
that file's origin tree and, for plugin code, its own plugin; a plugin file resolves only
inside an enabled plugin, so a symlink or `..` cannot run a disabled one.
`load`/`loadstring` tag their chunks with the running origin and never keep an absolute name,
so neither can be forged.

Every chunk carries an origin from its resolved path class: `user` (~/.config/zemi, plus each
subdirectory of it in `Dirs.UserTrees`, which the read jail registers too, so a symlinked
`skills` or `lua` still resolves inside) > `builtin` (embedded runtime/) > `plugin`
(`<data>/plugins`) > `project` (<repo>/.agents). The code a call runs as
(`Runtime.CurrentOrigin`, `CurrentPath`) is the least trusted of the frame the host entry pushed
and every Lua function on the stack, resumers included, builtin ones transparent: a plugin's
`setup()` that init.lua calls runs as the plugin, and a user handler a project's
`zemi.event.emit` runs inline runs as the project; chunks compile with no tail calls, which
would drop a frame. A metamethod leaves no frame of the code that set it, so the sandbox's
`setmetatable` (tables only) lets code below builtin install only its own origin's Lua
functions as handlers, and a plugin only its own plugin's, copied at the call, and a plugin module
that exports a Go function fails to require (`exportsGo`: `obj:method()` would run it as the caller
with the module table as its argument). Plugins (hostapi/plugins.go, `PluginsStep`): `plugins.Scan` lists
`installed/` (clones) and `local/` (developed in place) minus `disabled`, sorted by name; a local
plugin named like an installed one or a symlinked dir does not load, and a gone or
reinstalled plugin's `zemi.state` keys are dropped (`sweepPluginState`: the key `plugin:<name>`
holds the id `plugins.Install` writes into the clone's `.git`). The same pick, fixed for the generation (`Host.pluginDirs`),
gives the plugins' skills, agents, markdown commands and workflows, loaded after the user's and
the builtin ones, init.lua failed or not; a name two plugins ship goes to the first by name, and
`/errors` names both. `Host.PluginOf` names a plugin by its directory: its state keys are
`plugin:<name>:<key>`, its Lua permission principal and its overlay badge `plugin:<name>`
(`Host.principal` raises for plugin code with no file of its plugin on the stack), and a call from plugin a into plugin b's function runs as a. Registries enforce: cross-origin
collision is **refused** with the higher-trust origin winning; within one origin it is
last-writer-wins, except that one plugin can neither replace nor remove another's command, keymap,
widget, theme, event handler, tool, completion source, health row or config watch (`mayReplace`, which every add and remove runs). Project origin additionally cannot register providers or builtin
tool names, and its `tool.pre` rewrites are ignored (logged). No origin can emit host-owned
events. A keymap that is a prefix or an extension of a more trusted key is refused,
`zemi.command.run` refuses a more trusted origin's Lua command (its body runs under that
origin), overlays can be closed only by user, builtin or the origin that opened
them, and `app.quit` denials from project handlers are ignored. When adding a
registry, follow `internal/hostapi/registry.go`.

### Skills (internal/skills, internal/hostapi/skills.go)

Skills are file-based units, each a `SKILL.md` with YAML frontmatter: user skills under
`<config>/skills/`, then `~/.agents/skills/` (`Dirs.AgentsHome`, in `UserTrees`; body only,
since installers write there; the config dir wins a name clash), builtin (zemi's
self-knowledge, `runtime/skills/zemi-*`, embedded and rewritten to `<data>/builtin-skills/` on
every sync) and plugin (`skills.Plugin`, each enabled plugin's `skills/`, body only), all in the
table and enabled by default. A trusted project adds its own (`.agents/skills`): body only,
enabled, never in the table or `skillsReg`, scanned by `SetProjectTrusted`, which every reload
runs, so a skill written mid-session loads with the reload the config watcher starts. Recipes (`skills.Recipe`) are the project's
private skills under `<state>/projects/<hash>/skills` (`recipesDir`, a read jail root no sandbox
policy can write): trusted only, body only, enabled, and they lose every name clash. The
pinned prompt (`ag.pin`, below) lists the ones found when it was built; `skillIndex` and
`enabledSkills` discover the dir live, so a recipe written mid-session loads without moving the
prompt, and `runSkill` marks the file read so an `edit` can follow. The agent writes recipes with
the plain file tools (the environment prompt's `Recipes:` line says how); the dir is a
`protectedWrite`: no rule and neither auto mode nor a read-only agent settles a write (yolo still
allows), and the ask says when `sessionLocalOnly` fails.
`recipeAfterWrite` (the `AfterWrite` hook, run before the writers' `markRead` because it may
rewrite the file) scrubs every file written there in place (`learn.ScrubShapes`: no entropy
pass, which cuts the long paths and `NAME=url` a procedure is made of), names the redacted
lines and tells the model what will not load. `/skills` and its commands read the dir live.
`/skills delete <name>` (`deleteRecipe`, the Delete action only recipe rows have) removes one
and drops the pinned prompt. Invariants:

- **Only user or builtin origin may enable/disable** (`refuseBelowUser` gate at the Lua
  namespace boundary - NOT inside the Go helper; off-interpreter goroutines read `OriginHost`,
  which would defeat a Go-side gate). A disabled skill leaves the index but `/skills use` still
  sends it.
- **Token efficiency**: the prompt carries only the index (name + description, cut to 512 runes
  with a warning); bodies load on demand via the `skill` tool, after a permission check.
- **Hook placement**: a `<config>/skills` skill's `zemi-skill.lua` runs once, in the boot
  step after user init.lua, as user origin; enabling later does not run it.

### Type annotations (internal/hostapi/types.go, runtime/types/zemi.lua)

`runtime/types/zemi.lua` is the LuaLS (`---@meta`) description of the whole `zemi.*` API, never
loaded by the interpreter. `syncTypes` rewrites it plus a copy of `runtime/lua/` to
`<data>/types/` on every start, and the `.luarc.json` in `<config>` and in each local plugin whenever
its content moved (an editor reloads on each write; the config watcher skips it). `TestTypes*` keep it
honest: every API member annotated, nothing annotated that zemi dropped, and the
`zemi.EventName`, `zemi.HostAction` and `zemi.WidgetCtx` aliases pinned to their Go sources, like
each constant table (`enumTable`: `zemi.provider.kind`, `zemi.permission.decision`,
`zemi.memory.scope`, `zemi.model.role`, `zemi.model.effort`, `zemi.notify.level`, keyed by the
value upper-cased) and the alias that types the fields taking one (`TestTypesEnums`), and every
other closed set Lua sees (a Go `type X string` with a slice of its values: `RowKind`, `CallStatus`,
`HealthLevel`, ...) as a multi-line `---|` alias, the layout kinds as the `kind` literals of the
`zemi.LayoutNode` classes (`TestTypesLayoutKinds`). It
also carries `---@deprecated` stubs for the globals the sandbox strips, since the `.luarc.json`
files turn off LuaLS's own libraries and it flags a deprecated global only when every
definition is deprecated. Known ceiling: with no CI, `make luals` runs only where
lua-language-server is installed.

### Turns, sessions and the learning loop (internal/hostapi: agent.go, session.go, compact.go, memory.go, reflector.go)

- **Models are roles.** `models.chat` runs the conversation; `models.agents` (subagents with
  no model of their own), `models.titles`, `models.compact` and `models.memory` (the
  reflector) each follow it while empty (`roleModel`). Nothing falls back to a cheaper model on
  its own, so the `/model roles` list shows every mapping. A `models.*` value is `""` or an id
  with a `/` (`config.set` refuses a role name), so roles never nest. A profile's own model
  runs the conversation and moves no role: `chat` is always `models.chat` (`roleIDs`,
  `zemi.model.list`, a `"chat"` reference), and the `/model roles` chat row tags the override
  `profile <name>`. Anything that takes a
  model - a profile, an agent definition, a workflow - takes a role name or a
  `provider/model` id (`modelRef`). `/health` checks each distinct role model.
- **One ledger.** Every model call is billed through `Host.bill`: the session row, and the
  live tally when that session is still open. Calls outside a runner (summary, title,
  reflector) go through `Host.generate`, which bills them and sends the provider's body fields
  without the effort; the reflector bills the session it read, closed or not.
- **The active profile is a config key.** `agent.profile` names what `resolveProfile("")`
  runs (`default` is the shipped shape), so only user config writes it and a reload keeps a
  runtime pick like any key. `PurgeGeneration` clears the definitions and `cfgSettle` returns
  a key config stopped writing to `default`, so deleting a profile and its `config.set` line
  runs the shipped one; a name nothing defines (a typo, a pick of a since deleted profile)
  fails the send. A profile is defined with `zemi.agent.profile.add`: a `-name` denylist or an
  allowlist of tools, so a new tool is in the default set until a profile drops it.
- **One runner per send, one conversation.** `ensureRunner` rebuilds the runner (model and
  tools may change; the prompt comes from `ag.pin`); `AgentSend` copies `History` from the previous idle runner. `runTurns`
  loops the queue's follow-ups on one runner through `sendCompacting` (compact.go): compaction
  runs before a send, when `Runner.Full` cuts a turn at a step boundary, and once after a
  context-too-long refusal. The size is `agent.ContextUsed` of the last step, never a sum; an
  unknown window means no auto compaction. When `models.compact` is the runner's model
  (`ag.turnModel`, "" for the bare runner a resume or rewind leaves) the summary is asked
  through `Runner.Aside` after the unpruned head (`summarizeAside`, `compactNote`), so it
  reads the provider's cache; a separate compact model, or an aside with no summary or a
  context-too-long refusal, gets the flat `summarize`. The summary row is followed by the verbatim tail
  stored with row role `tail`, which resume reads and transcript readers skip. An unfinished
  todo list goes after `</summary>`, where `summaryOf` drops it, so a stale one is never
  merged.
- **A turn's work survives the turn.** `Runner.Persist` writes each step's messages from
  `OnStepFinish`, so a cancelled or crashed turn keeps what it did; the next send runs
  `closeToolCalls` over the history first, inserting an error result after any tool call left
  unanswered (providers refuse a dangling call); a reply cut mid-arguments by a provider that
  then drops the call (openai-compatible) gets a note naming the pending tool instead, so
  the model does not repeat the same oversized call. Every resolved model is wrapped in `idleModel`
  (providers.go): a call that makes no progress for 5 minutes is cancelled, since the HTTP
  header timeout stops counting once the status line arrives and a stalled stream would hold
  the turn forever; the timer resets on each stream part and tools run between model calls, so
  a slow build is never covered.
- **Steering.** Messages sent mid-turn queue; `Runner.Steer` injects them at the next step
  boundary through fantasy's `PrepareStep`, which rebuilds step input from scratch, so steered
  messages are re-inserted every step. Leftovers become one follow-up turn, and `active` stays
  held across that handoff (`nextTurn`) so a submit cannot jump the queue; `/compact` and
  rewind claim `active` and a `turnScope` together for the same reason.
- **Side questions.** `/btw` (`Host.btw`) sends `Runner.Snapshot` (mid-turn, the running
  step's input) plus the question on the runner the next send would build, one step through
  `Runner.Aside`: same tools and tool choice, since Anthropic drops the message cache when
  tool choice changes, with every tool refusing to run. Billed, never persisted.
  `Host.aside` is that call; the idle recap (`Host.Recap`, `session.recap`) is one more: the UI
  calls it `recapDelay` (3 min, inside Anthropic's 5-minute cache) after a turn that ended well
  with no key, paste, click, wheel or focus since, which also cancel it; a turn that ran a tool
  gets a live-only `recap: ...` notice row, dropped when the transcript moved during the call.
- **Store order.** `Messages` orders by `rowid`: a turn persists several rows in one
  millisecond and ids are random. Tool telemetry rows share the table; `historyFrom` skips
  anything that does not decode as a fantasy message, and reads a file tool's or a bash call's
  diff, an agent call's run and summary line and a step's thought time back from the
  telemetry rows that carry them (a resume could not rebuild them).
- **Live equals settled.** The live turn holds one stretch in stream order (reasoning, calls,
  reply); `flushOutput` moves it into the transcript whenever the next part would break that
  order (reasoning after a call or a reply, a call after a reply), so a step's reasoning lands
  under the calls before it. It moves only the calls that finished, so none settles early, and
  moves both sides under `ag.mu`, which the widget ctx reads them under (`liveView`), so no
  frame draws a row twice or not at all; `TurnDone` settles a call still open as stopped after
  an interrupt and as failed after an error. `historyFrom` splits text and merges reasoning the
  same way, so a resume draws the rows the live turn drew.
- **Proposals persist** in the scope DB until `/memory` or `zemi.learn.decide`; `o` opens the
  raw text through `EditFile` after the other marks apply, re-running intake as user origin and
  keeping the id.
- **Reflector timing.** It runs on session close (`/new`, `/resume`) and in a startup sweep
  (`ReflectSweep`, once per process, not on a reload) over the newest sessions idle for ten
  minutes, at most three, never on quit. `reflectDue` reads the delta past kv
  `reflector.seen.<sid>` (`RewindTo` clamps it, `ForkSession` starts it at the copied history)
  and is due only when the delta has two user messages that are not compaction summaries and a
  tool call, or one failed tool call: one exchange that went fine would cost a model call at
  every launch; `/reflect` skips the floor. `claimReflect` takes an atomic kv claim per session
  (`reflector.claim.<sid>`, leased), so two windows never reflect it at once, and
  `reflectClaimed` runs it (one at a time; a skipped run releases its claim). It runs on
  `models.memory`; a reply that is not the JSON document asked for marks
  the delta seen and is the `/health` reflector row's news, never an `/errors` entry, and a
  second failed try (kv `reflector.tries.<sid>`) marks it seen too. Its prompt carries the
  scrubbed delta (`reflectLine`), the newest failed tool results (`failedCalls`, read from the
  history rows: the telemetry rows keep no text), the instruction files (`instructionSections`,
  shared with `systemWith`), the whole memory index with budget use and quotes, and the
  project's skills and recipes by name, so it leaves a repository's procedure to a recipe. It
  proposes memories only, no skills: each entry passes `learn.Intake` with its `quote` and
  `gateMemory` like the `memory` tool's (stored at once under `memory.autonomy = "auto"` when
  policy-clean, queued for `/memory` otherwise, a quote not in the session dropped, DESIGN
  §10.4); an entry with empty text and `replaces` queues a forget proposal, never auto-applied.
  There is no config-patch pipeline, the agent edits config files directly. `digestFor()`
  renders every live memory, newest first per scope, under `learn.ScopeBudget` (3 KB per
  scope); no search, no ranking.
- **Provider text shape.** Model responses carry `fantasy.TextContent`, stored messages
  `fantasy.TextPart`; `concatText` handles both. A stub that returns the wrong one hides real
  provider bugs (this once made the reflector read empty output everywhere).
- **Jail.** `inJail` resolves symlinks on both the path and the roots; `fsDefault`,
  `resolveJailed` and `@file` attachments all rely on it.

### Subagents, workflows, agent runs, the Lua tool, MCP, LSP

- **Subagents** (subagent.go) are fresh `agent.Runner`s over `PipelineFor()` (same permission
  gate; asks are prefixed with the agent name). Definitions: builtin `explore`, `web`,
  `general` and `plan` (a read-only architect the planner delegates drafting to), plus `agents/*.md` (user over builtin over plugin over a trusted
  repo's `.agents`). They never get `agent`, `workflow`, `todos`, `question`,
  `plan` or `lua` (nesting depth 1). A run is a child session (`parent_id`);
  `ListSessions` hides children. They have no summarizer: `sendPruning` clears their own old
  tool output when the window fills and, with none left to clear, drops the middle of the
  history once, keeping the task and a recent tail. The delegation guidance lives in
  `delegationPrompt`: without it models rarely delegate.
- **Workflows** (workflow.go) are Lua scripts (model-written via the `workflow` tool, saved
  under `workflows/`, builtin in `runtime/workflows/`, a plugin's after both). They run in their **own** gopher-lua
  state on a host goroutine, never on the interpreter (DESIGN §4): a coroutine per branch,
  `agent()`/`parallel()` yield and the scheduler resumes them as subagents finish. Invariants:
  the script sees only base/string/table/math + the workflow API (no fs, net, `zemi.*`);
  `agent`/`parallel` are `lua.Suspending` wrappers, and both refuse under `pcall` or a library
  callback (`lua.InGoCall`), where a yield would unwind the Go call. The tool's description
  points at the zemi-customize skill for the API, which every request would otherwise pay for.
- **Agent runs** (tasks.go): every `runSubagent` (tool, background, workflow, Lua) registers an
  `agentRun` whose hooks build a live transcript, which `/agents view <id>` draws instead of
  the session's, and a submit while it runs goes to it through `MessageAgent`. A run's queue is
  its `Runner.Steer`; what arrives after its last step boundary becomes one more `Send`
  (`followUp`), which closes the queue in the same lock when there is nothing left, so a
  message is never accepted and then dropped. Background runs report through `AgentSend` behind
  `agent.AttachMarker`, which the live transcript and resume cut off; the notice row is drawn from
  the message wherever the model reads it (`appendUserRows`: a new turn, a step boundary, a
  drained queue, a resume), so it sits under the rows of the turn it arrived in. A report or a
  plugin send an esc leaves queued gets a toast saying it waits for the next message, and a
  report a closing session drops (`/new`, `/resume`) is stored in its own session and toasted
  (`notifyReport`), like one for a session that moved on.
  `/agents` lists the live session's runs and, under Running, the live runs of every other session (`runningElsewhere`,
  named by that session's title): they block `/rewind`, so their stop must be reachable.
- **Scratchpad** (scratch.go): `<state>/scratch/<session id>`, short enough for a Linux unix
  socket path under it, pads of every project idle a week removed. It is a sandboxed command's
  `TMPDIR`. `fsDefault` allows `fs.write` there and `Pipeline.Scratch` lets it pass a read-only
  agent; a program may read it, never run from it.
- **Lua tool** (luatool.go, tool name `lua`): model Lua runs through `Runtime.Eval` as `user`
  origin, default-deny: without approval it may call only `luaReadOnly` members. Every Go
  function in a namespace passes the sandbox guard (`Sandbox.SetGuard` -> `guardLua`), which
  raises for tool code (frame path `lua-tool*`) unless the innermost tool code is an approved
  run, and, while a widget render runs, for anything but the `luaReadOnly` members other than
  `zemi.fs.*` and a handle's `wait`. `luaUnlisted` tokenizes the script first: any zemi path
  off the list, a non-read handle method call, or dynamic access (`_G`, `load`, `require`,
  ...) asks `tool.lua` once, as `Request.Unsandboxed` so no mode settles it, and the approved
  run evaluates as chunk `lua-tool#<n>`. In an untrusted project or a read-only agent's run
  every script asks. A new API member is guarded until added to `luaReadOnly`;
  `TestLuaToolDefaultDeny` checks every member. Not a new
  origin: the gate is a flag on the chunk name. A `path` script is a `.lua` file read through
  the read tool's `fs.read` check. Runs on the interpreter as a task (10s ceiling per slice),
  main agent only.
- **Model memory writes**: the one `memory` tool goes through `memoryTool` -> `gateMemory`: a
  near-duplicate (`learn.Similar` over both scopes and the pending proposals) is refused with
  its id and text to merge through `replaces` (a pending one only as queued); a learned
  user-scope memory needs a `quote`, checked against the typed part of the session's user rows
  (`quoteInSession`, cut at `agent.AttachMarker`), and a quote not found drops it; a scope over
  `learn.ScopeBudget` queues with the oldest ids to replace, and `acceptProposal`, `moveMemory`
  and `rewriteMemory` refuse, pointing at the oldest in `/memory`. Text with `replaces`
  supersedes, empty text with it forgets (`forgetMemory`): a memory the user saved asks
  (`memory.write` as `Unsandboxed`, so no mode settles it), a learned one goes at once when
  `memoryAuto` holds (autonomy auto, a trusted project, no `fetch`, `web_search`, `lua` or
  non-builtin tool call in the session or its subagents'; `subagentHooks` records those,
  `ForkSession` copies them, and `markNotLocal` records a plugin's send or `tool.post`
  rewrite the same way, once per session and in a subagent run's own) and is proposed for
  review otherwise. A plugin's send reaches the model behind
  `agent.Attached("plugin:<name>")`, so no quote comes from it, and draws as a notice row
  (`reportHeads`).
- **MCP** (mcp.go, internal/mcp): a dependency-free JSON-RPC client (stdio, streamable HTTP
  with JSON or SSE replies, OAuth discovery where a server needs it; `client_id` and
  `callback_port` for one without dynamic registration). Servers come from `zemi.mcp.add` (user
  origin only); `PurgeGeneration` parks them, the replay keeps a live one (or one waiting for a
  sign-in) it adds with the same spec and the rest stop. They run unconfined in `DataHome` with `ZEMI_PROJECT_DIR` set,
  trusted project or not: `npx` and `uv` read config from the working directory, and most
  servers need the network a sandbox refuses. Tools are
  `mcp_<server>_<tool>`, gated as subject `mcp.<server>` with the tool name as argument. Past
  `mcpDeferAt` (10) MCP tools in a set, `deferMCP` adds `tool_search` and `Runner.ActiveTools`
  offers each step only the other tools, `tool_search` and the loaded ones: a search loads at
  most five into the session's set (`sessionLogs`, dropped with the session, reseeded on resume
  from the history's `mcp_` calls; a subagent run has its own). Every MCP tool stays
  registered, so a call to one not loaded still runs; the catalog changes only with the
  servers, and each load costs one prompt-cache miss. A start's outcome and a sign-in
  request go through `setMCP`, which refreshes an open `/mcp` (`Overlay.Refresh`, as a
  language server's finished start refreshes `/lsp`), as do a sign-out and the sign-in link. A
  sign-out stops the server and leaves it `needs sign-in` with no start, assuming a start would
  fail the same way (NOTE in the code: a server whose auth is optional waits until Restart). `ServiceStatus` (host.go) is the one status word of `/mcp`, `/lsp`, `/plugins`,
  `/health` and `zemi.mcp.list`/`zemi.lsp.list`, colored by the UI's `statusHL`, a map the
  exhaustive linter keeps complete. Test against `internal/mcp/mcptest`.
- **LSP** (lsp.go, internal/lsp): servers start lazily on the first matching file, and only in
  a trusted project: they run repo code (rust-analyzer build scripts), so `lspDefs` returns
  nothing untrusted and `noLSP` says why. With a live backend every server starts through
  `lsp.Config.Wrap` under `serverPolicy`: the project read-only, writes only to
  `<cache>/lsp/<trust key>/<server>`. A confined cargo cannot write a missing `Cargo.lock`, so
  rust-analyzer reports nothing until one exists; without a backend it (`RunsCode`) does not
  start at all. The `diagnostics` and `lsp` tools leave the tool set when `lspDefs` is empty,
  so an untrusted project does not carry their schemas every step. A server re-reads a file
  only when told, so `lspSync` diffs two `git status` listings and sends
  `workspace/didChangeWatchedFiles` for what changed; it runs before a touch or query only once
  something other than zemi's own file tools ran (`lspDirty`). A slow or failed server means no
  diagnostics, never a failed edit. Test against `internal/lsp/lsptest`.
- **Never start a turn on the interpreter goroutine.** `systemWith` runs `agent.assemble`
  handlers through `RT.Do`, so `AgentSend` from Lua would deadlock; `zemi.agent.send` goes
  through `GoSafe`.

### Screen layout (internal/hostapi/layout.go, internal/ui/layout.go)

`zemi.ui.layout.set` hands Go a layout table; `ParseLayout` validates it into immutable
`hostapi.Layout` data (no closures) behind an atomic pointer `compose` reads every frame, so
drawing never waits on Lua. The shipped `tui/layout.lua` sets the default, user config
may replace it, lower origins are refused. A reload keeps the old layout drawn (the spec is
plain data, safe across generations); `ForceReload` drops one the replay did not set again and
`DefaultLayout`, the Go copy of the shipped tree, draws. `span` sizes one box per frame from
cells, percent, content and weighted fill, takes cells back from fill first when they do not
fit, and never fails (`FuzzSpan`). The stage node is where prompts, overlays and toasts
go. A content-sized node's height is the most rows it could get, never
its last draw, so a widget that pads to `ctx.height` can still shrink. Hit testing reads the
painted segments, never the tree. `/health` has a `layout` row naming widgets in regions the
layout does not draw. A `composer` node with `frame` draws the square frame around the composer
and puts the first line of the widgets in its `top`, `left` and `right` regions into the rules
(`composerFrame`, ui/composer.go; `top` gets the whole rule, whose left corner holds the
snail of `tui/snail.lua`, its eyes showing the state, how a turn ended included
(`ctx.ended`), and whose middle holds the turn's status), which is where the shipped layout keeps the mascot and the statusline: agent
state changes what a rule says, never how many rows the frame takes, so nothing above it
reflows. The statusline clusters draw from their outer edges by widget priority and a short
row drops the highest first (`fitClusters`), so what lasts sits outermost and never moves.

### Rewind, read-only agents, prompts, composer bridge

- **Rewind** (rewind.go, store/checkpoints.go): the pipeline's `Checkpoint` hook saves a file's
  pre-turn content (keyed by the turn's user message, first write wins) before a write tool
  touches it. `RewindTo` restores files first and only then truncates messages, so a failed
  restore leaves the conversation intact; then it drops what the session learned between the
  prompt and the last row (`forgetLearned`: a reflector run or an accept after it stays) and
  rebuilds the todo list from the kept rows. A bash call's changes are checkpointed from the
  before tree `treeDiff` compares against (repository files only: not ignored ones, not outside
  a repository, no submodule or secret-shaped file, and not once two slow snapshots switched the
  snapshot off); a symbolic link comes back as a link.
- **Read-only agents** (the planner profile, `Profile.ReadOnly`) are enforced in
  `tools.Pipeline.Check` (`Plan` hook): `fs.write` denied even under allow rules, `mcp.*`
  allows and a rule's bash allow other than `ReadCommand` flip to ask; the sandbox keeps the
  project unwritable. Approving the plan returns to the agent and mode that ran before: the read-only
  turn ends (the read-only pipeline cannot implement) and `nextTurn` hands the claim to a
  fresh send as the restored agent (`planApproved`, the plan already in history), so the
  agent never passes through idle. A read-only turn that ends in plain text instead of the
  `question` or `plan` tool is nudged once per user message (`planFollowup`, `planNudge`,
  drawn as a notice row by `reportHeads`):
  a question or a report is not a plan, so the text is never presented for approval.
- **Go-owned prompts** (permission, trust, question) share one renderer in ui/agent.go
  (`promptBox`, `choiceRows`); Lua reads the composer through a snapshot the UI publishes and
  edits it through `UIHooks.ComposerEdit`, applied on the UI goroutine.
- **System prompt** (prompt.go): `basePrompt` + `delegationPrompt` for the default profile,
  then `zemiPrompt`, the read-only agent's plan section, instruction files
  under `# Project instructions` (the repository's files share a 32 KB budget spent deepest
  file first, so a huge root `AGENTS.md` cannot squeeze out the package's own; the user's
  `<config>/AGENTS.md` gets its own budget), skills, Lua sections (`agent.assemble`, then the
  `prompt` of each Lua tool in the tool set), the per-session pinned `environmentPrompt`,
  digest: most stable first, so a provider that caches any shared prefix (OpenAI, DeepSeek,
  Gemini) shares the instructions across sessions. It is built from the
  run's tool set (`hasTool`), main agent and subagents alike: no `agent` tool drops
  `delegationPrompt`, no `skill` tool drops the skills index and the zemi-* sentence, no `read`
  drops its "Using tools" line, and a read-only agent without the `plan` tool
  (headless, a profile) ends in `planHeadless`. No kept tool's description names a dropped tool
  (`TestPromptFollowsToolSet`). Anything that varies per turn must be
  pinned per session or it busts the provider's prompt cache - which is why `gitStatusSection`
  runs once per process, warmed at boot so a big repository's `git status` is not paid for at
  first token. The main agent's prefix is one pin (`ag.pin`, `pinPrompt`): the system text
  and every tool's `Info` (through `pinnedTool`), sent verbatim while its key holds (profile,
  model, `ProjectTrusted`, the mode's prompt, each tool's name and input schema, since a stale
  schema fails calls). `closeSession` (`/new`, `/resume`) zeroes it, `compactDone` and
  `deleteRecipe` clear its key (`dropPin`: `/prompt` and a running turn keep the text, and a
  build a drop overtook is not stored); rewind keeps it, and a reload drops nothing, since the config watcher reloads on
  any config-tree write, the model's own skill writes included. So instruction files, skills,
  descriptions (saved workflows, agent definitions) and `agent.assemble` output are read only
  when it is built, and a hit costs no file read and no Lua. An instruction file the pin read,
  or one created where it would load (`instructionPaths`), whose mtime moved rides the next
  user message once (`instructionEdits`, beside `takeShellNotes` in `expandAttachments`,
  framed like `nearestInstructions` by `instructionBlock`, after the same `instructionFile`
  check), unless the read log holds that mtime: the model read or wrote it itself. A rewind
  (`forgetInstructions`) lets it ride again. The main agent's reads count the pinned files as
  in the prompt.
  Subagents build theirs once per run (`systemWith`).
- **Tool schemas**: fantasy wraps `ToolInfo.Parameters` as the properties map, so tools pass
  `tools.SchemaProperties(schema)` and a non-nil `Required`; strict providers reject anything
  else.

## Testing patterns

- `internal/hostapi/host_test.go` has the canonical helpers: `newTestHost(t)` boots a fresh
  runtime + namespaces step with temp dirs; `runUser(t, r, luaSrc)` runs a chunk as user origin
  **with a 3s guard so an interpreter deadlock fails loudly instead of hanging the binary** -
  reuse it for any test that runs Lua.
- The Lua API surface is pinned by `TestNamespaceVersion`, the rendered UI by the golden files.
- Drive behavior through Lua where the Lua contract is the thing being pinned; Go-internal
  units (perm normalizer, learn intake, store) get table tests.
- Skills tests: `newTestHostSkills(t)` boots namespaces + skills steps;
  `newTestHostSkillsNoBoot(t)` lays files down WITHOUT booting - every `Boot` builds a fresh
  generation, so steps sharing state must be passed in ONE call.
- Agent-turn tests stub the model with `reflectStub` and wait with `waitIdle`. End to end,
  drive the real binary in tmux against a fake OpenAI-compatible server: pyte and friends
  garble bubbletea's incremental renderer, tmux does not. Set the `ZEMI_*_HOME` variables
  inside the pane's command: a tmux server that already runs does not pass the caller's
  environment, and zemi would open the real config.
- `internal/lua/sandbox_test.go` is adversarial by design: monkey-patching attempts must raise.

## Gotchas and non-obvious conventions

- **Not built**, whatever DESIGN §13 implies: M4 evals (no `zemi.eval`) and embeddings.
  Everything else it names ships, plus permission modes, rewind, scratchpads, the `lua` tool
  and print mode.
- **`zemi.*` namespaces are frozen and integrity-checked.** Tables are metatable-locked,
  `rawset`/`rawequal` are stripped, and Go re-verifies namespace contents per request class. A
  runtime-mutable member must go through the Go side, and each Go function exposed to Lua runs
  under `recover` (a panic becomes a catchable Lua error, logged as a zemi bug).
- **Error convention (DESIGN §6.0):** misuse (bad arguments, a call a render may not make)
  raises; runtime failure of a sync op returns `nil, err`; async ops deliver through their
  handle.
- **Sanitize at boundaries.** Model-influenced strings pass through `internal/sanitize` before
  reaching widgets, overlays or the log. Secrets are scrubbed by the learn intake scrubber at
  the memory/store boundary - don't log raw tool output or memory text.
- **Memory intake policy** (`internal/learn`): text cap 300 and quote 160, provenance required,
  `learn.Similar` refuses a near-duplicate (the user's own text only a word-for-word repeat),
  `origin` (`user` or `learned`) gates autonomy and the replacement guard, no kind.
  `memory.autonomy` is user config only.
- **Config store** (`internal/config/store.go`): dotted-path keys from a Go-owned schema; only
  user config sets them, and a read of a key outside it panics under `go test`. Providers ship
  none; user config registers them, keys included (`api_key = zemi.env.get("NAME")`;
  `zemi.env.get` reads secret-shaped names for user and builtin origin only). Config fields
  hold values, never names of env vars.
- **The Go-reserved triggers** (`ctrl+c` cancel, `/reload`) are never routed through Lua
  keymaps and cannot be rebound - `/reload` is matched on composer submit before the command
  registry, so nothing can shadow it. Keep them out of any "rebind everything" refactor.
- **No editor modes.** The composer has the keys unless a `focusable` layout node does
  (`Host.Focus`, counted only while the last frame drew it); commands are slash commands typed
  in the composer. Keymaps are keys or key sequences (`<leader>` is `keymap.leader`; a key name
  the terminal never reports raises, `checkKey`). Printable and composer editing keys stay with
  the composer - scrolling is on `pgup`/`pgdown`/`shift+up`/`shift+down`/`ctrl+home`/`ctrl+end` - and chrome, an open
  palette and completion popups keep their keys before keymaps are consulted. A focused node
  takes every key but ctrl+c through `Keymaps.Lookup(focus, key)`: `{ focus = id }` bindings
  first unless a more trusted global binding holds the key; unbound keys drop and an unbound
  esc returns the keys.
- **Mouse capture is load-bearing.** `App.View` sets `MouseModeAllMotion` (unless `ui.mouse =
  false`) so the wheel scrolls the transcript (the altscreen buffer has no native scrollback)
  and plain motion drives hover. `App.target` is the one hit test for clicks and hover, so a
  highlight always means a click acts; only an item's non-blank or background-painted cells are
  a target, and lists scroll at the edge instead of recentering, so a click does not move the
  row under the pointer. `Segment.NoHover` marks a row that acts without lighting up, which is
  how chat body rows stay quiet. Selection and clicks are zemi's own (internal/ui/mouse.go): a
  drag copies the last frame's segments, leaving out `Segment.NoCopy` cells (decoration) and
  rejoining a row that ends in a `Segment.Soft` one with the next. `hostapi.WrapSegs` sets
  both, so a wrapped prompt, reply or code block copies as the text that was written, which is
  also why `Host.Markdown` renders glamour unwrapped (tables aside) and wraps it itself. A
  click acts on `Segment.Hit` (chrome rows) or `Segment.Click`/`Segment.Widget` (a widget's
  `on_click`, run on the background worker); a click id with no widget is a Go statusline pill
  naming its command. Helpers that rebuild segments must copy those fields; only `dimRows`
  drops them, because dimmed rows are not targets. Overlays (internal/ui/overlay.go) are data:
  an item with `detail` or `actions` pushes a detail view whose rows draw as the prompts'
  buttons, keyed by `hotkey` or position (`actionKey`), and `Body` is text above the items
  (markdown with `BodyMD`; `BodySegs` draws styled rows in its place, sanitized like `Body`,
  which stays what `y` copies; a Go screen whose details open with `key: value` lines sets
  `Fields`, and `fieldRows` sets those lines as a table, prose after them, an `error:` last
  since every line after it is its text). Opening the id of an open overlay replaces it in place
  (`replaceOverlay`, keeping selection and filter; only an origin that may close it), and a
  toggle list rebuilds when the overlay stacked on it closes (`closeOverlayByID`; a detail
  view only after an action) and after a reload (`SignalBoot`), so every caller acts first
  and closes after: the worker then runs the rebuild behind the action. A row that fills the
  composer closes a toggle list, which would keep the keys. A row's `verb` names enter's action
  in the hint and keeps space from toggling it.
- **Every child process goes through `internal/proc`** (bash tool, `!command`, jobs, Lua jobs,
  MCP and LSP servers, git). A child runs in its own session: no controlling terminal (nothing
  can read the TUI's keys or hang on a password prompt) and its own process group, which
  cancel, timeout, `proc.Wait` and `proc.KillAll` on exit kill whole. A plain `exec.Command`
  here is a bug (a cancelled `sh -c` pipeline once hung the turn forever), with two exceptions:
  the user's editor (ui/editor.go, which needs the controlling terminal `Setsid` drops) and
  the sandbox backend probes.
- **The key loop never waits on Lua.** Keymap right-hand sides, Lua commands, completion
  sources and a toggle list's `Overlay.Reload` run in order on one worker goroutine
  (`App.background`); Go commands stay inline. Keymaps, clicks, one-shot timers and async
  callbacks call `refresh` when they return. The frame tick sleeps 250 ms unless something
  moves by itself (a turn, a toast, an armed flag) and always lands on a 50 ms wall boundary,
  so `ctx.frame` steps one per tick while a turn re-renders every one; a change goes through
  `App.bump`, which wakes it, and all stale widgets render as one message; a widget a message
  resized (a composer newline, a resize) renders on that message, so no frame draws lines
  rendered for another size. A render's ctx
  builds `transcript`, `agent`, `todos`, `recent` and `instructions_missing` on first read (`widgetCtxTable`,
  `BenchmarkWidgetCtx`): keep new expensive fields lazy. A widget never changes a line table
  it returned: `Widget.tableSegments` converts one it saw last render only once and
  `widgetCache.lineRows` wraps it once per width, and `Host.Session` shares the transcript
  rows, which are appended or replaced whole, never written in place. A render returning
  `true` second runs again next frame and wakes the tick, which is how `chat.lua` fills a
  long session under a 30 ms budget, latest units first; a region scrolled up keeps the last
  whole render until the fill ends (`App.scrolledUp`), so the reader's rows stay put. A turn's `turnScope` context (host.go) covers compaction, the model and tools, so
  a second esc within 2s (`App.arm`, shared with the idle ctrl+c quit) stops all of them;
  a second ctrl+c while "Cancelling" quits. A job the user waits on that is not a turn
  (`/btw`, `/plugins` network work, `/model update`, `/health`, the reflector, an MCP start) holds a `Host.Busy`
  entry: the working rule draws the newest as `ctx.busy`, and the same double esc
  (`CancelBusy`, never `zemi.agent.cancel`) cancels every entry that has a cancel func.
- **Widgets read UI flags, never Go-drawn text.** The composer frame's rules and the quit, key
  sequence and scroll flags are `tui/` widgets reading `hostapi.UIState`; a new flag goes
  into `UIState` and the ctx table, not into `statusRow` or `compose`. Animation a widget
  cannot render at the 50 ms frame clock is a segment `effect` the UI applies (`shimmer`). The terminal itself
  gets OSC 9 (notify when unfocused), OSC 9;4 (`ui.progress`: a running turn as taskbar
  progress, resent every few seconds so terminals do not hide it, cleared on quit) and OSC 11
  (the theme's Normal bg as its own background, through `tea.View.BackgroundColor` so bubbletea
  resets it on every exit, a signal included; only when it answered the query and the color
  profile shows the color). Host chrome has no canvas of its own: `hlStyle` fills it from
  Normal.bg like any group.
- **Drag and drop is a paste.** `ui/drop.go` turns a paste made only of existing paths into
  attachment tokens. Files from outside the project are copied to `<data>/drops` and referenced
  as `@drop:<name>`; `expandAttachments` resolves `drop:` only inside that directory, so a
  forged reference cannot attach other files.
- **gopher-lua multiple assignment**: `a[#a + 1], b = b, x` stores `x` in `a`. Split any
  assignment whose right side names a left-side target (this once garbled wrapped text).
- **Handles vs snapshots**: host resources (job, http, timer, session) are handle
  tables with colon methods, everything else a snapshot copy; marshaling caps string size and
  depth and refuses cycles.
- No vendoring, no generated code.
