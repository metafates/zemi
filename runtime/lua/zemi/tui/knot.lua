-- The knot of the splash and the exit screen: zemi's whorl knot, baked from
-- docs/whorl-knot.js (the torus-knot piece by @bas3line, MIT) on a 28x14 grid,
-- frame 1, its density ramp in one Accent group. Static art: an idle zemi
-- redraws nothing, so the knot does not turn here as it does on the site.
local M = {}

local KNOT = {
	"    #%@@%#+",
	"  #%##%#@@@%#+ @%#",
	" #*=::   =*###* #%#%#",
	"+*=:    %   ***- -#%@@",
	"++=:  ##*:-  ++-   +###",
	" ++ @@*+-:   =-:    #%#",
	"   %@#*:     :::-  =@@*",
	"  #%#-  **%%#     %@%*=",
	"  #*=    +#%#@%@%%#%#:",
	"  **==-     *##%**+",
	"   ++=++++**",
	"      --==-",
}

local W = 0
for _, r in ipairs(KNOT) do
	W = math.max(W, #r)
end

--- The knot, or one "zemi" row where the knot does not fit.
---@param room integer
---@return zemi.Line[]
function M.logo(room)
	if room < W then return { { { text = "zemi", hl = "Title" } } } end
	local rows = {}
	for i, r in ipairs(KNOT) do
		rows[i] = { { text = r, hl = "Accent" } }
	end
	return rows
end

return M
