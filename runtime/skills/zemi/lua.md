# zemi Lua

zemi runs Lua 5.1 (gopher-lua) in a sandbox. There is no `io` and no C modules, and `os` keeps only `time`, `date` and `clock`; filesystem, network and processes go through `zemi.fs`, `zemi.http` and `zemi.job`, each behind the permission gate. The `zemi.*` tables are frozen: you cannot add or replace members, and reading a member a namespace does not have raises with the list of the ones it does.

## Origins

Every chunk runs with an origin from where it lives: `user` (`~/.config/zemi`), `builtin` (shipped), `plugin` (`<data>/plugins/installed|local/<name>`) or `project` (`<repo>/.agents`). The origin is a label: it tags error and traceback chunk names, the overlay badge and a plugin's state keys, and it grants nothing. A call runs as the innermost file on its stack, so a plugin's `setup()` that init.lua calls runs as the plugin. Every origin calls the same API with the same rights. A registration with the same id replaces the one before it, except that one plugin can neither replace nor remove another's; a key that starts a longer sequence shadows it. The manifest is the one capability model: a plugin may call only what its `uses` lists.

A gated host-API call (`zemi.fs` writes, `zemi.http`, `zemi.job`, `zemi.classify.ask`, and a plugin's `zemi.agent.send` and `zemi.model.ask`) never opens a prompt; a read-only agent's writes deny, everything else runs. A plugin's call is refused before the gate unless its manifest (`uses`, `plugins.md`) declares it, naming the line to add.

`setmetatable` takes any handler, Lua or Go, and reads it when it runs; a handler runs as the code that triggers it, so a `zemi.*` member used as `__index` gets the table and the key as its arguments. `plugins.md` lists what a plugin's manifest must declare.

## Modules

`require("a.b")` loads `lua/a/b.lua` (or `lua/a/b/init.lua`) from the config dir, from `.agents/lua/`, or from the plugin named `a` (`a/.zemi/lua/a/`). Any code may require from any of those trees. `require("./x")` and `require("../x")` resolve next to the calling file and must stay in its tree, and plugin code in its own plugin. Each file loads once per session (both names share an instance) and every requirer gets that one table, so keep a secret in a local. `/reload` loads fresh. `require("zemi.util")` adds `split`, `trim`, `keys`, `get`, `validate` and `inspect`.

## Rules that keep zemi responsive

- Keymap and event handlers finish in about 10 ms (a vetoable one has 50 ms; a wedged handler is stopped at the 1 s hard ceiling); long work goes to zemi.job, zemi.http or zemi.timer callbacks, or to a `wait` in a task (below). Widget render has stricter rules, in `ui.md`.
- Misuse raises; a runtime failure of a synchronous call returns `nil, err` (`fs.read`, `fs.write`, `state.get`, `state.set`, `json.decode`, `session.new`, `session.resume`, `job.start`, and the `memory`, `learn` and `skills` calls). An empty path, one that does not resolve, or a bad pattern is misuse and raises (`fs.read`, `glob`, `grep`): wrap the call in `pcall` when the path is built at runtime.
- Removal reports a boolean and never raises for "not there" (`keymap.del`, `event.off`, `widget.del`, `config.off`), so a reloaded config unbinds idempotently.
- A wedged handler turns the runtime "hot"; `/reload` rebuilds it. Errors show in `/errors`.
- Once a `pcall` catches an error, closures made before it stop sharing locals with the function that made them (gopher-lua): a later `n = n + 1` inside one is lost outside it. Share state through a table (`local got = {}`), or return values out of the `pcall`.
- Module locals die on a reload. `zemi.state` is persisted and shared by every project and running zemi, so put the project in the key (`"branch:" .. zemi.project.root()`) and keep what only this run needs in a local. Each plugin gets its own space (stored under `plugin:<name>:`) and never sees another's; every other origin shares one space.
- Nothing stores a value that can never work: a color that is not `#rrggbb`, a palette key or an ANSI color name, a printable key with no `focus`, a key sequence containing ctrl+c (cancel and quit), a `zemi.config.watch` path outside the schema, and a tool spec with no `description` or with a wrapped `input` all raise.

## Waiting

A handle's `wait()` (`zemi.http`, `zemi.job` and `zemi.model.ask` handles) and `zemi.job.wait` suspend the task that calls them: the interpreter serves keys, renders and other callbacks until the result is there, and the task goes on after the call. A task is a Lua tool's `execute`, a Lua command, a keymap function, an overlay callback (`on_select`, `on_action`, `on_close`, `keys`, `reload`), an `on_done` or `on_exit` callback, a `tool.post` handler, and a lua tool script. Anywhere else `wait` raises: in a widget render, any other event handler, a timer callback, `on_stdout`, a completion source or a health check use the callback form (`on_done`, `on_exit`) instead. It also raises under `pcall` and inside a library callback such as a `table.sort` comparator or a `gsub` function, and in a coroutine you made: call it outside them and check its `nil, err`. `return job:wait()` is fine. A keymap or command that waits lets the next key run, so two presses can interleave, and a reload ends every waiting task at once.

## The lua tool

Its description covers what comes back, the 10 s ceiling between waits and which calls need the user's approval. Keep a script you reuse in the scratchpad and pass `path` instead of `script`. A script runs as `user` origin, so an approved one can do what init.lua can. A call reached through a name built from strings, `_G` or `load` needs approval too, and one the scan misses raises; a read-only agent's script is denied outright. After the run, callbacks the script registered keep only the read-only calls and its globals are gone, so lasting behavior belongs in init.lua. SKILL.md's Check a change has the loop for checking config you wrote.

A script also drives this run's tools: `zemi.tool.call(name, args)` starts one through the same gate, `tool.pre` and `tool.post` a direct call goes through, and returns a handle whose `wait()` gives its output, or `nil, err` like every other zemi call (a tool that failed reports its output as the error). Independent calls run beside each other (8 at once, the rest waiting for a slot), so start them, then wait on each. `zemi.tool.list()` names every tool a script may call: zemi's own, the Lua ones registered, and each ready MCP server's, whose schemas the model's own list may be holding back, which is how a script reaches a server's tool with no search step first; `zemi.tool.describe(name)` gives one's parameters. Only the script's own print lines and return values reach the model, a return as JSON, so filter big output there instead of spending context on it; a call the script drove gets the tool's output whole, where a direct call is cut at 10k (shell) or 30k (lua). A script that only drives tools needs no approval of its own, since every call is judged as if the model had made it; only the lua tool's running script reaches `call`, so a callback the script registered cannot drive tools later. A script that calls zemi itself still asks once, and a nested call's ask is labeled `lua script` as who asked.

One script for a whole step is called codemode: when calls chain, a shape of call repeats, or the output is far larger than the model needs, the sequence goes in one script that returns the digest the next decision needs, so no middle output enters the conversation.

## The API

Zemi can read itself from a script: the current model is `zemi.agent.current().model`, the session `zemi.session.id()` and `.title()`, the screen as text `zemi.ui.screenshot()`, errors `zemi.errors.list()`, health `zemi.health.list()` and the layout rects `zemi.ui.layout.rects()`. `zemi.model.pick()` opens the model picker (choosing one runs `/model`); it does not return the current model.

Every member, with its parameters, is one shell grep of the types file (the system prompt names it): `grep -n '^function zemi\.'` lists all of them, `grep -n -B20 'function zemi.lsp.'` one namespace with the `---` lines above each hit, which are its rules: who may call it, what it returns, when it raises. `grep -n -A20 '@class zemi.<Name>'` gives the fields of the class a parameter names. `zemi.notify` and `zemi.log` are also callable, and `zemi.api.version` is the API version string. `memory.md` has `zemi.memory`, `zemi.learn` and `zemi.skills` with examples.

The verbs are regular: `add` registers, `set` writes a value, `get` and `list` read back, and `del` removes: `grep '^function zemi\..*\.del('` over the types file lists what can be removed. A tool or health row goes with the reload that no longer adds it. `event.on` and `config.watch` hand back the id their `off` and `unwatch` take.

The types file is LuaLS annotations, and zemi writes a `.luarc.json` (and EmmyLua's `.emmyrc.json`,
which drops a config it cannot parse whole, zemi's library included) pointing at it in the user
config dir and in each local plugin, so an editor checks config as it is written. Each event's payload is a `zemi.ev.*` class that `zemi.event.on` picks by name, and the globals the sandbox strips (`setfenv`, `rawset`, `dofile`, ...) are marked deprecated. Do not edit it: the next start overwrites it. `types/zemi/util.lua` beside it is the `zemi.util` source.

## Examples

```lua
-- Codemode: a step's calls in one script; only what it prints or returns
-- reaches the model, so the raw output never costs context.
for _, t in ipairs(zemi.tool.list()) do print(t.name) end -- what this run can call

local hits, err = zemi.tool.call("grep", { pattern = "WithoutCancel", path = "." }):wait()
if not hits then return "grep failed: " .. err end
local files = {}
for line in hits:gmatch("[^\n]+") do files[line:match("^([^:]+)")] = true end
local names = {}
for f in pairs(files) do names[#names + 1] = f end
table.sort(names)
return { callers = names, count = #names }
```

```lua
-- Command with argument completion; handler gets { args = "raw", list = { ... } }
zemi.command.add("greet", function(a)
	zemi.notify({ title = "hello", body = a.list[1] or "world" })
end, {
	desc = "say hi",
	nargs = "?",
	complete = function(lead, line) return { "alice", "bob" } end,
})

-- Picker
zemi.ui.select({ "patch", "minor" }, { prompt = "Bump" }, function(choice)
	if choice then zemi.agent.send("bump the " .. choice .. " version") end
end)

-- A list whose items open a view: enter shows `fields` as a table and `detail` under them.
-- What acts on a row is its own `keys`, flat: the character acts on the selected row from
-- the list, and esc, y and the footer's hints are the view's. A key fills the composer,
-- runs a command (`run`) or, with an `id`, calls on_action; one with
-- `verb = "open"` opens a screen over the list, which stays. j, k, /, y and Y raise as keys.
-- `danger = true` (an item or a key that cannot be undone) draws it red, and its first
-- press only arms it: the same key again runs it. A list's own `keys` act on the selection
-- and win over a row's.
-- The item a callback gets is a copy: match on an id field, never on the table. A callback
-- closes the list unless it has `reload`; to stay open without one, open it again from the callback.
zemi.ui.overlay.open({
	id = "deploys",
	title = "Deploys",
	empty = "Nothing deployed yet.",
	items = {
		{
			text = "api",
			desc = "failed, 2m ago",
			section = "Failed",
			fields = { { "status", "failed" }, { "exit", "1" } },
			detail = "The migration timed out.",
			keys = { { key = "r", desc = "retry", run = "deploy api" }, { key = "x", desc = "drop", id = "drop", danger = true } },
		},
		-- preview: shown whole under the list while the row is selected
		{ text = "web", desc = "live, 1h ago", section = "Live", fill = "/deploy web ", preview = "Deployed from main." },
	},
	on_action = function(item, key) zemi.notify({ body = key.id .. " " .. item.text }) end,
	keys = { l = { desc = "logs", fn = function(item) zemi.agent.send("show the logs of " .. item.text) end } },
})

-- Background work
zemi.job.start({
	cmd = "go",
	args = { "test", "./..." },
	on_stdout = function(lines) zemi.log.info(table.concat(lines, "\n")) end,
	on_exit = function(res) zemi.notify({ title = "go test", body = res.code == 0 and "pass" or "fail" }) end,
})
local t = zemi.timer.every(60000, function() zemi.log.info("tick") end) -- t:stop()

-- A tool the model can call
zemi.tool.add({
	name = "ticket",
	description = "look up a ticket title by id",
	input = { id = { type = "string" } }, -- the parameters, flat
	required = { "id" },
	execute = function(args)
		if not args.id:match("^%u+-%d+$") then return nil, "not a ticket id: " .. args.id end
		return "ticket " .. args.id
	end,
})

-- Guidance ships with the tool; state lives in a data dir
zemi.tool.add({
	name = "ticket_seen",
	description = "mark a ticket as seen",
	prompt = "Call ticket_seen after you read a ticket, once per ticket.",
	input = { id = { type = "string" }, about = { type = "string" } },
	required = { "id" },
	execute = function(args)
		local ok, err = zemi.fs.write(zemi.fs.data_dir("tickets") .. "/seen.json", zemi.json.encode({ args.id }))
		if not ok then return nil, err end
		return "seen " .. args.id
	end,
})

-- Run the project's tests after the model changes Go files: only in a trusted project,
-- from the repository root.
zemi.event.on("tool.post", function(ev)
	if not zemi.project.trusted() or not table.concat(ev.changed, " "):find("%.go") then return end
	zemi.job.start({
		cmd = "go",
		args = { "test", "./..." },
		cwd = zemi.project.root(),
		on_exit = function(res) zemi.log.info("go test: " .. res.stdout .. res.stderr) end,
	})
end)

-- Spend per turn
zemi.event.on("agent.finish", function(ev) zemi.log.info(ev.model .. " $" .. ev.usage.cost) end)

-- Subagents and workflows run off the interpreter; callbacks fire when done, and a reload cancels them
zemi.agent.run({ agent = "explore", prompt = "Where is the retry logic?" }, function(text, err)
	zemi.notify({ title = "explore", body = text or err })
end)
zemi.command.add("review", function()
	zemi.workflow.run({ name = "review" }, function(result, err) zemi.agent.send(result or err) end)
end, { desc = "run workflows/review.lua and hand the result to the agent" })
```

Agent files and workflow scripts are not config Lua: `agents.md` and `workflows.md` have them.

## Events

`app.ready`, `session.new`, `session.open`, `session.close`, `agent.start`, `agent.step` (after each model call, a subagent's too: session, model, `step`, `ms`, `first_token_ms`, `tools`, `usage` `{ ["in"], out, cache_read, cache_write, cost }`), `agent.finish` (session, model, `usage`: this turn's `{ ["in"], out, total, cost }`), `agent.error`, `agent.subagent.start`, `agent.subagent.finish`, `agent.assemble` (return a string or `{ title, text }` to add a system prompt section; it runs when the prompt is built (a new session, `/resume`, a compaction, a model, trust or tool change), not per send, so a section edit applies after `/new` or `/compact`), `composer.change`, `tool.denied` (tool, subject, arg, by, reason), `app.quit` (vetoable), `agent.text_delta`, `agent.reasoning_delta`, `tool.pre` (vetoable: deny, halt, rewrite), `tool.post` (tool, input, ok, `output` in full, `ms`, `changed`: the absolute paths a file tool or a shell command changed; it runs as a task, and returning `{ output = "..." }` replaces what the model gets, which the next handler sees), `permission.request`, `permission.resolved`, `composer.submit` (vetoable), `ui.resize`, `memory.written`, `config.reloaded` (after /reload or a config file change), `reflector.start`, `reflector.done`, `job.exit` (a zemi.job start; the model's job_* tools do not fire it - watch tool.post), `lsp.request` (server, path, kind `diagnostics`, after a write), and your own `user.*` names.

Overlays have no events: a plugin watches its own overlay through its `on_close` callback. A vetoable handler vetoes by returning `{ decision = "deny", reason = "..." }` (`tool.pre` also takes `"halt"`, and `"rewrite"` with `input`), and on `permission.request` `{ decision = "ask" }` runs zemi's ask path; nil lets the event through. Any other return denies `tool.pre` and `permission.request` and is ignored for `composer.submit` and `app.quit`. A vetoable handler has 50 ms, so it cannot wait for an answer; registering a `permission.request` handler needs the plugin's `perm` word. A denied `composer.submit` keeps the draft, so to confirm a send: deny, ask, and let the same text through once. `composer.submit` also takes `{ decision = "rewrite", text = "..." }`, which sends `text` in the place of what was typed - the way to expand a shorthand before the turn starts.

```lua
local pass
zemi.event.on("composer.submit", function(ev)
	if pass == ev.text or not ev.text:find("prod") then
		pass = nil
		return
	end
	zemi.ui.select({ "send", "cancel" }, { prompt = "This mentions prod" }, function(choice)
		if choice ~= "send" then return end
		pass = ev.text
		zemi.composer.submit()
	end)
	return { decision = "deny", reason = "confirm first" }
end)
```

Every event with a handler is recorded, one with none only counted: `zemi.event.list({ name, limit })` returns the last 500 newest first, each the table a handler would have seen plus `at` and `ts` (`tool.post`'s entry carries its output cut to 4096 runes); `zemi.event.stats()` counts emissions per name since start (the streaming deltas are counted but not logged). Diff two `stats()` snapshots for a session or a turn, for example `stats()["lsp.request"]` in a widget. `zemi.notify.list()` is the last 100 notifications newest first (`at`, `ts`, `level`, `title`, `body`).
