# zemi

A coding agent for the terminal. It works inside an OS sandbox and asks only
about what the sandbox cannot confine.

## Install

```sh
go install github.com/metafates/zemi@latest
export ANTHROPIC_API_KEY=...   # or OPENAI_API_KEY, ...
zemi
```

Go 1.27. Local models and gateways go in `~/.config/zemi/init.lua`.

## On its own, or with a question

| runs on its own             | asks first                       |
| --------------------------- | -------------------------------- |
| reads and edits the project | `rm -rf` and friends             |
| builds, tests, scripts      | `git commit`, anything in `.git` |
| read-only MCP tools         | the network, leaving the sandbox |

Seatbelt on macOS, Landlock on Linux. Your keys and shell history are hidden
from it. `ctrl+y` is yolo. `ctrl+t` is the planner, which changes nothing
until you approve.

## It remembers

How you run tests, how you write commits, what bit you last week. `/memory`
shows what it kept, `/rewind` takes a turn back, files included.

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
