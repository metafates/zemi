---
name: zemi-perm-sandbox
description: "Load before changing internal/perm, internal/sandbox, internal/tools or internal/proc, or before adding or judging a permission rule, an ask or a shell parse: the tool pipeline, the gates, the modes, the sandbox policy and the always answers."
---

# Permissions, the sandbox and the tool pipeline

## Tool pipeline (internal/tools/tools.go)

Every model tool call runs: JSON unmarshal -> schema validation -> `tool.pre` Lua events (deny
/ halt / rewrite) -> permission gate -> run -> `tool.post` handlers, then an override's `post`
(`Host.toolPost`, each a task seeing the output the last left; a `{ output }` return rewrites
what the model gets, never the diff, `changed` or the decision, and a project one is ignored;
no handler, no Lua). Invariants: a
rewritten input **re-enters at the permission gate** (per-run pipeline copy with
`RewriteLoosened`, so a non-user rewrite can't ride a pre-existing allow glob); an ask with no
overlay available fails closed; a denied result says who decided and why; errors become
tool-error responses, not panics. Host-implemented tools (`agent`, `workflow`, `lua`,
`update`, MCP and Lua tools) call the gate themselves and run the events through `preCheck`
(which refuses a rewrite with an error result), `postNotify` and `Pipeline.CheckDenied`.
A command a permission mode settled runs sandboxed (per-run `Pipeline.Sandboxed`); a
`--unsafe` run has no backend at all, so nothing it settles is confined.

The lua tool's script can drive the run's own tools: `zemi.tool.call(name, args)`
(hostapi/luatool.go) starts one through that same pipeline off the interpreter, capped at 8
at once, and only the script's print and return values reach the model, so it
batches and filters what would otherwise be a step per call. Its output comes back whole
(`Pipeline.Script` on the per-call pipeline copy, which `tools.(*Pipeline).cut` reads),
and only the running script reaches it, so a script that only drives tools needs no approval
of its own: its calls are gated as the model's own are.

The shell tool is special: `Pipeline.Check` normalizes the command once (`perm.Normalize`, internal/perm/bash.go:
mvdan.cc/sh's `syntax` parser, never `interp` or `expand`, walked into one segment per simple
command plus the redirect and env-assignment gates; heavily table-tested, extend those tables when
touching it), and `perm.Store.CheckCommandFor`, the predicates on `perm.Normalized`, the ask hook
and `alwaysChoice` (per segment, `Normalized.Rows`) all judge that one parse (`Request.Command`); a
nested `sh -c` body, `eval` text or a heredoc body a shell may run is parsed again. The command
is parsed once as POSIX sh whichever sh runs it (DESIGN §9), bodies included (`shellText` for
one a runner such as `xargs` or `sudo` hands a shell), and perm knows nothing of which shell that
is. Any parse error fails the call (`bashism`, naming the POSIX form of the bash-only syntax models
write most, `posixForms`), as does text some sh reads otherwise than the parse: `$'..'`, `$".."`,
`[[`, `source`, `let`, a `((` opening a statement, `x+=1` or `a[0]=1` as a command's first word,
a brace word (`{a,b}`, `{1..3}`) and a backslash ending the text. A failed parse is the shell and
`job_start` tools' result before the gate (`shellRequests`, which hands the parse on in
`Request.cmd`): the reason alone, no ask and no permission verdict. A carriage return, an `alias`
(sh expands it in later lines), in a body another shell runs a heredoc inside `$( )` (bash 3.2,
macOS sh, ends it early), and bash-only syntax in a heredoc body a reader other than a shell or an
interpreter takes (`Normalized.bash`: `make -f -`, `$SHELL`) are `Normalized.doubtful`: it asks,
and `Destructive` and `Programs` do not vouch for it.
`TestRowsCoverShells` runs a sample under `proc.Shell` with a PATH of stubs and checks each program
it ran is a segment. A heredoc body is commands unless every
program in the command runs none of it (`inert`: a `dataReader` or a plain git builtin), and
an unquoted one is read with the backslashes the shell drops removed; leading assignments' values
are read in place of `$NAME` where the words render (never into a heredoc delimiter). An assignment
prefix, or arithmetic naming a variable, downgrades an allow verdict to ask (`Assign`), since the
rules never see the value and `GIT_EXTERNAL_DIFF=./x git diff` runs `./x`.
A segment with a word that climbs (`docs/../x`) is judged as written and with the word cleaned,
and the stricter verdict holds (`cleanDotDot`), so `cat docs/*` does not allow it; a path word
the shell could still expand into a climb (`{`, `$`, a glob after a leading dot) turns an allow
into an ask (`expandsToDotDot`). A deny, ask or not_match rule also meets the segment's `perm.canonical` form
(runner prefixes like `env`, `sudo`, `timeout 5` stepped past, the program
by `perm.ProgramName`, its leading options dropped), so `git -C . push` meets a `git push*` rule; an
allow is judged as written, and a git alias still slips past (NOTE in the code). The lookups that
decide risk - the canonical form, the runner, subcommand, plain-flag and destructive lists - go
through `perm.ProgramName`, the base name case folded as APFS folds it, since `/bin/RM` runs rm and
`ſh` (long s) runs sh; `safeSegment`'s refused-flag and reader tables, the `cd` test and the always
answer's rule name read the word as written.

