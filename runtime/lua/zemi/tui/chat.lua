-- The chat widget: the transcript, the live turn and the todo list.
local fmt = require("zemi.tui.text")
local util = require("zemi.util")

-- The one per-row "running now" mark: a braille spinner, stepped by
-- ctx.frame (50 ms a step), so a row that only shows live animates cleanly.
local BRAILLE = { "⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏" }

-- The expand hint on the latest collapsed reasoning line.
local HINT = "click or ctrl+o to read"

-- A tool row: a settled transcript row or a call of the live turn.
---@alias zemi.tui.Tool zemi.TranscriptRow|zemi.ToolCall

-- The settled transcript is rendered in units, a message or a run of tool
-- rows, kept across frames by the index of their first message: while a
-- reply streams the widget renders many times a second and only the live
-- turn changes. `settled` is the units' lines in order, which a render hands
-- back as they are while the transcript has `count` rows.
---@class zemi.tui.ChatUnit
---@field lines zemi.Line[]
---@field next integer Index of the message after the unit.
---@field hint boolean The unit is the reasoning line that carries the expand hint.
---@type table<integer, zemi.tui.ChatUnit>
local units = {}
local units_key = ""
---@type { count: integer, lines: zemi.Line[], n: integer, live_reason: boolean }?
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
-- reasoning line toggles it; one on an agent's row shows that run, one on a
-- workflow's opens its view; one on a tool row expands it (a file tool's
-- diff is always shown regardless).
zemi.ui.widget.add({
	id = "chat",
	region = "chat",
	priority = 10,
	wrap = true,
	---@param ev zemi.WidgetClick
	on_click = function(ev)
		local run, flow = ev.id:match("^agent:(.+)"), ev.id:match("^workflow:(.+)")
		if run then
			assert(zemi.command.run("agents", "view " .. run))
		elseif flow then
			assert(zemi.command.run("workflows", "view " .. flow))
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
				for _, sg in ipairs(segs) do
					row[#row + 1] = sg
				end
				lines[#lines + 1] = row
			end
		end
		-- A row's mark: the spinner while it runs, else how it ended (text.MARK).
		-- A mark never hovers: a hovered header changes only its text.
		---@param kind string A text.MARK key, or "spin".
		---@return zemi.Segment
		local function mark(kind)
			if kind == "spin" then
				return { text = BRAILLE[ctx.frame % #BRAILLE + 1] .. " ", hl = "Dim", copy = false, hover = false }
			end
			local m = fmt.MARK[kind]
			return { text = m[1] .. " ", hl = m[2], copy = false, hover = false }
		end
		-- The user's own words, sent or queued: the bar on every row, and a tag
		-- ahead of the first row's text. A placeholder word as wide as the tag
		-- keeps that row short enough, then gives its cells to the tag. A sent
		-- message's rows are sticky: the chat pins its first row while the
		-- turn's rows fill the view, and the bar never hovers there.
		---@param text string
		---@param tag string?
		local function user_rows(text, tag)
			local pad = tag and string.rep("_", #tag + 1) .. " " or ""
			for i, segs in ipairs(zemi.ui.wrap(pad .. text, width - 2)) do
				local row = { { text = "▌ ", hl = "UserMsg", copy = false, hover = false, sticky = not tag or nil } }
				if i == 1 and tag then
					segs[1].text = segs[1].text:sub(#pad + 1)
					row[2] = { text = " " .. tag .. " ", hl = "Queued", copy = false }
				end
				for _, sg in ipairs(segs) do
					sg.hl = "Normal"
					row[#row + 1] = sg
				end
				lines[#lines + 1] = row
			end
		end
		-- Reasoning is a quiet aside: "✻ Thought for 4.2s", the text under a thin
		-- bar with ctrl+o. While the model thinks, its last lines show (tail), so
		-- the wait is not a blank screen.
		---@param label string
		---@param hint string? The expand hint after the label.
		---@param text string
		---@param tail integer? Show only the last this many lines while collapsed.
		---@param spin boolean? Animate the mark with the spinner (live only).
		local function add_reasoning(label, hint, text, tail, spin)
			local head = { mark(spin and "spin" or "thought"), { text = label, hl = "AgentReason" } }
			if hint then head[3] = { text = "  " .. hint, hl = "Comment" } end
			for _, sg in ipairs(head) do
				sg.click = "reasoning"
			end
			lines[#lines + 1] = head
			if not ctx.show_reasoning and not tail then return end
			local body = zemi.ui.wrap((text:gsub("%s+$", "")), math.max(width - 6, 20))
			local first = (ctx.show_reasoning or not tail) and 1 or math.max(1, #body - tail + 1)
			for n = first, #body do
				local row = { { text = "  │ ", hl = "ThinkingBar", copy = false } }
				for _, sg in ipairs(body[n]) do
					sg.hl = "AgentReason"
					row[#row + 1] = sg
				end
				lines[#lines + 1] = row
			end
		end
		---@param secs number
		---@return string
		local function thought(secs) return secs >= 0.05 and "Thought for " .. fmt.duration(secs) or "Thought" end

		-- Tool calls read as what they did: "Read internal/ui/app.go". Finished
		-- calls that only look around collapse into one line ("Read 3 files,
		-- ran 2 commands"); edits, agents, questions and failures keep their
		-- own rows, and ctrl+o shows every call.
		-- a verb, then the singular and plural noun; a verb alone never counts
		---@type table<string, { [1]: string, [2]: string?, [3]: string? }>
		local COUNT = {
			read = { "read", "file", "files" },
			glob = { "searched for", "pattern", "patterns" },
			grep = { "searched for", "pattern", "patterns" },
			web_search = { "searched the web for", "query", "queries" },
			bash = { "ran", "command", "commands" },
			fetch = { "fetched", "page", "pages" },
			skill = { "loaded", "skill", "skills" },
			memory = { "saved", "memory", "memories" },
			diagnostics = { "checked", "file", "files" },
			job_read = { "checked", "job", "jobs" },
			job_list = { "listed jobs" },
			job_kill = { "stopped", "job", "jobs" },
			todos = { "updated todos" },
		}
		---@param t zemi.tui.Tool
		---@return string
		local function tool_name(t) return t.tool or t.name or "?" end
		---@param t zemi.tui.Tool
		---@return boolean
		local function collapsible(t)
			if t.kind ~= nil and t.kind ~= "tool" then return false end
			-- a command that changed files keeps its row: the diff is the news
			return not ctx.show_reasoning and t.status == "ok" and COUNT[tool_name(t)] ~= nil and (t.diff or "") == ""
		end
		---@param s string
		---@return string
		local function relpath(s) return fmt.relpath(s, ctx.cwd, ctx.home) end
		---@param group zemi.tui.Tool[]
		---@param click string The id a click on the summary row carries.
		local function add_group(group, click)
			---@type string[], table<string, { c: { [1]: string, [2]: string?, [3]: string? }, n: integer }>
			local order, seen = {}, {}
			-- each command as the model labelled it, else as it reads
			---@type string[]
			local commands = {}
			for _, t in ipairs(group) do
				local c = COUNT[tool_name(t)]
				local key = c[1] .. "|" .. (c[2] or "")
				if not seen[key] then
					seen[key] = { c = c, n = 0 }
					order[#order + 1] = key
				end
				seen[key].n = seen[key].n + 1
				if tool_name(t) == "bash" then
					commands[#commands + 1] = (t.label or "") ~= "" and fmt.lower(t.label)
						or relpath(t.text or t.input or ""):gsub("%s+", " ")
				end
			end
			---@type string[]
			local parts = {}
			---@type string?
			local ran
			for _, key in ipairs(order) do
				local c, n = seen[key].c, seen[key].n
				local part = c[2] and c[1] .. " " .. fmt.count(n, c[2], c[3]) or c[1]
				if c == COUNT.bash then
					-- last, so the commands read as its list
					ran = part .. ": " .. table.concat(commands, ", ")
				else
					parts[#parts + 1] = part
				end
			end
			parts[#parts + 1] = ran
			local summary = table.concat(parts, ", ")
			summary = summary:sub(1, 1):upper() .. summary:sub(2)
			local ok = mark("ok")
			ok.click = click
			lines[#lines + 1] = { ok, { text = fmt.clip(summary, width - 4), hl = "Normal", click = click } }
		end
		-- How a finished row's status marks it. A call running or not started
		-- yet spins, unless the open prompt asks about it: that one waits on the
		-- user, so it neither spins nor counts.
		---@type table<string, string>
		local MARK_OF = { ok = "ok", err = "err", stopped = "stop", bg = "bg" }
		---@param t zemi.tui.Tool
		---@param idx integer Transcript index, negative for a live-turn call.
		local function add_tool(t, idx)
			local name = tool_name(t)
			local open = ctx.show_reasoning or expanded[idx] or false
			-- NOTE: every running row reads as asked about while a prompt is open,
			-- a parallel call that runs on meanwhile included.
			local asked = t.status == "run" and (ctx.waiting or "") ~= ""
			local input = relpath(t.text or t.input or "")
			-- a collapsed row is one line; an open one keeps a heredoc's lines
			---@type string[]
			local more = {}
			local body = input
			if open then
				body = input:gsub("%s+$", "")
				more = util.split(body, "\n")
				input = table.remove(more, 1)
			else
				input = (input:gsub("%s+", " "))
			end
			local label = t.label or ""
			-- expanded rows wrap (the widget wraps), collapsed ones stay one line
			---@param text string
			---@param room integer
			---@return string
			local function fit(text, room) return open and text or fmt.clip(text, room) end
			local verb, rest = fmt.head(name, label)
			if label == "" and input ~= "" then rest = " " .. input end
			-- a command is highlighted where it is what the row reads, the head and
			-- an opened row; collapsed under its label it stays a Comment detail
			local code = name == "bash"
					and input ~= ""
					and (open or label == "")
					and zemi.ui.highlight(open and body or fit(input, width - fmt.cells(verb) - 13), "bash")
				or nil
			-- an agent's rows open its transcript and a workflow's its view (ids must
			-- differ between adjacent rows), every other row (file tools included)
			-- expands the call
			local click = (t.run or "") ~= "" and ((name == "workflow" and "workflow:" or "agent:") .. t.run)
				or ("tool:" .. idx)
			local head =
				{ mark(asked and "wait" or MARK_OF[t.status] or "spin"), { text = verb, hl = "AgentToolName" } }
			if code and label == "" then
				head[3] = { text = " ", hl = "Normal" }
				for _, sg in ipairs(code[1] or {}) do
					head[#head + 1] = sg
				end
			elseif rest ~= "" then
				head[3] = { text = fit(rest, width - fmt.cells(verb) - 12), hl = "Normal" }
			end
			if t.status == "run" and not asked and (t.elapsed or 0) >= 1 then
				head[#head + 1] = { text = " · " .. fmt.duration(t.elapsed, true), hl = "Comment" }
			end
			for _, sg in ipairs(head) do
				sg.click = click
			end
			lines[#lines + 1] = head
			-- under the head one corner, every later row four cells in
			local corner = true
			---@param text string|zemi.Line
			---@param hl string
			local function detail(text, hl)
				local row = { { text = corner and "  ⎿ " or "    ", hl = "Comment", copy = false } }
				for _, sg in ipairs(type(text) == "table" and text or { { text = text, hl = hl } }) do
					row[#row + 1] = { text = sg.text, hl = sg.hl, click = click, hover = false }
				end
				lines[#lines + 1] = row
				corner = false
			end
			-- an answered question's output holds "question\tanswer" lines, which
			-- take the place of the question under its header
			local output = t.output or ""
			local answered = name == "question" and output:find("\t") ~= nil
			-- the command a description stands for, then progress, then the job
			-- a timed-out command went on as (the phrase is tools.JobHandoff)
			if answered then
				more = {}
			elseif label ~= "" and input ~= "" then
				detail(code and code[1] or fit(input, width - 6), "Comment")
			end
			for i, l in ipairs(more) do
				detail(code and code[i + 1] or l, "Comment")
			end
			if (t.detail or "") ~= "" then detail(fit((t.detail:gsub("%s+", " ")), width - 6), "Comment") end
			local job = t.status == "bg"
				and (
					(t.output or ""):match("moved to the background as (job%-%d+)")
					or (t.output or ""):match("^started (job%-%d+)")
				)
			if job then detail("running as " .. job, "Comment") end
			-- File tools show what they changed as numbered, tinted rows, four
			-- cells in; a diff row is body: no hover, but the click expands like the head
			for _, row in ipairs(zemi.ui.diff(t.diff or "", width - 4)) do
				for _, sg in ipairs(row) do
					sg.click, sg.hover = click, false
				end
				table.insert(row, 1, { text = "    ", copy = false, click = click, hover = false })
				lines[#lines + 1] = row
			end
			-- the result's last lines: always for a failure, for what the user
			-- answered (question, plan), for commands with ctrl+o
			-- what the user just decided needs no restating: the model's text
			-- carries the reason and the instructions, the row the verdict
			if output:match("^permission denied %(user") then output = "denied by you" end
			if output:match("^The user dismissed") then output = "dismissed by you" end
			if
				output ~= ""
				and (
					t.status == "err"
					or t.status == "stopped"
					or name == "question"
					or name == "plan"
					or expanded[idx]
					or (ctx.show_reasoning and name == "bash")
				)
			then
				local out = util.split(output, "\n")
				-- a collapsed failure shows three lines where it says what went
				-- wrong: a tool's first, a command's last (its stderr and exit
				-- status); a click shows the rest
				local err, tail = t.status == "err", name == "bash"
				local first, last = 1, #out
				if err and not open and #out > 3 then
					if tail then
						first = #out - 2
					else
						last = 3
					end
				end
				-- the host's own cut stands for the lines it dropped
				local hidden = 0
				for i = 1, #out do
					local n = out[i]:match("^… (%d+) more lines$")
					if i < first or i > last then hidden = hidden + (n and tonumber(n) or 1) end
				end
				local more = "… " .. fmt.count(hidden, "more line", "more lines")
				if hidden > 0 and first > 1 then detail(more, "Comment") end
				for i = first, last do
					local q, a = out[i]:match("^(.-)\t(.*)$")
					if answered and q then
						if fmt.cells(q) + 2 + fmt.cells(a) <= width - 4 then
							detail({ { text = q, hl = "Comment" }, { text = "  " .. a, hl = "Normal" } }, "Comment")
						else
							detail(q, "Comment")
							detail(a, "Normal")
						end
					else
						detail(out[i], err and i == (tail and #out or 1) and "Danger" or "Comment")
					end
				end
				if hidden > 0 and first == 1 then detail(more, "Comment") end
			end
		end
		-- rows from i on: a collapsed group or one call; returns the next index
		---@param list zemi.tui.Tool[]
		---@param i integer
		---@param live boolean? The live turn's calls, indexed apart from the transcript.
		---@return integer
		local function add_tools(list, i, live)
			local idx = live and -i or i
			if collapsible(list[i]) then
				---@type zemi.tui.Tool[]
				local group = {}
				while i <= #list and collapsible(list[i]) do
					group[#group + 1] = list[i]
					i = i + 1
				end
				-- one click id for the whole group: it opens every row of it and
				-- a click on any of those closes it again
				if expanded[idx] then
					for _, t in ipairs(group) do
						add_tool(t, idx)
					end
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
		if epoch ~= expanded_epoch then
			expanded, expanded_epoch = {}, epoch
		end
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
		---@return zemi.tui.ChatUnit
		local function unit(tr, first, last, hinted)
			local m = tr[first]
			lines, prev = {}, first > 1 and "above" or nil
			if m.kind == "reasoning" then
				gap("activity")
				add_reasoning(
					thought(m.thought or 0),
					(hinted and not ctx.show_reasoning and not ctx.headless) and HINT or nil,
					m.text
				)
			elseif m.kind == "tool" then
				local i = first
				while i <= last do
					gap("activity")
					i = add_tools(tr, i)
				end
			elseif m.kind == "shell" then
				-- a !command: the command, then its output, already cut to a head
				-- and a tail by the host; a failure's mark takes the prompt's place
				gap("user")
				lines[#lines + 1] = {
					m.status == "err" and mark("err") or { text = "$ ", hl = "Title", copy = false },
					{ text = m.text, hl = "Normal" },
				}
				for _, ol in ipairs(util.split(m.detail or "", "\n")) do
					lines[#lines + 1] = { { text = "  ", copy = false }, { text = ol, hl = "Comment" } }
				end
			elseif m.kind == "compact" or m.kind == "notice" then
				gap("activity")
				-- a report's notice heads the rows that answer it, as a prompt does
				local row = { mark("note"), { text = m.text, hl = "Comment" } }
				row[1].sticky = m.head or nil
				if (m.detail or "") ~= "" then
					row[3] = { text = " (" .. m.detail .. ")", hl = "Comment", copy = false }
				end
				lines[#lines + 1] = row
			elseif m.kind == "error" then
				-- the headline, then the raw error as its detail
				gap("activity")
				local cancelled = m.status == "cancelled"
				for n, text in ipairs(util.split(m.text, "\n")) do
					if n == 1 then
						lines[#lines + 1] = {
							mark(cancelled and "stop" or "err"),
							{ text = text, hl = cancelled and "Comment" or "Danger" },
						}
					else
						lines[#lines + 1] = {
							{ text = n == 2 and "  ⎿ " or "    ", hl = "Comment", copy = false },
							{ text = text, hl = "Comment" },
						}
					end
				end
			elseif m.role == "user" then
				-- plain text: what the user typed is not markdown
				gap("user")
				user_rows(m.text)
			else
				gap("text")
				add_markdown(m.text)
				---@type string[]
				local parts = {}
				local tout = fmt.tokens(m.out)
				if (m.elapsed or 0) > 0 then parts[#parts + 1] = fmt.duration(m.elapsed) end
				if tout then parts[#parts + 1] = tout .. " out" end
				if (m.ended or 0) > 0 then parts[#parts + 1] = os.date("%H:%M", m.ended) end
				if #parts > 0 then
					gap("stats")
					lines[#lines + 1] =
						{ { text = "  ", copy = false }, { text = table.concat(parts, " · "), hl = "Comment" } }
				end
			end
			return { lines = lines, next = last + 1, hint = hinted }
		end

		-- markdown and code carry the theme's colors: a light terminal's
		-- variant renders them anew
		local key =
			table.concat({ epoch, width, tostring(ctx.show_reasoning), ctx.cwd or "", zemi.ui.theme.get() }, "|")
		if key ~= units_key or epoch == 0 then
			units, units_key, settled = {}, key, nil
		end
		-- A long session resumed, a new width or ctrl+o leave every unit to
		-- render, far more than one render may take: the latest come first, and
		-- past the budget the render returns them and asks for another.
		local more = false
		local ag = ctx.agent or {}
		-- Only the latest collapsed reasoning line carries the expand hint: the
		-- live turn's, while it has one.
		local live_reason = (ag.reasoning or "") ~= ""
		local done = settled and settled.count == count and settled.live_reason == live_reason and settled
		if not done then
			-- ctx.transcript copies every row into Lua, so only a miss reads it
			local tr = ctx.transcript or {}
			local last_reason = 0
			for n, m in ipairs(tr) do
				if m.kind == "reasoning" and not live_reason then last_reason = n end
			end
			local deadline = epoch ~= 0 and os.clock() + 0.03
			---@type zemi.tui.ChatUnit[]
			local shown = {}
			local rendered = 0
			local last = #tr
			while last >= 1 do
				local first = last
				while first > 1 and tr[first].kind == "tool" and tr[first - 1].kind == "tool" do
					first = first - 1
				end
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
				for _, l in ipairs(shown[n].lines) do
					all[#all + 1] = l
				end
			end
			done = { count = count, lines = all, n = #all, live_reason = live_reason }
			settled = not more and done or nil
		end
		-- the live turn's lines go after the settled ones in the same list
		lines, prev = done.lines, count > 0 and "settled" or nil
		for n = #lines, done.n + 1, -1 do
			lines[n] = nil
		end

		-- Live turn (§6.3) from the host-owned ctx.agent snapshot, drawn as the
		-- settled rows will be: the reasoning, the calls, the reply.
		local tools, text, queued = ag.tools or {}, ag.text or "", ag.steering or {}
		if ag.active or #tools > 0 or text ~= "" or #queued > 0 then
			if live_reason then
				gap("activity")
				-- the tail while the model thinks; once a reply or a call starts,
				-- the head the settled row draws
				if text == "" and #tools == 0 then
					add_reasoning("Thinking", nil, ag.reasoning, 3, true)
				else
					add_reasoning(
						thought(ag.thought or 0),
						not ctx.show_reasoning and not ctx.headless and HINT or nil,
						ag.reasoning
					)
				end
			end
			local ti = 1
			while ti <= #tools do
				gap("activity")
				ti = add_tools(tools, ti, true)
			end
			if text ~= "" then
				gap("text")
				add_markdown(text)
			end
			-- queued messages are the user's own words waiting their turn, apart
			-- from the agent's rows above
			for _, q in ipairs(queued) do
				gap("queued")
				user_rows(q, "queued")
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
			local head = {
				{ text = "Todos ", hl = "TodoTitle" },
				{ text = (#todos - left) .. "/" .. #todos .. "  ", hl = "Comment" },
			}
			for n, td in ipairs(todos) do
				if n > 12 then break end
				head[#head + 1] = {
					text = "━",
					hl = td.status == "done" and "TodoDone" or td.status == "in_progress" and "TodoMark" or "Rule",
					copy = false,
				}
			end
			lines[#lines + 1] = head
			-- at most 12 rows and a third of the chat, the live ones first: in
			-- progress, then pending, then done, so a long list never hides the
			-- item being worked on
			local cap = math.min(12, math.max(3, math.floor((ctx.height or 36) / 3)))
			local rank = { in_progress = 1, pending = 2, done = 3 }
			---@type { td: zemi.Todo, n: integer }[]
			local shown = {}
			for n, td in ipairs(todos) do
				shown[#shown + 1] = { td = td, n = n }
			end
			table.sort(shown, function(a, b)
				local ra, rb = rank[a.td.status] or 2, rank[b.td.status] or 2
				if ra ~= rb then return ra < rb end
				return a.n < b.n
			end)
			for n, it in ipairs(shown) do
				local td = it.td
				if n > cap then
					local hidden, done = #shown - cap, 0
					for i = cap + 1, #shown do
						if shown[i].td.status == "done" then done = done + 1 end
					end
					if done == hidden then
						lines[#lines + 1] = {
							{ text = "  ✓ ", hl = "TodoDone", copy = false },
							{ text = done .. " done", hl = "Comment" },
						}
					else
						lines[#lines + 1] = {
							{ text = "  … ", hl = "Comment", copy = false },
							{ text = fmt.count(hidden, "more todo", "more todos"), hl = "Comment" },
						}
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
