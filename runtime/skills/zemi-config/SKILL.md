---
name: zemi-config
description: Configure zemi - init.lua, providers, models, settings, keymaps, commands, themes, project .zemi config, permission rules. Load to change how zemi behaves.
---

# Configuring zemi

## Where things live

| Path | What |
| --- | --- |
| `~/.config/zemi/init.lua` | user config, runs at startup with the trusted `user` origin |
| `~/.config/zemi/skills/<name>/SKILL.md` | skills the user wrote |
| `<project>/{.zemi,.agents,.claude}/skills/<name>/SKILL.md` | project skills: only in a project the user trusts, body only |
| `~/.config/zemi/.luarc.json` | lua-language-server settings, rewritten by zemi when they change |
| `~/.local/share/zemi/types/zemi.lua` | LuaLS annotations for the whole `zemi.*` API, rewritten on every start |
| `<project>/.zemi/init.lua` | project config: loads only in a project the user trusts, low-trust `project` origin |
| `~/.local/share/zemi/site/pack/*/start/*/plugin/*.lua` | plugins |
| `~/.local/state/zemi/` | logs, databases, trust file (see zemi-debug) |

`ZEMI_CONFIG_HOME`, `ZEMI_DATA_HOME`, `ZEMI_STATE_HOME` and `ZEMI_CACHE_HOME` (language
server caches, `~/.cache/zemi`) override the four roots.

Rules when editing:

