---
name: zemi-subagents
description: "Load before changing a subagent, a fork, an agent run, the workflow tool, the lua tool, MCP or LSP: definitions and nesting, run lifecycle and reporting, workflow scripts and their Lua sandbox, model memory writes, MCP tools and language servers."
---

# Subagents, workflows, MCP and LSP

## Subagents, workflows, agent runs, the Lua tool, MCP, LSP

- **Subagents** (subagent.go) are fresh `agent.Runner`s over `PipelineFor()` (same permission
  gate; an ask carries the agent's name in `Request.Agent`). Definitions: builtin `explore`, `general` and `plan` (a read-only architect the planner delegates drafting to), plus `agents/*.md` (user over builtin over plugin over a trusted
  repo's `.agents`; a file's `read_only: true` makes a read-only agent like `explore`).
  They never get `agent`, `workflow`, `todos`, `question`,
  `plan` or `lua` (nesting depth 1: only the live turn forks). A run is a child session (`parent_id`) named by the run's id;
  `ListSessions` hides children. They have no summarizer: `sendPruning` clears their own old
  tool output when the window fills and, with none left to clear, drops the middle of the
  history once, keeping the task and a recent tail. The delegation guidance lives in
  `delegationPrompt`: without it models rarely delegate. The fork is the one run that is no
  definition: `agent = "fork"` (`forkAgent`, and `agents/fork.md` is ignored with an error)
  continues the caller's conversation off the live `Runner.Fork` - the caller's model, system
  prompt, context message and request options, with the history it last sent - over a pipe of its
  own, where `forkTools` rebuilds the caller's own names and order (`Runner.Tools`) and pins the
  caller's `Info`, and its first request offers the names the caller's own request offered
  (`Runner.carried`), so the array stays identical; its calls also keep the caller's session key, so
  the provider reads the prefix it cached. It lists `agent`, `workflow`, `todos`, `question`,
  `plan` and `lua` and not `update`, since that array is the caller's.
  `forkSetup` refuses a fork asked from inside a run, one asked after the turn
  ended and one the tool asked for with `background`, so only the live turn
  starts one; its task says what it is, so it does not ask for another.
