---
name: zemi-host-flow
description: "Load before changing main.go, internal/cli, internal/config, internal/store, internal/classify, or hostapi agent, session, compact, memory, classify or prompt code: boot order and trust, the CLI and headless runs, config reload and kept picks, turns, sessions, compaction and the pinned prompt."
---

# Host flow: CLI, boot, config, turns

## CLI (internal/cli/cli.go, internal/cli/run.go)

The command line is a cobra tree (`execute`): `zemi` opens the TUI, `zemi run` answers one
prompt on stdout, and `sessions` (and `sessions export [--full] <id>`, `hostapi.SessionMarkdown` on
stdout, `--full` as `/export full`; `projectStore` opens the store through `store.OpenRead`, which creates, migrates and
sweeps nothing and takes no lock a running zemi waits on), `projects` (`hostapi.ListProjects` with its
stores read the same way, one line each, the absolute root at column 36 for `cut -c36-`), `models` (and `models update`),
`plugins`, `health`, cobra's `completion` and `help` are subcommands, each declaring its
`Args` (cobra checks positionals only on the root). The listing ones (`sessions`, `projects`,
`plugins`, `health`) take `--json` (`jsonFlag`): JSON Lines through `writeJSON`, the one writer
`zemi run --json` uses too. Their plain lines are colored only on a terminal (`paint` and `pad`
over `colorprofile.Detect`, basic ANSI colors and no Lua theme, which they never boot), with a
header row there only, so a pipe reads the same bytes as always. A prompt argument is sent as
the first message, or run as a command when it names one (`Host.SendPrompt`, TUI and `zemi run`
alike, so `/review` works from a git hook); a prompt whose first word names a zemi command goes
after `--`, and a lone word a near miss of one is refused (`options.finish`), since it would pay
for a model call on a typo. Exit status 2 is anything that fails before the root's
`PersistentPreRunE`, which cobra runs once flags and arguments passed; after it a command's
error is 1, and one that printed its own diagnostic returns `exitError`. That hook runs `-C
dir` (`enterDir`: `PWD` follows) before anything else, then resolves `config.Dirs` once into
`options.dirs`, which every command reads (`completionDirs` resolves its own, since cobra runs no
hook before a completion function), and a switch from `/projects` is an exec
of `zemi -C <dir>` and nothing else (`relaunch`), so no flag of this run widens to another
project. Piped stdin (1 MiB at most) is attached
behind the typed prompt, is the typed prompt itself after `-` (`options.text`: sent to the model even
when it starts with `/`, so piped text runs no command), and is a usage error for the TUI.
Completion offers session ids from the project store (`completeSessions`) and plugin names
from `plugin.Scan`; `completionDirs` applies `-C`, since cobra runs no hook before a
completion function. `zemi models` and the `-m`, `--profile` and `--mode` completion boot
inert (`bootHeadless` with `Host.Inert`): no store, no log file, `proc.Refuse`, and four
guards (`resolveModel` calls no model, `luaGate` refuses all but reads, `addMCP` starts no
server, `installNamespaces` syncs no files), so a tab press runs nothing config asks for.
Flags apply after boot so they win over `init.lua`, except `--trust`, which the trust boot step
reads. `-m`, `/model <provider/model>` and `zemi.model.set` write the conversation's slot - the active
profile's own (`Host.ConversationTarget`), else `models.chat` - so a pick under a profile moves what the
conversation runs on (`--profile` applies before `-m`).
`Host.SetModel` parses the rest of `/model` and `Host.CompleteModel` offers the same grammar.
`--profile` and `/profile` (`Host.SetProfile`) write `agent.profile` for the next send: a switch
changes the prompt and the tool set, so it busts the provider's prompt cache once.

