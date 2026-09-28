# Plugins (`<data>/plugins/local/<name>/`)

A plugin is a directory shaped like the config dir, in the local plugins directory the system prompt names. At startup zemi runs every enabled plugin's `init.lua`, sorted by name, before the user's init.lua:

```
<name>/                  the plugin's name: lowercase letters, digits, "_", "-", starting with a letter or digit
  init.lua               runs as the `plugin` origin; line 1 "-- <what it does>" is its /plugins description
  lua/<name>/init.lua    require("<name>") from the plugin and the user's init.lua
  skills/  agents/  commands/  workflows/    as in the config dir; the user's and zemi's own win a
                         name, and a skill's zemi-skill.lua does not run
  tests/<topic>.lua     its tests (Testing, below); shared code in tests/lib/
```

There is no manifest: the version is the checked-out `vX.Y.Z` tag, the source the `origin` remote. `installed/<name>/` holds the clones the user manages with `/plugins` (or `zemi plugins` in a shell: install from a git URL, check and update, turn on and off, delete, which removes a local plugin too); `<data>/plugins/disabled` lists the names turned off. You never write there; to change one, the user deletes it and you clone it into `local/`. A local plugin named like an installed one, or a symlink in `local/`, does not load.

## New plugin

The user asked for one, or `/plugins new` sent you here:

1. Read `lua.md`. Unless the request already says, ask with the `question` tool what the plugin should do and where it shows up: a command, a key, a statusline widget, a tool the model calls, an event handler or a theme. Ask whether anything that runs on its own (a timer, an event handler that acts, a network call, a message to the agent) should be on from the start.
2. Before writing code, check whether zemi already does it. With the Lua tool, read `zemi.config.schema()`, `zemi.command.list()`, `zemi.keymap.list()`, `zemi.ui.widget.list()` and `zemi.tool.list()`; also read the shipped UI source and the plugins directory. If a setting, a shipped command, a prompt command (`commands/<name>.md`) or a workflow does the job, say which, offer that instead, and write a plugin only if the user still wants one.
3. Pick a name nothing registers yet and say in one message what you will write: `local/<name>/init.lua`, `lua/<name>/init.lua` and a `README.md` naming its commands, keys, `setup` options and every rule it needs.
4. Write it to work with no line in the user's init.lua (below), `init.lua` first: that write creates the plugin and is the one the user approves, so it shows them the code. Then run `zemi.config.reload()`, and in a second Lua tool script check `zemi.errors.list()` and the plugin's `/plugins` row, and try what it registered (`zemi.command.run`, `zemi.ui.screenshot()`).
5. Write `tests/<name>.lua`, run `zemi plugins test <name>` and fix until it passes.
6. Tell the user what it registered, that `/plugins` turns it off, and which README lines they may add to init.lua for more. Do not add them yourself.

`init.lua` registers what the plugin is for, with defaults, so it works the moment it loads with no line in the user's init.lua: its commands, its widgets (rendering `{}` until they have something to say), its tool, and keys only where `zemi.keymap.get(key)` is nil. What the user did not ask for stays off until `setup(opts)` turns it on: timers and polling beyond what its own widgets need, event handlers that act (sending to the agent, vetoing a submit or a tool call), network calls, a key someone else holds. Keep `setup` safe to call again, and start gated calls on `app.ready`: the user's allow rules are added after the plugin's init.lua runs.

`init.lua`:

```lua
-- the git branch in the statusline
require("branch")
```

`lua/branch/init.lua`:

```lua
local M = { opts = { interval = 30000 } }
local branch = ""

function M.setup(opts)
	for k, v in pairs(opts or {}) do
		M.opts[k] = v
	end
end

local missing -- the rule line while zemi.job is not allowed yet

local function poll()
	local ok, err = pcall(zemi.job.start, {
		cmd = "git",
		args = { "rev-parse", "--abbrev-ref", "HEAD" },
		on_exit = function(res) branch = res.code == 0 and res.stdout:gsub("%s+$", "") or "" end,
	})
	missing = not ok and err or nil
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

zemi.event.on("app.ready", function()
	poll()
	zemi.timer.every(M.opts.interval, poll)
end)

return M
```

The README lists what the user may add to their init.lua: the rule a gated call needs and the `setup` options. You never add them yourself unless the user asks:

```lua
zemi.permission.rule.add({
	subject = "lua.job.start",
	pattern = "git rev-parse *",
	decision = zemi.permission.decision.ALLOW,
	match = "git rev-parse --abbrev-ref HEAD",
	principal = "plugin:branch",
})
require("branch").setup({ interval = 10000 })
```

## Identity and permissions

