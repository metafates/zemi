---
name: zemi-host-flow
description: "Load before changing main.go, internal/cli, internal/config, internal/store, internal/classify, or hostapi agent, chatlog, memtree, session, classify or prompt code: boot order and trust, the CLI, config reload and kept picks, turns, the chat log and its builder, reset and the pinned prompt."
---

# Host flow: CLI, boot, config, turns

## CLI (internal/cli/cli.go)

The command line is a cobra tree (`execute`): `zemi` opens the TUI, and
`projects` (`hostapi.ListProjects` with its
stores read through `store.OpenRead`, which creates, migrates and
sweeps nothing, runs `query_only` so even a write through it fails, and takes no lock a running zemi waits on;
one line each, the absolute root at column 36 for `cut -c36-`), `models` (and `models update`),
`db` (one read-only query against the project's store, its first argument whole and the rest bound to `?1`, `?2`, ..., `--global` the global one), `plugin` (bare, it lists), `health`, cobra's `completion` and `help` are subcommands, each declaring its
`Args` (cobra checks positionals only on the root). The listing ones (`projects`,
`plugin`, `health`, `plugin marketplace list`) take `--json` (`jsonFlag`): JSON Lines through `writeJSON`, the one writer
every listing shares. Their plain lines share one `table` renderer (aligned columns,
`paint` over `colorprofile.Detect`, basic ANSI colors and no Lua theme, which they never boot),
with a header row there only, so a pipe reads the same bytes as always. A prompt argument is sent as
the first message, or run as a command when it names one (`Host.SendPrompt`, so `/review` works
from the composer's first send); a message prompt is held (`Host.HoldPrompt`) from the TUI's first
frame - it is the transcript's first row then and claims the turn it starts - and `SendHeld` runs
that turn once boot and the MCP starts settle; esc takes it back to the composer. A prompt whose
first word names a zemi command goes
after `--`, and a lone word a near miss of one is refused (`options.finish`), since it would pay
for a model call on a typo. Exit status 2 is anything that fails before the root's
`PersistentPreRunE`, which cobra runs once flags and arguments passed; after it a command's
error is 1, and one that printed its own diagnostic returns `exitError`. That hook runs `-C
dir` (`enterDir`: `PWD` follows) before anything else, then resolves `config.Dirs` once into
`options.dirs`, which every command reads (`completionDirs` resolves its own, since cobra runs no
hook before a completion function), and a switch from `/projects` is an exec
of `zemi -C <dir>` (`relaunch`), so this
run's other flags are left behind. The TUI takes a terminal on stdin and stdout, and refuses
a pipe on either.
Completion offers plugin names
from `plugin.Scan`; `completionDirs` applies `-C`, since cobra runs no hook before a
completion function. `zemi models` and the `-m` completion boot
inert (`bootHeadless` with `Host.Inert`): no store, no log file, `proc.Refuse`, and four
guards (`resolveModel` calls no model, `luaGate` refuses all but reads, `addMCP` starts no
server, `installNamespaces` syncs no files), so a tab press runs nothing config asks for.
Flags apply after boot so they win over `init.lua`, except `--trust`, which the trust boot step
reads. `-m`, `/model <provider/model>` and `zemi.model.set` write `models.default`, so a pick moves what the
conversation runs on.
`Host.SetModel` parses the rest of `/model` and `Host.CompleteModel` offers the same grammar.

`zemi health`, `zemi models` and the flag completions boot headless (`bootHeadless`, the TUI's
steps without it; its `stderr` is `Host.Stderr`). None of them sends a prompt: a host with no
UI never runs a turn. It still drops what
needs a user (`headlessExcluded`: a no-UI host cannot answer a question or approve a plan).
With no UI `Host.Notify` prints a
warning or an error to `Host.Stderr` as "zemi: <title>: <body>", so an early failure is never
swallowed. A command that opens a screen gets `Host.Screen`'s `ErrNeedsTUI` there, which
`RunCommand` names ("/help needs the TUI", status 1).

`zemi plugin [verb]` (`pluginCommand`) runs `plugin.Run` and exits (`marketplace list` prints itself from `plugin.Marketplaces`, as the listing commands do): no Lua boot, TUI, trust or store, so it works beside a running TUI, whose config watcher
reloads on the change. `delete` asks on the terminal, `--yes` answers, and off a terminal it refuses,
so an agent's command cannot delete a plugin. `test` is the one verb that boots Lua (below). `/plugin` (hostapi/plugincmd.go) takes the same verbs plus `toggle`,
`open`, `new` and `modify <name> [what to change]`, which send the agent a prompt (they run
before the UI attaches too) pointing at the zemi skill's plugins.md. A usage error names only its verb's grammar (`pluginsErr`), which fits a toast. Its list is built
off the key loop; the git verbs run on `GoSafe` and refresh the list (`Overlay.Refresh`: it
replaces an open one in place and opens nothing once esc closed it), and a busy line names what it acts on (`marketplaceLabel`: the source for add, whose clone decides the name, the argument for the rest). Enter on a
marketplace row opens its drill-down (`/plugin marketplace <name>`, TUI-only): the
marketplace's update and remove, then one row per entry, install on the ones missing
(the installed ones carry no key: they move with the marketplace's pull); its `Reload` rebuilds it after its own actions, and an
unreadable manifest is named on the row (`MarketplaceInfo.Err`), never shown as no plugins.

`zemi plugin test <name|dir> [pattern]` (internal/cli/plugintest.go, hostapi/plugintest.go; DESIGN §5) runs a
plugin's `.zemi/tests/*.lua`: `testPlugins` resolves a name as the plugins step would (`hostapi.PluginDir`),
copies the plugin into temp dirs (`copyPlugin`: `.zemi` whole with the layer dirs, nothing else
of the root, no `node_modules`, a link only when relative and inside, capped), and boots one zemi per case in this
process (`hostapi.NewPluginTest`,
`bootSteps` with no trust and the test file where init.lua runs), which `Host.Close` ends. The
first boot collects the case names. `proc.Refuse` makes the process start nothing, git included. The test zemi runs on a fake `lua.Clock` only `test.advance` moves, with
`WholeTasks` so a driver waits for what it started, and is quiet when no `GoSafe` goroutine runs
(`goes`) and no timer is due. `luaGate` asks `PluginTest.reply` first about a call that does IO:
only a `test.stub` reply passes, standing in for the transport at the call site, and a `false`
reply refuses the call. The assertions are the builtin module
`zemi.test.assert`. `zemi.test` exists only there
(`TestNamespaceVersion`); `TestPluginsTest` runs it over `testdata/plugin/demo`.

A first run writes a fully commented starter `init.lua` (`config.Dirs.Ensure`), and a TUI
boot no model can run opens the `/model` picker once (`Host.OfferModel`, after the flags
apply, never on a reload or after a failed init.lua). Builtin
providers come from the catwalk catalog (`agent.LoadCatalog`, converted at boot by
`agent.Builtin` with the key env vars resolved in Go, never stored in config, into the
`Host.Builtin` pointer): the embedded one, or the one `/model update` or `zemi models update`
(`agent.UpdateCatalog`) fetched into `<data>/catwalk.json`, never `<cache>`, since the catalog picks where each key is sent. A fetched one reads only the
variables the bundled one names (`bundledRef`), so the feed cannot pick which secret goes. No
network at boot. Each builtin is on only while its key is exported, one without is in
`Registry.Missing` so the no-model hint (`NoModelHint`) can name the variable, and
`Registry.Reset` seeds every generation before `init.lua` (DESIGN §6.3). A key can also
live in the OS keychain: `/keys` (or enter on a "Needs a key" row in `/model`) pastes one
under `key:<provider>` (`DataHome/keys/<provider>` as the 0600 file fallback), precedence
is init.lua `api_key` > env > keychain, a `GoSafe("stored keys")` pass after each
`Providers.Reset` fills the still-keyless providers, and a send that fails for a missing
key makes one keychain attempt itself (`resolveTurnModel`; never in `modelRunErr`, which
classify runs on the Lua actor). `/keys` pastes, replaces and removes the stored ones.
Config reload is not fsnotify: main hands the host its reload and a fingerprint of the config
trees (`setReload`, `Host.SetReload`; past dot-directories (a plugin's `.zemi` aside),
`node_modules` and the editor configs zemi writes itself, `.luarc.json` and `.emmyrc.json`, plus
the local plugins, the plugins' `disabled` file, `marketplaces.json`, each
marketplace clone's manifest and `FETCH_HEAD` (the pull's fingerprint, a
code-only commit included) and each cache clone's `FETCH_HEAD`,
`pluginRoots`;
an untrusted project's `.agents` is not in it, a trusted one's is, `worktrees` aside), whose
baseline every reload moves. Once a second while no turn is active the watcher calls
`Host.ReloadIfChanged`, which reloads when the fingerprint moved; only the TUI's launch runs the
watcher. A replay that writes a key the
same way as the last boot is skipped (`cfgReplayed`, which `zemi.config.set` and the setters with
a key of their own, `zemi.model.set`, all
run a valid value through) and a key the new config writes fewer
times is settled to its last value (`cfgSettle`), so `/model`, `/effort`
and the flags survive a reload until the config changes that key. A key the config stopped
writing returns to its saved pick, else its schema default, unless a runtime pick moved it since.

Runtime picks outlive the process: the layers are schema default < saved pick < init.lua <
this run. Every runtime write goes through `Host.writeSetting` (`setWatched`, and
`zemi.config.set` and `zemi.model.set` past boot), whose `keepPick` keeps the value in the
global store's kv as `config:<path>`, or deletes the row when the value is `cfgBase` (config's
last write when init.lua wrote the key, else the default). It keeps nothing while booting or
before `Host.KeepPicks`, which launch turns on after the TUI's flags apply, so a flag and
`zemi health` keep none.
`NewHost` applies the rows before any Lua (`loadPicks`, each through `Settings.Check`), so init.lua still wins; per user, not per project.
A plugin key's row waits unchecked in `picked` until `zemi.config.add` registers the key and
applies it, and `sweepPluginState` drops a deleted or reinstalled plugin's rows.

`/config` (hostapi/configcmd.go) opens a root of one row per section (the first
path segment, in schema order), and `/config <section>` lists that section's keys with their
`cfgSource` (`init.lua`, `saved`, `default` or `this run`) and value; the root stays open under
the section, so esc pops back. A key changes on a screen of its own, its picker or
`/config edit` (its choices, else a one-line prompt whose `Overlay.Run`, Go-only, runs `config set`
with the text), which closes back onto the rebuilt list; a value the command refuses keeps the
prompt open with the error it named. `/config` and `/effort` write through
`Host.SetConfig`, which sends `models.*` through their
own setters and refuses a `ui.theme` no theme answers to (`checkTheme`); `zemi.config.set` takes
any name, since a plugin's or the project's theme may not be added yet, and `CheckTheme`
records one nothing added once boot is done, while whorl draws. A key's `Choices` refuse any other value in `config.checkKind`,
which also takes `default` for a "" choice, `zemi.config.set` included; every runtime write,
those setters included, fires the key's watches through `setWatched`. zemi never edits
init.lua. A `/config` verb that opens a screen refuses without one; only `config set` runs.

## Architecture and control flow

`main.go` is the entry alone; the front end lives in `internal/cli`. Boot order (each step is a `lua.BootStep`, see `bootSteps` in internal/cli/boot.go),
and the order matters: host namespaces (`zemi.*`) -> builtin commands ->
`require("zemi.tui")` (shipped theme, widgets and keymaps, one module each) -> project
trust (`trustGate.step`, before any config that can start an MCP server) -> plugins -> user
`init.lua` -> user skill hooks -> markdown commands
(`commands/*.md`: user, each plugin's, then a trusted repo's `.agents`) -> `app.ready`.
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
`.agents` (`Dirs.ProjectConfigDir`, at the repo root only: skills, agents,
commands), instruction files (AGENTS.md, else CLAUDE.md, per directory),
project skills, every project language server, and the Lua tool's ask. `Host.git` and
`internal/plugin` shell out to the `git` binary (no git library in go.mod) through `proc.Git`,
with `GIT_CONFIG_NOSYSTEM=1`, and switch off fsmonitor, hooks, submodule recursion, LFS
smudging, log's signature checks (`gpg.program`), every transport but https, ssh and file, and repo-local filter drivers (a plugin
clone's global ones too) in every repository, trusted or not: a command can plant a
nested repo whose config zemi would run.

## Store

The store (`internal/store`) is SQLite via modernc.org/sqlite (pure Go), WAL,
`MaxOpenConns(1)`. Two handles: a global DB (skills, kv) and a per-project one under
`<state>/projects/<hash of Dirs.TrustRoot()>/`, whose `root` file names it (`store.ProjectRoot`
reads it only when it hashes to the dir), so a subdirectory and a worktree share the
repository's one chat. The schema is goose migrations embedded from
`internal/store/migrations` and applied on open, under a `zemi.db.lock` file lock so two
zemis never run one twice; a change is a new numbered file there, never an edit of an
applied one. A store made before goose only gains its version table: the baseline is all
`CREATE ... IF NOT EXISTS`. The one migration builds today's tables (`sessions`,
`messages`, `todos`, `tool_calls`, `skills`, `kv`, `log`, `nodes`, `view_lines`,
`usage_days`) and plants the `main` anchor session row billing and todos ride (DESIGN §7).

## Turns, the chat and the builder (internal/hostapi: agent.go, chatlog.go, memtree.go, session.go, reset.go)

- **Models are targets.** `models.default` is the root: it runs the conversation and every
  role left unset follows it; `models.agents`
  (subagents with no model of their own) and `models.compact` (the summary
  builder) fall back to it while empty (`roleModel`). Nothing falls
  back to a cheaper model on its own, so the `/model roles` list shows every mapping. A
  `models.*` value is `""` or an id with a `/` (`config.set` refuses a bare word), so targets
  never nest, and a bare `/model` pick writes the default role and no other slot. A
  slot `zemi.model.add` registered is a target like the roles
  (`addTarget`; the registry lives in `agentHost.targets` under its own leaf mutex, cleared by
  `PurgeGeneration`). Anything that takes a model — an agent definition, a workflow —
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
  call is gated as `lua.classify.ask` (a plugin needs `model` in its uses), billed to the
  live session, and waits `classifyTimeout`, a minute. A `decisions` model is never resolved as
  a chat model: only the slots `zemi.model.add(name, { classify = true })` made take one.
- **One ledger.** Every model call is billed through `Host.bill`: the `main` session row, and
  the live tally while the chat runs. A runner's call counts as its stream
  finishes (`StreamHooks.StreamDone`), before its tools run, so the tally and a subagent's
  row move during a slow tool; a subagent's run bills once, at its end. A builder call bills
  `main` too (`memtree.build` on `models.compact`). Calls outside a runner (`zemi.model.ask`,
  the goal check) go through `Host.generate` or an aside, which bill and send the provider's
  body fields without the effort.
- **The eternal chat** (`chatlog.go`): every main-chat message persists into the `log` table
  (`persistMessage` -> `logChatMessage`), a failure costing the log a line, never the turn.
  A reply is agent rows per text stretch, each call its own tool row, each result an echo
  row; reasoning is never logged. A text past 8,000 characters (`chatSplitChars`) is logged
  as several sibling rows; an echo clips to 30,000 (`echoClipChars`). Every append commits in
  one transaction and wakes the builder. `Host.Note` appends a note row (`/note`), mid-turn
  included.
- **The builder** (`memtree.go`): one worker per store owns the tree state. Leaves are
  message lines (a message of at most 512 bytes its own node verbatim; the tool and echo
  leaves template-built in the append transaction), merges join siblings, and both go out as
  calls on `models.compact` carrying the compaction view as context and the 512-dash ruler.
  Up to 8 calls at once (`buildSlots`), each build up to 5 tries in one conversation with the
  shortest fitting line kept; a failed build retries on a doubling backoff and parks after
  six failures in a row until new rows land (`buildStreak`) - except an empty reply, which no
  try can fix, and parks on its first one. Builder calls get the model's own output cap (8192
  when it names none) and, when the model lists effort levels, its lowest. A model that lists
  none (a user-spec gateway model) gets the force-off: effort low plus a
  `thinking: {"type": "disabled"}` extra-body field - reasoning models spend the whole output
  cap on thinking and return no line otherwise; the pair is measured on acme-gateway
  (GLM-5.3-Flash) and DeepSeek, where a bare effort low would turn thinking ON and the
  thinking field alone does nothing on the gateway. The view is the
  persisted sawtooth (DESIGN §10.3): one line per message, then one batch past 128,000 bytes
  merges the most due pairs down to 64,000; the compaction view is the second sawtooth in
  kv. A reset quiesces the builder - a fence: nothing
  dispatches into the wipe and every build before it has landed, the wait bounded by
  `quiesceWait` so a wedged provider call refuses the reset instead of wedging it. `/health`'s
  `chat builder`
  row reads the queue depths and the failed builds (`builderHealth`).
