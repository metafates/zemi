# AGENTS.md - zemi

zemi is a terminal AI agent harness whose userland (keymaps, the chat's summary builder surface)
is written in embedded Lua while Go owns the runtime and the screen (internal/ui): the agent loop
over
`charm.land/fantasy`, the bubbletea v2 TUI, the sandboxed gopher-lua interpreter, and SQLite
persistence. `DESIGN.md` is the spec; code comments cite its sections (`DESIGN §4`, `§6.2`).
**Before changing behavior, read the DESIGN section the code cites** - it explains the why
(usually a security or concurrency invariant), and a change that contradicts it is almost
always wrong. When spec and tree disagree, the tree is the truth.

Prior art with local checkouts under `~/Code/github/` (study, don't copy): `crush` (same Go
stack; tool descriptions, permission flow and store patterns trace back to it), `pi` (prompt
wording), `codex` and `opencode` (tool behavior), `neovim`
(the model for the Lua userland: keymap, health and loader patterns).

## House rules

These are the owner's rules for anyone changing this repo, human or agent.

Every new line must be deserved, and the best change deletes code. Before adding anything: does
it need to exist, does the tree already have it, does the stdlib or an installed dependency do
it. No abstractions nobody asked for, no speculative options, fewest files. A report names a
symptom: find the root cause and fix the class it belongs to in the shared primitive, never the
one case per screen or per caller.

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
non-trivial logic leaves exactly one runnable check, and security assertions are never
weakened. An existing expectation is a contract: a fix that must flip a test's `want` or
regenerate a golden is a behavior change, and DESIGN.md or the user names the new behavior
before it lands — a polish-round finding that would flip one is a question for the user,
not a fix.

Fast is a feature. Nothing slow sits between submit and the provider request, an idle zemi
redraws nothing, and there are no per-frame Lua or allocation hot spots. Cosmetics never cost a
model call: the model supplies its own labels, like a shell call's `about`, as it streams the
call.

The permission system is a UX feature, not isolation: everything runs, and a
plugin's `permission.request` handler (the `perm` word in its `uses`) may deny
or force the ask overlay per action. It never parses a command to judge it,
and it is never a security boundary - a run that must not touch the machine
runs in a container or VM. An ask no one can answer fails closed.

The UI has one identity in every theme: neutrals from its fg and bg, one accent (whorl's
periwinkle), one literal for text quoted as written (whorl's apricot), the chat on the canvas but
the user's prompt on the panel, every box and the composer on the panel, square corners joined
where two lines touch, and the `@` snail whose state lives in its eyes, plain eye color in every state.
Widgets use the semantic theme groups (`Accent`, `Dim`, `Rule`, `Title`, `Literal`, `Danger`,
`Warn`, `Ok`); every other group links to one or names a palette role. No state change may
add or remove a row or shift text. Hover in the chat is minimal: body rows get none, a header
only changes foreground. A popup covers only its own cells. Polished UI must be easy to build
from Lua and ugly UI hard, so what Go draws comes from the layout and widget API rather than
hardcoded strings.

Docs follow the code in the same change: the AGENTS.md, DESIGN.md, `.agents/skills` and
`runtime/skills/zemi` lines a change makes wrong are fixed with it. `make check` must be clean,
and goldens are regenerated with `make golden` and the diff read. Commits are small, one
lowercase line saying what changed, no prefixes, no body unless the why is not obvious. Never
push unless asked.

Workers in git worktrees: worktree isolation cuts from main, so land prior work on main first.
Goldens conflict on every UI merge; take either side and run `make golden`.

No vendoring, no generated code.

## Hard rules

Each of these has a failure mode, not a style preference; the skill in brackets has the why.

- Only the Lua actor goroutine touches the `lua.LState`; everything else goes through the queue in `internal/lua`. [zemi-lua-runtime]
- No bare `go func()` in hostapi: start one with `h.GoSafe(where, fn)`, and a turn goroutine also defers `h.endTurnAfterPanic()`. [zemi-lua-runtime]
- All `ui.App` state mutates on the bubbletea Update goroutine, and nothing `Update` calls may call `program.Send`/`program.Quit` (bubbletea's unbuffered channel makes the loop wait on itself: the `/quit` hang). Blocking hooks run off the UI goroutine only. [zemi-ui]
- Never keep a pointer into an old generation: every registration is `Gen`-tagged and purged before the replay. [zemi-lua-runtime]
- A Lua handler on the UI path stays under ~10 ms; a vetoable event's handler has 50 ms and then takes the event's logged default (for `permission.request`, silence: the request runs unless the overlay denies it; the hot runtime still denies). [zemi-lua-runtime]
- An ask with no overlay available fails closed, and a rewritten tool input re-enters at the permission gate. [zemi-perm]
- A child process starts through `internal/proc`, never a plain `exec.Command`. [zemi-perm]
- The store schema is embedded goose migrations run on open: a change is a new numbered file under `internal/store/migrations`, applied at the next boot, and no version refusal. [zemi-host-flow]

## Commands

A Makefile wraps the plain Go toolchain (module `github.com/metafates/zemi`, Go 1.27). No CI;
`make check` is what a change has to pass:

```
make check              # gofmt, stylua, shellcheck, go vet (host and windows), make luals, make lint, then go test - fast failures first, tests last, all must be clean
make luals              # lua-language-server over runtime/; prints a skip line without the binary
                        # not on PATH? make check LUALS=/path/to/lua-language-server
make stylua             # stylua --check over runtime/ and testdata/ (stylua.toml: tabs); a skip line without the binary
make shellcheck         # shellcheck over tracked scripts with a sh shebang; a skip line without the binary
make lint               # golangci-lint run (.golangci.yml: curated linters and the gofumpt, goimports, gci formatters); a skip line without the binary
make build / make run   # go build -o bin/zemi . / run the TUI against your real ~/.config/zemi
make site               # serve the one-page site in docs/ at localhost:1314
make first-run          # zemi as a newcomer sees it: fresh HOME and repo, deleted on exit; KEYS=0 without keys, DIR=<abs path>, ARGS=<flags>
make test               # full suite (~20s, pure Go, no CGO; network only for one macOS TLS check)
make race               # go test -race ./...
make flake              # the suite as a loaded machine sees it: -race, three repeats; the flake hunter
make golden             # regenerate internal/ui/testdata/*.golden, then read the diff
make fmt                # golangci-lint fmt over the Go files (gofmt without the binary), and stylua over the runtime/ and testdata/ Lua
```

For a throwaway config tree, redirect all dirs: `ZEMI_CONFIG_HOME`, `ZEMI_DATA_HOME`,
`ZEMI_STATE_HOME`, `ZEMI_CACHE_HOME` (internal/config/dirs.go; a relative one stops the boot,
where a relative `XDG_*` is ignored as the spec says), and `HOME` too when the
skills in `~/.agents/skills`, which no override moves, must stay out (`make first-run` is that
with a fresh `HOME`, for looking at a first run); most tests inject
`config.Dirs{...}` with `t.TempDir()` instead. Tests boot with empty dirs and an untrusted
project: call `h.SetProjectTrusted(true)` when one needs project instructions, language servers
or settled builds. `make check` runs each test once, so a scheduling flake slips it: a change
that waits on goroutines - concurrency, timers, turn or step ordering, and the tests for them
- runs `make flake` (-race, three repeats) before it lands.

## Where the rest lives

The flow, the invariants and the gotchas behind these commands are this repo's own skills under
`.agents/skills/`, one per area: the permission gate and the process layer; the Lua runtime and
its types; host flow, the CLI, boot and trust, config, turns and the chat's builder; subagents, workflows,
MCP and LSP; the TUI. Their triggers are in the skills index in your context - load the matching
one before changing that area. `zemi-dev-lessons` and `zemi-tui-test` hold the traps and how to
drive the binary.
