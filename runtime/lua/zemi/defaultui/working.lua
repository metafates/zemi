local fmt = require("zemi.defaultui.text")
local snail = require("zemi.defaultui.snail")

-- The working row above the composer while a turn runs: the mascot in the
-- state of the turn, a shimmering label naming what the agent does, the
-- elapsed time, the interrupt key and the queue depth. It stays hidden for
-- the first 300ms so instant turns do not flash it, and the mascot stays a
-- few seconds after the turn to say it finished.
zemi.ui.widget.add({
  id = "working",
  region = "working",
  priority = 10,
  ---@param ctx zemi.WidgetCtx
  ---@return zemi.Line[]
  render = function(ctx)
    local ag = ctx.agent or {}
    if not ctx.agent_active then
      if not ctx.done then return {} end
      return { snail.row("done", ctx.frame), { { text = "" } } }
    end
    if (ag.elapsed or 0) < 0.3 and not ctx.cancelling then return {} end
    local label, state = "Working", "working"
    for _, t in ipairs(ag.tools or {}) do
      if t.status == "run" then label = t.label ~= "" and t.label or ("Running " .. t.name) end
    end
    if label == "Working" and ag.reasoning ~= "" and ag.text == "" then label, state = "Thinking", "thinking" end
    local parts = { fmt.elapsed(ag.elapsed) }
    if ag.retry then
      label, state = "Retrying", "error"
      parts = { "attempt " .. ag.retry.attempt }
      if ag.retry.why ~= "" then parts[#parts + 1] = ag.retry.why end
      parts[#parts + 1] = "next try in " .. fmt.elapsed(ag.retry.next_in)
    end
    if ag.compacting then label = "Compacting" end
    if ctx.cancelling then label = "Cancelling" end
    if ctx.editing ~= "" then
      label, state = "Editing " .. ctx.editing, "retracted"
      parts = { "save and close the editor to continue" }
    end
    -- esc answers an open prompt, so the row does not offer it
    if ctx.waiting ~= "" then
      label, state = ctx.waiting == "approval" and "Waiting for your approval" or "Waiting for your answer", "retracted"
    else
      parts[#parts + 1] = ctx.cancelling and "ctrl+c quits" or "esc to interrupt"
    end
    local queued = #ag.steering - ag.reports
    if queued > 0 then parts[#parts + 1] = queued .. " queued, up to edit" end
    local row = snail.row(state, ctx.frame)
    row[#row + 1] = { text = label, hl = "Comment", effect = "shimmer" }
    row[#row + 1] = { text = "  " .. table.concat(parts, " · "), hl = "Comment" }
    return { row, { { text = "" } } } -- a blank row between it and the composer
  end,
})
