package hostapi

import (
	"fmt"
	"strings"
	"testing"
)

// TestSpecFieldTypes pins DESIGN §6.0 for option tables: a field of the
// wrong type raises at the caller's line naming the key, never reads as absent.
func TestSpecFieldTypes(t *testing.T) {
	_, r := newTestHost(t)
	cases := []struct{ name, opts, want string }{
		{"absent", `{}`, ""},
		{"the right types", `{ desc = "d", nargs = "?", complete = function() return {} end }`, ""},
		{"a number for a string", `{ nargs = 1 }`, "test.lua:1: nargs must be a string, not a number"},
		{"a string for a function", `{ complete = "x" }`, "test.lua:1: complete must be a function, not a string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runUser(t, r, fmt.Sprintf(`local ok, err = pcall(function() zemi.command.add("t", function() end, %s) end)
				local want = %q
				assert(ok == (want == ""), tostring(err))
				assert(want == "" or tostring(err):find(want, 1, true), tostring(err))`, c.opts, c.want))
		})
	}
}

// TestArgTypes pins the same for a positional argument, which a bare
// assertion used to drop without a word.
func TestArgTypes(t *testing.T) {
	_, r := newTestHost(t)
	cases := []struct{ name, src, want string }{
		{"agent.run on_done", `zemi.agent.run({ prompt = "x" }, "x")`, "zemi.agent.run: the argument must be a function, not a string"},
		{"workflow.run on_done", `zemi.workflow.run({ script = "return 1" }, 1)`, "zemi.workflow.run: the argument must be a function, not a number"},
		{"http.get opts", `zemi.http.get("http://127.0.0.1:1/x", "x")`, "zemi.http.get: the argument must be a table, not a string"},
		{"job.start on_stdout", `zemi.job.start({ cmd = "true", on_stdout = "x" })`, "on_stdout must be a function, not a string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runUser(t, r, fmt.Sprintf(`local ok, err = pcall(function() %s end)
				assert(not ok and tostring(err):find(%q, 1, true), tostring(err))`, c.src, c.want))
		})
	}
}

// dumpItems renders rows as name:value words in name order, keys and
// fields nested: the fixture form the list tests read a row by.
func dumpItems(items []OverlayItem) string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = dumpItem(it)
	}
	return "[" + strings.Join(out, " ") + "]"
}

// dumpFields renders a view's fields as [name value] words.
func dumpFields(fields []Field) string {
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = "[" + f.Name + " " + f.Value + "]"
	}
	return strings.Join(out, " ")
}

// dumpItem is one row in dumpItems' form.
func dumpItem(it OverlayItem) string {
	var words []string
	add := func(k, v string) {
		if v != "" {
			words = append(words, k+":"+v)
		}
	}
	add("copy", it.Copy)
	if it.Danger {
		add("danger", "true")
	}
	add("desc", it.Desc)
	add("detail", it.Detail)
	if len(it.Fields) > 0 {
		add("fields", dumpFields(it.Fields))
	}
	add("fill", it.Fill)
	add("id", it.ID)
	add("tag", it.Tag)
	if len(it.Keys) > 0 {
		add("keys", "["+strings.Join(dumpKeys(it.Keys), " ")+"]")
	}
	if it.Locked != nil {
		locked := "false"
		if *it.Locked {
			locked = "true"
		}
		add("locked", locked)
	}
	add("mark", string(it.Mark))
	if it.On != nil {
		onv := "false"
		if *it.On {
			onv = "true"
		}
		add("on", onv)
	}
	add("preview", it.Preview)
	add("run", it.Run)
	add("section", it.Section)
	if it.Tail {
		add("tail", "true")
	}
	add("text", it.Text)
	add("verb", string(it.Verb))
	return strings.Join(words, " ")
}

// dumpKeys renders a row's keys as name:value words in name order.
func dumpKeys(keys []ItemKey) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		var words []string
		add := func(name, v string) {
			if v != "" {
				words = append(words, name+":"+v)
			}
		}
		if k.Danger {
			add("danger", "true")
		}
		add("desc", k.Desc)
		add("fill", k.Fill)
		add("id", k.ID)
		add("key", k.Key)
		add("run", k.Run)
		add("verb", string(k.Verb))
		out[i] = strings.TrimSpace(strings.Join(words, " "))
	}
	return out
}
