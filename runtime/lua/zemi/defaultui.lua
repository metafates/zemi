-- defaultui: the shipped theme, widgets, and default keymaps (DESIGN §6.6,
-- §8). Runs as the builtin origin via require("zemi.defaultui"); every
-- registration here is replaceable by user config (origin precedence:
-- user > builtin > plugin > project). The working indicator above the
-- composer is Go-drawn (it animates per frame) and styled by Normal,
-- Comment and Title.

zemi.ui.theme.register("default", {
  palette = {
    bg = "#1a1b26",
    fg = "#c0caf5",
    muted = "#565f89",
    accent = "#7aa2f7",
    ok = "#9ece6a",
    warn = "#e0af68",
    err = "#f7768e",
    panel = "#24283b",
  },
  hl = {
    Normal = { fg = "fg" },
    Comment = { fg = "muted" },
    Title = { fg = "accent", bold = true },
    UserMsg = { fg = "accent" },
    ToastError = { fg = "bg", bg = "err", bold = true },
    Toast = { fg = "fg", bg = "panel" },
    ToastTitle = { fg = "fg", bg = "panel", bold = true },
    ToastInfo = { fg = "accent", bg = "panel" },
    ToastWarn = { fg = "warn", bg = "panel" },
    ToastErr = { fg = "err", bg = "panel" },
    MarkdownHeading = { fg = "accent", bold = true },
    MarkdownCode = { fg = "warn", bg = "panel" },
    MarkdownLink = { fg = "accent", underline = true },
    MarkdownQuote = { fg = "muted", italic = true },
    AgentReason = { fg = "muted", italic = true },
    AgentToolName = { fg = "fg", bold = true },
    AgentToolRun = { fg = "muted" },
    AgentToolOk = { fg = "ok" },
    AgentToolErr = { fg = "err" },
    StatusUsage = { fg = "muted" },
    StatusUsageWarn = { fg = "warn" },
    StatusUsageErr = { fg = "err", bold = true },
  },
})
zemi.ui.theme.set("default")

local function fmt_tokens(n)
  if not n or n <= 0 then return nil end
  if n >= 1000000 then return string.format("%.1fM", n / 1000000) end
  if n >= 1000 then return string.format("%.1fk", n / 1000) end
  return string.format("%d", n)
end

local function fmt_secs(s)
  if not s or s <= 0 then return nil end
  if s >= 60 then return string.format("%dm%02ds", s / 60, s % 60) end
  return string.format("%.1fs", s)
end

local function empty() return { { { text = "" } } } end

-- Statusline left: working directory (tail kept when long, so the right
-- cluster always fits), then the reflector badge.
zemi.ui.widget.register({
  id = "statusline.cwd",
  region = "statusline.left",
  priority = 10,
  render = function(ctx)
    local cwd, room = ctx.cwd or "", math.floor((ctx.width or 80) / 3)
    if #cwd > room and room > 1 then
      cwd = "…" .. cwd:sub(-(room - 1)):gsub("^[\128-\191]+", "")
    end
    return { { { text = cwd, hl = "Comment" } } }
  end,
})

-- Reflector feedback (§10.3): a badge while a run is active, a toast with
-- the outcome when it ends.
zemi.ui.widget.register({
  id = "statusline.reflect",
  region = "statusline.left",
  priority = 20,
  render = function(ctx)
    if not ctx.reflecting then return empty() end
    return { { { text = "✻ reflecting", hl = "AgentReason" } } }
  end,
})

