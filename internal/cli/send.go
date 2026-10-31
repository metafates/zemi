package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/metafates/zemi/internal/ipc"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
)

// sendDialWait bounds the leader's dial; sendReplyWait bounds the round trip
// past the window's own read ceiling, so a command that asks a model still
// gets its verdict.
const (
	sendDialWait  = 2 * time.Second
	sendReplyWait = 90 * time.Second
)

func sendCommand(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "send <text...>",
		Short: "deliver a prompt to the window writing this project's chat",
		Long:  "deliver a prompt to the window writing this project's chat: a command runs as if typed, a message starts a turn or joins the running one; with no window writing, it fails",
		Args:  minArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			text := strings.Join(args, " ")
			dbPath := store.ProjectDBPath(o.dirs.StateHome, o.dirs.TrustRoot())
			nowhere := fmt.Errorf("no zemi window is writing this chat in %s", perm.TildePath(o.dirs.TrustRoot()))
			// deliver prints the verdict and reports whether it happened; the
			// window that refused names why
			deliver := func(sock string) (bool, error) {
				conn, err := ipc.Dial(sock, sendDialWait)
				if err != nil {
					return false, err
				}
				reply, err := ipc.RoundTrip(conn, &ipc.Request{Text: text}, sendReplyWait)
				_ = conn.Close()
				if err != nil {
					return false, err
				}
				if !reply.OK {
					return false, errors.New(cmp.Or(reply.Error, "the window refused this delivery"))
				}
				switch {
				case reply.Ran:
					fmt.Fprintf(c.OutOrStdout(), "delivered: the command runs in the window\n")
				case reply.Queued:
					fmt.Fprintf(c.OutOrStdout(), "delivered: the message joined a running turn in the window\n")
				default:
					fmt.Fprintf(c.OutOrStdout(), "delivered: a turn starts in the window\n")
				}
				if reply.Note != "" {
					// the window names itself by its live task, when one runs
					fmt.Fprintf(c.OutOrStdout(), "  · %s\n", reply.Note)
				}
				return true, nil
			}
			// the lease names the one window a delivery may reach: an
			// unreadable store names itself, a missing row or file is no
			// writer at all
			read := func() (store.Leader, error) {
				l, err := store.ReadLeaderAt(dbPath)
				if errors.Is(err, store.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
					return l, nowhere
				}
				return l, err
			}
			l, err := read()
			if err != nil {
				return err
			}
			ok, derr := deliver(l.Sock)
			if ok {
				return nil
			}
			// the first answer was a death or a refusal, and a handover swaps
			// the row under a dial: read the lease once more, and a row that
			// moved gets the delivery
			if n, err := read(); err != nil {
				return err
			} else if n.Origin != l.Origin {
				if ok, derr = deliver(n.Sock); ok {
					return nil
				}
			}
			if errors.Is(derr, ipc.ErrDead) {
				return errors.New("the writable window did not answer; it may be handing over - try again")
			}
			return derr
		},
	}
}
