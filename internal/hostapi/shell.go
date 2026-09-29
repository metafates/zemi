// shell.go runs `!command` lines from the composer: the user's own shell
// command in the project directory, shown in the transcript and handed to
// the model with the next message so it can use the output.

package hostapi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
)

const shellTimeout = 2 * time.Minute

// RunShell runs cmd (typed by the user, so no permission gate) without
// blocking the caller.
func (h *Host) RunShell(cmd string) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return
	}
	h.lspDirty()
	h.GoSafe("shell", func() {
		ctx, cancel := context.WithTimeout(context.Background(), shellTimeout)
		defer cancel()
		c := proc.CommandContext(ctx, "sh", "-c", cmd)
		c.Dir = h.Dirs.ProjectDir
		c.Env = append(os.Environ(), proc.NonInteractiveEnv...)
		out := &proc.Buffer{Max: 1 << 20}
		c.Stdout, c.Stderr = out, out
		err := proc.Run(c)
		raw, written := out.Snapshot()
		text := tools.CmdOut(h.ScratchDir(), "shell", raw, written-int64(out.Max))
		status := CallOK
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			status, text = CallErr, strings.TrimSpace(text+"\ntimed out after "+shellTimeout.String())
		case err != nil:
			status, text = CallErr, strings.TrimSpace(text+"\n"+err.Error())
		}
		// the model reads the output as a bash call's, through shellNotes; the
		// row keeps its head and its tail, where the failure is
		h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Kind: RowShell, Text: sanitize.String(cmd), Detail: shortOutput(text), Status: status})
		ag := h.agents
		ag.mu.Lock()
		ag.shellNotes = append(ag.shellNotes, fmt.Sprintf("\n<shell command=%q status=%q>\n%s\n</shell>", cmd, status, text))
		ag.mu.Unlock()
	})
}

// takeShellNotes returns and clears the shell output waiting for the next
// message.
func (h *Host) takeShellNotes() string {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	notes := strings.Join(ag.shellNotes, "")
	ag.shellNotes = nil
	return notes
}
