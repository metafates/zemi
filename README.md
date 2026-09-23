# zemi

A terminal coding agent that learns how you work. It reads and edits your code, runs
commands in a sandbox, and asks before anything that leaves it. The layout, widgets,
keys and commands are Lua you can change, like Neovim.

## Install

```sh
go install github.com/metafates/zemi@latest
```

Needs Go 1.27. Export a key (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`,
`DEEPSEEK_API_KEY`, `OPENROUTER_API_KEY`, ...; `/model` lists them) and run `zemi` in a
project. A local server, or a gateway in front of a known provider, goes in
`~/.config/zemi/init.lua`; the starter file there has the block:

```lua
zemi.provider.add({
  id = "local",
  kind = zemi.provider.kind.OPENAI_COMPAT,   -- or ANTHROPIC, OPENAI, GOOGLE
  base_url = "http://localhost:11434/v1",
  models = { { id = "qwen3", context_window = 32000 } },
})
```

`/health` checks the setup. To change anything else, ask zemi: it knows its own config.

## Use

- zemi asks once per project whether to trust it. Trusted, the project's `.zemi` config,
  AGENTS.md and language servers load; untrusted, auto mode asks before anything but reads.
  `/trust` changes the answer.
- `/mode` switches the permission mode: `auto` edits the project and runs sandboxed
  commands on its own and asks for the rest, `yolo` asks for nothing. `ctrl+y` toggles yolo.
- `ctrl+t` switches to the `planner` profile: it reads and proposes a plan, and nothing
  changes until you approve it.
- `/rewind` undoes a turn and its file changes. `/btw` asks a side question.
- After a session zemi keeps what it learned about you and the project as memories.
  `/memory` reviews them, `/memory <note>` adds one.
- `zemi -p "prompt"` answers on stdout for scripts and git hooks; `zemi --help` lists the flags.

`/help` lists every key and command. [DESIGN.md](DESIGN.md) is the spec,
[AGENTS.md](AGENTS.md) the internals.
