-- Review uncommitted changes (or the diff against args, a git ref) from three
-- angles in parallel, then keep only the findings that hold up in the code.
local base = type(args) == "string" and args ~= "" and args or nil
local diff = base and ("git diff " .. base) or "git diff HEAD"

-- One agent reads the changes, so the reviewers can stay read-only and
-- never stop to ask for permission.
phase("collect changes")
local changes, err = agent(
  "List the changes in this repository: run `git status --short` and `" .. diff .. "`. "
    .. "For every changed file give its path and a precise summary of each change with line numbers. "
    .. "Do not review or judge anything.",
  { agent = "general", description = "collect changes" }
)
if not changes then
  return "could not read the changes: " .. err
end

local lenses = {
  { "correctness", "logic errors, broken edge cases, wrong error handling, races" },
  { "security", "unsafe input handling, injection, leaked secrets, missing permission or path checks" },
  { "simplicity", "needless code, duplicated logic, abstractions nothing uses" },
}

phase("review")
local jobs = {}
for _, lens in ipairs(lenses) do
  jobs[#jobs + 1] = function()
    return agent(
      "Review these changes for " .. lens[1] .. " problems (" .. lens[2] .. "). "
        .. "Read the changed code and what it calls before judging. "
        .. "Report at most 5 findings, most severe first, each as: file:line - problem - why it matters. "
        .. "Report nothing you cannot point to in the code; if nothing is wrong, say so.\n\n"
        .. changes,
      { agent = "explore", description = lens[1] .. " review" }
    )
  end
end
local reports, errs = parallel(jobs)

local findings = {}
for i, lens in ipairs(lenses) do
  if reports[i] then
    findings[#findings + 1] = "## " .. lens[1] .. "\n" .. reports[i]
  elseif errs and errs[i] then
    log(lens[1] .. " review failed: " .. errs[i])
  end
end

phase("verify")
return agent(
  "Verify these review findings. For each one read the cited code and keep it only if the problem is real. "
    .. "Answer with the confirmed findings, most severe first, as: file:line - problem - suggested fix. "
    .. "Then list rejected findings, one line each with the reason.\n\n"
    .. "Changes:\n" .. changes .. "\n\nFindings:\n" .. table.concat(findings, "\n\n"),
  { agent = "explore", description = "verify findings" }
)
