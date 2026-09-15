package hostapi

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// validPatch is the control input the refusal table is measured against.
const validPatch = `{"action":"patch","title":"t","rules":[
	{"subject":"tool.bash","pattern":"rm -rf *","decision":"deny","match":"rm -rf build"}]}`

func mustPatch(t *testing.T, h *Host, sid, raw string) *cfgProposal {
	t.Helper()
	p, err := h.intakeConfigPatch(sid, json.RawMessage(raw))
	if err != nil {
		t.Fatalf("intakeConfigPatch: %v", err)
	}
	return p
}

// TestConfigPatchIntakeRefuses covers the strict §10.3 intake: only
// well-formed rule objects inside a patch get through; anything else
// (unknown fields, non-rule content, bad enums, broken examples) refuses.
func TestConfigPatchIntakeRefuses(t *testing.T) {
	h, _ := newTestHostDB(t)
	cases := []struct{ name, doc string }{
		{"wrong action", `{"action":"load","title":"t","rules":[` + ruleObj(`"deny"`, "") + `]}`},
		{"no rules", `{"action":"patch","title":"t","rules":[]}`},
		{"unknown patch field", `{"action":"patch","title":"t","extra":1,"rules":[` + ruleObj("deny", "") + `]}`},
		{"unknown rule field", `{"action":"patch","title":"t","rules":[{"subject":"tool.bash","pattern":"a*","decision":"deny","match":"a1","handler":"function() end"}]}`},
		{"bad decision", `{"action":"patch","title":"t","rules":[` + ruleObj("sudo", "") + `]}`},
		{"unknown decision string", `{"action":"patch","title":"t","rules":[` + ruleObj(`"foo"`, "") + `]}`},
		{"bad scope", `{"action":"patch","title":"t","rules":[` + ruleObj("deny", `"scope":"global",`) + `]}`},
		{"match example not matching", `{"action":"patch","title":"t","rules":[{"subject":"tool.bash","pattern":"rm -rf *","decision":"deny","match":"git push"}]}`},
		{"not_match without both examples", `{"action":"patch","title":"t","rules":[{"subject":"tool.bash","pattern":"rm -rf *","decision":"deny","match":"rm -rf build","not_match":"rm -f build"}]}`},
		{"empty subject", `{"action":"patch","title":"t","rules":[{"subject":"","pattern":"a*","decision":"deny","match":"a1"}]}`},
		{"rule not an object", `{"action":"patch","title":"t","rules":["deny everything"]}`},
		{"rules not an array", `{"action":"patch","title":"t","rules":{"0":{"subject":"s","pattern":"a*","decision":"deny","match":"a1"}}}`},
	}
	for _, tc := range cases {
		if _, err := h.intakeConfigPatch("s1", json.RawMessage(tc.doc)); err == nil {
			t.Fatalf("%s: accepted", tc.name)
		}
	}
	mustPatch(t, h, "s1", validPatch)
}

func ruleObj(decision, extra string) string {
	return `{"subject":"tool.bash","pattern":"a*","decision":` + decision + `,` + extra + `"match":"a1"}`
}

// TestConfigPatchQueuedUnderAsk drives the reflector end-to-end with a
// config patch: ask autonomy queues it with Go-stamped provenance and
// scrubbed leaves, nothing auto-applies.
func TestConfigPatchQueuedUnderAsk(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Settings.SetHost("config.autonomy", "ask")
	sid := seedSession(t, h)
	seedTranscript(t, h, sid)
	doc := `{"config":[{"action":"patch","title":"block ghp_0123456789abcdefghij012345 leaks","risk":"safe",
		"rules":[{"subject":"tool.bash","pattern":"rm -rf *","decision":"deny","match":"rm -rf build"}]}]}`
	reflectStub(t, h, doc)

	if err := h.reflectSession(context.Background(), sid); err != nil {
		t.Fatalf("reflectSession: %v", err)
	}
	h.agents.mu.Lock()
	n := len(h.agents.cfgProposals)
	var p *cfgProposal
	if n > 0 {
		p = h.agents.cfgProposals[0]
	}
	h.agents.mu.Unlock()
	if n != 1 || p == nil {
		t.Fatalf("queued patches=%d", n)
	}
	if p.SessionID != sid {
		t.Fatalf("provenance=%q want %q", p.SessionID, sid)
	}
	if !p.Scrubbed || strings.Contains(p.Title, "ghp_") {
		t.Fatalf("secret not scrubbed: %q", p.Title)
	}
	if rules := h.Perm.Rules(); len(rules) != 0 {
		t.Fatalf("ask mode must not apply: %d rules", len(rules))
	}
	if got := cfgRuleLines(p.Entries); len(got) != 1 || got[0] != "+ deny tool.bash rm -rf *" {
		t.Fatalf("lines: %q", got)
	}
}

