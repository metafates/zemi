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
  start; `a` in a permission prompt allows until zemi exits. A leftover `permissions.lua`
  is not read.
- File writes ask first and show the diff, and write, edit and multiedit
  refuse a file the model has not read or that changed since it read it. zemi reloads when
  config files change; `/reload` forces it.
- Never write API keys into files: read them with `zemi.env.get("NAME")`.

## Providers and models

```lua
zemi.provider.add({
  id = "anthropic",
  kind = zemi.provider.kind.ANTHROPIC,        -- or OPENAI, OPENAI_COMPAT, GOOGLE
  api_key = zemi.env.get("ANTHROPIC_API_KEY"),  -- the key from the environment
  -- base_url = "https://llm.internal/v1",   -- required for openai_compat
  -- headers = { ["X-Team"] = "core" },     -- sent with every request; as secret as the key
  models = {
    { id = "claude-sonnet-4-5", display = "Sonnet 4.5",
      context_window = 200000, cost_per_1m_in = 3, cost_per_1m_out = 15 },
  },
})
zemi.config.set("agent.model", "anthropic/claude-sonnet-4-5")
```

`images = false` marks a model that refuses image input: `read` on an image then returns a
text error and `@image` attachments are omitted with a note. Without it zemi learns the same
from the provider's 400 over an image: the turn goes on with notes in place of the images and
the model stays text-only for the process; the flag makes that permanent.

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
in Lua (`Authorization = "Bearer " .. zemi.env.get("GITHUB_TOKEN")`). `/model` opens a picker;
`/model provider/model` switches directly.

## Settings

`zemi.config.get(path)`, `zemi.config.set(path, value)`, `zemi.config.watch(path, fn)`.
A reload keeps a value picked at runtime (`/model`, `/effort`, the mode cycle, `-m`) unless
init.lua now sets that key to a different value.
`zemi.config.schema()` lists every key with kind, default and description; prefer it over
guessing. Common keys:

- `agent.model`, `reflector.model` (empty: use the agent model)
- `agent.subagent_model`: default model for subagents whose definition sets none (empty: the
  session model); a cheaper model here keeps searches cheap. The agent may still pick another
  configured model per run.
- `agent.small_model`: a fast, cheap model for small jobs (session titles, the builtin `web`
  agent, and auto-mode screening unless `permissions.auto_model` is set); empty uses the agent model, e.g.
  `zemi.config.set("agent.small_model", "anthropic/claude-haiku-4-5")`.
- `web.search_url`: a SearXNG endpoint (`http://localhost:8888/search`, json format enabled)
  for `web_search`; empty uses DuckDuckGo Lite, which blocks some networks with a bot check.
- `agent.effort`: `low`, `medium`, `high` or empty (provider default); `/effort` sets it.
  Anthropic models get the effort parameter, OpenAI and compatible ones reasoning_effort.
- `ui.window.unfocus` (ctrl+\): the key that takes typing back from a focused window.
- `editor.command` (empty: `$VISUAL`, then `$EDITOR`, then vi) is the editor for
  `open_in_editor`, `zemi.ui.edit` and the composer draft (ctrl+g). It runs in a focused
  float over zemi with the user's full environment; ctrl+c goes to the editor, `ui.window.unfocus`
  shrinks it to uncover the composer, and the window closes when it exits. × or `/win close <id>` kills a running editor only when repeated.
- `agent.context_window`: tokens, overrides the model's `context_window` (0 = the model's)
- `compact.model`: model that writes compaction summaries (empty uses `agent.model`)
- `session.auto_title` (true): name sessions from the first exchange with `agent.small_model`
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
- `ui.chat.reasoning` (false): finished reasoning blocks start expanded; ctrl+o toggles
- `ui.completion.files`, `ui.completion.max_items`
- `memory.digest_lines` (12): lines of the memory digest; 0 leaves it out of the prompt
- `memory.autonomy` (`ask`): `auto` stores memories the model or the reflector propose
  without review, except instruction-shaped text, retirements, replacements of the user's
  own memories, and anything from an untrusted project or a session that used `fetch`,
  `web_search`, the `Lua` tool or a tool zemi does not ship (MCP, `zemi.tool`)
