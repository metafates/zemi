-- The knot of the splash and the exit screen: zemi's whorl knot, baked from
-- docs/whorl-knot.js (the torus-knot piece by @bas3line, MIT) on a 28x14 grid,
-- frame 1. Each cell keeps the renderer's shade, the 0-31 index the landing
-- feeds through its audited ramp, which the Knot groups draw in the theme's
-- colors. Static art: an idle zemi redraws nothing, so the knot does not turn.
local M = {}

-- glyphs and the cells' shades (base 32), one row each
local KNOT = {
	{ "    #%@@%#+            ", "0000mrtuqoh000000000000" },
	{ "  #%##%#@@@%#+ @%#     ", "00nqopqovvuqofvvqo00000" },
	{ " #*=::   =*###* #%#%#  ", "0nlc42000bjpomiupqoto00" },
	{ "+*=:    %   ***- -#%@@ ", "hid20000qpbjiki809mqvv0" },
	{ "++=:  ##*:-  ++-   +###", "efa200ooj5700gg9000gnpm" },
	{ " ++ @@*+-:   =-:    #%#", "0gf9uvlga2000b940000nsn" },
	{ "   %@#*:     :::-  =@@*", "00gsumi2ng000222600ctvi" },
	{ "  #%#-  **%%#     %@%*=", "00prn7sokiqpn0dge0qvrjd" },
	{ "  #*=    +#%#@%@%%#%#: ", "00nlc0000fnsousvqppqm60" },
	{ "  **==-     *##%**+    ", "00jicb80000ninnrlle0000" },
	{ "   ++=++++**           ", "000efdefhhikik500000000" },
	{ "      --==-            ", "0000008acc9000000000000" },
}

local W = 0
for _, r in ipairs(KNOT) do
	W = math.max(W, #r[1])
end

local function rowSegs(row)
	local glyphs, shades = row[1], row[2]
	local last = #glyphs
	while last > 0 and glyphs:sub(last, last) == " " do
		last = last - 1
	end
	local line, at = {}, 1
	while at <= last do
		local j = at
		while j < last and shades:sub(j + 1, j + 1) == shades:sub(at, at) do
			j = j + 1
		end
		line[#line + 1] = { text = glyphs:sub(at, j), hl = "Knot" .. tonumber(shades:sub(at, at), 32) }
		at = j + 1
	end
	return line
end

--- The knot, or one "zemi" row where the knot does not fit.
---@param room integer
---@return zemi.Line[]
function M.logo(room)
	if room < W then return { { { text = "zemi", hl = "Title" } } } end
	local rows = {}
	for i, row in ipairs(KNOT) do
		rows[i] = rowSegs(row)
	end
	return rows
end

return M