`zemi run` and `zemi health` boot headless (`bootHeadless`, the TUI's steps without it; its
`stderr` is `Host.Stderr`, which `zemi run` makes its view before boot, so a diagnostic prints
above the tail). `zemi run` (`runHeadless`, internal/cli/run.go; DESIGN §8 Printed runs) runs one turn, or a
goal's chain of them. It sends on `GoSafe` and polls `Host.RunFrame` every 100 ms: `ui.RunView`
renders the chat and working widgets over the frame with `UIState.Printed` (`ctx.headless`),
and the `follower` turns the same frames into `--json` events, each once (a call by
`ChatMessage.ID`, which `flushOutput` sets). An ask no UI can answer is denied as
`tools.ByNobody` and recorded (`Host.HeadlessDenials`, subagents' too). It also drops what
needs a user (`headlessExcluded`), the web tools and the `web` agent when nothing could allow a
fetch (`webOff`), the agent tool's `background` and `send_to` (a detached
report would outlive the run), the workflow tool's `background` and every `action` but
`status`, `list`, `wait` and `rerun`, and the prompt lines naming them, and a plugin's `zemi.agent.send`
raises there (nothing would bound its chain of turns). With no UI `Host.Notify` prints a
warning or an error to `Host.Stderr` as "zemi: <title>: <body>", so `zemi run` never swallows
one. A command that opens a screen gets `Host.Screen`'s `ErrNeedsTUI` there, which
`RunCommand` names ("/help needs the TUI", status 1); `/btw` claims the agent like
`/compact`, so `zemi run` waits on it and ctrl+c cancels it.

`zemi plugins [verb]` (`pluginsCommand`) runs `plugin.Run` and exits: no Lua boot, TUI, trust or store, so it works beside a running TUI, whose config watcher
reloads on the change. `delete` asks on the terminal, `--yes` answers, and off a terminal it refuses,
so an agent's command cannot delete a plugin. `test` is the one verb that boots Lua (below). `/plugins` (hostapi/pluginscmd.go) takes the same verbs plus `toggle`,
`open`, `new` and `modify <name> [what to change]`, which send the agent a prompt (under
`zemi run` too) pointing at the zemi skill's plugins.md (`modify` records the plugin in
`madePlugins`). A usage error names only its verb's grammar (`pluginsErr`), which fits a toast. Its list is built
off the key loop; the git verbs run on `GoSafe` and refresh the list (`Overlay.Refresh`: it
replaces an open one in place and opens nothing once esc closed it).

`zemi plugins test <name|dir> [pattern]` (internal/cli/plugintest.go, hostapi/plugintest.go; DESIGN §5) runs a
plugin's `.zemi/tests/*.lua`: `testPlugins` resolves a name as the plugins step would (`hostapi.PluginDir`),
copies the plugin into temp dirs (`copyPlugin`: `.zemi` whole with the layer dirs, nothing else
of the root, no `node_modules`, a link only when relative and inside, capped), and boots one zemi per case in this
process (`hostapi.NewPluginTest`,
`bootSteps` with no trust and the test file where init.lua runs), which `Host.Close` ends. The
first boot collects the case names. `proc.Refuse` makes the process start nothing, git and the
sandbox probe included. The test zemi runs on a fake `lua.Clock` only `test.advance` moves, with
`WholeTasks` so a driver waits for what it started, and is quiet when no `GoSafe` goroutine runs
(`goes`) and no timer is due. `luaGate` asks `PluginTest.reply` first about a call that does IO:
only a `test.stub` reply passes, standing in for the transport at the call site, and a `false`
reply raises the missing-rule error whatever rules exist. The assertions are the builtin module
`zemi.test.assert`. `zemi.test` exists only there
(`TestNamespaceVersion`); `TestPluginsTest` runs it over `testdata/plugins/demo`.

A first run writes a fully commented starter `init.lua` (`config.Dirs.Ensure`), and a TUI
boot no model can run opens the `/model` picker once (`Host.OfferModel`, after the flags
apply, never on a reload or after a failed init.lua). Builtin
providers come from the catwalk catalog (`agent.LoadCatalog`, converted at boot by
`agent.Builtin` with the key env vars resolved in Go, never stored in config, into the
`Host.Builtin` pointer): the embedded one, or the one `/model update` or `zemi models update`
(`agent.UpdateCatalog`) fetched into `<data>/catwalk.json`, never `<cache>`, which a sandbox
can write, since the catalog picks where each key is sent. A fetched one reads only the
variables the bundled one names (`bundledRef`), so the feed cannot pick which secret goes. No
network at boot. Each builtin is on only while its key is exported, one without is in
`Registry.Missing` so the no-model hint (`NoModelHint`) can name the variable, and
`Registry.Reset` seeds every generation before `init.lua` (DESIGN §6.3).
Config reload is not fsnotify: main hands the host its reload and a fingerprint of the config
trees (`setReload`, `Host.SetReload`; past dot-directories, `node_modules` and the editor configs
zemi writes itself, `.luarc.json` and `.emmyrc.json`, plus the local plugins, the plugins'
`disabled` file and each clone's git index, `pluginRoots`;
an untrusted project's `.agents` is not in it, a trusted one's is, `worktrees` aside), whose
baseline every reload moves. Once a second while no turn is active the watcher calls
`Host.ReloadIfChanged`, which reloads when the fingerprint moved; `zemi run` has no watcher. A replay that writes a key the
same way as the last boot is skipped (`cfgReplayed`, which `zemi.config.set` and the setters with
a key of their own, `zemi.permission.mode.set` and `zemi.model.set`, all
run a valid value through) and a key the new config writes fewer
times is settled to its last value (`cfgSettle`), so `/model`, `/effort`, `/profile`, the mode
cycle and the flags survive a reload until the config changes that key. A key the config stopped
writing returns to its saved pick, else its schema default, unless a runtime pick moved it since.