- **One turn is one fresh call.** `runTurns` sets `History` to `[viewMessage]` (the persisted
  view as one user message of 4-line parts, `chatlog.go viewMessage`) before every send, so
  nothing carries between turns but the view; `keepSigned` degenerates to a no-op
  cross-turn. A context-too-large refusal runs one view merge batch (`MergeOnce`, which
  returns once it ran) and retries once; a refused output cap is dropped, and image parts a
  model refuses become a note, both remembered for the process (`capRejected`,
  `imagesRejected`).
- **A turn's work survives the turn.** A run's runner wires its own `Persist`
  (`storeMessage`), so a cancelled or crashed run keeps what it did in its child session's
  rows; the next send runs
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
  held across that handoff (`nextTurn`) so a submit cannot jump the queue; reset claims
  `active` and a `turnScope` together for the same reason. With nothing left, a
  set goal's continuation is the other follow-up `nextTurn` starts under the claim. `release`
  settles the todo list on the way out.
- **Goals.** `/goal <condition>` (goal.go, `Host.SetGoal`, DESIGN §6.3) sends the typed line with
  a brief behind `agent.Attached("goal")`; `setGoalFrom` sets `ag.goal` only for a user row
  carrying the brief `SetGoal` last sent, and stores it in the kv row `goal` (the condition
  and the log seq it was set at). After a turn that
  ended cleanly `nextTurn` keeps the claim and `continueGoal` has `Host.aside` judge it
  (`checkGoal`, `parseVerdict`). A verdict is a live notice row; `goalDropped` drops the goal
  a reset wipes the chat of.
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
  `Host.aside` is that call, the goal check another; the idle recap (`Host.Recap`, `session.recap`, a duration, 3m default) is one more: the UI
  arms it `session.recap` after a turn that ended well
  with no input since, and its live-only `Recap: ...` notice row is dropped when the transcript
  moved during the call; while it runs it holds a `Host.Busy` entry with a cancel, so esc
  stops it and the chat's pending row names it.
