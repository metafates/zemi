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

-- Mode indicator with the armed multi-key pending sequence (§6.2).
zemi.ui.widget.register({
  id = "statusline.mode",
  region = "statusline.left",
  priority = 10,
  render = function(ctx)
    local mode = (ctx.mode or "insert"):upper()
    local hl = mode == "NORMAL" and "Title" or "StatusLineMode"
    local segs = { { text = " " .. mode .. " ", hl = hl } }
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
-- message's own newlines; user rows are accented, the rest muted.
zemi.ui.widget.register({
  id = "chat",
  region = "chat",
  priority = 10,
  render = function(ctx)
    local tr = ctx.transcript or {}
    if #tr == 0 then
      return { { { text = "no messages yet - type below and press enter", hl = "Comment" } } }
    end
    local lines = {}
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
    return lines
  end,
})

-- Default keymaps (§8): all rebindable by user config.
zemi.keymap.bind("normal", ":", "zemi.cmdline.open", { desc = "open cmdline" })
zemi.keymap.bind("normal", "tab", "zemi.ui.sidebar.toggle", { desc = "toggle sidebar" })
zemi.keymap.bind("normal", "?", "zemi.ui.help", { desc = "help overlay" })