Runtime picks outlive the process: the layers are schema default < saved pick < init.lua <
this run. Every runtime write goes through `Host.writeSetting` (`setWatched`, and
`zemi.config.set` and `zemi.model.set` past boot), whose `keepPick` keeps the value in the
global store's kv as `config:<path>`, or deletes the row when the value is `cfgBase` (config's
last write when init.lua wrote the key, else the default). It keeps nothing while booting or
before `Host.KeepPicks`, which main turns on after the TUI's flags apply, so a flag, `zemi run`
and `zemi health` keep none, and never a mode whose base is yolo (never fail open across restarts).
`NewHost` applies the rows before any Lua (`loadPicks`, each through `Settings.Check`, a yolo
row left out since user Lua can write kv directly), so init.lua still wins; per user, not per project.
A plugin key's row waits unchecked in `picked` until `zemi.config.add` registers the key and
applies it, and `sweepPluginState` drops a deleted or reinstalled plugin's rows.

`/config` (hostapi/configcmd.go) lists every schema key with its `cfgSource` (`init.lua`,
`saved`, `default` or `this run`) and value. A key changes on a screen of its own, its picker or
`/config edit` (its choices, else a one-line prompt whose `Overlay.Run`, Go-only, runs `config set`
with the text), which closes back onto the rebuilt list. `/config` and `/effort` write through
`Host.SetConfig`, which sends `models.*`, `permissions.mode` and `agent.profile` through their
own setters and refuses a `ui.theme` no theme answers to (`checkTheme`); `zemi.config.set` takes
any name, since a plugin's or the project's theme may not be added yet, and `CheckTheme`
records one nothing added once boot is done, while whorl draws. A key's `Choices` refuse any other value in `config.checkKind`,
which also takes `default` for a "" choice, `zemi.config.set` included; every runtime write,
those setters included, fires the key's watches through `setWatched`. zemi never edits
init.lua. Under `zemi run` only `config set` runs.

## Architecture and control flow

`main.go` is the entry alone; the front end lives in `internal/cli`. Boot order (each step is a `lua.BootStep`, see `bootSteps` in internal/cli/boot.go),
and the order matters: host namespaces (`zemi.*`) -> builtin commands ->
`require("zemi.tui")` (shipped theme, widgets and keymaps, one module each) -> project
trust (`trustGate.step`, before any config that can start an MCP server) -> plugins -> user
`init.lua` -> user skill hooks -> a trusted project's `.agents/init.lua` -> markdown commands
(`commands/*.md`: user, each plugin's, then a trusted repo's `.agents`, then skill commands) -> `app.ready`.
Boot runs on its own goroutine and may block on the trust prompt.