- **Workflows** (workflow.go, DESIGN §6.3) are Lua scripts (model-written via the `workflow`
  tool, saved under `workflows/`, builtin in `runtime/workflows/` - `review`, `research`,
  `hunt` - a plugin's after both). `/workflows` heads with a `New workflow` fill row and
  `new <what it should do>` sends the line with a prompt pointing at the New workflow steps
  in `workflows.md`, as `/agents new` and `/plugins new` do. They
  run in their **own** gopher-lua state on a host goroutine, never on the interpreter (DESIGN
  §4): a coroutine per branch, `zemi.workflow.agent`/`parallel` yield and the scheduler resumes
  them as subagents finish. Invariants: the script sees only base/string/table/math,
  `zemi.workflow` and `zemi.json` (no fs, net or other `zemi.*`); `agent`/`parallel` are
  `lua.Suspending` wrappers, and both refuse under `pcall` or a library callback
  (`lua.InGoCall`), where a yield would unwind the Go call. The tool's description carries the
  whole script API, inline first (a model sent to a skill wrote a file instead);
  `TestPromptBudget` counts it. A run (`flowRun`) is live work until its script ends, paused or
  between agents too: `taskReg.live` counts it, and not its agents, for rewind, fork, session
  delete, the queue drain and the statusline. The tool's `list` action names this session's
  runs (`flowsOf`), with the id to steer one: a run the user started in `/workflows` is a
  chat row the model never reads, so that list is its only notice of one while it runs. `flowControl` is
  the one control path; controls
  write the run's fields under its lock and wake the scheduler, so none waits on a script busy
  in Lua. A run's script and journal in the pad are created `O_EXCL` through `perm.Lresolve`
  and read back no-follow, regular and capped (`readPadFile`), since any command can
  write the pad, and a rerun the user starts asks about the script as the tool's does
  (`approveRerun`), since the model can rewrite it; for that reason `/workflows save`
  saves the source the run held (`flowRun.src`), and only for a run this process holds.
- **Agent runs** (tasks.go): every `runSubagent` (tool, background, workflow, Lua) registers an
  `agentRun` (a workflow's carry `workflow` and `phase`, and /agents lists them as one row per
  workflow) whose hooks build a live transcript, which `/agents view <id>` draws instead of
  the session's, and a submit while it runs goes to it through `MessageAgent`, with up-arrow on
  an empty composer taking that run's waiting queue back (`AgentTakeQueue`, the session's
  untouched). Its thinking buffers as the session's does and settles as one Thought row where
  the call or the reply that ends it lands (`flush`). A fork is an
  `agentRun` too (`profile` `fork`), and its `forkBase` is where its own messages start in the
  history: the conversation it opened with is not its report, and `send_to` refuses a fork this
  process forgot, whose rows hold its own work alone. `send_to` with `stop` kills a running run
  (the model's one stop, as `/agents stop` is the user's; a stopped run reports nothing), `wait`
  beside it blocks for its end instead, and the two refuse each other. A run's queue is
  its `Runner.Steer`; what arrives after its last step boundary becomes one more `Send`
  (`followUp`), which closes the queue in the same lock when there is nothing left, so a
  message is never accepted and then dropped. A foreground `agent` or `workflow` call waits for
  its run off the turn's goroutine (a workflow run is `startFlow` and its `flowRun.done`); the
  user's own typed line (`Host.AgentSendUser`, from the composer's `sendMessage` in palette.go or
  a command-line prompt, and never a report, a plugin send or a loop) hands the
  run off: the call answers with the background text and the run's end delivers its report as a
  message. `flowRun.detach`/`agentRun.detach` take the handoff under the run's own lock and
  answer false once it ended, so the result is the caller's and is never both or neither. The
  signal (`Host.UserSpoke`) is raised for a message that joined a *running* turn alone - the
  composer idle starts one, and its own calls wait - and it is re-armed where the queue is read
  at a step boundary or taken back for editing, so a later call in the turn waits as usual;
  headless (no UI hooks) raises none. A background run past
  `agent.max_background`
  (4 by default, workflows counted with agents, a handed-off run counted too) is refused
  (`taskReg.detached`). What the chat
  draws of a queue is one row per message (`queuedRows`): the user's own words as typed, and a
  subagent's report, update or plugin send as a notice row naming it, since the whole of that
  message is the model's. Background runs report through
  `AgentSend` behind `agent.AttachMarker`, which the live transcript and resume cut off; the
  notice row is drawn from the message wherever the model reads it (`appendUserRows`: a new
  turn, a step boundary, a drained queue, a resume), so it sits under the rows of the turn it
  arrived in. Consecutive notice rows are one block in the chat (chat.lua `B.notices`), no
  blank row between them and the older ones of a long burst behind one click-to-open row, so
  a fleet of runs reporting at once cannot take the transcript over. A report a
  closing session drops (`/new`, `/resume`) is stored in its own session and toasted
  (`notifyReport`). A run's notice and its `/agents` row are named by the run's label
  (`agentRun.label`: the model's description, or the definition's name when it wrote none),
  the row's desc keeping the definition. `/agents` lists the live session's runs and,
  under Running and Workflows, the live runs and
  workflows of every other session (`agentRunItems`, the desc ending in that session's title,
  `fromSession`): they block `/rewind`, so their stop must be reachable. Every run, agent and job
  row there and in `/workflows` leads with the chat's mark for its status word (`RunMarks`,
  `tools.Job.Mark`, into `hostapi.Marks`, the one table `zemi.ui.marks` hands chat.lua), as
  does the status field of its detail and of a run's view (`Overlay.Mark`), and a run's view
  heads each phase with its rows' combined mark (`Overlay.Phases`); an agent its run never
  started reads cancelled once the run ended.