// TestConfigPatchAutoApplyUnderAuto covers the §10.4 allowlist: under
// config.autonomy=auto only session-scope deny/ask patches auto-apply;
// permanent scope or allow rules queue; the per-session rule cap queues.
func TestConfigPatchAutoApplyUnderAuto(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Settings.SetHost("config.autonomy", "auto")
	sid := "sess-1"

	deny := `{"action":"patch","title":"d","rules":[
		{"subject":"tool.bash","pattern":"rm -rf *","decision":"deny","match":"rm -rf build"}]}`
	perm := `{"action":"patch","title":"p","rules":[
		{"subject":"tool.bash","pattern":"rm -rf *","decision":"deny","scope":"permanent","match":"rm -rf build"}]}`
	allow := `{"action":"patch","title":"a","rules":[
		{"subject":"fs.read","pattern":"/tmp/*","decision":"allow","match":"/tmp/x"}]}`

	prop := func(raw ...string) *reflectProposal {
		p := &reflectProposal{}
		for _, r := range raw {
			p.Config = append(p.Config, json.RawMessage(r))
		}
		return p
	}

	queued, applied, skipped := h.applyConfigProposals(sid, prop(deny))
	if queued != 0 || applied != 1 || skipped != 0 {
		t.Fatalf("deny patch routing: queued=%d applied=%d skipped=%d", queued, applied, skipped)
	}
	if len(h.Perm.Rules()) != 1 {
		t.Fatalf("applied rules=%d", len(h.Perm.Rules()))
	}
	if h.agents.cfgAuto != 1 {
		t.Fatalf("cfgAuto=%d", h.agents.cfgAuto)
	}

	queued, applied, skipped = h.applyConfigProposals(sid, prop(perm, allow))
	if queued != 2 || applied != 0 || skipped != 0 {
		t.Fatalf("permanent/allow routing: queued=%d applied=%d skipped=%d", queued, applied, skipped)
	}
	if len(h.Perm.Rules()) != 1 {
		t.Fatalf("permanent/allow must not apply: %d rules", len(h.Perm.Rules()))
	}

	h.agents.mu.Lock()
	h.agents.cfgAuto = rulesAutoApplyCap
	h.agents.mu.Unlock()
	if _, applied, _ = h.applyConfigProposals(sid, prop(deny)); applied != 0 {
		t.Fatal("cap exhausted but patch still auto-applies")
	}
}

// TestConfigReviewAcceptPermanent covers the review overlay path: an
// accepted patch applies (permanent rule lands in the dump and the
// revisions table with the learn commit message), a rejected one is
// dropped, undecided ones stay queued.
func TestConfigReviewAcceptPermanent(t *testing.T) {
	h, _ := newTestHostSkills(t)
	accept := &fakeUI{cfgAcceptAll: true}
	h.SetUIHooks(accept)

	p := mustPatch(t, h, "sess-9", `{"action":"patch","title":"Deny rm -rf in bash","rules":[
		{"subject":"tool.bash","pattern":"rm -rf *","decision":"deny","scope":"permanent",
			 "match":"rm -rf build","not_match":"rm -f build","not_match_example":"rm -f build"}]}`)
	h.queueCfgProposal(p)

	h.openConfigReview()

	rules := h.Perm.Rules()
	if len(rules) != 1 || rules[0].Subject != "tool.bash" {
		t.Fatalf("accepted rules: %+v", rules)
	}
	data, err := os.ReadFile(h.permanentRulesPath())
	if err != nil {
		t.Fatalf("permissions.lua not written: %v", err)
	}
	parsed, err := parsePermissionsDump(data)
	if err != nil || len(parsed) != 1 || parsed[0].Pattern != "rm -rf *" {
		t.Fatalf("dump: %q err=%v", data, err)
	}
	h.agents.mu.Lock()
	left := len(h.agents.cfgProposals)
	h.agents.mu.Unlock()
	if left != 0 {
		t.Fatalf("accepted patch should leave the queue: %d left", left)
	}

	revs, err := h.GlobalDB.ListRevisions()
	if err != nil || len(revs) != 1 {
		t.Fatalf("revisions: %v %d", err, len(revs))
	}
	wantTitle := "learn: Deny rm -rf in bash (from sess-9)"
	if revs[0].Title != wantTitle || revs[0].Status != "applied" || revs[0].AppliedBy != "review" {
		t.Fatalf("revision: %+v", revs[0])
	}
	if _, err := exec.LookPath("git"); err == nil {
		out, err := exec.Command("git", "-C", h.Dirs.ConfigHome, "log", "--oneline").CombinedOutput()
		if err != nil {
			t.Fatalf("config repo history: %v", err)
		}
		if !strings.Contains(string(out), wantTitle) {
			t.Fatalf("commit missing from history: %q", out)
		}
	}
}

