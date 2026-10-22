# Configuring zemi

Everything here goes in the user's init.lua (`<config>/init.lua`, run at startup as the trusted `user` origin) unless it says otherwise.

## Providers and models

The builtin providers (anthropic, openai, gemini, deepseek, openrouter, ...) are on while their key is exported in the shell that starts zemi (`ANTHROPIC_API_KEY`, ...); `/model` lists them and names the variable a missing one needs. Their models come from the catalog bundled with zemi; `/model update` (or `zemi models update` in a shell; `zemi models` lists the ids) fetches the latest from catwalk.charm.land and keeps it for later starts, until a zemi upgrade brings a newer bundle; the `/health` catalog row says which is in use. A gateway, a local server or a provider zemi does not know:

```lua
zemi.provider.add({
	id = "gw", -- a builtin id replaces that provider whole, key included
	kind = zemi.provider.kind.OPENAI_COMPAT, -- or ANTHROPIC, GOOGLE, OPENAI (gpt-4 and later over /responses), OPENROUTER, VERCEL, SYSTEM_ONE (a classifier endpoint)
	base_url = "https://llm.internal/v1", -- required for OPENAI_COMPAT and SYSTEM_ONE, refused for OPENROUTER
	api_key = zemi.env.get("GATEWAY_KEY"),
	-- headers = { ["X-Team"] = "core" }, -- as secret as the key
	-- body = { service_tier = "flex" }, -- extra request fields; not sent by GOOGLE
	-- object_mode = zemi.provider.object_mode.TOOL, -- how a schema'd ask is put to it: auto, json, tool or text
	models = {
		{ id = "deepseek-chat", context_window = 128000, cost_per_1m_in = 0.27, cost_per_1m_out = 1.1 },
	},
})
zemi.config.set("models.chat", "gw/deepseek-chat")
zemi.provider.del("openai") -- drop one builtin; providers.builtin = false drops them all
```

A longer prompt cache is an opt-in `body` entry, never a default since model support varies: `body = { prompt_cache_retention = "24h" }` on an `OPENAI` provider keeps OpenAI's cache for a day on the models that accept it.

Model fields besides `id` and `display`:

- `context_window`: without it there is no context percent and no auto compaction, only `/compact` and a provider's too-long refusal.
- `cost_per_1m_in`, `cost_per_1m_out`, `cost_per_1m_cache_write` (tokens written to the prompt cache), `cost_per_1m_cache_read` (tokens read from it). Without the cache pair, cached turns read cheaper than they were.
- `max_output`: reply cap in tokens; unset, anthropic models get 32000, others the provider default.
- `vision = false`: the model takes no images: read's description tells it so, read refuses image files, and images leave its history. Without it zemi learns that from the provider's first refusal, for the process.
- `decisions = true`: a `SYSTEM_ONE` provider's model that answers `zemi.classify.ask` itself, through that provider's own protocol, with real probabilities. A decisions model is no chat model: `/model` offers it to the slots `zemi.model.add(name, { classify = true })` made, and nothing else takes one.

Any other key, on the provider or on a model entry, is refused as an unknown field.

`models.favorites` is the list of model ids `/model` lists first; `f` marks the selected model there and writes this key.

Model targets: `models.chat` runs the conversation, `/btw`, the goal check, the idle recap and every target left empty; `models.agents` (subagents whose definition names no model), `models.titles` (session titles: a session is named from the user's first message at once, and this call replaces that name in the background), `models.compact` (compaction summaries) and `models.memory` (the reflector) each take a `provider/model` or `""` to fall back.
A definition is a target as well: `models.<profile>` overrides the `model` a profile was defined with, and `zemi.model.add(name, { desc, model, classify = true })` adds a slot of its own for code that runs a model on a job rather than a conversation (`zemi.model.del` drops it); with `classify = true` it is a classifier slot, and `/model <name>` offers it the decisions models beside the chat ones. A target's own model names a builtin role (resolved on every read, so `model = "titles"` follows `models.titles` set or picked later) or a `provider/model`; its own name, `default` and other targets are refused, so targets never nest. An unset chat runs the first added provider's default model, else the first usable builtin's. Nothing falls back to a cheaper model on its own: set titles and memory to one to save tokens. Compact on the chat model is asked after the conversation itself, so it reads the prompt cache; a cheaper compact model gets a flat transcript instead. Set a target with `zemi.config.set("models.titles", "gw/deepseek-chat")`, or `/model roles` (the list; enter opens a target's models), `/model <target> <provider/model>`, or `/model <target> chat` to leave it on its own model, else chat. A bare `/model <provider/model>`, `-m` and `zemi.model.set` write the conversation's slot: the active profile's own when it has one, else `models.chat`; `/model chat <id>` picks that key explicitly. Agent files, profiles and workflows take a target name or a `provider/model`.

