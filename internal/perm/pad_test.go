package perm

import (
	"reflect"
	"strings"
	"testing"
)

// TestPadInlineParity: a name assigned from $TMPDIR is judged as the text
// with $TMPDIR written in its place.
func TestPadInlineParity(t *testing.T) {
	for _, tail := range []string{"cat $P", "rm -rf $P", "cat $P/.env", "ls > $P", "cat < $P", "rm -rf $P/../x", `grep -r x "$P"`, "cd $P && rm -rf build"} {
		for _, v := range []string{"$TMPDIR/a", "$TMPDIR", "${TMPDIR}/a/b", `"$TMPDIR/a"`} {
			a := Normalize("set -e\nP=" + v + "; " + tail)
			b := Normalize("set -e\n" + strings.ReplaceAll(tail, "$P", strings.Trim(strings.ReplaceAll(v, "${TMPDIR}", "$TMPDIR"), `"`)))
			if !reflect.DeepEqual(a.Args, b.Args) || a.expands != b.expands || a.tmpdir != b.tmpdir ||
				!reflect.DeepEqual(a.ReadPaths, b.ReadPaths) || !reflect.DeepEqual(a.WritePaths, b.WritePaths) ||
				a.Destructive("/pad") != b.Destructive("/pad") || a.ForceAsk != b.ForceAsk || a.Assign != b.Assign || a.Opaque != b.Opaque {
				t.Errorf("P=%s; %s\n inlined %+v\n written %+v", v, tail, a, b)
			}
		}
	}
}
