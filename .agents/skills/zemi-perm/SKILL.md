---
name: zemi-perm
description: "Load before changing the tool pipeline or its gate in internal/tools, the ask and the mode in internal/hostapi, or internal/proc, or before judging a permission ask: the two modes, the gate, the veto, the process layer."
---

# Permissions and the process layer

## The two modes

The permission system is UX, not isolation: it exists to keep the user aware of what the agent
does. A run that must not touch the machine goes in a container or a VM.

`permissions.mode` is `ask` or `allow`, `allow` the default:

- `allow`: everything runs without asking.
- `ask`: every action prompts - any command, any file write or edit, any fetch or search, any
  MCP tool its server does not mark read-only, any Lua tool. Reads never prompt.

ctrl+y toggles the two, `/mode` picks one, `--mode` and `zemi.permission.mode.set` set one (the
namespace is user origin only). `zemi.permission` holds only `mode.get`, `mode.set` and
`mode.list`: no rules, no principals, no custom modes.

## Tool pipeline (internal/tools/tools.go)

Every model tool call runs: JSON unmarshal -> schema validation -> `tool.pre` Lua events (deny
/ halt / rewrite) -> permission gate -> run -> `tool.post` handlers, then an override's `post`.
A rewritten input re-enters at the permission gate; errors become tool-error responses, not
panics; a denied result names who decided and why. Host-implemented tools (`agent`,
`workflow`, `lua`, MCP and Lua tools) run the events through `preCheck` and call the gate
themselves.

The gate (`Pipeline.Check`), in order: a read-only agent (`Profile.ReadOnly`, the planner
profile) denies every action - writes, commands, fetches, Lua tools - while its reads pass;
then the mode settles, `allow` runs and `ask` puts the request to the user. An
ask nobody can answer (no overlay: headless, or a call from Lua) denies fail-closed and is
recorded (`Host.HeadlessDenials`, which `zemi run` prints and exits on). Asks reach the user
one at a time under `Host.askMu`, and a waiter is judged again once it holds the lock
(`Request.Recheck`), so a switch to allow settles the asks behind it.

Subjects label the asks: `tool.shell`, `tool.lua`, `tool.workflow`, `tool.<name>` of a Lua
tool, `fs.read`, `fs.write`, `net.<host>` (the model's `fetch` and `web_search`), `mcp.<server>`,
`memory.write`, and `lua.<ns.call>` for a host-API call config code makes (`lua.fs.write`,
`lua.job.start`, `lua.http.<method>`, ...). The overlay offers three answers: y (yes, once),
n (no), t (no, and tell zemi - denies the waiting asks and stops the turn).

The `permission.request` event vetoes before the overlay. A handler has 50 ms (`lua.VetoDeadline`)
and then the logged default applies, deny for an ask; while the runtime is hot it denies too.
`permission.resolved` reports the outcome either way.

The Lua tool's script asks once, as one action, the whole script the preview; once approved, a
`zemi.job.start` it makes rides that approval, and its `zemi.tool.call` gates each call like the
model's own, the ask named "lua script" as who asked. Any other host-API call from Lua never
opens the overlay: in ask mode it raises "permission ask required", and the fix is allow mode. A
plugin's declared capabilities settle its own calls in both modes; a call its manifest does not
declare is refused before any ask, naming the `uses` line to add.

## Processes (internal/proc)

Every child process starts through `internal/proc` - the shell tool, jobs, MCP and LSP servers,
git. `proc.Command` and `CommandContext` put the child in its own session and its own process
group, and cancellation, a timeout and `proc.KillAll` on exit kill the group whole; `WaitDelay`
bounds a hung wait. A plain `exec.Command` here is a bug (a cancelled pipeline once hung the
turn forever). The exceptions: the user's editor, which needs the controlling terminal a new
session drops, and main's `relaunch`, the exec that replaces zemi with `zemi -C <dir>` once the
TUI restored the terminal and the children `proc.KillAll` killed were reaped. `proc.Refuse`
makes every start fail while it holds (`zemi plugins test`), and `proc.Shell` resolves the
shell once, on PATH.
