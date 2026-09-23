-- The statusline widgets: statusline.left holds the mode, the project and
-- what needs attention, statusline.right the model and context use.
local fmt = require("zemi.defaultui.text")

--- Runs the command a statusline segment names in its click id.
---@param ev zemi.WidgetClick
local function run_click(ev)
  assert(zemi.command.run(ev.id))
end

-- Statusline left starts with the permission mode as a pill, colored like
-- the composer prompt. A click opens the mode picker.
zemi.ui.widget.add({
  id = "statusline.mode",
  region = "statusline.left",
  priority = 5,
  on_click = run_click,
  ---@param ctx zemi.WidgetCtx
  ---@return zemi.Line[]
  render = function(ctx)
    local mode = ctx.permission_mode or "auto"
    local base = ctx.permission_mode_base or mode
    local hl = ({ auto = "ModeAuto", yolo = "ModeYolo" })[base] or "ModeAuto"
    return { { { text = mode, hl = hl, click = "mode" } } }
  end,
})

-- A first idle ctrl+c arms quit; say what the second one does. Last in the
-- rule, so nothing before it moves when it comes and goes.
zemi.ui.widget.add({
  id = "statusline.quit",
  region = "statusline.left",
  priority = 110,
  ---@param ctx zemi.WidgetCtx
  ---@return zemi.Line[]
  render = function(ctx)
    if not ctx.quit_armed then return {} end
    return { { { text = "ctrl+c again to quit", hl = "Title" } } }
  end,
})

-- The keys of a bound sequence typed so far, waiting for the rest.
zemi.ui.widget.add({
  id = "statusline.keys",
  region = "statusline.left",
  priority = 95,
  ---@param ctx zemi.WidgetCtx
  ---@return zemi.Line[]
  render = function(ctx)
    if (ctx.key_seq or "") == "" then return {} end
    return { { { text = ctx.key_seq .. " …", hl = "Title" } } }
  end,
})

-- While the chat is scrolled up, say how many rows are below and which key
-- goes back: the bar shows only where the reader is, not what a running
-- turn added under them or how a keyboard gets back.
zemi.ui.widget.add({
  id = "statusline.scroll",
  region = "statusline.left",
  priority = 100,
  ---@param ctx zemi.WidgetCtx
  ---@return zemi.Line[]
  render = function(ctx)
    if (ctx.rows_below or 0) == 0 then return {} end
    return { { { text = "↓ " .. ctx.rows_below .. " · ctrl+end", hl = "Title" } } }
  end,
})
-- Then the project: its path when short, else its name (the splash has
-- the whole path), and whether it is untrusted.
zemi.ui.widget.add({
  id = "statusline.cwd",
  region = "statusline.left",
  priority = 10,
  ---@param ctx zemi.WidgetCtx
  ---@return zemi.Line[]
  render = function(ctx)
    local p = ctx.project or ctx.cwd or ""
    if fmt.cells(p) > math.floor((ctx.width or 80) / 4) then p = p:match("([^/]+)/*$") or p end
    if ctx.trusted == false then p = p == "" and "untrusted" or p .. " · untrusted" end
    return { { { text = p, hl = "Comment" } } }
  end,
})

-- Reflector feedback (§10.3): a blinking badge while a run is active
-- (ctx.frame is the shared tick), a toast with what it learned when it ends
-- (a failure goes to /errors).
zemi.ui.widget.add({
  id = "statusline.reflect",
  region = "statusline.left",
  priority = 20,
  ---@param ctx zemi.WidgetCtx
  ---@return zemi.Line[]
  render = function(ctx)
    if not ctx.reflecting then return {} end
    return { { { text = ctx.frame % 20 >= 10 and "✧ reflecting" or "✻ reflecting", hl = "AgentReason" } } }
  end,
})

