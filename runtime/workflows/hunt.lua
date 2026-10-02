-- Find why something misbehaves: reproduce it, test the plausible causes at
-- once, and report the one the evidence supports; args is the symptom.
local wf = zemi.workflow
local symptom = type(wf.args) == "string" and wf.args ~= "" and wf.args or nil
if not symptom then return "hunt needs a symptom: /workflows run hunt <what happens>" end

wf.phase("reproduce")
local evidence, err = wf.agent(
	"This is a report of something misbehaving: "
		.. symptom
		.. "\n\nReproduce it: run the narrowest command, test or script that shows it, and read the code on the path it takes. "
		.. "Report what happens, what should happen instead, the exact command and its output, and file:line for every line you read. "
		.. "Name no cause and propose no fix: the agents after you do that.",
	{ agent = "general", description = "reproduce" }
)
if not evidence then return "could not reproduce it: " .. err end

local lenses = {
	{ "state", "the data the failing path carries, and where it is written or read" },
	{ "control flow", "the calls, branches and error paths that reach the failure" },
	{ "edges", "parsing, IO, concurrency, timing, and the values at the boundaries" },
}

wf.phase("causes")
local jobs = {}
for _, lens in ipairs(lenses) do
	jobs[#jobs + 1] = function()
		return wf.agent(
			"Give the most likely cause of this failure seen through "
				.. lens[2]
				.. ". Read the code to check the cause instead of guessing at it, and answer with the cause, the evidence for it (file:line), "
				.. "and what would prove it wrong. Say plainly when this lens shows nothing.\n\nEvidence:\n"
				.. evidence,
			{ agent = "explore", description = lens[1] .. " cause" }
		)
	end
end
local reports, errs = wf.parallel(jobs)

local causes = {}
for i, lens in ipairs(lenses) do
	if reports[i] then
		causes[#causes + 1] = "## " .. lens[1] .. "\n" .. reports[i]
	elseif errs and errs[i] then
		wf.log(lens[1] .. " came back with nothing: " .. errs[i])
	end
end
if #causes == 0 then return "nothing to go on for: " .. symptom end

wf.phase("verdict")
return wf.agent(
	"Decide the cause of this failure. Read the code each cause cites and keep only the one the evidence supports: "
		.. "a cause that explains the symptom but contradicts the code is not it. "
		.. "Answer with the cause and the file:line it lives at, why the evidence rules the others out, "
		.. "the smallest change that would fix it (make none), and the check that would show it fixed. "
		.. "When the evidence supports none of them, say so and name what to look at next.\n\nEvidence:\n"
		.. evidence
		.. "\n\nCauses:\n"
		.. table.concat(causes, "\n\n"),
	{ agent = "explore", description = "the cause" }
)