- Add lasting rules with `zemi.permission.rule.add` in init.lua, which adds them again on every
  start; `a` in a permission prompt allows until zemi exits (a prompt's `p` choice allows a command
  permanently in one trusted project). A leftover `permissions.lua`
  is not read. For one trusted project, a `permissions` file in zemi's state dir
  (`<state>/projects/<hash>/permissions`, the path `/permissions` shows; one bash command per
  line, written there by a prompt's `p` answer or by hand) allows them without a prompt in
  that project. A repository cannot ship one: a `.zemi/permissions` file is not read.
- File writes ask first and show the diff, and write, edit and multiedit
  refuse a file the model has not read or that changed since it read it, and a symbolic
  link (edit its target). A bash row shows a diff of the files the command changed. zemi reloads when
  config files change; `/reload` forces it.
- Never write API keys into files: read them with `zemi.env.get("NAME")`.

## Providers and models

zemi knows the usual providers (anthropic, openai, gemini, deepseek, openrouter, ...; `/model`
lists them): export the key (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, ...) in
the shell that starts zemi and that provider is on, with its models in `/model`. A provider
zemi does not know, a local server, or a gateway in front of a known one (reuse its id: yours
replaces it whole, key included):

```lua
zemi.provider.add({
  id = "anthropic",
  kind = zemi.provider.kind.ANTHROPIC,        -- or OPENAI, OPENAI_COMPAT, GOOGLE
  base_url = "https://llm.internal/v1",       -- required for openai_compat, an override for the rest
  api_key = zemi.env.get("GATEWAY_KEY"),      -- the key from the environment
  -- headers = { ["X-Team"] = "core" },     -- sent with every request; as secret as the key
  -- body = { service_tier = "flex" },      -- extra request-body fields; anthropic and openai_compat only
  models = {
    { id = "claude-sonnet-4-6", display = "Sonnet 4.6",
      context_window = 200000, cost_per_1m_in = 3, cost_per_1m_out = 15 },
  },
})
zemi.provider.del("openai")                   -- drop one builtin provider
zemi.config.set("providers.builtin", false)   -- or all of them
zemi.config.set("models.main", "anthropic/claude-sonnet-4-6")
```

A tier left unset (`models.main`, `models.small`) runs the default model of the first provider
you added, else of the first usable builtin one in catalog order (anthropic, openai, gemini,
...); `models.smart` falls back to main.

`images = false` marks a model that refuses image input: `read` on an image then returns a
text error and `@image` attachments are omitted with a note. Without it zemi learns the same
from the provider's 400 over an image: the turn goes on with notes in place of the images and
the model stays text-only for the process (not when the 400 says one image was too large);
the flag makes that permanent.

`context_window` (or the `agent.context_window` setting, which overrides it) drives the
statusline percent and auto compaction. Compaction starts at `max(window - 33k, window/2)`
tokens, before a turn or between steps of one: old tool output is cleared first, then older
messages are summarized while recent ones stay verbatim. It also runs once when the provider
rejects a prompt as too long, and stops for the session after 3 failed tries (`/compact` or
`/new` then). Without a window zemi compacts only on that rejection or on
`/compact <what to keep>` (refused while a turn runs).

`cost_per_1m_in_cached` prices tokens written to the prompt cache and
`cost_per_1m_out_cached` prices tokens read back from it (not cached output),
which providers bill apart from prompt tokens; without them cached turns read as cheaper
than they were.

`max_output` caps each reply in tokens. Unset, Anthropic models get 32000 and other kinds
the provider's default; a model that refuses 32000 is asked again without a cap. A reply
that hits the cap ends the turn with an error row.

Only `init.lua` may register providers, and only user config may read secret-looking
variables with `zemi.env.get`. MCP `headers` and `env` values are plain strings: build them
in Lua (`Authorization = "Bearer " .. zemi.env.get("GITHUB_TOKEN")`). `/model` opens a picker (context window and USD per million tokens next to each model);
`/model provider/model` switches directly.

## Settings

`zemi.config.get(path)`, `zemi.config.set(path, value)`, `zemi.config.watch(path, fn)`.
A reload keeps a value picked at runtime (`/model`, `/effort`, the mode cycle, `-m`) unless
init.lua now sets that key to a different value.
`zemi.config.schema()` lists every key with kind, default and description; prefer it over
guessing. Common keys:

- Model tiers, each a `provider/model`; a tier nothing sets falls back to `models.main`:
  - `models.main`: the model everything runs on unless a tier or a definition names another.
  - `models.small`: the short jobs (session titles, compaction summaries, the reflector, the
    builtin `web` agent), e.g.
    `zemi.config.set("models.small", "anthropic/claude-haiku-4-5")`.
  - `models.smart`: the main conversation, which orchestrates and delegates; subagents stay
    on `models.main`.
  `/model` and `-m` write the tier the conversation reads: smart when it is set, else main.
  Agent definitions, the `agent` tool and workflows take a tier name where they take a model.
- `reflector.model` (empty: `models.small`, then `models.main`)
- `web.search_url`: a SearXNG endpoint (`http://localhost:8888/search`, json format enabled)
  for `web_search`; empty uses DuckDuckGo Lite, which blocks some networks with a bot check.
- `agent.profile`: the profile the main conversation runs as, `default` or a name defined
  with `zemi.agent.profile`; a name nothing defines fails the send. `/profile` picks one;
  zemi-customize shows how to define one.
- `agent.effort`: `low`, `medium`, `high` or empty (provider default); `/effort` sets it.
  Anthropic models get the effort parameter, OpenAI and compatible ones reasoning_effort.
- `editor.command` (empty: `$VISUAL`, then `$EDITOR`, then vi) is the editor for
  `zemi.ui.edit` and the composer draft (ctrl+g). It runs fullscreen
  over zemi and the TUI resumes when it exits; ctrl+c is the editor's to use.
- `agent.context_window`: tokens, overrides the model's `context_window` (0 = the model's)
- `compact.model`: model that writes compaction summaries (empty uses `models.small`)
- `session.auto_title` (true): name sessions from the first exchange with `models.small`
- `lsp.auto` (true): start installed gopls, pyright, typescript-language-server, clangd or
  rust-analyzer when their marker file (go.mod, package.json, Cargo.toml, ...) is next to a
  file zemi touches or above it, up to the repo root. typescript starts only with a
  `tsserver` on PATH outside the repo (never the repo's node_modules). No server starts,
  builtin or added, in a project the user has not trusted: servers run the repo's code.
  With a sandbox (`/health` sandbox row) every server runs in it: the project read-only,
  writes only to `~/.cache/zemi/lsp/` (`ZEMI_CACHE_HOME`), localhost only. rust-analyzer
  needs a `Cargo.lock` to exist there (a sandboxed `cargo build` makes one), and without a
  sandbox it does not start.
  `zemi.lsp.add(name, { command, args, env, filetypes, settings, init_options })` adds a
  server (user config only); adding a builtin's name replaces it, so
  `zemi.lsp.add("gopls", { command = "gopls", filetypes = { "go" }, settings = { gopls = { staticcheck = true } } })`
  configures gopls. `zemi.lsp.disable(name)` keeps one from starting. `zemi.lsp.list()` and
  `/lsp` show what the project can use (available, running, failed); `/lsp restart <name>`.
- `keymap.leader` (ctrl+g): what `<leader>` means in keymaps; set it before binding
- `ui.theme`, `ui.animations`, `ui.composer.multiline`, `ui.composer.history`
- `ui.mouse` (true): drag copies, clicks act on rows; false leaves selection to the terminal
- `ui.notify` (true): terminal notification when zemi is unfocused and a turn ends or a
  prompt waits
- `ui.progress` (true): OSC 9;4 while a turn runs, which taskbars and tab bars draw
- `ui.chat.reasoning` (false): finished reasoning blocks start expanded; ctrl+o toggles
- `ui.completion.files`, `ui.completion.max_items`
- `memory.autonomy` (`auto`): memories the model or the reflector propose
  store at once unless policy flags them (instruction-shaped text, retirements,
  replacements of the user's, an untrusted project, or a session that used `fetch`,
  `web_search`, the `lua` tool or a tool zemi does not ship (MCP, `zemi.tool`));
  set `ask` to review every one in `/memory`
- `permissions.mode` (`auto`, which an unknown value also reads as): `auto` (reads, edits
  inside the project and commands run on their own, the commands sandboxed; bulk deletes
  like `rm -rf`, `git clean` or `find -delete`, git commands that write `.git` and everything
  else go to the user) or `yolo` (every ask is allowed; deny rules still apply, to the command
  as written and behind `env`, `sudo`, `timeout`, a path or leading options, but not to what
  only runs decide, like a git alias). Planning is
  not a mode but the read-only `planner` profile (`/profile planner`, or any profile with
  `read_only`): file writes are refused, commands run in a sandbox that keeps the project
  read-only (without trust or a sandbox, those that change things ask) and MCP calls ask, so
  the agent investigates and proposes a plan, whatever the mode.
  In a project the user has not trusted (`/trust`), auto runs only commands
  that just read (no builds, tests or git), edits nothing, and the Lua tool asks. In any project the Lua tool asks before a script calls
  anything that is not a read (config, providers, permissions, jobs, servers, the
  composer, ...). Commands auto runs without asking run in a
  sandbox (seatbelt on macOS, Landlock on Linux): writes only in the project (not for a
  read-only agent), the scratchpad and temp dirs, `.git` and `.zemi` read-only, no network beyond
  localhost, credential files in the home dir and zemi's own dirs unreadable; Go builds get
  a cache of their own. On Linux with user namespaces (`landlock + namespaces` in
  `/health`) localhost is the sandbox's own, so servers already running on the machine are
  unreachable; if such a command creates `.zemi` or `.git`, project trust turns off until
  `/trust`. Without namespaces (`landlock + seccomp`) the project is read-only to these
  commands and there is no network at all. A project inside zemi's own dirs, with a
  symlinked `.git` or `.zemi`, or started with a relative entry (`.`, `bin`) in `PATH`, is
  read-only to them too, and a `PATH` dir inside the project (an activated `.venv/bin`) always is. Build, test and
  lint tools run there with any flags; a program named by a path (`./run.sh`), `git config`,
  `git remote -v` and git commands that print file content in a repository tracking a
  secret-shaped file always ask the user. On Linux the sandbox cannot hide `.env`, so a
  build or test flag that runs code can read it in a trusted project. A command that needs the network is retried with `network: true` (the sandbox stays, sockets are added); one that needs more with `sandbox: false`, which asks the
  user (not with `-p`, where nobody can answer). Without a sandbox (`/health` sandbox row) they run only commands that just read,
  as in an untrusted project. At runtime:
  `/mode <name>`, `tab` (next), `shift+tab` (previous) or `ctrl+y` (yolo until the next
  press); for one run: `zemi --mode auto`. `/permissions` lists the mode and every rule in
  force; an "always" answer (the only rule made at runtime) has a Remove action there,
  config rules do not. An "always" answer covers a command's program (with its subcommand
  for git, go, cargo and the like), a file's directory or a URL's host, and a file read is
  also offered its whole repository; a deleting program (rm, mv, find, tar, ...) is kept
  exact, and a command that runs whatever it is handed (a shell, an interpreter, sudo,
  xargs) is offered no always answer at all. Widgets read it as `ctx.permission_mode` (and its behavior as
  `ctx.permission_mode_base`). Suggest yolo only when the user asks for it.
