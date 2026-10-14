# Plugins (`<data>/plugins/local/<name>/`)

A plugin is one directory in the local plugins directory the system prompt names, its zemi code under `.zemi/` and the dirs it layers over the user's at its root. At startup zemi runs every enabled plugin's `.zemi/init.lua`, sorted by name, before the user's init.lua:

```
<name>/                  the plugin's name: lowercase letters, digits, "_", "-", starting with a letter or digit
  .zemi/plugin.json      optional: its version, description and what it uses (below)
  .zemi/init.lua         runs as the `plugin` origin; line 1 "-- <what it does>" is its description when
                         the manifest has none
  .zemi/lua/<name>/init.lua    require("<name>") from the plugin and the user's init.lua
  .zemi/tests/<topic>.lua      its tests (Testing, below); shared code in .zemi/tests/lib/
  .zemi/workflows/<name>.lua   saved workflows
  skills/  agents/  commands/  as in the config dir; the user's and zemi's own win a name, and a skill's
                         zemi-skill.lua does not run
  README.md stylua.toml  what the repository needs; zemi writes .luarc.json and .emmyrc.json beside them
```

Everything zemi runs or reads as Lua goes under `.zemi/`; the layer dirs stay at the root, where a repository's own conventions live. There is no other shape: a plugin with `init.lua`, `lua/`, `tests/` or `workflows/` at its root is refused, with `/errors` saying where its Lua goes.

`.zemi/plugin.json` is the manifest: `{ "version": "1.2.0", "description": "what it does, one line", "uses": ["model", "http"] }`, every field optional. It marks the directory as a zemi plugin and is what `/plugins` and `zemi plugins` show; a plugin without one reports the checked-out `vX.Y.Z` tag (else the short commit) and `.zemi/init.lua`'s first line, as it always has. Write one for a new plugin, and bump its `version` when the user asks for a release; what `install` and `update` follow is still the git tag a published plugin carries. The name is never in it: the directory is the plugin's name in the settings keys (`plugins.<name>.<key>`), the principal (`plugin:<name>`) and every verb. `installed/<name>/` holds the clones the user manages with `/plugins` (or `zemi plugins` in a shell: install from a git URL, check and update, turn on and off, delete, which removes a local plugin too and asks first), each with the `origin` remote as its source; `<data>/plugins/disabled` lists the names turned off. You never write there; to change one, the user deletes it and you clone it into `local/`. A local plugin named like an installed one, or a symlink in `local/`, does not load.

`uses` is what the plugin calls, and the user reads it before switching the plugin on: enabling a plugin grants exactly the names it declares and nothing else. A name outside this set refuses the plugin at load, with the valid ones in the error.

| name | what it grants |
| --- | --- |
| `model` | `zemi.model.ask`, `zemi.classify.ask`: a model call, billed to the user |
| `agent` | `zemi.agent.send`: text into the model's conversation |
| `http` | `zemi.http.*`: a request to any URL |
| `job` | `zemi.job.start`: a command |
| `fs.write` | a `zemi.fs` write |
| `tool` | a tool the plugin registers with `zemi.tool.add`, which the model calls as subject `tool.<name>` |

Reading files needs no declaration, and neither does the plugin's own state or its settings.

## Plugin steps

`/plugins new` and `/plugins modify` send you here, or the user asked for a plugin or a change to one. A plugin is checked by its tests, never by trying it in this session.

1. New: unless the request already says, ask with the `question` tool what the plugin should do and where it shows up (a command, a key, a statusline widget, a tool the model calls, an event handler, a theme), and whether anything that runs on its own (a timer, an event handler that acts, a network call, a message to the agent) should be on from the start. In one Lua tool script, check that zemi does not do it already: `zemi.config.schema()`, `zemi.command.list()`, `zemi.keymap.list()`, `zemi.ui.widget.list()` (id, region, priority) and `zemi.tool.list()`. If a setting, a command, a prompt command (`commands/<name>.md`) or a workflow does the job, offer that instead. A change: read the plugin's files first (the prompt names its directory) and ask only if the request is unclear.
2. Say in one message what you will write. A new plugin takes a name nothing registers yet and is `.zemi/init.lua`, `.zemi/lua/<name>/init.lua`, `.zemi/tests/<topic>.lua`, `.zemi/plugin.json` (`{"version": "0.1.0", "description": "<one line>", "uses": [...]}`, the version the user sees in `/plugins` and the names it calls, `uses` above), a `README.md` naming its commands, keys and settings, and `stylua.toml` (below). Write `.zemi/init.lua` first: that write creates the plugin and is the one the user approves, so it shows them the code.
3. A widget or a key: read `ui.md` first (placement by priority, clicks, render rules). Write the code and its tests together, indented with tabs: each thing the user can see gets a case, and a fix starts with the case that fails without it. Read `lua.md` only for API you do not know (waiting on a job, HTTP or a model); for what a widget's render gets, grep `@class zemi.WidgetCtx` in the types file and read that block.
4. Run `<zemi> plugins test <name>` with the shell tool and fix until it passes, `<zemi>` being the path the zemi skill ends with ("This zemi runs from"): a bare `zemi` may be an older one on PATH. Name the plugin, not its path. The tests are the check: do not try the plugin in this session (`zemi.command.run`, keys, `zemi.ui.screenshot()`, widget listings) unless the user asks, and write the tests even then.
5. Then, once, `return zemi.errors.list()` in a Lua tool script: the tool reloads the changed plugin first and its result starts with what failed to load. Only this shows a clash with the user's own config (a key or an id they hold). Fix a `plugin <name>:` error and go back to 4.
6. Tell the user what it registered or what changed, that `/plugins` turns it off, and which README lines they may add to init.lua. Do not add them yourself.

