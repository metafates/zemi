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
    AgentReason = { fg = "muted" },
    ThinkingMark = { fg = "accent" },
    ThinkingBar = { fg = "#3b4261" },
    AgentToolName = { fg = "fg", bold = true },
    AgentToolRun = { fg = "muted" },
    AgentToolOk = { fg = "ok" },
    AgentToolErr = { fg = "err" },
    DiffAdd = { fg = "ok" },
    DiffDel = { fg = "err" },
    TodoActive = { fg = "fg", bold = true },
    StatusUsage = { fg = "muted" },
    StatusUsageWarn = { fg = "warn" },
    StatusUsageErr = { fg = "err", bold = true },
    ModeManual = { fg = "muted", bg = "panel" },
    ModePlan = { fg = "bg", bg = "ok", bold = true },
    ModeAuto = { fg = "bg", bg = "accent", bold = true },
    ModeYolo = { fg = "bg", bg = "err", bold = true },
    PromptPlan = { fg = "ok", bold = true },
    PromptAuto = { fg = "accent", bold = true },
    PromptYolo = { fg = "err", bold = true },
    -- splash wordmark, a top-down blend from accent to violet
    Logo1 = { fg = "#7aa2f7", bold = true },
    Logo2 = { fg = "#80a0f5", bold = true },
    Logo3 = { fg = "#8f9cf3", bold = true },
    Logo4 = { fg = "#9d99f1", bold = true },
    Logo5 = { fg = "#ac9ef3", bold = true },
    Logo6 = { fg = "#bb9af7", bold = true },
    SplashKey = { fg = "fg", bold = true },
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

-- Display width of a string: one cell per UTF-8 character (the splash only
-- uses narrow glyphs).
local function cells(s)
  return select(2, s:gsub("[^\128-\191]", ""))
end

local UTF8_CHAR = "[%z\1-\127\194-\244][\128-\191]*"