**Project trust** (internal/cli/boot.go `trustGate`, DESIGN §5): one decision per repository, keyed by
`Dirs.TrustRoot()` (the symlink-resolved repo root, searched no higher than below `$HOME` so a
dotfiles repo is nobody's root; a linked worktree maps to its main checkout by reading the
`.git` file, never by running git) and saved in `<state>/trust.json`. Order: a decision held
for this process (`--trust`, a Later answer, `/trust` where saving is impossible), the saved
one, then the Go-owned prompt. `$HOME` and `/` are never asked about or saved
(`hostapi.TrustUnsaved`), and the TUI does not ask at start where the splash, its first screen
with no prompt argument and no resume, offers other projects (`Host.OffersProjects`: a dir
outside a repository), which runs untrusted as after Later. `/projects` (hostapi/projects.go) lists every root zemi keeps state
for (`ListProjects`), switches by restarting (`UIHooks.Relaunch`, past the quit veto, then
main's exec: the project is bound at start and never changes in place; refused while a turn,
an agent run or a workflow lives), forgets another project's decision, never granting one, and
deletes another project's data (`deleteProject`; `store.ClaimForDelete` refuses while another
zemi has the store open). `/trust` reopens the prompt off the UI goroutine, saves, and calls `Host.Retrust`, which sets
the decision and reloads when it changed (a reload that fails before the trust step must not
leave the old decision in force) under the watcher's lock, so the watcher does not reload first
for the tree the change moved. `Host.ProjectTrusted()` gates project
`.agents` (`Dirs.ProjectConfigDir`, at the repo root only: init.lua, lua, skills, agents,
commands, workflows), instruction files (AGENTS.md, else CLAUDE.md, per directory) and profile `context.files`,
project skills, every project language server, the Lua tool's ask, the read jail reaching past the
launch dir to `Dirs.RepoRoot()`, and what auto mode and read-only agents settle. `Host.git` and
`internal/plugin` shell out to the `git` binary (no git library in go.mod) through `proc.Git`,
with `GIT_CONFIG_NOSYSTEM=1`, and switch off fsmonitor, hooks, submodule recursion, LFS
smudging, log's signature checks (`gpg.program`), every transport but https, ssh and file, and repo-local filter drivers (a plugin
clone's global ones too) in every repository, trusted or not: a sandboxed command can plant a
nested repo whose config zemi would run.

## Store

