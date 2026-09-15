-- defaultui: the shipped theme, widgets, and default keymaps (DESIGN §6.6,
-- §8). Runs as the builtin origin via require("zemi.defaultui"); every
-- registration here is replaceable by user config (origin precedence:
-- user > builtin > plugin > project).

zemi.ui.theme.register("default", {
  palette = {
    bg = "#1a1b26",
    fg = "#c0caf5",
    muted = "#565f89",
    accent = "#7aa2f7",
    ok = "#9ece6a",
    warn = "#e0af68",
    err = "#f7768e",
  },
  hl = {
    Normal = { fg = "fg" },
    Comment = { fg = "muted" },
    Title = { fg = "accent", bold = true },
    StatusLine = { fg = "muted", bg = "bg" },
    StatusLineMode = { fg = "bg", bg = "accent", bold = true },
    Cmdline = { fg = "fg" },
    CmdlineError = { fg = "err" },
    UserMsg = { fg = "accent" },
    ToastWarn = { fg = "warn" },
    ToastError = { fg = "bg", bg = "err", bold = true },
    AgentTag = { fg = "accent", bold = true },
    AgentReason = { fg = "muted", italic = true },
    AgentToolName = { fg = "fg", bold = true },
    AgentToolRun = { fg = "warn" },
    AgentToolOk = { fg = "ok" },
    AgentToolErr = { fg = "err" },
  },
})
zemi.ui.theme.set("default")

-- Header: app name badge + working directory.
zemi.ui.widget.register({
  id = "header",
  region = "header",
  priority = 10,
  render = function(ctx)
    return {
      {
        { text = " zemi ", hl = "StatusLineMode" },
        { text = " " .. (ctx.cwd or ""), hl = "Comment" },
      },
    }
  end,
})

-- Mode indicator with the armed multi-key pending sequence (§6.2) and
-- the live-agent badge (§6.3).
zemi.ui.widget.register({
  id = "statusline.mode",
  region = "statusline.left",
  priority = 10,
  render = function(ctx)
    local mode = (ctx.mode or "insert"):upper()
    local hl = mode == "NORMAL" and "Title" or "StatusLineMode"
    local segs = { { text = " " .. mode .. " ", hl = hl } }
    if ctx.agent_active then
      segs[#segs + 1] = { text = " ● agent ", hl = "AgentTag" }
    end
    if ctx.pending and ctx.pending ~= "" then
      segs[#segs + 1] = { text = " " .. ctx.pending, hl = "Comment" }
    end
    return { segs }
  end,
})

-- Clock. The time comes from ctx.time (host-provided UI state), so the
-- golden harness can pin it; no os.date polling.
zemi.ui.widget.register({
  id = "clock",
  region = "statusline.right",
  priority = 10,
  render = function(ctx)
    return { { { text = ctx.time or "", hl = "Comment" } } }
  end,
})

-- Chat transcript: one `role │ text` row per message, wrapped on the
-- message's own newlines; user rows are accented, the rest muted. Under
-- the transcript, the live agent stream (DESIGN §6.3) renders from the
-- host-owned ctx.agent snapshot: tool call lines, dim reasoning, and the
-- streaming reply; nothing here holds Lua state between frames.
zemi.ui.widget.register({
  id = "chat",
  region = "chat",
  priority = 10,
  render = function(ctx)
    local lines = {}
    local tr = ctx.transcript or {}
    if #tr == 0 and not ctx.agent_active then
      return { { { text = "no messages yet - type below and press enter", hl = "Comment" } } }
    end
    for _, m in ipairs(tr) do
      local role_hl = m.role == "user" and "UserMsg" or "Comment"
      local pad = string.rep(" ", #m.role)
      local first = true
      for text in (m.text .. "\n"):gmatch("(.-)\n") do
        local role_seg = { text = first and m.role or pad, hl = role_hl }
        lines[#lines + 1] = { role_seg, { text = " │ ", hl = "Comment" }, { text = text, hl = "Normal" } }
        first = false
      end
    end
    local ag = ctx.agent or {}
    local has_stream = ag.active
      or (ag.text and ag.text ~= "")
      or (ag.reasoning and ag.reasoning ~= "")
      or (ag.tools and #ag.tools > 0)
    if has_stream then
      lines[#lines + 1] = { { text = "" } }
      if ag.active then
        local head = { { text = "● agent", hl = "AgentTag" } }
        local model = ag.model or ctx.model
        if model and model ~= "" then
          head[#head + 1] = { text = " │ ", hl = "Comment" }
          head[#head + 1] = { text = model, hl = "Comment" }
        end
        lines[#lines + 1] = head
      end
      for _, t in ipairs(ag.tools or {}) do
        local mark, hl = "→", "AgentToolRun"
        if t.status == "ok" then
          mark, hl = "✓", "AgentToolOk"
        elseif t.status == "err" then
          mark, hl = "✗", "AgentToolErr"
        end
        local inp = (t.input or ""):gsub("%s+", " ")
        if #inp > 48 then
          inp = inp:sub(1, 45) .. "..."
        end
        local row = { { text = "  " }, { text = mark, hl = hl }, { text = " " .. (t.name or "?"), hl = "AgentToolName" } }
        if inp ~= "" then
          row[#row + 1] = { text = "  " .. inp, hl = "Comment" }
        end
        lines[#lines + 1] = row
      end
      if ag.reasoning and ag.reasoning ~= "" then
        for rl in (ag.reasoning .. "\n"):gmatch("(.-)\n") do
          lines[#lines + 1] = { { text = "  ⋮ ", hl = "Comment" }, { text = rl, hl = "AgentReason" } }
        end
      end
      if ag.text and ag.text ~= "" then
        for tl in (ag.text .. "\n"):gmatch("(.-)\n") do
          lines[#lines + 1] = { { text = "  ┃ ", hl = "AgentTag" }, { text = tl, hl = "Normal" } }
        end
      end
      if (ag.steering or 0) > 0 then
        lines[#lines + 1] = { { text = "  ⧗ " .. ag.steering .. " queued", hl = "AgentToolRun" } }
      end
    end
    return lines
  end,
})

-- Default keymaps (§8): all rebindable by user config.
zemi.keymap.bind("normal", ":", "zemi.cmdline.open", { desc = "open cmdline" })
zemi.keymap.bind("normal", "tab", "zemi.ui.sidebar.toggle", { desc = "toggle sidebar" })
zemi.keymap.bind("normal", "?", "zemi.ui.help", { desc = "help overlay" })