- `permissions.env_keep` (empty): comma-separated env var names a command, job or
  language server keeps although they look secret-shaped (`*KEY*`, `*TOKEN*`, `*SECRET*`,
  `*PASSWORD*`, ...), which zemi otherwise drops from every child. Name only what a build
  really needs: whatever is kept, the model can print.
- Modes are configurable in init.lua. The start mode is `zemi.config.set("permissions.mode", "auto")`.
  `zemi.permission.mode.del("yolo")` takes a mode out of tab and `/mode`,
  `zemi.permission.mode.set(name)` switches the active one, and
  `zemi.permission.mode.add(name, { base = "auto"|"yolo", description, prompt, rules, decide })`
  adds one (or replaces one with that name) after the others. It behaves like its base; its
  `rules` (`{ "tool.bash", "go test*", "allow" }`, `{ "tool.bash", "git push*", "deny" }`) answer
  what would otherwise ask, `decide(req)` answers one ask the rules left open with "allow" or
  "deny" (60s ceiling, so it may call a model of its own) and anything else sends it to the
  user, whatever the base would settle, and `prompt` joins the system prompt while it is on:

  ```lua
  zemi.permission.mode.del("yolo")
  zemi.permission.mode.add("ask", {   -- every ask reaches the user
    base = "auto", description = "I answer everything", decide = function() end,
  })
  ```

