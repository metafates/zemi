---
name: zemi-config
description: Load before changing zemi's settings, providers, models, permissions, MCP or language servers, plugins or trust, or to explain a zemi command, flag or key.
---

# Configuring zemi

## Files

| Path | What |
| --- | --- |
| `<config>/init.lua` | user config, run at startup as the trusted `user` origin |
| `<config>/lua/`, `skills/`, `agents/`, `commands/`, `workflows/` | modules, skills, and the files zemi-customize describes |
| `<repo>/.zemi/` | project config with the same layout, loaded only once the user trusts the project, as the low-trust `project` origin |
| `<data>/site/pack/<any>/start/<name>/` | plugins |
| `<data>/types/zemi.lua` | LuaLS annotations for the whole API, rewritten every start |
| `<state>/` | logs, databases, trust decisions, and per project its `permissions` file and recipes (zemi-debug) |

`<config>` is the user config directory the system prompt names. `ZEMI_CONFIG_HOME`, `ZEMI_DATA_HOME`, `ZEMI_STATE_HOME` and `ZEMI_CACHE_HOME` move the roots (default `~/.config/zemi`, `~/.local/share/zemi`, `~/.local/state/zemi`, `~/.cache/zemi`). zemi reloads when a config file changes; `/reload` forces it. Never write an API key into a file: read it with `zemi.env.get("NAME")`.

## Providers and models

The builtin providers (anthropic, openai, gemini, deepseek, openrouter, ...) are on while their key is exported in the shell that starts zemi (`ANTHROPIC_API_KEY`, ...); `/model` lists them and names the variable a missing one needs. A gateway, a local server or a provider zemi does not know:

```lua
zemi.provider.add({
  id = "gw",                                -- a builtin id replaces that provider whole, key included
  kind = zemi.provider.kind.OPENAI_COMPAT,  -- or ANTHROPIC, OPENAI, GOOGLE
  base_url = "https://llm.internal/v1",     -- required for OPENAI_COMPAT
  api_key = zemi.env.get("GATEWAY_KEY"),
  -- headers = { ["X-Team"] = "core" },     -- as secret as the key
  -- body = { service_tier = "flex" },      -- extra request fields; ANTHROPIC and OPENAI_COMPAT only
  models = {
    { id = "deepseek-chat", context_window = 128000, cost_per_1m_in = 0.27, cost_per_1m_out = 1.1 },
  },
})
zemi.config.set("models.main", "gw/deepseek-chat")
zemi.provider.del("openai")                 -- drop one builtin; providers.builtin = false drops them all
```

Model fields besides `id` and `display`:

- `context_window`: without it there is no context percent and no auto compaction, only `/compact` and a provider's too-long refusal. `agent.context_window` overrides it.
- `cost_per_1m_in`, `cost_per_1m_out`, `cost_per_1m_in_cached` (tokens written to the prompt cache), `cost_per_1m_out_cached` (tokens read from it). Without the cached pair, cached turns read cheaper than they were.
- `max_output`: reply cap in tokens; unset, anthropic models get 32000, others the provider default.
- `images = false`: the model refuses images. Without it zemi learns that from the provider's first refusal, for the process.

Tiers: `models.main` runs everything; `models.small` (titles, compaction summaries, the reflector, the `web` agent) and `models.smart` (the main conversation, which delegates; subagents stay on main) fall back to it. An unset main runs the first added provider's default model, else the first usable builtin's. `/model` and `-m` write the tier the conversation reads (smart when set, else main) and refuse while the active profile sets its own model. Agent files, the `agent` tool and workflows take a tier name or a `provider/model`.

## Settings

`zemi.config.set(path, value)` in init.lua. `zemi.config.schema()` returns every key with kind, default and description; read it with the Lua tool instead of guessing. A runtime pick (`/model`, `/effort`, `/profile`, the mode) survives a reload until init.lua changes that key. The keys that need more than the schema says:

- `agent.profile`: `default`, `planner`, or a profile defined with `zemi.agent.profile` (zemi-customize); `/profile` or ctrl+t switches.
- `agent.effort`: `low`, `medium`, `high`, or empty for the provider default; `/effort`.
  A builtin catalog model gets the nearest level it lists, or none when it lists none.
- `reflector.model`, `compact.model`: a model for those jobs instead of the small tier.
- `web.search_url`: a SearXNG endpoint with the json format on; empty uses DuckDuckGo Lite, which some networks answer with a bot check.
- `memory.autonomy`: `auto` stores policy-clean memories at once, `ask` queues every one for `/memory` (zemi-learning).
- `editor.command`: the editor for ctrl+g and `zemi.ui.edit`; empty uses `$VISUAL`, then `$EDITOR`, then vi.
- `permissions.mode`, `permissions.env_keep`: under Permissions.

