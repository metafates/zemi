# zemi

A coding agent for the terminal. It learns how you work between sessions,
keeps the prompt cheap, and its UI is Lua.

## Install

```sh
go install github.com/metafates/zemi@latest
export ANTHROPIC_API_KEY=...   # or OPENAI_API_KEY, ...
zemi
```

Go 1.27. Local models and gateways go in `~/.config/zemi/init.lua`.

## It learns

When a session closes, zemi reads it back and keeps what would have cost a
question next time: how you test, how you commit, what bit you. A memory about
you must quote you. A procedure it worked out for a repo becomes a recipe it
reuses there.

`/memory` shows what it kept. `/rewind` takes a turn back, files and lessons
included.

## Tokens go to the code

The prompt is pinned per session, so the provider's cache hits every turn.
Skills are an index until one is needed. Past ten MCP tools, they load by
search. Titles, summaries and the learning pass run on a small model.

## On its own, or with a question

| runs on its own             | asks first                       |
| --------------------------- | -------------------------------- |
| reads and edits the project | `rm -rf` and friends             |
| builds, tests, scripts      | `git commit`, anything in `.git` |
| read-only MCP tools         | the network, leaving the sandbox |

Seatbelt on macOS, Landlock on Linux. Your keys and shell history are hidden
from it. `ctrl+y` is yolo. `ctrl+t` is the planner, which changes nothing
until you approve.

## The UI is Lua

```lua
zemi.keymap.set("ctrl+t", "zemi.agent.profile.cycle")
```

Keys, commands, widgets, layout, permission rules: one file, reloaded on save,
like Neovim. Or ask zemi to edit it.

## Scripts and hooks

```sh
zemi -p "explain main.go"
git diff | zemi -p "review this"
```

`/help` lists every key and command. [DESIGN.md](DESIGN.md) is the spec,
[AGENTS.md](AGENTS.md) the internals.
