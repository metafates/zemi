-- Review uncommitted changes, the branch since its merge base with args (a
-- git ref), or one commit (args a sha) from three angles; keeps findings
-- that hold up.
local wf = zemi.workflow
local base = type(wf.args) == "string" and wf.args ~= "" and wf.args or nil
-- a hex name is a commit, and its merge base with HEAD is itself, so
-- reviewing it as a base would review everything since it instead
local sha = base and #base >= 7 and #base <= 40 and base:match("^%x+$")

local task
local what = "the uncommitted changes"
if sha then
	-- a hex name reads as a base ref, so say which one this is
	what = "the one commit " .. base
	task = "List the changes commit " .. base .. " introduced: run `git show " .. base .. "`. "
elseif base then
	what = "the changes since " .. base
	task = "List the changes in this repository: run `git status --short` and `git diff --merge-base " .. base .. "`. "
else
	task = "List the changes in this repository: run `git status --short` and `git diff HEAD`. "
end
wf.log("reviewing " .. what)

-- One agent reads the changes, so the reviewers can stay read-only and
-- never stop to ask for permission.
wf.phase("collect changes")
local changes, err = wf.agent(
	task
		.. "For every changed file give its path and a precise summary of each change with line numbers. "
		.. (sha and "" or "Read each untracked (??) file from git status in full and summarize it as a new file. ")
		.. "Do not review or judge anything.",
	{ agent = "general", description = "collect changes" }
)
if not changes then return "could not read the changes: " .. err end

local lenses = {
	{ "correctness", "logic errors, broken edge cases, wrong error handling, races" },
	{ "security", "unsafe input handling, injection, leaked secrets, missing permission or path checks" },
	{ "simplicity", "needless code, duplicated logic, abstractions nothing uses" },
}

wf.phase("review")
local jobs = {}
for _, lens in ipairs(lenses) do
	jobs[#jobs + 1] = function()
		return wf.agent(
			"Review these changes for "
				.. lens[1]
				.. " problems ("
				.. lens[2]
				.. "). "
				.. "Read the changed code and what it calls before judging. "
				.. "Report only problems these changes introduced; leave pre-existing ones alone, "
				.. "and do not ask for more rigor than the rest of the file has. "
				.. "Report at most 5 findings, most severe first, each as: file:line - problem - why it matters. "
				.. "Report nothing you cannot point to in the code; if nothing is wrong, say so.\n\n"
				.. changes,
			{ agent = "explore", description = lens[1] .. " review" }
		)
	end
end
local reports, errs = wf.parallel(jobs)

local findings = {}
for i, lens in ipairs(lenses) do
	if reports[i] then
		findings[#findings + 1] = "## " .. lens[1] .. "\n" .. reports[i]
	elseif errs and errs[i] then
		wf.log(lens[1] .. " review failed: " .. errs[i])
	end
end

wf.phase("verify")
return wf.agent(
	"Verify these review findings. For each one read the cited code and keep it only if the problem is real "
		.. "and these changes introduced it: reject a finding whose problem is also in code the changes did not touch. "
		.. "Answer with the confirmed findings, most severe first, as: file:line - problem - suggested fix. "
		.. "Then list rejected findings, one line each with the reason.\n\n"
		.. "Changes:\n"
		.. changes
		.. "\n\nFindings:\n"
		.. table.concat(findings, "\n\n"),
	{ agent = "explore", description = "verify findings" }
)