`.zemi/init.lua` registers what the plugin is for, with defaults, so it works the moment it loads with no line in the user's init.lua: its commands, its widgets (rendering `{}` until they have something to say), its tool, its settings, and keys only where `zemi.keymap.get(key)` is nil. Every option that is a bool, a number, a string or one of a list is a setting, `zemi.config.add(key, { kind, default, desc, choices })`, which /config lists under the plugin's name and keeps across starts. The plugin's code reads and watches it by the bare key; the user's init.lua sets it as `plugins.<name>.<key>` with `zemi.config.set`. What the user did not ask for stays off behind a bool setting that defaults to false, which the user turns on in /config: timers and polling beyond what its own widgets need, event handlers that act (sending to the agent, vetoing a submit or a tool call), network calls, a key someone else holds. Plugins load before the user's init.lua, so read a setting where it is used (the timer tick, the handler, `app.ready`), or watch it (`zemi.config.watch`) to start and stop what it controls; a value read at load misses init.lua's. A plugin never writes its settings. A setting never holds a secret (a token, a password): the store keeps it in plain text and any code and the model can read it. `setup(opts)` is only for what a setting cannot hold: a function, a table, or a key the user passes from `zemi.env.get`.

`.zemi/init.lua`:

```lua
-- the git branch in the statusline
require("branch")
```

`.zemi/plugin.json`: the manifest, whose `uses` is what the plugin calls and what enabling it grants.

```json
{
  "version": "0.1.0",
  "description": "the git branch in the statusline",
  "uses": ["job"]
}
```

`.zemi/lua/branch/init.lua`:

```lua
local branch = ""
local missing -- why the branch could not be read

local function poll()
	zemi.job.start({
		cmd = "git",
		args = { "rev-parse", "--abbrev-ref", "HEAD" },
		on_exit = function(res)
			branch = res.code == 0 and res.stdout:gsub("%s+$", "") or ""
			missing = res.code ~= 0 and res.stderr:gsub("%s+$", "") or nil
		end,
	})
end

zemi.health.add("branch", function()
	return missing and { level = "warn", detail = missing } or "reading the branch"
end)

zemi.ui.widget.add({
	id = "branch",
	region = "statusline.right",
	priority = 50,
	render = function() return branch == "" and {} or { { { text = branch, hl = "Dim" } } } end,
})

zemi.config.add("interval", {
	kind = "number",
	default = 30000,
	desc = "how often the branch is read, in ms",
})

zemi.event.on("app.ready", function()
	zemi.timer.after(0, poll) -- an event handler has 50 ms, a timer callback 1 s
	local tick = zemi.timer.every(zemi.config.get("interval"), poll)
	zemi.config.watch("interval", function(ms)
		tick:stop()
		tick = zemi.timer.every(ms, poll)
	end)
end)
```

The README lists its settings and what the user may add to their init.lua. The plugin's declared calls are settled by the manifest, so put what it calls in `uses` and say so there.

`stylua.toml`, the same in every plugin, so the user's editor formats it the way zemi's own Lua is:

```toml
# Every option stylua 2.0 knows is pinned. block_newline_gaps (2.2+) is not:
# its default is what older builds do, and naming it makes 2.0 and 2.1 refuse the file.
syntax = "Lua51"
column_width = 120
line_endings = "Unix"
indent_type = "Tabs"
# how wide a tab counts against column_width: four cells, as zemi draws one
indent_width = 4
quote_style = "AutoPreferDouble"
call_parentheses = "Always"
collapse_simple_statement = "Always"
space_after_function_names = "Never"

[sort_requires]
enabled = false
```

## Identity and permissions

