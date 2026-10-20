-- The chat widget: the transcript, the live turn and the todo list. A turn's
-- activity (thinking and tool calls) reads as a rail, which once settled
-- folds the calls a family counts into one digest row, its rail a click away.
local fmt = require("zemi.tui.text")
local util = require("zemi.util")

-- A tool row: a settled transcript row or a call of the live turn.
---@alias zemi.tui.Tool zemi.TranscriptRow|zemi.ToolCall

-- How a run of activity rows draws: its digest alone (settled), that digest
-- over its whole rail (a digest clicked open), a rail of one row per step
-- (the live turn, a printed run, an agent run's view), or the rail with every
-- row and thought open (zemi run --reasoning).
---@alias zemi.tui.RunMode "fold"|"unfold"|"rail"|"open"

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
---@type { count: integer, lines: zemi.Line[], n: integer, active: boolean, replying: boolean, block: string? }?
local settled
-- the activity run a list walks open (the messages picker's rail view): a
-- render that opens or closes one cannot reuse the settled lines
local rail_open

-- The folds a click opens in place, by index: a step's calls (a summary row,
-- not an action), a run's whole rail and a notice or queued burst. A call or
-- a thought never folds here: its click opens a view of its own off the
-- widget (/call). Cleared when the transcript is replaced, and the step half
-- when a new turn starts, since the indexes are then someone else's.
---@type table<integer, true>
local steps = {}
---@type table<integer, true>
local unfolded = {}
-- runs shown whole, by first index (0 the live turn): a rail keeps its last
-- RAIL_KEEP rows and folds the earlier ones into one row until clicked
---@type table<integer, true>
local whole = {}
local RAIL_KEEP = 6
-- the queued burst of notices drawn whole: its older rows sit behind one
-- fold row until clicked, and go back behind it when the queue empties
local queued_open = nil
-- The air a rail row keeps after the text it cuts, so an ellipsis reads as the
-- row's own and never runs into the window's edge; a row's right-aligned tail
-- keeps the measure's edge instead
local RAIL_EDGE = 4
-- notice rows of one burst kept in view, the older ones behind a fold row
local NOTICES_KEEP = 3

-- The label and click target of the one row that stands for a folded run:
-- a notice burst and a rail fold the same way, and opened it says what its
-- click does.
---@param open boolean?
---@param n integer
---@param click string
---@return string, string
local function foldRow(open, n, click) return open and "… collapse" or "… " .. n .. " earlier", click end
-- the transcript those tables belong to: its epoch, or the agent run on screen
---@type integer|string
local opened_in = 0
local live_tools = 0

---@class zemi.tui.chat
---@field click fun(ev: zemi.WidgetClick) The shipped chat's `on_click`, keyed by its click ids, so a replacement chat keeps its rows acting.
local M = {}

-- How a finished row's status marks it. A call running or not started yet
-- spins, unless the open prompt asks about it: that one waits on the user,
-- so it neither spins nor counts.
---@type table<string, string>
local MARK_OF = { ok = "ok", err = "err", stopped = "stop", bg = "bg", warn = "warn" }

-- A detail row hangs under its head by one corner, every later row the same
-- cells in, two less when the row is nested under a branch mark, whose own
-- cells stand above it.
local HANG, INDENT = "  ⎿ ", "    "

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

-- The rail a turn's activity stands on: column 0, its rows' marks from column
-- 2 and their text from column 4.
---@param pad string?
---@return zemi.Segment
local function bar(pad) return { text = "│ " .. (pad or ""), hl = "ThinkingBar", copy = false, hover = false } end
-- A rail row with only the bar: air around prose on the rail.
---@return zemi.Line
local function air() return { { text = "│", hl = "ThinkingBar", copy = false, hover = false } } end

-- A clipped block sinks at its top the way a scrolled region's edge does: its
-- first SINK rows, deepest first, easing off as the hidden rows come back.
local SINK = 4
---@param k integer Row's place in the block, 0 the top.
---@param past integer Rows the block hides above it.
---@return number
local function sink(k, past)
	if k >= SINK or past == 0 then return 0 end
	return (SINK - k) / (SINK + 1) * math.min(1, past / SINK)
end

--- Whether a row is thinking or a call: a run of them is one unit.
---@param m zemi.TranscriptRow
---@return boolean
local function activity(m) return m.kind == "reasoning" or m.kind == "tool" end

--- Whether a row is a notice: a burst of them draws as one block.
---@param m zemi.TranscriptRow
---@return boolean
local function notice(m) return m.kind == "notice" end

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

-- "+4 -1": the lines a diff adds and removes, in the colors a diff marks the
-- same counts with (Ok adds, Danger removes). A count of zero is left out: the
-- sign alone would report nothing, and the "+++"/"---" headers are no changes.
-- Counted once per row, whose diff the text settles on: a render asks for a
-- row's diff again and again.
---@type table<integer, { diff: string, add: integer, del: integer }>
local counts = {}
---@param diff string
---@param idx integer The row's transcript index, negative for a live call.
---@return zemi.Line
local function diffstat(diff, idx)
	local c = counts[idx]
	if not c or c.diff ~= diff then
		local add, del = 0, 0
		for l in diff:gmatch("[^\n]+") do
			if l:match("^%+") and not l:match("^%+%+%+ ") then
				add = add + 1
			elseif l:match("^%-") and not l:match("^%-%-%- ") then
				del = del + 1
			end
		end
		c = { diff = diff, add = add, del = del }
		counts[idx] = c
	end
	---@type zemi.Line
	local out = {}
	if c.add > 0 then out[#out + 1] = { text = "+" .. c.add, hl = "Ok" } end
	if c.del > 0 then out[#out + 1] = { text = (c.add > 0 and " -" or "-") .. c.del, hl = "Danger" } end
	return out
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
	-- The bar-only row that bounds the rail and sets its entries apart; never
	-- two in a row, so a rail opening on one draws no second.
	local function frame()
		local prev = B.lines[#B.lines]
		if prev and #prev == 1 and prev[1].text == "│" then return end
		push(air())
	end
	function B.blank() push({ { text = "" } }) end
	---@param block "user"|"activity"|"text"|"note"|"stats"|"queued"
	function B.gap(block)
		if B.prev and not (B.prev == "activity" and block == "activity") then B.blank() end
		B.prev = block
	end
	---@param s string
	---@return string
	local function relpath(s) return fmt.relpath(s, ctx.cwd, ctx.home) end
	-- row ending in tail, a line of segments, flush with the right edge
	---@param row zemi.Line
	---@param tail zemi.Line
	---@param click string?
	---@return zemi.Line
	local function suffixed(row, tail, click)
		if #tail == 0 then return row end
		row[#row + 1] = { text = string.rep(" ", math.max(2, width - fmt.cells(row) - fmt.cells(tail))) }
		for _, sg in ipairs(tail) do
			row[#row + 1] = { text = sg.text, hl = sg.hl, click = click }
		end
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
	-- The user's own words: the bar on every row, in the accent for a message
	-- that went and dim for one still waiting its turn. A sent message's rows
	-- are sticky: the chat pins its first row while the turn's rows fill the
	-- view, and the bar never hovers there.
	---@param text string
	---@param waiting boolean? Waiting for the turn's next step boundary.
	function B.user(text, waiting)
		local bar_hl = waiting and "Queued" or "UserMsg"
		for _, segs in ipairs(zemi.ui.wrap(text, width - 2)) do
			local row = { { text = "▌ ", hl = bar_hl, copy = false, hover = false, sticky = not waiting } }
			for _, sg in ipairs(segs) do
				sg.hl = waiting and "Dim" or "Normal"
				row[#row + 1] = sg
			end
			push(row)
		end
	end

	-- A subagent's or a plugin's message waiting for the turn's next step
	-- boundary: the notice row it becomes when the model reads it, tagged
	-- while it waits. A row naming a run opens its transcript.
	---@param text string
	---@param run string?
	function B.queued(text, run)
		local click = (run or "") ~= "" and "agent:" .. run or nil
		B.lines[#B.lines + 1] = {
			mark("note", click),
			{ text = " queued ", hl = "Queued", copy = false, click = click },
			{ text = text, hl = "Dim", click = click },
		}
	end

	-- Thinking text on the rail at column 4, prose wide at most, its last
	-- tail lines when tail is set, sunk at the top while lines are hidden
	-- above; a blank line keeps only the bar. Its rows are body: no hover,
	-- but a click acts as the row above them.
	---@param text string
	---@param tail integer?
	---@param click string?
	---@return integer rows
	function B.reasoning(text, tail, click)
		local body = zemi.ui.wrap((text:gsub("%s+$", "")), math.max(math.min(width - 4, fmt.PROSE), 20))
		local from = tail and math.max(1, #body - tail + 1) or 1
		for n = from, #body do
			local row, blank = { bar("  ") }, true
			local sunk = sink(n - from, from - 1)
			for _, sg in ipairs(body[n]) do
				sg.hl, sg.click, sg.hover, sg.fade = "AgentReason", click, false, sunk
				row[#row + 1] = sg
				blank = blank and sg.text == ""
			end
			push(blank and air() or row)
		end
		return #body - from + 1
	end
	-- "│ ✻ thought 8s", with the text under it when given, and one bar-only row
	-- between the header and prose of two or more lines so it breathes.
	---@param secs number
	---@param click string
	---@param text string?
	function B.thought(secs, click, text)
		push({ bar(), mark("thought", click), { text = fmt.thought(secs), hl = "Dim", click = click } })
		if not text then return end
		local at = #B.lines + 1
		if B.reasoning(text, nil, click) > 1 then table.insert(B.lines, at, air()) end
	end
	-- The model thinking now: drawn outside the rail's own loop, so it frames
	-- itself as an entry does. The label breathes while the row lives; a
	-- settled thought is a thought row, static.
	---@param tail string? The thinking so far, whose last lines show under it.
	function B.thinking(tail)
		frame()
		push({ bar(), mark("spin"), { text = "thinking", hl = "Dim", effect = "shimmer" } })
		if tail then B.reasoning(tail, 4) end
		frame()
	end

	-- What counted calls did: "read 2 files, ran 1 command: go vet".
	---@param group zemi.tui.Tool[]
	---@return string
	local function said(group)
		---@type string[]
		local commands = {}
		for _, t in ipairs(group) do
			if fmt.tool(t) == "shell" then
				commands[#commands + 1] = relpath(t.text or t.input or ""):gsub("%s+", " ")
			end
		end
		return fmt.counts(group, commands, true)
	end
	-- A step's unlabelled counted calls as one rail row saying what they
	-- did, marked as they are: every call it counts went well.
	---@param group zemi.tui.Tool[]
	---@param click string
	function B.group(group, click)
		push({
			bar(),
			mark("ok", click),
			{ text = fmt.clip(said(group), width - 4 - RAIL_EDGE), hl = "Normal", click = click },
		})
	end

	-- One call as its own row: the verb and the label or input, what it
	-- changed or how long it runs at the right, and under it what it said.
	-- Open (a printed run), a row shows the whole command, the diff and a
	-- command's output; a click opens that view instead, off the widget.
	---@param t zemi.tui.Tool
	---@param idx integer Transcript index, negative for a live-turn call.
	---@param o { open: boolean?, nest: string? } nest: the two cells a row hangs by, its branch mark.
	function B.tool(t, idx, o)
		local name = fmt.tool(t)
		local open = o.open
		-- NOTE: every running row reads as asked about while a prompt is open,
		-- a parallel call that runs on meanwhile included.
		local asked = t.status == "run" and (ctx.waiting or "") ~= ""
		-- a call nested under its step's row hangs by its branch mark, and the
		-- rows under it keep a line in that mark's place: a bar while more
		-- siblings follow, a gap under the last one, so the tree stays joined
		local pad = o.nest
		local below = pad and (pad == "└ " and "  " or "│ ") or nil
		-- a running row reads Dim beside its spinner until it ends
		local dim = not asked and MARK_OF[t.status] == nil
		local inner = width - 2 - (pad and 2 or 0)
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
		-- the row's right-aligned tail: what it changed, then how long it ran
		---@type zemi.Line
		local tail = (t.diff or "") ~= "" and diffstat(t.diff, idx) or {}
		---@param text string
		local function add(text)
			if #tail > 0 then tail[#tail + 1] = { text = " · ", hl = "Dim" } end
			tail[#tail + 1] = { text = text, hl = "Dim" }
		end
		if t.status == "run" and not asked and (t.elapsed or 0) >= 1 then add(fmt.duration(t.elapsed, true)) end
		local sw = fmt.cells(tail)
		local verb, rest = fmt.head(name, label, input, t.status)
		verb = fmt.lower(verb)
		local room = inner - 2 - zemi.ui.width(verb) - (sw > 0 and sw + 2 or 1 + RAIL_EDGE)
		-- open rows wrap (the widget wraps), collapsed ones stay one line
		---@param text string
		---@param n integer
		---@return string
		local function fit(text, n) return open and text or fmt.clip(text, n) end
		-- a command is highlighted where it is what the row reads, the head and
		-- an opened row; collapsed under its label it stays a Dim detail
		local code = name == "shell"
				and input ~= ""
				and (open or label == "")
				and not dim
				and zemi.ui.highlight(open and body or fit(input, room - 1), "bash")
			or nil
		-- a click opens the row's own view; an agent's row opens its transcript
		-- and a workflow's its view (ids must differ between adjacent rows)
		local click = (t.run or "") ~= "" and ((name == "workflow" and "workflow:" or "agent:") .. t.run)
			or ("call:" .. idx)
		-- a call that only looked around reads plainly, as its step's row does
		local vhl = dim and "Dim" or fmt.looks(t) and "Normal" or "AgentToolName"
		local head = { mark(asked and "wait" or MARK_OF[t.status] or "spin"), { text = verb, hl = vhl } }
		if code and label == "" then
			head[3] = { text = " ", hl = "Normal" }
			for _, sg in ipairs(code[1] or {}) do
				head[#head + 1] = sg
			end
		elseif rest ~= "" then
			head[3] = { text = fit(rest, room), hl = dim and "Dim" or "Normal" }
		end
		for _, sg in ipairs(head) do
			sg.click = click
		end
		-- the bar opens every row and the mark follows it; a nested row hangs by
		-- its branch mark where its own would stand, its step's state for it
		if pad then
			head[1] = bar(pad)
		else
			table.insert(head, 1, bar(pad))
		end
		local first = #B.lines + 1
		push(suffixed(head, tail, click))
		local hang, indent = pad and "⎿ " or HANG, pad and "  " or INDENT
		local corner = true
		---@param text string|zemi.Line
		---@param hl string
		---@param id string? The row's click id, the call's by default.
		local function detail(text, hl, id)
			local row = { { text = corner and hang or indent, hl = "Dim", copy = false } }
			for _, sg in ipairs(type(text) == "table" and text or { { text = text, hl = hl } }) do
				row[#row + 1] = { text = sg.text, hl = sg.hl, click = id or click, hover = false }
			end
			push(row)
			corner = false
		end
		-- an answered question's output holds "question\tanswer\tnote" lines,
		-- which take the place of the question under its header
		local output = t.output or ""
		local answered = name == "question" and output:find("\t") ~= nil
		-- the command a description stands for, then progress, then the job
		-- a timed-out command went on as, which the row carries
		if answered then
			more = {}
		elseif label ~= "" and input ~= "" and (open or not fmt.looks(t)) then
			-- a labelled counted call on the rail shows its command once opened
			detail(code and code[1] or fit(input, inner - 2 - RAIL_EDGE), "Dim")
		end
		for i, l in ipairs(more) do
			detail(code and code[i + 1] or l, "Dim")
		end
		if (t.detail or "") ~= "" then detail(fit((t.detail:gsub("%s+", " ")), inner - 2 - RAIL_EDGE), "Dim") end
		-- the job a timed-out command went on as opens its own view
		if (t.job or "") ~= "" then detail("running as " .. t.job, "Dim", "job:" .. t.job) end
		-- an open file tool shows what it changed as numbered, tinted rows, four
		-- cells in; a diff row is body: no hover, but the click folds like the head
		if open then
			for _, row in ipairs(zemi.ui.diff(t.diff or "", inner - 4)) do
				for _, sg in ipairs(row) do
					sg.click, sg.hover = click, false
				end
				table.insert(row, 1, { text = indent, copy = false, click = click, hover = false })
				push(row)
			end
		end
		-- the result's last lines: always for a failure, for what the user
		-- answered (question, plan), for a command of an open run; what the
		-- user just decided needs no restating: the model's text carries the
		-- reason and the instructions, the row the verdict. A running call
		-- keeps its line to the row; its view says the rest.
		if output:match("^permission denied %(user") then output = "denied by you" end
		if output:match("^The user dismissed") then output = "dismissed by you" end
		if
			output ~= ""
			and (
				t.status == "err"
				or t.status == "stopped"
				or name == "question"
				or name == "plan"
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
		for n = first + 1, #B.lines do
			table.insert(B.lines[n], 1, bar(below))
		end
	end

	-- A folded run is its digest alone, a click away from its whole rail: what
	-- the run did, as one line; a digest of nothing folds nothing, and the rail
	-- is drawn alone. An opened digest heads its rail: the chat pins it under the
	-- prompt down to the rail's last row.
	---@param list zemi.TranscriptRow[]
	---@param first integer
	---@param last integer
	---@param opened boolean
	function B.fold(list, first, last, opened)
		-- the messages picker's rail view names this run: it draws whole, so
		-- every call the list walks has a row to band
		local full = (ctx.rail or 0) == first
		local digest = fmt.digest(list, first, last)
		if digest == "" then
			B.rail(list, first, last, { full = full })
			return
		end
		local head = opened and "section" or nil
		local click = "turn:" .. first
		push({
			{ text = "· ", hl = "Dim", copy = false, hover = false, click = click, sticky = head },
			{ text = fmt.clip(digest, width - 2 - RAIL_EDGE), hl = "Dim", click = click },
		})
		if not opened then return end
		local at = #B.lines
		-- a lone thought opens as its text: the digest above it already says how long it thought
		B.rail(list, first, last, { unroll = first == last and list[first].kind == "reasoning" or nil, full = full })
		if #B.lines > at then B.lines[#B.lines][1].sticky = "end" end
	end

	-- A notice row: the status mark it came in with, what it says, and any
	-- detail in brackets after it.
	---@param status string?
	---@param text string
	---@param detail string?
	---@return zemi.Line
	function B.note(status, text, detail)
		local row = { mark(MARK_OF[status] or "note"), { text = text, hl = "Dim" } }
		if (detail or "") ~= "" then row[3] = { text = " (" .. detail .. ")", hl = "Dim", copy = false } end
		return row
	end

	-- A burst of notice rows as one block, no blank row between them: reports
	-- and updates arrive together, and the older ones of a long burst sit
	-- behind one row a click opens, as a long rail's do.
	---@param list zemi.TranscriptRow[]
	---@param first integer
	---@param last integer
	function B.notices(list, first, last)
		B.gap("note")
		local hidden = last - first + 1 - NOTICES_KEEP
		-- a printed run lists every row: its fold row would have nothing to click
		if hidden > 1 and not ctx.headless then
			local label, click = foldRow(whole[first], hidden, "more:" .. first)
			B.lines[#B.lines + 1] = {
				{ text = "  ", copy = false, hover = false },
				{ text = label, hl = "Dim", click = click },
			}
			if not whole[first] then first = last - NOTICES_KEEP + 1 end
		end
		for i = first, last do
			local m = list[i]
			B.lines[#B.lines + 1] = B.note(m.status, m.text, m.detail)
		end
	end

	-- A run of activity rows as a rail, one row per entry: a step's unlabelled
	-- counted calls, two or more of them, as one row whose click opens one
	-- row per call, a lone one as its own row, a thought a row of its own, every
	-- other call as its own row: a label tells what its call did. A call's or
	-- a thought's click opens its own view.
	-- NOTE: a step is the calls between two thoughts, so a model that does not
	-- think runs its steps together into one row.
	---@param list zemi.tui.Tool[]
	---@param first integer
	---@param last integer
	---@param o { open: boolean?, full: boolean?, live: boolean?, run: integer?, unroll: boolean? } o.open draws every row and thought of the run starting at run with its bodies; o.full draws every row of it with the bodies folded; o.unroll draws a thought as its text.
	function B.rail(list, first, last, o)
		frame()
		local at = #B.lines
		B.heads = {}
		B.steps(list, first, last, o)
		frame()
		-- a long rail keeps its last entries in view, each with the rows under
		-- it, the earlier ones behind one row that a click opens or closes; a
		-- printed run lists everything
		local key = o.live and 0 or o.run or first
		local n = #B.heads - RAIL_KEEP
		if o.open or o.full or ctx.headless or n < 2 then return end
		local label, click = foldRow(whole[key], n, "more:" .. key)
		if not whole[key] then
			for _ = at + 1, B.heads[n + 1] - 1 do
				table.remove(B.lines, at + 1)
			end
		end
		table.insert(B.lines, at + 1, { bar(), { text = label, hl = "Dim", click = click } })
		table.insert(B.lines, at + 2, air())
	end

	-- the rows a rail's entries start at: a thought, a step or a call, with
	-- what nests under it
	---@type integer[]
	B.heads = {}
	local function head() B.heads[#B.heads + 1] = #B.lines + 1 end

	-- The rows a call or a thought draws carry its transcript index, so a
	-- list naming one call (a rail's own) bands those rows alone.
	---@param at integer
	---@param idx integer
	local function markCall(at, idx)
		for k = at, #B.lines do
			local row = B.lines[k]
			if row[1] then row[1].call = idx end
		end
	end

	---@param list zemi.tui.Tool[]
	---@param first integer
	---@param last integer
	---@param o { open: boolean?, full: boolean?, live: boolean?, run: integer?, unroll: boolean? }
	function B.steps(list, first, last, o)
		---@param t zemi.tui.Tool
		local function grouped(t) return fmt.looks(t) and (t.label or "") == "" end
		local i = first
		-- The rail opens on a bar and closes on one; between entries one stands
		-- where their kinds differ, and around one that failed or was stopped.
		local prev, prev_fail = nil, false
		while i <= last do
			local t = list[i]
			local idx = o.live and -i or i
			-- a counted call with no sibling is its own row: a "read 1 file"
			-- row over it would hide which file
			local lone = grouped(t) and not grouped(list[i + 1] or {})
			local kind = t.kind == "reasoning" and "thought" or fmt.tool(t)
			local fail = t.status == "err" or t.status == "stopped"
			if prev ~= kind or prev_fail or fail then frame() end
			prev, prev_fail = kind, fail
			head()
			if t.kind == "reasoning" then
				local click = "call:" .. idx
				local at = #B.lines + 1
				if o.unroll and (t.text or "") ~= "" then
					B.reasoning(t.text, nil, click)
				else
					B.thought(t.thought or 0, click, o.open and t.text or nil)
				end
				markCall(at, idx)
				i = i + 1
			elseif grouped(t) and not lone then
				---@type zemi.tui.Tool[]
				local group = {}
				while i + #group <= last and grouped(list[i + #group]) do
					group[#group + 1] = list[i + #group]
				end
				-- the step's row heads its calls, nested two cells in, and its
				-- click closes them again
				B.group(group, "step:" .. idx)
				if o.open or o.full or steps[idx] then
					for k, g in ipairs(group) do
						local n = i + k - 1
						local at = #B.lines + 1
						B.tool(g, o.live and -n or n, {
							open = o.open,
							nest = k == #group and "└ " or "├ ",
						})
						markCall(at, o.live and -n or n)
					end
				end
				i = i + #group
			else
				local at = #B.lines + 1
				B.tool(t, idx, { open = o.open })
				markCall(at, idx)
				i = i + 1
			end
		end
	end
	return B
end

-- The block the rows printed above ended in: a printed batch's first unit
-- seeds its block gap from it, where a printed run draws one blindly.
---@param ctx zemi.WidgetCtx
---@return "user"|"activity"|"text"|"note"|nil
local function prev_block(ctx) return ctx.printed_prev and block_of(ctx.printed_prev) or nil end

--- A click on the chat's rows: on a call's or a thought's row opens a view of
--- it, on an agent's row shows that run, on a workflow's its view, on a job's
--- line its output, and on a fold row (a digest, a step, a burst's `… N
--- earlier`) opens or closes what it stands for in place.
---@param ev zemi.WidgetClick
function M.click(ev)
	local run, flow, call, job =
		ev.id:match("^agent:(.+)"),
		ev.id:match("^workflow:(.+)"),
		ev.id:match("^call:(-?%d+)$"),
		ev.id:match("^job:(.+)$")
	if run then
		assert(zemi.command.run("agents", "view " .. run))
	elseif flow then
		assert(zemi.command.run("workflows", "view " .. flow))
	elseif call then
		assert(zemi.command.run("call", call))
	elseif job then
		assert(zemi.command.run("jobs", "view " .. job))
	else
		local kind, id = ev.id:match("^(%a+):(-?%d+)$")
		if kind == "moreq" then
			-- the queued burst's fold row: it holds no transcript rows, and the
			-- queue is one block, so one state serves every render of it
			queued_open = not queued_open or nil
			return
		end
		local n = tonumber(id)
		if not n then return end
		---@type integer?
		local stop
		for first, u in pairs(units) do
			if first <= n and n < u.next then
				stop = u.next
				units[first] = nil
			end
		end
		settled = nil
		local set = kind == "turn" and unfolded or kind == "step" and steps or whole
		set[n] = not set[n] or nil
		-- what was opened in a folded run has no row left to close it by
		if kind == "turn" and not set[n] and stop then
			for _, s in ipairs({ steps, whole }) do
				for i = n, stop - 1 do
					s[i] = nil
				end
			end
		end
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
		local count = ctx.transcript_count or 0
		-- the splash owns the empty chat: it draws the pending row too, in a
		-- block that fits, so nothing here can scroll it off the top
		if fmt.empty_chat(ctx) then return {} end
		-- work that outlives this view of the session, drawn at the tail: a
		-- session's counts are about other work, so a run's view leaves it out
		local pending = fmt.pending(ctx)
		if ctx.view then pending = nil end
		-- An agent run's transcript mutates in place and reports epoch 0: the
		-- run's own revision says when it did, so its rows are cached like the
		-- session's, its last row aside.
		local epoch = ctx.transcript_epoch or 0
		local rev = ctx.view_revision or 0
		-- rows are keyed by transcript index, and an agent view reports epoch 0
		-- for whichever run it shows: that run is the identity instead
		local ident = ctx.view and ctx.view.id or epoch
		if ident ~= opened_in then
			steps, unfolded, whole, queued_open, opened_in = {}, {}, {}, nil, ident
		end
		-- The live turn's calls are keyed by negative index and the host restarts
		-- that list every turn, so a shorter list means those keys now stand for
		-- another turn's rows.
		local ntools = #((ctx.agent or {}).tools or {})
		if ntools < live_tools then
			for n in pairs(steps) do
				if n < 0 then steps[n] = nil end
			end
			whole[0] = nil
		end
		live_tools = ntools
		local active = ctx.agent_active or false
		local B = builder(ctx, width)
		-- Every drawn row of a unit carries its first transcript index, which a
		-- following list (the messages picker) names to scroll the chat here and
		-- to band the whole message. The rows from upto on are meta (a reply's
		-- readout), outside the message. Go reads the anchor and never draws it.
		---@param lines zemi.Line[]
		---@param first integer
		---@param upto integer?
		local function anchored(lines, first, upto)
			for i = 1, upto or #lines do
				local row = lines[i]
				if row[1] and fmt.cells(row) > 0 then row[1].anchor = first end
			end
		end
		-- The messages first..last as one unit: a run of activity rows, else one.
		---@param tr zemi.TranscriptRow[]
		---@param first integer
		---@param last integer
		---@param mode zemi.tui.RunMode|""
		---@return zemi.tui.ChatUnit
		local function unit(tr, first, last, mode)
			local m = tr[first]
			local content = nil -- where the message ends and meta begins
			B.lines, B.prev = {}, first > 1 and block_of(tr[first - 1]) or prev_block(ctx)
			if mode == "fold" or mode == "unfold" then
				B.gap("activity")
				B.fold(tr, first, last, mode == "unfold")
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
			elseif notice(m) then
				B.notices(tr, first, last)
			elseif m.kind == "compact" then
				B.gap("note")
				B.lines[#B.lines + 1] = B.note("note", m.text, m.detail)
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
							{ text = n == 2 and HANG or INDENT, hl = "Dim", copy = false },
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
				-- The readout rides the reply that ends the exchange, and only the
				-- next text row decides it: a call, notice or error after it changes
				-- nothing, while another reply means a sentence between runs.
				---@type zemi.Line
				local stats = {}
				local final = true
				for i = last + 1, #tr do
					local r = tr[i]
					if (r.kind or "") == "" then
						final = r.role == "user"
						break
					end
				end
				if final then
					---@param text string
					local function stat(text)
						if #stats > 0 then stats[#stats + 1] = { text = " · ", hl = "Dim" } end
						stats[#stats + 1] = { text = text, hl = "Dim" }
					end
					local tout = fmt.tokens(m.out)
					if (m.elapsed or 0) > 0 then stat(fmt.duration(m.elapsed)) end
					if tout then stat(tout .. " out") end
					if (m.ended or 0) > 0 then stat(fmt.clock(m.ended)) end
				end
				content = #B.lines
				if #stats > 0 then
					B.gap("stats")
					local row = { { text = "  ", copy = false } }
					for _, sg in ipairs(stats) do
						row[#row + 1] = sg
					end
					B.lines[#B.lines + 1] = row
				end
			end
			anchored(B.lines, first, content)
			return { lines = B.lines, next = last + 1, mode = mode }
		end

		-- markdown and code carry the theme's colors: a light terminal's
		-- variant renders them anew. A settled row whose text grew in place (a
		-- joined reasoning chunk) re-renders its unit, which the 30 ms budget
		-- bounds; the rows the reader opened keep their place.
		-- The view is in the key: two runs report revisions of their own.
		local key = table.concat({
			epoch,
			width,
			tostring(ctx.show_reasoning),
			ctx.cwd or "",
			zemi.ui.theme.get(),
			tostring(ctx.transcript_edit or 0),
			ctx.view and ctx.view.id or "",
			rev,
		}, "|")
		local reset = key ~= units_key
		if reset then counts = {} end
		if (ctx.rail or 0) ~= (rail_open or 0) then
			rail_open, settled = ctx.rail or 0, nil
		end
		-- a printed run is a snapshot and reports epoch 0: only the session and
		-- a shown run cache their rows
		local cacheable = epoch ~= 0 or ctx.view ~= nil
		if reset or not cacheable then
			units, units_key, settled = {}, key, nil
		end
		-- A long session resumed, a new width or a new theme leave every unit
		-- to render, far more than one render may take: the latest come first,
		-- and once they fill the view and 30 ms passed, the render returns them
		-- and asks for another, so a slow or loaded machine still draws the view
		-- whole; the header above them, the prompt the view pins, goes on top.
		local more = false
		local ag = ctx.agent or {}
		-- A reply closes the run it follows at once, streaming or settled: the run
		-- the turn is working in is its last activity run, while that reaches the
		-- tail and the turn is not answering.
		local replying = (ag.text or "") ~= ""
		-- A viewed run's rows change under a count that does not: its view
		-- never takes the whole-output shortcut, its tail redraws instead.
		local done = not ctx.view
			and settled
			and settled.count == count
			and settled.active == active
			and settled.replying == replying
			and settled
		if not done then
			-- ctx.transcript copies every row into Lua, so only a miss reads it
			local tr = ctx.transcript or {}
			local live_first = 0
			if active and not replying and #tr > 0 and activity(tr[#tr]) then
				live_first = #tr
				while live_first > 1 and activity(tr[live_first - 1]) do
					live_first = live_first - 1
				end
			end
			---@param first integer
			---@return zemi.tui.RunMode|""
			local function mode_of(first)
				if not activity(tr[first]) then return "" end
				if ctx.show_reasoning then return "open" end
				-- the live turn's own run and a printed run read whole; every
				-- other run folds, the one an agent view shows included
				if ctx.headless or first == live_first then return "rail" end
				if (ctx.rail or 0) == first then return "unfold" end
				return unfolded[first] and "unfold" or "fold"
			end
			local deadline = cacheable and os.clock() + 0.03
			---@type zemi.tui.ChatUnit[]
			local shown = {}
			local rendered, got = 0, 0
			---@param tail boolean? The group holding the last row: a run rewrites it in place as it streams, so it always redraws.
			local function stale(first, last, tail)
				if tail then return true end
				local u = units[first]
				return not u or u.next ~= last + 1 or u.mode ~= mode_of(first)
			end
			local function cached(first, last, tail)
				if stale(first, last, tail) then
					units[first] = unit(tr, first, last, mode_of(first))
					-- the tail redraws every frame and is no work the budget
					-- needs to bound: only a group the cache lost counts
					if not tail then rendered = rendered + 1 end
				end
				return units[first]
			end
			local last = #tr
			local tail = true
			while last >= 1 do
				local first = last
				if notice(tr[last]) then
					while first > 1 and notice(tr[first - 1]) do
						first = first - 1
					end
				else
					while first > 1 and activity(tr[first]) and activity(tr[first - 1]) do
						first = first - 1
					end
				end
				if deadline and rendered > 0 and got >= ctx.height and stale(first, last) and os.clock() > deadline then
					more = true
					break
				end
				local u = cached(first, last, tail)
				tail = false
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
			-- the transcript's first row stands one row off the window's top
			-- edge; a printed run's output keeps its own framing
			if last == 0 and #tr > 0 and not ctx.headless then all[1] = { { text = "" } } end
			for n = #shown, 1, -1 do
				for _, l in ipairs(shown[n].lines) do
					all[#all + 1] = l
				end
			end
			done = {
				count = count,
				lines = all,
				n = #all,
				active = active,
				replying = replying,
				block = tr[#tr] and block_of(tr[#tr]),
			}
			settled = not more and done or nil
		end
		-- the live turn's lines go after the settled ones in the same list
		B.lines, B.prev = done.lines, done.block or prev_block(ctx)
		for n = #B.lines, done.n + 1, -1 do
			B.lines[n] = nil
		end

		-- Live turn (§6.3) from the host-owned ctx.agent snapshot, drawn on the
		-- rail after the rows of it already settled: the thinking, the calls,
		-- the reply.
		local tools, text, queued = ag.tools or {}, ag.text or "", ag.steering or {}
		if #queued == 0 then queued_open = nil end
		if ag.active or #tools > 0 or text ~= "" or #queued > 0 then
			-- the thinking shows while the model thinks: a reasoning block of its own
			-- is open, so a thought that ended does not go on spinning over the
			-- calls that follow it. Each finished thought is a rail row of its own,
			-- framed as an entry is.
			if ag.thinking and (ag.reasoning or "") ~= "" and text == "" then
				B.gap("activity")
				B.thinking(ag.reasoning)
			elseif
				text == ""
				and #tools == 0
				and ag.active
				and not ctx.view -- a run's view draws its own rows: its calls and reply never reach tools or text
				and not ag.retry
				and not ag.compacting -- the pending row already names the summary
				and not ctx.cancelling
				and ctx.waiting == ""
				and (ag.elapsed or 0) >= 0.3
			then
				-- the wait before any output: the thinking row opens with the turn,
				-- after the same 300ms that guards the working rule's block
				B.gap("activity")
				B.thinking(nil)
			end
			if #tools > 0 then
				B.gap("activity")
				B.rail(tools, 1, #tools, { live = true, open = ctx.show_reasoning })
			end
			if text ~= "" then
				B.gap("text")
				B.markdown(text)
			end
			-- what waits for the turn's next step boundary: the user's own words
			-- as typed, and a subagent's message as the head line the model
			-- reads it by, apart from the agent's rows above. Waiting notices
			-- that arrive together draw as one block, as a settled burst does,
			-- a long one behind one row a click opens; the user's own words
			-- never fold.
			local notes = 0
			for _, q in ipairs(queued) do
				if q.notice then notes = notes + 1 end
			end
			local fold = notes - NOTICES_KEEP
			-- a printed run lists every row, its fold row would have nothing to
			-- click, and one hidden notice is not worth a row either: nothing
			-- hides unless the fold row draws
			if ctx.headless or fold < 2 then fold = 0 end
			local seen, note = 0, false
			for _, q in ipairs(queued) do
				if q.notice then
					seen = seen + 1
					-- the fold row stands where the oldest folded notice was,
					-- and stays to close what a click opened
					if seen == 1 and fold > 1 then
						if not note then B.gap("note") end
						note = true
						local label, click = foldRow(queued_open, fold, "moreq:0")
						B.lines[#B.lines + 1] = {
							{ text = "  ", copy = false, hover = false },
							{ text = label, hl = "Dim", click = click },
						}
					end
					if seen > fold or queued_open then
						if not note then B.gap("note") end
						note = true
						B.queued(q.text, q.run)
					end
				else
					B.gap("queued")
					B.user(q.text, true)
					note = false
				end
			end
		end
		-- What will change this conversation later sits between it and the
		-- todos: a static row, so nothing here keeps the frame clock awake.
		if pending then
			if #B.lines > 0 then B.blank() end
			B.lines[#B.lines + 1] = pending
		end
		local lines = B.lines
		-- The todo list stays under the conversation until every item is done.
		local todos, left = ctx.todos or {}, 0
		for _, td in ipairs(todos) do
			if td.status ~= "done" then left = left + 1 end
		end
		if left > 0 then
			B.blank()
			-- at most 12 rows and a third of the chat, in the order the model
			-- wrote them: the window trails the item in progress, so a long
			-- list never hides what is being worked on
			local cap = math.min(12, math.max(3, math.floor((ctx.height or 36) / 3)))
			local live = 0
			for n, td in ipairs(todos) do
				if td.status == "in_progress" then live = n end
			end
			local first = math.max(1, math.min(live - cap + 1, #todos - cap + 1))
			local last = math.min(#todos, first + cap - 1)
			-- a header in the composer rule's voice: the name, how far along, and
			-- a short meter of one cell per item shown under it
			local head = {
				{ text = "Todos ", hl = "TodoTitle" },
				{ text = (#todos - left) .. "/" .. #todos .. "  ", hl = "Dim" },
			}
			for n = first, last do
				local td = todos[n]
				head[#head + 1] = {
					text = td.status == "done" and "█" or "░",
					hl = td.status == "done" and "TodoDone" or td.status == "in_progress" and "TodoMark" or "Rule",
					copy = false,
				}
			end
			lines[#lines + 1] = head
			---@param hidden integer
			---@param above boolean?
			local function fold(hidden, above)
				lines[#lines + 1] = {
					{ text = "  … ", hl = "Dim", copy = false },
					{ text = fmt.count(hidden, "more todo", "more todos") .. (above and " above" or ""), hl = "Dim" },
				}
			end
			if first > 1 then fold(first - 1, true) end
			for n = first, last do
				local td = todos[n]
				local tick, mhl, hl, item = "  ○ ", "Rule", "Normal", td.content
				if td.status == "done" then
					tick, mhl, hl = "  ✓ ", "TodoDone", "Dim"
				elseif td.status == "in_progress" then
					tick, mhl, hl = "  ▸ ", "TodoMark", "TodoActive"
					if (td.active_form or "") ~= "" then item = td.active_form end
				end
				lines[#lines + 1] = { { text = tick, hl = mhl, copy = false }, { text = item, hl = hl } }
			end
			if last < #todos then fold(#todos - last) end
		end
		return lines, more
	end,
})

return M