The store (`internal/store`) is SQLite via modernc.org/sqlite (pure Go), WAL,
`MaxOpenConns(1)`. Two handles: a global DB (memory, kv, skills) and a per-project one under
`<state>/projects/<hash of Dirs.TrustRoot()>/`, whose `root` file names it (`store.ProjectRoot`
reads it only when it hashes to the dir), so a subdirectory and a worktree share the
repository's sessions and memories. Schema is one `schema` const of `CREATE TABLE IF NOT
EXISTS` run on open - add tables and columns there. There are no migrations before the
first release: a schema change bumps `schemaVersion`, and a store stamped with another one
is refused with its path, to delete (the owner is the only user).

## Turns, sessions and the learning loop (internal/hostapi: agent.go, session.go, compact.go, memory.go, reflector.go)

- **Models are targets.** `models.chat` runs the conversation; `models.agents` (subagents with
  no model of their own), `models.titles`, `models.compact`, `models.memory` (the
  reflector) and `models.planner` each fall back to it while empty (`roleModel`). Nothing falls
  back to a cheaper model on its own, so the `/model roles` list shows every mapping. A
  `models.*` value is `""` or an id with a `/` (`config.set` refuses a bare word), so targets
  never nest. A profile is a target: its definition's `model` is the slot's default and
  `models.<name>` overrides it, while `models.chat` moves the conversation and no other slot. A
  slot `zemi.model.add` registered is a target like the roles, with no shape of its own
  (`addTarget`; the registry lives in `agentHost.targets` under its own leaf mutex, cleared by
  `PurgeGeneration`). Anything that takes a model — a profile, an agent definition, a workflow —
  takes a target name or a `provider/model` id (`modelRef`). `/health` checks each distinct
  target model.
- **Classifying a state** (`zemi.classify.ask`, classify.go, `internal/classify`): one call
  judges every question of a `state` table and answers a typed value per question, so nothing
  parses a sentence (DESIGN §6.3). The ladder is fixed: `protocol` when the target is a
  `decisions` model of a `system_one` provider (`Providers.Decisions`; the state and questions
  go to `{base_url}/systemone` with the key as a bearer token and the answers carry real
  probabilities), else `logprobs` (one prompt per question, its answer the softmax over the
  label tokens), else `schema` (the structured ask per question type, which has no confidence
  to report). A strategy the model cannot answer through is skipped and one that fails does not
  stop the next; the call is an error when the last one fails. Every answer is validated
  against its question - an unknown label, a probability outside 0..1, a question the reply
  left out, a score outside its levels, a body that is not JSON are errors, never a nearest
  match - and `classifyQuestions` raises on a state or a question no model could be asked. The
  call is gated as `lua.classify.ask` (project code may not call a model at all), billed to the
  live session, and waits `classifyTimeout`, a minute. A `decisions` model is never resolved as
  a chat model: only the slots `zemi.model.add(name, { classify = true })` made take one.
- **One ledger.** Every model call is billed through `Host.bill`: the session row, and the
  live tally when that session is still open. A runner's call counts as its stream
  finishes (`StreamHooks.StreamDone`), before its tools run, so the tally and a subagent's
  row move during a slow tool; a subagent's run bills once, at its end. Calls outside a runner (summary, title,
  reflector) go through `Host.generate`, which bills them and sends the provider's body fields
  without the effort; the reflector bills the session it read, closed or not.
- **The active profile is a config key.** `agent.profile` names what `resolveProfile("")`
  runs (`default` is the shipped shape), so only user config writes it and a reload keeps a
  runtime pick like any key. `PurgeGeneration` clears the definitions and `cfgSettle` returns
  a key config stopped writing to `default`, so deleting a profile and its `config.set` line
  runs the shipped one; a name nothing defines (a typo, a pick of a since deleted profile)
  fails the send. A profile is defined with `zemi.agent.profile.add`: a `-name` denylist or an
  allowlist of tools, so a new tool is in the default set until a profile drops it. An allowed
  name no tool has is an `/errors` entry; a `-name` no tool has fails the send, since the tool
  it meant to drop may run under another name (`profileTools`).
- **One runner per send, one conversation.** `ensureRunner` rebuilds the runner (model and
  tools may change; the prompt comes from `ag.pin`); `AgentSend` carries the previous idle runner's conversation over (`Runner.Continue`), and
  every step's input passes `Runner.signed`, which drops the thinking whose signed prefix
  (system prompt, tool names, earlier messages) changed since the last step, so no history
  edit replays a signature Anthropic refuses; the open tool loop past the last user text
  (a steered one joins the tool results and closes nothing) keeps its own until that loop closes, since Anthropic refuses a final tool_use without it
  on every account. `runTurns`
  loops the queue's follow-ups on one runner through `sendCompacting` (compact.go): compaction
  runs before a send, when `Runner.Full` cuts a turn at a step boundary, and once after a
  context-too-long refusal. The size is `agent.ContextUsed` of the last step, never a sum; an
  unknown window means no auto compaction. When `models.compact` is the runner's model
  (`ag.turnModel`, "" for the bare runner a resume or rewind leaves) the summary is asked
  through `Runner.Aside` after the whole unpruned history, with a note naming how many
  messages stay as the verbatim tail (`summarizeAside`, `compactNote`), so it reads the
  prefix the last step just cached; a separate compact model, an overflow (the whole
  history was just refused), or an aside with no summary or a context-too-long refusal
  gets the flat `summarize`. The summary row is followed by the verbatim tail stored with
  row role `tail`, which resume reads and transcript readers skip. An unfinished
  todo list goes after `</summary>`, where `summaryOf` drops it, so a stale one is never
  merged.
- **A turn's work survives the turn.** `Runner.Persist` writes each step's messages from
  `OnStepFinish`, so a cancelled or crashed turn keeps what it did; the next send runs
  `closeToolCalls` over the history first, inserting an error result after any tool call left
  unanswered (providers refuse a dangling call); a reply cut mid-arguments by a provider that
  then drops the call (openai-compatible) gets a note naming the pending tool instead, so
  the model does not repeat the same oversized call. Every resolved model is wrapped in `idleModel`
  (providers.go): a call that makes no progress for 5 minutes is cancelled, since the HTTP
  header timeout stops counting once the status line arrives and a stalled stream would hold
  the turn forever; the timer resets on each stream part and tools run between model calls, so
  a slow build is never covered.
- **Steering.** Messages sent mid-turn queue; `Runner.Steer` injects them at the next step
  boundary through fantasy's `PrepareStep`, which rebuilds step input from scratch, so steered
  messages are re-inserted every step. Leftovers become one follow-up turn, and `active` stays
  held across that handoff (`nextTurn`) so a submit cannot jump the queue; `/compact` and
  rewind claim `active` and a `turnScope` together for the same reason. With nothing left, a
  set goal's continuation is the other follow-up `nextTurn` starts under the claim. `release`
  settles the todo list on the way out, and `ResumeSession` settles it too: a leftover
  `in_progress` goes back to `pending`, so no item reads as work in hand while no turn runs.
- **Goals.** `/goal <condition>` (goal.go, `Host.SetGoal`, DESIGN §6.3) sends the typed line with
  a brief behind `agent.Attached("goal")`; `persistMessage` sets `ag.goal` only when it stores a
  user row carrying the brief `SetGoal` last sent (`ag.goalPending`, `setGoalFrom`), so attached
  text that quotes one, or a queued /goal a cancel hands back, sets nothing. After a turn that
  ended cleanly `nextTurn` keeps the claim and `continueGoal` has `Host.aside` judge it
  (`checkGoal`, `parseVerdict`). Each verdict is a `goal` row, and `goalIn` reads the goal back
  on resume and rewind; no other reader looks at role `goal`.
- **Loops.** `/loop [interval] <prompt>` (loop.go, `Host.SetLoop`; the TUI only, DESIGN §6.3)
  keeps one `Loop` per live session in this process, never stored. Its timer is
  `lua.Clock.AfterFunc`, so the fake clock drives it in tests. `fireLoop` sends like the user
  would, on a claim taken under the lock that saw the agent idle, and arms the next run before
  sending. `loopTurnEnded` (in `runTurns`) knows a loop run by `loopTag` in the turn's text and
  acts only while `ag.loopRun`, the loop that sent the last run, is the live one, except
  that an interrupt of any turn nobody typed stops the live loop (`loopInterrupted`, which
  `continueGoal` calls for an interrupted check too). `ag.byLoop` marks a chain a run began:
  it leaves `goalRuns` alone, and the UI arms no recap and sends no alert for it
  (`StreamSnapshot.ByLoop`), only for the loop's end.
- **Side questions.** `/btw` (`Host.btw`) sends `Runner.Snapshot` (mid-turn, the running
  step's input) plus the question on the runner the next send would build, one step through
  `Runner.Aside`: same tools and tool choice, since Anthropic drops the message cache when
  tool choice changes, with every tool refusing to run. Billed, never persisted.
  `Host.aside` is that call, the goal check another; the idle recap (`Host.Recap`, `session.recap`) is one more: the UI
  calls it `recapDelay` (3 min, inside Anthropic's 5-minute cache) after a turn that ended well
  with no input since, and its live-only `Recap: ...` notice row is dropped when the transcript
  moved during the call; while it runs it holds a `Host.Busy` entry with a cancel, so esc
  stops it and the chat's pending row names it.
- **Store order.** `Messages` orders by `rowid`: a turn persists several rows in one
  millisecond and ids are random. Tool telemetry rows share the table; `historyFrom` skips
  anything that does not decode as a fantasy message, and reads a file tool's or a shell call's
  diff, an agent call's run and summary line, a step's thought time and the turn's own tokens
  and duration back from the telemetry rows that carry them (a resume could not rebuild them;
  the turn's rides one row written after its messages, which the resume gives to the reply
  those rows left).
- **Live equals settled.** The live turn holds one stretch in stream order (reasoning, calls,
  reply); `flushOutput` moves it into the transcript whenever the next part would break that
  order (reasoning after a call or a reply, a call after a reply), and at every tool call that
  starts, so a step's reasoning is a row of its own before the call that follows it rather than
  waiting for the next step or the turn's end with the run's digest. It moves only the calls that
  finished, so none settles early, and
  moves both sides under `ag.mu`, which the widget ctx reads them under (`liveView`), so no
  frame draws a row twice or not at all; `TurnDone` settles a call still open as stopped after
  an interrupt and as failed after an error. `historyFrom` splits text and merges reasoning the
  same way, so a resume draws the rows the live turn drew.
- **Proposals persist** in the scope DB until `/memory` or `zemi.learn.decide`. `/memory` is a
  Go-built toggle list whose actions are `memoryCommand` verbs by row id ending in `--yes`, which
  no note takes, applied at once; each, and a saved edit (`o` opens the raw text through
  `EditFile`, re-running intake as user origin and keeping the id), pushes its reverse onto
  `Host.memUndo`, which `u` pops and opening the list empties, and rebuilds the whole list, since
  its `u` key shows only while the stack holds a step.
- **Reflector timing** (DESIGN §10.3). It runs on session close (`/new`, `/resume`), once the
  open session has had no turn for `ReflectIdle` (`Host.ReflectLive`), and in a startup sweep
  (`ReflectSweep`, once per process, not on a reload), never on quit. `reflectDue` reads the
  delta past kv `reflector.seen.<sid>` (`RewindTo` clamps it, `ForkSession` starts it at the
  copied history) and holds a floor, since one exchange that went fine would cost a model call
  at every launch; `/reflect` skips it. The sweep records every session it listed with the count
  it held (`Host.swept`), and `reflectPending` skips a session still at that count, so one
  process reflects a session's rows once; the sweep itself runs at most one call
  (`reflectSweepMax`), a backlog a crash or a `zemi run` left draining one per process.
  `claimReflect` takes an atomic kv claim per session (`reflector.claim.<sid>`, leased), so two
  windows never reflect it at once. A reply that is not the JSON document asked for marks the
  delta seen and is the `/health` reflector row's news, never an `/errors` entry. It proposes memories only: each entry passes `learn.Intake`
  and `gateMemory` like the `memory` tool's, and an entry with empty text and `replaces`
  queues a forget proposal, never auto-applied. There is no config-patch pipeline, the agent
  edits config files directly. The brain build (`brainFor`) renders the `# Brain` section: the
  partitions' catalogs, the memory lines newest first per scope under the scope budget, the loop
  text and the lint warnings; no search, no ranking.
