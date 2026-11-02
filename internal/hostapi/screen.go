package hostapi

import (
	"os"
	"time"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
)

// ScreenState is everything a frontend draws one frame from: the host's
// state as data, with no cells, styles or widget ids in it. The TUI turns it
// into rows; another frontend could turn it into its own.
type ScreenState struct {
	// the frame
	Width, Height  int
	Time           string
	Now            time.Time
	Frame          int
	Ended          TurnEnd
	QuitArmed      bool
	InterruptArmed bool
	KeySeq         string
	RowsBelow      int
	Cancelling     bool
	Editing        string
	Waiting        Waiting
	Rail           int

	// where zemi is
	Version   string
	CWD       string
	Home      string
	Project   string
	Branch    string
	Trusted   bool
	Trustable bool
	InitError string
	ModelHint string
	Model     string
	Effort    string

	// the conversation
	AgentActive     bool
	View            *RunView
	Transcript      []ChatMessage
	TranscriptCount int
	TranscriptEpoch int64
	TranscriptCut   int64
	TranscriptEdit  int64
	ViewRevision    uint64

	// the turn
	Agent AgentState

	// the rest of what the screen reports
	Goal             *GoalState
	Loop             *LoopState
	Usage            Usage
	Context          Context
	Busy             []BusyItem
	AgentsRunning    int
	WorkflowsRunning int
	JobsRunning      int

	// filled on first read: a widget that never reads them does not pay for
	// them, and the ones that read disk do it once per render at most
	todos        func() []store.Todo
	projects     func() []ProjectRef
	instructions func() ([]string, bool)
	logRows      func() int
}

// RunView is an agent run shown in the chat's place.
type RunView struct {
	ID          string
	Agent       string
	Description string
	Status      string
}

// AgentState is the live turn: what the model is writing, thinking and
// calling right now.
type AgentState struct {
	Active       bool
	Model        string
	Text         string
	Reasoning    string
	Thinking     bool
	Steering     []string
	Thought      time.Duration
	Elapsed      time.Duration
	CheckingGoal bool
	Retry        *Retry
	Tools        []AgentTool
}

// Retry is a provider retry waiting or under way.
type Retry struct {
	Attempt int
	Why     string
	NextIn  time.Duration
}

// AgentTool is one live call: the stream's row plus how long it has run.
type AgentTool struct {
	StreamTool
	Elapsed time.Duration
}

// GoalState is the standing goal, clipped for the statusline.
type GoalState struct {
	Condition string
	Turns     int
	Reason    string
}

// LoopState is the repeating prompt.
type LoopState struct {
	Prompt string
	Every  time.Duration
	Next   time.Time
	Runs   int
}

// Usage is the session's token and cost tally.
type Usage struct {
	In, Out, Total int64
	Cost           float64
	Priced         bool
}

// Context is the current-context estimate: the last prompt against the
// model's window.
type Context struct {
	Tokens, Window int64
	Pct            float64
}

// BusyItem is a job the user waits on that is not a turn.
type BusyItem struct {
	Label       string
	Elapsed     time.Duration
	Cancellable bool
}

// ProjectRef is one recent project the splash offers.
type ProjectRef struct {
	Path string // tilde-spelled, as the row reads
	Root string
	Ago  string
}

// Todos is the session's list, empty while an agent run is shown.
func (s *ScreenState) Todos() []store.Todo {
	if s.todos == nil {
		return nil
	}
	return s.todos()
}

// Projects is the recent projects the splash offers, nil when the splash has
// nothing to offer or the session is past its first message.
func (s *ScreenState) Projects() []ProjectRef {
	if s.projects == nil {
		return nil
	}
	return s.projects()
}

// Instructions is the project's instruction files, and false where project
// files cannot load (untrusted, or $HOME and /, which are nobody's project).
func (s *ScreenState) Instructions() ([]string, bool) {
	if s.instructions == nil {
		return nil, false
	}
	return s.instructions()
}

// LogRows is how many log rows the chat window's head cut off: the head
// /messages pages back from.
func (s *ScreenState) LogRows() int {
	if s.logRows == nil {
		return 0
	}
	return s.logRows()
}

