# zemi

A terminal coding agent that learns how you work. Go runs the agent loop, the TUI and
the store; everything you see and bind is Lua you can replace, in the spirit of Neovim.

## What it does

- Chats with a model inside your repository and works through tools: `bash`, `read`,
  `write`, `edit`, `multiedit`, `glob`, `grep`, `fetch`, background jobs, todos,
  structured questions, skills and memory.
- Asks before anything risky. Edits show their diff in the permission prompt; `a`
  (always) stores an exact rule, and permanent rules live in a generated
  `permissions.lua` that is versioned with git.
- Lets you steer mid-turn: messages typed while the agent works join the running turn at
  the next step. `up` on an empty composer pulls the queue back for editing, `esc`
  interrupts.
- Keeps sessions: `/new`, `/sessions`, `/resume`. Long sessions compact into a summary
  before the context window fills (`/compact` does it on demand).

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
  models = { { id = "claude-sonnet-4-5", display = "Sonnet 4.5", context_window = 200000 } },
})
zemi.config.set("agent.model", "anthropic/claude-sonnet-4-5")
```

Run `zemi` in your project. `/health` checks the model key, the store, `permissions.lua`,
git and the reflector.

## Keys and commands

| Key | Action |
| --- | --- |
| `enter` / `alt+enter` | send / newline |
| `esc`, `ctrl+c` | interrupt the turn (idle: `ctrl+c` twice quits) |
| `up` on empty composer | edit queued messages |
| `ctrl+o` | expand reasoning |
| `pgup` `pgdown` `ctrl+home` `ctrl+end` | scroll the transcript |
| `/` | command palette (tab completes names and arguments) |
| `@` | attach a project file |

Commands: `/help`, `/model`, `/new`, `/sessions`, `/resume [id]`, `/compact`,
`/remember`, `/memory`, `/config`, `/skills`, `/reflect`, `/health`, `/errors`, `/quit`.

## Lua

The `zemi.*` API mirrors `vim.*`: keymaps, commands with completion, events, widgets,
themes, overlays and pickers, timers, jobs, http, and the learning namespaces.

```lua
zemi.keymap.bind("insert", "ctrl+t", function()
  zemi.ui.select({ "fix", "explain" }, { prompt = "Ask the agent to" }, function(choice)
    if choice then zemi.agent.send(choice .. " the failing test") end
  end)
end, { desc = "quick request" })

local util = require("zemi.util")   -- split, trim, map, filter, inspect, validate, ...
```

Plugins load from `~/.local/share/zemi/site/pack/*/start/*`; a project's `.zemi/init.lua`
loads after a one-time trust prompt. Lua runs sandboxed, and every registration carries
its origin (`user > builtin > plugin > project`), so project code cannot override yours.
[DESIGN.md](DESIGN.md) is the full specification.

## Files

| Path | Contents |
| --- | --- |
| `~/.config/zemi/` | `init.lua`, `permissions.lua`, authored `skills/` (a git repo) |
| `~/.local/share/zemi/` | plugins, learned `skills/` |
| `~/.local/state/zemi/` | SQLite stores: sessions, memories, proposals, trust |

Override with `ZEMI_CONFIG_HOME`, `ZEMI_DATA_HOME`, `ZEMI_STATE_HOME`.

## Development

```sh
go test ./...                    # full suite
go test ./internal/ui -update    # regenerate UI goldens
```

See [AGENTS.md](AGENTS.md) for architecture and invariants.
