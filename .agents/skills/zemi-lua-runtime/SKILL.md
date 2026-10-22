---
name: zemi-lua-runtime
description: "Load before changing internal/lua or its Lua sandbox, a zemi.* namespace, runtime/types/zemi.lua or runtime/lua, the skill loader, or anything crossing a Lua generation: the actor and tasks, the watchdog, require and the origin model, the type annotations."
---

# The Lua runtime

## The concurrency rules (DESIGN §4) - violate these and things deadlock or corrupt

1. **Only the Lua actor goroutine touches the `lua.LState`.** Everything else goes through the
   queue in `internal/lua` with priority classes `ui > agent > io`. `ctx` cancellation preempts
   running Lua at the next opcode (catchable once, then re-raises). A task (`lua.Task`,
   internal/lua/task.go) runs a callback as a coroutine: a handle's `wait()` yields it, a host
   goroutine waits, and the next slice is an ordinary request, so a resume never touches the
   `LState` off the actor. The task's thread stays in `Generation.tasks` until it ends, and the
   sandbox's `coroutine.resume` raises for it: only the driver resumes a task.
2. **Watchdog / hot mode.** A wedged request trips the watchdog; after a grace period the
   runtime goes "hot" - every Lua request fails fast until force-reload builds a fresh `LState`
   (generation). All registrations (keymaps, widgets, events, timers, jobs, Lua tools) are
   generation-tagged (`hostapi.Gen`) and purged before replay,
   so never keep a pointer into an old generation. `ForceReload` refuses an init.lua that does
   not parse (the old generation stays), and `tool.pre` and `permission.request` vetoes wait out a reload, so a turn is never checked
   against half a config.
3. **UI single-writer.** All `ui.App` state mutates on the bubbletea Update goroutine; work
   from other goroutines is funneled through a hook channel (internal/ui/hooks.go). hostapi
   never imports internal/ui - it defines `UIHooks` and the App implements it; keep that
   direction. Corollary: never call `program.Send`/`program.Quit` from inside `Update` (or
   anything it calls) - bubbletea v2's `p.msgs` channel is unbuffered, so the loop would wait
   on itself (the `/quit` hang). Wrap such calls in a goroutine, like `App.Quit` does. Blocking
   hooks (`AskPermission`, `AskTrust`, `AskQuestion`, `AskPlan`) must only run on non-UI
   goroutines.
4. **UI-critical Lua handlers (keymaps, widget render, event handlers) must be fast** (~10 ms).
   Long work belongs in `zemi.job` / `zemi.http` / `zemi.timer`. `wait()` suspends a task (a
   Lua tool's `execute`, a Lua command, a keymap function, an overlay callback, an
   `on_done`/`on_exit`, a `tool.post` handler, `Runtime.Eval`) and raises
   anywhere else: a render, any other event handler, a timer, under `pcall` or a library
   callback (`lua.InGoCall`). A handle's `wait` is a `lua.Suspending` wrapper, since gopher-lua
   drops the resume values of a Go function reached by a tail call. A keymap or command returns
   once it suspends (`StartTask`), so the next key runs; a purged generation ends every waiting
   task at once.
5. **Vetoable events** (`permission.request`, `app.quit`, `composer.submit`, `tool.pre`) have a
   50 ms Lua-handler deadline and then take a logged default (fail-closed for permission asks).
   While the runtime is hot those handlers cannot run, so both deny until `/reload`.
6. **No bare goroutines in hostapi.** Start them with `h.GoSafe(where, fn)`: a panic is logged
   with its stack, shown in `/errors`, and zemi keeps running. Turn goroutines also defer
   `h.endTurnAfterPanic()` so the agent is not left "active"; `sendClaimed`, which every
   caller reaches holding the claim, releases it the same way and returns the panic as an
   error, so the caller keeps the text. A panic in a plain `go func()`
   kills the process with the terminal still in raw mode.

## require and the origin model (internal/lua/runtime.go, origin.go, registries)

Origin is a label, never a privilege: it tags chunk names for errors and tracebacks, the overlay
badge, a plugin's state keys and its permission principal, and every origin calls the same host
API with the same rights. Modules are cached per generation by resolved file alone: one instance
per file whatever requires it, so every requirer shares that table. Dotted names search each
candidate root's code dir (`<root>/lua`, `<plugin>/.zemi/lua`) over the runtimepath:
the project's `.agents`, then the user's `~/.config/zemi`, then the one plugin the plugins step
loaded (`lua.Generation.Plugins`, resolved, fixed per generation) named like the first segment,
which that plugin's own code searches first. Any requirer may use any tree. `./x` and `../x`
resolve against the calling function's chunk name, which is why file chunks are compiled with
their absolute path, and must stay in that file's tree and, for plugin code, its own plugin; a
plugin file resolves only inside an enabled plugin, so a symlink or `..` cannot run a disabled
one. `load`/`loadstring` tag their chunks with the running origin and never keep an absolute
name, so neither can be forged.

