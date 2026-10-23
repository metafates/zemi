---
name: zemi-perm
description: "Load before changing the tool pipeline or its gate in internal/tools, the ask and the veto in internal/hostapi, or internal/proc, or before judging a permission ask: the gate, the veto, the ask overlay, the process layer."
---

# Permissions and the process layer

## The model

The permission system is UX, not isolation: it exists to keep the user aware of what the agent
does. A run that must not touch the machine goes in a container or a VM. There are no modes and
no rules: everything runs, and the two interception points a plugin may ship (with `perm` in its
manifest's `uses`) sit in the gate:

- the vetoable `permission.request` event, once per action request: `{ decision = "deny", reason }`
  refuses, `{ decision = "ask", reason }` runs the ask path, nil runs;
- `zemi.perm.ask({subject=, arg=, reason=, preview=})`, a plugin's own ask from a task, whose
  handle's `:wait()` returns `{ answer = "allow" | "deny" | "told" }`.

Both need the manifest's `perm` word: steering the gate is a declared capability, and enabling
the plugin is the consent.

## Tool pipeline (internal/tools/tools.go)

Every model tool call runs: JSON unmarshal -> schema validation -> `tool.pre` Lua events (deny
/ halt / rewrite) -> permission gate -> run -> `tool.post` handlers, then an override's `post`.
A rewritten input re-enters at the permission gate; errors become tool-error responses, not
panics; a denied result names who decided and why. Host-implemented tools (`agent`,
`workflow`, `lua`, MCP and Lua tools) run the events through `preCheck` and call the gate
themselves.

The gate (`Pipeline.Check`), in order: a read-only agent (`Profile.ReadOnly`) denies every action - writes, commands, Lua tools - while its reads and
its pad writes pass; then the veto (`Pipeline.Veto`, only when an asker exists - a gate
without one cannot survive the veto's interpreter round-trip); then the ask path for an ask
decision, and everything else runs.

An ask nobody can answer (no UI) denies fail-closed (`tools.ByNobody`). Asks reach the user one at a
time under `Host.askMu`; no Lua runs under the lock.

Subjects label the asks: `tool.shell`, `tool.lua`, `tool.workflow`, `tool.<name>` of a Lua
tool, `fs.read`, `fs.write`, `mcp.<server>`,
`memory.write`, and `lua.<ns.call>` for a host-API call config code makes (`lua.fs.write`,
`lua.job.start`, `lua.http.<method>`, ...). The overlay offers three answers: y (yes, once),
n (no), t (no, and tell zemi - denies the waiting asks and stops the turn).

The veto handler has 50 ms (`lua.VetoDeadline`) and then the logged default applies - no
veto, so the request runs; while the runtime is hot it denies. `permission.resolved` reports
the outcome either way.

The Lua tool's script asks once, as one action, the whole script the preview; once approved, a
`zemi.job.start` it makes rides that approval, and its `zemi.tool.call` gates each call like the
model's own, the ask named "lua script" as who asked. Any other host-API call from Lua never
opens the overlay; the gate settles it: a read-only agent's writes deny, everything else runs.
A plugin's declared capability is checked before the gate - the manifest is the consent - and a
call it does not declare is refused there, naming the `uses` line to add.

## Processes (internal/proc)

Every child process starts through `internal/proc` - the shell tool, jobs, MCP and LSP servers,
git. `proc.Command` and `CommandContext` put the child in its own session and its own process
group, and cancellation, a timeout and `proc.KillAll` on exit kill the group whole; `WaitDelay`
bounds a hung wait. A plain `exec.Command` here is a bug (a cancelled pipeline once hung the
turn forever). The exceptions: the user's editor, which needs the controlling terminal a new
session drops, and `relaunch` (internal/cli/boot.go), the exec that replaces zemi with `zemi -C <dir>` once the
TUI restored the terminal and the children `proc.KillAll` killed were reaped. `proc.Refuse`
makes every start fail while it holds (`zemi plugin test`), and `proc.Shell` resolves the
shell once, on PATH.
