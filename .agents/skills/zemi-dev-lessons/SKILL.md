---
name: zemi-dev-lessons
description: Hard-won rules for changing zemi that AGENTS.md does not spell out - the check sequence, golden and API-version discipline, what makes tests flaky here, prompt-cache traps, and how to land work on main. Read before a non-trivial change.
---

# Lessons for changing zemi

## Landing a change

1. Read the DESIGN section the code cites before touching behavior; the tree wins over the spec.
2. `make check` (vet, tests, luals, gofmt) must be clean; lua-language-server is installed, so
   `make luals` is not optional. A wrong `---@param` union in `runtime/lua` fails it.
3. UI change: `make golden`, then **read** the golden diff line by line. Every changed line must
   be an intended change; a row that appeared or moved on a state change is a bug.
4. Lua API change: `runtime/types/zemi.lua`, `TestNamespaceVersion` and the `zemi.api.version`
   string move in the same commit; `TestTypes*` and `TestLuaToolDefaultDeny` decide whether a
   new member is annotated and whether it is a pure read (`luaReadOnly`).
5. Docs move with the code: grep AGENTS.md, DESIGN.md, README.md and `runtime/skills/zemi-*`
   for every renamed or removed thing. The skills reach the model, so a stale tool name there
   costs real turns.
6. Work on a branch, then `git merge --ff-only` into main and delete the branch. Never push.
7. Comments say why, never what the diff did. Commit lines are one lowercase sentence.

## Prompt cache

Anything that varies per request in the prefix busts the provider cache for the rest of the
session. Pin per session (`environmentPrompt`, `ag.env`), never per turn: no dates, git
status, map-ordered schemas, or a plan re-injected every step. A profile or tool-set switch
is allowed to bust it once because the user asked. Check by diffing two consecutive
`last_request.json` bodies from the fake server (see the zemi-tui-test skill).

## Tests that will not flake

- Never synchronize on `time.Sleep`; wait on a channel, `waitIdle`, a hook, or an observable
  state. A sleep either fails under load or makes the assertion vacuous.
- Pure timer logic (watchdog, idle model, priority queues) runs under `testing/synctest`
  with a fake clock. It does not fit child processes, ptys, sockets, or a Host booted
  outside the bubble (its Lua actor and `database/sql` goroutines never go idle).
- `t.Parallel()` on slow process-driven tests (LSP, sandbox builds) cut the hostapi package
  from 37 s to 14 s. It is safe only with no `t.Setenv`, no `t.Chdir`, no package globals and
  no shared ports. `TestBashChanges` and `TestLuaAPIIO` cannot go parallel (a 300 ms snapshot
  cutoff and a 1 s interpreter ceiling trip under load).
- Boot-time goroutines (`git status` warm-up, MCP start) must capture what they read by
  value; a test that mutates `h.Dirs` after boot races them.
- A package global written per host (`perm.EnvKeep` once) is a data race between parallel
  hosts; make it a field.
- Always run `-race` on hostapi, ui, perm and lua after touching concurrency.

## Traps seen this month

- Multi-agent work: agents sharing one working tree must not commit concurrently; give each
  a package or run them in sequence. Goldens conflict on every UI merge: take either side
  and run `make golden`.
- An openai_compat model entry without `context_window` gets no auto compaction at all;
  `/health` warns.
- `ZEMI_*_HOME` must be set inside the tmux pane command, or the run hits the real config.
- The secret scrubber's entropy pass cuts long paths and `NAME=url`; recipes use
  `learn.ScrubShapes`, memories keep the full pass.
- `-p` waits 3 s on a non-tty stdin; append `</dev/null`.
- gopher-lua: `a[#a+1], b = b, x` stores `x`; split any assignment whose right side names a
  left-side target. A caught error under `pcall` detaches upvalues of live closures.
- A plain `exec.Command` outside `internal/proc` is a bug except the user's editor and the
  sandbox probes.