-- clip cuts text to room cells, ending with an ellipsis when it cuts.
local function clip(text, room)
  if cells(text) <= room then return text end
  if room < 1 then return "" end
  local chars = {}
  for ch in text:gmatch(UTF8_CHAR) do
    if #chars == room - 1 then break end
    chars[#chars + 1] = ch
  end
  return table.concat(chars) .. "…"
end

-- dot separates statusline groups.
local function dot(segs)
  if #segs > 0 then segs[#segs + 1] = { text = " · ", hl = "Comment" } end
end

local LOGO = {
  "███████╗███████╗███╗   ███╗██╗",
  "╚══███╔╝██╔════╝████╗ ████║██║",
  "  ███╔╝ █████╗  ██╔████╔██║██║",
  " ███╔╝  ██╔══╝  ██║╚██╔╝██║██║",
  "███████╗███████╗██║ ╚═╝ ██║██║",
  "╚══════╝╚══════╝╚═╝     ╚═╝╚═╝",
}

-- Splash: the wordmark, what zemi will run with, recent sessions to pick up
-- and the keys worth knowing, centered in the empty chat.
local function splash(ctx)
  -- the block is at most 72 cells wide, so long paths clip instead of
  -- pushing the logo against the left edge
  local width, rows = ctx.width or 80, {}
  local maxw = math.min(width, 72)
  local function row(segs) rows[#rows + 1] = segs end
  if width >= 36 then
    for i, l in ipairs(LOGO) do row({ { text = l, hl = "Logo" .. i } }) end
  else
    row({ { text = "zemi", hl = "Title" } })
  end
  local ver = (ctx.version or "") ~= "" and (ctx.version .. " · ") or ""
  row({ { text = ver .. "a coding agent that learns how you work", hl = "Comment" } })
  row({ { text = "" } })
  row({ { text = "model    ", hl = "Comment" }, { text = (ctx.model ~= "" and ctx.model) or "none: /health explains", hl = "Normal" } })
  local cwd, room = ctx.cwd or "", maxw - 10
  if cells(cwd) > room and room > 1 then
    -- keep the tail: the project name is at the end
    local chars = {}
    for ch in cwd:gmatch(UTF8_CHAR) do chars[#chars + 1] = ch end
    cwd = "…" .. table.concat(chars, "", #chars - room + 2)
  end
  row({ { text = "project  ", hl = "Comment" }, { text = cwd, hl = "Normal" } })
  local recent = ctx.recent or {}
  if #recent > 0 then
    row({ { text = "" } })
    row({ { text = "recent", hl = "Title" } })
    for _, r in ipairs(recent) do
      local title = (r.title ~= "" and r.title) or "(untitled)"
      row({ { text = string.format("  %-8s ", r.ago), hl = "Comment" }, { text = title, hl = "Normal" } })
    end
    row({ { text = "  /resume", hl = "SplashKey" }, { text = " continues the latest, ", hl = "Comment" },
      { text = "/sessions", hl = "SplashKey" }, { text = " lists them", hl = "Comment" } })
  end
  row({ { text = "" } })
  local tips = { { "/", "commands" }, { "@", "attach or drop files" }, { "!", "shell" }, { "tab", "permission mode" } }
  local tip = {}
  for i, t in ipairs(tips) do
    if i > 1 then tip[#tip + 1] = { text = "   " } end
    tip[#tip + 1] = { text = t[1], hl = "SplashKey" }
    tip[#tip + 1] = { text = " " .. t[2], hl = "Comment" }
  end
  row(tip)

  -- Center the block as a whole (rows stay left-aligned to each other),
  -- clipping to the width so a narrow window never wraps the logo.
  local widest = 0
  for i, r in ipairs(rows) do
    local n, clipped = 0, {}
    for _, seg in ipairs(r) do
      local left = maxw - n
      if left <= 0 then break end
      local text = clip(seg.text, left)
      clipped[#clipped + 1] = { text = text, hl = seg.hl }
      n = n + cells(text)
    end
    rows[i] = clipped
    widest = math.max(widest, n)
  end
  local pad = string.rep(" ", math.max(0, math.floor((width - widest) / 2)))
  local out = {}
  local top = math.max(0, math.floor(((ctx.height or 0) - #rows) / 2))
  for _ = 1, top do out[#out + 1] = { { text = "" } } end
  for _, r in ipairs(rows) do
    table.insert(r, 1, { text = pad })
    out[#out + 1] = r
  end
  -- the chat is bottom-anchored: trailing rows keep the block centered
  for _ = 1, math.max(0, (ctx.height or 0) - top - #rows - 1) do out[#out + 1] = { { text = "" } } end
  return out
end

-- Statusline left starts with the permission mode as a pill (tab cycles
-- it): quiet in manual, filled in auto and yolo, matching the composer
-- prompt color.
zemi.ui.widget.register({
  id = "statusline.mode",
  region = "statusline.left",
  priority = 5,
  render = function(ctx)
    local mode = ctx.permission_mode or "manual"
    local base = ctx.permission_mode_base or mode
    local hl = ({ plan = "ModePlan", auto = "ModeAuto", yolo = "ModeYolo" })[base] or "ModeManual"
    return { { { text = " " .. mode .. " ", hl = hl } } }
  end,
})

-- Then the working directory (tail kept when long, so the right cluster
-- always fits), then the reflector badge.
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

-- Agents launched from Lua or workflows, running outside the turn.
zemi.ui.widget.register({
  id = "statusline.agents",
  region = "statusline.left",
  priority = 25,
  render = function(ctx)
    local n = ctx.agents_running or 0
    if n == 0 then return empty() end
    return { { { text = "⧗ " .. n .. (n == 1 and " agent" or " agents"), hl = "Title" } } }
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
      dot(segs)
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
  -- a session with nothing worth keeping ends quietly
  if #parts == 0 then return end
  zemi.notify({ title = "learned from the last session", body = table.concat(parts, ", ") })
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
      dot(segs)
      segs[#segs + 1] = { text = text, hl = hl }
    end
    if (ctx.model or "") ~= "" then
      add(ctx.model .. ((ctx.effort or "") ~= "" and (" " .. ctx.effort) or ""), "Comment")
    end
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
    -- Blocks are separated by one blank line: the user's message, a run of
    -- activity (thinking and tools), the reply, its stats.
    local prev
    -- NOTE: gopher-lua mis-evaluates `a[#a + 1], b = b, x` (a gets x), so
    -- assignments whose right side names a left-side target stay split.
    local function gap(block)
      if prev then blank() end
      prev = block
    end
    local function has_text(segs)
      for _, sg in ipairs(segs) do
        if (sg.text or ""):match("%S") then return true end
      end
      return false
    end
    -- A reply: its first line carries the bullet, the rest indent under it.
    local function add_markdown(text, mark)
      for _, segs in ipairs(zemi.ui.markdown(text, width - 2)) do
        local lead = "  "
        if mark and has_text(segs) then lead, mark = "● ", false end
        local row = { { text = lead, hl = "Normal" } }
        for _, sg in ipairs(segs) do row[#row + 1] = sg end
        lines[#lines + 1] = row
      end
    end
    -- Reasoning is a quiet aside: "✻ Thought for 4.2s" once done, the text
    -- under a thin bar with ctrl+o. While the model thinks, its last lines
    -- show (tail), so the wait is not a blank screen.
    local function wrap_words(text, room)
      local out = {}
      for para in (text .. "\n"):gmatch("(.-)\n") do
        local cur = ""
        for word in para:gmatch("%S+") do
          if cur ~= "" and cells(cur) + 1 + cells(word) > room then
            out[#out + 1] = cur
            cur = word
          else
            cur = cur == "" and word or (cur .. " " .. word)
          end
        end
        if cur ~= "" or (#out > 0 and out[#out] ~= "") then out[#out + 1] = cur end
      end
      while #out > 0 and out[#out] == "" do out[#out] = nil end
      return out
    end
    local function add_reasoning(label, hint, text, tail)
      local head = { { text = "✻ ", hl = "ThinkingMark" }, { text = label, hl = "AgentReason" } }
      if hint then head[#head + 1] = { text = "  " .. hint, hl = "Comment" } end
      lines[#lines + 1] = head
      if not ctx.show_reasoning and not tail then return end
      local body = wrap_words(text, math.max(width - 6, 20))
      local first = (ctx.show_reasoning or not tail) and 1 or math.max(1, #body - tail + 1)
      for n = first, #body do
        lines[#lines + 1] = { { text = "  │ ", hl = "ThinkingBar" }, { text = body[n], hl = "AgentReason" } }
      end
    end

    -- Tool calls read as what they did: "Read internal/ui/app.go". Finished
    -- calls that only look around collapse into one line ("Read 3 files,
    -- ran 2 commands"); edits, agents, questions and failures keep their
    -- own rows, and ctrl+o shows every call.
    local VERB = {
      read = "Read", write = "Write", edit = "Edit", multiedit = "Edit", glob = "Find", grep = "Search",
      fetch = "Fetch", sourcegraph = "Search code", bash = "Run", job_start = "Start", job_read = "Check job",
      job_list = "List jobs", job_kill = "Stop job", memory_add = "Remember", memory_search = "Recall",
      diagnostics = "Check", todos = "Update todos", question = "Ask", skill = "Load skill",
      agent = "Agent", workflow = "Workflow",
    }
    local COUNT = {
      read = { "read", "file", "files" }, glob = { "searched for", "pattern", "patterns" },
      grep = { "searched for", "pattern", "patterns" }, sourcegraph = { "searched for", "pattern", "patterns" },
      bash = { "ran", "command", "commands" }, fetch = { "fetched", "page", "pages" },
      skill = { "loaded", "skill", "skills" }, memory_add = { "saved", "memory", "memories" },
      memory_search = { "searched memory" }, diagnostics = { "checked", "file", "files" },
      job_start = { "started", "job", "jobs" }, job_read = { "checked", "job", "jobs" },
      job_list = { "listed jobs" }, job_kill = { "stopped", "job", "jobs" }, todos = { "updated todos" },
    }
    local function tool_name(t) return t.tool or t.name or "?" end
    local function collapsible(t)
      return t.kind ~= nil and t.kind ~= "tool" and false or
        (not ctx.show_reasoning and t.status == "ok" and COUNT[tool_name(t)] ~= nil)
    end
    local function relpath(s)
      local cwd = ctx.cwd or ""
      if cwd == "" then return s end
      return (s:gsub(cwd:gsub("%p", "%%%0") .. "/", ""))
    end
    local function add_group(group)
      local order, seen = {}, {}
      for _, t in ipairs(group) do
        local c = COUNT[tool_name(t)]
        local key = c[1] .. "|" .. (c[2] or "")
        if not seen[key] then
          seen[key] = { c = c, n = 0 }
          order[#order + 1] = key
        end
        seen[key].n = seen[key].n + 1
      end
      local parts = {}
      for _, key in ipairs(order) do
        local c, n = seen[key].c, seen[key].n
        parts[#parts + 1] = c[2] and string.format("%s %d %s", c[1], n, n == 1 and c[2] or c[3]) or c[1]
      end
      local summary = table.concat(parts, ", ")
      summary = summary:sub(1, 1):upper() .. summary:sub(2)
      lines[#lines + 1] = { { text = "● ", hl = "AgentToolOk" }, { text = clip(summary, width - 4), hl = "Normal" } }
    end
    local function add_tool(t)
      local name = tool_name(t)
      local mark, hl = ctx.pulse and "◦ " or "● ", "AgentToolRun"
      if t.status == "ok" then
        mark, hl = "● ", "AgentToolOk"
      elseif t.status == "err" then
        mark, hl = "● ", "AgentToolErr"
      end
      local input = relpath(((t.text or t.input or ""):gsub("%s+", " ")))
      local label = t.label or ""
      local head = { { text = mark, hl = hl } }
      if label ~= "" then
        head[#head + 1] = { text = clip(label, width - 12), hl = "Normal" }
      else
        local verb = VERB[name] or name
        head[#head + 1] = { text = verb, hl = "AgentToolName" }
        if input ~= "" then head[#head + 1] = { text = " " .. clip(input, width - cells(verb) - 12), hl = "Comment" } end
      end
      if t.status == "run" and (t.elapsed or 0) >= 1 then
        head[#head + 1] = { text = string.format(" · %ds", math.floor(t.elapsed)), hl = "Comment" }
      end
      lines[#lines + 1] = head
      -- under the row: the command a description stands for, then progress
      for _, sub in ipairs({ label ~= "" and input or "", ((t.detail or ""):gsub("%s+", " ")) }) do
        if sub ~= "" then
          lines[#lines + 1] = { { text = "  ⎿ ", hl = "Comment" }, { text = clip(sub, width - 6), hl = "Comment" } }
        end
      end
      -- File tools show what they changed.
      for dl in ((t.diff or "") .. "\n"):gmatch("(.-)\n") do
        if dl ~= "" then
          local dhl = dl:sub(1, 2) == "- " and "DiffDel" or dl:sub(1, 2) == "+ " and "DiffAdd" or "Comment"
          lines[#lines + 1] = { { text = "    " .. dl:gsub("\t", "    "), hl = dhl } }
        end
      end
    end
    -- rows from i on: a collapsed group or one call; returns the next index
    local function add_tools(list, i)
      if collapsible(list[i]) then
        local group = {}
        while i <= #list and (list[i].kind == nil or list[i].kind == "tool") and collapsible(list[i]) do
          group[#group + 1] = list[i]
          i = i + 1
        end
        add_group(group)
        return i
      end
      add_tool(list[i])
      return i + 1
    end

    local tr = ctx.transcript or {}
    if #tr == 0 and not ctx.agent_active then
      return empty() -- the splash widget fills the empty chat
    end
    -- Only the latest collapsed reasoning line carries the expand hint.
    local last_reason = 0
    for i, m in ipairs(tr) do
      if m.kind == "reasoning" then last_reason = i end
    end
    local i = 1
    while i <= #tr do
      local m = tr[i]
      local next_i = i + 1
      if m.kind == "reasoning" then
        gap("activity")
        local secs = fmt_secs(m.thought)
        local label = secs and ("Thought for " .. secs) or "Thought"
        local hint = (i == last_reason and not ctx.show_reasoning) and "click or ctrl+o to read" or nil
        add_reasoning(label, hint, m.text)
      elseif m.kind == "tool" then
        gap("activity")
        next_i = add_tools(tr, i)
      elseif m.kind == "shell" then
        -- a !command: the command, then its output (long output folds)
        gap("user")
        local hl = m.status == "err" and "AgentToolErr" or "Title"
        lines[#lines + 1] = { { text = "$ ", hl = hl }, { text = m.text, hl = "Normal" } }
        local n = 0
        for ol in ((m.detail or "") .. "\n"):gmatch("(.-)\n") do
          n = n + 1
          if n > 12 then
            lines[#lines + 1] = { { text = "  …", hl = "Comment" } }
            break
          end
          lines[#lines + 1] = { { text = "  " .. ol, hl = "Comment" } }
        end
      elseif m.kind == "compact" then
        gap("activity")
        lines[#lines + 1] = { { text = "✻ " .. m.text, hl = "AgentReason" } }
      elseif m.kind == "error" then
        -- Headline, then the raw error dimmed underneath.
        gap("activity")
        local cancelled = m.status == "cancelled"
        local first = true
        for text in (m.text .. "\n"):gmatch("(.-)\n") do
          local hl = (cancelled or not first) and "Comment" or "AgentToolErr"
          local mark = first and (cancelled and "■ " or "✗ ") or "  "
          lines[#lines + 1] = { { text = mark, hl = hl }, { text = text, hl = hl } }
          first = false
        end
      elseif m.role == "user" then
        -- Markdown like replies, one typed line at a time so line breaks
        -- stay (glamour joins lines into paragraphs); a code fence renders
        -- as one block.
        gap("user")
        local chunks, fence = {}, nil
        for l in (m.text .. "\n"):gmatch("(.-)\n") do
          local marker = l:match("^%s*```") ~= nil
          if fence then
            fence[#fence + 1] = l
            if marker then
              chunks[#chunks + 1] = table.concat(fence, "\n")
              fence = nil
            end
          elseif marker then
            fence = { l }
          else
            chunks[#chunks + 1] = l
          end
        end
        if fence then chunks[#chunks + 1] = table.concat(fence, "\n") end
        for _, chunk in ipairs(chunks) do
          local rendered = chunk:match("%S") and zemi.ui.markdown(chunk, width - 2) or { {} }
          for _, segs in ipairs(rendered) do
            local row = { { text = "▌ ", hl = "UserMsg" } }
            for _, sg in ipairs(segs) do row[#row + 1] = sg end
            lines[#lines + 1] = row
          end
        end
      else
        gap("text")
        add_markdown(m.text, true)
        local parts = {}
        local telapsed, tout = fmt_secs(m.elapsed), fmt_tokens(m.out)
        if telapsed then parts[#parts + 1] = telapsed end
        if tout then parts[#parts + 1] = tout .. " out" end
        if #parts > 0 then
          gap("stats")
          lines[#lines + 1] = { { text = "  " .. table.concat(parts, " · "), hl = "Comment" } }
        end
      end
      i = next_i
    end

    -- Live turn (§6.3) from the host-owned ctx.agent snapshot; the Go
    -- working row above the composer carries status and elapsed time.
    -- An empty Lua table would marshal as a map, so an empty render
    -- returns one blank line instead.
    local ag = ctx.agent or {}
    local tools, text, queued = ag.tools or {}, ag.text or "", ag.steering or {}
    if ag.active or #tools > 0 or text ~= "" then
      local waiting = false
      local ti = 1
      while ti <= #tools do
        if tools[ti].status ~= "ok" and tools[ti].status ~= "err" then waiting = true end
        gap("activity")
        ti = add_tools(tools, ti)
      end
      -- the tail only while the model is thinking, not while a tool runs
      if (ag.reasoning or "") ~= "" and text == "" and not waiting then
        gap("activity")
        add_reasoning("Thinking", nil, ag.reasoning, 3)
      end
      if text ~= "" then
        gap("text")
        add_markdown(text, true)
      end
      for _, q in ipairs(queued) do
        lines[#lines + 1] = { { text = "⧗ " .. q:gsub("%s+", " "), hl = "Comment" } }
      end
    end
    -- The todo list stays under the conversation until every item is done.
    local todos, open = ctx.todos or {}, false
    for _, td in ipairs(todos) do
      if td.status ~= "done" then open = true end
    end
    if open then
      blank()
      for i, td in ipairs(todos) do
        if i > 12 then
          lines[#lines + 1] = { { text = "… " .. (#todos - 12) .. " more", hl = "Comment" } }
          break
        end
        local mark, hl, text = "☐ ", "Normal", td.content
        if td.status == "done" then
          mark, hl = "☑ ", "Comment"
        elseif td.status == "in_progress" then
          mark, hl = "◼ ", "TodoActive"
          if (td.active_form or "") ~= "" then text = td.active_form end
        end
        lines[#lines + 1] = { { text = mark, hl = hl }, { text = text, hl = hl } }
      end
    end
    return #lines > 0 and lines or empty()
  end,
})

-- Splash: fills the chat until the first message. Register a widget with
-- id "splash" in init.lua to replace it (ctx carries version, model, cwd,
-- recent, width and height).
zemi.ui.widget.register({
  id = "splash",
  region = "chat",
  priority = 5,
  render = function(ctx)
    if #(ctx.transcript or {}) > 0 or ctx.agent_active then return empty() end
    return splash(ctx)
  end,
})

-- Default keymap: user config can rebind or zemi.keymap.del these.
-- Printable keys stay with the composer. The transcript is bottom-anchored, so these keys and the wheel
-- move the viewport; sending snaps back to the tail.
zemi.keymap.set("ctrl+o", "zemi.ui.reasoning.toggle", { desc = "show thinking and every tool call" })
zemi.keymap.set("tab", "zemi.permission.mode.cycle", { desc = "next permission mode" })
zemi.keymap.set("shift+tab", "zemi.permission.mode.prev", { desc = "previous permission mode" })
zemi.keymap.set("pgup", "zemi.ui.scroll.up", { desc = "scroll chat up a page" })
zemi.keymap.set("pgdown", "zemi.ui.scroll.down", { desc = "scroll chat down a page" })
zemi.keymap.set("ctrl+home", "zemi.ui.scroll.top", { desc = "scroll chat to top" })
zemi.keymap.set("ctrl+end", "zemi.ui.scroll.bottom", { desc = "scroll chat to bottom" })

-- require("zemi.defaultui").splash(ctx) builds the default splash rows, so a
-- custom splash widget can add to it instead of copying it.
return { splash = splash }
