local fmt = require("zemi.defaultui.text")

-- The working row above the composer while a turn runs: a pulsing bullet,
-- a shimmering label naming what the agent does, the elapsed time, the
-- interrupt key and the queue depth. It stays hidden for the first 300ms so
-- instant turns do not flash it.
zemi.ui.widget.add({
  id = "working",
  region = "working",
  priority = 10,
  ---@param ctx zemi.WidgetCtx
  ---@return zemi.Line[]
  render = function(ctx)
    local ag = ctx.agent or {}
    if not ctx.agent_active or ((ag.elapsed or 0) < 0.3 and not ctx.cancelling) then return {} end
    local label = "Working"
    for _, t in ipairs(ag.tools or {}) do
      if t.status == "run" then label = t.label ~= "" and t.label or ("Running " .. t.name) end
    end
    if label == "Working" and ag.reasoning ~= "" and ag.text == "" then label = "Thinking" end
    local parts = { fmt.elapsed(ag.elapsed) }
    if ag.retry then
      label = "Retrying"
      parts = { "attempt " .. ag.retry.attempt }
      if ag.retry.why ~= "" then parts[#parts + 1] = ag.retry.why end
      parts[#parts + 1] = "next try in " .. fmt.elapsed(ag.retry.next_in)
    end
    if ag.compacting then label = "Compacting" end
    if ctx.cancelling then label = "Cancelling" end
    if ctx.editing ~= "" then
      label = "Editing " .. ctx.editing
      parts = { "save and close the editor to continue" }
    end
    -- esc answers an open prompt, so the row does not offer it
    if ctx.waiting ~= "" then
      label = ctx.waiting == "approval" and "Waiting for your approval" or "Waiting for your answer"
    else
      parts[#parts + 1] = ctx.cancelling and "ctrl+c quits" or "esc to interrupt"
    end
    local queued = #ag.steering - ag.reports
    if queued > 0 then parts[#parts + 1] = queued .. " queued, up to edit" end
    local bullet = ctx.pulse and { text = "◦ ", hl = "Comment", copy = false } or { text = "• ", hl = "Title", copy = false }
    return {
      {
        bullet,
        { text = label, hl = "Comment", effect = "shimmer" },
        { text = "  " .. table.concat(parts, " · "), hl = "Comment" },
      },
      { { text = "" } }, -- a blank row between it and the composer
    }
  end,
})