## Command line

`zemi [flags] [prompt]`: `-p` answers on stdout without the TUI (piped stdin joins the
prompt), `-c` continues the last session, `-r <id>` resumes one, `-m provider/model`,
`--mode auto|yolo`, `--output text|json` (json needs `-p`), `-v` version,
`--health` prints the checks `/health` shows and exits 1 when any of them errors.
A prompt starting with a registered command runs it instead of reaching the model, so
`zemi -p "/review HEAD~1"` or a `commands/*.md` name works from a script or a git hook.
`zemi --help` has the full list. With `-p` nobody answers asks, so an ask the mode does not
settle is denied; a run that had calls denied says so on stderr.

## Keymaps, commands, UI

```lua
zemi.keymap.set("ctrl+t", function() zemi.agent.send("run the tests") end, { desc = "run tests" })
zemi.keymap.set("ctrl+r", "compact")   -- a command name or a host action like "zemi.ui.help"
zemi.keymap.set("ctrl+t", "new")       -- setting a bound key again rebinds it
zemi.keymap.del("tab")                 -- unbind, also a shipped default
for _, k in ipairs(zemi.keymap.list()) do print(k.key, k.action, k.origin) end
local k = zemi.keymap.get("ctrl+o")    -- nil when unbound
-- host actions: zemi.ui.help, zemi.ui.reasoning.toggle, zemi.ui.scroll.up, zemi.ui.scroll.down,
-- zemi.ui.scroll.top, zemi.ui.scroll.bottom (each takes a node id after a space),
-- zemi.ui.focus.set [id], zemi.ui.focus.next, zemi.ui.focus.prev, zemi.model.pick,
-- zemi.composer.submit, zemi.composer.insert <text>, zemi.composer.history_prev,
-- zemi.composer.history_next, zemi.composer.paste, zemi.composer.edit,
-- zemi.permission.mode.cycle, zemi.permission.mode.prev, zemi.agent.profile.cycle, zemi.agent.profile.prev
-- (focus, composer, permission-mode and agent actions: user config only; zemi.HostAction in the type
-- annotations is the whole list)

zemi.command.add("standup", function(args)
  zemi.agent.send("summarize yesterday's commits " .. args.args)
end, { desc = "draft a standup", nargs = "*" })

zemi.ui.theme.add("mine", {
  palette = { fg = "#e0def4", muted = "#6e6a86", accent = "#c4a7e7" },
  hl = { Normal = { fg = "fg" }, Comment = { fg = "muted" }, Title = { fg = "accent", bold = true } },
})
zemi.ui.theme.set("mine")
```