## Settings

`zemi.config.set(path, value)` in init.lua. `zemi.config.schema()` returns every key with kind, default, description and, for a key with a fixed list, its `choices`; read it with the Lua tool instead of guessing. A value of the wrong kind is refused: the key keeps its default and /errors names the refusal, and an unknown key raises. A runtime pick (`/model`, `/effort`, `/profile`, the mode, `/config`) survives a reload until init.lua changes that key, and a restart unless init.lua sets that key: zemi keeps picks per user in its own store, never in init.lua. So a user who wants a value in every start either just picks it, or, to pin it against later picks, sets it in init.lua. `/config` opens on its sections: enter (or `l`) falls into one and shows its keys with where each came from (`init.lua`, `saved` for a kept pick, `default`, or `this run` for what a restart drops) and its value, a bool's as a switch: space toggles a bool and enter opens a key's detail to change it (a switch, a list, or a prompt holding the value, which sets the key and returns to /config, a refused value staying in the prompt with the error it named) or reset it to init.lua's value or the default, which forgets the kept pick, `/config set <key> <value>` sets one, and `/config lua` shows every key off its default as `zemi.config.set` lines to copy. A plugin's settings are `plugins.<name>.<key>` keys it registers: /config lists them under Plugins and they layer like any other, and a line for a plugin that is off does nothing. The keys that need more than the schema says:

- `agent.profile`: `default`, or a profile defined with `zemi.agent.profile.add` (`agents.md`); `/profile` or ctrl+t switches.
- `agent.effort`: `low`, `medium`, `high` (`zemi.model.effort.HIGH`, ...), or empty (or `default`) for the model's default; `/effort` opens the levels with the current pick keyed and what each sends on the model. A builtin catalog model runs at its catalog default when unset, else the nearest level it lists, and gets none when it lists none; any other model gets the pick, or no effort when unset.
- `memory.autonomy`: `auto` stores policy-clean memories at once, `ask` queues every one for `/memory` (`memory.md`).
- `ui.notify`: how zemi alerts while its window is unfocused: `osc` (OSC 9, which iTerm2, Ghostty, WezTerm and kitty show; tmux, screen and zellij drop it without passthrough), `native` (osascript on macOS, notify-send on Linux; a bell when neither is found), `bell` or `off`. `auto` picks osc for a terminal known to show it outside those multiplexers, else native on this machine, else a bell; the `/health` notify row names the pick and why.
- `editor.command`: the editor for ctrl+g and `zemi.ui.edit`; empty uses `$VISUAL`, then `$EDITOR`, then vi.
- `keymap.leader`: what `<leader>` stands for in keymaps (`ui.md`).
- `lsp.auto`: under MCP and language servers.
- `log.level`: what the launch log file records, `debug` (default), `info`, `warn` or `error`; one file per start under `~/.local/state/zemi/logs`, newest 20 kept (`/logs` names the current one).

- `agent.compact_at`: the percent of the model's context window auto compaction waits for (80, never under half the window); `100` leaves only `/compact` and a provider's too-long refusal. Every model divides by its own `context_window`.
- `agent.compact_idle`: a duration; when set, /compact's summary runs on its own once the session has sat idle this long after a turn, so a long session you left stays lean.
- `agent.max_background`, `session.auto_title`, `session.recap` (a duration; `0` turns the recap off), `ui.theme` and `ui.mouse`: the schema's description covers them.

## Permissions

