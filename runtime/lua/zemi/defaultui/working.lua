local fmt = require("zemi.defaultui.text")
local snail = require("zemi.defaultui.snail")

---@type table<string, boolean>
local READS = { read = true, grep = true, glob = true, fetch = true, web_search = true, skill = true }
-- a retry countdown keeps the width it started at, so what follows it
-- never moves left as it counts down
---@type integer?, integer
local retry_attempt, retry_w = nil, 0

-- The composer frame's top rule, all ctx.width cells of it: the mascot in the
-- left corner in the state of the turn and, right after it, a shimmering
-- label naming what the agent does, the elapsed time, the interrupt key and
-- the queue depth. Left-anchored, so ticking and changing text grows into the
-- rule and never moves the label. The label stays away for the first 300ms so
-- instant turns do not flash it, and the done mark stays a few seconds after.
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
        label, state = "Reflecting", "reading"
      elseif ctx.done then
        state = "done"
      end
    elseif (ag.elapsed or 0) >= 0.3 or ctx.cancelling then
      label, state = "Working", "working"
      -- the first: serial calls run in call order, and a parallel batch
      -- starts together
      for _, t in ipairs(ag.tools or {}) do
        if t.status == "run" then
          local verb, rest = fmt.head(t.name, t.label)
          -- an unlabelled call reads as its row does: the verb and its input
          if t.label == "" and t.input ~= "" then rest = " " .. fmt.relpath(t.input, ctx.cwd):gsub("%s+", " ") end
          label = verb .. rest
          if READS[t.name] then state = "reading" end
          break
        end
      end
      if label == "Working" and ag.reasoning ~= "" and ag.text == "" then label, state = "Thinking", "thinking" end
      parts = { fmt.duration(ag.elapsed, true) }
      if ag.retry then
        label, state = "Retrying", "error"
        parts = { "attempt " .. ag.retry.attempt }
        if ag.retry.why ~= "" then parts[#parts + 1] = ag.retry.why end
        local next_in = fmt.duration(math.ceil(ag.retry.next_in), true)
        if retry_attempt ~= ag.retry.attempt then retry_attempt, retry_w = ag.retry.attempt, #next_in end
        parts[#parts + 1] = "next try in " .. string.rep(" ", retry_w - #next_in) .. next_in
      else
        retry_attempt = nil
      end
      if ag.compacting then label, state = "Compacting", "reading" end
      if ctx.cancelling then label = "Cancelling" end
      if ctx.editing ~= "" then
        label, state = "Editing " .. ctx.editing, "idle"
        parts = { "save and close the editor to continue" }
      end
      -- the open prompt says what answers it, so the rule shrinks to the
      -- mascot alone rather than repeating it; in an agent view esc goes back
      if ctx.waiting ~= "" then
        label, state, parts = "", "asking", {}
      elseif ctx.cancelling then
        parts[#parts + 1] = "ctrl+c quit"
      elseif not ctx.view then
        parts[#parts + 1] = ctx.interrupt_armed and "esc again" or "esc interrupt"
      end
      local queued = #ag.steering - ag.reports
      if queued > 0 then parts[#parts + 1] = queued .. " queued · up edit" end
    end
    -- a narrow rule clips the label to 16 cells first, then drops the queue
    -- depth first and the elapsed time last, keeping the snail 8 cells of rule
    local after = #parts > 0 and zemi.ui.width(" · " .. table.concat(parts, " · ")) or 0
    label = fmt.clip(label, math.max(ctx.width - 10 - after, 16))
    while #parts > 0 and zemi.ui.width(label .. " · " .. table.concat(parts, " · ")) + 10 > ctx.width do
      parts[#parts] = nil
    end
    local tail = #parts > 0 and " · " .. table.concat(parts, " · ") or ""
    local text = label .. tail ~= "" and zemi.ui.width(label .. tail) + 2 or 0
    local row = snail.rule(state, ctx.frame, ctx.width - text)
    if text > 0 then
      table.insert(row, 3, { text = " " .. label, hl = "Comment", effect = "shimmer" })
      table.insert(row, 4, { text = tail .. " ", hl = "Comment" })
    end
    return { row }
  end,
})
