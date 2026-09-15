// configrev.go is the §10.3-§10.4 config-patch pipeline: the reflector
// may propose permission rules as structured rule objects only (never
// Lua), intake validates and scrubs every leaf, proposals park in the
// session-scoped review queue, and accepting one applies the rules
// through the same paths as zemi.permission.rule (session rules to the
// perm store, permanent ones through the §6.1 review commit).
package hostapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
)

// rulesAutoApplyCap bounds model-proposed rules stored without review per
// session (DESIGN §10.4, mirroring the memory cap); cfgTitleMax caps the
// display title and cfgRulesMax one patch's size.
const (
	rulesAutoApplyCap = 20
	cfgTitleMax       = 200
	cfgRulesMax       = 16
)

// ConfigRow is one review-overlay row: a pending config patch rendered as
// rule lines (the patch is all additions in v1, so the card is the diff).
type ConfigRow struct {
	ID       string
	Title    string
	From     string // provenance session id, stamped by Go at intake
	Scrubbed bool   // a secret-shaped leaf was redacted at intake
	Lines    []string
}

// cfgProposal is one validated config patch parked for review.
type cfgProposal struct {
	ID        string
	Title     string
	SessionID string
	Scrubbed  bool
	Entries   []cfgRuleEntry
}

// cfgRuleEntry pairs a validated rule with its intake examples (the
// examples prove the pattern's shape but are not stored as rules).
type cfgRuleEntry struct {
	Rule       perm.Rule
	MatchEx    string
	NotMatchEx string
}

// rawCfgPatch mirrors the model-facing schema; unknown fields are refused
// (DESIGN §10.3: a config patch is rule objects, anything else inside it
// is refused at intake).
type rawCfgPatch struct {
	Action string            `json:"action"`
	Title  string            `json:"title"`
	Rules  []json.RawMessage `json:"rules"`
	Risk   string            `json:"risk"` // display-only, never trusted
}

type rawCfgRule struct {
	Subject         string `json:"subject"`
	Pattern         string `json:"pattern"`
	Decision        string `json:"decision"`
	NotMatch        string `json:"not_match"`
	Scope           string `json:"scope"`
	Match           string `json:"match"`
	NotMatchExample string `json:"not_match_example"`
}

// decodeStrict decodes exactly one JSON value with unknown fields
// refused.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// intakeConfigPatch validates one model-proposed config patch. Every
// string leaf is scrubbed before validation (the scrubbed text is what
// gets validated, stored and committed); any field outside the schema
// refuses the whole patch.
func (h *Host) intakeConfigPatch(sid string, raw json.RawMessage) (*cfgProposal, error) {
	var rp rawCfgPatch
	if err := decodeStrict(raw, &rp); err != nil {
		return nil, fmt.Errorf("config patch refused: %w", err)
	}
	if rp.Action != "patch" {
		return nil, fmt.Errorf("config patch refused: action %q, only %q exists", rp.Action, "patch")
	}
	if len(rp.Rules) == 0 {
		return nil, errors.New("config patch refused: no rules")
	}
	if len(rp.Rules) > cfgRulesMax {
		return nil, fmt.Errorf("config patch refused: %d rules, max %d", len(rp.Rules), cfgRulesMax)
	}
	title, scrubbed := learn.ScrubSecrets(strings.TrimSpace(rp.Title))
	if title == "" {
		title = "config patch"
	}
	p := &cfgProposal{ID: store.NewID(), Title: clipRunes(title, cfgTitleMax), SessionID: sid, Scrubbed: scrubbed}
	for i, rr := range rp.Rules {
		e, didScrub, err := intakeCfgRule(rr)
		if err != nil {
			return nil, fmt.Errorf("rule %d: %w", i+1, err)
		}
		p.Scrubbed = p.Scrubbed || didScrub
		p.Entries = append(p.Entries, e)
	}
	return p, nil
}

// intakeCfgRule validates one rule object: the same shape rules the
// zemi.permission.rule namespace accepts, with examples required exactly
// as there (the match example must match, a not_match needs both).
func intakeCfgRule(raw json.RawMessage) (cfgRuleEntry, bool, error) {
	var rr rawCfgRule
	if err := decodeStrict(raw, &rr); err != nil {
		return cfgRuleEntry{}, false, fmt.Errorf("rule object refused: %w", err)
	}
	scrubbed := false
	scrub := func(s string) string {
		out, did := learn.ScrubSecrets(strings.TrimSpace(s))
		if did {
			scrubbed = true
		}
		return out
	}
	r := perm.Rule{
		Subject:  scrub(rr.Subject),
		Pattern:  scrub(rr.Pattern),
		Decision: perm.Decision(rr.Decision),
		NotMatch: scrub(rr.NotMatch),
		Scope:    perm.Scope(strings.TrimSpace(rr.Scope)),
	}
	matchEx, notMatchEx := scrub(rr.Match), scrub(rr.NotMatchExample)
	if err := validateRule(&r, matchEx, notMatchEx); err != nil {
		return cfgRuleEntry{}, scrubbed, err
	}
	return cfgRuleEntry{Rule: r, MatchEx: matchEx, NotMatchEx: notMatchEx}, scrubbed, nil
}

