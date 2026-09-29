# zemi Lua

zemi runs Lua 5.1 (gopher-lua) in a sandbox. There is no `io` and no C modules, and `os` keeps only `time`, `date` and `clock`; filesystem, network and processes go through `zemi.fs`, `zemi.http` and `zemi.job`, each behind the permission engine. The `zemi.*` tables are frozen: you cannot add or replace members, and reading a member a namespace does not have raises with the list of the ones it does.

## Origins

Every chunk runs with an origin from where it lives: `user` (`~/.config/zemi`) > `builtin` (shipped) > `plugin` (`<data>/plugins/installed|local/<name>`) > `project` (`<repo>/.agents`). A call runs as the least trusted code on its stack, so a plugin's `setup()` that init.lua calls runs as the plugin, not as user config. A registration with the same id from a lower origin is refused; the same origin replaces its own, except that one plugin can neither replace nor remove another's, and a binding may not be a prefix or an extension of a more trusted key. Only `user` may register providers, add allow rules or decide learning proposals. Below `builtin`, code may not call `zemi.model.set` or `zemi.config.reload`, or run a more trusted origin's command with `zemi.command.run`, and a project handler cannot deny `app.quit`.

Lua calls to gated APIs (`zemi.fs` writes, `zemi.http`, `zemi.job`, and a plugin's `zemi.agent.send` and `zemi.model.ask`) never open a prompt: an "ask" decision raises `permission ask required` with the `zemi.permission.rule.add` line that allows it, which only the user's init.lua may add (show it to the user and ask before adding it); a plugin's line carries `principal = "plugin:<name>"`, so the rule allows that plugin alone. No permission mode allows such a call, yolo included.

In plugin and project code, `setmetatable` takes only that code's own Lua functions as handlers (a `zemi.*` member, a builtin helper or another plugin's function raises) and reads them when it runs, so set every handler before the call. `plugins.md` lists what a plugin cannot do.

## Modules

`require("a.b")` loads `lua/a/b.lua` (or `lua/a/b/init.lua`) from the config dir, else from the plugin named `a` (`a/lua/a/`), which its own code searches first; project code also searches `.agents/lua/` first. A plugin answers only names under its own, so it cannot shadow another's modules. `require("./x")` and `require("../x")` resolve next to the calling file and must stay in the same origin's tree, and plugin code in its own plugin. Each file loads once (both names share an instance), except that code below the file's origin gets its own instance, one per plugin. Builtin modules for plugin and project code, and one plugin's modules for another plugin, come read-only, so keep a secret in a local, not in the module table. `/reload` loads fresh. `require("zemi.util")` adds `split`, `trim`, `keys`, `get`, `validate` and `inspect`.

## Rules that keep zemi responsive

- Keymap and event handlers finish in about 10 ms; long work goes to zemi.job, zemi.http or zemi.timer callbacks, or to a `wait` in a task (below). Widget render has stricter rules, in `ui.md`.
- Misuse raises; a runtime failure of a synchronous call returns `nil, err` (`fs.read`, `fs.write`, `state.get`, `state.set`, `json.decode`, `session.new`, `session.resume`, `job.start`, `workflow.run`, and the `memory`, `learn` and `skills` calls). A jail escape or a bad pattern is misuse, so `fs.read` outside the jail raises: wrap it in `pcall` when the path may be outside.
- Removal reports a boolean and never raises for "not there" (`keymap.del`, `event.off`, `widget.del`, `config.unwatch`), so a reloaded config unbinds idempotently.
- A wedged handler turns the runtime "hot"; `/reload` rebuilds it. Errors show in `/errors`.
- Once a `pcall` catches an error, closures made before it stop sharing locals with the function that made them (gopher-lua): a later `n = n + 1` inside one is lost outside it. Share state through a table (`local got = {}`), or return values out of the `pcall`.
- Module locals die on a reload. `zemi.state` is persisted and shared by every project and running zemi, so put the project in the key (`"branch:" .. zemi.project.root()`) and keep what only this run needs in a local. Each plugin and project code get their own space (stored under `plugin:<name>:` or `project:`) and never see user keys; user config sees them all.
- Nothing stores a value that can never work: a color that is not `#rrggbb`, a palette key or an ANSI color name, a rule subject outside `tool. fs. net. lua. mcp. memory.`, a printable key with no `focus`, a key sequence containing ctrl+c (cancel and quit), a `zemi.config.watch` path outside the schema, and a tool spec with no `description` or with a wrapped `input` all raise.