There are no permission modes and no rules: model tool calls run, and the user stays aware
through the ask overlay, which only a plugin opens - a `permission.request` handler returning
`{ decision = "ask" }`, or its own `zemi.perm.ask` - with `perm` in the manifest's `uses`
(`plugins.md`). The permission system keeps the user aware of what the agent does; it is not
isolation. A run that must not touch the machine goes in a container or a VM.

At a prompt `y` allows once, `n` denies, and `t` denies and stops the turn so the user can say what to do instead.

Model tool names, for the `tools` of a profile or agent: `shell`, `read`, `write`, `edit`, `glob`, `grep`, `todos`, `question`, `plan`, `job_start`, `job_read`, `job_list`, `job_kill`, `memory`, `skill`, `diagnostics`, `lsp`, `agent`, `workflow`, `lua`, `update` (subagent definitions only, and not a fork's: see `agents.md`), `mcp_<server>_<tool>`, `tool_search` (past ten MCP tools their schemas leave the request and the model loads them through it) and tools added from Lua.

Planning is a read-only profile: with `read_only = true` every action is denied - writes, commands, Lua tools - and the turn ends in a plan the user approves (implemented in the next turn), asks changes to, or saves to start later. Nothing ships one: define it with `zemi.agent.profile.add("planner", { read_only = true })` in `init.lua` (a plugin may too, `plugins.md`), then `/profile planner` or ctrl+t.

## MCP and language servers

```lua
zemi.mcp.add("github", {
	url = "https://api.githubcopilot.com/mcp/",
	headers = { Authorization = "Bearer " .. zemi.env.get("GITHUB_TOKEN") },
})
zemi.mcp.add("files", {
	command = "npx",
	args = { "-y", "@modelcontextprotocol/server-filesystem", "/abs/path/to/project" },
})
```

A command server starts in zemi's data dir with the project path in `ZEMI_PROJECT_DIR`, so pass absolute paths, never `.`. `enabled_tools`, `disabled_tools` and `timeout` (seconds) trim a server. An HTTP server that needs an account, or was signed out, shows "needs sign-in" in `/mcp`, whose Sign in action opens the browser; one that refuses dynamic registration needs `client_id` (and `client_secret`) from an app registered by hand, plus `callback_port` when its redirect URI has a fixed port. Past ten MCP tools their schemas leave the request and the model loads them through `tool_search`. `/mcp restart|sign-in|sign-out <name>`; `/mcp sign-in-all` signs every waiting HTTP server in, one at a time.

Language servers (gopls, pyright, typescript-language-server, clangd, rust-analyzer, lua-language-server) start on the first file with their marker (go.mod, package.json, Cargo.toml, .luarc.json, .emmyrc.json, ...) at or above it, only in a trusted project; a `.lua` file of your config or a local plugin gets lua-language-server with zemi's types in any project. `zemi.lsp.add(name, { command, args, env, filetypes, settings, init_options })` adds a server or replaces a builtin of that name, `zemi.lsp.del(name)` keeps one from starting, `lsp.auto = false` turns off the builtin ones. `/lsp` lists them; `/lsp restart <name>`.

## Command line

`zemi [flags] [prompt]` opens the TUI; `zemi run [prompt | -]` answers without it: the last message on stdout, the run as the chat draws it on stderr (`-q`: only what went wrong; `--reasoning`: every thought and call whole), or with `--json` one JSON event per line on stdout, `done` last (DESIGN §8 has the fields). Piped stdin, 1 MiB at most, is attached to the prompt, or is the prompt after `-`, sent as text even when it starts with `/`; from a script add `</dev/null`, or it waits 3 s for input. Both take `-c` (continue the last session), `-r <id>`, `-m provider/model`, `--profile` and `--trust`; `-C <dir>` runs any command as if started in dir.
`zemi health` prints the checks (exits 1 when one errors); `zemi sessions` lists this project's sessions (`-a` every project's, `search <words>` finds them by what was said), `zemi sessions export <id>` prints one as markdown (`--full` as `/export full`); `zemi projects` lists every project zemi keeps state for, the path from column 36 (`zemi -C "$(zemi projects | fzf | cut -c36-)"` opens a picked one); `zemi models` lists the ids `-m` takes. `sessions`, `projects`, `plugins` and `health` take `--json`: one object per line (DESIGN §8 has the fields). A prompt naming a command runs it: `zemi run "/review HEAD~1"`; one that opens a screen fails with "/<name> needs the TUI", `/btw` prints its answer, and `zemi run "/goal make check is clean"` runs until the goal is met (exit 1 when it stops blocked or paused). A `zemi run` has nobody to approve an ask, so one a plugin's veto puts to the user is denied ("asks nobody could answer in a headless run", listed on stderr at the end); everything else runs. A `zemi run` makes no title call; the first line of its first message names the session. `zemi plugins ...` manages plugins from a shell (`plugins.md`), and `zemi completion <shell>` prints a completion script.

