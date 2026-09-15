package ui

import "time"

// toast is a transient notification (zemi.notify, DESIGN §6.1); rendering
// happens in the frame compose, expiry by timestamp pruning.
type toast struct {
	id      int
	level   string
	title   string
	body    string
	expires time.Time
}