-- Learned proposals waiting for review; they persist across restarts.
zemi.ui.widget.register({
  id = "statusline.learn",
  region = "statusline.left",
  priority = 30,
  render = function(ctx)
    local segs = {}
    local function add(n, what, cmd)
      if (n or 0) == 0 then return end
      if #segs > 0 then segs[#segs + 1] = { text = " · ", hl = "Comment" } end
      segs[#segs + 1] = { text = "✻ " .. n .. " " .. what, hl = "Title" }
      segs[#segs + 1] = { text = " " .. cmd, hl = "Comment" }
    end
    add(ctx.pending_memory, "learned", "/memory")
    add(ctx.pending_config, "config", "/config")
    if #segs == 0 then return empty() end
    return { segs }
  end,
})

zemi.event.on("reflector.done", function(ev)
  if ev.error then
    zemi.notify.error({ title = "reflect failed", body = ev.error })
    return
  end
  local parts = {}
  local function add(n, one, many)
    if (n or 0) > 0 then parts[#parts + 1] = n .. " " .. (n == 1 and one or many) end
  end
  add(ev.stored, "memory stored", "memories stored")
  add(ev.queued, "memory to review in /memory", "memories to review in /memory")
  add(ev.superseded, "memory superseded", "memories superseded")
  add(ev.config_applied, "config rule applied", "config rules applied")
  add(ev.config_queued, "config change to review in /config", "config changes to review in /config")
  add(ev.skills_drafted, "skill drafted, enable it in /skills", "skills drafted, enable them in /skills")
  local body = #parts > 0 and table.concat(parts, ", ") or "nothing worth keeping"
  zemi.notify({ title = "reflect", body = body })
end)

-- Statusline right: "model · 12.3k tok · 45%". The context percent colors
-- by threshold: muted, warn at 80%, err at 95%.
zemi.ui.widget.register({
  id = "statusline.info",
  region = "statusline.right",
  priority = 10,
  render = function(ctx)
    local segs = {}
    local function add(text, hl)
      if #segs > 0 then segs[#segs + 1] = { text = " · ", hl = "Comment" } end
      segs[#segs + 1] = { text = text, hl = hl }
    end
    if (ctx.model or "") ~= "" then add(ctx.model, "Comment") end
    local total = fmt_tokens((ctx.usage or {}).total)
    if total then
      add(total .. " tok", "Comment")
      local pct = tonumber((ctx.context or {}).pct or 0) or 0
      if pct > 0 then
        local hl = pct >= 95 and "StatusUsageErr" or pct >= 80 and "StatusUsageWarn" or "StatusUsage"
        add(string.format("%d%%", pct), hl)
      end
      local cost = tonumber((ctx.usage or {}).cost or 0) or 0
      if cost > 0 then add(string.format("$%.2f", cost), "Comment") end
    end
    if #segs == 0 then return empty() end
    return { segs }
  end,
})

-- Chat transcript. User rows carry an accent bar; a turn's reasoning, tool
-- rows and markdown reply stay together, identical while streaming and
-- after, so nothing jumps when a reply lands. Reasoning collapses to one
-- line unless ctx.show_reasoning (ctrl+o). The widget wraps.
zemi.ui.widget.register({
  id = "chat",
  region = "chat",
  priority = 10,
  wrap = true,
  render = function(ctx)
    local width = ctx.width or 80
    local lines = {}
    local function blank() lines[#lines + 1] = { { text = "" } } end
    local function add_markdown(text)
      for _, segs in ipairs(zemi.ui.markdown(text, width - 2)) do
        local row = { { text = "  " } }
        for _, s in ipairs(segs) do row[#row + 1] = s end
        lines[#lines + 1] = row
      end
    end
    -- Reasoning: one "✻ thought 4.2s" header; ctrl+o (ctx.show_reasoning)
    -- expands the text under it, indented and dim.
    local function add_reasoning(label, text)
      lines[#lines + 1] = { { text = "  ✻ " .. label, hl = "AgentReason" } }
      if not ctx.show_reasoning then return end
      local body = {}
      for rl in (text .. "\n"):gmatch("(.-)\n") do
        body[#body + 1] = rl
      end
      while #body > 0 and body[#body] == "" do
        body[#body] = nil
      end
      for _, rl in ipairs(body) do
        lines[#lines + 1] = { { text = "    " .. rl, hl = "AgentReason" } }
      end
    end
    -- One tool row: status mark (running marks pulse with the working
    -- row), name, input cut by UTF-8 character to fit.
    local function add_tool(name, input, status)
      local mark, hl = ctx.pulse and "◦" or "•", "AgentToolRun"
      if status == "ok" then
        mark, hl = "✓", "AgentToolOk"
      elseif status == "err" then
        mark, hl = "✗", "AgentToolErr"
      end
      name = name or "?"
      local room, chars = width - #name - 7, {}
      for ch in (input or ""):gsub("%s+", " "):gmatch("[%z\1-\127\194-\244][\128-\191]*") do
        if #chars >= room then
          chars[#chars] = "…"
          break
        end
        chars[#chars + 1] = ch
      end
      local inp = table.concat(chars)
      lines[#lines + 1] = {
        { text = "  " .. mark, hl = hl },
        { text = " " .. name, hl = "AgentToolName" },
        { text = inp ~= "" and "  " .. inp or "", hl = "Comment" },
      }
    end

    local tr = ctx.transcript or {}
    if #tr == 0 and not ctx.agent_active then
      return { { { text = "no messages yet - type below, or / for commands", hl = "Comment" } } }
    end
    -- Only the latest collapsed reasoning line carries the expand hint.
    local last_reason = 0
    for i, m in ipairs(tr) do
      if m.kind == "reasoning" then last_reason = i end
    end
    for i, m in ipairs(tr) do
      -- Blank lines separate turns: around each user message only.
      if i > 1 and (m.role == "user" or tr[i - 1].role == "user") then blank() end
      if m.kind == "reasoning" then
        local secs = fmt_secs(m.thought)
        local label = "thought" .. (secs and " " .. secs or "")
        if i == last_reason and not ctx.show_reasoning then label = label .. " (ctrl+o to expand)" end
        add_reasoning(label, m.text)
      elseif m.kind == "tool" then
        add_tool(m.tool, m.text, m.status)
      elseif m.kind == "compact" then
        lines[#lines + 1] = { { text = "  ✻ " .. m.text, hl = "AgentReason" } }
      elseif m.kind == "error" then
        local cancelled = m.status == "cancelled"
        lines[#lines + 1] = {
          { text = cancelled and "  ■ " or "  ✗ ", hl = cancelled and "Comment" or "AgentToolErr" },
          { text = m.text, hl = cancelled and "Comment" or "AgentToolErr" },
        }
      elseif m.role == "user" then
        local first = true
        for text in (m.text .. "\n"):gmatch("(.-)\n") do
          lines[#lines + 1] = { { text = first and "▌ " or "  ", hl = "UserMsg" }, { text = text, hl = "UserMsg" } }
          first = false
        end
      else
        add_markdown(m.text)
        local parts = {}
        local telapsed, tout = fmt_secs(m.elapsed), fmt_tokens(m.out)
        if telapsed then parts[#parts + 1] = telapsed end
        if tout then parts[#parts + 1] = tout .. " out" end
        if #parts > 0 then
          lines[#lines + 1] = { { text = "  " .. table.concat(parts, " · "), hl = "Comment" } }
        end
      end
    end

    -- Live turn (§6.3) from the host-owned ctx.agent snapshot; the Go
    -- working row above the composer carries status and elapsed time.
    -- An empty Lua table would marshal as a map, so an empty render
    -- returns one blank line instead.
    local ag = ctx.agent or {}
    local tools, text, queued = ag.tools or {}, ag.text or "", ag.steering or {}
    if ag.active or #tools > 0 or text ~= "" then
      if #tr > 0 and tr[#tr].role == "user" then blank() end
      for _, t in ipairs(tools) do add_tool(t.name, t.input, t.status) end
      if ctx.show_reasoning and (ag.reasoning or "") ~= "" then add_reasoning("thinking", ag.reasoning) end
      if text ~= "" then add_markdown(text) end
      for _, q in ipairs(queued) do
        lines[#lines + 1] = { { text = "  ⧗ " .. q:gsub("%s+", " "), hl = "Comment" } }
      end
    end
    return #lines > 0 and lines or empty()
  end,
})

-- Default keymap (§8): rebindable by user config. Insert-mode bindings are
-- exact single keys (ctrl/alt combos); printable keys stay with the
-- composer. The transcript is bottom-anchored, so these keys and the wheel
-- move the viewport; sending snaps back to the tail.
zemi.keymap.bind("insert", "ctrl+o", "zemi.ui.reasoning.toggle", { desc = "toggle reasoning" })
zemi.keymap.bind("insert", "pgup", "zemi.ui.scroll.up", { desc = "scroll chat up a page" })
zemi.keymap.bind("insert", "pgdown", "zemi.ui.scroll.down", { desc = "scroll chat down a page" })
zemi.keymap.bind("insert", "ctrl+home", "zemi.ui.scroll.top", { desc = "scroll chat to top" })
zemi.keymap.bind("insert", "ctrl+end", "zemi.ui.scroll.bottom", { desc = "scroll chat to bottom" })
