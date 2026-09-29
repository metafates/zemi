// digest.go renders the §10.7 memory section: every live memory, newest
// first per scope, under a byte budget per scope.

package learn

import (
	"fmt"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/store"
)

// digestHeader lets a preference the user stated stand unless the session
// or project instructions say otherwise (DESIGN §10.7); the age and the cue
// to check what any other line names keep a stale memory from being
// repeated as fact.
const digestHeader = "# Memory\nLines earlier sessions kept, as [id who age]. Follow a preference the user stated there unless this conversation or the project instructions say otherwise. Any other line held when it was written: check the file, command or flag it names before relying on it."

// lineOverhead is what a rendered line costs beyond its text: the widest
// "- [id origin age] " prefix and the newline.
const lineOverhead = len("- [12345678 learned 365d] \n")

// LineSize is the bytes one memory takes in the digest, which the write
// gate charges against ScopeBudget.
func LineSize(text string) int { return len(text) + lineOverhead }

var scopeTitles = []struct {
	scope store.Scope
	title string
}{{store.ScopeProject, "## this project"}, {store.ScopeUser, "## every project"}}

// Digest renders rows, given newest first, into the framed section: the
// project scope first, each scope cut once its lines reach budget bytes.
// No line at all yields "".
func Digest(rows []*store.Memory, at time.Time, budget int) string {
	var b strings.Builder
	for _, sc := range scopeTitles {
		used, first := 0, true
		for _, m := range rows {
			if m == nil || m.Scope != sc.scope {
				continue
			}
			if used += LineSize(m.Text); used > budget {
				break
			}
			if first {
				b.WriteString("\n\n" + sc.title)
				first = false
			}
			fmt.Fprintf(&b, "\n- [%s %s%s] %s", Truncate(m.ID, 8), m.Origin, Age(m.UpdatedAt, at), m.Text)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return digestHeader + b.String()
}

// Age is " 12d" for a memory last written at updatedMs, and "" when it is
// younger than two days.
func Age(updatedMs int64, at time.Time) string {
	days := int(at.Sub(time.UnixMilli(updatedMs)).Hours() / 24)
	if days < 2 {
		return ""
	}
	return fmt.Sprintf(" %dd", days)
}