Widgets replace or extend the screen. Registering a widget with an existing id from
`init.lua` replaces the builtin one (zemi-customize lists the ids, the regions and the
rules a `render` follows):

```lua
zemi.ui.widget.add({
  id = "statusline.branch", region = "statusline.left", priority = 40,
  render = function(ctx) return { { { text = "main", hl = "Comment" } } } end,
})
```

Attaching: `@path` attaches a project file; dragging files onto the terminal attaches them
(images from anywhere are copied as `@drop:<name>`), and ctrl+v pastes an image from the
clipboard. Images must be png, jpg, gif or webp up to 20 MB, other dropped files UTF-8 text;
secret-looking paths are refused. An image over 2000 px a side or 3.75 MB is shrunk before
it is sent (a webp over 3.75 MB is refused: zemi cannot decode webp). Images reach the model only if it accepts images.

Slash commands in the palette: `/new`, `/sessions`, `/rewind`, `/compact`, `/model`,
`/mode`, `/permissions`, `/memory`, `/skills`, `/agents`, `/workflow`, `/review`, `/mcp`, `/init`,
`/usage`, `/prompt`, `/copy`, `/trust`, `/health`, `/errors`, `/messages`, `/help`, `/quit`. They also run when typed but stay out of the
palette: `/resume`, `/fork`, `/export`, `/effort`, `/lsp`, `/reflect`,
`/reload`). Arguments:

- `/sessions`: enter on an earlier session offers Resume and Delete. Delete removes its
  messages, subagent runs and scratchpad from disk; memories learned from it stay.
- `/rewind [id]`: back to an earlier prompt. Files changed by write, edit and multiedit
  return to their state before it, the conversation is cut there and the prompt comes back
  in the composer. Files a command changed go back too when they are in the repository and
  not ignored. Lua: `zemi.session.rewind(id)`.
- `/fork` continues in a new session and keeps the original; `/export [path]` writes
  markdown (default `zemi-<id>.md`); `/copy` puts the last reply's raw markdown on
  the clipboard (a drag copies the wrapped screen rows); when the reply has code blocks it
  opens a picker instead (1-9 copy that block, then all blocks, then the whole reply), and
  `/copy reply`, `/copy code` and `/copy code <n>` skip it; `/init` writes or updates AGENTS.md.
- `/effort low|medium|high|default`, `/mode <name>`, `/compact <what to keep>`.
- `/usage` shows tokens and cost per session, day and all time, the share of this run's
  input the prompt cache served, plus what the fixed prompt every request carries (system,
  tools, mcp, instructions, lua, skills, memory). `/prompt` shows that system prompt
  itself, as the last send assembled it (`zemi.agent.prompt()` returns the same text).
- `/messages` shows the last 100 `print()` and `zemi.log` lines, newest first, which is
  where a print from a keymap, a widget render, a timer or an event handler goes
  (`zemi.log.list()` returns them; the log file keeps them all).
- Settings and their values: `zemi.config.get(path)` with the Lua tool; the changes zemi
  made to its own config are the git log of the config directory.
- `/agents view <id>` shows a subagent run's transcript in the chat (esc returns),
  `/agents stop [id]` stops a running one (without an id, the one on screen);
  `/agents` also lists the model's background jobs from every session (`job_start`, a bash
  command past its timeout) with their output, and `/agents kill <job id>` stops one;
  `/lsp restart <name>`;
  `/mcp restart|sign-in|sign-out <name>`.