Identity: a plugin's code runs as the plugin wherever it is called from, its `setup()` in the user's init.lua included. A command, keymap, widget, theme, tool, completion source or health row another plugin registered first (by name) is refused, with both named in `/errors`, and a user or builtin one always wins, so prefix ids with the plugin name and bind only keys nothing else uses. Its `zemi.state` keys are its own (stored under `plugin:<name>:`) and go when it is deleted or installed again. Only it answers `require("<name>...")`, and another plugin gets its modules read-only.

Permissions: a job, an HTTP request, a `zemi.fs` write, a model call (`zemi.model.ask`) or a message to the agent (`zemi.agent.send`) raises until the user's init.lua allows it; the error carries the rule line to paste, with `principal = "plugin:<name>"` so it covers that plugin alone. A plugin tool asks the user on every call outside yolo until the user allows `tool.<name>`. List every rule the plugin needs in its README. A missing rule is a state the plugin reports, not a crash: call the gated API under `pcall`, draw nothing while it fails, and put the rule line in a `zemi.health.add` row, so the plugin loads clean and `/health` says what to paste. Show the user that line and ask before adding it to their init.lua. Prefer a design that needs no rule: a tool the model calls asks the user itself, and a widget can read the render `ctx` instead of running a job. A plugin cannot edit the composer, change settings, the theme, the layout, the model or the mode, add allow rules, or read secret-shaped env vars: a key comes through `setup({ key = zemi.env.get("NAME") })` in the user's init.lua.

What a plugin cannot do at all: register providers, MCP servers or language servers (they run unconfined or hold keys, so the README gives the user the `zemi.provider.add` or `zemi.mcp.add` line to paste), rewrite the conversation or the provider request, draw its own transcript rows or a panel, change the tool set per turn (a profile does it once), or replace a builtin tool (`zemi.tool.override` adds `pre` and `post` around one). Each would break the prompt cache, the conversation or the UI rules below.

## Style

So a plugin looks like zemi: colors are theme groups (`Accent`, `Dim`, `Ok`, `Warn`, `Danger`, ...), never hex, so the user's theme applies. No state change adds or removes a row: status goes in a statusline widget that renders `{}` when there is nothing to say (with a high priority, since it comes and goes) or in a toast (`zemi.notify`), whose title says what happened like zemi's own: "Plugin installed" with the name in the body, "Could not reach the server" with the cause in the body. A tool's call row is zemi's own, labeled by a string `about` parameter, with `run:progress(text)` for the running line. Slow work goes in a `wait` inside a task or a job's `on_exit`, never in a render (`lua.md` has the API, `ui.md` the render rules).

## Developing one

- Reads under `local/` need no approval. Writes ask the user, with no always answer and no mode but yolo settling them, since plugin code runs in every project; the exception is the plugin you created: once the user approved the write that created its directory, auto mode settles your later edits there until zemi exits (its `.git` aside), while any other plugin asks every time. Say in one message what you will write.
- zemi reloads within a second of an edit once no turn runs; `zemi.config.reload()` from the Lua tool applies it as soon as the script returns. A load error is in `zemi.errors.list()` (`plugin <name>: ...`), and the plugin's `/plugins` row reads `failed` with the error in its detail.
- zemi writes the plugin's `.luarc.json` (editor types, with this machine's paths): never write it, and add it to `.gitignore` when publishing.
- The sandbox hides a plugin's `.git`, so git there needs `sandbox: false`, which asks each time outside yolo.
- Moving a feature out of the config dir is the one plugin task that edits init.lua: say which lines leave it and which stay, and wait for the user's yes. Then put its registrations in `local/<name>/init.lua` and its modules in `lua/<name>/`, leave `require("<name>").setup(...)` in the user's init.lua, and keep there what only user config may do (replacing a shipped widget, `hl.set`, layout, settings). Its allow rules keep working (a rule with no `principal` covers every caller); add the principal to narrow them. Its old `zemi.state` keys stay behind.

## Testing

`zemi plugins test <name|dir> [pattern]` runs each case of `tests/*.lua` in a fresh zemi holding only the plugin: no user config, no rules, and a clock only `test.advance` moves. The file's top level runs where init.lua would, before `app.ready`, so stub there what the plugin calls at boot. A job, HTTP request, model ask, agent send or `zemi.fs` write raises unless `test.stub` answers it, and a failing case prints the stub line to add. The drivers reach only what the plugin registered; `zemi.test` in the types lists them with the reply and request fields. Plugin code can tell it is under test, so passing tests are no review of what it does. `tests/branch.lua` for the plugin above:

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
```

## Publishing

`git init`, commit, push to a host the user picks, then tag `v0.1.0` and push the tag. An install checks out the newest `vX.Y.Z` tag and an update stays in its major version (one installed from a branch moves to the first tag ahead of it), so a breaking change bumps the major. Users install with `/plugins install <git url>` or `zemi plugins install <git url>`.
