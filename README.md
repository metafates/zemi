# zemi

A terminal coding agent that learns how you work. Go runs the agent loop, the TUI and
the store; everything you see and bind is Lua you can replace, in the spirit of Neovim.

## What it does

- Chats with a model inside your repository and works through tools: `bash`, `read`,
  `write`, `edit`, `multiedit`, `glob`, `grep`, `fetch`, background jobs, todos,
  structured questions, skills and memory.
- Asks before anything risky. Edits show their diff in the permission prompt; `a`
  (always) allows the same kind of action until zemi exits, and rules in `init.lua` apply
  on every start. `tab` switches the mode shown at the left
  of the statusline: `manual`, `plan` (the agent investigates and proposes; file changes
  are refused and every command asks), `auto` (a model approves routine actions, you get
  the rest) or `yolo`.
- Taps you on the shoulder: when the terminal is not focused, a finished turn or a
  waiting permission prompt sends a desktop notification (OSC 9) and a bell
  (`ui.notify = false` turns it off).
- Lets you steer mid-turn: messages typed while the agent works join the running turn at
  the next step. `up` on an empty composer pulls the queue back for editing, `esc`
  interrupts.
- Keeps sessions: `/new`, `/sessions`, `/resume`, `/fork` (try another direction without
  losing this one), `/export` (markdown). Long sessions compact into a summary
  before the context window fills (`/compact` does it on demand).
- Undoes turns: `/rewind` lists your prompts; picking one restores the files the agent
  changed since (through `write`, `edit` and `multiedit`, not bash), drops that part of
  the conversation and puts the prompt back in the composer to edit.

## Language servers