- **Provider text shape.** Model responses carry `fantasy.TextContent`, stored messages
  `fantasy.TextPart`; `concatText` handles both. A stub that returns the wrong one hides real
  provider bugs (this once made the reflector read empty output everywhere).
- **Jail.** `inJail` resolves symlinks on both the path and the roots; `fsDefault`,
  `resolveJailed` and `@file` attachments all rely on it.

## Rewind, read-only agents, prompts, composer bridge

- **Rewind** (rewind.go, store/checkpoints.go): the pipeline's `Checkpoint` hook saves a file's
  pre-turn content (keyed by the turn's user message, first write wins) before a write tool
  touches it. `RewindTo` restores files first and only then truncates messages, so a failed
  restore leaves the conversation intact; then it drops what the session learned between the
  prompt and the last row (`forgetLearned`: a reflector run or an accept after it stays) and
  rebuilds the todo list from the kept rows. The cut holds `cutMu`, which a reflector run takes
  to store what it read, so a run whose rows a rewind removed during its model call stores nothing. A shell call's changes are checkpointed from the
  before tree `treeDiff` compares against (repository files only: not ignored ones, not outside
  a repository, no submodule or secret-shaped file, and not once two slow snapshots switched the
  snapshot off); a symbolic link comes back as a link.
- **Read-only agents** (the planner profile, `Profile.ReadOnly`) are enforced in
  `tools.Pipeline.Check` (`Plan` hook): `fs.write` denied even under allow rules, `mcp.*`
  allows and a rule's shell allow other than `ReadCommand` flip to ask; the sandbox keeps the
  project unwritable. Approving the plan returns to the agent and mode that ran before: the read-only
  turn ends (the read-only pipeline cannot implement) and `nextTurn` hands the claim to a
  fresh send as the restored agent (`planApproved`, the plan already in history), so the
  agent never passes through idle. A read-only turn that ends in plain text instead of the
  `question` or `plan` tool is nudged once per user message (`planFollowup`, `planNudge`):
  a question or a report is not a plan, so the text is never presented for approval.
- **Go-owned prompts** (permission, trust, question) share one renderer in ui/agent.go
  (`promptBox`, `choiceRows`). The permission prompt reads only typed fields (`PermRequest.Where`,
  `Runs`, `Agent`, `Owner`), never meaning parsed back out of a reason or preview string.
  Lua reads the composer through a snapshot the UI publishes and
  edits it through `UIHooks.ComposerEdit`, applied on the UI goroutine.
- **System prompt** (prompt.go): `basePrompt` + `delegationPrompt` for the default profile,
  then `zemiPrompt`, the read-only agent's plan section, the user, builtin and plugin skills, the
  user's `<config>/AGENTS.md` under `# User instructions` (its own budget) and Lua sections
  (`agent.assemble`, then the `prompt` of each Lua tool in the tool set). Of the project it holds
  only what trust decides (the sandbox rules among them) and what a trusted project's own Lua
  adds, so a provider that caches a shared prefix (DeepSeek, OpenAI, Gemini, Anthropic's
  breakpoints) keeps it and the tools after it for every session of the user
  (`TestPromptSameAcrossProjects`). What varies is the context message (`agent.Runner.Context`), a
  user message every request sends after the system prompt and never in `History`: the
  repository's instruction files under `# Project instructions` (a 32 KB budget spent deepest file
  first, so a huge root `AGENTS.md` cannot squeeze out the package's own), the project's skills
  and recipes (`skillsIndexPrompt`'s second half), the per-session pinned `environmentPrompt` (in a
  trusted repository with its top level as the read tool lists it, `topLevel`, and the last
  commits: repository free text, left out untrusted), the
  `# Brain` section and, for the main agent on the shipped prompt, `replyReminder` last.
  The main agent stores it as a `context` row (`contextRow`, which no transcript reader reads)
  whenever the pin is built anew, and a resume sends the stored one again while `contextKey`
  (the pin key, working directory, scratchpad and brain render) holds, so the provider's cache of the whole
  conversation holds too; `dropPin` forgets it. It is built from the
  run's tool set (`hasTool`), main agent and subagents alike: no `agent` tool drops
  `delegationPrompt`, no `skill` tool drops the skills index, no `read`
  drops its "Using tools" line, and a read-only agent without the `plan` tool
  (headless, a profile) ends in `planHeadless`. No kept tool's description names a dropped tool
  (`TestPromptFollowsToolSet`). Anything that varies per turn must be
  pinned per session or it busts the provider's prompt cache - which is why `gitStatusSection`
  runs once per process, warmed at boot so a big repository's `git status` is not paid for at
  first token. The main agent's prefix is one pin (`ag.pin`, `pinPrompt`): the system text
  and every tool's `Info` (through `pinnedTool`), sent verbatim while its key holds (profile,
  model, `ProjectTrusted`, the mode's prompt, whether the model takes images, each tool's name and input schema, since a stale
  schema fails calls). `closeSession` (`/new`, `/resume`) zeroes it, `compactDone` and
  `deleteRecipe` clear its key (`dropPin`: `/prompt` and a running turn keep the text, and a
  build a drop overtook is not stored); rewind keeps it, and a reload drops nothing, since the config watcher reloads on
  any config-tree write, the model's own skill writes included. So instruction files, skills,
  descriptions (saved workflows, agent definitions) and `agent.assemble` output are read only
  when it is built, and a hit costs no file read and no Lua. The one exception, an instruction
  file edited by hand, rides the next user message once (`instructionEdits`, DESIGN §6.3).
  Subagents build theirs once per run (`systemWith`); a fork sends the caller's own text and
  tools instead, which is the cache its cheap start reads (`Runner.Fork`, `pinnedTool`).
  `TestPromptBudget` caps what the default request carries before the first message (system text
  and tool definitions); raise it only with a reason.
