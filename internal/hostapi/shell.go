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

	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/sanitize"
)

const (
	shellTimeout = 2 * time.Minute
	shellMaxOut  = 16 << 10
)

// RunShell runs cmd (typed by the user, so no permission gate) without
// blocking the caller.
func (h *Host) RunShell(cmd string) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return
	}
	h.GoSafe("shell", func() {
		ctx, cancel := context.WithTimeout(context.Background(), shellTimeout)
		defer cancel()
		c := proc.CommandContext(ctx, "sh", "-c", cmd)
		c.Dir = h.Dirs.ProjectDir
		c.Env = append(os.Environ(), proc.NonInteractiveEnv...)
		out := &proc.Buffer{Max: 4 * shellMaxOut}
		c.Stdout, c.Stderr = out, out
		err := proc.Run(c)
		text := strings.TrimRight(out.String(), "\n")
		if len(text) > shellMaxOut {
			text = text[:shellMaxOut] + "\n... (truncated)"
		}
		status := "ok"
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			status, text = "err", strings.TrimSpace(text+"\ntimed out after "+shellTimeout.String())
		case err != nil:
			status, text = "err", strings.TrimSpace(text+"\n"+err.Error())
		}
		h.appendChatMessage(ChatMessage{Role: "user", Kind: "shell", Text: sanitize.String(cmd), Detail: sanitize.String(text), Status: status})
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
