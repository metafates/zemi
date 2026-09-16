package hostapi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
)

// TestPermDumpRoundTrip pins the serialization contract: dump parses back
// to the same rules and is deterministic across runs.
func TestPermDumpRoundTrip(t *testing.T) {
	in := []perm.Rule{
		{Subject: "tool.bash", Pattern: "rm -rf *", Decision: perm.Deny,
			NotMatch: "rm -f build", Scope: perm.ScopePermanent},
		{Subject: "fs.read", Pattern: "/etc/*", Decision: perm.Ask,
			Principal: "model", Scope: perm.ScopePermanent},
		{Subject: "net.host", Pattern: "example.com", Decision: perm.Allow,
			Scope: perm.ScopeSession},
	}
	dumped := permDump(permanentRules(in))
	if strings.Contains(string(dumped), "net.host") {
		t.Fatal("dump must hold permanent rules only")
	}
	out, err := parsePermissionsDump(dumped)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("rules=%d, want 2", len(out))
	}
	if out[0].Subject != "fs.read" || out[0].Principal != "model" || out[0].Decision != perm.Ask {
		t.Fatalf("rule[0]: %+v", out[0])
	}
	if out[1].NotMatch != "rm -f build" {
		t.Fatalf("rule[1]: %+v", out[1])
	}
	again := permDump(permanentRules(in))
	if string(dumped) != string(again) {
		t.Fatal("dump is not deterministic")
	}
}

// TestParsePermissionsDumpRefuses covers fail-closed parsing: executable
// Lua, unknown fields, wrong values, and truncated content are all
// refused before anything could evaluate.
func TestParsePermissionsDumpRefuses(t *testing.T) {
	cases := map[string]string{
		"executable":     "return { version = 1, rules = (function() return {} end)() }",
		"os call":        "return { version = 1, rules = { { subject = os.getenv(\"HOME\"), pattern = \"x\", decision = \"deny\", scope = \"permanent\" } } }",
		"concat":         "return { version = 1, rules = { { subject = \"a\" .. \"b\", pattern = \"x\", decision = \"deny\", scope = \"permanent\" } } }",
		"unknown field":  "return { version = 1, rules = { { subject = \"a\", pattern = \"b\", decision = \"deny\", scope = \"permanent\", extra = \"x\" } } }",
		"unknown key":    "return { version = 1, evil = {}, rules = {} }",
		"bad decision":   "return { version = 1, rules = { { subject = \"a\", pattern = \"b\", decision = \"maybe\", scope = \"permanent\" } } }",
		"session scope":  "return { version = 1, rules = { { subject = \"a\", pattern = \"b\", decision = \"deny\", scope = \"session\" } } }",
		"missing fields": "return { version = 1, rules = { { subject = \"a\" } } }",
		"bad version":    "return { version = 2, rules = {} }",
		"truncated":      "return { version = 1, rules = { { subject = \"a\"",
		"not lua":        "drop table rules;",
		"empty":          "",
	}
	for name, src := range cases {
		if rules, err := parsePermissionsDump([]byte(src)); err == nil {
			t.Fatalf("%s: parsed to %d rules, want error", name, len(rules))
		}
	}
}

// TestPermissionsLoadStep covers the boot-window load: a valid dump loads
// into the store once however often it replays, an invalid dump fails the
// boot with the store empty.
func TestPermissionsLoadStep(t *testing.T) {
	h, r := newTestHostSkillsNoBoot(t)
	path := h.permanentRulesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	dump := permDump([]perm.Rule{{
		Subject: "tool.bash", Pattern: "git status",
		Decision: perm.Allow, Scope: perm.ScopePermanent,
	}})
	if err := os.WriteFile(path, dump, 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 { // a reload replays the step
		if err := r.Boot(context.Background(), []lua.BootStep{h.PermissionsLoadStep()}); err != nil {
			t.Fatalf("load step: %v", err)
		}
	}
	rules := h.Perm.Rules()
	if len(rules) != 1 || rules[0].Pattern != "git status" || rules[0].Scope != perm.ScopePermanent {
		t.Fatalf("rules after load: %+v", rules)
	}

	h2, r2 := newTestHostSkillsNoBoot(t)
	if err := os.WriteFile(h2.permanentRulesPath(), []byte("return { evil() }"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := r2.Boot(context.Background(), []lua.BootStep{h2.PermissionsLoadStep()})
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("invalid dump boot: %v", err)
	}
	if rules := h2.Perm.Rules(); len(rules) != 0 {
		t.Fatalf("fail-closed: store has %d rules", len(rules))
	}
}

// TestPermissionPermanentScope covers the Lua surface: user origin may
// add permanent rules (the dump regenerates), other origins may not, and
// allow rules are user-config-only per DESIGN §5/§6.1.
func TestPermissionPermanentScope(t *testing.T) {
	h, r := newTestHostSkills(t)

	runUser(t, r, `zemi.permission.rule({
		subject = "tool.bash", pattern = "rm -rf *", decision = "deny",
		match = "rm -rf build", not_match = "rm -f build",
		not_match_example = "rm -f build",
		scope = "permanent",
	})`)
	data, err := os.ReadFile(h.permanentRulesPath())
	if err != nil {
		t.Fatalf("permissions.lua not written: %v", err)
	}
	rules, err := parsePermissionsDump(data)
	if err != nil || len(rules) != 1 || rules[0].Pattern != "rm -rf *" {
		t.Fatalf("dump after permanent rule: %q err=%v", data, err)
	}

	if _, err := exec.LookPath("git"); err == nil {
		out, err := exec.Command("git", "-C", h.Dirs.ConfigHome, "log", "--oneline").CombinedOutput()
		if err != nil {
			t.Fatalf("config repo has no history: %v", err)
		}
		if !strings.Contains(string(out), "rules: deny tool.bash rm -rf *") {
			t.Fatalf("commit missing from history: %q", out)
		}
	}
	revs, err := h.GlobalDB.ListRevisions()
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	if len(revs) != 1 || revs[0].Status != "applied" || revs[0].AppliedBy != "user" {
		t.Fatalf("revisions: %+v", revs)
	}

	err = r.ExecuteChunk(context.Background(), `zemi.permission.rule({
		subject = "tool.bash", pattern = "x", decision = "deny",
		match = "x", scope = "permanent",
	})`, "project.lua", lua.OriginProject)
	if err == nil || !strings.Contains(err.Error(), "permanent rules may only come from user config") {
		t.Fatalf("project-origin permanent rule: %v", err)
	}
	err = r.ExecuteChunk(context.Background(), `zemi.permission.rule({
		subject = "tool.bash", pattern = "git status", decision = "allow",
		match = "git status",
	})`, "project.lua", lua.OriginProject)
	if err == nil || !strings.Contains(err.Error(), "allow rules may only come from user config") {
		t.Fatalf("project-origin allow rule: %v", err)
	}

	runUser(t, r, `zemi.permission.rule({
		subject = "tool.bash", pattern = "rm -rf *", decision = "deny",
		match = "rm -rf /tmp/x",
	})`)
	data2, _ := os.ReadFile(h.permanentRulesPath())
	if string(data) != string(data2) {
		t.Fatal("session rule must not touch the dump")
	}
}
