-- code the test files share: tests/ runs only its top-level files
local M = {}

function M.add(name, text) assert(zemi.test.run("demo", "add " .. name .. " " .. text)) end

return M
