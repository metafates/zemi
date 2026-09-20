# zemi

An AI assistant for your terminal that remembers how you work. It edits files, runs
commands, searches your code, and asks before anything risky. Everything you see is Lua
you can change, like Neovim.

## Install

```sh
go install github.com/metafates/zemi@latest
zemi
```

Needs Go 1.27. Add an AI provider in `~/.config/zemi/init.lua` (zemi ships none):

```lua
zemi.provider.add({
  id = "anthropic",
  kind = zemi.provider.kind.ANTHROPIC,            -- or OPENAI, OPENAI_COMPAT
  api_key = zemi.env.get("ANTHROPIC_API_KEY"),
  models = { { id = "claude-sonnet-4-5", context_window = 200000 } },
})
zemi.config.set("models.main", "anthropic/claude-sonnet-4-5")
```

`/health` checks your setup. `/help` lists every key and command.

## The basics

- `tab` cycles the safety mode at the left of the bar: `auto` (default, runs safe things
  on its own), `plan` (looks around and suggests, changes nothing), `yolo` (`ctrl+y`, no
  questions).
- It asks once per project before trusting it; `/trust` or `--trust` decide.
- `/memory` saves facts, `/rewind` undoes a turn, `/compact` summarizes long chats.
- Long chats learn into a summary; skills and memories carry over between sessions.

## More

See [DESIGN.md](DESIGN.md) for the full spec, [AGENTS.md](AGENTS.md) for internals, and
`/help` in the app for keys and commands including `/model`, `/sessions`, `/resume`,
`/agents`, `/workflow`, `/mcp`, `/lsp`, `/skills`, `/export`, `/review`.