HTTP MCP servers that need an account show "needs sign-in" in `/mcp`: its Sign in action opens
the browser, and zemi keeps and refreshes the grant. zemi registers itself with the server on
the fly; a server that answers 404 to that says so, and needs `client_id` (and
`client_secret` when it issued one) from an app registered by hand in `zemi.mcp.add`, plus
`callback_port` when that app's redirect URI is `http://localhost:<port>/callback` with a fixed port. MCP servers:
`zemi.mcp.add("name", { command = ..., args = ... })` or `{ url = ... }` in `init.lua`.
A command server starts in zemi's data dir, not the project, with the project path in
`ZEMI_PROJECT_DIR`: pass the project as an absolute path, not `.`.
Past ten MCP tools (after `enabled_tools` and `disabled_tools`) their schemas stop riding
every request: the model finds them through `tool_search`, whose description lists them by
server, and a search loads at most five for the rest of the session. `/usage` counts the
catalog and the loaded tools in its mcp row.
In auto mode a tool the server marks `readOnlyHint` runs without asking; every
other MCP tool asks the user. An image or audio part of a result reaches the model
as itself, not as a placeholder.

## Tools and permission rules

Model tools: `bash`, `read`, `write`, `edit`, `multiedit`, `glob`, `grep`, `fetch`,
`web_search`, `todos`, `question`, `plan`, `job_start`, `job_read`, `job_list`, `job_kill`,
`memory`, `skill`, `diagnostics`, `lsp`,
`agent`, `workflow`, `lua` (runs Lua inside zemi as user origin; read-only calls run at
once, a script that calls anything else asks the user once, and a read-only agent or an
untrusted project asks for every script), plus
`mcp_<server>_<tool>` and tools registered from Lua. Agent `tools:` frontmatter and a profile's
`tools` use these names: a plain name allowlists, `-name` removes one and keeps the rest, so a
list that only removes still gets whatever a later release adds. A name nothing answers to
fails an agent file and lands in `/errors` for a profile. `tool_search` (past ten MCP tools)
is added after that filter, so no list can name it.

Asking the user: `question` shows 1-4 questions; the user picks an option (space marks several
in a multi-select) or types an answer on the last row, and esc dismisses, which ends the turn.
Several questions show as tabs (tab/shift+tab, or left/right) ending in a review tab that lists
every answer; enter on a row there reopens that question, and submit sends them all, unanswered
ones included. `plan` opens a "Plan" popup with the full plan as scrollable
markdown: enter approves, typing asks for changes (the agent revises and presents again), and
esc dismisses. The agent and mode that ran before the planner are remembered, yolo included, so
approving returns to them without a per-plan prompt.
Without a UI (`zemi -p`) both tell the model nobody can answer, and it decides itself.
Subagents cannot ask.

