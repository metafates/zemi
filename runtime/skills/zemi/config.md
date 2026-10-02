# Configuring zemi

Everything here goes in the user's init.lua (`<config>/init.lua`, run at startup as the trusted `user` origin) unless it says otherwise.

## Providers and models

The builtin providers (anthropic, openai, gemini, deepseek, openrouter, ...) are on while their key is exported in the shell that starts zemi (`ANTHROPIC_API_KEY`, ...); `/model` lists them and names the variable a missing one needs. Their models come from the catalog bundled with zemi; `/model update` (or `zemi models update` in a shell; `zemi models` lists the ids) fetches the latest from catwalk.charm.land and keeps it for later starts, until a zemi upgrade brings a newer bundle; the `/health` catalog row says which is in use. A gateway, a local server or a provider zemi does not know:

```lua
zemi.provider.add({
	id = "gw", -- a builtin id replaces that provider whole, key included
	kind = zemi.provider.kind.OPENAI_COMPAT, -- or ANTHROPIC, GOOGLE, OPENAI (gpt-4 and later over /responses), OPENROUTER, VERCEL
	base_url = "https://llm.internal/v1", -- required for OPENAI_COMPAT, refused for OPENROUTER
	api_key = zemi.env.get("GATEWAY_KEY"),
	-- headers = { ["X-Team"] = "core" }, -- as secret as the key
	-- body = { service_tier = "flex" }, -- extra request fields; not sent by GOOGLE
	models = {
		{ id = "deepseek-chat", context_window = 128000, cost_per_1m_in = 0.27, cost_per_1m_out = 1.1 },
	},
})
zemi.config.set("models.chat", "gw/deepseek-chat")
zemi.provider.del("openai") -- drop one builtin; providers.builtin = false drops them all
```

A longer prompt cache is an opt-in `body` entry, never a default since model support varies: `body = { prompt_cache_retention = "24h" }` on an `OPENAI` provider keeps OpenAI's cache for a day on the models that accept it.

Model fields besides `id` and `display`:

- `context_window`: without it there is no context percent and no auto compaction, only `/compact` and a provider's too-long refusal. `agent.context_window` overrides it.
- `cost_per_1m_in`, `cost_per_1m_out`, `cost_per_1m_cache_write` (tokens written to the prompt cache), `cost_per_1m_cache_read` (tokens read from it). Without the cache pair, cached turns read cheaper than they were.
- `max_output`: reply cap in tokens; unset, anthropic models get 32000, others the provider default.
- `vision = false`: the model takes no images: read's description tells it so, read refuses image files, and images leave its history. Without it zemi learns that from the provider's first refusal, for the process.

Any other key, on the provider or on a model entry, is refused as an unknown field.

