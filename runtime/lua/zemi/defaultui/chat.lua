-- The chat widget: the transcript, the live turn and the todo list.
local fmt = require("zemi.defaultui.text")

-- A tool row: a settled transcript row or a call of the live turn.
---@alias zemi.defaultui.Tool zemi.TranscriptRow|zemi.ToolCall

-- Rows of the settled transcript, kept across frames: while a reply
-- streams the widget renders many times a second and only the live turn
-- changes. One entry, replaced whenever its key changes.
---@type table<string, { lines: zemi.Line[], prev: string? }>
local chat_cache = {}

-- Chat transcript. User rows carry an accent bar; a turn's reasoning, tool
-- rows and markdown reply stay together, identical while streaming and
-- after, so nothing jumps when a reply lands. Reasoning collapses to one
-- line unless ctx.show_reasoning (ctrl+o). The widget wraps. A click on a
-- reasoning line toggles it; one on an agent's row shows that run; one on a
-- file tool's row opens the file in the user's editor.
zemi.ui.widget.register({
  id = "chat",
  region = "chat",
  priority = 10,
  wrap = true,
  ---@param ev zemi.WidgetClick
  on_click = function(ev)
    local run = ev.id:match("^agent:(.+)")
    local line, path = ev.id:match("^file:(%d+):(.+)$")
    if run then
      assert(zemi.command.run("agents", "view " .. run))
    elseif path then
      line = tonumber(line)
      -- a diff row takes a double click: a single one may start a selection
      if line > 0 and (ev.clicks or 1) < 2 then return end
      local function failed(err) zemi.notify.error(tostring(err), "Could not open " .. path) end
      local ok, err = pcall(zemi.ui.edit, path, { line = line }, function(e)
        if e then failed(e) end
      end)
      if not ok then failed(err) end
    elseif ev.id == "reasoning" then
      zemi.ui.reasoning.toggle()
    end
  end,
  ---@param ctx zemi.WidgetCtx
  ---@return zemi.Line[]
  render = function(ctx)
    local width = ctx.width or 80
    ---@type zemi.Line[]
    local lines = {}
    local function blank() lines[#lines + 1] = { { text = "" } } end
    -- Blocks are separated by one blank line: the user's message, a run of
    -- activity (thinking and tools), the reply, its stats.
    ---@type string?
    local prev
    ---@param block "user"|"activity"|"text"|"stats"
    local function gap(block)
      if prev then blank() end
      prev = block
    end
    ---@param segs zemi.Line
    ---@return boolean
    local function has_text(segs)
      for _, sg in ipairs(segs) do
        if (sg.text or ""):match("%S") then return true end
      end
      return false
    end
    -- A reply: its first line carries the bullet, the rest indent under it.
    ---@param text string
    ---@param mark boolean Put the bullet on the first line with text.
    local function add_markdown(text, mark)
      for _, segs in ipairs(zemi.ui.markdown(text, width - 2)) do
        local lead = "  "
        if mark and has_text(segs) then lead, mark = "● ", false end
        local row = { { text = lead, hl = "Normal", copy = false } }
        for _, sg in ipairs(segs) do row[#row + 1] = sg end
        lines[#lines + 1] = row
      end
    end
    -- Reasoning is a quiet aside: "✻ Thought for 4.2s" once done, the text
    -- under a thin bar with ctrl+o. While the model thinks, its last lines
    -- show (tail), so the wait is not a blank screen.
    -- A row that wraps onto the next ends in a space and is soft: a copy
    -- joins it to the next row.
    ---@param text string
    ---@param room integer
    ---@return string[] rows
    ---@return table<integer, true> soft The rows that wrap onto the next.
    local function wrap_words(text, room)
      ---@type string[], table<integer, true>
      local out, soft = {}, {}
      for para in (text .. "\n"):gmatch("(.-)\n") do
        local cur = ""
        for word in para:gmatch("%S+") do
          if cur ~= "" and fmt.cells(cur) + 1 + fmt.cells(word) > room then
            out[#out + 1] = cur .. " "
            soft[#out] = true
            cur = word
          else
            cur = cur == "" and word or (cur .. " " .. word)
          end
        end
        if cur ~= "" or (#out > 0 and out[#out] ~= "") then out[#out + 1] = cur end
      end
      while #out > 0 and out[#out] == "" do out[#out] = nil end
      return out, soft
    end
    ---@param label string
    ---@param hint string? The expand hint after the label.
    ---@param text string
    ---@param tail integer? Show only the last this many lines while collapsed.
    local function add_reasoning(label, hint, text, tail)
      local head = { { text = "✻ ", hl = "ThinkingMark", click = "reasoning", copy = false }, { text = label, hl = "AgentReason", click = "reasoning" } }
      if hint then head[#head + 1] = { text = "  " .. hint, hl = "Comment", click = "reasoning" } end
      lines[#lines + 1] = head
      if not ctx.show_reasoning and not tail then return end
      local body, soft = wrap_words(text, math.max(width - 6, 20))
      local first = (ctx.show_reasoning or not tail) and 1 or math.max(1, #body - tail + 1)
      for n = first, #body do
        lines[#lines + 1] = { { text = "  │ ", hl = "ThinkingBar", copy = false }, { text = body[n], hl = "AgentReason", soft_wrap = soft[n] } }
      end
    end

    -- Tool calls read as what they did: "Read internal/ui/app.go". Finished
    -- calls that only look around collapse into one line ("Read 3 files,
    -- ran 2 commands"); edits, agents, questions and failures keep their
    -- own rows, and ctrl+o shows every call.
    ---@type table<string, string>
    local VERB = {
      read = "Read", write = "Write", edit = "Edit", multiedit = "Edit", glob = "Find", grep = "Search",
      fetch = "Fetch", web_search = "Search web", bash = "Run", job_start = "Start", job_read = "Check job",
      job_list = "List jobs", job_kill = "Stop job", memory_add = "Remember", memory_search = "Recall",
      diagnostics = "Check", todos = "Update todos", question = "Ask", plan = "Plan", skill = "Load skill",
      agent = "Agent", workflow = "Workflow",
    }
    -- a verb, then the singular and plural noun; a verb alone never counts
    ---@type table<string, { [1]: string, [2]: string?, [3]: string? }>
    local COUNT = {
      read = { "read", "file", "files" }, glob = { "searched for", "pattern", "patterns" },
      grep = { "searched for", "pattern", "patterns" }, web_search = { "searched the web for", "query", "queries" },
      bash = { "ran", "command", "commands" }, fetch = { "fetched", "page", "pages" },
      skill = { "loaded", "skill", "skills" }, memory_add = { "saved", "memory", "memories" },
      memory_search = { "searched memory" }, diagnostics = { "checked", "file", "files" },
      job_start = { "started", "job", "jobs" }, job_read = { "checked", "job", "jobs" },
      job_list = { "listed jobs" }, job_kill = { "stopped", "job", "jobs" }, todos = { "updated todos" },
    }
    ---@param t zemi.defaultui.Tool
    ---@return string
    local function tool_name(t) return t.tool or t.name or "?" end
    ---@param t zemi.defaultui.Tool
    ---@return boolean
    local function collapsible(t)
      if t.kind ~= nil and t.kind ~= "tool" then return false end
      return not ctx.show_reasoning and t.status == "ok" and COUNT[tool_name(t)] ~= nil
    end
    ---@param s string
    ---@return string
    local function relpath(s)
      local cwd = ctx.cwd or ""
      if cwd == "" then return s end
      return (s:gsub(cwd:gsub("%p", "%%%0") .. "/", ""))
    end
    ---@param group zemi.defaultui.Tool[]
    local function add_group(group)
      ---@type string[], table<string, { c: { [1]: string, [2]: string?, [3]: string? }, n: integer }>
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
      ---@type string[]
      local parts = {}
      for _, key in ipairs(order) do
        local c, n = seen[key].c, seen[key].n
        parts[#parts + 1] = c[2] and string.format("%s %d %s", c[1], n, n == 1 and c[2] or c[3]) or c[1]
      end
      local summary = table.concat(parts, ", ")
      summary = summary:sub(1, 1):upper() .. summary:sub(2)
      lines[#lines + 1] = { { text = "● ", hl = "AgentToolOk", copy = false }, { text = fmt.clip(summary, width - 4), hl = "Normal" } }
    end
    -- tools whose input is a file path: their rows open that file
    ---@type table<string, boolean>
    local FILE_TOOL = { read = true, write = true, edit = true, multiedit = true }
    ---@param t zemi.defaultui.Tool
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
      -- expanded rows wrap (the widget wraps), collapsed ones stay one line
      ---@param text string
      ---@param room integer
      ---@return string
      local function fit(text, room) return ctx.show_reasoning and text or fmt.clip(text, room) end
      local head = { { text = mark, hl = hl, copy = false } }
      if label ~= "" then
        head[#head + 1] = { text = fit(label, width - 12), hl = "Normal" }
      else
        local verb = VERB[name] or name
        head[#head + 1] = { text = verb, hl = "AgentToolName" }
        if input ~= "" then head[#head + 1] = { text = " " .. fit(input, width - fmt.cells(verb) - 12), hl = "Comment" } end
      end
      if t.status == "run" and (t.elapsed or 0) >= 1 then
        head[#head + 1] = { text = string.format(" · %ds", math.floor(t.elapsed)), hl = "Comment" }
      end
      -- The file a file tool touched, for the click ids below: line 0 is the
      -- head row, a diff row carries its own line.
      local file = FILE_TOOL[name] and ((t.text or t.input or ""):match("^%s*(.-)%s*$")) or ""
      ---@param n integer
      ---@return string?
      local function file_click(n) return file ~= "" and ("file:" .. n .. ":" .. file) or nil end
      -- an agent's rows open its transcript (ids must differ between adjacent rows)
      local click = (t.run or "") ~= "" and ("agent:" .. t.run) or file_click(0)
      for _, sg in ipairs(head) do sg.click = click end
      lines[#lines + 1] = head
      -- under the row: the command a description stands for, then progress
      for _, sub in ipairs({ label ~= "" and input or "", ((t.detail or ""):gsub("%s+", " ")) }) do
        if sub ~= "" then
          lines[#lines + 1] = { { text = "  ⎿ ", hl = "Comment", copy = false }, { text = fit(sub, width - 6), hl = "Comment", click = click } }
        end
      end
      -- File tools show what they changed as numbered, tinted rows.
      local old, new, hunks = 0, 0, 0
      for dl in ((t.diff or "") .. "\n"):gmatch("(.-)\n") do
        local a, b = dl:match("^@@ %-(%d+),?%d* %+(%d+),?%d* @@")
        local kind = dl:sub(1, 1)
        if a then
          if hunks > 0 then lines[#lines + 1] = { { text = "      ⋮", hl = "Comment", copy = false } } end
          hunks = hunks + 1
          old = tonumber(a) --[[@as integer]]
          new = tonumber(b) --[[@as integer]]
        elseif dl ~= "" and dl:sub(1, 3) ~= "---" and dl:sub(1, 3) ~= "+++" then
          local num, style = 0, ({ ["+"] = "DiffAdd", ["-"] = "DiffDel", [" "] = "DiffCtx" })[kind]
          -- NOTE: gopher-lua mis-evaluates `a[#a + 1], b = b, x` (a gets x), so
          -- assignments whose right side names a left-side target stay split.
          if kind == "-" then
            num = old
            old = old + 1
          elseif style then
            num = new
            new = new + 1
            if kind == " " then old = old + 1 end
          end
          if style then
            -- without a hunk header this is the input-only preview ("- old", "+ new"): no numbers
            local lead = hunks > 0 and string.format("  %5d %s ", num, kind) or "        " .. kind .. " "
            local room = math.max(width - fmt.cells(lead), 1)
            local blank_lead = string.rep(" ", fmt.cells(lead))
            -- the number and +/- are decoration: a copy gives the code
            local parts = fmt.chunks((dl:sub(hunks > 0 and 2 or 3):gsub("\t", "    ")), room)
            local dclick = hunks > 0 and file_click(num) or nil
            for j, part in ipairs(parts) do
              lines[#lines + 1] = {
                { text = j == 1 and lead or blank_lead, hl = style .. "Num", copy = false, click = dclick },
                { text = part, hl = style, soft_wrap = j < #parts, click = dclick },
                { text = string.rep(" ", math.max(0, room - fmt.cells(part))), hl = style, copy = false, click = dclick },
              }
            end
          else
            lines[#lines + 1] = { { text = "        ", copy = false }, { text = dl, hl = "Comment" } }
          end
        end
      end
      -- the result's last lines: always for a failure and for what the user
      -- answered, for commands with ctrl+o
      local output = t.output or ""
      if name == "question" then output = output:gsub("^The user answered:\n", "") end
      if output ~= "" and (t.status == "err" or name == "question" or (ctx.show_reasoning and name == "bash")) then
        local ohl = t.status == "err" and "AgentToolErr" or "Comment"
        for ol in (output .. "\n"):gmatch("(.-)\n") do
          lines[#lines + 1] = { { text = "    ", copy = false }, { text = ol, hl = ohl } }
        end
      end
    end
    -- rows from i on: a collapsed group or one call; returns the next index
    ---@param list zemi.defaultui.Tool[]
    ---@param i integer
    ---@return integer
    local function add_tools(list, i)
      if collapsible(list[i]) then
        ---@type zemi.defaultui.Tool[]
        local group = {}
        while i <= #list and collapsible(list[i]) do
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
      return {} -- the splash widget fills the empty chat
    end
    -- Only the latest collapsed reasoning line carries the expand hint.
    local last_reason = 0
    for i, m in ipairs(tr) do
      if m.kind == "reasoning" then last_reason = i end
    end
    -- An agent run's transcript mutates in place and reports epoch 0, so
    -- only the session's settled rows are cached.
    local key = (ctx.transcript_epoch or 0) ~= 0 and table.concat({
      ctx.transcript_epoch, #tr, width, tostring(ctx.show_reasoning), ctx.cwd or "" }, "|") or nil
    local cached = key and chat_cache[key]
    if cached then
      for _, l in ipairs(cached.lines) do lines[#lines + 1] = l end
      prev = cached.prev
    end
    local i = cached and (#tr + 1) or 1
    while i <= #tr do
      local m = tr[i]
      local next_i = i + 1
      if m.kind == "reasoning" then
        gap("activity")
        local secs = fmt.secs(m.thought)
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
        lines[#lines + 1] = { { text = "$ ", hl = hl, copy = false }, { text = m.text, hl = "Normal" } }
        local n = 0
        for ol in ((m.detail or "") .. "\n"):gmatch("(.-)\n") do
          n = n + 1
          if n > 12 then
            lines[#lines + 1] = { { text = "  …", hl = "Comment", copy = false } }
            break
          end
          lines[#lines + 1] = { { text = "  ", copy = false }, { text = ol, hl = "Comment" } }
        end
      elseif m.kind == "compact" then
        gap("activity")
        lines[#lines + 1] = { { text = "✻ ", hl = "AgentReason", copy = false }, { text = m.text, hl = "AgentReason" } }
      elseif m.kind == "error" then
        -- Headline, then the raw error dimmed underneath.
        gap("activity")
        local cancelled = m.status == "cancelled"
        local first = true
        for text in (m.text .. "\n"):gmatch("(.-)\n") do
          local hl = (cancelled or not first) and "Comment" or "AgentToolErr"
          local mark = first and (cancelled and "■ " or "✗ ") or "  "
          lines[#lines + 1] = { { text = mark, hl = hl, copy = false }, { text = text, hl = hl } }
          first = false
        end
      elseif m.role == "user" then
        -- Markdown like replies, keeping the line breaks the user typed.
        gap("user")
        for _, segs in ipairs(zemi.ui.markdown(m.text, width - 2)) do
          local row = { { text = "▌ ", hl = "UserMsg", copy = false } }
          for _, sg in ipairs(segs) do row[#row + 1] = sg end
          lines[#lines + 1] = row
        end
      else
        gap("text")
        add_markdown(m.text, true)
        ---@type string[]
        local parts = {}
        local telapsed, tout = fmt.secs(m.elapsed), fmt.tokens(m.out)
        if telapsed then parts[#parts + 1] = telapsed end
        if tout then parts[#parts + 1] = tout .. " out" end
        if #parts > 0 then
          gap("stats")
          lines[#lines + 1] = { { text = "  ", copy = false }, { text = table.concat(parts, " · "), hl = "Comment" } }
        end
      end
      i = next_i
    end
    if key and not cached then
      ---@type zemi.Line[]
      local snap = {}
      for n, l in ipairs(lines) do snap[n] = l end
      chat_cache = { [key] = { lines = snap, prev = prev } }
    end

    -- Live turn (§6.3) from the host-owned ctx.agent snapshot; the Go
    -- working row above the composer carries status and elapsed time.
    local ag = ctx.agent or {}
    local tools, text, queued = ag.tools or {}, ag.text or "", ag.steering or {}
    if ag.active or #tools > 0 or text ~= "" or #queued > 0 then
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
        lines[#lines + 1] = { { text = "⧗ ", hl = "Comment", copy = false }, { text = (q:gsub("%s+", " ")), hl = "Comment" } }
      end
    end
    -- The todo list stays under the conversation until every item is done.
    local todos, open = ctx.todos or {}, false
    for _, td in ipairs(todos) do
      if td.status ~= "done" then open = true end
    end
    if open then
      blank()
      for n, td in ipairs(todos) do
        if n > 12 then
          lines[#lines + 1] = { { text = "… ", hl = "Comment", copy = false }, { text = (#todos - 12) .. " more", hl = "Comment" } }
          break
        end
        local mark, hl, item = "☐ ", "Normal", td.content
        if td.status == "done" then
          mark, hl = "☑ ", "Comment"
        elseif td.status == "in_progress" then
          mark, hl = "◼ ", "TodoActive"
          if (td.active_form or "") ~= "" then item = td.active_form end
        end
        lines[#lines + 1] = { { text = mark, hl = hl, copy = false }, { text = item, hl = hl } }
      end
    end
    return lines
  end,
})