## Commands and input

The palette: `/new`, `/sessions` (resume, rename or delete; `search <words>` finds one in any project), `/rename [title]` (name this session; alone, a prompt), `/rewind [id]` (restores the files write and edit changed and a command's changes to files and links git does not ignore, then cuts the conversation there with the memories learned and todos set since), `/compact [what to keep]`, `/goal <condition>` (keeps sending turns until a check of the conversation finds it holds, 20 in a row at most; alone it shows the spend, `stop` drops it), `/loop [5m] <prompt or /command>` (sends it again every interval, 1m to 24h, or when the model's last line says `next: 10m`; one per session, gone with it, 72h at most; alone it shows the runs, `stop` stops it), `/btw <question>` (answered from the conversation, mid-turn too, in an overlay; neither joins the session, the cost does), `/model`, `/mode`, `/profile`, `/theme`, `/config`, `/memory`, `/skills`, `/agents` (subagent runs and the agents zemi can start; `new [what it should do]` has the agent write one, `open <name>` edits one, `view <id>`, `stop [id]`),
`/jobs` (the background processes the model started, running first, each with its clock; `view <id>` opens its output as it runs, `stop <id>` stops one, `stop all` every one), `/workflows` (runs and saved workflows; `new [what it should do]` has the agent write one, `run <name> [args]`, `view <id> [agent]`, `pause`, `resume`, `stop`, `rerun` or `open <id>`, `concurrency <id> <n>`, `restart <id> <agent>`, `save <id> [name]`), `/review [ref]` (the saved `review` workflow), `/mcp`, `/plugins` (`new [what it should do]` has the agent write one, `modify <name> [what to change]` change a local one), `/init` (writes AGENTS.md), `/usage` (cost, prompt cache share, fixed prompt cost), `/prompt` (the pinned system prompt and context message), `/copy [reply|code [n]]`, `/messages` (the chat as a list of turns, scrolled by its selection; `l` opens a turn, then one of its rails call by call; `alt+m`), `/logs` (recent `print` and `zemi.log` lines), `/projects` (projects zemi keeps state for: switch, forget trust, delete data), `/trust`, `/health`, `/errors`, `/help`, `/quit`.

Typed only: `/resume`, `/fork`, `/export [full] [path]` (full adds reasoning, every tool call expanded and the subagent runs), `/effort`, `/lsp`, `/reflect`, `/reload`.

`@path` attaches a project file as a reference, which the `@` completion popup inserts for one, as does a drop of a project file; a path the reference cannot carry keeps a chip or its quoted path. A file from outside the project dropped on the terminal, or an image pasted with ctrl+v, becomes `[Image <name>]` or `[File <name>]`, `<name>` the copy's own name; a directory, or a binary or oversized file, keeps its quoted path instead. Images are png, jpg, gif or webp up to 20 MB, shrunk past 2000 px or 3.75 MB, and reach only a model that takes images.

## Project config and trust

A repository's `.agents/` at the git root holds `init.lua`, `lua/`, `agents/`, `commands/`, `workflows/` and `skills/`, laid out like the user config dir; zemi reads no other project dir. It loads only in a trusted project, as the `project` origin: the first start shows a trust prompt once (`y` or `n` is saved; `l` Later, where the cursor starts, or esc brings it back next start) and `/trust` shows it again. One decision covers the repository and its linked worktrees. Project code calls the same API as the user's init.lua, with the same rights: widgets, commands, keymaps, settings, providers, language servers, MCP servers, profiles, tools. Its registrations load last, so they win a name collision, and origin is only a label on what it registered.
