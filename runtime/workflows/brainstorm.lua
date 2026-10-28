-- What the goal names, attacked from several lenses at once: candidates
-- invented, a critic kills the weak ones, the survivors with a cheap
-- check are smoke-tested, and the answer is the worth-try list with the
-- smoke evidence. Args is the goal.
local wf = zemi.workflow
local goal = type(wf.args) == "string" and wf.args ~= "" and wf.args or nil
if not goal then return "brainstorm needs a goal: /workflows run brainstorm <what to improve>" end

local shape = "End with one idea per block, opened by a line '=== IDEA: <short name>', then "
	.. "what to change, the mechanism that moves the metric, and the cheapest check that would "
	.. "prove the value (a command, a probe, a before/after), or 'no cheap check'.\n\n"

wf.phase("generate")
local lenses = {
	{ "repeats and waits", "Read the code and the docs for where the work repeats, waits, or is paid twice." },
	{
		"unmeasured assumption",
		"Name an assumption this area rests on that nobody has measured, and follow what becomes possible if it is wrong.",
	},
	{
		"borrowed mechanism",
		"Find the tool or field that handles this goal better and take its mechanism, not its feature.",
	},
}
local jobs = {}
for _, lens in ipairs(lenses) do
	jobs[#jobs + 1] = function()
		return wf.agent(
			lens[2]
				.. " Invent candidate ideas for the goal below - novel ones over variations of what is already there. "
				.. shape
				.. "Ground every idea in what you read, and say what you could not check.\n\nGoal: "
				.. goal,
			{ agent = "explore", description = lens[1] }
		)
	end
end
local reports, errs = wf.parallel(jobs)
local found = {}
for i, lens in ipairs(lenses) do
	if reports[i] then
		found[#found + 1] = "## " .. lens[1] .. "\n" .. reports[i]
	elseif errs and errs[i] then
		wf.log(lens[1] .. " came back with nothing: " .. errs[i])
	end
end
if #found == 0 then return "nothing came back for: " .. goal end

wf.phase("critique")
local critique = wf.agent(
	"You are the critic for a brainstorm on the goal below. Kill every idea that has no mechanism "
		.. "behind it, repeats what the code already does, or costs more than it saves: one line each on why it dies. "
		.. "Keep at most five, and for each survivor answer with a block:\n"
		.. "=== TRY: <short name>\n<what to change, the mechanism, the metric it moves>\n"
		.. "SMOKE: <the cheapest check that would show the value, as concrete steps; write 'none' when nothing cheap exists>\n"
		.. "Read the repository when an idea's claim about it needs checking.\n\nGoal: "
		.. goal
		.. "\n\nCandidate ideas:\n"
		.. table.concat(found, "\n\n"),
	{ agent = "explore", description = "critique" }
)
if not critique or critique == "" then return "the critique came back with nothing for: " .. goal end

local tries = {}
local cur
for line in (critique .. "\n"):gmatch("(.-)\n") do
	local name = line:match("^=== TRY: (.+)$")
	if name then
		cur = { name = name, body = {} }
		tries[#tries + 1] = cur
	elseif cur then
		local smoke = line:match("^SMOKE: (.+)$")
		if smoke then
			cur.smoke = smoke
		else
			cur.body[#cur.body + 1] = line
		end
	end
end
if #tries == 0 then return "every idea died in critique:\n" .. critique end

-- the smoke agents are general: they run commands, but a check never
-- changes the repository it judges
wf.phase("smoke")
local smoked = {}
for _, t in ipairs(tries) do
	if t.smoke and not t.smoke:match("^none") then smoked[#smoked + 1] = t end
end
while #smoked > 4 do
	table.remove(smoked)
end
local sjobs = {}
for _, t in ipairs(smoked) do
	sjobs[#sjobs + 1] = function()
		return wf.agent(
			"Run this cheap check and report what it shows. A handful of commands, ten minutes at most; "
				.. "build and write in a temp directory, never in tracked files. "
				.. "End with a line 'VERDICT: supported' or 'VERDICT: not supported', then the numbers you saw.\n\nIdea: "
				.. t.name
				.. "\n"
				.. table.concat(t.body, "\n")
				.. "\n\nCheck: "
				.. t.smoke,
			{ agent = "general", description = "smoke: " .. t.name }
		)
	end
end
local smokeReports, smokeErrs = wf.parallel(sjobs)
local evidence = {}
for _, t in ipairs(tries) do
	local ran = nil
	for i, s in ipairs(smoked) do
		if s == t then
			ran = smokeReports[i] or ("the check failed to run: " .. tostring(smokeErrs and smokeErrs[i] or "?"))
		end
	end
	evidence[#evidence + 1] = "## "
		.. t.name
		.. "\n"
		.. table.concat(t.body, "\n")
		.. "\n\n"
		.. (ran and ("Smoke evidence:\n" .. ran) or "No cheap check was run.")
end

wf.phase("report")
return wf.agent(
	"Turn this brainstorm into the worth-try list. Lead with the ideas worth trying, best first: "
		.. "what to change, the mechanism, the metric it moves, and the smoke numbers when there are any. "
		.. "An idea whose smoke contradicted it goes to the dead list, not the list. Then one line per killed "
		.. "idea with the reason it died. Do not pad.\n\nGoal: "
		.. goal
		.. "\n\nCritique:\n"
		.. critique
		.. "\n\nSmoke evidence:\n"
		.. table.concat(evidence, "\n\n"),
	{ agent = "explore", description = "worth-try list" }
)
