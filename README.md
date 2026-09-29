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
zemi run [prompt... | -]            one turn: the run on stderr, the answer on stdout
zemi sessions [export [--full] <id>]
                                    this project's sessions; one as markdown
zemi projects                       every project zemi keeps state for, last opened first
zemi models [update]                the ids -m takes; fetch the latest catalog
zemi plugins [install|update|check|enable|disable|delete|test]
zemi health                         the /health checks; 1 if any failed
zemi completion bash|zsh|fish
```

```sh
zemi run "explain main.go"
git diff | zemi run "review this"
zemi run - < task.md                # the prompt itself from stdin, never a /command
zemi run "/goal make check is clean" # turns until it holds; 1 if blocked
zemi -C ~/code/app run -c "and the tests?"
zemi -C "$(zemi projects | fzf | cut -c36-)"   # open a project picked from the list
zemi run -q "write the commit message for the staged diff" | git commit -F -
```

`zemi run` puts the answer on stdout and nothing else: the run's last message,
plain. stderr shows the run as the chat draws it, live on a terminal and as
plain lines in a log; where both streams are one terminal the answer is drawn
once. `-q` keeps stderr to what went wrong, and `--reasoning` shows what ctrl+o
shows. Nobody can approve a call, so one the mode leaves to the user is denied,
and stderr ends with those asks and what would let them through. The first
ctrl+c stops the turn, the second leaves. Exit status: 0 done, 1 the send or
the turn failed, 2 usage, 130 interrupted.

`--json` turns the view off and stdout into one JSON object per line, `done`
always last, so `tail -n1` is the report. A field a line does not carry is left
out, and every string is what the chat row holds:

| type | fields | when |
|---|---|---|
| `start` | `session`, `model`, `profile`, `mode`, `trusted`, `cwd` | once the run's session exists |
| `reasoning` | `text`, `ms` | a thinking block settled |
| `message` | `text` | an assistant message settled, narration or the answer |
| `tool_start` | `id`, `tool`, `input`, `label` | a call started running |
| `tool_end` | `id`, `tool`, `input`, `label`, `status` (`ok`, `err`, `stopped`, `bg`), `output`, `error`, `diff`, `detail` | a call finished |
| `ask` | `tool`, `subject`, `arg`, `reason`, `why` (`approval`, `outside_sandbox`, `network`, `needs_tui`) | an ask nobody could answer was denied, a subagent's too |
| `notice` | `text` | a notice row: a background report, the plan nudge |
| `compact` | `text` | the conversation was compacted or old tool output cleared |
| `retry` | `attempt`, `in_ms`, `error` | the provider call failed and is retried |
| `error` | `text` | the turn ended with an error |
| `done` | `result`, `is_error`, `error`, `session`, `duration_ms`, `usage` (`input_tokens`, `output_tokens`, `cache_read_tokens`, `cost_usd`), `compactions`, `denied` (the `ask` fields), `tools` (`id`, `tool`, `input`, `status`, `error`) | last, a failed boot or send included |

`input` is the argument a person reads first, `label` the model's own words
for a command, `output` the result's last lines, `detail` an agent's or a
workflow's progress. Pair tool events by `id`: a `tool_end` goes out once the
call finishes, while a message or a thought goes out once no provider retry
can take it back, one model step later, so narration can follow the next
call's `tool_start`. A subagent or a workflow is one call; its own calls are
not events.

Shell completion, with session ids, plugin names, models, profiles and modes:

```sh
source <(zemi completion bash)                     # in ~/.bashrc
zemi completion zsh > "${fpath[1]}/_zemi"
zemi completion fish > ~/.config/fish/completions/zemi.fish
```

`zemi <command> --help` says more.

`/help` lists every key and command. [DESIGN.md](DESIGN.md) is the spec,
[AGENTS.md](AGENTS.md) the internals.