// applyCfgProposal adds the rules to the perm store; permanent rules go
// through permRegenerate and a review commit keyed to the reflected
// session ("the keypress is the review", §6.1 - here the keypress is the
// accept in the review card). Commit failures are logged, the rules and
// the dump stay.
func (h *Host) applyCfgProposal(p *cfgProposal) {
	for _, e := range p.Entries {
		h.Perm.Add(e.Rule)
		if e.Rule.Scope != perm.ScopePermanent {
			continue
		}
		if err := h.persistPermanentRule(e.Rule, "learn: "+p.Title+" (from "+p.SessionID+")", "review"); err != nil {
			h.Log.Warn("config-rev", "dump regenerate failed", map[string]any{"error": err.Error()})
		}
	}
	h.memNotify("info", "config", "applied: "+p.Title+redactNote(p.Scrubbed))
	h.EmitAsync("config.applied", map[string]any{"id": p.ID, "title": p.Title})
}

// autoApplyCfgOK reports whether a patch may apply without review and, if
// so, spends its rules from the session cap (DESIGN §10.4): config.autonomy
// is "auto" and every rule is a session-scope deny/ask. Permanent scope
// always requires the review commit; allow rules widen and never auto-apply.
func (h *Host) autoApplyCfgOK(p *cfgProposal) bool {
	if h.setStr("config.autonomy") != "auto" {
		return false
	}
	for _, e := range p.Entries {
		if e.Rule.Scope != perm.ScopeSession || (e.Rule.Decision != perm.Deny && e.Rule.Decision != perm.Ask) {
			return false
		}
	}
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if ag.cfgAuto+len(p.Entries) > rulesAutoApplyCap {
		return false
	}
	ag.cfgAuto += len(p.Entries)
	return true
}

// queueCfgProposal parks a validated patch for review.
func (h *Host) queueCfgProposal(p *cfgProposal) {
	ag := h.agents
	ag.mu.Lock()
	ag.cfgProposals = append(ag.cfgProposals, p)
	ag.mu.Unlock()
	h.refreshPending()
	h.EmitAsync("config.proposed", map[string]any{"id": p.ID, "title": p.Title})
}

// takeCfgProposal removes and returns one queued patch by id, or nil.
func (h *Host) takeCfgProposal(id string) *cfgProposal {
	ag := h.agents
	ag.mu.Lock()
	var taken *cfgProposal
	for i, p := range ag.cfgProposals {
		if p.ID == id {
			ag.cfgProposals = append(ag.cfgProposals[:i], ag.cfgProposals[i+1:]...)
			taken = p
			break
		}
	}
	ag.mu.Unlock()
	if taken != nil {
		h.refreshPending()
	}
	return taken
}

// cfgRows snapshots the pending patches as review rows.
func (h *Host) cfgRows() []ConfigRow {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	rows := make([]ConfigRow, 0, len(ag.cfgProposals))
	for _, p := range ag.cfgProposals {
		rows = append(rows, ConfigRow{
			ID: p.ID, Title: p.Title, From: p.SessionID, Scrubbed: p.Scrubbed,
			Lines: cfgRuleLines(p.Entries),
		})
	}
	return rows
}

// cfgRuleLines renders the patch body: one line per added rule, the
// not_match exclusion on an indented follow-up.
func cfgRuleLines(entries []cfgRuleEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, "+ "+string(e.Rule.Decision)+" "+e.Rule.Subject+" "+e.Rule.Pattern)
		if e.Rule.NotMatch != "" {
			out = append(out, "    except "+e.Rule.NotMatch)
		}
	}
	return out
}

// configCommand dispatches /config: no args opens the review overlay on
// its own goroutine (the UI goroutine must not block on the hook),
// "revisions" lists the audit trail, "revert <id>" undoes one applied
// revision.
func (h *Host) configCommand(args string) error {
	fields := strings.Fields(args)
	switch {
	case len(fields) == 0:
		if h.cfgReview.TryLock() {
			h.GoSafe("config review", func() {
				defer h.cfgReview.Unlock()
				h.openConfigReview()
			})
			return nil
		}
		return errors.New("config review already open")
	case fields[0] == "revisions" && len(fields) == 1:
		return h.listRevisions()
	case fields[0] == "revert" && len(fields) == 2:
		return h.revertRevision(fields[1])
	default:
		return errors.New("usage: /config | /config revisions | /config revert <id>")
	}
}

