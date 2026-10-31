// ask.go is the permission gate's ask half: the permission.request veto the
// gate runs once per action, and the ask hook that puts what survives it to
// the user under the ask lock.

package hostapi

import (
	"context"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// askHook is the tools.Pipeline asker: the Go-owned overlay, behind the
// veto the gate already ran.
func (h *Host) askHook() tools.Asker {
	return func(req tools.Request) (bool, string) { return h.ask(req) }
}

// vetoHook is the tools.Pipeline veto: the permission.request event, once
// per action request at the gate. "ask" hands the request to the ask path;
// a denial reports on permission.resolved as an ask would have.
func (h *Host) vetoHook() func(tools.Request) (string, string) {
	return func(req tools.Request) (string, string) {
		var res VetoResult
		h.outsideReload(func() {
			res = h.EmitVetoable(context.Background(), EventPermissionRequest,
				map[string]any{"subject": req.Subject, "arg": req.Arg, "reason": req.Reason, "agent": req.Agent})
		})
		switch {
		case res.Denied:
			h.resolved(req, false, "event")
			return tools.VetoDeny, res.Reason
		case res.Ask:
			return tools.VetoAsk, ""
		default:
			return "", ""
		}
	}
}

// resolved reports one settled request on permission.resolved.
func (h *Host) resolved(req tools.Request, allowed bool, by string) {
	decision := "deny"
	if allowed {
		decision = "allow"
	}
	h.EmitAsync(EventPermissionResolved, map[string]any{
		"subject": req.Subject, "arg": req.Arg,
		"decision": decision,
		"by":       by,
	})
}

// ask answers one request. Asks reach the user one at a time under askMu.
// No Lua runs under askMu (the veto ran at the gate), so nothing on the
// interpreter can wait on it, and the UI goroutine never takes it.
// NOTE: sync.Mutex hands off in arrival order only once a waiter has
// waited over 1ms (starvation mode), so prompt order is FIFO in practice,
// not by guarantee.
func (h *Host) ask(req tools.Request) (bool, string) {
	resolved := func(allowed bool, by string) (bool, string) {
		h.resolved(req, allowed, by)
		return allowed, by
	}
	ui := h.hooks()
	if ui == nil {
		// an ask nobody can answer fails closed: the run goes on without it
		return resolved(false, tools.ByNobody)
	}
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