Identity: a plugin's code runs as the plugin wherever it is called from, its `setup()` in the user's init.lua included. A command, keymap, widget, theme, tool, completion source or health row another plugin registered first (by name) is refused, with both named in `/errors`, and a user or builtin one always wins, so prefix ids with the plugin name and bind only keys nothing else uses. Its `zemi.state` keys are its own (stored under `plugin:<name>:`) and go when it is deleted or installed again, like the settings the user picked for it; turned off, it keeps both, and init.lua's `zemi.config.set` lines for its settings do nothing. Only it answers `require("<name>...")`, and another plugin gets its modules read-only.

Permissions: the manifest declares what the plugin calls, and enabling it grants exactly that. A call the declaration covers runs with no prompt, and one it does not is refused, naming the line to add: `lua.http.get https://x: plugin jira declares no "http"; add "http" to uses in .zemi/plugin.json`. Never an ask: the manifest is the decision the user made. A read-only agent changes nothing, its writes included. Enabling grants nothing else: a provider, MCP server or language server stays the user's own line in init.lua. A plugin cannot edit the composer, change settings, the theme, the layout, the model or the mode, or read secret-shaped env vars: a key comes through `setup({ key = zemi.env.get("NAME") })` in the user's init.lua.

What a plugin cannot do at all: register providers, MCP servers or language servers (they hold keys or run outside the project, so the README gives the user the `zemi.provider.add` or `zemi.mcp.add` line to paste), rewrite the conversation or the provider request, draw its own transcript rows or a panel, change the tool set per turn (a profile does it once), or replace a builtin tool (`zemi.tool.override` adds `pre` and `post` around one). Each would break the prompt cache, the conversation or the UI rules below.

## A classifier

A decision is not a sentence, so ask for one: `zemi.classify.ask` answers typed questions about a state, with a probability, and never leaves the caller parsing text. It is a model call, so the manifest declares `model` (above). Give the model its own slot so a user who keeps a cheap one for this can say so - `zemi.model.add("classifier", { desc = "grades states", classify = true })`, which follows `models.chat` until the user sets `models.classifier` from `/model`. `classify = true` is what makes it a classifier slot: `/model classifier` then offers a decisions model (a `system_one` provider's, which answers the question itself with real probabilities) beside the chat ones, and the slot asks through whichever the user picked. A slot added by a plugin disappears with it, the user's pick included.

```lua
-- .zemi/lua/<name>/init.lua: judge a state with one classify call
zemi.model.add("classifier", { desc = "grades states", classify = true })

local res = zemi.classify.ask({
	model = "classifier",
	state = {
		branch = "feat/retry",
		behind = 12,
		ci = "green",
	},
	questions = {
		merge = {
			type = "bool",
			instructions = "Is this branch ready to merge?",
			criteria = {
				["true"] = "the CI is green and the branch is at most a few commits behind",
				["false"] = "the CI is red or the branch is far behind",
			},
		},
	},
}):wait()
```

`state` is the data to judge and goes to the model as data, not as prose the plugin wrote; a state never carries instructions that matter, and the strategies tell the model so. A question is `type` (`bool`, `choice` or `score`), `instructions` in one line, and `criteria`: a bool's `{ ["true"] = "...", ["false"] = "..." }`, a choice's `{ label = "what it means" }`, or a score's levels lowest first. The answer for a bool is `{ value, probability }`; `choice` gives `{ choice, probabilities, confidence }`, `score` gives `{ score, confidence }`. `res.strategy` says who answered (`protocol`, `logprobs` or `schema`); the last one has no probabilities, so `probability` and `confidence` are absent there. Keep the state small and ask one question per answer you need.

A failure is a state, not a crash: `zemi.classify.ask` returns `nil, err` when no strategy answered (a key not exported, a provider down, a model that cannot answer the question), so the plugin reports it in a `zemi.health.add` row.

## Style

So a plugin looks like zemi: colors are theme groups (`Accent`, `Dim`, `Ok`, `Warn`, `Danger`, ...), never hex, so the user's theme applies. No state change adds or removes a row: status goes in a statusline widget that renders `{}` when there is nothing to say (with a high priority, since it comes and goes) or in a toast (`zemi.notify`), whose title says what happened like zemi's own: "Plugin installed" with the name in the body, "Could not reach the server" with the cause in the body. A tool's call row is zemi's own, labeled by a string `about` parameter, with `run:progress(text)` for the running line. Slow work goes in a `wait` inside a task or a job's `on_exit`, never in a render.

## Developing one

- Reads under `local/` need no approval. A write asks in ask mode like any write, since plugin code runs in every project. Say in one message what you will write.
- zemi reloads within a second of an edit once no turn runs, and the Lua tool reloads before its script. A load error is in `zemi.errors.list()` (`plugin <name>: ...`), and the plugin's `/plugins` row reads `failed` with the error in its detail.
- zemi writes the plugin's `.luarc.json` and `.emmyrc.json` (editor types, with this machine's
  paths, one per editor): never write them, and add both to `.gitignore` when publishing. With
  lua-language-server or EmmyLua installed, a write to a plugin file reports the errors zemi's
  types find: an unknown global, a missing field, a wrong type or argument count.
