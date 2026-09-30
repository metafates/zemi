-- The chat widget: the transcript, the live turn and the todo list. A turn's
-- activity (thinking and tool calls) reads as a rail while it runs and folds
-- into one digest row once settled; tui/thinking.lua draws the same rows for
-- a turn it inspects.
local fmt = require("zemi.tui.text")
local util = require("zemi.util")

-- A tool row: a settled transcript row or a call of the live turn.
---@alias zemi.tui.Tool zemi.TranscriptRow|zemi.ToolCall

-- How a run of activity rows draws: its digest and the rows that keep their
-- own (settled), a rail of one row per step (the live turn, a printed run, an
-- agent run's view), or the rail with every row open (an unfolded digest, the
-- pane's inspector, zemi run --reasoning).
---@alias zemi.tui.RunMode "fold"|"rail"|"open"

-- The settled transcript is rendered in units, a message or a run of
-- activity rows, kept across frames by the index of their first message:
-- while a reply streams the widget renders many times a second and only the
-- live turn changes. `settled` is the units' lines in order, which a render
-- hands back as they are while the transcript has `count` rows.
---@class zemi.tui.ChatUnit
---@field lines zemi.Line[]
---@field next integer Index of the message after the unit.
---@field mode zemi.tui.RunMode|"" How its activity run drew; "" for any other unit.
---@type table<integer, zemi.tui.ChatUnit>
local units = {}
local units_key = ""
---@type { count: integer, lines: zemi.Line[], n: integer, active: boolean, block: string? }?
local settled

-- Rows the user opened, by transcript index (negative for the live turn's
-- calls), and digests, by their run's first index: a click shows that one
-- row or run whole and leaves the rest of the transcript alone. Cleared when
-- the transcript is replaced, and the negative half when a new turn starts,
-- since the indexes are then someone else's.
---@type table<integer, true>
local expanded = {}
---@type table<integer, true>
local unfolded = {}
local expanded_epoch = 0
local live_tools = 0

-- What the thinking pane shares with the chat.
---@class zemi.tui.chat
---@field inspected integer? The run the thinking pane shows, by its first transcript index; nil for the live turn.
---@field pane boolean Whether the last chat render saw the thinking pane drawn.
local M = { pane = false }

-- How a finished row's status marks it. A call running or not started yet
-- spins, unless the open prompt asks about it: that one waits on the user,
-- so it neither spins nor counts.
---@type table<string, string>
local MARK_OF = { ok = "ok", err = "err", stopped = "stop", bg = "bg" }

-- A row's mark: the spinner while it runs, else how it ended. A mark never
-- hovers: a hovered header changes only its text.
---@param kind string A zemi.ui.marks key.
---@param click string?
---@return zemi.Segment
local function mark(kind, click)
	local m = zemi.ui.marks[kind]
	local effect = kind == "spin" and "spin" or nil
	return { text = m.glyph .. " ", hl = m.hl, copy = false, hover = false, effect = effect, click = click }
end

-- The rail a turn's activity stands on while it runs.
---@param pad string?
---@return zemi.Segment
local function bar(pad) return { text = "│ " .. (pad or ""), hl = "ThinkingBar", copy = false, hover = false } end

--- Whether a row is thinking or a call: a run of them is one unit, a
--- digest once settled.
---@param m zemi.TranscriptRow
---@return boolean
function M.activity(m) return m.kind == "reasoning" or m.kind == "tool" end
local activity = M.activity

-- The block a row belongs to: blocks are one blank row apart, activity rows
-- stand together.
---@param m zemi.TranscriptRow
---@return "user"|"activity"|"text"|"note"
local function block_of(m)
	local k = m.kind or ""
	if k == "shell" or k == "" and m.role == "user" then return "user" end
	if k == "" then return "text" end
	return activity(m) and "activity" or "note"
end

-- "+4 -1": the lines a diff adds and removes.
---@param diff string
---@return string
local function diffstat(diff)
	local add, del = 0, 0
	for l in diff:gmatch("[^\n]+") do
		if l:match("^%+") and not l:match("^%+%+%+ ") then
			add = add + 1
		elseif l:match("^%-") and not l:match("^%-%-%- ") then
			del = del + 1
		end
	end
	return "+" .. add .. " -" .. del
end

-- The row builders one render uses, drawing at width into B.lines.
---@param ctx zemi.WidgetCtx
---@param width integer
local function builder(ctx, width)
	---@class zemi.tui.ChatBuilder
	---@field lines zemi.Line[]
	---@field prev string? The block the last row belongs to.
	local B = { lines = {} }
	---@param row zemi.Line
	local function push(row) B.lines[#B.lines + 1] = row end
	function B.blank() push({ { text = "" } }) end
	---@param block "user"|"activity"|"text"|"note"|"stats"|"queued"
	function B.gap(block)
		if B.prev and not (B.prev == "activity" and block == "activity") then B.blank() end
		B.prev = block
	end
	---@param s string
	---@return string
	local function relpath(s) return fmt.relpath(s, ctx.cwd, ctx.home) end
	-- row ending in suffix, flush with the right edge
	---@param row zemi.Line
	---@param suffix string
	---@param click string?
	---@return zemi.Line
	local function suffixed(row, suffix, click)
		if suffix == "" then return row end
		local w = 0
		for _, sg in ipairs(row) do
			w = w + zemi.ui.width(sg.text)
		end
		local sw = zemi.ui.width(suffix)
		row[#row + 1] = { text = string.rep(" ", math.max(2, width - w - sw)) }
		row[#row + 1] = { text = suffix, hl = "Dim", click = click }
		return row
	end

	-- A reply is indented, not bulleted: its lines share the user's left edge.
	---@param text string
	function B.markdown(text)
		for _, segs in ipairs(zemi.ui.markdown(text, width - 2)) do
			local row = { { text = "  ", hl = "Normal", copy = false } }
			for _, sg in ipairs(segs) do
				row[#row + 1] = sg
			end
			push(row)
		end
	end
	-- The user's own words, sent or queued: the bar on every row, and a tag
	-- ahead of the first row's text. A placeholder word as wide as the tag
	-- keeps that row short enough, then gives its cells to the tag. A sent
	-- message's rows are sticky: the chat pins its first row while the
	-- turn's rows fill the view, and the bar never hovers there.
	---@param text string
	---@param tag string?
	function B.user(text, tag)
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
			push(row)
		end
	end

	-- Thinking text on the rail, its last tail lines when tail is set.
	---@param text string
	---@param tail integer?
	function B.reasoning(text, tail)
		local body = zemi.ui.wrap((text:gsub("%s+$", "")), math.max(width - 4, 20))
		for n = tail and math.max(1, #body - tail + 1) or 1, #body do
			local row = { bar("  ") }
			for _, sg in ipairs(body[n]) do
				sg.hl = "AgentReason"
				row[#row + 1] = sg
			end
			push(row)
		end
	end
	-- "✻ thought 8s", with the text under it when given.
	---@param secs number
	---@param click string
	---@param text string?
	function B.thought(secs, click, text)
		push({ mark("thought", click), { text = fmt.thought(secs), hl = "Dim", click = click } })
		if text then B.reasoning(text) end
	end
	-- The model thinking now; a click opens or closes the thinking pane.
	---@param tail string? The thinking so far, whose last lines show under it.
	function B.thinking(tail)
		push({ mark("spin", "reasoning"), { text = "thinking", hl = "Dim", click = "reasoning" } })
		if tail then B.reasoning(tail, 4) end
	end

	-- What look-around calls did: "read 2 files, ran 1 command: go vet".
	---@param group zemi.tui.Tool[]
	---@return string
	local function said(group)
		---@type string[], table<string, { c: { [1]: string, [2]: string?, [3]: string? }, n: integer }>
		local order, seen = {}, {}
		---@type string[]
		local commands = {}
		for _, t in ipairs(group) do
			local c = fmt.COUNT[fmt.tool(t)]
			local key = c[1] .. "|" .. (c[2] or "")
			if not seen[key] then
				seen[key] = { c = c, n = 0 }
				order[#order + 1] = key
			end
			seen[key].n = seen[key].n + 1
			if fmt.tool(t) == "shell" then
				local cmd = relpath(t.text or t.input or ""):gsub("%s+", " ")
				commands[#commands + 1] = cmd
			end
		end
		---@type string[]
		local parts = {}
		---@type string?
		local ran
		for _, key in ipairs(order) do
			local c, n = seen[key].c, seen[key].n
			local part = c[2] and c[1] .. " " .. fmt.count(n, c[2], c[3]) or c[1]
			if c == fmt.COUNT.shell then
				-- last, so the commands read as its list
				ran = part .. ": " .. table.concat(commands, ", ")
			else
				parts[#parts + 1] = part
			end
		end
		parts[#parts + 1] = ran
		return table.concat(parts, ", ")
	end
	-- A step's look-around calls as one rail row: the first label the model
	-- gave one with the counts after it, else what they did.
	---@param group zemi.tui.Tool[]
	---@param click string
	---@param secs number? The thought before the step.
	function B.group(group, click, secs)
		---@type string?
		local label
		for _, t in ipairs(group) do
			if (t.label or "") ~= "" then
				label = fmt.lower(t.label)
				break
			end
		end
		---@type string[]
		local parts = {}
		if label then parts[1] = fmt.counts(group, true) end
		if secs and secs >= 0.05 then parts[#parts + 1] = fmt.duration(secs) end
		local suffix = table.concat(parts, " · ")
		local room = width - 2 - (suffix ~= "" and zemi.ui.width(suffix) + 2 or 0)
		push(
			suffixed(
				{ bar(), { text = fmt.clip(label or said(group), room), hl = "Normal", click = click } },
				suffix,
				click
			)
		)
	end

	-- One call as its own row: the verb and the label or input, what it
	-- changed or how long it runs at the right, and under it what it said.
	-- Open, a row shows the whole command, the diff and a command's output.
	---@param t zemi.tui.Tool
	---@param idx integer Transcript index, negative for a live-turn call.
	---@param o { open: boolean?, rail: boolean?, secs: number?, click: string? }
	function B.tool(t, idx, o)
		local name = fmt.tool(t)
		local open = o.open or expanded[idx] or false
		-- NOTE: every running row reads as asked about while a prompt is open,
		-- a parallel call that runs on meanwhile included.
		local asked = t.status == "run" and (ctx.waiting or "") ~= ""
		-- a finished row stands on the rail; a running one leads with its spinner
		local barred = o.rail and MARK_OF[t.status] ~= nil
		local inner = width - (barred and 2 or 0)
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
		---@type string[]
		local parts = {}
		if (t.diff or "") ~= "" then parts[1] = diffstat(t.diff) end
		if t.status == "run" and not asked and (t.elapsed or 0) >= 1 then
			parts[#parts + 1] = fmt.duration(t.elapsed, true)
		end
		-- the thought before the call, once its own clock stops
		if o.secs and o.secs >= 0.05 and t.status ~= "run" then parts[#parts + 1] = fmt.duration(o.secs) end
		local suffix = table.concat(parts, " · ")
		local verb, rest = fmt.head(name, label, input)
		verb = fmt.lower(verb)
		local room = inner - 2 - zemi.ui.width(verb) - (suffix ~= "" and zemi.ui.width(suffix) + 2 or 1)
		-- expanded rows wrap (the widget wraps), collapsed ones stay one line
		---@param text string
		---@param n integer
		---@return string
		local function fit(text, n) return open and text or fmt.clip(text, n) end
		-- a command is highlighted where it is what the row reads, the head and
		-- an opened row; collapsed under its label it stays a Dim detail
		local code = name == "shell"
				and input ~= ""
				and (open or label == "")
				and zemi.ui.highlight(open and body or fit(input, room - 1), "bash")
			or nil
		-- an agent's rows open its transcript and a workflow's its view (ids must
		-- differ between adjacent rows), every other row expands the call
		local click = (t.run or "") ~= "" and ((name == "workflow" and "workflow:" or "agent:") .. t.run)
			or o.click
			or ("tool:" .. idx)
		local head = { mark(asked and "wait" or MARK_OF[t.status] or "spin"), { text = verb, hl = "AgentToolName" } }
		if code and label == "" then
			head[3] = { text = " ", hl = "Normal" }
			for _, sg in ipairs(code[1] or {}) do
				head[#head + 1] = sg
			end
		elseif rest ~= "" then
			head[3] = { text = fit(rest, room), hl = "Normal" }
		end
		for _, sg in ipairs(head) do
			sg.click = click
		end
		-- on the rail an ok look-around call has no mark, as its step's row
		if barred and fmt.looks(t) then
			head[1] = bar()
		elseif barred then
			table.insert(head, 1, bar())
		end
		local first = #B.lines + 1
		push(suffixed(head, suffix, click))
		-- under the head one corner, every later row four cells in
		local corner = true
		---@param text string|zemi.Line
		---@param hl string
		local function detail(text, hl)
			local row = { { text = corner and "  ⎿ " or "    ", hl = "Dim", copy = false } }
			for _, sg in ipairs(type(text) == "table" and text or { { text = text, hl = hl } }) do
				row[#row + 1] = { text = sg.text, hl = sg.hl, click = click, hover = false }
			end
			push(row)
			corner = false
		end
		-- an answered question's output holds "question\tanswer\tnote" lines,
		-- which take the place of the question under its header
		local output = t.output or ""
		local answered = name == "question" and output:find("\t") ~= nil
		-- the command a description stands for, then progress, then the job
		-- a timed-out command went on as (the phrase is tools.JobHandoff)
		if answered then
			more = {}
		elseif label ~= "" and input ~= "" then
			detail(code and code[1] or fit(input, inner - 6), "Dim")
		end
		for i, l in ipairs(more) do
			detail(code and code[i + 1] or l, "Dim")
		end
		if (t.detail or "") ~= "" then detail(fit((t.detail:gsub("%s+", " ")), inner - 6), "Dim") end
		local job = t.status == "bg"
			and (
				(t.output or ""):match("moved to the background as (job%-%d+)")
				or (t.output or ""):match("^started (job%-%d+)")
			)
		if job then detail("running as " .. job, "Dim") end
		-- an open file tool shows what it changed as numbered, tinted rows, four
		-- cells in; a diff row is body: no hover, but the click folds like the head
		if open then
			for _, row in ipairs(zemi.ui.diff(t.diff or "", inner - 4)) do
				for _, sg in ipairs(row) do
					sg.click, sg.hover = click, false
				end
				table.insert(row, 1, { text = "    ", copy = false, click = click, hover = false })
				push(row)
			end
		end
		-- the result's last lines: always for a failure, for what the user
		-- answered (question, plan), for an opened row, for a command of an
		-- open run; what the user just decided needs no restating: the model's
		-- text carries the reason and the instructions, the row the verdict
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
				or (o.open and name == "shell")
			)
		then
			local out = util.split(output, "\n")
			-- a collapsed failure shows three lines where it says what went
			-- wrong: a tool's first, a command's last (its stderr and exit
			-- status); a click shows the rest
			local err, tail = t.status == "err", name == "shell"
			local from, to = 1, #out
			if err and not open and #out > 3 then
				if tail then
					from = #out - 2
				else
					to = 3
				end
			end
			-- the host's own cut stands for the lines it dropped
			local hidden = 0
			for i = 1, #out do
				local n = out[i]:match("^… (%d+) more lines$")
				if i < from or i > to then hidden = hidden + (n and tonumber(n) or 1) end
			end
			local cut = "… " .. fmt.count(hidden, "more line", "more lines")
			if hidden > 0 and from > 1 then detail(cut, "Dim") end
			for i = from, to do
				local q, a, note = out[i]:match("^([^\t]*)\t([^\t]*)\t(.*)$")
				if answered and q then
					if zemi.ui.width(q) + 2 + zemi.ui.width(a) <= inner - 4 then
						detail({ { text = q, hl = "Dim" }, { text = "  " .. a, hl = "Normal" } }, "Dim")
					else
						detail(q, "Dim")
						detail(a, "Normal")
					end
					if note ~= "" then detail("note: " .. note, "Dim") end
				else
					detail(out[i], err and i == (tail and #out or 1) and "Danger" or "Dim")
				end
			end
			if hidden > 0 and from == 1 then detail(cut, "Dim") end
		end
		if barred then
			for n = first + 1, #B.lines do
				table.insert(B.lines[n], 1, bar())
			end
		end
	end

	-- A settled run of activity rows: the digest of what folds (thinking and
	-- the calls that only looked around), then the rows that keep their own.
	---@param list zemi.TranscriptRow[]
	---@param first integer
	---@param last integer
	function B.fold(list, first, last)
		local digest = fmt.digest(list, first, last)
		if digest ~= "" then
			local click = "turn:" .. first
			push({
				{ text = "· ", hl = "Dim", copy = false, hover = false, click = click },
				{ text = fmt.clip(digest, width - 2), hl = "Dim", click = click },
			})
		end
		for i = first, last do
			if list[i].kind ~= "reasoning" and not fmt.looks(list[i]) then B.tool(list[i], i, {}) end
		end
	end

	-- A run of activity rows as a rail, one row per step: a step's
	-- look-around calls as one row carrying the thought before it, a thought
	-- over 5 s as its own row, every other call as its own row.
	-- NOTE: a step is the calls between two thoughts, so a model that does not
	-- think runs its steps together into one row.
	---@param list zemi.tui.Tool[]
	---@param first integer
	---@param last integer
	---@param o { open: boolean?, live: boolean?, run: integer?, secs: number? } run: the settled run's first index, whose digest a click folds back.
	function B.rail(list, first, last, o)
		local secs = o.secs
		local fold = o.open and o.run and "turn:" .. o.run or nil
		-- with the thinking pane drawn, a settled run's step or thought shows
		-- the run there
		local inspect = M.pane and o.run and "turn:" .. o.run or nil
		local i = first
		while i <= last do
			local t = list[i]
			local idx = o.live and -i or i
			if t.kind == "reasoning" then
				local s = t.thought or 0
				if o.open or s > 5 or i == last or list[i + 1].kind == "reasoning" then
					B.thought(s, fold or inspect or ("thought:" .. idx), (o.open or expanded[idx]) and t.text or nil)
					secs = nil
				else
					secs = s
				end
				i = i + 1
			elseif fmt.looks(t) then
				---@type zemi.tui.Tool[]
				local group = {}
				while i + #group <= last and fmt.looks(list[i + #group]) do
					group[#group + 1] = list[i + #group]
				end
				-- one click id for the whole step: it opens every call of it and a
				-- click on any of those closes it again
				if o.open or expanded[idx] then
					for k, g in ipairs(group) do
						local n = i + k - 1
						B.tool(g, o.live and -n or n, {
							open = true,
							rail = true,
							secs = k == 1 and secs or nil,
							click = fold or inspect or ("step:" .. idx),
						})
					end
				else
					B.group(group, inspect or ("step:" .. idx), secs)
				end
				secs = nil
				i = i + #group
			else
				B.tool(t, idx, { open = o.open, rail = true, secs = secs, click = fold })
				secs = nil
				i = i + 1
			end
		end
	end
	return B
end

--- Show a run in the thinking pane, by its first transcript index; the same
--- run again, or nil, goes back to the live turn. The pane follows the tail
--- either way: a short region cannot pin its top before the rows arrive.
---@param first integer?
function M.inspect(first)
	if first == M.inspected then first = nil end
	M.inspected = first
	zemi.ui.scroll("bottom", "thinking")
end

--- The last row of the activity run that starts at rows[first].
---@param rows zemi.TranscriptRow[]
---@param first integer
---@return integer
function M.last(rows, first)
	local last = first
	while last < #rows and activity(rows[last + 1]) do
		last = last + 1
	end
	return last
end

--- The rows of the activity run that starts at rows[first], every one open:
--- the thinking pane's inspector. A click on one goes back to the live turn.
---@param ctx zemi.WidgetCtx
---@param rows zemi.TranscriptRow[]
---@param first integer
---@param width integer
---@return zemi.Line[]
function M.run(ctx, rows, first, width)
	local B = builder(ctx, width)
	B.rail(rows, first, M.last(rows, first), { open = true, run = first })
	return B.lines
end

--- A click on the chat's rows, or on the thinking pane's, which draws the
--- same ones: on a digest it unfolds the run (with the thinking pane drawn,
--- shows it there), on a step or a thought row opens it, on an agent's row
--- shows that run, on a workflow's its view, on any other call's row expands
--- that call, and on the live thinking row opens or closes the thinking pane.
---@param ev zemi.WidgetClick
function M.click(ev)
	local run, flow = ev.id:match("^agent:(.+)"), ev.id:match("^workflow:(.+)")
	if run then
		assert(zemi.command.run("agents", "view " .. run))
	elseif flow then
		assert(zemi.command.run("workflows", "view " .. flow))
	elseif ev.id == "reasoning" then
		zemi.ui.reasoning.toggle()
	else
		local kind, id = ev.id:match("^(%a+):(-?%d+)$")
		local n = tonumber(id)
		if not n then return end
		-- with the pane, a digest shows its run there, and a live step or
		-- thought the live turn; an unfolded digest folds back in place
		if M.pane and kind ~= "tool" and not (kind == "turn" and unfolded[n]) then
			return M.inspect(kind == "turn" and n or nil)
		end
		local set = kind == "turn" and unfolded or expanded
		set[n] = not set[n] or nil
		for first, u in pairs(units) do
			if first <= n and n < u.next then units[first] = nil end
		end
		settled = nil
	end
end

-- Chat transcript. User rows carry an accent bar; a turn's activity, its
-- markdown reply and stats follow, a blank row apart.
zemi.ui.widget.add({
	id = "chat",
	region = "chat",
	priority = 10,
	wrap = true,
	on_click = M.click,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	---@return boolean more
	render = function(ctx)
		local width = ctx.width or 80
		M.pane = (ctx.regions or {}).thinking or false
		local count = ctx.transcript_count or 0
		if count == 0 and not ctx.agent_active then
			return {} -- the splash widget fills the empty chat
		end
		-- An agent run's transcript mutates in place and reports epoch 0, so
		-- only the session's settled rows are cached.
		local epoch = ctx.transcript_epoch or 0
		if epoch ~= expanded_epoch then
			expanded, unfolded, expanded_epoch, M.inspected = {}, {}, epoch, nil
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
		local active = ctx.agent_active or false
		local B = builder(ctx, width)
		-- The messages first..last as one unit: a run of activity rows, else one.
		---@param tr zemi.TranscriptRow[]
		---@param first integer
		---@param last integer
		---@param mode zemi.tui.RunMode|""
		---@return zemi.tui.ChatUnit
		local function unit(tr, first, last, mode)
			local m = tr[first]
			B.lines, B.prev = {}, first > 1 and block_of(tr[first - 1]) or nil
			if mode == "fold" then
				-- a run with nothing to say (a thought under a second) draws no row
				-- and so takes no gap
				local n = #B.lines
				B.gap("activity")
				local g = #B.lines
				B.fold(tr, first, last)
				if #B.lines == g then
					for i = g, n + 1, -1 do
						B.lines[i] = nil
					end
				end
			elseif mode ~= "" then
				B.gap("activity")
				B.rail(tr, first, last, { open = mode == "open", run = first })
			elseif m.kind == "shell" then
				-- a !command: the command, then its output, already cut to a head
				-- and a tail by the host; a failure's mark takes the prompt's place
				B.gap("user")
				B.lines[#B.lines + 1] = {
					m.status == "err" and mark("err") or { text = "$ ", hl = "Title", copy = false },
					{ text = m.text, hl = "Normal" },
				}
				for _, ol in ipairs(util.split(m.detail or "", "\n")) do
					B.lines[#B.lines + 1] = { { text = "  ", copy = false }, { text = ol, hl = "Dim" } }
				end
			elseif m.kind == "compact" or m.kind == "notice" then
				B.gap("note")
				local row = { mark("note"), { text = m.text, hl = "Dim" } }
				if (m.detail or "") ~= "" then row[3] = { text = " (" .. m.detail .. ")", hl = "Dim", copy = false } end
				B.lines[#B.lines + 1] = row
			elseif m.kind == "error" then
				-- the headline, then the raw error as its detail
				B.gap("note")
				local cancelled = m.status == "cancelled"
				for n, text in ipairs(util.split(m.text, "\n")) do
					if n == 1 then
						B.lines[#B.lines + 1] = {
							mark(cancelled and "stop" or "err"),
							{ text = text, hl = cancelled and "Dim" or "Danger" },
						}
					else
						B.lines[#B.lines + 1] = {
							{ text = n == 2 and "  ⎿ " or "    ", hl = "Dim", copy = false },
							{ text = text, hl = "Dim" },
						}
					end
				end
			elseif m.role == "user" then
				-- plain text: what the user typed is not markdown
				B.gap("user")
				B.user(m.text)
			else
				B.gap("text")
				B.markdown(m.text)
				---@type string[]
				local parts = {}
				local tout = fmt.tokens(m.out)
				if (m.elapsed or 0) > 0 then parts[#parts + 1] = fmt.duration(m.elapsed) end
				if tout then parts[#parts + 1] = tout .. " out" end
				if (m.ended or 0) > 0 then parts[#parts + 1] = fmt.clock(m.ended) end
				if #parts > 0 then
					B.gap("stats")
					B.lines[#B.lines + 1] =
						{ { text = "  ", copy = false }, { text = table.concat(parts, " · "), hl = "Dim" } }
				end
			end
			return { lines = B.lines, next = last + 1, mode = mode }
		end

		-- markdown and code carry the theme's colors: a light terminal's
		-- variant renders them anew
		local key = table.concat(
			{ epoch, width, tostring(ctx.show_reasoning), tostring(M.pane), ctx.cwd or "", zemi.ui.theme.get() },
			"|"
		)
		if key ~= units_key or epoch == 0 then
			units, units_key, settled = {}, key, nil
		end
		-- A long session resumed, a new width or a new theme leave every unit
		-- to render, far more than one render may take: the latest come first,
		-- and once they fill the view and 30 ms passed, the render returns them
		-- and asks for another, so a slow or loaded machine still draws the view
		-- whole; the header above them, the prompt the view pins, goes on top.
		local more = false
		local ag = ctx.agent or {}
		local done = settled and settled.count == count and settled.active == active and settled
		if not done then
			-- ctx.transcript copies every row into Lua, so only a miss reads it
			local tr = ctx.transcript or {}
			-- the runs after the last prompt are the live turn's while it runs
			local last_user = 0
			for n = #tr, 1, -1 do
				if block_of(tr[n]) == "user" and tr[n].kind ~= "shell" then
					last_user = n
					break
				end
			end
			---@param first integer
			---@return zemi.tui.RunMode|""
			local function mode_of(first)
				if not activity(tr[first]) then return "" end
				if ctx.show_reasoning then return "open" end
				if epoch == 0 or active and first > last_user then return "rail" end
				return unfolded[first] and "open" or "fold"
			end
			local deadline = epoch ~= 0 and os.clock() + 0.03
			---@type zemi.tui.ChatUnit[]
			local shown = {}
			local rendered, got = 0, 0
			local function stale(first, last)
				local u = units[first]
				return not u or u.next ~= last + 1 or u.mode ~= mode_of(first)
			end
			local function cached(first, last)
				if stale(first, last) then
					units[first] = unit(tr, first, last, mode_of(first))
					rendered = rendered + 1
				end
				return units[first]
			end
			local last = #tr
			while last >= 1 do
				local first = last
				while first > 1 and activity(tr[first]) and activity(tr[first - 1]) do
					first = first - 1
				end
				if deadline and rendered > 0 and got >= ctx.height and stale(first, last) and os.clock() > deadline then
					more = true
					break
				end
				local u = cached(first, last)
				shown[#shown + 1] = u
				got = got + #u.lines
				last = first - 1
			end
			-- a prompt: the row the view pins
			local function pins(m) return (m.kind or "") == "" and m.role == "user" end
			if more and not pins(tr[last + 1]) then
				for h = last, 1, -1 do
					if pins(tr[h]) then
						shown[#shown + 1] = cached(h, h)
						break
					end
				end
			end
			---@type zemi.Line[]
			local all = {}
			for n = #shown, 1, -1 do
				for _, l in ipairs(shown[n].lines) do
					all[#all + 1] = l
				end
			end
			done = { count = count, lines = all, n = #all, active = active, block = tr[#tr] and block_of(tr[#tr]) }
			settled = not more and done or nil
		end
		-- the live turn's lines go after the settled ones in the same list
		B.lines, B.prev = done.lines, done.block
		for n = #B.lines, done.n + 1, -1 do
			B.lines[n] = nil
		end

		-- Live turn (§6.3) from the host-owned ctx.agent snapshot, drawn on the
		-- rail after the rows of it already settled: the thinking, the calls,
		-- the reply. With the thinking pane drawn, the thinking shows there.
		local tools, text, queued = ag.tools or {}, ag.text or "", ag.steering or {}
		if ag.active or #tools > 0 or text ~= "" or #queued > 0 then
			local reasoning, thought = ag.reasoning or "", ag.thought or 0
			---@type number?
			local secs
			if reasoning ~= "" then
				if text == "" and #tools == 0 then
					B.gap("activity")
					B.thinking(not M.pane and reasoning or nil)
				elseif ctx.show_reasoning or thought > 5 or #tools == 0 then
					B.gap("activity")
					B.thought(thought, "reasoning", ctx.show_reasoning and reasoning or nil)
				else
					secs = thought
				end
			end
			if #tools > 0 then
				B.gap("activity")
				B.rail(tools, 1, #tools, { live = true, open = ctx.show_reasoning, secs = secs })
			end
			if text ~= "" then
				B.gap("text")
				B.markdown(text)
			end
			-- queued messages are the user's own words waiting their turn, apart
			-- from the agent's rows above
			for _, q in ipairs(queued) do
				B.gap("queued")
				B.user(q, "queued")
			end
		end
		local lines = B.lines
		-- The todo list stays under the conversation until every item is done.
		local todos, left = ctx.todos or {}, 0
		for _, td in ipairs(todos) do
			if td.status ~= "done" then left = left + 1 end
		end
		if left > 0 then
			B.blank()
			-- a header in the composer rule's voice: the name, how far along, and
			-- a short meter of one cell per item
			local head = {
				{ text = "Todos ", hl = "TodoTitle" },
				{ text = (#todos - left) .. "/" .. #todos .. "  ", hl = "Dim" },
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
					local hidden, done_n = #shown - cap, 0
					for i = cap + 1, #shown do
						if shown[i].td.status == "done" then done_n = done_n + 1 end
					end
					if done_n == hidden then
						lines[#lines + 1] = {
							{ text = "  ✓ ", hl = "TodoDone", copy = false },
							{ text = done_n .. " done", hl = "Dim" },
						}
					else
						lines[#lines + 1] = {
							{ text = "  … ", hl = "Dim", copy = false },
							{ text = fmt.count(hidden, "more todo", "more todos"), hl = "Dim" },
						}
					end
					break
				end
				local tick, mhl, hl, item = "  ○ ", "Rule", "Normal", td.content
				if td.status == "done" then
					tick, mhl, hl = "  ✓ ", "TodoDone", "Dim"
				elseif td.status == "in_progress" then
					tick, mhl, hl = "  ▸ ", "TodoMark", "TodoActive"
					if (td.active_form or "") ~= "" then item = td.active_form end
				end
				lines[#lines + 1] = { { text = tick, hl = mhl, copy = false }, { text = item, hl = hl } }
			end
		end
		return lines, more
	end,
})

return M