// openConfigReview shows pending patches and applies the verdict:
// accepted patches apply immediately, rejected ones are dropped,
// undecided ones stay queued (they die with the process, like memory
// proposals).
func (h *Host) openConfigReview() {
	ui := h.hooks()
	if ui == nil {
		h.memNotify("warn", "config", "review UI unavailable")
		return
	}
	rows := h.cfgRows()
	if len(rows) == 0 {
		h.memNotify("info", "config", "no pending config proposals")
		return
	}
	accepted, rejected := ui.ReviewConfigs(rows)
	byID := make(map[string]ConfigRow, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	applied, dropped := 0, 0
	for _, id := range accepted {
		if _, ok := byID[id]; !ok {
			continue
		}
		if p := h.takeCfgProposal(id); p != nil {
			h.applyCfgProposal(p)
			applied++
		}
	}
	for _, id := range rejected {
		if _, ok := byID[id]; !ok {
			continue
		}
		if h.takeCfgProposal(id) != nil {
			dropped++
		}
	}
	if applied+dropped > 0 {
		h.memNotify("info", "config", fmt.Sprintf("applied %d, rejected %d", applied, dropped))
	}
}

// listRevisions opens the revisions as a list; enter on a row fills
// "/config revert <id>" (the audit trail lives in the revisions table and
// the git log).
func (h *Host) listRevisions() error {
	if h.GlobalDB == nil {
		return errors.New("revision store unavailable")
	}
	revs, err := h.GlobalDB.ListRevisions()
	if err != nil {
		return err
	}
	ui := h.hooks()
	if len(revs) == 0 || ui == nil {
		h.memNotify("info", "revisions", "no config revisions yet")
		return nil
	}
	items := make([]any, 0, len(revs))
	for _, r := range revs {
		items = append(items, map[string]any{
			"text": shortID(r.ID) + " " + r.Status + " " + r.Title,
			"fill": "/config revert " + r.ID,
		})
	}
	ui.OpenOverlay(Overlay{ID: "revisions", Title: "Config revisions", Origin: lua.OriginBuiltin, Items: items})
	return nil
}

// revertRevision undoes one applied revision: git revert of its commit,
// then the reverted tree lands as its own `revert:` revision row and the
// original row is marked reverted. The perm store keeps its in-memory
// rules until the next reload (the notification says so); fail-closed
// beats half-applied state.
func (h *Host) revertRevision(id string) error {
	if h.GlobalDB == nil {
		return errors.New("revision store unavailable")
	}
	rev, err := h.GlobalDB.GetRevision(id)
	if err != nil {
		return err
	}
	if rev.Status != "applied" {
		return fmt.Errorf("revision %s is %s, only applied revisions revert", shortID(id), rev.Status)
	}
	if rev.Commit == "" {
		return fmt.Errorf("revision %s has no recorded commit", shortID(id))
	}
	dir := h.Dirs.ConfigHome
	if dir == "" {
		return errors.New("config repo: no config home")
	}
	if _, err := h.git(dir, "revert", "--no-edit", rev.Commit); err != nil {
		return fmt.Errorf("git revert failed (is the config repo clean?): %w", err)
	}
	// The revert commit is already in history ("Revert ..."); record it as
	// its own revision row. A missing SHA only costs the revert-of-revert
	// path, the audit trail is the git log.
	sha := ""
	if out, err := h.git(dir, "rev-parse", "HEAD"); err == nil {
		sha = strings.TrimSpace(out)
	}
	row := &store.ConfigRevision{
		ID:          store.NewID(),
		Title:       "revert: " + rev.Title,
		ChangesJSON: rev.ChangesJSON,
		PathsJSON:   rev.PathsJSON,
		Status:      "applied",
		AppliedBy:   "user",
		Commit:      sha,
	}
	if err := h.GlobalDB.AddRevision(row); err != nil {
		return err
	}
	if err := h.GlobalDB.SetRevisionStatus(rev.ID, "reverted"); err != nil {
		h.Log.Warn("config-rev", "revert status update failed", map[string]any{
			"id": rev.ID, "error": err.Error(),
		})
	}
	h.memNotify("info", "revisions", "reverted: "+rev.Title+" (reload to apply)")
	return nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ConfigCommandsStep registers the Go-owned /config command (builtin
// origin, user config may rebind it).
func (h *Host) ConfigCommandsStep() lua.BootStep {
	return lua.BootStep{
		Name: "config commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			_, err := h.Commands.Add(&Command{Name: "config", NArgs: "*", Hidden: true,
				Desc:       "review pending config changes; revisions, revert <id>",
				GoFn:       func(h *Host, args string) error { return h.configCommand(args) },
				GoComplete: subcommandCompleter([]string{"revisions", "revert"}, nil),
				Origin:     lua.OriginBuiltin, Gen: Gen(g.ID)})
			return err
		},
	}
}
