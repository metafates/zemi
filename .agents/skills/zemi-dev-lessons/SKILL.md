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
  the repo), and write patch files with `git diff --no-ext-diff` (a display
  diff tool that `git apply` cannot read: it is `diff.external` in the machine's global
  `~/.gitconfig`, not this repo's). When copying state for a boot check, leave out
  `scratch` and `tmp`, which run to gigabytes.

## Prompt cache

Anything in the prefix that varies per request busts the provider cache for the rest of the session. The main agent's whole prefix is one pin (`ag.pin`, `pinPrompt`): read files and Lua output when it is built, and put a new input that must rebuild it in its key, never in a pin field of its own. Never per turn: no dates, git status, map-ordered schemas or a plan re-injected every step. Check by diffing two consecutive request bodies from the fake server (zemi-tui-test).

## Tests that will not flake

- Never synchronize on `time.Sleep`; wait on a channel, `waitIdle`, a hook or an observable state. A sleep fails under load or makes the assertion vacuous.
- A test's budget measures work, never the clock: CPU time (`TestSendPrepCheap`); "returned before its deadline" is `ctx.Err() == nil` after the call. A wall-clock slice in the product gets a floor that holds on any machine, bounded by what it draws rather than by the data, and its test runs on a clock always past the slice (`TestColdChatRender`'s `lagClock`). A limit a case must wait out is short only in that case (`TestPluginsTest`'s `hangs`).
- Pure timer logic (watchdog, idle model, priority queues) runs under `testing/synctest`. It does not fit child processes, ptys, sockets, or a Host booted outside the bubble, whose Lua actor and `database/sql` goroutines never go idle.
- `t.Parallel()` on slow process-driven tests (LSP, plugin test boots) more than halves the hostapi run. It is safe only with no `t.Setenv`, `t.Chdir`, package globals or shared ports.
- Boot-time goroutines (the `git status` warm-up, MCP start) capture what they read by value; a test that mutates `h.Dirs` after boot races them.
- A package global written per host races between parallel hosts; make it a field.
- Assert on the settled state, never a milestone on the way to it: when a background goroutine still owes its critical section (a job exit's eviction sweep, a report joining the transcript), an earlier condition holds while the state is still moving (`TestJobsEvictFinished` waits for the settled cap, not the first eviction).
- A scripted handoff keeps the sender alive until the receiver has read: once the child ends, its follow-up joins the reader's same step boundary, and "the message was the last thing the model saw" becomes a coin flip (`TestAgentUpdateReachesTheCaller`'s child blocks on the read).
- A test that counts events counts a background emitter too: a status change's own rebuild lands any time after its readiness is observable, so anchor the count to that rebuild first (`TestMCPOverlayRefresh` waits for the ready rebuild).
- A product wall-time ceiling stops an innocent handler on a loaded machine: a test whose handler must land raises the host's ceiling first (`h.toolPostCeiling` and `h.luaCeiling`), and the test host closes its runtime on cleanup: a package whose tests leave one open fails its goleak gate (`main_test.go`), as every leaked actor pins its Lua state.
- Run `make flake` after touching concurrency, timers or turn ordering; `make race` alone runs each test once and lets a scheduling flake slip. `make flake` prints the failing package only in the moment it fails: read the whole output (no `tail`/`grep` over it), or a lost first run makes a real race read like a flake.

## Traps

- A pin flip is the loudest alarm in a polish round: a fix that must edit an existing test's
  `want` or regenerate a golden to pass is changing designed behavior, not fixing a defect.
  Round 12 "fixed" a persona's "digest scrolls off the top edge" exactly this way
  (TestChatStickyDigest's want edited, golden regen'd) while DESIGN.md's chat section named the
  old behavior as the design; the user reported the flood it caused two commits later. Before
  fixing a behavior finding, grep DESIGN.md for it: a spec-named or test-pinned behavior is the
  user's call, and a finding that wants it changed goes back as a question.
- Agents sharing one working tree must not commit concurrently; give each a package or run them in sequence.
- gopher-lua: once `pcall` catches an error, closures made before it stop sharing locals with their maker. Share state through a table.
- Lua's `os.clock` is wall time on the runtime's `lua.Clock`, not CPU time.
- Builds fill the disk: a worktree's builds add to the Go build cache and a binary is 100 MB. Check `df` between rounds of parallel workers and delete scratch binaries.
- ast-grep's Go grammar lags the toolchain: a generic method's body parses as top-level statements
  (local `var` reads as a global), and 13 files carry ERROR regions — spot-check a new rule against
  `internal/hostapi/plugintest.go` and `internal/config/store.go` before trusting its hit count.

## Testing patterns

- `internal/hostapi/host_test.go` has the canonical helpers: `newTestHost(t)` boots a fresh
  runtime + namespaces step with temp dirs; `runUser(t, r, luaSrc)` runs a chunk as user origin
  **with a 30s guard so an interpreter deadlock fails loudly instead of hanging the binary**
  (and a 30s slice ceiling, which load does not reach) - reuse it for any test that runs Lua.
- The Lua API surface is pinned by `TestTypesCoverAPI`, `TestTypesDeclareNothingExtra` and `TestTypesEnums` against the types file, its version by `TestNamespaceVersion`, the rendered UI by the golden files.
- Drive behavior through Lua where the Lua contract is the thing being pinned; Go-internal
  units (the gate in tools, the store) get table tests.
- Skills tests: `newTestHostSkills(t)` boots the namespaces step, which syncs skills;
  `newTestHostSkillsNoBoot(t)` lays files down WITHOUT booting - every `Boot` builds a fresh
  generation, so steps sharing state must be passed in ONE call.
- Agent-turn tests stub the model with `reflectStub` and wait with `waitIdle`. End to end,
  drive the real binary in tmux against a fake OpenAI-compatible server: the `zemi-tui-test`
  skill has the procedure, `zemi-safe`, tmux and `fake_openai.py`.
- `internal/lua/sandbox_test.go` is adversarial by design: monkey-patching attempts must raise.