- Moving a feature out of the config dir is the one plugin task that edits init.lua: say which lines leave it and which stay, and wait for the user's yes. Then put its registrations in `local/<name>/.zemi/init.lua` and its modules in `.zemi/lua/<name>/`, turn its options into settings (`zemi.config.add`) whose values the user's init.lua sets with `zemi.config.set`, leave `require("<name>").setup(...)` there only for a function or a key, and keep there what only user config may do (replacing a shipped widget, `hl.set`, layout, settings). Its old `zemi.state` keys stay behind.

## Testing

`<zemi> plugins test <name|dir> [pattern]`, by the path your prompt names, runs every case of `.zemi/tests/*.lua` in a fresh zemi holding only the plugin: no user config, a clock at 2026-01-01 00:00 UTC that only `test.advance` moves. Each top-level file there is a test file, and one that declares no case fails; code the files share goes in a subdirectory (`.zemi/tests/lib/x.lua`, loaded with `require("./lib/x")`). A file's top level runs where `init.lua` would, before `app.ready`: stub there what the plugin calls at boot. Drivers run only inside a case. A job, HTTP request, model ask, classify call, agent send or `zemi.fs` write raises unless a stub answers it, and the error names the stub to add. Drivers wait until zemi is quiet and return `nil, err` instead of raising, so call them bare, never under `pcall` or `test.raises`; an error zemi records meanwhile fails the case at the call.

```
local test = zemi.test
test(name, fn)                  a case, at the file's top level
test.run(cmd, args?)            the plugin's command as typed -> true | nil, err
test.keys("ctrl+x d")           what the plugin bound a key sequence to -> true | nil, err
test.tool(name, input)          the model's call of the plugin's tool -> output | nil, err
test.render(id, ctx?)           a widget's lines as text, then as segments (width 80, height 1)
test.click(id, click)           the widget's on_click for a segment's click id
test.emit(event, ev?)           a host event to the plugin's handlers
test.complete(line)             the rows the plugin's completion offers for a draft line
test.overlay()                  the top overlay { id, title, body, items, input }, or nil
test.answer(choice, action?)    enter on an item (index or text) or its action; nil is esc
test.advance(ms)                move the clock, firing due timers
test.set(key, value)            one of the plugin's settings, as /config changes it
test.stub(subject, pattern?, reply?) -> requests
                                answer lua.http.<method>, lua.job.start, lua.model.ask,
                                lua.classify.ask, lua.agent.send or lua.fs.write; false refuses
test.eq(got, want)  test.neq(got, other)  test.contains(s, sub)  test.raises(fn, sub?) -> message
```

Prefer `eq` with the exact value: `neq` passes on any wrong one. Reply fields and their defaults: http `{ status = 200, body = "", headers = {} }`, job `{ code = 0, stdout = "", stderr = "" }`, model a string or `{ text }`, classify the whole result `{ strategy = "...", answers = { <question> = { value = true, probability = 0.9 } } }`; `{ error = "..." }` (http and model replies) fails at the transport. Plugin code can tell it is under test, so passing tests are no review of what it does. `.zemi/tests/branch.lua` for the plugin above:

```lua
local test = zemi.test
local git = test.stub("lua.job.start", "git rev-parse *", { stdout = "main\n" })

test("the widget shows the branch git reports", function()
	test.eq(test.render("branch"), { "main" })
	test.eq(git[1].args, { "rev-parse", "--abbrev-ref", "HEAD" })
end)

test("a checkout shows on the next poll", function()
	test.stub("lua.job.start", "git rev-parse *", { stdout = "fix-login\n" })
	test.advance(30000)
	test.eq(test.render("branch"), { "fix-login" })
end)

test("the interval setting moves the poll", function()
	assert(test.set("interval", 10000))
	test.stub("lua.job.start", "git rev-parse *", { stdout = "fix-login\n" })
	test.advance(10000)
	test.eq(test.render("branch"), { "fix-login" })
end)
```

## Publishing

`git init`, commit, push to a host the user picks, then tag `v0.1.0` and push the tag, with the manifest's `version` in step with it: `/plugins` shows the manifest's, and it is what to bump. An install checks out the newest `vX.Y.Z` tag and an update stays in its major version (one installed from a branch moves to the highest tag once that tag is ahead of it), so a breaking change bumps the major. Users install with `/plugins install <git url>` or `zemi plugins install <git url>`.