- **The chat window** (session.go): the live transcript is the log's last `chatWindowRows`
  (1000) settled rows (`LogTail` at boot, `setTranscriptFromLog` after a reset), appended to
  and replaced whole; `InputHistory` reads the log's user rows. `historyFrom` decodes a run's
  stored messages into model history and transcript rows for its child session.
- **Live equals settled.** The live turn holds one stretch in stream order (reasoning, calls,
  reply); `flushOutput` moves it into the transcript whenever the next part would break that
  order (reasoning after a call or a reply, a call after a reply), and at every tool call that
  starts, so a step's reasoning is a row of its own before the call that follows it rather than
  waiting for the next step or the turn's end with the run's digest. It moves only the calls that
  finished, so none settles early, and
  moves both sides under `ag.mu`, which the widget ctx reads them under (`liveView`), so no
  frame draws a row twice or not at all; `TurnDone` settles a call still open as stopped after
  an interrupt and as failed after an error.
- **Provider text shape.** Model responses carry `fantasy.TextContent`, stored messages
  `fantasy.TextPart`; `concatText` handles both. A stub that returns the wrong one hides real
  provider bugs (this once made a summary call read empty output everywhere).
- **Roots.** `inRoots` resolves symlinks on both the path and the roots; `@file` attachments,
  instruction files, project LSP servers and recipe scrubbing all rely on it.