Every chunk carries an origin from its resolved path class: `user` (~/.config/zemi, plus each
subdirectory of it in `Dirs.UserTrees`, so a symlinked
`skills` or `lua` still resolves as user), `builtin` (embedded runtime/), `plugin`
(`<data>/plugins`) or `project` (<repo>/.agents). The code a call runs as
(`Runtime.CurrentOrigin`, `CurrentPath`) is the innermost Lua file on the stack, resumers
included: a plugin's `setup()` that init.lua calls runs as the plugin, and a callback that file
calls runs as itself; a chunk with no file of its own (a task's scratch source, a `loadstring`
chunk) takes the frame the host entry pushed. Chunks compile with no tail calls, which would
drop a frame, and the `Suspending` wrapper is tagged `host` so it reads through like the Go
function it wraps. A metamethod leaves no frame of the code that set it, so a handler behind
`obj.setup`, `obj()` or `==` runs as the code that triggers it; `setmetatable` (tables only)
installs it as given, and a Go function handed out as a value runs as its caller, with the module
table as its first argument when called as a method. Plugins (hostapi/plugins.go, `PluginsStep`,
DESIGN §5): `plugin.Scan` lists `local/` then `installed/` (an installed
plugin is a symlink into its marketplace's material; `pickPlugins` follows a
link only inside the plugins tree) minus `disabled`, and
the same pick, fixed for the generation (`Host.pluginDirs`, sorted by name in `pickPlugins`),
gives the plugins' skills, agents,
markdown commands and workflows; a plugin's own Lua sits under `.zemi` (`plugin.CodeDir`:
init.lua, lua/, tests/, workflows/, plugin.json), and one still keeping it at its root is refused
(`plugin.OldLayout`) rather than loading nothing. A gone or reinstalled plugin's `zemi.state`
keys are dropped (`sweepPluginState`). `Host.PluginOf` names a plugin by its directory: its state
keys are `plugin:<name>:<key>`, its state principal and its overlay badge `plugin:<name>`
(`Host.principal` raises for plugin code with no file of its plugin on the stack). Registries
enforce the one collision rule left (`mayReplace`, which every add and remove runs): **the last
registration wins**, except that one plugin can neither replace nor remove another's command,
keymap, widget, theme, event handler, tool, completion source, health row or config watch. No
origin can emit host-owned events. A plugin's manifest (`uses`) is the capability model: a gated
call a plugin makes without its declared word is refused, and registering a
`permission.request` handler or calling `zemi.perm.ask` needs `perm`. When adding a registry,
follow `internal/hostapi/registry.go`.
## Skills (internal/skill, internal/hostapi/skills.go)

