local fmt = require("zemi.defaultui.text")
local snail = require("zemi.defaultui.snail")

-- The composer frame's top rule, all ctx.width cells of it: the mascot in the
-- left corner in the state of the turn and, at the right end, a shimmering
-- label naming what the agent does, the elapsed time, the interrupt key and
-- the queue depth. The label stays away for the first 300ms so instant turns
-- do not flash it, and the done mark stays a few seconds after the turn.
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
      if ctx.reflecting then
        label, state = "Reflecting", "rereading"
      elseif ctx.done then
        state = "done"
      end
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
      if ag.compacting then label, state = "Compacting", "rereading" end
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
    local tail = #parts > 0 and "  " .. table.concat(parts, " · ") or ""
    -- a narrow rule keeps the label and the snail at least 8 cells
    if zemi.ui.width(label .. tail) + 10 > ctx.width then tail = "" end
    local text = label .. tail ~= "" and zemi.ui.width(label .. tail) + 2 or 0
    -- one small shell per running subagent, where the rule meets the text
    local kids = math.max(math.min(ctx.agents_running, ctx.width - text - 8), 0)
    local row = snail.rule(state, ctx.frame, ctx.width - text - kids)
    row[#row + 1] = { text = string.rep("ə", kids), hl = "Accent", copy = false }
    if text > 0 then
      row[#row + 1] = { text = " " .. label, hl = "Comment", effect = "shimmer" }
      row[#row + 1] = { text = tail .. " ", hl = "Comment" }
    end
    return { row }
  end,
})