## Reset, read-only agents, prompts, composer bridge

- **Reset** (reset.go, store/log.go): `/reset` (`ResetChat`) takes the claim, `cutMu` and the
  builder quiesced around `ResetLog`, which wipes the log, the nodes, the view lines and the
  compaction view whole and then vacuums and truncates the WAL so the text leaves
  the file; the todo list clears, every goal drops, and the transcript swaps to the empty
  log. It refuses a live turn or run, and asks through `AskFirst`'s armed key or `--yes`. The
  armed key's press runs the wipe on its own goroutine (not the UI's command worker), so a
  builder wedged under the quiesce costs a toast, never a wedged UI.
- **Read-only agents** (`AgentDef.ReadOnly`) are enforced in
  `tools.Pipeline.check` (`Pipeline.ReadOnly`): every action denied - a write, a command, a
  Lua tool; reads and scratchpad writes pass. A subagent the plan builtin starts, or one
  whose agent file says `read_only: true`, runs under it; the main conversation never is.
- **Go-owned prompts** (permission, trust, question) share one renderer in ui/agent.go
  (`promptBox`, `choiceRows`). The permission prompt reads only typed fields (`PermRequest.Tool`, `Subject`,
  `Arg`, `Preview`, `Agent`, `Owner`, `Script`), never meaning parsed back out of the text.
  Lua reads the composer through a snapshot the UI publishes and
  edits it through `UIHooks.ComposerEdit`, applied on the UI goroutine.