// TestConfigReviewRejectAndLeave covers the negative verdicts: rejected
// patches are dropped without applying; closing with esc (no verdict)
// leaves the patch queued.
func TestConfigReviewRejectAndLeave(t *testing.T) {
	h, _ := newTestHostSkills(t)
	h.SetUIHooks(&fakeUI{cfgRejectAll: true})

	p1 := mustPatch(t, h, "sess-2", `{"action":"patch","title":"reject me","rules":[
		{"subject":"tool.bash","pattern":"rm -rf *","decision":"deny","match":"rm -rf build"}]}`)
	h.queueCfgProposal(p1)
	h.openConfigReview()
	if rules := h.Perm.Rules(); len(rules) != 0 {
		t.Fatalf("rejected patch applied: %+v", rules)
	}
	h.agents.mu.Lock()
	left := len(h.agents.cfgProposals)
	h.agents.mu.Unlock()
	if left != 0 {
		t.Fatalf("rejected patch not dropped: %d left", left)
	}

	h.SetUIHooks(&fakeUI{})
	p2 := mustPatch(t, h, "sess-2", `{"action":"patch","title":"keep me","rules":[
		{"subject":"tool.curl","pattern":"*","decision":"deny","match":"curl https://x"}]}`)
	h.queueCfgProposal(p2)
	h.openConfigReview()
	h.agents.mu.Lock()
	left = len(h.agents.cfgProposals)
	h.agents.mu.Unlock()
	if left != 1 {
		t.Fatalf("undecided patch must stay queued: %d left", left)
	}
}

// TestRevisionRevert covers :revision revert end-to-end: an applied
// permanent-rule revision reverts (dump back to empty, revert row
// recorded, original marked reverted); unknown or non-applied ids refuse.
func TestRevisionRevert(t *testing.T) {
	h, _ := newTestHostSkills(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}

	p := mustPatch(t, h, "sess-7", `{"action":"patch","title":"Deny rm -rf in bash","rules":[
		{"subject":"tool.bash","pattern":"rm -rf *","decision":"deny","scope":"permanent",
		 "match":"rm -rf build","not_match":"rm -f build","not_match_example":"rm -f build"}]}`)
	h.SetUIHooks(&fakeUI{cfgAcceptAll: true})
	h.queueCfgProposal(p)
	h.openConfigReview()

	revs, err := h.GlobalDB.ListRevisions()
	if err != nil || len(revs) != 1 || revs[0].Commit == "" {
		t.Fatalf("revision with commit: %v %+v", err, revs)
	}

	if err := h.revertRevision(revs[0].ID); err != nil {
		t.Fatalf("revertRevision: %v", err)
	}

	// The reverted revision was the commit that introduced the dump, so
	// the revert removes the file; that is the rule being gone.
	if _, err := os.ReadFile(h.permanentRulesPath()); !os.IsNotExist(err) {
		t.Fatalf("dump should be gone after revert: %v", err)
	}
	revs, err = h.GlobalDB.ListRevisions()
	if err != nil || len(revs) != 2 {
		t.Fatalf("revisions after revert: %v %d", err, len(revs))
	}
	if revs[0].Title != "revert: learn: Deny rm -rf in bash (from sess-7)" || revs[0].Status != "applied" {
		t.Fatalf("revert row: %+v", revs[0])
	}
	if revs[1].Status != "reverted" {
		t.Fatalf("original row status: %+v", revs[1])
	}

	if err := h.revertRevision(revs[1].ID); err == nil {
		t.Fatal("reverting a reverted revision must refuse")
	}
	if err := h.revertRevision("no-such-id"); err == nil {
		t.Fatal("unknown revision must refuse")
	}
}
