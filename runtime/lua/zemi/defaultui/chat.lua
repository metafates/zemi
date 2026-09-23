-- The chat widget: the transcript, the live turn and the todo list.
local fmt = require("zemi.defaultui.text")

-- A running tool's mark: a smooth braille spinner, stepped by ctx.frame
-- (50 ms a step), so a row that only shows in the live turn animates cleanly.
local BRAILLE = { "⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏" }

-- A tool row: a settled transcript row or a call of the live turn.
---@alias zemi.defaultui.Tool zemi.TranscriptRow|zemi.ToolCall

local HOME = zemi.env.get("HOME") or ""

--- The plan tool's result is written for the model; the row shows the verdict.
---@param output string
---@return string
local function plan_verdict(output)
  local feedback = output:match("^The user has not approved the plan and asked for changes: (.-)\nRevise the plan")
  if feedback then return "changes requested: " .. feedback end
  if output:match("^The user dismissed") then return "dismissed" end
  if not output:match("^The user approved") then return output end
  local path = output:match("The plan is saved at (.-)%.$")
  if not path then return "approved" end
  if HOME ~= "" and path:sub(1, #HOME + 1) == HOME .. "/" then path = "~" .. path:sub(#HOME + 1) end
  return "approved, saved to " .. path
end

-- The settled transcript is rendered in units, a message or a run of tool
-- rows, kept across frames by the index of their first message: while a
-- reply streams the widget renders many times a second and only the live
-- turn changes. `settled` is the units' lines in order, which a render hands
-- back as they are while the transcript has `count` rows.
---@class zemi.defaultui.ChatUnit
---@field lines zemi.Line[]
---@field next integer Index of the message after the unit.
---@field hint boolean The unit is the reasoning line that carries the expand hint.
---@type table<integer, zemi.defaultui.ChatUnit>
local units = {}
local units_key = ""
---@type { count: integer, lines: zemi.Line[], n: integer }?
local settled

-- Tool rows the user opened, by transcript index (negative for the live
-- turn's calls): a click shows that one row's whole command and output and
-- leaves the rest of the transcript alone. Cleared when the transcript is
-- replaced, and the negative half when a new turn starts, since the indexes
-- are then someone else's.
---@type table<integer, true>
local expanded = {}
local expanded_epoch = 0
local live_tools = 0

-- Chat transcript. User rows carry an accent bar; a turn's reasoning, tool
-- rows and markdown reply stay together, identical while streaming and
-- after, so nothing jumps when a reply lands. Reasoning collapses to one
-- line unless ctx.show_reasoning (ctrl+o). The widget wraps. A click on a
-- reasoning line toggles it; one on an agent's row shows that run; one on a
-- tool row expands it (a file tool's diff is always shown regardless).
zemi.ui.widget.add({
  id = "chat",
  region = "chat",
  priority = 10,
  wrap = true,
  ---@param ev zemi.WidgetClick
  on_click = function(ev)
    local run = ev.id:match("^agent:(.+)")
    if run then
      assert(zemi.command.run("agents", "view " .. run))
    elseif ev.id == "reasoning" then
      zemi.ui.reasoning.toggle()
    else
      local n = tonumber(ev.id:match("^tool:(-?%d+)$"))
      if n then
        expanded[n] = not expanded[n] or nil
        for first, u in pairs(units) do
          if first <= n and n < u.next then units[first] = nil end
        end
        settled = nil
      end
    end
  end,
  ---@param ctx zemi.WidgetCtx
  ---@return zemi.Line[]
  ---@return boolean more
  render = function(ctx)
    local width = ctx.width or 80
    ---@type zemi.Line[]
    local lines = {}
    local function blank() lines[#lines + 1] = { { text = "" } } end
    -- Blocks are separated by one blank line: the user's message, a run of
    -- activity (thinking and tools), the reply, its stats.
    ---@type string?
    local prev
    ---@param block "user"|"activity"|"text"|"stats"|"queued"
    local function gap(block)
      if prev then blank() end
      prev = block
    end
    -- A reply is indented, not bulleted: its lines share the user's left edge.
    ---@param text string
    local function add_markdown(text)
      for _, segs in ipairs(zemi.ui.markdown(text, width - 2)) do
        local row = { { text = "  ", hl = "Normal", copy = false } }
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
    ---@param spin boolean? Animate the mark with the braille spinner (live only).
    local function add_reasoning(label, hint, text, tail, spin)
      local mark = spin and (BRAILLE[ctx.frame % #BRAILLE + 1] .. " ") or "✻ "
      local head = { { text = mark, hl = "ThinkingMark", click = "reasoning", copy = false, hover = false }, { text = label, hl = "AgentReason", click = "reasoning", hover = false } }
      if hint then head[#head + 1] = { text = "  " .. hint, hl = "Comment", click = "reasoning", hover = false } end
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
      job_list = "List jobs", job_kill = "Stop job", memory = "Remember",
      diagnostics = "Check", todos = "Update todos", question = "Ask", plan = "Plan", skill = "Load skill",
      agent = "Agent", workflow = "Workflow",
    }
    -- a verb, then the singular and plural noun; a verb alone never counts
    ---@type table<string, { [1]: string, [2]: string?, [3]: string? }>
    local COUNT = {
      read = { "read", "file", "files" }, glob = { "searched for", "pattern", "patterns" },
      grep = { "searched for", "pattern", "patterns" }, web_search = { "searched the web for", "query", "queries" },
      bash = { "ran", "command", "commands" }, fetch = { "fetched", "page", "pages" },
      skill = { "loaded", "skill", "skills" }, memory = { "saved", "memory", "memories" },
      diagnostics = { "checked", "file", "files" },
      job_start = { "started", "job", "jobs" }, job_read = { "checked", "job", "jobs" },
      job_list = { "listed jobs" }, job_kill = { "stopped", "job", "jobs" }, todos = { "updated todos" },
    }
    ---@param t zemi.defaultui.Tool
    ---@return string
    local function tool_name(t) return t.tool or t.name or "?" end
    -- title-case a tool name for the chat: underscores split into words
    -- ("web_search" -> "Web Search", "mcp_foo_bar" -> "Mcp Foo Bar").
    ---@param name string
    ---@return string
    local function title(name)
      local out = {}
      for w in name:gmatch("[^_]+") do
        out[#out + 1] = w:sub(1, 1):upper() .. w:sub(2)
      end
      return table.concat(out, " ")
    end
    ---@param t zemi.defaultui.Tool
    ---@return boolean
    local function collapsible(t)
      if t.kind ~= nil and t.kind ~= "tool" then return false end
      -- a command that changed files keeps its row: the diff is the news; so
      -- does one that went on as a job, which the user should know is running
      if tool_name(t) == "bash" and (t.output or ""):find("moved to the background as job-", 1, true) then return false end
      return not ctx.show_reasoning and t.status == "ok" and COUNT[tool_name(t)] ~= nil and (t.diff or "") == ""
    end
    ---@param s string
    ---@return string
    local function relpath(s)
      local cwd = ctx.cwd or ""
      if cwd == "" then return s end
      return (s:gsub(cwd:gsub("%p", "%%%0") .. "/", ""))
    end
    ---@param group zemi.defaultui.Tool[]
    ---@param click string The id a click on the summary row carries.
    local function add_group(group, click)
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
      lines[#lines + 1] = { { text = "✓ ", hl = "AgentToolOk", copy = false, click = click },
        { text = fmt.clip(summary, width - 4), hl = "Normal", click = click } }
    end
    ---@param t zemi.defaultui.Tool
    ---@param idx integer Transcript index, negative for a live-turn call.
    local function add_tool(t, idx)
      local name = tool_name(t)
      local open = ctx.show_reasoning or expanded[idx] or false
      local mark, hl = BRAILLE[ctx.frame % #BRAILLE + 1] .. " ", "AgentToolRun"
      if t.status == "ok" then
        mark, hl = "✓ ", "AgentToolOk"
      elseif t.status == "err" then
        mark, hl = "✗ ", "AgentToolErr"
      end
      local input = relpath(((t.text or t.input or ""):gsub("%s+", " ")))
      local label = t.label or ""
      -- expanded rows wrap (the widget wraps), collapsed ones stay one line
      ---@param text string
      ---@param room integer
      ---@return string
      local function fit(text, room) return open and text or fmt.clip(text, room) end
      local head = { { text = mark, hl = hl, copy = false } }
      if label ~= "" then
        head[#head + 1] = { text = fit(label, width - 12), hl = "Normal" }
      else
        local verb = VERB[name] or title(name)
        head[#head + 1] = { text = verb, hl = "AgentToolName" }
        if input ~= "" then head[#head + 1] = { text = " " .. fit(input, width - fmt.cells(verb) - 12), hl = "Comment" } end
      end
      if t.status == "run" and (t.elapsed or 0) >= 1 then
        head[#head + 1] = { text = " · " .. fmt.elapsed(t.elapsed), hl = "Comment" }
      end
      -- an agent's rows open its transcript (ids must differ between adjacent rows),
      -- every other row (file tools included) expands the call
      local click = (t.run or "") ~= "" and ("agent:" .. t.run) or ("tool:" .. idx)
      for _, sg in ipairs(head) do sg.click = click end
      lines[#lines + 1] = head
      -- under the row: the command a description stands for, then progress,
      -- then the job a timed-out command went on as (the ✓ alone would read
      -- as finished; the phrase is tools.JobHandoff)
      local job = name == "bash" and (t.output or ""):match("moved to the background as (job%-%d+)") or ""
      for _, sub in ipairs({ label ~= "" and input or "", ((t.detail or ""):gsub("%s+", " ")), job ~= "" and "still running as " .. job or "" }) do
        if sub ~= "" then
          lines[#lines + 1] = { { text = "  ⎿ ", hl = "Comment", copy = false }, { text = fit(sub, width - 6), hl = "Comment", click = click, hover = false } }
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
        -- file headers come before the first hunk; after it "---" is a removed "-- " line
        elseif dl ~= "" and (hunks > 0 or (dl:sub(1, 3) ~= "---" and dl:sub(1, 3) ~= "+++")) then
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
            for j, part in ipairs(parts) do
              -- a diff line is body: no hover, but the click expands like the head
              lines[#lines + 1] = {
                { text = j == 1 and lead or blank_lead, hl = style .. "Num", copy = false, click = click, hover = false },
                { text = part, hl = style, soft_wrap = j < #parts, click = click, hover = false },
                { text = string.rep(" ", math.max(0, room - fmt.cells(part))), hl = style, copy = false, click = click, hover = false },
              }
            end
          else
            -- a bash row names each file it changed: its hunks count from one
            hunks = 0
            lines[#lines + 1] = { { text = "        ", copy = false }, { text = dl, hl = "Comment" } }
          end
        end
      end
      -- the result's last lines: always for a failure, for what the user
      -- answered (question, plan), for commands with ctrl+o
      local output = t.output or ""
      if name == "question" then output = output:gsub("^The user answered:\n", "") end
      -- what the user just decided needs no restating: the model's text
      -- carries the reason and the instructions, the row the verdict
      if output:match("^permission denied %(user") then output = "denied by you" end
      if name == "plan" then output = plan_verdict(output) end
      if output ~= "" and (t.status == "err" or name == "question" or name == "plan" or expanded[idx] or (ctx.show_reasoning and name == "bash")) then
        local ohl = t.status == "err" and "AgentToolErr" or "Comment"
        for ol in (output .. "\n"):gmatch("(.-)\n") do
          lines[#lines + 1] = { { text = "    ", copy = false }, { text = ol, hl = ohl } }
        end
      end
    end
    -- rows from i on: a collapsed group or one call; returns the next index
    ---@param list zemi.defaultui.Tool[]
    ---@param i integer
    ---@param live boolean? The live turn's calls, indexed apart from the transcript.
    ---@return integer
    local function add_tools(list, i, live)
      local idx = live and -i or i
      if collapsible(list[i]) then
        ---@type zemi.defaultui.Tool[]
        local group = {}
        while i <= #list and collapsible(list[i]) do
          group[#group + 1] = list[i]
          i = i + 1
        end
        -- one click id for the whole group: it opens every row of it and
        -- a click on any of those closes it again
        if expanded[idx] then
          for _, t in ipairs(group) do add_tool(t, idx) end
        else
          add_group(group, "tool:" .. idx)
        end
        return i
      end
      add_tool(list[i], idx)
      return i + 1
    end

    local count = ctx.transcript_count or 0
    if count == 0 and not ctx.agent_active then
      return {} -- the splash widget fills the empty chat
    end
    -- An agent run's transcript mutates in place and reports epoch 0, so
    -- only the session's settled rows are cached.
    local epoch = ctx.transcript_epoch or 0
    if epoch ~= expanded_epoch then expanded, expanded_epoch = {}, epoch end
    -- The live turn's calls are keyed by negative index and the host restarts
    -- that list every turn, so a shorter list means those keys now stand for
    -- another turn's rows.
    local ntools = #((ctx.agent or {}).tools or {})
    if ntools < live_tools then
      for n in pairs(expanded) do
        if n < 0 then expanded[n] = nil end
      end
    end
    live_tools = ntools
    -- The messages first..last as one unit: a run of tool rows, else one.
    ---@param tr zemi.TranscriptRow[]
    ---@param first integer
    ---@param last integer
    ---@param hinted boolean
    ---@return zemi.defaultui.ChatUnit
    local function unit(tr, first, last, hinted)
      local m = tr[first]
      lines, prev = {}, first > 1 and "above" or nil
      if m.kind == "reasoning" then
        gap("activity")
        local secs = fmt.secs(m.thought)
        local label = secs and ("Thought for " .. secs) or "Thought"
        local hint = (hinted and not ctx.show_reasoning) and "click or ctrl+o to read" or nil
        add_reasoning(label, hint, m.text)
      elseif m.kind == "tool" then
        local i = first
        while i <= last do
          gap("activity")
          i = add_tools(tr, i)
        end
      elseif m.kind == "shell" then
        -- a !command: the command, then its output, already cut to a head
        -- and a tail by the host
        gap("user")
        local hl = m.status == "err" and "AgentToolErr" or "Title"
        lines[#lines + 1] = { { text = "$ ", hl = hl, copy = false }, { text = m.text, hl = "Normal" },
          m.status == "err" and { text = "  failed", hl = "AgentToolErr", copy = false } or nil }
        for ol in ((m.detail or "") .. "\n"):gmatch("(.-)\n") do
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
        add_markdown(m.text)
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
      return { lines = lines, next = last + 1, hint = hinted }
    end

    local key = table.concat({ epoch, width, tostring(ctx.show_reasoning), ctx.cwd or "" }, "|")
    if key ~= units_key or epoch == 0 then units, units_key, settled = {}, key, nil end
    -- A long session resumed, a new width or ctrl+o leave every unit to
    -- render, far more than one render may take: the latest come first, and
    -- past the budget the render returns them and asks for another.
    local more = false
    local done = settled and settled.count == count and settled
    if not done then
      -- ctx.transcript copies every row into Lua, so only a miss reads it
      local tr = ctx.transcript or {}
      -- Only the latest collapsed reasoning line carries the expand hint.
      local last_reason = 0
      for n, m in ipairs(tr) do
        if m.kind == "reasoning" then last_reason = n end
      end
      local deadline = epoch ~= 0 and os.clock() + 0.03
      ---@type zemi.defaultui.ChatUnit[]
      local shown = {}
      local rendered = 0
      local last = #tr
      while last >= 1 do
        local first = last
        while first > 1 and tr[first].kind == "tool" and tr[first - 1].kind == "tool" do first = first - 1 end
        local u = units[first]
        if not u or u.next ~= last + 1 or u.hint ~= (first == last_reason) then
          if deadline and rendered > 0 and os.clock() > deadline then
            more = true
            break
          end
          u = unit(tr, first, last, first == last_reason)
          units[first] = u
          rendered = rendered + 1
        end
        shown[#shown + 1] = u
        last = first - 1
      end
      ---@type zemi.Line[]
      local all = {}
      for n = #shown, 1, -1 do
        for _, l in ipairs(shown[n].lines) do all[#all + 1] = l end
      end
      done = { count = count, lines = all, n = #all }
      settled = not more and done or nil
    end
    -- the live turn's lines go after the settled ones in the same list
    lines, prev = done.lines, count > 0 and "settled" or nil
    for n = #lines, done.n + 1, -1 do lines[n] = nil end

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
        ti = add_tools(tools, ti, true)
      end
      -- the tail only while the model is thinking, not while a tool runs
      if (ag.reasoning or "") ~= "" and text == "" and not waiting then
        gap("activity")
        add_reasoning("Thinking", nil, ag.reasoning, 3, true)
      end
      if text ~= "" then
        gap("text")
        add_markdown(text)
      end
      -- queued messages are the user's own words waiting their turn, so they
      -- carry the user row's accent bar, marked queued, apart from the
      -- agent's rows above
      for _, q in ipairs(queued) do
        gap("queued")
        local first = true
        for _, segs in ipairs(zemi.ui.markdown(q, width - 2)) do
          local row = { { text = "▌ ", hl = "UserMsg", copy = false } }
          if first then
            row[#row + 1] = { text = " queued ", hl = "Queued", copy = false }
            first = false
          end
          for _, sg in ipairs(segs) do row[#row + 1] = sg end
          lines[#lines + 1] = row
        end
      end
    end
    -- The todo list stays under the conversation until every item is done.
    local todos, left = ctx.todos or {}, 0
    for _, td in ipairs(todos) do
      if td.status ~= "done" then left = left + 1 end
    end
    if left > 0 then
      blank()
      -- a header in the composer rule's voice: the name, how far along, and
      -- a short meter of one cell per item
      local head = { { text = "Todos ", hl = "TodoTitle" }, { text = (#todos - left) .. "/" .. #todos .. "  ", hl = "Comment" } }
      for n, td in ipairs(todos) do
        if n > 12 then break end
        head[#head + 1] = { text = "━", hl = td.status == "done" and "TodoDone" or td.status == "in_progress" and "TodoMark" or "Rule", copy = false }
      end
      lines[#lines + 1] = head
      -- at most 12 rows, the live ones first: in progress, then pending,
      -- then done, so a long list never hides the item being worked on
      local rank = { in_progress = 1, pending = 2, done = 3 }
      ---@type { td: zemi.Todo, n: integer }[]
      local shown = {}
      for n, td in ipairs(todos) do shown[#shown + 1] = { td = td, n = n } end
      table.sort(shown, function(a, b)
        local ra, rb = rank[a.td.status] or 2, rank[b.td.status] or 2
        if ra ~= rb then return ra < rb end
        return a.n < b.n
      end)
      for n, it in ipairs(shown) do
        local td = it.td
        if n > 12 then
          local hidden, done = #shown - 12, 0
          for i = 13, #shown do
            if shown[i].td.status == "done" then done = done + 1 end
          end
          if done == hidden then
            lines[#lines + 1] = { { text = "  ✓ ", hl = "TodoDone", copy = false }, { text = done .. " done", hl = "Comment" } }
          else
            lines[#lines + 1] = { { text = "  … ", hl = "Comment", copy = false }, { text = hidden .. " more", hl = "Comment" } }
          end
          break
        end
        local mark, mhl, hl, item = "  ○ ", "Rule", "Normal", td.content
        if td.status == "done" then
          mark, mhl, hl = "  ✓ ", "TodoDone", "Comment"
        elseif td.status == "in_progress" then
          mark, mhl, hl = "  ▸ ", "TodoMark", "TodoActive"
          if (td.active_form or "") ~= "" then item = td.active_form end
        end
        lines[#lines + 1] = { { text = mark, hl = mhl, copy = false }, { text = item, hl = hl } }
      end
    end
    return lines, more
  end,
})