- **System prompt** (prompt.go): `basePrompt` + `delegationPrompt`,
  then `zemiPrompt`, the user, builtin and plugin skills, the
  user's `<config>/AGENTS.md` under `# User instructions` (its own budget) and Lua sections
  (`agent.assemble`, then the `prompt` of each Lua tool in the tool set). Of the project it holds
  only what trust decides and what a trusted project's own Lua
  adds, so a provider that caches a shared prefix (DeepSeek, OpenAI, Gemini, Anthropic's
  breakpoints) keeps it and the tools after it for every session of the user
  (`TestPromptSameAcrossProjects`). What varies is the context message (`agent.Runner.Context`), a
  user message every request sends after the system prompt and never in `History`: the
  repository's instruction files under `# Project instructions` (a 32 KB budget spent deepest file
  first, so a huge root `AGENTS.md` cannot squeeze out the package's own), the project's skills
  and recipes (`skillsIndexPrompt`'s second half), the per-session pinned `environmentPrompt` (in a
  trusted repository with its top level as the read tool lists it, `topLevel`, and the last
  commits: repository free text, left out untrusted) and, for the main agent on the shipped
  prompt, `replyReminder` last. The main agent stores it as a `context` row (`contextRow`,
  which no transcript reader reads) whenever the pin is built anew, so a fork and a builder
  call that re-sends it read the same bytes. It is built from the
  run's tool set (`hasTool`), main agent and subagents alike: no `agent` tool drops
  `delegationPrompt`, no `skill` tool drops the skills index, no `read`
  drops its "Using tools" line. No kept tool's description names a dropped tool
  (`TestPromptFollowsToolSet`). Anything that varies per turn must be
  pinned per session or it busts the provider's prompt cache - which is why `gitStatusSection`
  runs once per process, warmed at boot so a big repository's `git status` is not paid for at
  first token. The main agent's prefix is one pin (`ag.pin`, `pinPrompt`): the system text
  and every tool's `Info` (through `pinnedTool`), sent verbatim while its key holds (model,
  `ProjectTrusted`, whether the model takes images, each tool's name and input schema, since a stale
  schema fails calls). `deleteRecipe` clears its key (`dropPin`: `/prompt` and a running turn
  keep the text, and a
  build a drop overtook is not stored); a reset keeps it, and a reload drops nothing, since the config watcher reloads on
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
  newline or an unprintable rune. Secrets are scrubbed by the shape scrubber
  (`sanitize.ScrubShapes`) where a recipe is written - don't log raw tool output.
- **Config store** (`internal/config/store.go`): dotted-path keys from a Go-owned schema plus
  the `plugins.<name>.*` keys a plugin registers while it loads (`zemi.config.add`, `Store.Add`;
  plugin code names its own by the bare key, `settingPath`;
  `dropPluginKeys` takes the ones a replay did not register again, and `settingOff` makes a
  `set` or `watch` of one whose plugin did not load do nothing); only
  user config sets them, and a read of a key outside it gets the zero value. Providers ship
  none; user config registers them, keys included (`api_key = zemi.env.get("NAME")`;
  `zemi.env.get` reads any name from any origin). Config fields
  hold values, never names of env vars.