- **Tool schemas**: fantasy wraps `ToolInfo.Parameters` as the properties map, so tools pass
  `tools.SchemaProperties(schema)` and a non-nil `Required`; strict providers reject anything
  else.

## Gotchas

- **Sanitize at boundaries.** Model-influenced strings pass through `internal/sanitize` before
  reaching widgets or overlays; the log's slog handler quotes any key or value holding a
  newline or an unprintable rune. Secrets are scrubbed by the learn intake scrubber at the
  memory/store boundary - don't log raw tool output or memory text.
- **Memory intake policy** (`internal/learn`): text cap 300 and quote 160, provenance required,
  `learn.Similar` refuses a near-duplicate (the user's own text only a word-for-word repeat),
  `origin` (`user` or `learned`) gates autonomy and the replacement guard, no kind.
  `memory.autonomy` is user config only.
- **Config store** (`internal/config/store.go`): dotted-path keys from a Go-owned schema plus
  the `plugins.<name>.*` keys a plugin registers while it loads (`zemi.config.add`, `Store.Add`;
  plugin code names its own by the bare key, `settingPath`;
  `dropPluginKeys` takes the ones a replay did not register again, and `settingOff` makes a
  `set` or `watch` of one whose plugin did not load do nothing); only
  user config sets them, and a read of a key outside it panics under `go test`. Providers ship
  none; user config registers them, keys included (`api_key = zemi.env.get("NAME")`;
  `zemi.env.get` reads secret-shaped names for user and builtin origin only, and for any
  origin under `--unsafe`). Config fields
  hold values, never names of env vars.
