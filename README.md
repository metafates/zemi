# zemi

A coding agent for the terminal. It learns how you work between sessions, keeps
the prompt cheap, and its UI is Lua.

## Install

```sh
go install github.com/metafates/zemi@latest
export ANTHROPIC_API_KEY=...   # or OPENAI_API_KEY, ...
zemi
```

## It learns

After a session, zemi keeps what would have cost a question next time: how you
test, how you commit. A memory about you must quote you. A procedure it worked
out for a repo becomes a recipe there.

`/memory` shows what it kept. `/rewind` takes a turn back, lessons included.

## Few tokens

The prompt is pinned per session, so the provider cache hits every turn. Skills
are an index until one is used. Past ten MCP tools, they load by search. Titles,
summaries and the learning pass can each run on a cheaper model (`/model roles`).

## On its own, or with a question

| runs on its own             | asks first                       |
| --------------------------- | -------------------------------- |
| reads and edits the project | `rm -rf` and friends             |
| builds, tests, scripts      | `git commit`, anything in `.git` |
| read-only MCP tools         | the network, leaving the sandbox |

Seatbelt on macOS, Landlock on Linux. Keys and shell history are hidden.
`ctrl+y` is yolo. `ctrl+t` is the planner: it reads and proposes, you approve.

## The UI is Lua

```lua
zemi.keymap.set("ctrl+t", "zemi.agent.profile.next")
```

Keys, commands, widgets, layout, permission rules: one file, reloaded on save,
like Neovim. Or ask zemi to edit it.

## Plugins

```sh
zemi plugins install https://github.com/someone/zemi-git-status
```

A plugin is a git repository shaped like the config dir. zemi checks out its
newest `vX.Y.Z` tag, and `zemi plugins update` stays in that major version.
Write your own in `~/.local/share/zemi/plugins/local/<name>/`, or type
`/plugins new <what it should do>` to have the agent write it, and pick
Modify in a local plugin's `/plugins` entry to have it change one. `/plugins`
turns them on and off. `zemi plugins test <name>` runs a plugin's
`tests/*.lua`, each case in a throwaway zemi with a fake clock and no
network or processes.

## Command line

```
zemi [prompt...]                    open the TUI; a prompt is sent first
zemi run [prompt... | -]            one turn, the reply on stdout
zemi sessions [export [--full] <id>]
                                    this project's sessions; one as markdown
zemi models [update]                the ids -m takes; fetch the latest catalog
zemi plugins [install|update|check|enable|disable|delete|test]
zemi health                         the /health checks; 1 if any failed
zemi completion bash|zsh|fish
```

```sh
zemi run "explain main.go"
git diff | zemi run "review this"
zemi run - < task.md                # the prompt itself from stdin
zemi -C ~/code/app run -c "and the tests?"
```

Shell completion, with session ids, plugin names, models, profiles and modes:

```sh
source <(zemi completion bash)                     # in ~/.bashrc
zemi completion zsh > "${fpath[1]}/_zemi"
zemi completion fish > ~/.config/fish/completions/zemi.fish
```

`zemi <command> --help` says more.

`/help` lists every key and command. [DESIGN.md](DESIGN.md) is the spec,
[AGENTS.md](AGENTS.md) the internals.