Roles: `models.chat` runs the conversation, `/btw`, the goal check, the idle recap and every role left empty; `models.agents` (subagents whose definition names no model, `web` included), `models.titles` (session titles), `models.compact` (compaction summaries) and `models.memory` (the reflector) each take a `provider/model` or `""` to follow chat. An unset chat runs the first added provider's default model, else the first usable builtin's. Nothing falls back to a cheaper model on its own: set titles and memory to one to save tokens. Compact on the chat model is asked after the conversation itself, so it reads the prompt cache; a cheaper compact model gets a flat transcript instead. Set them with `zemi.config.set("models.titles", "gw/deepseek-chat")`, or `/model roles` (the list; enter opens a role's models), `/model <role> <provider/model>` or `/model <role> chat` to follow chat. `/model`, `-m` and `zemi.model.set` write `models.chat` and refuse while the active profile sets its own model. Agent files, profiles and workflows take a role name or a `provider/model`.

## Settings

`zemi.config.set(path, value)` in init.lua. `zemi.config.schema()` returns every key with kind, default, description and, for a key with a fixed list, its `choices`; read it with the Lua tool instead of guessing. A value of the wrong kind raises, and so does one outside a key's fixed list (`agent.effort`, `memory.autonomy`, `ui.notify`). A runtime pick (`/model`, `/effort`, `/profile`, the mode, `/config`) survives a reload until init.lua changes that key, and a restart unless init.lua sets that key: zemi keeps picks per user in its own store, never in init.lua, and keeps no flag and no yolo mode. So a user who wants a value in every start either just picks it, or, to pin it against later picks, sets it in init.lua. `/config` lists every key with where it came from (`init.lua`, `saved` for a kept pick, `default`, or `this run` for what a restart drops) and its value, a bool's as a switch: space toggles a bool and enter opens a key's detail to change it (a switch, a list, or a prompt holding the value, which sets the key and returns to /config) or reset it to init.lua's value or the default, which forgets the kept pick, `/config set <key> <value>` sets one, and `/config lua` shows every key off its default as `zemi.config.set` lines to copy. A plugin's settings are `plugins.<name>.<key>` keys it registers: /config lists them under the plugin's name and they layer like any other, and a line for a plugin that is off does nothing. The keys that need more than the schema says:

- `agent.profile`: `default`, `planner`, or a profile defined with `zemi.agent.profile.add` (`agents.md`); `/profile` or ctrl+t switches.
- `agent.effort`: `low`, `medium`, `high` (`zemi.model.effort.HIGH`, ...), or empty (or `default`) for the model's default; `/effort`. A builtin catalog model runs at its catalog default when unset, else the nearest level it lists, and gets none when it lists none; any other model gets the pick, or no effort when unset.
- `memory.autonomy`: `auto` stores policy-clean memories at once, `ask` queues every one for `/memory` (`memory.md`).
- `ui.notify`: how zemi alerts while its window is unfocused: `osc` (OSC 9, which iTerm2, Ghostty, WezTerm and kitty show; tmux, screen and zellij drop it without passthrough), `native` (osascript on macOS, notify-send on Linux; a bell when neither is found), `bell` or `off`. `auto` picks osc for a terminal known to show it outside those multiplexers, else native on this machine, else a bell; the `/health` notify row names the pick and why.
- `editor.command`: the editor for ctrl+g and `zemi.ui.edit`; empty uses `$VISUAL`, then `$EDITOR`, then vi.
- `keymap.leader`: what `<leader>` stands for in keymaps (`ui.md`).
- `permissions.mode`, `permissions.env_keep`: under Permissions. `lsp.auto`: under MCP and language servers.

The schema's description covers `agent.max_background`, `session.auto_title`, `session.recap`, `ui.theme`, `ui.animations`, `ui.mouse`, `ui.progress`, `ui.reasoning`, `ui.composer.multiline`, `ui.composer.history`, `ui.completion.files` and `ui.completion.max_items`.

## Permissions

A tool call meets the rules first; what they leave open goes to the permission mode, then to the user.

```lua
zemi.permission.rule.add({
	subject = "tool.shell",
	pattern = "git push*",
	decision = zemi.permission.decision.DENY,
	match = "git push origin main",
})
```

A rule in init.lua is added again every start. `match` is an argument the pattern must catch, checked when the rule is added. Deny beats ask beats allow; within one decision the most specific pattern wins. Subjects: `tool.shell` (the normalized command), `tool.lua`, `tool.workflow` and `tool.<name>` of a Lua tool (a rule on another builtin tool raises; one on a tool no config adds is an /errors entry once boot is done, as is a `zemi.tool.override` of one), `fs.read` and `fs.write` (paths), `net.<host>` (the model's `fetch` and `web_search`), `mcp.<server>` (the tool name), `memory.write`, and `lua.fs.write` (path), `lua.job.start` (command line), `lua.http.<method>` (URL) for calls config code makes. A `lua.*` call never shows a prompt in any mode: without an allow rule it raises with the rule to add. A subject ending in `.*` covers its family. In a command pattern `*` spans spaces and `/`, so `cat docs/*` also allows `cat docs/a /etc/passwd`; an argument with `..` or one the shell may still expand gets the stricter verdict.

Model tool names, for the `tools` of a profile or agent: `shell`, `read`, `write`, `edit`, `glob`, `grep`, `fetch`, `web_search`, `todos`, `question`, `plan`, `job_start`, `job_read`, `job_list`, `job_kill`, `memory`, `skill`, `diagnostics`, `lsp`, `agent`, `workflow`, `lua`, `update` (subagents only), `mcp_<server>_<tool>` and tools added from Lua.

At a prompt `y` allows once, `a` for the session (a command's program, with its subcommand for git, go, cargo and the like; a file's directory; a URL's host), `r` a file read anywhere in the repository, `p` for this project, `n` denies and `t` denies and stops the turn so the user can say what to do instead. `p` appends the command to `<state>/projects/<hash>/permissions`, one command per line, editable by hand and read only while the project is trusted (a repository cannot ship one). A deleting program stays exact, and a shell, interpreter, `sudo`, `xargs` or `zemi` gets no always answer. `/permissions` lists the mode and every rule, and deletes session ones.

Modes: `permissions.mode`, `/mode` (a picker without a name), ctrl+y (yolo until pressed again), `--mode`.

- `auto` (default): reads and edits inside the project run. In a trusted project with a sandbox every command runs sandboxed with no approval, except bulk deletes (`rm -rf`, `git clean`, `find -delete`, ...), git commands that write `.git` (on macOS only config, remotes, worktrees, submodules, rebase, the network and dropping commits for good: commit, checkout, stash and reset run), one whose quoting zemi cannot read the way the shell does, and on Linux one naming a secret-shaped file. Writes to `.git`, `.agents/init.lua`, `.agents/lua/` or zemi's own dirs (but a local plugin whose creating write you approved, until zemi exits), `fetch`, `web_search`, Lua tools and MCP tools the server does not mark read-only need the user's approval. In an untrusted project, or with no sandbox (`/health` sandbox row), only commands that just read run on their own, and untrusted auto edits no project file.
- `yolo`: allows everything the rules leave open; deny rules still apply. Suggest it only when the user asks.

Planning is the `planner` profile (`/profile planner`, ctrl+t): file writes are refused, commands run with the project read-only, and the turn ends in a plan to approve, whatever the mode.

## Custom permission modes (user config only)

`zemi.permission.mode.add(name, { base = "auto" or "yolo", description, prompt, rules, decide })` adds a mode to `/mode` or replaces one of that name (a lowercase letter, then lowercase letters, digits and `-`, 16 at most), `zemi.permission.mode.del(name)` takes one out, `zemi.permission.mode.get()` returns the active one (`{ name, base, description }`), and `permissions.mode` is the start mode. A mode acts like its base; `rules` (`{ "tool.shell", "go test*", "allow" }`) answer what would otherwise need the user's approval, `prompt` joins the system prompt while it is on, and `decide(req)` answers a request the rules left open (`req.agent` names the subagent asking, "" for the main agent). `decide` returns `"allow"` or `"deny"`; anything else (nil, an error, its 60 s ceiling) sends the request to the user even where the base would have allowed it. A `wait` in it frees the interpreter. Neither `rules` nor `decide` can allow `sandbox: false`, `network: true`, a request an `ask` rule covers, or a write to `.git`, the Lua in `.agents` or zemi's own files.

```lua
-- every request reaches the user
zemi.permission.mode.add("ask", { base = "auto", description = "I answer everything", decide = function() end })

-- a local model approves commands; zemi.http needs the user's allow rule, localhost included
zemi.permission.rule.add({
	subject = "lua.http.post",
	pattern = "http://localhost:11434/*",
	decision = zemi.permission.decision.ALLOW,
	match = "http://localhost:11434/api/generate",
})
zemi.permission.mode.add("judged", {
	base = "auto",
	description = "a local model approves commands",
	decide = function(req)
		if req.subject ~= "tool.shell" then return end
		local body = zemi.json.encode({ model = "llama3", stream = false, prompt = "Reply allow or deny. Command: " .. req.arg })
		local res = zemi.http.request({ method = "POST", url = "http://localhost:11434/api/generate", body = body }):wait()
		local out = res and zemi.json.decode(res.body)
		if out and (out.response or ""):match("^allow") then return zemi.permission.decision.ALLOW end
	end,
})
```

## The shell and the sandbox

The shell tool and `job_start` run a command with dash where it is installed (macOS, Debian, Ubuntu), else with sh; the `/health` shell row names which. Either way the command is read as POSIX sh, and bash-only syntax fails before it runs, naming the POSIX form: `[ ]` for `[[ ]]`, `>file 2>&1` for `&>`, `.` for `source`, `printf` for `$'..'`, a file or a pipe for `<( )`, each word written out for `{a,b}` and `$(seq 1 3)` for `{1..3}`, `x=$((x+1))` for `let`, no arrays. A `bash -c` body is read as POSIX sh too, so a bash script goes in a file, run as `bash file`. The user's `!command` runs sh.

The sandbox (seatbelt on macOS, Landlock on Linux) lets a command write only the project and the scratchpad, which is every model command's `$TMPDIR`, sandboxed or not (`/tmp` is not writable), keeps `.git` and `.agents` read-only (`.agents/worktrees` aside; on macOS a repository root's `.git` is writable but for config, hooks, attributes, alternates, submodules, rebase state, legacy remotes and worktree links), allows no network beyond localhost, and hides credential files and zemi's own dirs; on macOS secret-shaped files (`.env`, `*.pem`, `*.key`) are unreadable to it, in the scratchpad too, `.agents` files are written with the write tool, and the window server and system services are off, so a browser, GUI app or docker dies confined ("Abort trap: 6"). On Linux with namespaces localhost is the sandbox's own; with `landlock + seccomp` the project is read-only and there is no network. A command that needs the network is retried with `network: true` (sockets added, sandbox kept), one that needs more with `sandbox: false`; both need the user's approval in every mode but yolo, so in `zemi run` without yolo both are denied. `permissions.env_keep` names secret-shaped env vars (`*TOKEN*`, `*KEY*`, ...) a child keeps; whatever is kept, the model can print.

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

A command server starts in zemi's data dir with the project path in `ZEMI_PROJECT_DIR`, so pass absolute paths, never `.`. `enabled_tools`, `disabled_tools` and `timeout` (seconds) trim a server. An HTTP server that needs an account, or was signed out, shows "needs sign-in" in `/mcp`, whose Sign in action opens the browser; one that refuses dynamic registration needs `client_id` (and `client_secret`) from an app registered by hand, plus `callback_port` when its redirect URI has a fixed port. Past ten MCP tools their schemas leave the request and the model loads them through `tool_search`. `/mcp restart|sign-in|sign-out <name>`.

Language servers (gopls, pyright, typescript-language-server, clangd, rust-analyzer, lua-language-server) start on the first file with their marker (go.mod, package.json, Cargo.toml, .luarc.json, .emmyrc.json, ...) at or above it, only in a trusted project, and sandboxed when a sandbox exists; a `.lua` file of your config or a local plugin gets lua-language-server with zemi's types in any project; rust-analyzer needs a `Cargo.lock` and does not start without a sandbox. `zemi.lsp.add(name, { command, args, env, filetypes, settings, init_options })` adds a server or replaces a builtin of that name, `zemi.lsp.del(name)` keeps one from starting, `lsp.auto = false` turns off the builtin ones. `/lsp` lists them; `/lsp restart <name>`.

## Command line

`zemi [flags] [prompt]` opens the TUI; `zemi run [prompt | -]` answers without it: the last message on stdout, the run as the chat draws it on stderr (`-q`: only what went wrong; `--reasoning`: every thought and call whole), or with `--json` one JSON event per line on stdout, `done` last (README.md has the fields). Piped stdin, 1 MiB at most, is attached to the prompt, or is the prompt after `-`, sent as text even when it starts with `/`; from a script add `</dev/null`, or it waits 3 s for input. Both take `-c` (continue the last session), `-r <id>`, `-m provider/model`, `--mode`, `--profile` and `--trust`; `-C <dir>` runs any command as if started in dir.
`zemi health` prints the checks (exits 1 when one errors); `zemi sessions` lists this project's sessions and `zemi sessions export <id>` prints one as markdown (`--full` as `/export full`); `zemi projects` lists every project zemi keeps state for, the path from column 36 (`zemi -C "$(zemi projects | fzf | cut -c36-)"` opens a picked one); `zemi models` lists the ids `-m` takes. `sessions`, `projects`, `plugins` and `health` take `--json`: one object per line (README.md has the fields). A prompt naming a command runs it: `zemi run "/review HEAD~1"`; one that opens a screen fails with "/<name> needs the TUI", `/btw` prints its answer, and `zemi run "/goal make check is clean"` runs until the goal is met (exit 1 when it stops blocked or paused). In `zemi run` nobody can approve, so a call the mode does not allow is denied ("no one can approve in a headless run"), and stderr ends with each such ask and what would let it through; without a `net.*` allow rule, yolo or a mode's `decide` the run has no `fetch`, `web_search` or `web` agent. A `zemi run` makes no title call; the first line of its first message names the session. `zemi plugins ...` manages plugins from a shell (`plugins.md`), and `zemi completion <shell>` prints a completion script.

## Commands and input

The palette: `/new`, `/sessions` (resume or delete), `/rewind [id]` (restores the files write and edit changed and a command's changes to files and links git does not ignore, secret-shaped files aside, then cuts the conversation there with the memories learned and todos set since), `/compact [what to keep]`, `/goal <condition>` (keeps sending turns until a check of the conversation finds it holds, 20 in a row at most; alone it shows the spend, `stop` drops it), `/loop [5m] <prompt or /command>` (sends it again every interval, 1m to 24h, or when the model's last line says `next: 10m`; one per session, gone with it, 72h at most; alone it shows the runs, `stop` stops it), `/btw <question>` (answered from the conversation, mid-turn too, in an overlay; neither joins the session, the cost does), `/model`, `/mode`, `/profile`, `/theme`, `/config`, `/permissions`, `/memory`, `/skills`, `/agents` (subagent runs, background jobs and the agents zemi can start; `new [what it should do]` has the agent write one, `open <name>` edits one, `view <id>`, `stop [id]`, `kill <job id>`), `/workflows` (runs and saved workflows; `run <name> [args]`, `view <id> [agent]`, `pause`, `resume`, `stop`, `rerun` or `open <id>`, `concurrency <id> <n>`, `restart <id> <agent>`, `save <id> [name]`), `/review [ref]` (the saved `review` workflow), `/mcp`, `/plugins` (`new [what it should do]` has the agent write one, `modify <name> [what to change]` change a local one), `/init` (writes AGENTS.md), `/usage` (cost, prompt cache share, and what the fixed prompt costs), `/prompt` (the pinned system prompt and context message), `/copy [reply|code [n]]`, `/projects` (projects zemi keeps state for: switch, forget one's trust, delete its data), `/trust`, `/health`, `/errors`, `/messages` (recent `print` and `zemi.log` lines), `/help`, `/quit`. Typed only: `/resume`, `/fork`, `/export [full] [path]` (full adds reasoning, every tool call expanded and the subagent runs), `/effort`, `/lsp`, `/reflect`, `/reload`.

`@path` attaches a project file, a file dropped on the terminal is attached (copied as `@drop:<name>` from outside the project), and ctrl+v pastes an image. Images are png, jpg, gif or webp up to 20 MB, shrunk past 2000 px or 3.75 MB, and reach only a model that takes images.

## Project config and trust

A repository's `.agents/` at the git root holds `init.lua`, `lua/`, `agents/`, `commands/`, `workflows/` and `skills/`, laid out like the user config dir; zemi reads no other project dir. It loads only in a trusted project, as the low-trust `project` origin: the first start shows a trust prompt once (`y` or `n` is saved; `l` Later, where the cursor starts, or esc brings it back next start) and `/trust` shows it again. One decision covers the repository and its linked worktrees. Project code may add widgets, commands, keymaps on free keys, and deny or ask rules. It cannot register providers, MCP or language servers, allow rules or modes, set settings, send to the agent, or read secret-shaped env vars: personal settings go in the user init.lua.