File tools (internal/tools/fs_tools.go): `write` refuses a file the
model has not read (outside the free zone: `perm.FreeZone`, the scratchpad and the temp dirs) or one whose mtime moved since (`Pipeline.Seen`, per session; subagents get
their own), and `edit` takes either only when `old_string` matches exactly once as written (the match proves the model knows that text and no other, so the file stays unread for `write`); both hold a per-path lock, so a concurrent agent or the user's editor cannot lose
changes; within one step the lock is taken in call order (`tools.CallStart` from fantasy's
`OnToolCall`, which fires for every call before any is dispatched; `tools.CallDone` from
`OnToolResult`, which fires for a call fantasy refused to run too, and once more for the
runner's ids when its turn ends, so a call nothing ran never holds the file), so two edits to
one file emitted together apply as written.
`ResumeSession` rebuilds that map from the stored rows (`seedReads`), leaving out
files touched after the last message. `tool.Run` runs the same check before the gate
(`tool.edits`), so nobody approves a change the tool would then refuse, and resolves the path
once (`perm.Resolve`) before the gate into the per-run `Pipeline.Path`; the tools open exactly
that `perm.Path` (`Path.Open`, an `openat` walk with `O_NOFOLLOW`, DESIGN §9) and never the input string,
so a sandboxed command cannot swap a directory for a symlink between check and open.
`zemi.fs`, `@file` attachments and the `lua` tool's `path` open through a `perm.Path` too
(`resolveJailed`). Both writers refuse a path whose last component is a symbolic link
and name the target to edit instead. `glob` and `grep` are gated on their root only, so neither
shows a hit a deny or ask `fs.read` rule covers, and both list through `tools.ListFiles`
(`git ls-files`, a walker where git cannot list or takes over 2 s; no symbolic links, so grep reads through
none). grep and a directory listing leave out the strict secret shapes
(`perm.PathIsStrictSecret`: not `*secret*`, `*credential*` or a `.env.example`): a listing names what it hid, and grep's answer never depends on them, since a
count of their hits would leak a key one bit per call. Output too long for a result is cut
head and tail at line breaks, the marker naming the cut lines, and saved to the scratchpad
(`tools.Spill`); command output, `!command` included, goes through `tools.CmdOut`, which also
counts what the 1 MB buffer dropped. A shell command still running at its timeout is handed to `Jobs.Adopt` instead of being killed
(which is why it is not bound to the turn's context; an interrupt still kills it). A shell row
shows what the command changed (hostapi/snapshot.go, `Pipeline.Changes`): a git tree of the
repository before and after, written through a warm temp index and object directory so the
user's index and object store are never touched; it leaves out the secret shapes the sandbox
hides. The two are diffed into the result's `Metadata`; the model gets one line of paths and `tool.post`
every path as `ev.changed`, next to the file tools' own. `read` carries the nearest
instruction file not already in the prompt, once per session, so a monorepo package's own
AGENTS.md reaches the model.

## Permission engine (internal/perm)

Rules `(subject, pattern, decision, not_match?, principal?)`, subjects like `tool.shell`,
`fs.read`, `fs.write`, `net.<host>`, `lua.<ns.call>`; a subject outside those families, or
a builtin tool other than `tool.shell`, `tool.lua` and `tool.workflow`, raises when added
(`Host.ruleShape`), and once boot is done `Host.CheckToolNames` records each `tool.*` rule
and `zemi.tool.override` no tool answers to (a Lua tool may come from a project or a
plugin, so the call cannot tell). Ordering: deny > ask > allow; within one
decision, fewest wildcards then longest literal prefix wins. A `not_match` hit falls through
and **forces ask**. The default when nothing matches is `Store.Default` (host wires
`fsDefault`: jail reads and the model's reads of the plugins, minus any `.git`, allow except
secret-shaped paths, writes ask) and finally ask - there
is no fail-open path. Past all of that, an enabled plugin's own gated calls are the one allow added
(`Host.pluginTrusted`, below). A file request in the free zone (`perm.FreeZone`: the pipeline's
scratchpad, or a native temp dir) allows before any rule, a secret-shaped name and a
`protectedWrite` (`.git`, the Lua in `.agents`, zemi's own dirs, a local plugin) aside, and the
file tools need no prior read there (`perm.InPad` is the narrower pad-only check the protected
ones use). New host-API side effects need the same `lua.` subject.

`permissions.mode` (internal/hostapi/permmode.go) only answers asks that survive the rules and
the `permission.request` veto, and its own rules are deterministic: no model screens anything.
The one exception is a mode's own `decide` (below): a model call there is what the user opted
into by writing the mode. `yolo`
allows, an ask a rule asked for included (an `ask` rule, a `not_match` exclusion:
`Request.Explicit`, which `auto` and a mode's own rules leave to the user). `auto` settles what `autoLocal` covers - reads and edits **inside the jail** that are
not secret-shaped (reads of the local plugins too), `tool.workflow` scripts (each agent they start is gated), shell commands
(`unsettled`), a `tool.<name>` a plugin registered (`Host.pluginTrusted` settles a plugin's own gated calls the same
way), and MCP tools a server annotates
read-only - and hands the rest to the user with a reason naming why (`askHook`; writes under
`.git` or zemi's own dirs (a local plugin included: it loads in every project, so like the
recipes it asks every time, with no always answer, except one whose creating write the user or
yolo allowed in this process (`madePlugins`, recorded by `askHook`, or by `/plugins modify`), which auto then edits in any
project), to the Lua in any `.agents` (`init.lua`, `lua/`) and to a
`.gitattributes` or `.gitmodules` are `protectedWrite`, checkouts in `.agents/worktrees` aside,
which no rule settles, a mode's own included; the rest of `.agents` (skills, agents, commands,
workflows) loads nothing into the interpreter and is an ordinary project write).
A read-only agent (`Profile.ReadOnly`, see below) denies writes and runs commands with the
project unwritable in the sandbox, whatever the mode; without trust or a sandbox, a changing
command asks (under yolo, only without a sandbox), as does an MCP tool not marked read-only.
Network requests are never settled by a rule: the URL is the model's. A mode from config, a
plugin's included, may carry
`decide`, a Lua function answering one ask ("allow", "deny", anything else sends it to the user
whatever the base would settle, which is how a user gets every ask); it runs as a task for at
most 60s, long enough for a model call of its own, and, unlike the mode's own rules, it may
allow an `Unsandboxed` or `Network` request, an `Explicit` one or a `protectedWrite`, since a
human could; each such allow leaves a `RowNotice` row saying so. Its table answer
`{ decision = "allow", scope = ..., reason = ... }` writes what the user's own answer of that
scope writes (`alwaysChoice`, `repoChoice`, `projectChoice`), a scope the request does not offer
narrowing to the closest below it (`req.options` says which are on offer), and its `reason`
rides the row and a denial. In a trusted project with a live sandbox `unsettled` parses nothing:
every command runs confined, except the short `perm.Normalized.Destructive` list (bulk deletes,
DESIGN §9; an `rm` whose every operand is below the scratchpad or a temp dir is outside it), a git command that writes what the sandbox keeps read-only in `.git`, reaches the
network or can drop commits for good (`perm.Normalized.GitWrites`, whose `gitDir` says whether the
policy opens `.git`, `Policy.Git`: a confined run could only fail, so the ask comes before a
wasted run) and, on a backend that
cannot hide files, one naming an existing secret-shaped file. Known ceiling: the sandbox policy is the only limit, so the model
can write code and run it unattended, rewrite any project file and read what the policy does
not hide, outside the project too. In an untrusted project both modes settle only `ReadCommand`
commands (every program only reads, named without a path; no builds, no git) and, with a sandbox,
`zemi plugins test <name>`, confined (`perm.Normalized.PluginTest`: a name, since the runner copies
a directory it is given where the model reads), and auto settles
no project writes (a `madePlugins` plugin is in no project). In a trusted project with no sandbox backend auto still settles jail writes, but
commands settle only when `ReadCommand` holds, since an approved one would run unconfined.
Asks reach the user one at a time (`Host.askMu`, held over the prompt stage only, never over
the veto or `decide`), and a waiter is judged again once it holds it (`Request.Recheck`, then
the mode and trust with no Lua), so an answer, a switch to yolo or new trust settles the asks
behind it, a rule's verdict returned whole (`tools.ByRecheck`) so a network rule keeps the sandbox.

**Sandbox** (internal/sandbox, internal/hostapi/sandbox.go, DESIGN §9): a `tool.shell` verdict a
mode gave (`DecidedBy` `mode:<name>`, base not yolo) makes `Pipeline.SandboxFor` return
`h.sandboxPolicy(readOnly, pad)`; `tool.Run` puts it on the per-run pipeline copy, and `runShell`,
`Jobs.Start`/`Adopt` call `sandbox.Wrap` before starting. A wrap error is a
tool error, never an unconfined run. Rule-allowed and user-approved runs, `!command`,
`zemi.job`, MCP servers and `Host.git` stay unconfined, except under a read-only agent, whose
every command runs confined whoever allowed it (`Request.ReadOnly`; `unsettled` then skips the
`Destructive` and `GitWrites` asks, since the confined run fails on its own); language servers
run under `serverPolicy`; a `--unsafe` run gives no policy, so a code-running server starts
unconfined, where a probe that failed leaves it out. `NewHost` probes the backend synchronously, so
`zemi health` and the first turn's
schema see it.

`sandboxPolicy` writes the policy; DESIGN §9 has the whys behind it: the scratchpad and the
machine's temp dirs (`perm.TempRoots`: this process's temp dir and `/tmp`) as write roots,
the scratchpad also every model command's `TMPDIR`, `GOCACHE` per trust root, the project writable only when
it is trusted, outside zemi's own dirs and on a backend that can protect paths inside it
(`Backend.EnforcesProtect`), `.git`, `.agents` and every `PATH` dir under a write root read-only
because zemi runs git, the shell and servers unconfined by name (on seatbelt a repository root's
`.git` opens but for what git runs or follows, `Policy.Git`, whose rules and the whys are in
DESIGN §9 and `seatbelt_darwin.go`'s `gitCode`), home credential paths hidden (`homes`),
the secret shapes hidden in the pad too (a command can move a directory there), zemi's types and builtin skills readable
back (`shippedDirs`, which the read jail and `safeCommandPath` share: the embedded runtime rewrites them every start), and a
local plugin's code (`plugin.Code`, `Policy.Read`, `Policy.List`) so `zemi plugins test` runs confined. The
repo's `.agents/worktrees` (`Dirs.WorktreesDir`) is a write root of its own, ignored by the `/*`
in its own `.gitignore` (`Dirs.WorktreesIgnore`, written only when missing), with each
checkout's own `.git` and `.agents` protected and nothing loaded from it. The namespace
backend binds only protected paths that exist, so `Pipeline.Guard` records the missing ones
before a run and checks them at exit: a created `.git` or `.agents` holds trust off until
`/trust`. The shell tool and `job_start` take two escapes no mode's rules settle, each of
which asks unless a mode's `decide` answers it: `network: true` only adds sockets,
`sandbox: false` drops the whole policy. A confined
failure `sandbox.LikelyDenied` recognizes, a death by signal (`sandbox.Killed`) included, gets `Pipeline.sandboxHint`; the limits themselves
are the system prompt's Sandbox block (`Host.sandboxPrompt`, from `sandboxPolicy` without the
project walk, which must not delay the first request).

`ReadCommand` is an allowlist of readers (internal/perm/safe.go, table-tested), which settle
unconfined: no shell expansion outside quotes (`$` and backticks inside double quotes count),
no write redirects (`2>/dev/null` and `2>&1` are none), no program named by a path, no chain of
`cd`s leaving the shell in more than 64 directories (each cd doubles the set every argument is
judged from, and past the cap the command asks), and every
argument must pass `safeCommandPath` (inside the project and not secret-shaped - `h.secretPath`
matches the resolved path too, so a symlink cannot hide one, and every home path the sandbox
hides (`secretDirs`: `~/.kube`, `~/.config/gh`, shell histories), whose names say nothing, so
one list drives the sandbox, the re-ask and auto's read check; a plain read may also reach the
request's scratchpad, zemi's config tree, the local plugins and `shippedDirs`, but `cd` may not enter them). Readers keep their
`refusedFlags` and script checks, and `perm.ArgUse` says how an argument is used: a search
pattern is no path, and a recursive search settles only over a tree with no secret-shaped file
in it and, unless it skips hidden entries like a plain `rg`, no `.git` directory (`grep -r`
reads one). Nothing may point into `.git`, since `.git/config` may hold tokens.

Every rule lives in memory: `zemi.permission.rule.add` in config adds it again on each boot,
and an "always" answer lasts until zemi exits. There is no rule file and zemi makes no git
commits (`scope = "permanent"` raises and points at init.lua). Untouched by that "no rule
file": a trusted project's `permissions` file in its state dir (`<state>/projects/<hash>/`,
next to the recipes, where no repository can ship it and no sandbox can write it;
`ProjectPermissionsStep`, internal/hostapi/projectperm.go) lists shell commands always allowed in that project, each by
the `alwaysChoice` rule shape (a wrapper or deleting program or a glob line offers no rule);
it is read as a Go boot step, so it never goes through the Lua origin gate below, and a
permission overlay's `p` answer appends the command to it, offered only when
the file would load exactly the rules the label names (`projectChoice`). Only `user`
origin may add allow rules (project may tighten only), scope a rule to one plugin
(`principal = "plugin:<name>"`, which a plugin's refused call puts in the rule line it names)
or switch the mode
(`zemi.permission.mode.set`, which the shipped ctrl+y yolo toggle uses); user, builtin and
plugin code may add or drop a mode, the project refused, the builtin names (`auto`, `yolo`)
are refused by name to everyone, and `mayReplace` refuses another plugin's name to a plugin;
the gate lives at the namespace closure, same off-interpreter-`OriginHost` reasoning as skills.
Enabling a plugin is its own trust decision (`Host.pluginTrusted`): an ask no rule decided settles for its own
`lua.model.ask`, `lua.classify.ask`, `lua.agent.send`, `lua.http.*`, `lua.fs.write` and its `lua.job.start` only
when the job asked for the sandbox (`sandbox = true`: an unconfined one runs with the whole environment),
a protected write (`protectedWhy`) and a secret shape aside, and `autoLocal` settles a `tool.<name>` whose tool
a plugin registered. A rule of the user's still decides first - an ask rule or a `not_match` exclusion is the
rule's ask, which `decidedBy` reports as `rule` - and a test zemi answers only its stubs.

The overlay's "always" answer writes session allow rules scoped to the `model` principal
(`Rule.Network` when the ask was a `network: true` command, so `SandboxFor` keeps the policy and
adds sockets instead of running the next call unconfined; `projectChoice` offers no file line
for one), as prefix globs rather than the exact request (`alwaysChoice`, ask.go: a command's program, or
program and subcommand for `git`, `go`, `cargo` and the other `perm.SubcommandPrograms`, leading
options kept (`git -C sub status`, `perm.Subcommand`) and the exact segment when options leave
no subcommand (`git --version`) or one not for sure (`npm -w web test`: `perm.LeadKnown`); a file's directory, or the
file alone when that is `$HOME`, `/` or between, which hold every dotfile; a URL's host). A deleting program (`rm`, `mv`, `find`, `tar`, ...) or a segment
`perm.Normalized.Destructive` names stays exact, since its rule would run unconfined, and so
do `[[` and the `perm.ShellState` builtins (`let`, `read`, ...), which evaluate a quoted
array subscript, where `$(...)` runs; such an exact form holding an expansion offers no rule
(`rm $f` deletes whatever a loop hands it). A file read is also
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
no `model` principal, stays authoritative, and shows read-only in `/permissions`, while each
always answer is one row (its bare and wildcard rules, `answerKey`) whose Delete drops both.
`sandbox: false` is denied outright for a read-only agent (`Pipeline.Check`, before any mode
could settle it), and its sandbox hint offers only `network: true`.

## Gotchas

- **Every child process goes through `internal/proc`** (shell tool, `!command`, jobs, Lua jobs,
  MCP and LSP servers, git). The model's command lines (shell, `job_start`, and so an adopted job)
  start through `proc.ShellCommand`: `proc.Shell` is dash where `PATH` has one, looked up once
  per process, else sh; the user's `!command` runs sh. A child runs in its own session: no controlling terminal (nothing
  can read the TUI's keys or hang on a password prompt) and its own process group, which
  cancel, timeout, `proc.Wait` and `proc.KillAll` on exit kill whole. A plain `exec.Command`
  here is a bug (a cancelled `sh -c` pipeline once hung the turn forever), with three exceptions:
  the user's editor (ui/editor.go, which needs the controlling terminal `Setsid` drops),
  the sandbox backend probes, and main's `relaunch`, the exec that replaces zemi with
  `zemi -C <dir>` once the TUI restored the terminal, `proc.Refuse` held further starts and
  the children `proc.KillAll` killed were reaped (`proc.Reaped`: on darwin an exec racing the
  reaping crashes the new image).