## Waiting

A handle's `wait()` (`zemi.http`, `zemi.job` and `zemi.model.ask` handles) and `zemi.job.wait` suspend the task that calls them: the interpreter serves keys, renders and other callbacks until the result is there, and the task goes on after the call. A task is a Lua tool's `execute`, a Lua command, a keymap function, an overlay callback (`on_select`, `on_action`, `on_close`, `keys`, `reload`), an `on_done` or `on_exit` callback, a `tool.post` handler, a permission mode's `decide`, and a lua tool script. Anywhere else `wait` raises: in a widget render, any other event handler, a timer callback, `on_stdout`, a completion source or a health check use the callback form (`on_done`, `on_exit`) instead. It also raises under `pcall` and inside a library callback such as a `table.sort` comparator or a `gsub` function, and in a coroutine you made: call it outside them and check its `nil, err`. `return job:wait()` is fine. A keymap or command that waits lets the next key run, so two presses can interleave, and a reload ends every waiting task at once.

## The lua tool

Its description covers what comes back, the 10 s ceiling between waits and which calls need the user's approval. A script runs as `user` origin, so an approved one can do what init.lua can, secret-shaped `zemi.env` names included. A call reached through a name built from strings, `_G` or `load` needs approval too, and one the scan misses raises; in an untrusted project or a read-only agent every script needs approval. After the run, callbacks the script registered keep only the read-only calls and its globals are gone, so lasting behavior belongs in init.lua. SKILL.md's Check a change has the loop for checking config you wrote.

## The API

Every member, with its parameters, is one grep of the types file (`<data>/types/zemi.lua`, named in SKILL.md): `^function zemi\.` lists all of them, `function zemi.lsp.` one namespace. The `---` lines above a hit are its rules: who may call it, what it returns, when it raises; the `---@class` a parameter names holds its fields. `zemi.notify` and `zemi.log` are also callable, and `zemi.api.version` is the API version string. `memory.md` has `zemi.memory`, `zemi.learn` and `zemi.skills` with examples.

The verbs are regular: `add` registers, `set` writes a value, `get` and `list` read back, and `del` removes: `grep '^function zemi\..*\.del('` over the types file lists what can be removed. A tool or health row goes with the reload that no longer adds it. `event.on` and `config.watch` hand back the id their `off` and `unwatch` take.

The types file is LuaLS annotations, and zemi writes a `.luarc.json` pointing at it in the user config dir and in each local plugin, so an editor checks config as it is written. Each event's payload is a `zemi.ev.*` class that `zemi.event.on` picks by name, and the globals the sandbox strips (`setfenv`, `rawset`, `dofile`, ...) are marked deprecated. Do not edit it: the next start overwrites it. `types/zemi/util.lua` beside it is the `zemi.util` source.

## Examples

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

-- A list whose items open a detail view: enter shows `fields` as a table, `detail`
-- under them and `actions` under that; an action fills the composer, runs a command (`run` only from user config) or,
-- with an `id`, calls on_action; one with none goes back. esc goes back, y copies the text.
-- An action's `hotkey` also acts on the selected row from the list; j, k, /, y and Y
-- raise as hotkeys. `danger = true` (an item or an action that cannot be undone) draws
-- it red, and its first press only arms it: the same key or enter runs it.
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
			actions = { { text = "Retry", run = "deploy api" }, { text = "Drop", id = "drop", danger = true } },
		},
		-- preview: shown whole under the list while the row is selected
		{ text = "web", desc = "live, 1h ago", section = "Live", fill = "/deploy web ", preview = "Deployed from main." },
	},
	on_action = function(item, action) zemi.notify({ body = action.id .. " " .. item.text }) end,
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

