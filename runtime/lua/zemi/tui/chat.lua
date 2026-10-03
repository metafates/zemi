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

-- Rows the user opened, by transcript index (negative for the live turn's
-- calls), steps and thoughts, by their first index, and digests, by their
-- run's first index: a click shows that one row or run whole and leaves the
-- rest of the transcript alone. Cleared when the transcript is replaced, and
-- the negative half when a new turn starts, since the indexes are then
-- someone else's. A step has its own table: its first call has its index.
---@type table<integer, true>
local expanded = {}
---@type table<integer, true>
local steps = {}
---@type table<integer, true>
local unrolled = {}
---@type table<integer, true>
local unfolded = {}
-- runs shown whole, by first index (0 the live turn): a rail keeps its last
-- RAIL_KEEP rows and folds the earlier ones into one row until clicked
---@type table<integer, true>
local whole = {}
local RAIL_KEEP = 6
-- notice rows of one burst kept in view, the older ones behind a fold row
local NOTICES_KEEP = 3
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

-- The rail a turn's activity stands on: column 0, its rows' marks and text
-- from column 2.
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
-- Counted once per diff text: a render asks for a row's diff again and again.
local counts = {}
---@param diff string
---@return zemi.Line
local function diffstat(diff)
	local c = counts[diff]
	if not c then
		local add, del = 0, 0
		for l in diff:gmatch("[^\n]+") do
			if l:match("^%+") and not l:match("^%+%+%+ ") then
				add = add + 1
			elseif l:match("^%-") and not l:match("^%-%-%- ") then
				del = del + 1
			end
		end
		c = { add, del }
		counts[diff] = c
	end
	---@type zemi.Line
	local out = {}
	if c[1] > 0 then out[#out + 1] = { text = "+" .. c[1], hl = "Ok" } end
	if c[2] > 0 then out[#out + 1] = { text = (c[1] > 0 and " -" or "-") .. c[2], hl = "Danger" } end
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
	-- The bar-only row that frames a running entry: never two in a row, so two
	-- entries that run at once share the one between them.
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

	-- A subagent's or a plugin's message waiting for the turn's next step
	-- boundary: the notice row it becomes when the model reads it, tagged
	-- while it waits.
	---@param text string
	function B.queued(text)
		B.lines[#B.lines + 1] = {
			mark("note"),
			{ text = " queued ", hl = "Queued", copy = false },
			{ text = text, hl = "Dim" },
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
	-- "│ ✻ thought 8s", with the text under it when given: a bar-only row frames
	-- the thought above and below, once it is done as well as while it runs, and
	-- one between the header and prose of two or more lines so it breathes.
	---@param secs number
	---@param click string
	---@param text string?
	function B.thought(secs, click, text)
		frame()
		push({ bar(), mark("thought", click), { text = fmt.thought(secs), hl = "Dim", click = click } })
		if not text then
			frame()
			return
		end
		local at = #B.lines + 1
		if B.reasoning(text, nil, click) > 1 then table.insert(B.lines, at, air()) end
		frame()
	end
	-- The model thinking now.
	---@param tail string? The thinking so far, whose last lines show under it.
	function B.thinking(tail)
		frame()
		push({ bar(), mark("spin"), { text = "thinking", hl = "Dim" } })
		if tail then B.reasoning(tail, 4) end
		frame()
	end

	-- What counted calls did: "read 2 files, ran 1 command: go vet".
	---@param group zemi.tui.Tool[]
	---@return string
	local function said(group)
		---@type string[], table<string, { c: { [1]: string, [2]: string?, [3]: string? }, n: integer }>
		local order, seen = {}, {}
		---@type string[]
		local commands = {}
		for _, t in ipairs(group) do
			local c = assert(fmt.family(fmt.tool(t)))
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
	-- A step's unlabelled counted calls as one rail row saying what they
	-- did.
	---@param group zemi.tui.Tool[]
	---@param click string
	function B.group(group, click)
		push({ bar(), { text = fmt.clip(said(group), width - 2), hl = "Normal", click = click } })
	end

	-- One call as its own row: the verb and the label or input, what it
	-- changed or how long it runs at the right, and under it what it said.
	-- Open, a row shows the whole command, the diff and a command's output.
	---@param t zemi.tui.Tool
	---@param idx integer Transcript index, negative for a live-turn call.
	---@param o { open: boolean?, nest: string?, click: string? } nest: the two cells a row hangs by, its branch mark.
	function B.tool(t, idx, o)
		local name = fmt.tool(t)
		local open = o.open or expanded[idx] or false
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
		local tail = (t.diff or "") ~= "" and diffstat(t.diff) or {}
		---@param text string
		local function add(text)
			if #tail > 0 then tail[#tail + 1] = { text = " · ", hl = "Dim" } end
			tail[#tail + 1] = { text = text, hl = "Dim" }
		end
		if t.status == "run" and not asked and (t.elapsed or 0) >= 1 then add(fmt.duration(t.elapsed, true)) end
		local sw = fmt.cells(tail)
		local verb, rest = fmt.head(name, label, input, t.status)
		verb = fmt.lower(verb)
		local room = inner - 2 - zemi.ui.width(verb) - (sw > 0 and sw + 2 or 1)
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
				and not dim
				and zemi.ui.highlight(open and body or fit(input, room - 1), "bash")
			or nil
		-- an agent's rows open its transcript and a workflow's its view (ids must
		-- differ between adjacent rows), every other row expands the call
		local click = (t.run or "") ~= "" and ((name == "workflow" and "workflow:" or "agent:") .. t.run)
			or o.click
			or ("tool:" .. idx)
		-- a counted call reads as its step's row, no bold verb
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
		-- an ok counted call has no mark, as its step's row
		if fmt.looks(t) then
			head[1] = bar(pad)
		else
			table.insert(head, 1, bar(pad))
		end
		-- a call still running, or with its arguments still streaming, is framed
		-- by a bar-only row above and below while it runs, and listed plainly
		-- once it is done; a row waiting on the user is not one of them
		local running = not asked and (t.status == "run" or t.status == "wait")
		if running then frame() end
		local first = #B.lines + 1
		push(suffixed(head, tail, click))
		-- under the head one corner, every later row the same cells in, two less
		-- when the row hangs off a branch mark, whose own cells come above it
		local hang, indent = pad and "⎿ " or "  ⎿ ", pad and "  " or "    "
		local corner = true
		---@param text string|zemi.Line
		---@param hl string
		local function detail(text, hl)
			local row = { { text = corner and hang or indent, hl = "Dim", copy = false } }
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
		elseif label ~= "" and input ~= "" and (open or not fmt.looks(t)) then
			-- a labelled counted call on the rail shows its command once opened
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
				table.insert(row, 1, { text = indent, copy = false, click = click, hover = false })
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
		for n = first + 1, #B.lines do
			table.insert(B.lines[n], 1, bar(below))
		end
		if running then frame() end
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
		local digest = fmt.digest(list, first, last)
		if digest == "" then
			B.rail(list, first, last, {})
			return
		end
		local head = opened and "section" or nil
		local click = "turn:" .. first
		push({
			{ text = "· ", hl = "Dim", copy = false, hover = false, click = click, sticky = head },
			{ text = fmt.clip(digest, width - 2), hl = "Dim", click = click },
		})
		if not opened then return end
		local at = #B.lines
		B.rail(list, first, last, {})
		if #B.lines > at then B.lines[#B.lines][1].sticky = "end" end
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
			local click = "more:" .. first
			local label = whole[first] and "… show the rest" or "… " .. hidden .. " earlier"
			B.lines[#B.lines + 1] = {
				{ text = "  ", copy = false, hover = false },
				{ text = label, hl = "Dim", click = click },
			}
			if not whole[first] then first = last - NOTICES_KEEP + 1 end
		end
		for i = first, last do
			local m = list[i]
			local row = { mark("note"), { text = m.text, hl = "Dim" } }
			if (m.detail or "") ~= "" then row[3] = { text = " (" .. m.detail .. ")", hl = "Dim", copy = false } end
			B.lines[#B.lines + 1] = row
		end
	end

	-- A run of activity rows as a rail, one row per entry: a step's unlabelled
	-- counted calls, two or more of them, as one row whose click opens one
	-- row per call, a lone one as its own row, a thought a row of its own, every
	-- other call as its own row: a label tells what its call did. A thought row's
	-- click unrolls its text.
	-- NOTE: a step is the calls between two thoughts, so a model that does not
	-- think runs its steps together into one row.
	---@param list zemi.tui.Tool[]
	---@param first integer
	---@param last integer
	---@param o { open: boolean?, live: boolean?, run: integer? } o.open draws every row and thought of the run starting at run, whose rows a click leaves.
	function B.rail(list, first, last, o)
		local at = #B.lines
		B.heads = {}
		B.steps(list, first, last, o)
		-- a long rail keeps its last entries in view, each with the rows under
		-- it, the earlier ones behind one row that a click opens or closes; a
		-- printed run lists everything
		local key = o.live and 0 or o.run or first
		local n = #B.heads - RAIL_KEEP
		if o.open or ctx.headless or n < 2 then return end
		local click = "more:" .. key
		if whole[key] then
			table.insert(B.lines, at + 1, { bar(), { text = "… show the rest", hl = "Dim", click = click } })
			return
		end
		for _ = at + 1, B.heads[n + 1] - 1 do
			table.remove(B.lines, at + 1)
		end
		table.insert(B.lines, at + 1, { bar(), { text = "… " .. n .. " earlier", hl = "Dim", click = click } })
	end

	-- the rows a rail's entries start at: a thought, a step or a call, with
	-- what nests under it
	---@type integer[]
	B.heads = {}
	local function head() B.heads[#B.heads + 1] = #B.lines + 1 end

	---@param list zemi.tui.Tool[]
	---@param first integer
	---@param last integer
	---@param o { open: boolean?, live: boolean?, run: integer? }
	function B.steps(list, first, last, o)
		local fold = o.open and o.run and "turn:" .. o.run or nil
		---@param t zemi.tui.Tool
		local function grouped(t) return fmt.looks(t) and (t.label or "") == "" end
		local i = first
		while i <= last do
			local t = list[i]
			local idx = o.live and -i or i
			-- a counted call with no sibling is its own row: a "read 1 file"
			-- row over it would hide which file
			local lone = grouped(t) and not grouped(list[i + 1] or {})
			if t.kind == "reasoning" then
				head()
				B.thought(t.thought or 0, fold or ("thought:" .. idx), (o.open or unrolled[idx]) and t.text or nil)
				i = i + 1
			elseif grouped(t) and not lone then
				---@type zemi.tui.Tool[]
				local group = {}
				while i + #group <= last and grouped(list[i + #group]) do
					group[#group + 1] = list[i + #group]
				end
				-- the step's row heads its calls, nested two cells in, and its
				-- click closes them again
				head()
				B.group(group, "step:" .. idx)
				if o.open or steps[idx] then
					for k, g in ipairs(group) do
						local n = i + k - 1
						B.tool(g, o.live and -n or n, {
							open = o.open,
							nest = k == #group and "└ " or "├ ",
							click = fold,
						})
					end
				end
				i = i + #group
			else
				head()
				B.tool(t, idx, { open = o.open, click = fold })
				i = i + 1
			end
		end
	end
	return B
end

--- A click on the chat's rows: on a digest it opens the run under it or folds
--- it back, closing what was opened in it, on a step row opens its calls
--- nested under it or closes them, on a thought row unrolls its text in
--- place, on an agent's row shows that run, on a workflow's its view, and on
--- any other call's row opens or closes that call.
---@param ev zemi.WidgetClick
function M.click(ev)
	local run, flow = ev.id:match("^agent:(.+)"), ev.id:match("^workflow:(.+)")
	if run then
		assert(zemi.command.run("agents", "view " .. run))
	elseif flow then
		assert(zemi.command.run("workflows", "view " .. flow))
	else
		local kind, id = ev.id:match("^(%a+):(-?%d+)$")
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
		local set = kind == "turn" and unfolded
			or kind == "step" and steps
			or kind == "thought" and unrolled
			or kind == "more" and whole
			or expanded
		set[n] = not set[n] or nil
		-- what was opened in a folded run has no row left to close it by
		if kind == "turn" and not set[n] and stop then
			for _, s in ipairs({ expanded, steps, unrolled, whole }) do
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
		if count == 0 and not ctx.agent_active then
			return {} -- the splash widget fills the empty chat
		end
		-- An agent run's transcript mutates in place and reports epoch 0, so
		-- only the session's settled rows are cached.
		local epoch = ctx.transcript_epoch or 0
		-- rows are keyed by transcript index, and an agent view reports epoch 0
		-- for whichever run it shows: that run is the identity instead
		local ident = ctx.view and ctx.view.id or epoch
		if ident ~= opened_in then
			expanded, steps, unrolled, unfolded, whole, opened_in = {}, {}, {}, {}, {}, ident
		end
		-- The live turn's calls are keyed by negative index and the host restarts
		-- that list every turn, so a shorter list means those keys now stand for
		-- another turn's rows.
		local ntools = #((ctx.agent or {}).tools or {})
		if ntools < live_tools then
			for _, set in ipairs({ expanded, steps }) do
				for n in pairs(set) do
					if n < 0 then set[n] = nil end
				end
			end
			whole[0] = nil
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
				if #stats > 0 then
					B.gap("stats")
					local row = { { text = "  ", copy = false } }
					for _, sg in ipairs(stats) do
						row[#row + 1] = sg
					end
					B.lines[#B.lines + 1] = row
				end
			end
			return { lines = B.lines, next = last + 1, mode = mode }
		end

		-- markdown and code carry the theme's colors: a light terminal's
		-- variant renders them anew. A settled row whose text grew in place (a
		-- joined reasoning chunk) re-renders every unit, which the 30 ms budget
		-- bounds; the rows the reader opened keep their place.
		local key = table.concat({
			epoch,
			width,
			tostring(ctx.show_reasoning),
			ctx.cwd or "",
			zemi.ui.theme.get(),
			tostring(ctx.transcript_edit or 0),
		}, "|")
		local reset = key ~= units_key
		if reset then counts = {} end
		if reset or epoch == 0 then
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
		local done = settled
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
				return unfolded[first] and "unfold" or "fold"
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
		B.lines, B.prev = done.lines, done.block
		for n = #B.lines, done.n + 1, -1 do
			B.lines[n] = nil
		end

		-- Live turn (§6.3) from the host-owned ctx.agent snapshot, drawn on the
		-- rail after the rows of it already settled: the thinking, the calls,
		-- the reply.
		local tools, text, queued = ag.tools or {}, ag.text or "", ag.steering or {}
		if ag.active or #tools > 0 or text ~= "" or #queued > 0 then
			-- the thinking shows while the model thinks: a reasoning block of its own
			-- is open, so a thought that ended does not go on spinning over the
			-- calls that follow it. Each finished thought is a rail row of its own,
			-- framed by a bar-only row above and below like the running entry.
			if ag.thinking and (ag.reasoning or "") ~= "" and text == "" then
				B.gap("activity")
				B.thinking(ag.reasoning)
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
			-- reads it by, apart from the agent's rows above. Waiting notices that
			-- arrive together draw as one block, as a settled burst does.
			local note = false
			for _, q in ipairs(queued) do
				if not (note and q.notice) then B.gap(q.notice and "note" or "queued") end
				note = q.notice or false
				if q.notice then
					B.queued(q.text)
				else
					B.user(q.text, "queued")
				end
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
					text = td.status == "done" and "█" or "░",
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
