-- Research a question from several angles at once and answer it with
-- sources; args is the question.
local wf = zemi.workflow
local question = type(wf.args) == "string" and wf.args ~= "" and wf.args or nil
if not question then return "research needs a question: /workflows run research <the question>" end

local angles = {
	{
		"web",
		"public sources",
		"Search the web for what is known about it. Read the pages that matter, follow the links they hold, and keep every URL you use. ",
	},
	{
		"explore",
		"this repository",
		"Find in this repository what bears on it: the code, the docs and the tests, with file:line for every fact. ",
	},
	{
		"explore",
		"the trade-offs",
		"Look for the options and their costs, in this repository and on the web: what each one buys, what it gives up, and which fits here. ",
	},
}

wf.phase("gather")
local jobs = {}
for _, a in ipairs(angles) do
	jobs[#jobs + 1] = function()
		return wf.agent(
			a[3]
				.. "Answer from what you read alone, and say plainly what you could not check. "
				.. "Quote the exact text when it matters, and end with the sources your answer rests on.\n\nQuestion: "
				.. question,
			{ agent = a[1], description = a[2] }
		)
	end
end
local reports, errs = wf.parallel(jobs)

local gathered = {}
for i, a in ipairs(angles) do
	if reports[i] then
		gathered[#gathered + 1] = "## " .. a[2] .. "\n" .. reports[i]
	elseif errs and errs[i] then
		wf.log(a[2] .. " came back with nothing: " .. errs[i])
	end
end
if #gathered == 0 then return "nothing came back for: " .. question end

wf.phase("answer")
return wf.agent(
	"Answer the question below from these reports alone. Lead with the answer in a sentence or two, then the detail it needs. "
		.. "Keep what they agree on, name where they disagree and why, and keep the source of every claim. "
		.. "Say what nobody could check, rather than smoothing it over.\n\nQuestion: "
		.. question
		.. "\n\nReports:\n"
		.. table.concat(gathered, "\n\n"),
	{ agent = "explore", description = "answer" }
)
