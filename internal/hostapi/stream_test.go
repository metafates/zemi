package hostapi

import (
	"cmp"
	"testing"

	"github.com/metafates/zemi/internal/tools"
)

// TestStreamLabelReadsEachToolsLeadKey: a streaming row's label is the tool's
// own lead argument, wherever the model put it, and a tool that names none
// shows none.
func TestStreamLabelReadsEachToolsLeadKey(t *testing.T) {
	t.Parallel()
	for tool, pair := range tools.LeadPairs {
		if pair[0] == "" {
			continue
		}
		input := `{"zz":1,"` + pair[0] + `":"x","` + cmp.Or(pair[1], "other") + `":"..."}`
		got, isPath := streamLabel(tool, input)
		if got != "x" || isPath != (pair[0] == "path") {
			t.Errorf("%s: label = %q (path %v), want x", tool, got, isPath)
		}
	}
	if got, _ := streamLabel("mcp__some", `{"about":"a","path":"p"}`); got != "" {
		t.Errorf("a tool with no lead argument labelled %q", got)
	}
}
