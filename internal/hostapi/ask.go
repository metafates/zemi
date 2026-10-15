// ask.go is the permission gate's ask half: the permission.request veto the
// gate runs once per action, and the ask hook that puts what survives it to
// the user under the ask lock.

package hostapi

import (
	"context"
	"strings"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
)

// askHook is the tools.Pipeline asker: the Go-owned overlay, behind the
// veto the gate already ran.
func (h *Host) askHook() tools.Asker {
	return func(req tools.Request) (bool, string) { return h.ask(req, false) }
}

// vetoHook is the tools.Pipeline veto: the permission.request event, once
// per action request at the gate. "ask" hands the request to the ask path;
// a denial reports on permission.resolved as an ask would have.
func (h *Host) vetoHook() func(tools.Request) string {
	return func(req tools.Request) string {
		var res VetoResult
		h.outsideReload(func() {
			res = h.EmitVetoable(context.Background(), EventPermissionRequest,
				map[string]any{"subject": req.Subject, "arg": req.Arg, "reason": req.Reason, "agent": req.Agent})
		})
		switch {
		case res.Denied:
			h.resolved(req, false, "event")
			return tools.VetoDeny
		case res.Ask:
			return tools.VetoAsk
		}
		return ""
	}
}

// resolved reports one settled request on permission.resolved.
func (h *Host) resolved(req tools.Request, allowed bool, by string) {
	h.EmitAsync("permission.resolved", map[string]any{
		"subject": req.Subject, "arg": req.Arg,
		"decision": map[bool]string{true: "allow", false: "deny"}[allowed],
		"by":       by,
	})
}

// ask answers one request. Asks reach the user one at a time under askMu,
// and each is judged again once it holds the lock (held): a mode switch to
// allow while it waited may settle it. No Lua runs under askMu (the veto
// ran at the gate, and the rejudge's copy has no asker), so nothing on the
// interpreter can wait on it, and the UI goroutine never takes it.
// NOTE: sync.Mutex hands off in arrival order only once a waiter has
// waited over 1ms (starvation mode), so prompt order is FIFO in practice,
// not by guarantee.
func (h *Host) ask(req tools.Request, held bool) (bool, string) {
	entry := req
	resolved := func(allowed bool, by string) (bool, string) {
		h.resolved(req, allowed, by)
		return allowed, by
	}
	ui := h.hooks()
	if ui == nil {
		why := req.Reason
		if req.Agent != "" {
			why = strings.TrimSuffix(req.Agent+": "+why, ": ")
		}
		h.modes.noteHeadless(Denial{Tool: req.Tool, Subject: req.Subject, Arg: sanitize.String(h.shownArg(req)), Reason: sanitize.String(why), Why: DenyApproval})
		return resolved(false, tools.ByNobody)
	}
	if !held {
		cut := h.askCut.Load()
		h.asksWaiting.Add(1)
		h.askMu.Lock()
		defer func() {
			h.asksWaiting.Add(-1)
			h.askMu.Unlock()
			ui.Refresh() // drops an answered prompt no other ask replaced
		}()
		if h.askCut.Load() != cut {
			return resolved(false, tools.ByUser) // told zemi what to do instead
		}
		if v, _ := entry.Recheck(); v.Allowed {
			return false, tools.ByRecheck
		}
		return h.ask(entry, true)
	}
	pr := PermRequest{
		Tool: req.Tool, Subject: req.Subject, Arg: h.shownArg(req),
		Reason: req.Reason, Preview: req.Preview, Description: req.Description, Agent: req.Agent, Owner: req.Owner,
	}
	// a call a lua tool's script drove: the user read the script, so name it
	if req.Script != "" {
		pr.Script = "lua script"
	}
	allowed, by := ui.AskPermission(pr)
	return resolved(allowed, by)
}

// AsksWaiting is how many asks are at the prompt stage, the one shown
// included; some may settle without a prompt.
func (h *Host) AsksWaiting() int { return int(h.asksWaiting.Load()) }

// DenyWaitingAsks denies every ask waiting for the prompt now, as the user.
func (h *Host) DenyWaitingAsks() { h.askCut.Add(1) }

// shownArg is the request's argument as the user reads it: a path relative
// to the project when inside it, or to the trusted repository's root, else
// with ~ for the home directory.
func (h *Host) shownArg(req tools.Request) string {
	if !tools.PathSubject(req.Subject) {
		return req.Arg
	}
	return perm.TildePath(h.relPath(req.Arg))
}

// relPath is path relative to the project, else to the trusted
// repository's root ("<root>/pkg/a.go" is "pkg/a.go" from "<root>/other"),
// else as given.
func (h *Host) relPath(path string) string {
	roots := []string{h.Dirs.ProjectDir}
	if h.ProjectTrusted() {
		roots = append(roots, h.Dirs.RepoRoot())
	}
	for _, root := range roots {
		// as given, then resolved (/var is /private/var on macOS)
		for _, p := range []string{path, perm.RealPath(path)} {
			if rel, ok := perm.Under(perm.RealPath(root), p); ok {
				return rel
			}
		}
	}
	return path
}
