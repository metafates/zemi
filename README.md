# zemi

A terminal coding agent that learns how you work. Go runs the agent loop, the TUI and
the store; everything you see and bind is Lua you can replace, in the spirit of Neovim.

## What it does

- Chats with a model inside your repository and works through tools: `bash`, `read`,
  `write`, `edit`, `multiedit`, `glob`, `grep`, `fetch`, background jobs, todos,
  structured questions, skills and memory.
- Asks before anything risky. Edits show their diff in the permission prompt; `a`
  (always) stores an exact rule, and permanent rules live in a generated
  `permissions.lua` that is versioned with git. `tab` switches the mode shown at the left
  of the statusline: `manual`, `plan` (the agent investigates and proposes; file changes
  are refused and every command asks), `auto` (a model approves routine actions, you get
  the rest) or `yolo`.
- Lets you steer mid-turn: messages typed while the agent works join the running turn at
  the next step. `up` on an empty composer pulls the queue back for editing, `esc`
  interrupts.
- Keeps sessions: `/new`, `/sessions`, `/resume`. Long sessions compact into a summary
  before the context window fills (`/compact` does it on demand).
- Undoes turns: `/rewind` lists your prompts; picking one restores the files the agent
  changed since (through `write`, `edit` and `multiedit`, not bash), drops that part of
  the conversation and puts the prompt back in the composer to edit.

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

`/repro parser.go empty input panics` runs it. Changes load on `/Reload`.

## Subagents and workflows

The model can hand a self-contained task to a subagent with the `agent` tool. A subagent
gets a fresh context and its own tool set, runs in parallel with others, and returns one
report; its tool calls go through the same permission prompts, labeled with the agent's
name. The row under the tool call shows live progress. zemi ships `general` and a
read-only `explore`; add your own as markdown in `~/.config/zemi/agents/` or
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

Workflow scripts run in their own sandbox: `agent`, `parallel`, `phase`, `log`, `json`
and `args` plus Lua's string, table and math libraries, with no file or network access.
`/agents` and `/workflow` list what is installed.

## How it learns

1. **Capture.** `/remember <text>` stores a fact right away. The model can save lessons
   with its `memory_add` tool.
2. **Reflect.** When a session closes (`/new`, `/resume`, or the next start), a reflector
   reads it and proposes memories, permission rule patches and skill drafts. Quitting
   never waits on it.
3. **Review.** Proposals persist until you decide. The statusline shows how many wait:
   `/memory` accepts (`a`) or rejects (`d`) memories, `/config` reviews rule patches,
   `/skills` enables drafted skills (they start disabled).
4. **Use.** Accepted memories reach the model as a labeled digest in the system prompt;
   enabled skills appear as an index the model loads on demand.

Every learned change is scrubbed for secrets, labeled `user` or `learned`, and
reversible (`/memory`, `/config revert <id>`, `/skills disable <name>`).

zemi also knows itself: builtin skills (`zemi-config`, `zemi-debug`, `zemi-lua`,
`zemi-learning`) teach the agent where zemi's files live, how to change its config, how
to read its logs and errors, and how its learning works. Ask it to "set up a provider",
"why did that fail" or "write a statusline widget".

## Install

```sh
go install github.com/metafates/zemi@latest
```

Requires Go 1.27. No CGO. `git` is optional (config changes are versioned when present).

## Configure