// ScreenState snapshots what a frontend draws, once per render.
func (h *Host) ScreenState(width, height int, state UIState) *ScreenState {
	// the edit counter before the rows: a join landing between them would move
	// it past a snapshot without the chunk, and nothing moves it again
	edit := h.trEdit.Load()
	sess, stream := h.liveView()
	// Showing an agent run, the chat renders its transcript alone and the run
	// reports its own state: the session's turn, todos and stream stay out of
	// it. Before the fields below, which read stream.
	run := h.viewedRun()
	transcript := sess.Transcript
	// The head cut reads with the rows it names: a trim landing between the
	// two would shift the chat's keys past the rows they index. A run's
	// transcript is no cut of the log, so it reports none.
	var cut int64
	if run == nil {
		h.tMu.RLock()
		transcript, cut = h.session.Transcript, h.trCut.Load()
		h.tMu.RUnlock()
	}
	st := &ScreenState{
		Width: width, Height: height, Time: state.Time, Now: h.now(), Frame: state.Frame,
		Ended: state.Ended, QuitArmed: state.QuitArmed, InterruptArmed: state.InterruptArmed,
		KeySeq: state.KeySeq, RowsBelow: state.RowsBelow, Cancelling: state.Cancelling,
		Editing: state.Editing, Waiting: state.Waiting, Rail: state.Rail,

		Version: h.Version, CWD: perm.TildePath(sess.CWD),
		Trusted: h.ProjectTrusted(), InitError: h.InitError(), ModelHint: h.NoModelHint(),
		Model: stream.Model, Effort: h.Effort(stream.Model), AgentActive: stream.Active,

		Transcript: transcript, TranscriptCount: len(transcript),
		TranscriptEpoch: h.trEpoch.Load(), TranscriptCut: cut, TranscriptEdit: edit,

		Agent: h.agentState(stream, h.now()),
	}
	st.Home, _ = os.UserHomeDir()
	st.Project, st.Branch = h.project(), h.gitBranch()
	if run != nil {
		status, rows, live, rev := run.view(stream)
		st.View = &RunView{ID: run.id, Agent: sanitize.String(run.label()), Description: sanitize.String(run.description), Status: string(status)}
		st.Transcript, stream, st.ViewRevision = rows, live, rev
		st.TranscriptCount, st.TranscriptEpoch = len(rows), 0
		st.AgentActive, st.Model, st.Effort = live.Active, live.Model, h.Effort(live.Model)
		st.Agent, st.Usage, st.Context = h.agentState(live, h.now()), Usage{In: live.SessionIn, Out: live.SessionOut, Total: live.SessionIn + live.SessionOut, Cost: live.SessionCost, Priced: st.Usage.Priced}, Context{Tokens: live.LastIn, Window: live.CtxWindow, Pct: contextPct(live.LastIn, live.CtxWindow)}
	}
	if jobs := h.BusyList(); len(jobs) > 0 {
		for _, j := range jobs {
			st.Busy = append(st.Busy, BusyItem(j))
		}
	}
	sid := h.AgentSessionID()
	st.AgentsRunning, st.WorkflowsRunning = h.tasks.live(func(p string) bool { return p == sid })
	st.JobsRunning = h.jobsRunningCount()
	if g, ok := h.Goal(); ok {
		st.Goal = &GoalState{Condition: clipLine(g.Condition, 48), Turns: g.Turns, Reason: sanitize.String(g.Reason)}
	}
	if l, ok := h.Loop(); ok {
		st.Loop = &LoopState{Prompt: sanitize.String(l.Prompt), Every: l.Every, Next: l.Next, Runs: l.Runs}
	}
	st.Usage = Usage{In: stream.SessionIn, Out: stream.SessionOut, Total: stream.SessionIn + stream.SessionOut, Cost: stream.SessionCost, Priced: h.modelPriced(h.turnModelID())}
	st.Context = Context{Tokens: stream.LastIn, Window: stream.CtxWindow, Pct: contextPct(stream.LastIn, stream.CtxWindow)}

	st.Trustable = !TrustUnsaved(h.trustRoot())
	st.todos = func() []store.Todo {
		if run != nil {
			return nil
		}
		return h.todoSnapshot()
	}
	st.instructions = func() ([]string, bool) {
		files := h.instructionsCached()
		return files, files != nil
	}
	st.projects = func() []ProjectRef {
		ps := h.splashProjects()
		if len(sess.Transcript) > 0 || len(ps) == 0 {
			return nil
		}
		out := make([]ProjectRef, 0, len(ps))
		for _, p := range ps {
			out = append(out, ProjectRef{Path: sanitize.String(perm.TildePath(p.Root)), Root: p.Root, Ago: ago(p.Used.UnixMilli())})
		}
		return out
	}
	st.logRows = func() int {
		if run != nil {
			return 0
		}
		// log rows the window's head cut off: the oldest stamped row's seq,
		// the log's seqs dense from 0. Live rows of kinds that pair with
		// nothing (a report, a thought) draw log rows of their own below the
		// first stamped one: they are on screen, not earlier. A row whose log
		// write failed counts as earlier it is not - rare, and the honest
		// direction to miss.
		shown := int64(0)
		for i := range transcript {
			if transcript[i].Seq != NoSeq {
				return max(int(transcript[i].Seq-shown), 0)
			}
			shown++
		}
		return 0
	}
	return st
}

// agentState is the live turn as a widget draws it. now is the host's clock,
// which the tool stamps were taken on.
func (h *Host) agentState(stream StreamSnapshot, now time.Time) AgentState {
	a := AgentState{
		Active: stream.Active, Model: stream.Model, Text: stream.Text,
		Reasoning: stream.Reasoning, Thinking: stream.Thinking,
		Thought: stream.Thought, Elapsed: stream.Elapsed, CheckingGoal: stream.CheckingGoal,
	}
	for _, s := range stream.Steering {
		a.Steering = append(a.Steering, s.Text)
	}
	if stream.Retrying {
		a.Retry = &Retry{Attempt: stream.Retries + 1, Why: sanitize.String(stream.RetryWhy), NextIn: stream.RetryIn}
	}
	a.Tools = make([]AgentTool, len(stream.Tools))
	for i, t := range stream.Tools {
		row := AgentTool{StreamTool: t}
		if !t.Started.IsZero() {
			row.Elapsed = now.Sub(t.Started)
		}
		a.Tools[i] = row
	}
	return a
}
