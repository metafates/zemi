---
name: codescan
description: "Use when one query answers the whole question across many files: every caller of a symbol, how many matches per subject or per directory, which files mention N things, or the same edit applied to many files."
---

# codescan

A scan is one lua script: `zemi.fs.grep(pattern, { path = "dir", include = "*.go" })` answers what a shell grep plus a pile of reads would, and `print` decides what the step costs. Put the whole question in that one script: filter, read and count together, not one script per sub-question. A long list is the one thing that costs as much as the reads it replaced, so print a count, or the first fifty, and only the fields the answer needs. Patterns are Go regular expressions, not Lua ones: `\d` and `(a|b)`, never `%d`.

```lua
for _, h in ipairs(zemi.fs.grep("loadConfig\\(", { include = "*.go" })) do
  print(h.path .. ":" .. h.line .. " " .. h.text)
end
```

Count per file, biggest first:

```lua
local n, order = {}, {}
for _, h in ipairs(zemi.fs.grep("TODO", { include = "*.go" })) do
  if not n[h.path] then order[#order + 1] = h.path end
  n[h.path] = (n[h.path] or 0) + 1
end
table.sort(order, function(a, b) return n[a] > n[b] end)
for _, p in ipairs(order) do print(n[p], p) end
```

Print only the signatures of a file set:

```lua
for _, f in ipairs(zemi.fs.glob("internal/*/*.go")) do
  local src = zemi.fs.read(f)
  for line in (src or ""):gmatch("[^\n]+") do
    if line:match("^func ") then print(f .. ": " .. line) end
  end
end
```

A hit carries only its own line, so when the value you need sits a few lines away, read that file once and print the lines around `h.line`. `zemi.fs.read` answers `nil, err` for a directory or a missing file: check the value before using it, and print what went wrong instead of rerunning the script unchanged. When the output changes the question, the task needed exploring rather than a script, so go back to grep and reads.

The same mechanical edit across many files is one script too: pick the files, read each (a script's read marks it seen, so its own write is accepted), transform and write, and print each file that changed. Lua patterns here: `%-` escapes, `%d`, `%s*`. When the change is hard to undo, print the would-be counts first, check them, then run again with the write in.

```lua
for _, f in ipairs(zemi.fs.glob("internal/*/*.go")) do
  local src = zemi.fs.read(f)
  if src and src:find("oldAPI()", 1, true) then
    zemi.fs.write(f, (src:gsub("oldAPI%(%)", "newAPI()")))
    print(f)
  end
end
```

A write refused with "changed since you read it" means something else touched the file between the read and the write: rerun the script so it reads fresh, never write blind. When the edit needs the model's judgment per file, it is not mechanical: stop the script after the counts and edit those files with the edit tool.
