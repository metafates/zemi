package cli

import (
	"cmp"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/metafates/zemi/internal/hostapi"
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
			type peer struct {
				sock  string
				fresh bool
			}
			var peersByAge []peer
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
				fresh := false
				if info, err := e.Info(); err == nil && time.Since(info.ModTime()) <= hostapi.PeersTTL {
					fresh = true
				}
				peersByAge = append(peersByAge, peer{hb.Sock, fresh})
			}
			// deliver prints the verdict and reports whether it happened; a
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
					fmt.Fprintln(c.OutOrStdout(), "delivered: the command runs in the window")
				case reply.Queued:
					fmt.Fprintln(c.OutOrStdout(), "delivered: the message joined the window's running turn")
				default:
					fmt.Fprintln(c.OutOrStdout(), "delivered: a turn starts in the window")
				}
				return true, nil
			}
			// fresh heartbeats first; an aged one is probed only when nothing
			// fresh answered, since mtime has writers other than the window
			// and a corpse fails its dial in ms
			var lastErr error
			for _, fresh := range []bool{true, false} {
				for _, p := range peersByAge {
					if p.fresh != fresh {
						continue
					}
					ok, err := deliver(p.sock)
					if ok {
						return nil
					}
					// a dead heartbeat is the same "nothing to deliver to" as
					// no heartbeat at all; only a real failure speaks for itself
					if !errors.Is(err, ipc.ErrDead) {
						lastErr = err
					}
				}
			}
			if lastErr != nil {
				return lastErr
			}
			return fmt.Errorf("no zemi window is live in %s", perm.TildePath(o.dirs.TrustRoot()))
		},
	}
}
