-- The knot of the splash and the exit screen: zemi's whorl knot, baked from
-- docs/whorl-knot.js (the torus-knot piece by @bas3line, MIT) on a 28x14 grid,
-- frame 1, its density ramp in one Accent group. Static art: an idle zemi
-- redraws nothing, so the knot does not turn here as it does on the site.
local M = {}

-- the wordmark, three rows of 19 cells
local WORD = {
	"━━┓ ┏━┓ ┏┳┓ ╻",
	"┏━┛ ┣━┛ ┃┃┃ ┃",
	"┗━━ ┗━━ ╹╹╹ ╹",
}

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

--- The knot and the wordmark, the knot left and the word beside its middle.
--- Below room for both, the wordmark alone; below the word, one "zemi" row.
---@param room integer
---@return zemi.Line[]
function M.logo(room)
	local w = 0
	for _, r in ipairs(KNOT) do
		w = math.max(w, #r)
	end
	if room < w + 2 + zemi.ui.width(WORD[1]) then
		if room < zemi.ui.width(WORD[1]) then return { { { text = "zemi", hl = "Title" } } } end
		local rows = {}
		for i, r in ipairs(WORD) do
			rows[i] = { { text = r, hl = "Normal" } }
		end
		return rows
	end
	local top = math.floor((#KNOT - #WORD) / 2)
	local rows = {}
	for i = 1, #KNOT do
		local line = { { text = KNOT[i], hl = "Accent" } }
		if WORD[i - top] then line[2] = { text = string.rep(" ", w - #KNOT[i] + 2) .. WORD[i - top], hl = "Normal" } end
		rows[i] = line
	end
	return rows
end

return M
