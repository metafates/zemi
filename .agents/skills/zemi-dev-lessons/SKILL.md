---
name: zemi-dev-lessons
description: "Load before a non-trivial change to zemi: checks against real user data, fail-opens at trust boundaries, prompt-cache traps, tests that do not flake, landing on main."
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

- A schema change is a new numbered file under `internal/store/migrations`, applied at the next boot like any goose migration; never edit one already applied and never reset the owner's stores for it.
- Never `go install` or write `~/go/bin/zemi` from a branch: that is the owner's daily binary. Build into a scratch dir and run it through `zemi-safe`.
- After a merge to main, boot the merged binary once with `zemi-safe <tree> health`, the tree holding copies of `~/.config/zemi` and `~/.local/state/zemi`: `make check` cannot see a user's data.

- Parallel workers in worktrees: never `git stash` (the stash is shared by every worktree of
  the repo), and write patch files with `git diff --no-ext-diff` (the repo sets a display
  diff tool that `git apply` cannot read). When copying state for a boot check, leave out
  `scratch` and `tmp`, which run to gigabytes.

## Prompt cache

Anything in the prefix that varies per request busts the provider cache for the rest of the session. The main agent's whole prefix is one pin (`ag.pin`, `pinPrompt`): read files and Lua output when it is built, and put a new input that must rebuild it in its key, never in a pin field of its own. Never per turn: no dates, git status, map-ordered schemas or a plan re-injected every step. A profile or tool-set switch may bust it once, since the user asked. Check by diffing two consecutive request bodies from the fake server (zemi-tui-test).

## Tests that will not flake

- Never synchronize on `time.Sleep`; wait on a channel, `waitIdle`, a hook or an observable state. A sleep fails under load or makes the assertion vacuous.
- A test's budget measures work, never the clock: CPU time (`TestSendPrepCheap`, the snapshot cutoff); "returned before its deadline" is `ctx.Err() == nil` after the call. A wall-clock slice in the product gets a floor that holds on any machine, bounded by what it draws rather than by the data, and its test runs on a clock always past the slice (`TestColdChatRender`'s `lagClock`). A limit a case must wait out is short only in that case (`TestPluginsTest`'s `hangs`).
- Pure timer logic (watchdog, idle model, priority queues) runs under `testing/synctest`. It does not fit child processes, ptys, sockets, or a Host booted outside the bubble, whose Lua actor and `database/sql` goroutines never go idle.
- `t.Parallel()` on slow process-driven tests (LSP, plugin test boots) more than halves the hostapi run. It is safe only with no `t.Setenv`, `t.Chdir`, package globals or shared ports.
- Boot-time goroutines (the `git status` warm-up, MCP start) capture what they read by value; a test that mutates `h.Dirs` after boot races them.
- A package global written per host races between parallel hosts; make it a field.
- Run `make race` after touching concurrency.

## Traps

- Agents sharing one working tree must not commit concurrently; give each a package or run them in sequence.
- gopher-lua: once `pcall` catches an error, closures made before it stop sharing locals with their maker. Share state through a table.
- Lua's `os.clock` is wall time on the runtime's `lua.Clock`, not CPU time.
- Builds fill the disk: a worktree's builds add to the Go build cache and a binary is 100 MB. Check `df` between rounds of parallel workers and delete scratch binaries.

## Testing patterns

- `internal/hostapi/host_test.go` has the canonical helpers: `newTestHost(t)` boots a fresh
  runtime + namespaces step with temp dirs; `runUser(t, r, luaSrc)` runs a chunk as user origin
  **with a 30s guard so an interpreter deadlock fails loudly instead of hanging the binary**
  (and a 30s slice ceiling, which load does not reach) - reuse it for any test that runs Lua.
- The Lua API surface is pinned by `TestTypesCoverAPI`, `TestTypesDeclareNothingExtra` and `TestTypesEnums` against the types file, its version by `TestNamespaceVersion`, the rendered UI by the golden files.
- Drive behavior through Lua where the Lua contract is the thing being pinned; Go-internal
  units (the gate in tools, learn intake, store) get table tests.
- Skills tests: `newTestHostSkills(t)` boots namespaces + skills steps;
  `newTestHostSkillsNoBoot(t)` lays files down WITHOUT booting - every `Boot` builds a fresh
  generation, so steps sharing state must be passed in ONE call.
- Agent-turn tests stub the model with `reflectStub` and wait with `waitIdle`. End to end,
  drive the real binary in tmux against a fake OpenAI-compatible server: the `zemi-tui-test`
  skill has the procedure, `zemi-safe`, tmux and `fake_openai.py`.
- `internal/lua/sandbox_test.go` is adversarial by design: monkey-patching attempts must raise.
