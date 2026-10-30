package cli

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/metafates/zemi/internal/ipc"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
)

// sendDialWait bounds one window's dial; sendReplyWait bounds the round trip
// past the window's own read ceiling, so a command that asks a model still
// gets its verdict.
const (
	sendDialWait  = 2 * time.Second
	sendReplyWait = 90 * time.Second
)

func sendCommand(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "send <text...>",
		Short: "deliver a prompt to a zemi window already running in this project",
		Long:  "deliver a prompt to a zemi window already running in this project: a command runs as if typed, a message starts a turn or joins the running one; none is live here, and it fails",
		Args:  minArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			text := strings.Join(args, " ")
			key := filepath.Base(filepath.Dir(store.ProjectDBPath(o.dirs.StateHome, o.dirs.TrustRoot())))
			peers := filepath.Join(o.dirs.StateHome, "peers", key)
			entries, err := os.ReadDir(peers)
			if err != nil {
				return fmt.Errorf("no zemi window is live in %s", perm.TildePath(o.dirs.TrustRoot()))
			}
			var lastErr error
			for _, e := range entries {
				if filepath.Ext(e.Name()) != ".json" {
					continue
				}
				var hb struct {
					Sock string `json:"sock"`
				}
				raw, err := os.ReadFile(filepath.Join(peers, e.Name()))
				if err != nil || json.Unmarshal(raw, &hb) != nil || hb.Sock == "" {
					continue
				}
				conn, err := ipc.Dial(hb.Sock, sendDialWait)
				if err != nil {
					lastErr = err
					continue
				}
				reply, err := ipc.RoundTrip(conn, &ipc.Request{Text: text}, sendReplyWait)
				_ = conn.Close()
				if err != nil {
					lastErr = err
					continue
				}
				if !reply.OK {
					return errors.New(reply.Error)
				}
				if reply.Queued {
					fmt.Fprintln(c.OutOrStdout(), "delivered: the message joined the window's running turn")
				} else {
					fmt.Fprintln(c.OutOrStdout(), "delivered: a turn starts in the window")
				}
				return nil
			}
			if lastErr != nil {
				return lastErr
			}
			return fmt.Errorf("no zemi window is live in %s", perm.TildePath(o.dirs.TrustRoot()))
		},
	}
}