- `permissions.mode`: `manual` (the user answers asks), `plan` (file writes are refused,
  reading commands and builds and tests run, the latter sandboxed with the project read-only,
  other commands and MCP calls ask, so the agent investigates and
  proposes a plan), `auto` (a model
  approves routine actions and hands the rest to the user) or `yolo` (every ask is
  allowed; deny rules still apply). `permissions.auto_model` picks a cheaper screening model.
  In a project the user has not trusted (`/trust`), auto and plan mode run only commands
  that just read (no builds, tests or git), auto edits nothing and screens nothing without
  the user, and the Lua tool asks. In any project the Lua tool asks before a script calls
  anything that is not a read (config, providers, permissions, jobs, windows, servers, the
  composer, ...). Commands auto and plan mode run without asking run in a
  sandbox (seatbelt on macOS, Landlock on Linux): writes only in the project (not in plan
  mode), the scratchpad and temp dirs, `.git` and `.zemi` read-only, no network beyond
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
  secret-shaped file are never run without the screening model (auto) or the user. On Linux the sandbox cannot hide `.env`, so a
  build or test flag that runs code can read it in a trusted project. A command that needs the network is retried with `network: true` (the sandbox stays, sockets are added); one that needs more with `sandbox: false`, which asks the
  user (not with `-p`, where nobody can answer). Typing into a window auto mode settles only
  when the window itself runs sandboxed; a text window (`tool.window.text`) runs nothing, so
  every mode but manual settles it. Giving a window the user's keyboard (`focus`) always asks,
  whatever the mode. Without a sandbox (`/health` sandbox row) they run only commands that just read,
  as in an untrusted project. At runtime:
  `/mode <name>`, `tab` (next) or `shift+tab` (previous); for one run:
  `zemi --mode auto`. `/permissions` lists the mode and every rule in force; an "always"
  answer (the only rule made at runtime) has a Remove action there, config rules do not. Widgets read it as `ctx.permission_mode` (and its behavior as
  `ctx.permission_mode_base`). Suggest yolo only when the user asks for it.
- Modes are configurable in init.lua. The start mode is `zemi.config.set("permissions.mode", "auto")`.
  `zemi.permission.mode.del("yolo")` takes a mode out of tab and `/mode`, and
  `zemi.permission.mode.add(name, { base = "manual"|"plan"|"auto"|"yolo", description, prompt, rules })`
  adds one (or replaces one with that name) after the others. It behaves like its base; its
  `rules` (`{ "tool.bash", "go test*", "allow" }`, `{ "tool.bash", "git push*", "deny" }`) answer
  what would otherwise ask, and `prompt` joins the system prompt while it is on:

  ```lua
  zemi.permission.mode.del("yolo")
  zemi.permission.mode.add("tests", {
    base = "manual", description = "tests and git reads run, the rest asks",
    rules = { { "tool.bash", "go test*", "allow" }, { "tool.bash", "git status*", "allow" } },
  })
  ```

## Command line