After `write`, `edit` or `multiedit` changes a file, the language server's errors and
warnings for it are appended to the result, so the model sees a broken build right away;
the `diagnostics` tool checks any other file, and the `lsp` tool asks the server for a
symbol's definition, its references, a semantic rename or a file's outline. gopls, pyright, rust-analyzer,
typescript-language-server and clangd start on their own when installed and the project
has their marker file (`go.mod`, `pyproject.toml`, `Cargo.toml`, `package.json`, ...), in
projects you trust (servers run the project's build scripts).
Add others in `init.lua`, configure a builtin by adding its name, or disable one; `/lsp`
shows what the project can use:

```lua
zemi.lsp.add("zls", { command = "zls", filetypes = { "zig" } })
zemi.lsp.add("gopls", { command = "gopls", filetypes = { "go" }, settings = { gopls = { staticcheck = true } } })
zemi.lsp.disable("clangd")
```

## MCP servers

Tools from [MCP](https://modelcontextprotocol.io) servers join the model's tool set as
`mcp_<server>_<tool>`. Add servers in `init.lua`:

```lua
zemi.mcp.add("github", {
  command = "github-mcp-server", args = { "stdio" },
  env = { GITHUB_PERSONAL_ACCESS_TOKEN = "$GITHUB_TOKEN" },  -- expanded from your environment
})
zemi.mcp.add("docs", { url = "https://mcp.example.com/mcp", disabled_tools = { "delete_page" } })
```

Each call asks first (subject `mcp.<server>`, the tool name as argument, so "always"
approves that tool). `/mcp` shows every server's state; enter on one offers restart and sign-in.

## Prompt commands

A markdown file in `~/.config/zemi/commands/` or `.zemi/commands/` becomes a slash
command that sends its body to the agent. `$ARGUMENTS` is everything typed after the
name, `$1` to `$9` are single words:

```markdown
---
description: Write a test that reproduces a bug
argument-hint: <file> <what breaks>
---
Read $1 and write a failing test that shows: $ARGUMENTS. Do not fix the bug yet.
```

`/repro parser.go empty input panics` runs it. Changes load on `/reload`.

## Subagents and workflows

The model can hand a self-contained task to a subagent with the `agent` tool. A subagent
gets a fresh context and its own tool set, runs in parallel with others, and returns one
report; its tool calls go through the same permission prompts, labeled with the agent's
name. The row under the tool call shows live progress. The model can also start one in the
background: it keeps working (or goes idle) and the report arrives as a message when the
agent is done; `/agents` lists them and stops one. zemi ships `general`, a
read-only `explore` and `web` for research on the web; add your own as markdown in `~/.config/zemi/agents/` or
`.zemi/agents/`:

```markdown
---
description: Reviews a diff for correctness bugs and reports them with file:line
tools: read, grep, glob, bash
model: anthropic/claude-sonnet-4-5    # optional, default is the session model
---
You are a strict reviewer. Report only defects you can point to in the code.
```

For work shaped like several agents with logic between them, the model writes a
workflow: a short Lua script that runs agents in sequence or with `parallel`, and returns
the combined result. You see the script in the permission prompt before it runs. Save
workflows you reuse as `workflows/<name>.lua` (the leading `--` comment is the
description) and run them with `/workflow <name> [args]`, or from Lua:

```lua
-- workflows/review.lua: review the current diff from three angles, then verify
local jobs = {}
for _, lens in ipairs({ "correctness", "security", "simplicity" }) do
  jobs[#jobs + 1] = function()
    return agent("Review `git diff` for " .. lens .. " problems, with file:line.", { agent = "explore" })
  end
end
phase("verify")
return agent("Keep only the findings that hold up against the code:\n" .. table.concat(parallel(jobs), "\n\n"))
```

zemi ships one: `/review [git ref]` has an agent collect the changes, three read-only
reviewers look for correctness, security and simplicity problems in parallel, and a
final agent keeps only the findings that hold up in the code.

Workflow scripts run in their own sandbox: `agent`, `parallel`, `phase`, `log`, `json`
and `args` plus Lua's string, table and math libraries, with no file or network access.
`/agents` and `/workflow` list what is installed.

## How it learns

1. **Capture.** `/memory <text>` stores a fact right away. The model can save lessons
   with its `memory_add` tool.
2. **Reflect.** When a session closes (`/new`, `/resume`, or the next start), a reflector
   reads it and proposes memories and skill drafts. Quitting never waits on it.
3. **Review.** Proposals persist until you decide. The statusline shows how many wait:
   `/memory` accepts (`a`) or rejects (`d`) memories, `/skills` enables drafted skills
   (they start disabled).
4. **Use.** Accepted memories reach the model as a labeled digest in the system prompt;
   enabled skills appear as an index the model loads on demand.

Every learned change is scrubbed for secrets, labeled `user` or `learned`, and
reversible (`/memory`, `/skills disable <name>`).

zemi also knows itself: builtin skills (`zemi-config`, `zemi-customize`, `zemi-debug`,
`zemi-lua`, `zemi-learning`, `zemi-worktrees`) teach the agent where zemi's files live, how
to change its config, how to restyle the UI and add workflows, how to read its logs and
errors, how its learning works, and how to run parallel work in git worktrees. Ask it to
"set up a provider", "why did that fail" or "write a statusline widget".

Programs can run inside zemi's screen. The agent's `window` tool (and `zemi.win` from Lua)
starts any command in a real terminal drawn as a split below the chat, a full-height sidebar
beside it, or a float over it: the TUI it just built, a dev server, a test watcher, a REPL.
It keeps working while the program runs, reads the screen back as text, presses keys in
neovim notation, clicks and scrolls, changes a window's kind, size and position, hides it
and shows it again, waits for it to exit or closes it, and hears when the program exits on
its own. Subagents get the same tool; their windows stay hidden until shown. Click a window
to type into it yourself, drag a float by its title, resize it by its border or a corner, a
sidebar by its separator, a split by its title; `ctrl+\` brings your keys back, `/win` lists, focuses, hides, shows and closes.
It is a pure Go terminal emulator, no cgo.

Your editor is part of the loop: `ctrl+g` edits the draft in it, `zemi.ui.edit(path)` opens a
file from Lua, and the agent's `open_in_editor` tool hands you a file when only you can make
the change and continues with the diff once you close it. The editor opens in a focused
window over zemi, sized to the screen, and the window closes when the editor exits.
`editor.command` overrides `$VISUAL` and `$EDITOR`.

It can also see its own screen: `zemi.ui.screenshot()` returns the drawn frame as plain
text, so the agent debugs a widget or a theme by looking instead of asking you what
changed.

## Install

```sh
go install github.com/metafates/zemi@latest
```

Requires Go 1.27. No CGO. `git` is optional (without it the prompt has no git status), and
so is `ripgrep` (`glob` and `grep` use `rg` when installed and fall back to a Go walker).

## Configure

zemi ships no providers. Register one in `~/.config/zemi/init.lua`:

```lua
zemi.provider.add({
  id = "anthropic",
  kind = zemi.provider.kind.ANTHROPIC,            -- or OPENAI, OPENAI_COMPAT
  api_key = zemi.env.get("ANTHROPIC_API_KEY"),
  -- base_url = "http://localhost:11434/v1",     -- required for OPENAI_COMPAT
  models = {
    { id = "claude-sonnet-4-5", display = "Sonnet 4.5", context_window = 200000,
      cost_per_1m_in = 3, cost_per_1m_out = 15 },   -- optional: session cost in the statusline
  },
})
zemi.config.set("agent.model", "anthropic/claude-sonnet-4-5")
```

Instructions reach the model from `~/.config/zemi/AGENTS.md` (yours, every project), then
`ZEMI.md` and `AGENTS.md` in each directory from the repository root down to where you
started zemi; a directory with neither contributes its `CLAUDE.md`. A trusted project's skills
in `.zemi/skills`, `.agents/skills` or `.claude/skills` join the skill index.

Run `zemi` in your project. `/health` checks the model key, the store,
git, ripgrep and the reflector; `zemi --health` runs the same checks without the TUI.

## Command line

```sh
zemi                              # TUI
zemi "fix the failing test"       # TUI with a first message
zemi -c                           # continue the last session (-r <id> for another)
zemi -p "explain main.go"         # answer on stdout, no TUI
git diff | zemi -p "review this"  # piped stdin joins the prompt
zemi -p --trust --mode auto -m anthropic/claude-haiku-4-5 "run the tests and fix failures"
zemi --health                     # run the /health checks, exit 1 if any failed
```

With `-p` nobody can answer a permission ask, so asks are denied unless `--mode` is
`auto` or `yolo`; a project you have not trusted in the TUI needs `--trust` for auto mode
to run builds and tests. Exit status: 0 success, 1 error, 2 usage error, 130 interrupted.
`--output json` prints the reply, the tool calls, token usage and cost as one JSON object
for scripts. `zemi --help` lists every flag.

## Keys and commands

| Key | Action |
| --- | --- |
| `enter` / `alt+enter`, `ctrl+j` | send / newline |
| `alt+left` `alt+right` (or `alt+b` `alt+f`), `ctrl+z` | move by word, undo in the composer |
| `ctrl+g` | edit the draft in your editor (unless `<leader>` bindings claim it) |
| `esc`, `ctrl+c` | interrupt the turn (idle: `ctrl+c` twice quits) |
| `up` on empty composer | edit queued messages |
| `tab` / `shift+tab` | next / previous permission mode (in popups tab accepts) |
| `ctrl+o` | expand reasoning and every tool call (a click opens one tool row) |
| `ctrl+k` `ctrl+j` (or `ctrl+p` `ctrl+n`) | move in lists and popups; `space` toggles in `/skills` |
| typing in a list | filters it (`/sessions`, `/agents`, `/mcp`, pickers); `esc` clears, then closes |
| `pgup` `pgdown` `ctrl+home` `ctrl+end` | scroll the transcript |
| `/` | command palette (tab completes names and arguments) |
| `@` | attach a project file (images go to vision models as images); `enter` accepts the highlighted one |
| `!cmd` | run a shell command; its output goes to the agent with your next message |

Commands: `/help`, `/model`, `/mode`, `/new`, `/sessions`, `/resume [id]`, `/compact`,
`/effort`, `/usage`, `/prompt`, `/init`, `/review`, `/rewind`, `/fork`, `/export`, `/agents`, `/workflow`, `/mcp`, `/lsp`, `/memory`, `/skills`, `/reflect`,
`/health`, `/errors`, `/messages`, `/quit`.

## Lua

The `zemi.*` API mirrors `vim.*`: keymaps, commands with completion, events, widgets,
themes, overlays and pickers, timers, jobs, http, and the learning namespaces.

```lua
zemi.keymap.set("<leader>r", "review")   -- ctrl+g then r (keymap.leader)
zemi.keymap.set("ctrl+t", function()
  zemi.ui.select({ "fix", "explain" }, { prompt = "Ask the agent to" }, function(choice)
    if choice then zemi.agent.send(choice .. " the failing test") end
  end)
end, { desc = "quick request" })

local util = require("zemi.util")   -- split, trim, map, filter, inspect, validate, ...
local keys = require("./keys")      -- relative to this file
local myui = require("myui.status") -- ~/.config/zemi/lua/myui/status.lua
```

Plugins load from `~/.local/share/zemi/site/pack/*/start/*/plugin/*.lua`, with their modules under `lua/` next to it. A repository can ship its own
setup in `.zemi/` (like `.claude/` or `.crush/`): `.zemi/init.lua`, modules under
`.zemi/lua/`, `agents/`, `commands/` and `workflows/`, found from any subdirectory and loaded
only in a project you trust. zemi asks once per repository (`/trust` asks again, `--trust`
decides one run); an untrusted one also gets no AGENTS.md or language servers, and auto
mode runs only commands that just read there. Lua runs sandboxed, and every registration carries its
origin (`user > builtin > plugin > project`), so project code cannot override yours.
[DESIGN.md](DESIGN.md) is the full specification.

## Files

| Path | Contents |
| --- | --- |
| `~/.config/zemi/` | `init.lua`, `.luarc.json`, authored `skills/`, `agents/`, `commands/`, `workflows/` |
| `~/.local/share/zemi/` | plugins, learned `skills/`, `types/` (LuaLS annotations for `zemi.*`) |
| `~/.local/state/zemi/` | SQLite stores: sessions, memories, proposals, trust |
| `~/.cache/zemi/` | sandboxed language servers' caches and build output |

Override with `ZEMI_CONFIG_HOME`, `ZEMI_DATA_HOME`, `ZEMI_STATE_HOME`, `ZEMI_CACHE_HOME`.

## Development

```sh
go test ./...                    # full suite
go test ./internal/ui -update    # regenerate UI goldens
```

See [AGENTS.md](AGENTS.md) for architecture and invariants.
