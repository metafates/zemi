local fmt = require("zemi.tui.text")
local snail = require("zemi.tui.snail")

---@type table<string, boolean>
local READS = { read = true, grep = true, glob = true, fetch = true, web_search = true, skill = true }

-- The composer frame's top rule, all ctx.width cells of it: the mascot in the
-- left corner in the state of the turn and, centered on the rule, a shimmering
-- label naming what the agent does, the interrupt key, the queue depth and
-- the elapsed time. A part that ticks or flips keeps the cells it can grow
-- to, so the block is centered once and the label never slides. The label
-- stays away for the first 300ms so instant turns do not flash it, and the
-- eyes say how the turn ended for a few seconds after. A busy job shows the
-- same way while idle, and as the first part to go during a turn.

-- the state the eyes last showed and the frame it began on: an eye cycle
-- starts with its state, whatever the clock read
---@type string?, integer
local shown, since = nil, 0

zemi.ui.widget.add({
  id = "working",
  region = "working",
  priority = 10,
  ---@param ctx zemi.WidgetCtx
  ---@return zemi.Line[]
  render = function(ctx)
    local ag = ctx.agent or {}
    -- each part with its worth: a narrow rule sheds the least worth first;
    -- a ticking or flipping part also has the cells it grows to, drawn as
    -- rule after the last part
    ---@type { [1]: string, [2]: integer, [3]: integer? }[]
    local parts = {}
    local label, state = "", "idle"
    local busy = ctx.busy and ctx.busy.elapsed >= 0.3 and ctx.busy or nil
    if not ctx.agent_active then
      if busy then
        label, state = busy.label, "working"
        if busy.cancellable then parts[1] = { ctx.interrupt_armed and "esc again" or "esc interrupt", 6, 13 } end
        parts[#parts + 1] = { fmt.duration(busy.elapsed, true), 5, 7 }
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
      if label == "Working" and ag.reasoning ~= "" and ag.text == "" then
        label, state = "Thinking", "thinking"
      end
      ---@type { [1]: string, [2]: integer, [3]: integer }?
      local tick = { fmt.duration(ag.elapsed, true), 5, 7 }
      if ag.compacting then
        label, state = "Compacting", "reading"
      end
      if ag.retry then
        label, state = "Retrying", "error"
        parts = { { "attempt " .. ag.retry.attempt, 3 } }
        if ag.retry.why ~= "" then parts[#parts + 1] = { ag.retry.why, 2 } end
        tick = {
          ag.retry.next_in > 0 and "next try in " .. fmt.duration(math.ceil(ag.retry.next_in), true) or "retrying now",
          5,
          18,
        }
      end
      if ctx.cancelling then label = "Cancelling" end
      if ctx.editing ~= "" then
        label, state, tick = "Editing " .. ctx.editing, "idle", nil
        parts = { { "save and close the editor to continue", 5 } }
      end
      -- the open prompt says what answers it, so the rule shrinks to the
      -- mascot alone rather than repeating it; in an agent view esc goes back
      if ctx.waiting ~= "" then
        label, state, parts, tick = "", "asking", {}, nil
      elseif ctx.cancelling then
        parts[#parts + 1] = { "ctrl+c quit", 6, 13 }
      elseif not ctx.view then
        parts[#parts + 1] = { ctx.interrupt_armed and "esc again" or "esc interrupt", 6, 13 }
      end
      local queued = #ag.steering - ag.reports
      if queued > 0 then parts[#parts + 1] = { queued .. " queued · up edit", 1 } end
      if busy then parts[#parts + 1] = { busy.label, 0 } end
      -- last, where its changing width moves nothing
      if tick then parts[#parts + 1] = tick end
    end
    ---@return integer
    local function room()
      local n = 0
      for _, p in ipairs(parts) do
        n = n + 3 + math.max(fmt.cells(p[1]), p[3] or 0)
      end
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
    for i, p in ipairs(parts) do
      texts[i] = p[1]
    end
    local after = #texts > 0 and " · " .. table.concat(texts, " · ") or ""
    local text = label .. after ~= "" and fmt.cells(label) + room() + 2 or 0
    if state ~= shown then
      shown, since = state, ctx.frame
    end
    local left = text > 0 and math.floor((ctx.width - text) / 2) or ctx.width
    local row = snail.rule(state, ctx.frame - since, left)
    if text > 0 then
      row[#row + 1] = { text = " " .. label, hl = "Comment", effect = "shimmer" }
      row[#row + 1] = { text = after .. " ", hl = "Comment" }
      row[#row + 1] =
        { text = string.rep("─", ctx.width - left - fmt.cells(label .. after) - 2), hl = "Rule", copy = false }
    end
    return { row }
  end,
})