-- Subagents at work: the turn's own, background ones, workflows and Lua.
zemi.ui.widget.add({
  id = "statusline.agents",
  region = "statusline.left",
  priority = 25,
  on_click = run_click,
  ---@param ctx zemi.WidgetCtx
  ---@return zemi.Line[]
  render = function(ctx)
    local agents, jobs = ctx.agents_running or 0, ctx.jobs_running or 0
    if agents == 0 and jobs == 0 then return {} end
    ---@type string[]
    local parts = {}
    if agents > 0 then parts[#parts + 1] = agents .. (agents == 1 and " agent" or " agents") end
    if jobs > 0 then parts[#parts + 1] = jobs .. (jobs == 1 and " job" or " jobs") end
    return { { { text = "⧗ " .. table.concat(parts, ", "), hl = "Title", click = "agents" }, { text = " /agents", hl = "Comment", click = "agents" } } }
  end,
})

-- While the chat shows an agent run's transcript (/agents, or a click on
-- its row), say whose it is, where a message goes and how to get back.
zemi.ui.widget.add({
  id = "statusline.view",
  region = "statusline.left",
  priority = 1,
  ---@param ctx zemi.WidgetCtx
  ---@return zemi.Line[]
  render = function(ctx)
    local v = ctx.view
    if not v then return {} end
    local send = v.status == "running" and " · enter messages it" or ""
    local tail = " · " .. v.status .. send .. " · esc to go back"
    return { {
      { text = "viewing " .. v.agent, hl = "Title" },
      { text = ": " .. fmt.clip(v.description, math.max(10, math.floor((ctx.width or 80) / 3))) .. tail, hl = "Comment" },
    } }
  end,
})

-- Learned proposals waiting for review; they persist across restarts.
zemi.ui.widget.add({
  id = "statusline.learn",
  region = "statusline.left",
  priority = 30,
  on_click = run_click,
  ---@param ctx zemi.WidgetCtx
  ---@return zemi.Line[]
  render = function(ctx)
    local n = ctx.pending_memory or 0
    if n == 0 then return {} end
    return { {
      { text = "✻ " .. n .. " to review", hl = "Title", click = "memory" },
      { text = " /memory", hl = "Comment", click = "memory" },
    } }
  end,
})

zemi.event.on("reflector.done", function(ev)
  if ev.error then return end
  ---@type string[]
  local parts = {}
  ---@param n integer?
  ---@param one string
  ---@param many string
  local function add(n, one, many)
    if (n or 0) > 0 then parts[#parts + 1] = n .. " " .. (n == 1 and one or many) end
  end
  add(ev.stored, "memory stored", "memories stored")
  add(ev.queued, "memory to review in /memory", "memories to review in /memory")
  add(ev.superseded, "memory superseded", "memories superseded")
  -- a session with nothing worth keeping ends quietly
  if #parts == 0 then return end
  zemi.notify({ title = "Learned from the last session", body = table.concat(parts, ", ") })
end)

-- Statusline right: "model · 24.5k/200k ctx · 45%", falling back to the
-- session token total when the model's context window is unknown. The
-- percent colors by threshold: muted, warn at 80%, err at 95%. A click on
-- the model opens the model picker, on the rest /usage. One widget per
-- part, so config can replace or delete any of them.
---@type { [1]: string, [2]: fun(ctx: zemi.WidgetCtx): string?, string?, string? }[]
local right = {
  { "profile", function(ctx)
    if ctx.profile_read_only then
      return ctx.profile, "ModePlan", "profile"
    end
    return ctx.profile, ctx.profile ~= "default" and "Comment" or "Dim", "profile"
  end },
  { "model", function(ctx)
    if (ctx.model or "") == "" then return end
    return ctx.model .. ((ctx.effort or "") ~= "" and (" " .. ctx.effort) or ""), "Comment", "model"
  end },
  -- how much of the window is in use, not the running session total: the
  -- total says nothing about how much room is left.
  { "context", function(ctx)
    local cx = ctx.context or {}
    local used, window = fmt.tokens(cx.tokens), fmt.tokens(cx.window)
    local total = fmt.tokens((ctx.usage or {}).total)
    if used and window then return used .. "/" .. window .. " ctx" end
    return total and total .. " tok"
  end },
  { "percent", function(ctx)
    local pct = tonumber((ctx.context or {}).pct or 0) or 0
    if pct < 1 then return end
    return string.format("%d%%", pct), pct >= 95 and "StatusUsageErr" or pct >= 80 and "StatusUsageWarn" or "StatusUsage"
  end },
  { "cost", function(ctx)
    local cost = tonumber((ctx.usage or {}).cost or 0) or 0
    if cost > 0 then return string.format("$%.2f", cost) end
  end },
}
for i, part in ipairs(right) do
  zemi.ui.widget.add({
    id = "statusline." .. part[1],
    region = "statusline.right",
    priority = 9 + i,
    on_click = run_click,
    ---@param ctx zemi.WidgetCtx
    ---@return zemi.Line[]
    render = function(ctx)
      local text, hl, click = part[2](ctx)
      if not text then return {} end
      return { { { text = text, hl = hl or "Comment", click = click or "usage" } } }
    end,
  })
end
