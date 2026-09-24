# zemi

A coding agent for the terminal. It reads and edits your code and runs commands in an OS
sandbox (seatbelt on macOS, Landlock on Linux). Anything the sandbox cannot confine, it
asks about first. Between sessions it keeps notes on how you and the project work.

The UI, keys and commands are written in Lua, the way Neovim does it, so you can change
any of them.

## Install

```sh
go install github.com/metafates/zemi@latest
```

Needs Go 1.27. Export a provider key and run `zemi` inside a project:

```sh
export ANTHROPIC_API_KEY=...   # or OPENAI_API_KEY, GEMINI_API_KEY, DEEPSEEK_API_KEY, ...
zemi
```

`/model` shows every provider it knows and which key each one needs. `/health` checks
that the setup works.

For a local server, or a gateway in front of a known provider, add it in
`~/.config/zemi/init.lua`. The first run writes that file with this block commented out:

```lua
zemi.provider.add({
  id = "local",
  kind = zemi.provider.kind.OPENAI_COMPAT,   -- or ANTHROPIC, OPENAI, GOOGLE
  base_url = "http://localhost:11434/v1",
  models = { { id = "qwen3", context_window = 32000 } },
})
```

## How it behaves

The first time you open a project, zemi asks whether to trust it. A trusted project
loads its `.zemi` config, AGENTS.md and language servers. In an untrusted one, every
write and every command beyond a plain read asks first. `/trust` changes the answer
later.

There are two permission modes, switched with `/mode` or `ctrl+y`:

- `auto` edits the project and runs sandboxed commands on its own. It still asks about
  anything destructive (`rm -rf`, `git commit`), anything that leaves the sandbox, and
  anything that needs the network.
- `yolo` asks for nothing.

`ctrl+t` switches to the `planner` profile, which reads the code and proposes a plan.
Nothing changes until you approve it.

`/rewind` undoes a turn and the files it changed. `/btw` asks a side question that
stays out of the conversation.

Whatever zemi learns about you and the project is kept as memories. `/memory` shows
them for review, and `/memory <note>` adds one by hand.

## Scripts and hooks

```sh
zemi -p "explain main.go"         # answer on stdout, no TUI
git diff | zemi -p "review this"  # stdin is attached to the prompt
```

With `-p` there is nobody to approve a call, so any call that needs approval is denied.
`zemi --help` lists the flags.

## Config

Everything lives in `~/.config/zemi/init.lua`, plain Lua, reloaded when you save it:
providers, keymaps, commands, widgets, the layout, permission rules. If you would rather
not read the API, ask zemi to change the file for you. It knows its own config.

`/help` lists every key and command. [DESIGN.md](DESIGN.md) is the spec,
[AGENTS.md](AGENTS.md) the internals.