Skills are file-based units, each a `SKILL.md` with YAML frontmatter: user skills under
`<config>/skills/`, then `~/.agents/skills/` (`Dirs.AgentsHome`, in `UserTrees`; body only,
since installers write there; the config dir wins a name clash), builtin (zemi's
self-knowledge, one skill per directory in `runtime/skills`: `zemi`, whose SKILL.md routes to
reference files the model reads on demand, `sessions`, finding, reading and resuming a past
conversation, and `codescan`, one script over many files; each reference file fits the skill tool's cap (`TestBuiltinSkillsFitTheSkillTool`)
and is named in its own SKILL.md (`TestBuiltinSkillReferences`), and every builtin body ends with
this binary's path (`skillNote`); embedded and rewritten to `<data>/builtin-skills/` on
every sync) and plugin (`skill.Plugin`, each enabled plugin's `skills/`, body only), all in the
table and enabled by default. A trusted project adds its own (`.agents/skills`): body only,
enabled, never in the table or `skillsReg`, scanned by `SetProjectTrusted`, which every reload
runs, so a skill written mid-session loads with the reload the config watcher starts. Recipes (`skill.Recipe`) are the project's
private skills under `<state>/projects/<hash>/skills` (`recipesDir`): trusted only, body only, enabled, and they lose every name clash. The
pinned prompt (`ag.pin`, below) lists the ones found when it was built; `skillIndex` and
`enabledSkills` discover the dir live, so a recipe written mid-session loads without moving the
prompt, and `runSkill` marks the file read so an `edit` can follow. The agent writes recipes with
the plain file tools (the environment's `Recipes:` line names the dir, the system prompt's
`recipesUse` says how); a write there is an ordinary write.
`recipeAfterWrite` (the `AfterWrite` hook, run before the writers' `markRead` because it may
rewrite the file) scrubs every file written there in place (`learn.ScrubShapes`: no entropy
pass, which cuts the long paths and `NAME=url` a procedure is made of), names the redacted
lines and tells the model what will not load. `/skills` and its commands read the dir live.
`/skills delete <name>` (`deleteRecipe`, the Delete action only recipe rows have) removes one
and drops the pinned prompt. Invariants:

- **Any origin may enable/disable** (`zemi.skills.enable`). A disabled skill leaves the index
  but `/skills use` still sends it (the body rides behind `agent.AttachMarker`, like a markdown
  command's).
- **Token efficiency**: the prompt carries only the index (name + description, cut to 512 runes
  with a warning when parsed, and to 240 plus `...` in the line a request carries); bodies load
  on demand via the `skill` tool.
- **Hook placement**: a `<config>/skills` skill's `zemi-skill.lua` runs once, in the boot
  step after user init.lua, as user origin; enabling later does not run it.
- **Invocation**: `user-invocable` registers `/<name>` last in `MarkdownCommandsStep`, never
  over a builtin or same-origin command and over any other by `mayReplace`
  (`addSkillCommand`; a toggle reloads config to rebuild it); the index and the skill tool take
  every enabled skill, so Claude's `disable-model-invocation` hides nothing; recipes ignore the
  command fields.

## Type annotations (internal/hostapi/types.go, runtime/types/zemi.lua)

`runtime/types/zemi.lua` is the LuaLS (`---@meta`) description of the whole `zemi.*` API, never
loaded by the interpreter. `syncTypes` rewrites it plus a copy of `runtime/lua/` to
`<data>/types/` on every start, and the `.luarc.json` and `.emmyrc.json` in `<config>` and in each
local plugin whenever their content moved (an editor reloads on each write; the config watcher
skips them): the second is EmmyLua's, which reads a lua-language-server severity as an unknown
value and drops the whole config, zemi's library included. `/workflows` writes both in
`<state>/scratch` before it opens a run's script. `TestTypes*` keep it
honest: every API member annotated, nothing annotated that zemi dropped, and the
`zemi.EventName`, `zemi.HostAction` and `zemi.WidgetCtx` aliases pinned to their Go sources, like
each constant table (`enumTable`, keyed by the value upper-cased) and the alias that types the
fields taking one (`TestTypesEnums`), and every other closed set Lua sees (a Go `type X string`
with a slice of its values) as a multi-line `---|` alias (`TestTypesLayoutKinds` for the layout
kinds). A member whose call shape its signature
and description do not tell (a callback and its return, a handle to wait on, a surprising spec
field, a string grammar, a return shape unlike its siblings, a call order) carries a fenced
```` ```lua ```` example of one to four real lines before its `---@param`s, and one whose
signature tells it carries none; `TestTypesExamples` parses each
and checks every `zemi.*` and `test.*` name it calls is registered. It
also carries `---@deprecated` stubs for the globals the sandbox strips, since the `.luarc.json`
files turn off LuaLS's own libraries and it flags a deprecated global only when every
definition is deprecated. Known ceiling: with no CI, `make luals` runs only where
lua-language-server is installed.

## Gotchas

- **`zemi.*` namespaces are frozen and integrity-checked.** Tables are metatable-locked,
  `rawset`/`rawequal` are stripped, and Go re-verifies namespace contents per request class. A
  runtime-mutable member must go through the Go side, and each Go function exposed to Lua runs
  under `recover` (a panic becomes a catchable Lua error, logged as a zemi bug).
- **Error convention (DESIGN §6.0):** misuse (bad arguments, a call a render may not make)
  raises; runtime failure of a sync op returns `nil, err`; async ops deliver through their
  handle.
- **gopher-lua multiple assignment**: `a[#a + 1], b = b, x` stores `x` in `a`. Split any
  assignment whose right side names a left-side target (this once garbled wrapped text). In a
  generic-for header, a method call on a parenthesized `c and x or y` (`(s:sub(-1) == "\n" and
  s or s .. "\n"):gmatch(...)`) gets the wrong receiver: compute it into a local first.
- **Handles vs snapshots**: host resources (job, http, timer, a model, classify, a run or
  workflow, a perm ask, a tool call) are handle tables with colon methods, everything else a
  snapshot copy; marshaling caps string size and
  depth and refuses cycles.