- **Scratchpad** (scratch.go): `<state>/scratch/<session id>`, 8 characters (`store.ShortID`),
  so a unix socket path under it fits macOS's 104 bytes. A pipeline binds it once (`Pipeline.Pad`):
  `PipelineFor` the live session's, a subagent run its parent session's, so a run
  that outlives `/new` or `/resume` keeps the pad its prompt names, and a host tool's cut
  output (`truncOutput`: MCP, Lua and agent results, a `tool.post` rewrite) spills there too. It is `TMPDIR`, `TMP` and
  `TEMP` in every command the model starts (`Pipeline.cmdEnv`; the user's
  `!command` keeps the user's). Writes in the pad settle by permission mode like any write,
  and the read-only agent may write only there; `zemi.fs` and the Lua tool jail it to
  the pad alone.
  `write`
  and `edit` need a prior read like anywhere else, and a pad write is never
  checkpointed. A file tool path starting with `$`, `~` or
  `%VAR%` is refused naming the pad. A subagent run that can write is told in its task, never its
  system prompt (which runs of one agent share in the provider cache), to keep its files in
  `<pad>/<agent>-<run id>`. `ensureScratch` (building the environment prompt) creates the live
  pad and hands `sweepScratch` to `GoSafe`, off the send path, which removes pads with nothing
  in them changed for a week, never the live one (NOTE: another window's pad unused that long
  goes too).
- **Lua tool** (luatool.go, tool name `lua`; DESIGN §9): model Lua runs through `Runtime.Eval`
  as `user` origin, default-deny: without approval it may call only `luaReadOnly` members,
  which the sandbox guard (`Sandbox.SetGuard` -> `guardLua`) enforces on every Go function in
  a namespace, and a script `luaUnlisted` finds reaching past them asks once, as one action
  with the whole script the preview. In an untrusted project or a read-only agent every script
  asks. A new API member is guarded until added to
  `luaReadOnly`; `TestLuaToolDefaultDeny` checks every member. Not a new origin: the gate is a
  flag on the chunk name (`lua-tool#<n>` for an approved run). Runs on the interpreter as a
  task (10s ceiling per slice), main agent only. Once approved, a changed config reloads
  before the script (`ReloadIfChanged`), whose note leads the result: the model never
  schedules a reload, since no caller can outlive the generation its reload retires.
- **Model memory writes** (DESIGN §10.4): the one `memory` tool goes through `memoryTool` ->
  `gateMemory`, as the reflector's entries do: `learn.Similar` refuses a near-duplicate, a
  learned user-scope memory's `quote` is checked against the typed part of the session's user
  rows (`quoteInSession`, cut at `agent.AttachMarker`), and an apply that would push its
  scope past `brain.ScopeBudget` cuts its oldest learned lines to the ledger, a cut that
  would take a user line queueing instead. A forget (`forgetMemory`) of a memory the user saved asks (`memory.write`),
  and a learned one goes at once only when `memoryAuto` holds; `subagentHooks`
  and `markNotLocal` record what makes a session not local-only, once per session and in a
  subagent run's own. A plugin's send reaches the model behind
  `agent.Attached("plugin:<name>")`, so no quote comes from it.
- **MCP** (mcp.go, internal/mcp): a dependency-free JSON-RPC client (stdio, streamable HTTP
  with JSON or SSE replies, OAuth discovery where a server needs it; `client_id` and
  `callback_port` for one without dynamic registration). Servers come from `zemi.mcp.add` (user
  origin only) or from a Claude-layout plugin's `.mcp.json` and manifest, registered by the
  plugins step under the plugin's name as a prefix; `PurgeGeneration` parks them, the replay
  keeps a live one (or one waiting for a
  sign-in) it adds with the same spec and the rest stop. They run in `DataHome` with `ZEMI_PROJECT_DIR` set,
  trusted project or not: `npx` and `uv` read config from the working directory, and most
  servers need the network. Tools are
  `mcp_<server>_<tool>`, gated as subject `mcp.<server>` with the tool name as argument. Past
  `mcpDeferAt` (10) MCP tools in a set, `deferMCP` adds `tool_search` and `Runner.ActiveTools`
  offers each step only the other tools, `tool_search` and the loaded ones: a search loads at
  most five into the session's set (`sessionLogs`, dropped with the session, reseeded on resume
  from the history's `mcp_` calls; a subagent run has its own). Every MCP tool stays
  registered, so a call to one not loaded still runs; the catalog changes only with the
  servers, and each load costs one prompt-cache miss. A start's outcome and a sign-in
  request go through `setMCP`, which refreshes an open `/mcp`. `ServiceStatus` (host.go) is the
  one status word of `/mcp`, `/lsp`, `/plugins`, `/health` and `zemi.mcp.list`/`zemi.lsp.list`,
  colored by the UI's `statusHL`, a map the exhaustive linter keeps complete. Test against
  `internal/mcp/mcptest`.
- **LSP** (lsp.go, internal/lsp): servers start lazily on the first matching file, and only in
  a trusted project: they run repo code (rust-analyzer build scripts), so `lspDefs` returns
  nothing untrusted and `noLSP` says why. A `.lua` file of the user's config or a local plugin
  gets lua-language-server (`zemiLua`, DESIGN §9) in any project; it is ready only once a
  symbol request answers, since it
  never checks a file opened while it loads. A file outside the repository goes to no other
  server. rust-analyzer gets no save while its `experimental/serverStatus` says it is not
  quiescent: it exits on the save of a file opened during its workspace scan, and checks the
  whole workspace once quiescent.
  A reload stops only the servers whose definition it changed or dropped or whose start failed (`lspStopChanged`), like
  the MCP replay, so a write to config or a plugin leaves them warm. The `diagnostics` and `lsp` tools leave the tool set when `lspDefs` is empty,
  so an untrusted project does not carry their schemas every step. A server re-reads a file
  only when told, so `lspSync` diffs two `git status` listings and sends
  `workspace/didChangeWatchedFiles` for what changed; it runs before a touch or query only once
  something other than zemi's own file tools ran (`lspDirty`). A slow or failed server means no
  diagnostics, never a failed edit. Test against `internal/lsp/lsptest`.
- **Never start a turn on the interpreter goroutine.** `systemWith` runs `agent.assemble`
  handlers through `RT.Do`, so `AgentSend` from Lua would deadlock; `zemi.agent.send` goes
  through `GoSafe`.