`zemi [flags] [prompt]`: `-p` answers on stdout without the TUI (piped stdin joins the
prompt), `-c` continues the last session, `-r <id>` resumes one, `-m provider/model`,
`--mode manual|plan|auto|yolo`, `--output text|json` (json needs `-p`), `-v` version,
`--health` prints the checks `/health` shows and exits 1 when any of them errors.
A prompt starting with a registered command runs it instead of reaching the model, so
`zemi -p "/review HEAD~1"` or a `commands/*.md` name works from a script or a git hook.
`zemi --help` has the full list. With `-p` nobody answers asks, so they are denied unless
the mode is auto or yolo; a run that had calls denied says so on stderr.

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
-- zemi.permission.mode.cycle, zemi.permission.mode.prev
-- (focus, composer and mode actions: user config only; zemi.HostAction in the type
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
`init.lua` replaces the builtin one (`chat`, `splash`, `statusline.mode`, `statusline.cwd`,
`statusline.info`, `statusline.learn`, `statusline.agents`, `statusline.reflect`,
`statusline.view`):

```lua
zemi.ui.widget.add({
  id = "statusline.branch", region = "statusline.left", priority = 40,
  render = function(ctx) return { { { text = "main", hl = "Comment" } } } end,
})
```

Attaching: `@path` attaches a project file; dragging files onto the terminal attaches them
(images from anywhere are copied as `@drop:<name>`), and ctrl+v pastes an image from the
clipboard. Images must be png, jpg, gif or webp up to 5 MB, other dropped files UTF-8 text;
secret-looking paths are refused. Images reach the model only if it accepts images.

Slash commands in the palette: `/new`, `/sessions`, `/rewind`, `/compact`, `/model`,
`/mode`, `/permissions`, `/memory`, `/skills`, `/agents`, `/workflow`, `/review`, `/mcp`, `/init`,
`/usage`, `/prompt`, `/copy`, `/trust`, `/health`, `/errors`, `/messages`, `/help`, `/quit`. They also run when typed but stay out of the
palette: `/resume`, `/fork`, `/export`, `/effort`, `/lsp`, `/reflect`,
`/reload`, `/win` (a menu of windows; `focus`, `hide`, `show <id>`, `close <id|all>`). Arguments:

- `/sessions`: enter on an earlier session offers Resume and Delete. Delete removes its
  messages, subagent runs and scratchpad from disk; memories learned from it stay.
- `/rewind [id]`: back to an earlier prompt. Files changed by write, edit and multiedit
  return to their state before it, the conversation is cut there and the prompt comes back
  in the composer. Bash changes are not undone. Lua: `zemi.session.rewind(id)`.
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
the browser, and zemi keeps and refreshes the grant. MCP servers:
`zemi.mcp.add("name", { command = ..., args = ... })` or `{ url = ... }` in `init.lua`.
A command server starts in zemi's data dir, not the project, with the project path in
`ZEMI_PROJECT_DIR`: pass the project as an absolute path, not `.`.
In auto mode a tool the server marks `readOnlyHint` runs without a screening call; every
other MCP tool is screened or asked. An image or audio part of a result reaches the model
as itself, not as a placeholder.

## Tools and permission rules

Model tools: `bash`, `read`, `write`, `edit`, `multiedit`, `glob`, `grep`, `fetch`,
`web_search`, `todos`, `question`, `plan`, `open_in_editor`, `window`, `job_start`, `job_read`, `job_list`, `job_kill`,
`memory_add`, `memory_search`, `memory_update`, `memory_forget`, `skill`, `diagnostics`, `lsp`,
`agent`, `workflow`, `Lua` (runs Lua inside zemi as user origin; read-only calls run at
once, a script that calls anything else asks the user once, and plan mode or an untrusted
project asks for every script), plus
`mcp_<server>_<tool>` and tools registered from Lua. Agent `tools:` frontmatter and a profile's
`tools` use these names: a plain name allowlists, `-name` removes one and keeps the rest, so a
list that only removes still gets whatever a later release adds. A name nothing answers to
fails an agent file and lands in `/errors` for a profile.

Asking the user: `question` shows 1-4 questions; the user picks an option (space marks several
in a multi-select) or types an answer on the last row, and esc dismisses, which ends the turn.
Several questions show as tabs (tab/shift+tab, or left/right) ending in a review tab that lists
every answer; enter on a row there reopens that question, and submit sends them all, unanswered
ones included. `plan` puts a plan in the chat and asks "Implement this plan?": approve, or
type what to change and the agent revises it. In plan mode each approval names the mode to
implement in (the mode used before plan mode first, never yolo), so approving leaves plan mode.
`window` runs a program in a real terminal drawn inside zemi (a split below the chat, a vsplit
sidebar beside it, or a float over it; `zemi.win` is the same from Lua): open, read the screen
as text (`expect` waits for text to appear, `scrollback` adds lines that scrolled off), press
keys in neovim notation (`ihello<esc>:wq<cr>`, `<c-x>`, `<s-tab>`), click or scroll at a cell,
`set` the kind, size (`frac`, `cols`, `rows`), position (`x`, `y`) or title of an open window,
`hide` and `show` it (it keeps running off screen), wait for exit, close. It sees only the
windows the model opened: the user's, Lua's and the editor's are not its to read or close.
Subagents have the tool too; their windows open hidden until shown. The user drags a float by its title, resizes it by
its border or a corner, a vsplit by its separator and a split by its title. The user clicks a window to type into it and presses
`ui.window.unfocus` (default ctrl+\) to come back; a click on × closes it, and a drag over a
window nobody is typing in copies its text. Use it to show a TUI
you built, a log, a REPL, then read it back to see what the user sees.
`text` opens a panel that shows text instead of a program (`markdown` renders it like the chat),
and `set_text` rewrites it in place while the work runs: benchmark or test numbers refreshed as
each run finishes, the handful of lines that matter out of a long build or log, the checklist
being ticked off. A panel nobody gave a `frac`, `cols` or `rows` fits its body, at most half the
area it sits in, and re-wraps when the user resizes; panels survive `/reload` and a new session.
Nothing you can simply say in a reply is worth a panel, and one that stops changing is worth
closing. Images (`chafa x.png`)
show as pixels when zemi runs directly in Ghostty or kitty, and as colored symbols elsewhere
(tmux and NO_COLOR included); a window's text reads an image's cells as blanks.
`open_in_editor` opens a file in the user's editor (`editor.command`) and waits
until they close it; the result is a diff of what they changed. It is for changes only the user
can make, or when they ask to edit something themselves; subagents never get it.
Without a UI (`zemi -p`) both tell the model nobody can answer, and it decides itself.
Subagents cannot ask.

Rule subjects: `tool.<name>` (`tool.bash` patterns match the normalized command),
`fs.read`, `fs.write` (path patterns), `net.<host>`, `mcp.<server>` (tool name as
pattern), `lua.<ns.call>` such as `lua.fs.write`, `lua.job.start` (its patterns match the
program and its args as a normalized command line, like `tool.bash`). A subject ending in
`.*` covers its family, so one `net.*` rule answers every `net.<host>` ask. Deny beats ask
beats allow; within one decision the most specific pattern wins.

Regions: `header`, `chat`, `statusline.left`, `statusline.right`. `render` must be pure and
fast: a `zemi.*` call that changes anything (registering, `config.set`, `state.set`, `fs`
writes, jobs, http, `agent.send`) raises inside it. A widget redraws only when something
else does, so one with its own clock calls `zemi.ui.redraw` from a `zemi.timer.every`
callback. For the full API see the zemi-lua skill.

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
`~/.config/zemi/` for every project.

It loads only in a project the user trusts. The first start in a repository asks once
(`y` trust, `n` don't trust, both saved, or a click on the row; esc, enter or ctrl+c: not now,
asked again next start),
and `/trust` asks again any time. The decision covers the whole repository, its linked
worktrees included, and later changes to `.zemi/` do not ask again. Project code
cannot register providers, MCP or LSP servers, allow rules, permission modes,
`tool.pre` rewrites, builtin tool names or `agent.assemble` sections, cannot set settings,
call `zemi.agent.run`, `zemi.agent.send`, `zemi.agent.profile`, `zemi.workflow.run`,
`zemi.model.set`, `zemi.config.reload`, `zemi.memory.add` or the `zemi.session` and
`zemi.composer` mutators, run a Go command or a more trusted origin's command, touch windows
it did not open, deny quitting, read secret-looking env vars, or override user registrations; put personal settings in `~/.config/zemi/init.lua` instead.

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
