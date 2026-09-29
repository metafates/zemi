---
name: zemi-dev-lessons
description: "Load before a non-trivial change to zemi: checks against real user data, fail-opens in the bash parse and sandbox, prompt-cache traps, tests that do not flake, landing on main."
---

# Lessons for changing zemi

AGENTS.md has the house rules and `make check`; these are the ones it leaves out.

## Landing a change

- lua-language-server is installed here, so `make check` runs the luals step: a wrong `---@param` union in `runtime/lua` fails it.
- A rename or removal also greps README.md. A stale tool or key name in `runtime/skills` costs the model real turns.
- Work on a branch, `git merge --ff-only` into main, delete the branch. Never push.
- A rebase that meets main's api version bump takes the next minor above main's, in `host.go` and `TestNamespaceVersion` together.
- golangci-lint holds one lock per machine: while another worktree lints, `make check` fails with "parallel golangci-lint is running". Run it again; that is no finding.

## Every commit boots

- zemi keeps no migrations before its first release. A schema change bumps `schemaVersion` in `internal/store/store.go`; after merging it, `make reset-db` drops the owner's stores (allowed) instead of writing an ALTER.
- Never `go install` or write `~/go/bin/zemi` from a branch: that is the owner's daily binary. Build into a scratch dir and run it through `zemi-safe`.
- After a merge to main, boot the merged binary once with `zemi-safe <tree> health`, the tree holding copies of `~/.config/zemi` and `~/.local/state/zemi`: `make check` cannot see a user's data.

- Parallel workers in worktrees: never `git stash` (the stash is shared by every worktree of
  the repo), and write patch files with `git diff --no-ext-diff` (the repo sets a display
  diff tool that `git apply` cannot read). When copying state for a boot check, leave out
  `sandbox-cache`, `scratch` and `tmp`, which run to gigabytes.

## The bash parse and the sandbox

A change that treats text as data (a heredoc body, a quoted word) or that exempts a path from
the policy is where a fail-open comes from, a fix included. mvdan.cc/sh reads the grammar right;
what fails open is a shell that reads the same text otherwise.

- Try what runs the text under another name: `$SHELL`, `busybox sh`, `make -f -`, `eval "$(cat)"`, a pipe into a shell, a git alias, `RM` on a case-insensitive disk, an escape that spells a name (`$'\x72m'`).
- Try what the other shells make of it. zemi runs the model's commands with dash where it is installed and judges them as dash reads them, failing any text its parse refuses; the two readings (bash and POSIX) judge the rest: a `sh` fallback that may be bash, and every body another shell runs (`sh -c`, `bash -c`, a heredoc fed to a shell). macOS `sh` is bash 3.2: it closes a `$( )` at a `)` in a heredoc body and runs the rest, and fails on an apostrophe there. Debian's is dash: it ends `$'\'` at the backslash (a newer dash decodes `$'..'` as bash does) and runs the words of `[[ ]]` and `(( ))` as commands. Both expand an `alias` in the lines after it, and hand an unquoted heredoc body on with `\$` and `` \` `` unescaped. The parser itself reads a comment's trailing backslash and a carriage return otherwise than every shell. Run the text under /bin/sh, /bin/dash and /bin/bash with a PATH of stubs that log their argv, one write per record, in an empty dir (`TestRowsCoverShells`), never with the real PATH; a row covers what ran only word for word.
- The POSIX parser refuses more than dash does: dash parses `${x:1}`, `&>`, `{fd}>` and `function f {` and runs the lines after them. A reading that stops at a parser error hides those lines; fix the text inside that reading's own parse, since the other reading's offsets miss what it quotes otherwise. Anything that recurses in both readings (a heredoc, `sh -c`) doubles per level of nesting unless it is read once.
- A rule keyed on a file's name is judged where the file is, so a writable root exempt from it takes a renamed directory with the files inside.
- A rewrite of `perm/bash.go` that must change no verdict is checked against a copy of main's package: every string literal of the perm, hostapi and tools tests through both, then a few minutes of fuzzing the difference, and each difference that loosens a verdict run under the shells as above. The tables alone missed a panic and an endless loop.

## Prompt cache

Anything in the prefix that varies per request busts the provider cache for the rest of the session. The main agent's whole prefix is one pin (`ag.pin`, `pinPrompt`): read files and Lua output when it is built, and put a new input that must rebuild it in its key, never in a pin field of its own. Never per turn: no dates, git status, map-ordered schemas or a plan re-injected every step. A profile or tool-set switch may bust it once, since the user asked. Check by diffing two consecutive request bodies from the fake server (zemi-tui-test).

## Tests that will not flake

- Never synchronize on `time.Sleep`; wait on a channel, `waitIdle`, a hook or an observable state. A sleep fails under load or makes the assertion vacuous.
- A test's budget measures work, never the clock: CPU time (`TestSendPrepCheap`, the snapshot cutoff); "returned before its deadline" is `ctx.Err() == nil` after the call. A wall-clock slice in the product gets a floor that holds on any machine, bounded by what it draws rather than by the data, and its test runs on a clock always past the slice (`TestColdChatRender`'s `lagClock`). A limit a case must wait out is short only in that case (`TestPluginsTest`'s `hangs`).
- Pure timer logic (watchdog, idle model, priority queues) runs under `testing/synctest`. It does not fit child processes, ptys, sockets, or a Host booted outside the bubble, whose Lua actor and `database/sql` goroutines never go idle.
- `t.Parallel()` on slow process-driven tests (LSP, sandbox builds) more than halves the hostapi run. It is safe only with no `t.Setenv`, `t.Chdir`, package globals or shared ports.
- Boot-time goroutines (the `git status` warm-up, MCP start) capture what they read by value; a test that mutates `h.Dirs` after boot races them.
- A package global written per host (`perm.EnvKeep` once) races between parallel hosts; make it a field.
- Run `make race` after touching concurrency.

## Traps

- Agents sharing one working tree must not commit concurrently; give each a package or run them in sequence.
- gopher-lua: once `pcall` catches an error, closures made before it stop sharing locals with their maker. Share state through a table.
- Lua's `os.clock` is wall time on the runtime's `lua.Clock`, not CPU time.
- Builds fill the disk: a worktree's builds add to the Go build cache and a binary is 100 MB. Check `df` between rounds of parallel workers and delete scratch binaries.