Rule subjects: `tool.<name>` (`tool.bash` patterns match the normalized command),
`fs.read`, `fs.write` (path patterns), `net.<host>` (the model's `fetch` and `web_search`),
`mcp.<server>` (tool name as pattern), `lua.<ns.call>` for calls config code makes:
`lua.fs.write` (path), `lua.job.start` (the program and its args as a normalized command
line, like `tool.bash`), `lua.http.<method>` (the URL; `net.<host>` does not cover
`zemi.http`). Such a call never prompts: without an allow rule it raises, and the error
holds the rule to add.

```lua
zemi.permission.rule.add({ subject = "lua.http.get", pattern = "https://api.github.com/*",
  decision = "allow", match = "https://api.github.com/repos/o/r" })
```

A subject ending in `.*` covers its family, so one `net.*` rule answers every `net.<host>` ask. Deny beats ask
beats allow; within one decision the most specific pattern wins. In a command pattern `*`
spans spaces and `/`, so `cat docs/*` also allows `cat docs/a /etc/passwd`; an argument that
climbs with `..` is matched cleaned as well and the stricter verdict holds, so
`cat docs/../.env` asks, and so does a path the shell still expands (`docs/{a,b}`, `docs/$X`,
`docs/.*`).

For the full API see the zemi-lua skill.

## Project config

A repository can carry its own zemi setup in `.zemi/` at its root (found from any
subdirectory up to the `.git` root), like `.claude/` or `.crush/`:

```
.zemi/init.lua          entry point: widgets, keymaps, commands, deny/ask rules
.zemi/lua/<mod>.lua     modules for project code: require("mod")
.zemi/agents/<n>.md     subagents (frontmatter description required, name, tools, model; body = prompt)
.zemi/commands/<n>.md   prompt commands: /<n> sends the body ($ARGUMENTS, $1..$9)
.zemi/workflows/<n>.lua workflows: /workflow <n>
```

The same `agents/`, `commands/` and `workflows/` directories work under
`~/.config/zemi/` for every project. `agents/`, `commands/` and `skills/` are also read
from `.agents/` and `.claude/` at the repository root, so files written for another
harness load unchanged; the nearest `.zemi` wins a name clash.

It loads only in a project the user trusts. The first start in a repository asks once
(`y` trust, `n` don't trust, both saved, or a click on the row; esc, enter or ctrl+c: not now,
asked again next start),
and `/trust` asks again any time. The decision covers the whole repository, its linked
worktrees included, and later changes to `.zemi/` do not ask again. Project code
cannot register providers, MCP or LSP servers, allow rules, permission modes,
`tool.pre` rewrites, builtin tool names or `agent.assemble` sections, cannot set settings,
call `zemi.agent.run`, `zemi.agent.send`, `zemi.agent.profile`, `zemi.workflow.run`,
`zemi.model.set`, `zemi.config.reload`, `zemi.memory.add` or the `zemi.session` and
`zemi.composer` mutators, run a Go command or a more trusted origin's command, deny quitting, read secret-looking env vars, or override user registrations; put personal settings in `~/.config/zemi/init.lua` instead.

## Plugins

A plugin is a directory `~/.local/share/zemi/site/pack/<any>/start/<name>/`: its
`plugin/*.lua` files run at startup, before init.lua, with the `plugin` origin, and its
`lua/` holds modules anyone can `require`. A plugin may add commands, keymaps on free keys,
widgets, overlays, panels, health rows, tools, completions, deny and ask rules, event
handlers and `agent.assemble` sections, and read files in the project. It cannot, today:

- start a job, make an HTTP request, or write a file: each raises until the
  user's init.lua has an allow rule for it (`lua.job.start`, `lua.http.<method>`,
  `lua.fs.write`), and a plugin cannot add allow rules. Print the rule in the plugin's
  README, or return the specs from a function the user loops over. No permission mode
  settles these, yolo included. A plugin's tool needs the user's `tool.<name>` allow rule
  the same way, or every call asks.
- edit or submit the composer (an overlay item's `fill` still fills it), send to the agent
  (`zemi.agent.send`, `run`, `profile`, `zemi.workflow.run`), or use `zemi.fs.datadir`. No
  rule changes this.
- set settings, the theme, a highlight, the layout, the focus or the tab title, switch the
  model, session or permission mode, reload, register providers, MCP or LSP servers, enable
  skills, decide proposals, read secret-looking env vars, run a Go command or a more
  trusted origin's command, or replace anything user config or zemi registered.

`plugin/*.lua` runs before init.lua, so it cannot read options: register with defaults
there and make `setup(opts)` safe to call again from init.lua. The plugin tree is not
watched; after editing a plugin, `/reload`.

What runs is judged by who registered the callback, not by where the function was written.
So a plugin that needs the second group ships it as a module and the user wires it in
init.lua, where it runs as user config:

```lua
-- ~/.config/zemi/init.lua
local snippets = require("snippets")   -- site/pack/x/start/snippets/lua/snippets/init.lua
zemi.command.add("snip", snippets.pick, { desc = "insert a snippet" })   -- may call zemi.composer.insert
```

## Splitting config into files

`require("name.sub")` searches `lua/` under the config dir (and plugins; project code
also searches `.zemi/lua/` first). Relative names resolve next to the file that calls
`require`, also from functions that run later:

```lua
-- ~/.config/zemi/init.lua
require("./keys")                 -- ~/.config/zemi/keys.lua
local ui = require("./lua/myui")  -- ~/.config/zemi/lua/myui/init.lua or myui.lua
-- inside lua/myui/init.lua
local util = require("./util")    -- lua/myui/util.lua, same instance as require("myui.util")
```

A relative path must stay inside its own tree: user files cannot reach project or plugin
files that way, and project files cannot reach the user config.
