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
    StatusUsage = { fg = "muted" },
    StatusUsageWarn = { fg = "warn" },
    StatusUsageErr = { fg = "err", bold = true },
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

-- Live-agent badge (§6.3). There are no modes: the composer is always
-- focused and slash commands replace the old cmdline.
zemi.ui.widget.register({
  id = "statusline.agent",
  region = "statusline.left",
  priority = 10,
  render = function(ctx)
    if not ctx.agent_active then
      return { { { text = "" } } }
    end
    return { { { text = " ● agent", hl = "AgentTag" } } }
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

-- Current model on the right edge (empty when nothing is registered).
zemi.ui.widget.register({
  id = "statusline.model",
  region = "statusline.right",
  priority = 9,
  render = function(ctx)
    local model = ctx.model or ""
    if model == "" then
      return { { { text = "" } } }
    end
    return { { { text = model, hl = "Comment" } } }
  end,
})

-- Session token tally and the context-window fill (§8): "12.3k tok · 45%".
-- The percent colors by threshold: muted, warn at 80%, err at 95%. Both
-- numbers come from the host-provided ctx.usage / ctx.context tables.
zemi.ui.widget.register({
  id = "statusline.usage",
  region = "statusline.right",
  priority = 9,
  render = function(ctx)
    local function fmt_tokens(n)
      if not n or n <= 0 then return nil end
      if n >= 1000000 then return string.format("%.1fM", n / 1000000) end
      if n >= 1000 then return string.format("%.1fk", n / 1000) end
      return string.format("%d", n)
    end
    local u = ctx.usage or {}
    local c = ctx.context or {}
    local total = fmt_tokens(u.total)
    if not total then
      return { { { text = "" } } }
    end
    local segs = { { text = total .. " tok", hl = "Comment" } }
    local pct = tonumber(c.pct or 0)
    if pct and pct > 0 then
      local hl = "StatusUsage"
      if pct >= 95 then
        hl = "StatusUsageErr"
      elseif pct >= 80 then
        hl = "StatusUsageWarn"
      end
      segs[#segs + 1] = { text = " · " .. string.format("%d%%", pct), hl = hl }
    end
    return { segs }
  end,
})

-- Chat transcript: no role names, the gutter makes the roles visually
-- distinctive instead (user rows carry an accent bar, assistant rows are
-- plain). The widget sets wrap, so long lines word-wrap instead of being
-- truncated. Assistant replies carry their turn stats (tokens, thought
-- time, elapsed) as a muted line; persisted reasoning blocks collapse to
-- one "✻ thought Xs" line unless ctx.show_reasoning is set (ctrl+o
-- toggles). Under the transcript, the live agent stream (DESIGN §6.3)
-- renders from the host-owned ctx.agent snapshot: tool call lines, dim
-- reasoning, the streaming reply, the model and the running thought time;
-- nothing here holds Lua state between frames.
zemi.ui.widget.register({
  id = "chat",
  region = "chat",
  priority = 10,
  wrap = true,
  render = function(ctx)
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
    local lines = {}
    local tr = ctx.transcript or {}
    if #tr == 0 and not ctx.agent_active then
      return { { { text = "no messages yet - type below, or / for commands", hl = "Comment" } } }
    end
    for i, m in ipairs(tr) do
      local is_reason = m.kind == "reasoning"
      -- Reasoning blocks stay attached to the reply they belong to: no
      -- blank line between user -> reasoning -> assistant text.
      if i > 1 and not is_reason and (tr[i - 1].kind or "") ~= "reasoning" then
        lines[#lines + 1] = { { text = "" } }
      end
      if is_reason then
        -- Collapsed by default (Claude Code style); ctrl+o expands. The
        -- effective state is session toggle, else ui.chat.reasoning.
        if ctx.show_reasoning then
          for text in (m.text .. "\n"):gmatch("(.-)\n") do
            lines[#lines + 1] = { { text = "  ⋮ ", hl = "Comment" }, { text = text, hl = "AgentReason" } }
          end
        else
          local tthought = fmt_secs(m.thought)
          local label = "✻ thought " .. (tthought or "0s")
          lines[#lines + 1] = { { text = "  " }, { text = label .. " (ctrl+o to expand)", hl = "AgentReason" } }
        end
      else
        local is_user = m.role == "user"
        local bar, bar_hl, body_hl = "  ", "", "Normal"
        if is_user then
          bar, bar_hl, body_hl = "▌ ", "UserMsg", "UserMsg"
        end
        local first = true
        for text in (m.text .. "\n"):gmatch("(.-)\n") do
          lines[#lines + 1] = {
            { text = first and bar or "  ", hl = bar_hl },
            { text = text, hl = body_hl },
          }
          first = false
        end
        if not is_user then
          local parts = {}
          local tin, tout = fmt_tokens(m["in"]), fmt_tokens(m.out)
          local tthought, telapsed = fmt_secs(m.thought), fmt_secs(m.elapsed)
          if tin then parts[#parts + 1] = tin .. " in" end
          if tout then parts[#parts + 1] = tout .. " out" end
          if tthought then parts[#parts + 1] = "thought " .. tthought end
          if telapsed then parts[#parts + 1] = telapsed end
          if #parts > 0 then
            lines[#lines + 1] = { { text = "  ↳ ", hl = "Comment" }, { text = table.concat(parts, " · "), hl = "Comment" } }
          end
        end
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
        local tthought = fmt_secs(tonumber(ag.thought or 0))
        if tthought then
          head[#head + 1] = { text = " │ ", hl = "Comment" }
          head[#head + 1] = { text = "✻ " .. tthought, hl = "AgentReason" }
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

-- Default keymap (§8): rebindable by user config. There are no modes;
-- insert-mode bindings are exact single keys (ctrl/alt combos), so
-- printable keys stay with the composer. /Reload is reserved and never
-- reaches the command registry.
zemi.keymap.bind("insert", "ctrl+o", "zemi.ui.reasoning.toggle", { desc = "toggle reasoning" })

-- Chat scrollback: the transcript is bottom-anchored (the altscreen has
-- no native scrollback), so the wheel and these keys move the viewport.
-- Sending a message snaps back to the tail; scrolling down to the bottom
-- re-arms the follow.
zemi.keymap.bind("insert", "pgup", "zemi.ui.scroll.up", { desc = "scroll chat up a page" })
zemi.keymap.bind("insert", "pgdown", "zemi.ui.scroll.down", { desc = "scroll chat down a page" })
zemi.keymap.bind("insert", "ctrl+home", "zemi.ui.scroll.top", { desc = "scroll chat to top" })
zemi.keymap.bind("insert", "ctrl+end", "zemi.ui.scroll.bottom", { desc = "scroll chat to bottom" })
