package hostapi

import "testing"

func TestMarkdown(t *testing.T) {
	_, r := newTestHost(t)
	runUser(t, r, `
		local function join(line)
			local s = ""
			for _, seg in ipairs(line) do s = s .. seg.text end
			return s
		end
		local lines = zemi.ui.markdown("# Title\n\nsome **bold** text that wraps\n", 16)
		local got = {}
		for i, l in ipairs(lines) do got[i] = join(l) end
		local want = { "# Title", "", "some bold text", "that wraps" }
		assert(#got == #want, "lines: " .. table.concat(got, "|"))
		for i = 1, #want do
			assert(got[i] == want[i], "line " .. i .. ": " .. got[i])
		end
		assert(lines[1][1].hl:find("^sgr:") ~= nil, "heading hl=" .. tostring(lines[1][1].hl))
		local bold
		for _, seg in ipairs(lines[3]) do
			if seg.text == "bold" then bold = seg end
		end
		assert(bold and bold.hl ~= lines[3][1].hl, "bold run not split out")

		local esc = zemi.ui.markdown("a\27[31mb", 20)
		assert(join(esc[1]) == "ab", "escape leaked: " .. join(esc[1]))
	`)
}
