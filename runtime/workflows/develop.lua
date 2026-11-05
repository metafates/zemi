-- Develop a requirement end to end, ready to ship: read from all sides,
-- planned and audited until sound, implemented by workers (parallel
-- worktrees when it splits), merged and unified, then a verify-and-fix
-- loop runs until nothing is left, and the scaffolding cleans itself up.
-- args is the requirement.
local wf = zemi.workflow
local req = type(wf.args) == "string" and wf.args ~= "" and wf.args or nil
if not req then return "develop needs a requirement: /workflows run develop <what to build or change>" end

local cadence = "Commit each verified area as its own lowercase one-line commit; never push. "
	.. "Never leave the tree red: when a commit breaks the checks, revert that commit and fix forward - a red tree is not slept on. "

-- a reply without its verdict line counts as clean, so a confused round
-- ends a loop instead of spinning it
local function flawed(text)
	if type(text) ~= "string" then return false end
	for line in (text .. "\n"):gmatch("(.-)\n") do
		if line:match("^=== VERDICT: flawed") then return true end
	end
	return false
end

wf.phase("understand")
local lenses = {
	{ "happy path", "what done looks like: the user-visible behavior, its inputs and outputs" },
	{ "edges", "boundaries, empty and huge inputs, concurrency, and the values nobody thinks of" },
	{ "failure states", "what can go wrong on the way, and how each failure should surface" },
	{
		"the ground",
		"what the codebase already has for this, where the change lands, and what to reuse instead of add",
	},
}
local jobs = {}
for _, lens in ipairs(lenses) do
	jobs[#jobs + 1] = function()
		return wf.agent(
			"Read this repository with one question only: "
				.. lens[2]
				.. ". Report facts with file:line; judge nothing, plan nothing, change nothing. Say what you could not find.\n\nRequirement: "
				.. req,
			{ agent = "explore", description = lens[1] }
		)
	end
end
local views, uerrs = wf.parallel(jobs)
local found = {}
for i, lens in ipairs(lenses) do
	if views[i] then
		found[#found + 1] = "## " .. lens[1] .. "\n" .. views[i]
	elseif uerrs and uerrs[i] then
		wf.log(lens[1] .. " came back with nothing: " .. uerrs[i])
	end
end
if #found == 0 then return "nothing came back for: " .. req end

wf.phase("plan")
local angles = {
	{ "simplest", "the smallest change that fully meets the requirement" },
	{ "sturdiest", "edge cases, failure states and error handling first" },
	{ "fittest", "fitting the code that is already there, reusing it over adding" },
}
jobs = {}
for _, a in ipairs(angles) do
	jobs[#jobs + 1] = function()
		return wf.agent(
			"Plan how to implement the requirement below, seen through "
				.. a[2]
				.. ". Read the code first. Answer with a numbered list of concrete steps, each with its files and the check that proves it, "
				.. "and what is deliberately left out. No code, no diffs.\n\nRequirement: "
				.. req
				.. "\n\nWhat the repo shows:\n"
				.. table.concat(found, "\n\n"),
			{ agent = "explore", description = "plan: " .. a[1] }
		)
	end
end
local drafts, perrs = wf.parallel(jobs)
local pool = {}
for i, a in ipairs(angles) do
	if drafts[i] then
		pool[#pool + 1] = "## " .. a[1] .. "\n" .. drafts[i]
	elseif perrs and perrs[i] then
		wf.log("the " .. a[1] .. " plan came back with nothing: " .. perrs[i])
	end
end
if #pool == 0 then return "no plan came back for: " .. req end

local plan, cerr = wf.agent(
	"Converge these implementation plans into one. Keep every step at its best version across the drafts, drop duplicates "
		.. "and anything the requirement does not need: the less code the better. "
		.. "Answer with the single plan, the same shape: numbered concrete steps with their files and proving checks, and what is left out.\n\nRequirement: "
		.. req
		.. "\n\nDrafts:\n"
		.. table.concat(pool, "\n\n"),
	{ agent = "explore", description = "converge plans" }
)
if not plan then return "could not converge a plan: " .. cerr end

wf.phase("audit")
for round = 1, 3 do
	local audit = wf.agent(
		"Audit the implementation plan below against its requirement. Read the code it touches before judging. "
			.. "Kill it on holes (a step that cannot work or misses a case the requirement names), security (untrusted input, secrets, permissions), "
			.. "deviation from the requirement, and more code or cleverness than the requirement needs. "
			.. "Pre-existing bugs and messes in this repository are off topic. "
			.. "Answer with a line '=== VERDICT: sound' when the plan is fit to implement, or '=== VERDICT: flawed' followed by one line per issue, "
			.. "most severe first, each as '- <what is wrong and what fixes it>'.\n\nRequirement: "
			.. req
			.. "\n\nWhat the repo shows:\n"
			.. table.concat(found, "\n\n")
			.. "\n\nPlan:\n"
			.. plan,
		{ agent = "explore", description = "audit the plan" }
	)
	if not flawed(audit) then break end
	wf.log("audit round " .. round .. " found flaws; revising")
	local fixed = wf.agent(
		"Revise the plan below so every issue the audit named is answered, in the same shape, the least change per issue. "
			.. "Answer with the full revised plan only.\n\nRequirement: "
			.. req
			.. "\n\nPlan:\n"
			.. plan
			.. "\n\nAudit:\n"
			.. audit,
		{ agent = "explore", description = "revise the plan" }
	)
	plan = fixed or plan
end

wf.phase("split")
local split = wf.agent(
	"Decide whether implementing the plan below parallelizes safely across git worktrees, and set the split up when it does. "
		.. "Read the plan and the files it touches. Split only when zones touch disjoint files and no zone needs another's result: "
		.. "when one must land before another, they are one zone; when in doubt, one zone. At most 4 zones. "
		.. "When there is a split, create every worktree yourself before answering, as "
		.. "`git worktree add <path> -b develop/<zone>` with <path> a sibling directory of this repository named <repo>-<zone>, and commit nothing. "
		.. "Answer with a line '=== ZONES: <count>' followed by one block per zone:\n"
		.. "=== ZONE: <short name>\nworktree: <absolute path>\ntask: <the plan steps this worker owns, and the checks to run>\n"
		.. "When there is no split, answer '=== ZONES: 1' and nothing else.\n\nRequirement: "
		.. req
		.. "\n\nPlan:\n"
		.. plan,
	{ agent = "general", description = "plan the split" }
)
local zones, cur = {}, nil
if type(split) == "string" then
	for line in (split .. "\n"):gmatch("(.-)\n") do
		local n = line:match("^=== ZONES: (%d+)$")
		if n then zones.count = tonumber(n) end
		local z = line:match("^=== ZONE: (.+)$")
		if z then
			cur = { name = z, task = {} }
			zones[#zones + 1] = cur
		elseif cur then
			local wt = line:match("^worktree: (.+)$")
			if wt then
				cur.wt = wt
			else
				cur.task[#cur.task + 1] = line
			end
		end
	end
end
for _, z in ipairs(zones) do
	if not z.wt or not zones.count or zones.count < 2 then
		zones = {}
		break
	end
end
while #zones > 4 do
	table.remove(zones)
end
if #zones > 1 then wf.log(#zones .. " zones, one worker each in its own worktree") end

wf.phase("implement")
local work, branch = {}, "develop/"
local function zoneTask(z)
	return "Implement one zone of an approved plan in the git worktree "
		.. z.wt
		.. " (branch "
		.. branch
		.. z.name
		.. "). Work only inside that worktree; the other zones are other workers' files. "
		.. "Implement exactly your zone of the plan: the least code that works, no unrelated changes, no fixing of pre-existing bugs, no cleanup beyond your zone. "
		.. "Run the checks your zone's task names and the project's own checks for what you touched, and fix what your zone broke. "
		.. cadence
		.. "Answer with what landed and the checks' outcome.\n\nRequirement: "
		.. req
		.. "\n\nPlan:\n"
		.. plan
		.. "\n\nYour zone:\n"
		.. table.concat(z.task, "\n")
end
if #zones > 1 then
	local wjobs = {}
	for _, z in ipairs(zones) do
		wjobs[#wjobs + 1] = function()
			return wf.agent(zoneTask(z), { agent = "general", description = "zone: " .. z.name })
		end
	end
	local zoneReports, zerrs = wf.parallel(wjobs)
	local landed = 0
	for i, z in ipairs(zones) do
		if zoneReports[i] then
			landed = landed + 1
			work[#work + 1] = "## zone " .. z.name .. "\n" .. zoneReports[i]
		elseif zerrs and zerrs[i] then
			wf.log("zone " .. z.name .. " failed: " .. zerrs[i])
		end
	end
	if landed == 0 then
		wf.log("every zone failed; cleaning up")
		table.insert(work, 1, "## plan\n" .. plan)
		return "every zone failed:\n\n" .. table.concat(work, "\n\n")
	end
	local merged, merr = wf.agent(
		"Merge the zones' branches into this working tree: for each branch below bring its commits in (merge, cherry-pick or patches, whatever fits), "
			.. "resolve conflicts, and unify code two or more zones wrote twice into one shared helper - the less code the better. "
			.. "Then run the project's own full checks and fix what the merge broke. "
			.. cadence
			.. "Leave the result as commits on this branch, nothing pushed. Answer with what landed, what you unified, and the checks' outcome.\n\nRequirement: "
			.. req
			.. "\n\nPlan:\n"
			.. plan
			.. "\n\nZones:\n"
			.. table.concat(work, "\n\n"),
		{ agent = "general", description = "merge zones" }
	)
	if merged then
		work[#work + 1] = "## merge\n" .. merged
	else
		wf.log("the merge failed: " .. tostring(merr) .. "; the verify rounds take it from here")
	end
else
	local landed, werr = wf.agent(
		"Implement the approved plan below in this repository, exactly: the least code that works, no unrelated changes, "
			.. "no fixing of pre-existing bugs, no cleanup beyond the plan. "
			.. "Run the project's own checks once the plan's steps are in, and fix what the change broke. "
			.. cadence
			.. "Answer with what landed and the checks' outcome.\n\nRequirement: "
			.. req
			.. "\n\nPlan:\n"
			.. plan,
		{ agent = "general", description = "implement" }
	)
	if not landed then return "implementation failed: " .. werr end
	work[#work + 1] = "## implementation\n" .. landed
end

wf.phase("verify")
local clean, leftover = false, nil
for round = 1, 4 do
	local state, serr = wf.agent(
		"Summarize what changed in this repository: run `git status --short` and `git diff HEAD` (plus `git diff --cached` when anything is staged). "
			.. "For every changed file give its path and each change with line numbers. Do not judge anything.",
		{ agent = "general", description = "collect changes" }
	)
	if not state then
		leftover = "the changes could not be read for verification: " .. tostring(serr)
		wf.log(leftover)
		break
	end
	local lenses = {
		{ "bugs", "explore", "correctness: logic errors, broken edge cases, wrong or missing error handling, races" },
		{
			"break it",
			"general",
			"QA: run the thing and try to break it from its normal paths and its edges; run anything, but change no tracked file",
		},
		{
			"scope",
			"explore",
			"conformance and scope: every plan step done, nothing the requirement did not ask for, no pre-existing bug fixed, no scope creep",
		},
		{
			"gates",
			"general",
			"the mechanical gates: find this project's own check and test commands, run them whole, and report every failure",
		},
	}
	local vjobs = {}
	for _, lens in ipairs(lenses) do
		vjobs[#vjobs + 1] = function()
			return wf.agent(
				"You review a change that implements a requirement, through one lens only: "
					.. lens[3]
					.. ". Read the code before judging; a finding you cannot point to in the code or the run is none. "
					.. "Pre-existing problems outside this change are off topic and left out. "
					.. "Answer with a line '=== VERDICT: clean' when your lens finds nothing, or '=== VERDICT: flawed' followed by one line per finding, most severe first, "
					.. "each as '- file:line - what is wrong - the fix'.\n\nRequirement: "
					.. req
					.. "\n\nPlan:\n"
					.. plan
					.. "\n\nWhat landed:\n"
					.. table.concat(work, "\n\n")
					.. "\n\nThe changes:\n"
					.. state,
				{ agent = lens[2], description = lens[1] }
			)
		end
	end
	local reports, rerrs = wf.parallel(vjobs)
	local trail, bad = {}, false
	for i, lens in ipairs(lenses) do
		if reports[i] then
			trail[#trail + 1] = "## " .. lens[1] .. "\n" .. reports[i]
			if flawed(reports[i]) then bad = true end
		elseif rerrs and rerrs[i] then
			wf.log("the " .. lens[1] .. " review failed: " .. rerrs[i])
		end
	end
	if not bad then
		clean = true
		break
	end
	leftover = table.concat(trail, "\n\n")
	wf.log("verify round " .. round .. " found flaws; fixing")
	local fixed = wf.agent(
		"Fix every finding below in this repository: the least change per finding and its direct cause, no refactoring, "
			.. "no fixing of pre-existing problems. When a finding contradicts the requirement, the requirement wins and you say so. "
			.. "Then run the project's own checks and fix what your fixes broke. "
			.. cadence
			.. "Answer with what you changed and the checks' outcome.\n\nFindings:\n"
			.. leftover,
		{ agent = "general", description = "fix round " .. round }
	)
	work[#work + 1] = "## fix round " .. round .. "\n" .. (fixed or "the fixer came back with nothing")
end

wf.phase("cleanup")
local gone = {}
for _, z in ipairs(zones) do
	gone[#gone + 1] = z.wt .. " (branch " .. branch .. z.name .. ")"
end
local cleaned = wf.agent(
	"Clean up after a development run and touch nothing else. Remove every worktree this run created with "
		.. "`git worktree remove --force <path>` and delete its branch with `git branch -D`, then run `git worktree prune`; "
		.. (#zones > 1 and "they are: " .. table.concat(gone, ", ") .. ". " or "there were no worktrees, so only the prune runs. ")
		.. "Delete stray temp or scratch files the work left in or beside the repository. "
		.. "Never commit, never push, never touch the commits and changes in the working tree, never fix pre-existing messes. "
		.. "Answer with what you removed.",
	{ agent = "general", description = "cleanup" }
)
work[#work + 1] = "## cleanup\n" .. (cleaned or "the cleanup came back with nothing")

wf.phase("report")
local head = clean and "ready to ship" or "stopped with open findings"
return head
	.. ": "
	.. req
	.. "\n\n## plan\n"
	.. plan
	.. "\n\n"
	.. table.concat(work, "\n\n")
	.. (not clean and leftover and ("\n\n## still open after the verify rounds\n" .. leftover) or "")
