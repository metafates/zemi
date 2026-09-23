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
-- instant turns do not flash it, and the eyes say how the turn ended for a
-- few seconds after.
zemi.ui.widget.add({
  id = "working",
  region = "working",
  priority = 10,
  ---@param ctx zemi.WidgetCtx
  ---@return zemi.Line[]
  render = function(ctx)
    local ag = ctx.agent or {}
    -- each part with its worth: a narrow rule sheds the least worth first;
    -- the elapsed time also has the cells it grows to in ten minutes, so the
    -- label is clipped once and not again each time it gains a digit
    ---@type { [1]: string, [2]: integer, [3]: integer? }[]
    local parts = {}
    local label, state = "", "idle"
    if not ctx.agent_active then
      if ctx.reflecting then
        label, state = "Reflecting", "reading"
      elseif ctx.ended == "ok" then
        state = "done"
      elseif ctx.ended == "error" then
        state = "error"
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
      parts = { { fmt.duration(ag.elapsed, true), 5, 6 } }
      if ag.compacting then label, state = "Compacting", "reading" end
      if ag.retry then
        label, state = "Retrying", "error"
        parts = { { "attempt " .. ag.retry.attempt, 3 } }
        if ag.retry.why ~= "" then parts[#parts + 1] = { ag.retry.why, 2 } end
        local next_in = fmt.duration(math.ceil(ag.retry.next_in), true)
        if retry_attempt ~= ag.retry.attempt then retry_attempt, retry_w = ag.retry.attempt, #next_in end
        local countdown = "next try in " .. string.rep(" ", retry_w - #next_in) .. next_in
        if ag.retry.next_in <= 0 then countdown = "retrying now" .. string.rep(" ", #countdown - 12) end
        parts[#parts + 1] = { countdown, 5 }
      else
        retry_attempt = nil
      end
      if ctx.cancelling then label = "Cancelling" end
      if ctx.editing ~= "" then
        label, state = "Editing " .. ctx.editing, "idle"
        parts = { { "save and close the editor to continue", 5 } }
      end
      -- the open prompt says what answers it, so the rule shrinks to the
      -- mascot alone rather than repeating it; in an agent view esc goes back
      if ctx.waiting ~= "" then
        label, state, parts = "", "asking", {}
      elseif ctx.cancelling then
        parts[#parts + 1] = { "ctrl+c quit", 4 }
      elseif not ctx.view then
        parts[#parts + 1] = { ctx.interrupt_armed and "esc again" or "esc interrupt", 4 }
      end
      local queued = #ag.steering - ag.reports
      if queued > 0 then parts[#parts + 1] = { queued .. " queued · up edit", 1 } end
    end
    ---@return integer
    local function room()
      local n = 0
      for _, p in ipairs(parts) do n = n + 3 + math.max(fmt.cells(p[1]), p[3] or 0) end
      return n
    end
    -- the snail keeps 8 cells of rule; the label gives way down to 16 cells
    -- before a part goes, then clips into what the parts leave
    local keep = math.min(fmt.cells(label), 16)
    while #parts > 0 and keep + room() + 10 > ctx.width do
      local least = 1
      for i, p in ipairs(parts) do
        if p[2] < parts[least][2] then least = i end
      end
      table.remove(parts, least)
    end
    label = fmt.clip(label, ctx.width - 10 - room())
    ---@type string[]
    local texts = {}
    for i, p in ipairs(parts) do texts[i] = p[1] end
    local after = #texts > 0 and " · " .. table.concat(texts, " · ") or ""
    local text = label .. after ~= "" and fmt.cells(label .. after) + 2 or 0
    local row = snail.rule(state, ctx.frame, ctx.width - text)
    if text > 0 then
      table.insert(row, 3, { text = " " .. label, hl = "Comment", effect = "shimmer" })
      table.insert(row, 4, { text = after .. " ", hl = "Comment" })
    end
    return { row }
  end,
})