The schema's description covers `session.auto_title`, `ui.theme`, `ui.animations`, `ui.mouse`, `ui.notify`, `ui.progress`, `ui.chat.reasoning`, `ui.composer.multiline`, `ui.composer.history`, `ui.completion.files` and `ui.completion.max_items`.

## Permissions

A tool call meets the rules first; what they leave open goes to the permission mode, then to the user.

```lua
zemi.permission.rule.add({ subject = "tool.bash", pattern = "git push*", decision = "deny", match = "git push origin main" })
```

A rule in init.lua is added again every start. `match` is an argument the pattern must catch, checked when the rule is added. Deny beats ask beats allow; within one decision the most specific pattern wins. Subjects: `tool.<name>` (`tool.bash` matches the normalized command), `fs.read` and `fs.write` (paths), `net.<host>` (the model's `fetch` and `web_search`), `mcp.<server>` (the tool name), and `lua.fs.write` (path), `lua.job.start` (command line), `lua.http.<method>` (URL) for calls config code makes. A `lua.*` call never shows a prompt in any mode: without an allow rule it raises with the rule to add. A subject ending in `.*` covers its family. In a command pattern `*` spans spaces and `/`, so `cat docs/*` also allows `cat docs/a /etc/passwd`; an argument with `..` or one the shell may still expand gets the stricter verdict.

Model tool names, for `tool.<name>` rules and the `tools` of a profile or agent: `bash`, `read`, `write`, `edit`, `glob`, `grep`, `fetch`, `web_search`, `todos`, `question`, `plan`, `job_start`, `job_read`, `job_list`, `job_kill`, `memory`, `skill`, `diagnostics`, `lsp`, `agent`, `workflow`, `lua`, `mcp_<server>_<tool>` and tools added from Lua.

At a prompt `y` allows once, `a` for the session (a command's program, with its subcommand for git, go, cargo and the like; a file's directory; a URL's host), `r` a file read anywhere in the repository, and `p` for this project: it appends the command to `<state>/projects/<hash>/permissions`, one command per line, editable by hand and read only while the project is trusted (a repository cannot ship one). A deleting program stays exact, and a shell, interpreter, `sudo` or `xargs` gets no always answer. `/permissions` lists the mode and every rule, and removes session ones.

Modes: `permissions.mode`, `/mode`, tab and shift+tab, ctrl+y (yolo until pressed again), `--mode`.

- `auto` (default): reads and edits inside the project run. In a trusted project with a sandbox every command runs sandboxed with no approval, except bulk deletes (`rm -rf`, `git clean`, `find -delete`, ...), git commands that write `.git`, and on Linux one naming a secret-shaped file. Writes to `.git`, `.zemi` or zemi's own dirs, `fetch`, `web_search` and MCP tools the server does not mark read-only need the user's approval. In an untrusted project, or with no sandbox (`/health` sandbox row), only commands that just read run on their own, and untrusted auto edits nothing.
- `yolo`: allows everything the rules leave open; deny rules still apply. Suggest it only when the user asks.
- Custom modes (`zemi.permission.mode.add`): zemi-customize.

Planning is the `planner` profile (`/profile planner`, ctrl+t): file writes are refused, commands run with the project read-only, and the turn ends in a plan to approve, whatever the mode.

The sandbox (seatbelt on macOS, Landlock on Linux) lets a command write only the project, the scratchpad and temp dirs, keeps `.git` and `.zemi` read-only (`.zemi/worktrees` aside), allows no network beyond localhost, and hides credential files and zemi's own dirs. On Linux with namespaces localhost is the sandbox's own; with `landlock + seccomp` the project is read-only and there is no network. A command that needs the network is retried with `network: true` (sockets added, sandbox kept), one that needs more with `sandbox: false`; both need the user's approval in every mode but yolo, so under `-p` without yolo both are denied. `permissions.env_keep` names secret-shaped env vars (`*TOKEN*`, `*KEY*`, ...) a child keeps; whatever is kept, the model can print.

## MCP and language servers

```lua
zemi.mcp.add("github", { url = "https://api.githubcopilot.com/mcp/",
  headers = { Authorization = "Bearer " .. zemi.env.get("GITHUB_TOKEN") } })
zemi.mcp.add("files", { command = "npx", args = { "-y", "@modelcontextprotocol/server-filesystem", "/abs/path/to/project" } })
```

A command server starts in zemi's data dir with the project path in `ZEMI_PROJECT_DIR`, so pass absolute paths, never `.`. `enabled_tools`, `disabled_tools` and `timeout` (seconds) trim a server. An HTTP server that needs an account shows "needs sign-in" in `/mcp`, whose Sign in action opens the browser; one that refuses dynamic registration needs `client_id` (and `client_secret`) from an app registered by hand, plus `callback_port` when its redirect URI has a fixed port. Past ten MCP tools their schemas leave the request and the model loads them through `tool_search`. `/mcp restart|sign-in|sign-out <name>`.

Language servers (gopls, pyright, typescript-language-server, clangd, rust-analyzer) start on the first file with their marker (go.mod, package.json, Cargo.toml, ...) at or above it, only in a trusted project, and sandboxed when a sandbox exists; rust-analyzer needs a `Cargo.lock` and does not start without a sandbox. `zemi.lsp.add(name, { command, args, env, filetypes, settings, init_options })` adds a server or replaces a builtin of that name, `zemi.lsp.disable(name)` keeps one from starting, `lsp.auto = false` turns off the builtin ones. `/lsp` lists them; `/lsp restart <name>`.

## Command line

`zemi [flags] [prompt]`: `-p` answers on stdout without the TUI (piped stdin, 1 MiB at most, is attached to the prompt; from a script add `</dev/null`, or it waits 3 s for input), `--output json` with `-p`, `-c` continues the last session, `-r <id>` resumes one, `-m provider/model`, `--mode`, `--profile`, `--trust`, `--health` (exits 1 when a check errors). A prompt naming a command runs it: `zemi -p "/review HEAD~1"`. Under `-p` nobody can approve, so a call the mode does not allow is denied and stderr counts them. A `-p` run makes no title call; the first line of its first message names the session.

## Commands and input

The palette: `/new`, `/sessions` (resume or delete), `/rewind [id]` (restores the files write and edit changed and a command's changes to files git does not ignore, links and secret-shaped files aside, then cuts the conversation there with the memories learned and todos set since), `/compact [what to keep]`, `/btw <question>` (answered from the conversation, mid-turn too, in an overlay; neither joins the session, the cost does), `/model`, `/mode`, `/profile`, `/permissions`, `/memory`, `/skills`, `/agents` (subagent runs and background jobs; `view <id>`, `stop [id]`, `kill <job id>`), `/workflow`, `/review`, `/mcp`, `/init` (writes AGENTS.md), `/usage` (cost, prompt cache share, and what the fixed prompt costs), `/prompt` (the system prompt as last sent), `/copy [reply|code [n]]`, `/trust`, `/health`, `/errors`, `/messages` (recent `print` and `zemi.log` lines), `/help`, `/quit`. Typed only: `/resume`, `/fork`, `/export [path]`, `/effort`, `/lsp`, `/reflect`, `/reload`.

`@path` attaches a project file, a file dropped on the terminal is attached (copied as `@drop:<name>` from outside the project), and ctrl+v pastes an image. Images are png, jpg, gif or webp up to 20 MB, shrunk past 2000 px or 3.75 MB, and reach only a model that takes images.

## Project config and trust

A repository's `.zemi/` (found up to the git root) holds `init.lua`, `lua/`, `agents/`, `commands/`, `workflows/` and `skills/`; `agents/`, `commands/` and `skills/` are also read from `.agents/` and `.claude/`. It loads only in a trusted project: the first start shows a trust prompt once (`y` or `n` is saved, esc brings it back next start) and `/trust` shows it again. One decision covers the repository and its linked worktrees. Project code may add widgets, commands, keymaps on free keys, and deny or ask rules. It cannot register providers, MCP or language servers, allow rules or modes, set settings, send to the agent, or read secret-shaped env vars: personal settings go in the user init.lua.

## Plugins

A plugin is `<data>/site/pack/<any>/start/<name>/`: `plugin/*.lua` runs at startup, before init.lua, as the `plugin` origin, and `lua/` holds modules anyone can require. It may add commands, keymaps on free keys, widgets, overlays, tools, event handlers, `agent.assemble` sections, and deny and ask rules. A job, an HTTP request or a file write raises until the user's init.lua allows it (`lua.job.start`, `lua.http.<method>`, `lua.fs.write`), and a plugin tool needs the user's approval on every call until the user allows `tool.<name>`: print those rules in the README. It cannot edit the composer, send to the agent, change settings, the theme, the layout, the model or the mode, or register providers or servers. Code that needs to ships as a module the user wires up in init.lua, where it runs as user config:

```lua
local snippets = require("snippets")   -- <plugin>/lua/snippets/init.lua
zemi.command.add("snip", snippets.pick, { desc = "insert a snippet" })
```

`plugin/*.lua` runs before init.lua, so register with defaults there and make `setup(opts)` safe to call again. The plugin tree is not watched: `/reload` after editing it.

## Splitting config

`require("a.b")` loads `lua/a/b.lua` or `lua/a/b/init.lua` from the config dir or a plugin; `require("./x")` loads `x.lua` next to the calling file and must stay inside that file's own tree.
