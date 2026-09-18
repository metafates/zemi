local fmt = require("zemi.defaultui.text")
local snail = require("zemi.defaultui.snail")

-- The composer frame's top rule: the mascot in the state of the turn, a
-- shimmering label naming what the agent does, the elapsed time, the
-- interrupt key and the queue depth. The mascot is always there, so the rule
-- never gains or loses a row; the label stays away for the first 300ms so
-- instant turns do not flash it, and the mascot stays a few seconds after
-- the turn to say it finished.
zemi.ui.widget.add({
  id = "working",
  region = "working",
  priority = 10,
  ---@param ctx zemi.WidgetCtx
  ---@return zemi.Line[]
  render = function(ctx)
    local ag = ctx.agent or {}
    ---@type string[]
    local parts = {}
    local label, state = "", "idle"
    if not ctx.agent_active then
      if ctx.done then state = "done" end
    elseif (ag.elapsed or 0) >= 0.3 or ctx.cancelling then
      label, state = "Working", "working"
      for _, t in ipairs(ag.tools or {}) do
        if t.status == "run" then label = t.label ~= "" and t.label or ("Running " .. t.name) end
      end
      if label == "Working" and ag.reasoning ~= "" and ag.text == "" then label, state = "Thinking", "thinking" end
      parts = { fmt.elapsed(ag.elapsed) }
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
      -- the open prompt says what answers it, so the rule retracts to the
      -- mascot alone rather than repeating it
      if ctx.waiting ~= "" then
        label, state, parts = "", "retracted", {}
      else
        parts[#parts + 1] = ctx.cancelling and "ctrl+c quits" or "esc to interrupt"
      end
      local queued = #ag.steering - ag.reports
      if queued > 0 then parts[#parts + 1] = queued .. " queued, up to edit" end
    end
    local row, w = snail.row(state, ctx.frame)
    if label == "" and #parts == 0 then return { row } end
    -- the lane keeps the text still while the snail crawls under it
    row[#row + 1] = { text = string.rep(" ", math.max(2, snail.WIDTH - w)), copy = false }
    if label ~= "" then row[#row + 1] = { text = label, hl = "Comment", effect = "shimmer" } end
    if #parts > 0 then row[#row + 1] = { text = "  " .. table.concat(parts, " · "), hl = "Comment" } end
    return { row }
  end,
})
