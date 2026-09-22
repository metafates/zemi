# zemi

An AI assistant for your terminal that remembers how you work. It edits files, runs
commands, searches your code, and asks before anything risky. Everything you see is Lua
you can change, like Neovim.

## Install

```sh
go install github.com/metafates/zemi@latest
zemi
```

Needs Go 1.27. Export a key (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`,
`DEEPSEEK_API_KEY`, `OPENROUTER_API_KEY`, ...; `/health` lists them) and run `zemi`. A local
server, or a gateway in front of a known provider, goes in `~/.config/zemi/init.lua`:

```lua
zemi.provider.add({
  id = "local",
  kind = zemi.provider.kind.OPENAI_COMPAT,   -- or ANTHROPIC, OPENAI, GOOGLE
  base_url = "http://localhost:11434/v1",
  models = { { id = "qwen3", context_window = 32000 } },
})
```

The starter `init.lua` has the block. `/health` checks your setup. `/help` lists every key and
command.

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