zemi ships no providers. If you use [Crush](https://github.com/charmbracelet/crush), its
`~/.config/crush/crush.json` providers are imported automatically. Otherwise register one
in `~/.config/zemi/init.lua`:

```lua
zemi.provider.register({
  id = "anthropic",
  kind = "anthropic",            -- anthropic | openai | openai_compat
  api_key_env = "ANTHROPIC_API_KEY",
  models = {
    { id = "claude-sonnet-4-5", display = "Sonnet 4.5", context_window = 200000,
      cost_per_1m_in = 3, cost_per_1m_out = 15 },   -- optional: session cost in the statusline
  },
})
zemi.config.set("agent.model", "anthropic/claude-sonnet-4-5")
```

Instructions reach the model from `~/.config/zemi/AGENTS.md` (yours, every project), then
`ZEMI.md` and `AGENTS.md` in each directory from the repository root down to where you
started zemi; a directory with neither contributes its `CLAUDE.md`.

Run `zemi` in your project. `/health` checks the model key, the store, `permissions.lua`,
git and the reflector.

## Command line

```sh
zemi                              # TUI
zemi "fix the failing test"       # TUI with a first message
zemi -c                           # continue the last session (-r <id> for another)
zemi -p "explain main.go"         # answer on stdout, no TUI
git diff | zemi -p "review this"  # piped stdin joins the prompt
zemi -p --mode auto -m anthropic/claude-haiku-4-5 "run the tests and fix failures"
```

With `-p` nobody can answer a permission ask, so asks are denied unless `--mode` is
`auto` or `yolo`. Exit status: 0 success, 1 error, 2 usage error, 130 interrupted.
`zemi --help` lists every flag.

## Keys and commands

| Key | Action |
| --- | --- |
| `enter` / `alt+enter` | send / newline |
| `esc`, `ctrl+c` | interrupt the turn (idle: `ctrl+c` twice quits) |
| `up` on empty composer | edit queued messages |
| `tab` / `shift+tab` | next / previous permission mode (in popups tab accepts) |
| `ctrl+o` | expand reasoning |
| `ctrl+k` `ctrl+j` (or `ctrl+p` `ctrl+n`) | move in lists and popups; `space` toggles in `/skills` |
| `pgup` `pgdown` `ctrl+home` `ctrl+end` | scroll the transcript |
| `/` | command palette (tab completes names and arguments) |
| `@` | attach a project file |
| `!cmd` | run a shell command; its output goes to the agent with your next message |

Commands: `/help`, `/model`, `/mode`, `/new`, `/sessions`, `/resume [id]`, `/compact`,
`/rewind`, `/agents`, `/workflow`, `/remember`, `/memory`, `/config`, `/skills`, `/reflect`,
`/health`, `/errors`, `/quit`.

## Lua

The `zemi.*` API mirrors `vim.*`: keymaps, commands with completion, events, widgets,
themes, overlays and pickers, timers, jobs, http, and the learning namespaces.

```lua
zemi.keymap.set("ctrl+t", function()
  zemi.ui.select({ "fix", "explain" }, { prompt = "Ask the agent to" }, function(choice)
    if choice then zemi.agent.send(choice .. " the failing test") end
  end)
end, { desc = "quick request" })

local util = require("zemi.util")   -- split, trim, map, filter, inspect, validate, ...
local keys = require("./keys")      -- relative to this file
local myui = require("myui.status") -- ~/.config/zemi/lua/myui/status.lua
```

Plugins load from `~/.local/share/zemi/site/pack/*/start/*`. A repository can ship its own
setup in `.zemi/` (like `.claude/` or `.crush/`): `.zemi/init.lua`, modules under
`.zemi/lua/`, `agents/`, `commands/` and `workflows/`, found from any subdirectory and loaded after a trust prompt that repeats
whenever those files change. Lua runs sandboxed, and every registration carries its
origin (`user > builtin > plugin > project`), so project code cannot override yours.
[DESIGN.md](DESIGN.md) is the full specification.

## Files

| Path | Contents |
| --- | --- |
| `~/.config/zemi/` | `init.lua`, `permissions.lua`, authored `skills/`, `agents/`, `commands/`, `workflows/` (a git repo) |
| `~/.local/share/zemi/` | plugins, learned `skills/` |
| `~/.local/state/zemi/` | SQLite stores: sessions, memories, proposals, trust |

Override with `ZEMI_CONFIG_HOME`, `ZEMI_DATA_HOME`, `ZEMI_STATE_HOME`.

## Development

```sh
go test ./...                    # full suite
go test ./internal/ui -update    # regenerate UI goldens
```

See [AGENTS.md](AGENTS.md) for architecture and invariants.