-- Guidance ships with the tool; state lives in a data dir (the rule is the user's to add)
zemi.permission.rule.add({
	subject = "lua.fs.write",
	pattern = zemi.fs.data_dir("tickets") .. "/*",
	decision = zemi.permission.decision.ALLOW,
	match = zemi.fs.data_dir("tickets") .. "/seen.json",
})
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
-- confined, from the repository root.
zemi.event.on("tool.post", function(ev)
	if not zemi.project.trusted() or not table.concat(ev.changed, " "):find("%.go") then return end
	zemi.job.start({
		cmd = "go",
		args = { "test", "./..." },
		cwd = zemi.project.root(),
		sandbox = true,
		on_exit = function(res) zemi.log.info("go test: " .. res.stdout .. res.stderr) end,
	})
end)

-- Spend per turn
zemi.event.on("agent.finish", function(ev) zemi.log.info(ev.model .. " $" .. ev.usage.cost) end)

-- Subagents and workflows run off the interpreter; callbacks fire when done
zemi.agent.run({ agent = "explore", prompt = "Where is the retry logic?" }, function(text, err)
	zemi.notify({ title = "explore", body = text or err })
end)
zemi.command.add("review", function()
	zemi.workflow.run({ name = "review" }, function(result, err) zemi.agent.send(result or err) end)
end, { desc = "run workflows/review.lua and hand the result to the agent" })
```

Agent files and workflow scripts are not config Lua: `agents.md` and `workflows.md` have them.

## Events

`app.ready`, `session.new`, `session.open`, `session.close`, `agent.start`, `agent.step` (after each model call, a subagent's too: session, model, `step`, `ms`, `first_token_ms`, `tools`, `usage` `{ ["in"], out, cache_read, cache_write, cost }`), `agent.finish` (session, model, `usage`: this turn's `{ ["in"], out, total, cost }`), `agent.error`, `agent.subagent.start`, `agent.subagent.finish`, `agent.assemble` (return a string or `{ title, text }` to add a system prompt section; it runs when the prompt is built (a new session, `/resume`, a compaction, a model, profile, trust or tool change), not per send, so a section edit applies after `/new` or `/compact`; user, builtin and plugin handlers only), `composer.change`, `tool.denied` (tool, subject, arg, by, reason), `app.quit` (vetoable), `agent.text_delta`, `agent.reasoning_delta`, `tool.pre` (vetoable: deny, halt, rewrite), `tool.post` (tool, input, ok, `output` in full, `ms`, `changed`: the absolute paths a file tool or a bash command changed; it runs as a task, and returning `{ output = "..." }` replaces what the model gets, which the next handler sees; a project handler's return is ignored), `permission.request`, `permission.resolved`, `composer.submit` (vetoable), `ui.resize`, `ui.theme`, `memory.written`, `config.reloaded` (after /reload or a config file change), `reflector.start`, `reflector.done`, `job.exit`, `lsp.request` (server, path, kind `diagnostics` after a write or `tool`), and your own `user.*` names.

Overlays have no events: a plugin watches its own overlay through its `on_close` callback. A vetoable handler vetoes by returning `{ decision = "deny", reason = "..." }` (`tool.pre` also takes `"halt"`, and `"rewrite"` with `input`), and nil lets the event through. Any other return denies `tool.pre` and `permission.request` and is ignored for `composer.submit` and `app.quit`. A vetoable handler has 50 ms, so it cannot wait for an answer. A denied `composer.submit` keeps the draft, so to confirm a send: deny, ask, and let the same text through once.

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
		zemi.composer.submit() -- user config only; from a plugin, tell the user to press enter
	end)
	return { decision = "deny", reason = "confirm first" }
end)
```

Every event is recorded whether or not anything listens: `zemi.event.log({ name, limit })` returns the last 500 newest first, each the table a handler would have seen plus `at` and `ts`; `zemi.event.stats()` counts emissions per name since start (the streaming deltas are counted but not logged). Diff two `stats()` snapshots for a session or a turn, for example `stats()["lsp.request"]` in a widget. `zemi.notify.list()` is the last 100 notifications newest first (`at`, `ts`, `level`, `title`, `body`).
