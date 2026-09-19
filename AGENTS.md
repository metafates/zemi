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
(`Accent`, `Dim`, `Rule`, `Title`, `Selected`, `Button`, `Danger`, `Ok`). No state change may
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

A Makefile wraps the plain Go toolchain (module `github.com/metafates/zemi`, Go 1.27). No CI,
no lint config; `make check` is what a change has to pass:

```
make check              # go vet + go test + make luals + a gofmt check, all must be clean
make luals              # lua-language-server over runtime/; prints a skip line without the binary
                        # not on PATH? make check LUALS=/path/to/lua-language-server
make build / make run   # go build -o bin/zemi . / run the TUI against your real ~/.config/zemi
make test               # full suite (~20s, pure Go, no network, no CGO)
make race               # go test -race ./internal/...
make golden             # regenerate internal/ui/testdata/*.golden, then read the diff
make fmt                # gofmt -w over the tracked Go files
```

For a throwaway config tree, redirect all dirs: `ZEMI_CONFIG_HOME`, `ZEMI_DATA_HOME`,
`ZEMI_STATE_HOME`, `ZEMI_CACHE_HOME` (internal/config/dirs.go); most tests inject
`config.Dirs{...}` with `t.TempDir()` instead. Tests boot with empty dirs and an untrusted
project: call `h.SetProjectTrusted(true)` when one needs project instructions, language servers
or settled builds.

## CLI (cli.go)

`zemi` opens the TUI; a prompt argument is sent as the first message, or run as a command when
it names one (`sendPrompt`, TUI and `-p` alike, so `/review` works from a git hook). `--help`
lists the flags. Piped stdin is appended to the prompt and is a usage error without `-p`. Flags
apply after boot so they win over `init.lua`, except `--trust`, which the trust boot step
reads. `-m` and `/model` write the tier the conversation reads (`Host.ConversationKey`:
`models.smart` when config set it, else `models.main`). Both, and `zemi.model.set`, refuse
while the active profile sets its own model (`Host.ProfileModelErr`; `--profile` applies
before `-m`), since the tier would change and the conversation would not. `--profile` and
`/profile` (`Host.SetProfile`; no argument opens the picker, which the statusline's profile
segment clicks into) write `agent.profile` for the next send: a switch changes the prompt
and the tool set, so it busts the provider's prompt cache once and has no cycle key.

`--health` and `-p` boot the same step list and skip only the TUI. `-p` runs one turn, then
reports on stderr how many tool calls the gate denied - nobody can answer an ask headless, so
`deniedHint` points at `--trust` or at an allow rule. It also drops what needs a user
(`headlessExcluded`), the agent tool's `background` and `send_to` (a detached report would
outlive the run), and the prompt lines naming them. A headless run with no saved trust decision
runs untrusted and says so.

A first run writes a fully commented starter `init.lua` (`config.Dirs.Ensure`) because zemi
ships no providers and an empty config cannot answer a prompt. Config reload is not fsnotify:
`watchConfig` walks the config trees once a second and reloads when a fingerprint moved and no
turn is active; an untrusted project's `.zemi` is not watched. A replay that writes a key the
same way as the last boot is skipped (`cfgReplayed`) and a key the new config writes fewer
times is settled to its last value (`cfgSettle`), so `/model`, `/effort`, `/profile`, the mode
cycle and the flags survive a reload until the config changes that key. A key the config stopped
writing returns to its schema default, unless a runtime pick moved it since.

## Architecture and control flow

`main.go` is wiring only. Boot order (each step is a `lua.BootStep`, see `steps` in main.go),
and the order matters: host namespaces (`zemi.*`) -> builtin commands ->
`require("zemi.defaultui")` (shipped theme, widgets and keymaps, one module each) -> project
trust (`trustGate.step`, before any config that can start an MCP server) -> plugins -> user
`init.lua` -> authored skill hooks -> a trusted project's `.zemi/init.lua` -> markdown commands
(`commands/*.md`: user, then a trusted repo's `.zemi`, `.agents`, `.claude`) -> `app.ready` ->
learned skill hooks (skills load last, lowest trust). Boot runs on its own goroutine and may
block on the trust prompt.

**Project trust** (main.go `trustGate`, DESIGN §5): one decision per repository, keyed by
`Dirs.TrustRoot()` (the symlink-resolved repo root, searched no higher than below `$HOME` so a
dotfiles repo is nobody's root; a linked worktree maps to its main checkout by reading the
`.git` file, never by running git) and saved in `<state>/trust.json`. Order: a decision held
for this process (`--trust`, an esc answer, `/trust` where saving is impossible), the saved
one, then the Go-owned prompt. `$HOME` and `/` are never asked about or saved
(`hostapi.TrustUnsaved`). `/trust` reopens the prompt off the UI goroutine, saves, calls
`Host.SetProjectTrusted` and reloads when the decision changed (a reload that fails before the
trust step must not leave the old decision in force). `Host.ProjectTrusted()` gates project
`.zemi` (init.lua, agents, commands, workflows), instruction files and profile `context.files`,
project skills, every language server, the Lua tool's ask, the read jail reaching past the
launch dir to `Dirs.RepoRoot()`, and what auto and plan mode settle. `Host.git` shells out to
the `git` binary (no git library in go.mod) with `GIT_CONFIG_NOSYSTEM=1`, and switches off
fsmonitor, hooks and repo-local filter drivers in every repository, trusted or not: a sandboxed
command can plant a nested repo whose config zemi would run.

### The concurrency rules (DESIGN §4) - violate these and things deadlock or corrupt

1. **Only the Lua actor goroutine touches the `lua.LState`.** Everything else goes through the
   queue in `internal/lua` with priority classes `ui > agent > io`. `ctx` cancellation preempts
   running Lua at the next opcode (catchable once, then re-raises).
2. **Watchdog / hot mode.** A wedged request trips the watchdog; after a grace period the
   runtime goes "hot" - every Lua request fails fast until force-reload builds a fresh `LState`
   (generation). All registrations (keymaps, widgets, events, timers, jobs, Lua tools, a
   permission mode's `decide`) are generation-tagged (`hostapi.Gen`) and purged before replay,
   so never keep a pointer into an old generation. `ForceReload` keeps the old Lua permission
   rules (`staleRules`) until the replay succeeds, and `tool.pre` and `permission.request`
   vetoes wait out a reload, so a turn is never checked against half a config.
3. **UI single-writer.** All `ui.App` state mutates on the bubbletea Update goroutine; work
   from other goroutines is funneled through a hook channel (internal/ui/hooks.go). hostapi
   never imports internal/ui - it defines `UIHooks` and the App implements it; keep that
   direction. Corollary: never call `program.Send`/`program.Quit` from inside `Update` (or
   anything it calls) - bubbletea v2's `p.msgs` channel is unbuffered, so the loop would wait
   on itself (the `/quit` hang). Wrap such calls in a goroutine, like `App.Quit` does. Blocking
   hooks (`AskPermission`, `AskTrust`, `AskQuestion`, `ReviewMemories`) must only run on non-UI
   goroutines.
4. **UI-critical Lua handlers (keymaps, widget render, event handlers) must be fast** (~10 ms).
   Long work belongs in `zemi.job` / `zemi.http` / `zemi.timer`.
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
   EXISTS` + FTS5 triggers run on open - add tables there; a new column on an existing table
   also needs an ALTER in `migrations`, since `IF NOT EXISTS` skips it on old databases.

### Tool pipeline (internal/tools/tools.go)

Every model tool call runs: JSON unmarshal -> schema validation -> `tool.pre` Lua events (deny
/ halt / rewrite) -> permission gate -> run -> `tool.post` notification. Invariants: a
rewritten input **re-enters at the permission gate** (per-run pipeline copy with
`RewriteLoosened`, so a non-user rewrite can't ride a pre-existing allow glob); an ask with no
overlay available fails closed; a denied result says who decided and why; errors become
tool-error responses, not panics. Host-implemented tools (`agent`, `workflow`, `window`, `Lua`,
MCP and Lua tools) call the gate themselves and run the events through `preCheck` (which
refuses a rewrite with an error result), `postNotify` and `Pipeline.CheckDenied`. A command a
permission mode settled runs sandboxed (per-run `Pipeline.Sandboxed`).

Bash is special: `perm.Store.CheckCommand` normalizes the command first (compound splitting,
redirect and env-assignment gates - internal/perm/bash.go, heavily table-tested; extend those
tables when touching it). An assignment prefix the normalizer strips downgrades an allow
verdict to ask, since the rules never see it and `GIT_EXTERNAL_DIFF=./x git diff` runs `./x`.

File tools (internal/tools/fs_tools.go): `write`, `edit` and `multiedit` refuse a file the
model has not read, or one whose mtime moved since (`Pipeline.Seen`, per session; subagents get
their own), and hold a per-path lock, so a concurrent agent or the user's editor cannot lose
changes. `ResumeSession` rebuilds that map from the stored rows (`seedReads`), leaving out
files touched after the last message. `tool.Run` runs the same check before the gate
(`tool.edits`), so nobody approves a change the tool would then refuse, and resolves the path
(`perm.RealPath`) before the gate; the tools open exactly that path through `openGated` (an
`openat` walk with `O_NOFOLLOW`), so a sandboxed command cannot swap a directory for a symlink
between check and open. The three writers refuse a path whose last component is a symbolic link
and name the target to edit instead. `glob` and `grep` are gated on their root only, so neither
shows a hit a deny or ask `fs.read` rule covers, and grep hides secret-shaped files. Output too
long for a result is cut head and tail and saved whole to the scratchpad (`tools.Spill`). A
bash command still running at its timeout is handed to `Jobs.Adopt` instead of being killed
(which is why it is not bound to the turn's context; an interrupt still kills it). A bash row
shows what the command changed (hostapi/snapshot.go, `Pipeline.Changes`): a git tree of the
repository before and after, written through a warm temp index so the user's index is never
touched, diffed into the result's `Metadata`; the model gets one line of paths. `read` carries
the nearest instruction file not already in the prompt, once per session, so a monorepo
package's own AGENTS.md reaches the model.

### Permission engine (internal/perm)

Rules `(subject, pattern, decision, not_match?, principal?)`, subjects like `tool.bash`,
`fs.read`, `fs.write`, `net.<host>`, `lua.<ns.call>`. Ordering: deny > ask > allow; within one
decision, fewest wildcards then longest literal prefix wins. A `not_match` hit falls through
and **forces ask**. The default when nothing matches is `Store.Default` (host wires
`fsDefault`: jail reads allow except secret-shaped paths, writes ask) and finally ask - there
is no fail-open path. New host-API side effects need the same `lua.` subject.

`permissions.mode` (internal/hostapi/permmode.go) only answers asks that survive the rules and
the `permission.request` veto, and it is deterministic: no model screens anything. `yolo`
allows. `auto` settles what `autoLocal` covers - reads and edits **inside the jail** that are
not secret-shaped, `tool.workflow` scripts (each agent they start is gated), bash commands
(`unsettled`), input to a window that already runs sandboxed, and MCP tools a server annotates
read-only - and hands the rest to the user with a reason naming why (`askHook`; writes under
`.git`, `.zemi` or zemi's own dirs are `protectedWrite`, checkouts in `.zemi/worktrees` aside).
`plan` denies writes and runs commands like auto, sandboxed with the project unwritable.
Network requests are never settled: the URL is the model's. A mode from config may carry
`decide`, a Lua function answering one ask ("allow", "deny", anything else sends it to the user
whatever the base would settle, which is how a user gets every ask); it holds the interpreter
with a 60s ceiling, long enough for a model call of its own, and cannot allow an `Unsandboxed`
or `Network` request. In a trusted project with a live sandbox `unsettled` parses nothing:
every command runs confined, except the short `perm.Destructive` list (forced or recursive
`rm`, `git clean`, `find -delete`, `shred`, `truncate`, `dd`, `rsync --delete`, also behind
`xargs`, `find -exec`, `sh -c`, `eval`) and, on a backend that cannot hide files, one naming an
existing secret-shaped file. Known ceiling: the sandbox policy is the only limit, so the model
can write code and run it unattended, rewrite any project file and read what the policy does
not hide, outside the project too. In an untrusted project, and in a trusted one with no
sandbox backend, both modes settle only `ReadCommand` commands (every program only reads, named
without a path; no builds, no git) and auto settles no writes.

**Sandbox** (internal/sandbox, internal/hostapi/sandbox.go, DESIGN §9): a `tool.bash` verdict a
mode gave (`DecidedBy` `mode:<name>`, base not yolo) makes `Pipeline.SandboxFor` return
`h.sandboxPolicy(plan)`; `tool.Run` puts it on the per-run pipeline copy, and `runBash`,
`Jobs.Start`/`Adopt` and `win.Spec.Wrap` call `sandbox.Wrap` before starting. A wrap error is a
tool error, never an unconfined run. Rule-allowed and user-approved runs, `!command`,
`zemi.job`, MCP servers and `Host.git` stay unconfined; language servers run under
`serverPolicy`. `NewHost` probes the backend synchronously, so `--health` and the first turn's
schema see it.

`sandboxPolicy` writes the policy; the whys behind it: the sandbox temp dir is never the shared
system one (macOS keeps the `xcrun_db` there that picks what `/usr/bin/make` and `git` run),
`GOCACHE` is one per trust root (Go trusts cache entries, so a shared one lets one project
poison another's build), the project is writable only when it is trusted, outside zemi's own
dirs and on a backend that can protect paths inside it (`Backend.EnforcesProtect`), `.git`,
`.zemi` and every `PATH` dir under a write root stay read-only because zemi runs git, sh and
servers unconfined by name, and home credential paths are hidden. The exception is the repo's
`.zemi/worktrees` (`Dirs.WorktreesDir`), where the agent keeps git worktrees: a write root of
its own, listed in `.zemi/.gitignore` (`Dirs.IgnoreLocal`, appended, never rewritten), with
each checkout's own `.git` and `.zemi` protected and nothing loaded from it. The namespace
backend binds only protected paths that exist, so `Pipeline.Guard` records the missing ones
before a run and checks them at exit: a created `.git` or `.zemi` holds trust off until
`/trust`. Bash, `job_start` and `window` take two escapes no mode settles and each of which
asks: `network: true` only adds sockets, `sandbox: false` drops the whole policy. A confined
failure `sandbox.LikelyDenied` recognizes gets `Pipeline.sandboxHint`, which names the writable
dirs and offers `network: true` first.

`ReadCommand` is an allowlist of readers (internal/perm/safe.go, table-tested), which settle
unconfined: no shell expansion outside quotes (`$` and backticks inside double quotes count),
no write redirects (`2>/dev/null` and `2>&1` are none), no program named by a path, and every
argument must pass `safeCommandPath` (inside the project and not secret-shaped - `h.secretPath`
matches the resolved path too, so a symlink cannot hide one; a plain read may also reach the
scratchpad and zemi's config tree, but `cd` may not enter them). Readers keep their
`refusedFlags` and script checks, and `perm.ArgUse` says how an argument is used: a search
pattern is no path, and a recursive search settles only over a tree with no secret-shaped file
in it. Nothing may point into `.git`, since `.git/config` may hold tokens.

Every rule lives in memory: `zemi.permission.rule.add` in config adds it again on each boot,
and an "always" answer lasts until zemi exits. There is no rule file and zemi makes no git
commits (`scope = "permanent"` raises and points at init.lua). Only `user` origin may add allow
rules (project may tighten only) or switch the mode (`zemi.permission.mode.set`, which the
shipped ctrl+y yolo toggle uses); the gate lives at the namespace closure, same
off-interpreter-`OriginHost` reasoning as skills.

The overlay's "always" answer writes session allow rules scoped to the `model` principal, as
prefix globs rather than the exact request (`alwaysChoice`, ask.go: a command's two-word
prefix, a file's directory, a URL's host). A file read is also offered the wider `repoChoice`
("allow reads anywhere in <repo>", answered as `user:wider`), because in a deep tree the
directory rule is the same ask again one package over. No always option is offered at all for a
gate-forced ask (heredoc, env assignment), a pattern holding a glob wildcard, an opaque segment
(an inner `sh -c`, a stripped assignment) or a wrapper program that runs what it is handed
(`wrapperPrograms`: shells, interpreters, `sudo`, `xargs`, ...) - no rule written from such a
segment says what the next call would run - nor for a `protectedWrite`, which `Pipeline.Check`
asks about whatever rule allows it. A directory glob cannot exclude the secret shapes
either, so `Pipeline.Check`'s loosen sends a file request a `model` rule allowed back to the
ask when `Secret` matches; a user-config rule has no principal, stays authoritative, and shows
read-only in `/permissions`, while a `model` rule gets a Remove action.

### require and the origin model (internal/lua/runtime.go, origin.go, registries)

Modules are cached per generation by resolved file and trust class: a requirer below the file's
origin gets its own instance run under its own origin (builtin modules are shared instead,
through a deep read-only view), so project, plugin or learned code cannot plant functions into
a table that higher-origin callbacks call. Dotted names search `<root>/lua` over the
runtimepath: user, plugins, and the project's `.zemi` first but only when the requirer is
project code. `./x` and `../x` resolve against the calling function's chunk name, which is why
file chunks are compiled with their absolute path, and must stay in that file's origin tree;
`load`/`loadstring` tag their chunks with the running origin and never keep an absolute name,
so neither can be forged.

Every chunk carries an origin from its resolved path class: `user` (~/.config/zemi, plus each
subdirectory of it in `Dirs.UserTrees`, which the read jail registers too, so a symlinked
`skills` or `lua` still resolves inside) > `builtin` (embedded runtime/) > `plugin` (site/pack)
> `project` (<project>/.zemi); `learned` code is denylisted from dangerous APIs. Registries
enforce: cross-origin collision is **refused** with the higher-trust origin winning; within one
origin it is last-writer-wins. Project origin additionally cannot register providers or builtin
tool names, and its `tool.pre` rewrites are ignored (logged). No origin can emit host-owned
events. A keymap that is a prefix or an extension of a more trusted key is refused,
`zemi.command.run` refuses a more trusted origin's Lua command (its body runs under that
origin), windows and overlays can be closed only by user, builtin or the origin that opened
them, and `app.quit` denials from project or learned handlers are ignored. When adding a
registry, follow `internal/hostapi/registry.go`.

### Skills (internal/skills, internal/hostapi/skills.go)

Skills are file-based units, each a `SKILL.md` with YAML frontmatter: authored under
`<config>/skills/`, builtin (zemi's self-knowledge, `runtime/skills/zemi-*`, embedded and
rewritten to `<data>/builtin-skills/` on every sync, enabled by default), learned (reflector
drafts, body only, no Lua hook, scrubbed, start disabled) under `<data>/skills/`. A trusted
project adds its own (`.zemi/skills`, `.agents/skills`, `.claude/skills`): body only, enabled,
never in the table or `skillsReg`, scanned by `SetProjectTrusted`, so a skill a command writes
mid-session waits for the next boot or `/trust`. Recipes (`skills.Recipe`) are the project's
private skills under `<state>/projects/<hash>/skills` (`recipesDir`, a read jail root no sandbox
policy can write): trusted only, body only, enabled, and they lose every name clash. The prompt
lists the snapshot `environmentPrompt` pins with `ag.env` (new session, resume, `compactDone`);
`skillIndex` discovers the dir live, so a recipe written mid-session loads without moving the
prompt, and `runSkill` marks the file read so an `edit` can follow. The agent writes recipes with
the plain file tools (the environment prompt's `Recipes:` line says how); the dir is a
`protectedWrite`: no rule and neither auto nor plan settles a write (yolo still allows), and the
ask says when `sessionLocalOnly` fails.
`recipeAfterWrite` (the `AfterWrite` hook, run before the writers' `markRead` because it may
rewrite the file) scrubs every file written there in place (`learn.ScrubShapes`: no entropy
pass, which cuts the long paths and `NAME=url` a procedure is made of), names the redacted
lines and tells the model what will not load. `/skills` and its commands read the dir live.
`/skills delete <name>` (`deleteRecipe`, the Delete action only recipe rows have) removes one
and pins the list again. Invariants:

- **Quarantine until enabled.** A skill is inert until `zemi.skills.enable` runs, and only user
  or builtin origin may enable/disable (`refuseBelowUser` gate at the Lua namespace boundary -
  NOT inside the Go helper; off-interpreter goroutines read `OriginHost`, which would defeat a
  Go-side gate). Enabling a learned skill runs its `zemi-skill.lua`: inline when
  `zemi.skills.enable` calls it (already on the interpreter goroutine), otherwise enqueued via
  `h.RT.Do` - the inline-vs-Do split in `enableHook` is load-bearing.
- **Token efficiency**: the prompt carries only the index (name + description, cut to 512 runes
  with a warning); bodies load on demand via the `skill` tool, after a permission check.
- **Hook placement**: authored hooks register after user init.lua, learned ones after
  `app.ready` - the learned step must not re-run on replay without its `Gen` tag.

### Type annotations (internal/hostapi/types.go, runtime/types/zemi.lua)

`runtime/types/zemi.lua` is the LuaLS (`---@meta`) description of the whole `zemi.*` API, never
loaded by the interpreter. `syncTypes` rewrites it plus a copy of `runtime/lua/` to
`<data>/types/` on every start, and `<config>/.luarc.json` whenever its content moved (an
unchanged file is left alone: the config watcher reloads on an mtime). `TestTypes*` keep it
honest: every API member annotated, nothing annotated that zemi dropped, and the
`zemi.EventName`, `zemi.HostAction` and `zemi.WidgetCtx` aliases pinned to their Go sources. It
also carries `---@deprecated` stubs for the globals the sandbox strips, since the `.luarc.json`
files turn off LuaLS's own libraries and it flags a deprecated global only when every
definition is deprecated. Known ceiling: with no CI, `make luals` runs only where
lua-language-server is installed.

### Turns, sessions and the learning loop (internal/hostapi: agent.go, session.go, compact.go, memory.go, reflector.go)

- **Models are tiers.** `models.main` runs everything; `models.small` (session titles,
  compaction summaries, the builtin `web` agent) and `models.smart` (the main conversation)
  fall back to it. Anything that takes a model - an agent definition, the `agent` tool, a
  workflow - takes a tier name or a `provider/model` id (`modelRef`).
- **One ledger.** Every model call is billed through `Host.bill`: the session row, and the
  live tally when that session is still open. Calls outside a runner (summary, title,
  reflector) go through `Host.generate`, which bills them; the reflector bills the session it
  read, closed or not.
- **The active profile is a config key.** `agent.profile` names what `resolveProfile("")`
  runs (`default` is the shipped shape), so only user config writes it and a reload keeps a
  runtime pick like any key. `PurgeGeneration` clears the definitions and `cfgSettle` returns
  a key config stopped writing to `default`, so deleting a profile and its `config.set` line
  runs the shipped one; a name nothing defines (a typo, a pick of a since deleted profile)
  fails the send. `defaultui/init.lua` ships one, `lean`: a `-name` denylist of the heavy,
  rarely called schemas, so a new tool is in it by default and user config redefines it by
  name.
- **One runner per send, one conversation.** `ensureRunner` rebuilds the runner (model, prompt,
  tools may change); `AgentSend` copies `History` from the previous idle runner. `runTurns`
  loops the queue's follow-ups on one runner through `sendCompacting` (compact.go): compaction
  runs before a send, when `Runner.Full` cuts a turn at a step boundary, and once after a
  context-too-long refusal. The size is `agent.ContextUsed` of the last step, never a sum; an
  unknown window means no auto compaction. The summary row is followed by the verbatim tail
  stored with row role `tail`, which resume reads and transcript readers skip. An unfinished
  todo list goes after `</summary>`, where `summaryOf` drops it, so a stale one is never
  merged.
- **A turn's work survives the turn.** `Runner.Persist` writes each step's messages from
  `OnStepFinish`, so a cancelled or crashed turn keeps what it did; the next send runs
  `closeToolCalls` over the history first, inserting an error result after any tool call left
  unanswered (providers refuse a dangling call). Every resolved model is wrapped in `idleModel`
  (providers.go): a call that makes no progress for 5 minutes is cancelled, since the HTTP
  header timeout stops counting once the status line arrives and a stalled stream would hold
  the turn forever; the timer resets on each stream part and tools run between model calls, so
  a slow build is never covered.
- **Steering.** Messages sent mid-turn queue; `Runner.Steer` injects them at the next step
  boundary through fantasy's `PrepareStep`, which rebuilds step input from scratch, so steered
  messages are re-inserted every step. Leftovers become one follow-up turn, and `active` stays
  held across that handoff (`nextTurn`) so a submit cannot jump the queue; `/compact` and
  rewind claim `active` and a `turnScope` together for the same reason.
- **Store order.** `Messages` orders by `rowid`: a turn persists several rows in one
  millisecond and ids are random. Tool telemetry rows share the table; `historyFrom` skips
  anything that does not decode as a fantasy message, and reads a file tool's or a bash call's
  diff back from the telemetry row that carries it (computed against the files as they were, so
  a resume could not rebuild it).
- **Proposals persist** in the scope DB until `/memory` or `zemi.learn.decide`; `e` opens the
  raw text through `EditFile` after the other marks apply, re-running intake as user origin and
  keeping the id.
- **Reflector timing.** It runs on session close (`/new`, `/resume`) and at startup for the
  latest session (once per process, not on a reload), never on quit; `reflectDue` compares the
  message count with kv `reflector.seen.<sid>`, so a session is reflected again only after new
  messages. Its prompt carries the instruction files (`instructionSections`, shared with
  `systemWith`), a memory index and a skills index that includes the project's skills and
  recipes, so it drafts no global skill for a procedure the repository has; an entry with
  empty text and `supersedes` queues a forget proposal, never auto-applied. It proposes
  memories and skills only, always for review (DESIGN §10.4): there is no config-patch
  pipeline and no `/config` command, the agent edits config files directly.
  `digestFor(query)` ranks by the FTS score against the session's first message (bm25 stays
  near zero for common words, so they lift nothing), user origin x2 and recency over 30 days.
- **Provider text shape.** Model responses carry `fantasy.TextContent`, stored messages
  `fantasy.TextPart`; `concatText` handles both. A stub that returns the wrong one hides real
  provider bugs (this once made the reflector read empty output everywhere).
- **Jail.** `inJail` resolves symlinks on both the path and the roots; `fsDefault`,
  `resolveJailed` and `@file` attachments all rely on it.

### Windows (internal/win, internal/hostapi/win.go, internal/ui/win.go)

A window is a program on a pseudo-terminal (creack/pty, no cgo) feeding a pure Go emulator
(charmbracelet/x/vt), or text (`Manager.OpenText`): no pty, no emulator, the UI renders and
wraps the body itself, so a text window never exits and refuses program input. `win.Manager` on
the host owns windows; they outlive turns and reloads (only Lua `on_exit` callbacks are
generation-tagged). Each carries a `Layout` the UI reads per frame: splits and vsplits go where
the layout tree has a `windows` slot of their kind, floats over everything in list order
(`Manager.Raise` puts a focused or clicked one on top), and overlays, popups and prompts draw
above every window. Hidden windows get a label on the dock row (`dockRow`) that shows them
again.

`Screen()` turns cells into `sgr:` segments so colors reach the renderer untouched. Keys go the
xterm way, and as CSI u while the program keeps kitty keyboard flag 1 on for the screen it is
on (a bool per screen like zellij, no flag stack), so nvim tells esc, ctrl+i and shift+enter
apart. OSC 52 writes reach the host clipboard only from a window whose `Spec.Clipboard` is set:
the editor and `zemi.win.open` from user or builtin code outside the Lua tool, never a window
the model drives, which could plant a command the user pastes. Input queues and a `writeLoop`
goroutine feeds the pty, because the emulator writes under the screen lock and a program that
does not read stdin must not wedge it. A window's env drops the host terminal's identity, or
image tools emit protocols x/vt drops. Images (kitty.go) are kitty Unicode placeholders, on
ghostty and kitty only: the APC handler takes direct base64 transmissions and never `t=f/t/s`,
which the unconfined host would read from disk; the cells are plain text, so clipping and
scrolling need nothing image-specific and every text reader sees blanks, while
`sanitize.String` blanks them everywhere else, so model text cannot draw an image in chrome.

A focused window takes every key, ctrl+c included, until `ui.window.unfocus`. A press on a
title, separator or float border starts a `winDrag`; a release without motion focuses,
minimizes to the dock, or closes on ×. A left drag over an **unfocused** window's cells is
zemi's own selection instead, so the user can copy what a window printed; the press reaches the
program only when the release comes without a drag. `UIHooks.Screenshot` hands back the last
composed frame stripped of styling, so `zemi.ui.screenshot()` works from the interpreter
goroutine without either side waiting; it is how the agent debugs its own widgets and layout.
Editor windows come out blank.

The model's `window` tool gates `open` as `tool.bash` (same rules and modes, and a mode-settled
open runs sandboxed through `win.Spec.Wrap`) and `keys`/`mouse`/`send` as `tool.window`,
`Unsandboxed` (so no mode settles them) unless the window runs sandboxed. `focus` asks always
unsandboxed and only for a shown window: it hands the user's own keyboard over.
`text`/`set_text` ask `tool.window.text` with the body as preview, which every mode settles: a
text window runs nothing and reads nothing. Subagents' asks carry the agent name and their
windows open hidden; the tool sees only windows it may act on (`windowTool.mine`), never the
user's, Lua's, or the editor windows `App.EditFile` (ui/editor.go) opens for the user's own
editor with `os.Environ()`.

### Subagents, workflows, agent runs, the Lua tool, MCP, LSP

- **Subagents** (subagent.go) are fresh `agent.Runner`s over `PipelineFor()` (same permission
  gate; asks are prefixed with the agent name). Definitions: builtin `explore`, `web` (on the
  small tier) and `general`, plus `agents/*.md` (user over builtin over a trusted repo's
  `.zemi`, `.agents`, `.claude`). They never get `agent`, `workflow`, `todos`, `question`,
  `plan`, `open_in_editor` or `Lua` (nesting depth 1). A run is a child session (`parent_id`);
  `ListSessions` hides children. They have no summarizer: `sendPruning` clears their own old
  tool output when the window fills and, with none left to clear, drops the middle of the
  history once, keeping the task and a recent tail. The delegation guidance lives in
  `delegationPrompt`: without it models rarely delegate.
- **Workflows** (workflow.go) are Lua scripts (model-written via the `workflow` tool, saved
  under `workflows/`, builtin in `runtime/workflows/`). They run in their **own** gopher-lua
  state on a host goroutine, never on the interpreter (DESIGN §4): a coroutine per branch,
  `agent()`/`parallel()` yield and the scheduler resumes them as subagents finish. Invariants:
  the script sees only base/string/table/math + the workflow API (no fs, net, `zemi.*`);
  `agent`/`parallel` are Lua wrappers because gopher-lua loses resume values on a tail call of
  a yielding Go function, and both refuse under `pcall`, where a yield silently kills the
  coroutine. The tool's description points at the zemi-customize skill for the API, which every
  request would otherwise pay for.
- **Agent runs** (tasks.go): every `runSubagent` (tool, background, workflow, Lua) registers an
  `agentRun` whose hooks build a live transcript, which `/agents view <id>` draws instead of
  the session's, and a submit while it runs goes to it through `MessageAgent`. A run's queue is
  its `Runner.Steer`; what arrives after its last step boundary becomes one more `Send`
  (`followUp`), which closes the queue in the same lock when there is nothing left, so a
  message is never accepted and then dropped. Background runs report through `AgentSend` behind
  `attachMarker`, which the live transcript and resume cut off.
- **Scratchpad** (scratch.go): `<state>/projects/<hash>/scratch/<session id>`, pads idle a week
  removed. `fsDefault` allows `fs.write` there and `Pipeline.Scratch` lets it pass plan mode; a
  program may read it, never run from it.
- **Lua tool** (luatool.go, tool name `Lua`): model Lua runs through `Runtime.Eval` as `user`
  origin, default-deny: without approval it may call only `luaReadOnly` members. Every Go
  function in a namespace passes the sandbox guard (`Sandbox.SetGuard` -> `guardModelLua`),
  which raises for tool code (frame path `lua-tool*`) unless the innermost tool code is an
  approved run. `luaUnlisted` tokenizes the script first: any zemi path off the list, a
  non-read handle method call, or dynamic access (`_G`, `load`, `require`, ...) asks `tool.lua`
  once, as `Request.Unsandboxed` so no mode settles it, and the approved run evaluates as chunk
  `lua-tool#<n>`. In an untrusted project or plan mode every script asks. A new API member is
  guarded until added to `luaReadOnly`; `TestLuaToolDefaultDeny` checks every member. Not a new
  origin: the gate is a flag on the chunk name. Runs on the interpreter (10s ceiling), main
  agent only.
- **Model memory edits**: `memory_update`/`memory_forget` go through `modelEditMemory`, which
  re-runs intake and asks the user (`memory.write` as `Unsandboxed`, so no mode settles it)
  unless `memoryAuto` holds: autonomy auto, a trusted project, the memory learned, the text not
  instruction-shaped, and no `fetch`, `web_search`, `Lua` or non-builtin tool call in the
  session or its subagents' (`subagentHooks` records those, `ForkSession` copies them).
- **MCP** (mcp.go, internal/mcp): a dependency-free JSON-RPC client (stdio, streamable HTTP
  with JSON or SSE replies, OAuth discovery where a server needs it; `client_id` and
  `callback_port` for one without dynamic registration). Servers come from `zemi.mcp.add` (user
  origin only); they stop on `PurgeGeneration` and run unconfined in `DataHome` with
  `ZEMI_PROJECT_DIR` set, trusted project or not: `npx` and `uv` read config from the working
  directory, and most servers need the network a sandbox refuses. Tools are
  `mcp_<server>_<tool>`, gated as subject `mcp.<server>` with the tool name as argument. Test
  against `internal/mcp/mcptest`.
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
drawing never waits on Lua. The shipped `defaultui/layout.lua` sets the default, user config
may replace it, lower origins are refused. A reload keeps the old layout drawn (the spec is
plain data, safe across generations); `ForceReload` drops one the replay did not set again and
`DefaultLayout`, the Go copy of the shipped tree, draws. `span` sizes one box per frame from
cells, percent, content and weighted fill, takes cells back from fill first when they do not
fit, and never fails (`FuzzSpan`). The stage node is where prompts, overlays, toasts and
centered window floats go. A content-sized node's height is the most rows it could get, never
its last draw, so a widget that pads to `ctx.height` can still shrink. Hit testing reads the
painted segments, never the tree. `/health` has a `layout` row naming widgets in regions the
layout does not draw. A `composer` node with `frame` draws the square frame around the composer
and puts the first line of the widgets in its `top`, `left` and `right` regions into the rules
(`composerFrame`, ui/composer.go; `top` gets the whole rule, which the snail in
`defaultui/snail.lua` moves along, keeping its place in module locals), which is where the
shipped layout keeps the mascot and the statusline: agent state changes what a rule says, never
how many rows the frame takes, so nothing above it reflows.

### Rewind, plan mode, prompts, composer bridge

- **Rewind** (rewind.go, store/checkpoints.go): the pipeline's `Checkpoint` hook saves a file's
  pre-turn content (keyed by the turn's user message, first write wins) before a write tool
  touches it. `RewindTo` restores files first and only then truncates messages, so a failed
  restore leaves the conversation intact. Bash changes are not tracked.
- **Plan mode** is enforced in `tools.Pipeline.Check` (`Plan` hook): `fs.write` denied even
  under allow rules, `mcp.*` allows and bash allows other than `ReadCommand` flip to ask.
- **Go-owned prompts** (permission, trust, question) share one renderer in ui/agent.go
  (`promptBox`, `choiceRows`); Lua reads the composer through a snapshot the UI publishes and
  edits it through `UIHooks.ComposerEdit`, applied on the UI goroutine.
- **System prompt** (prompt.go): `basePrompt` + `delegationPrompt` for the default profile,
  then `zemiPrompt`, plan mode, the per-session pinned `environmentPrompt`, instruction files
  under `# Project instructions` (the repository's files share a 32 KB budget spent deepest
  file first, so a huge root `AGENTS.md` cannot squeeze out the package's own; the user's
  `<config>/AGENTS.md` gets its own budget), Lua sections, skills, digest. It is built from the
  run's tool set (`hasTool`), main agent and subagents alike: no `agent` tool drops
  `delegationPrompt`, no `skill` tool drops the skills index and the zemi-* sentence, no `read`
  or `todos` drops its "Using tools" line, and plan mode without the `plan` tool (headless, a
  profile) ends in `planHeadless`. No kept tool's description names a tool the shipped
  `lean` profile drops (`TestPromptFollowsToolSet`). Anything that varies per turn must be
  pinned per session or it busts the provider's prompt cache - which is why `gitStatusSection`
  runs once per process, warmed at boot so a big repository's `git status` is not paid for at
  first token.
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
  garble bubbletea's incremental renderer, tmux does not.
- `internal/lua/sandbox_test.go` is adversarial by design: monkey-patching attempts must raise.

## Gotchas and non-obvious conventions

- **Not built**, whatever DESIGN §13 implies: M4 evals (no `zemi.eval`), the plugin manager,
  embeddings. Everything else it names ships, plus windows, permission modes, rewind,
  scratchpads, the `Lua` tool and print mode.
- **`zemi.*` namespaces are frozen and integrity-checked.** Tables are metatable-locked,
  `rawset`/`rawequal` are stripped, and Go re-verifies namespace contents per request class. A
  runtime-mutable member must go through the Go side, and each Go function exposed to Lua runs
  under `recover` (a panic becomes a catchable Lua error, logged as a zemi bug).
- **Error convention (DESIGN §6.0):** misuse (bad arguments, masked mutator) raises; runtime
  failure of a sync op returns `nil, err`; async ops deliver through their handle.
- **Sanitize at boundaries.** Model-influenced strings pass through `internal/sanitize` before
  reaching widgets, overlays or the log. Secrets are scrubbed by the learn intake scrubber at
  the memory/store boundary - don't log raw tool output or memory text.
- **Memory intake policy** (`internal/learn`): text cap 500, fixed kind enum, provenance
  required, `origin` (`user` or `learned`) gates autonomy, supersede and digest labeling.
  `memory.autonomy` is user config only.
- **Config store** (`internal/config/store.go`): dotted-path keys from a Go-owned schema; only
  user config sets them. Providers ship none; user config registers them, keys included
  (`api_key = zemi.env.get("NAME")`; `zemi.env.get` reads secret-shaped names for user and
  builtin origin only). Config fields hold values, never names of env vars.
- **The Go-reserved triggers** (`ctrl+c` cancel, `/reload`) are never routed through Lua
  keymaps and cannot be rebound - `/reload` is matched on composer submit before the command
  registry, so nothing can shadow it. Keep them out of any "rebind everything" refactor.
- **No editor modes.** The composer has the keys unless a `focusable` layout node does
  (`Host.Focus`, counted only while the last frame drew it); commands are slash commands typed
  in the composer. Keymaps are keys or key sequences (`<leader>` is `keymap.leader`; a key name
  the terminal never reports raises, `checkKey`). Printable and composer editing keys stay with
  the composer - scrolling is on `pgup`/`pgdown`/`ctrl+home`/`ctrl+end` - and chrome, an open
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
  which stays what `y` copies).
- **Every child process goes through `internal/proc`** (bash tool, `!command`, jobs, Lua jobs,
  MCP and LSP servers, git). A child runs in its own session: no controlling terminal (nothing
  can read the TUI's keys or hang on a password prompt) and its own process group, which
  cancel, timeout, `proc.Wait` and `proc.KillAll` on exit kill whole. A plain `exec.Command`
  here is a bug: a cancelled `sh -c` pipeline once hung the turn forever.
- **The key loop never waits on Lua.** Keymap right-hand sides, Lua commands, completion
  sources and a toggle list's `Overlay.Reload` run in order on one worker goroutine
  (`App.background`); Go commands stay inline. Keymaps, clicks, one-shot timers and async
  callbacks call `refresh` when they return. A render's ctx builds `transcript`, `agent`,
  `todos` and `recent` on first read (`widgetCtxTable`, `BenchmarkWidgetCtx`): keep new
  expensive fields lazy. A turn's `turnScope` context (host.go) covers compaction, the model
  and tools, so esc stops all of them; a second ctrl+c while "Cancelling" quits.
- **Widgets read UI flags, never Go-drawn text.** The composer frame's rules and the quit, key
  sequence and scroll flags are `defaultui/` widgets reading `hostapi.UIState`; a new flag goes
  into `UIState` and the ctx table, not into `statusRow` or `compose`. Animation a widget
  cannot render at 30 fps is a segment `effect` the UI applies (`shimmer`). The terminal itself
  gets OSC 9 (notify when unfocused) and OSC 9;4 (`ui.progress`: a running turn as taskbar
  progress), both written only when the state changed and cleared on quit.
- **Drag and drop is a paste.** `ui/drop.go` turns a paste made only of existing paths into
  attachment tokens. Files from outside the project are copied to `<data>/drops` and referenced
  as `@drop:<name>`; `expandAttachments` resolves `drop:` only inside that directory, so a
  forged reference cannot attach other files.
- **gopher-lua multiple assignment**: `a[#a + 1], b = b, x` stores `x` in `a`. Split any
  assignment whose right side names a left-side target (this once garbled wrapped text).
- **Handles vs snapshots**: host resources (job, http, timer, session, window) are handle
  tables with colon methods, everything else a snapshot copy; marshaling caps string size and
  depth and refuses cycles.
- No vendoring, no generated code.
