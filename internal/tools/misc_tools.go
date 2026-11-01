// misc_tools.go ships todos, question, plan and skill (DESIGN §6.4):
// session todo list, structured user questions, plan approval and
// on-demand SKILL.md loading. They reach the host through pipeline hooks;
// a nil hook is a result the model can act on (no user to ask), never a
// panic.

package tools

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/skill"
	"github.com/metafates/zemi/internal/store"
)

// todosArgs is the todos tool's input and its schema source.
type todosArgs struct {
	Todos []todoItem `json:"todos,omitempty"`
	// the name other harnesses taught models; runTodos reads it as todos
	Items []todoItem `json:"items,omitempty" desc:"the same list as todos"`
}

// todoItem is one list entry as the todos tool reads it.
type todoItem struct {
	Content    string           `json:"content"`
	Status     store.TodoStatus `json:"status"`
	ActiveForm string           `json:"active_form,omitempty" desc:"on the in_progress item only: present tense shown while it runs, e.g. Running tests"`
}

func runTodos(ctx context.Context, p *Pipeline, in todosArgs) (fantasy.ToolResponse, error) {
	if in.Todos == nil {
		in.Todos = in.Items
	}
	out, dropped, ok, err := parseTodos(in.Todos)
	// a run whose host wired no todo hook has no list to write or read
	if p.TodosRead == nil || ok && p.TodosWrite == nil {
		return fantasy.NewTextErrorResponse("there is no todo list in this run: keep the steps in your reply instead"), nil
	}
	if ok {
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		// a write that quietly drops open items is how a long session loses
		// track of work: name what vanished, so the list stays the record.
		// One open item reworded in place — same length, every other content
		// standing — is a rename, not a drop; an explicit cancel is the
		// write's own accounting.
		vanished := []string{}
		if old, err := p.TodosRead(); err == nil {
			var missing []string
			for _, td := range old {
				if td.Status != store.TodoDone && !slices.ContainsFunc(out, func(n store.Todo) bool { return n.Content == td.Content }) {
					missing = append(missing, td.Content)
				}
			}
			fresh := 0
			for _, n := range out {
				if !slices.ContainsFunc(old, func(td store.Todo) bool { return td.Content == n.Content }) {
					fresh++
				}
			}
			renamed := len(old) == len(out) && len(missing) == 1 && fresh == 1
			for _, td := range old {
				if td.Status == store.TodoDone {
					continue
				}
				if slices.ContainsFunc(out, func(n store.Todo) bool { return n.Content == td.Content }) {
					continue
				}
				if renamed || slices.Contains(dropped, td.Content) {
					continue
				}
				vanished = append(vanished, td.Content)
			}
		}
		if err := p.TodosWrite(out); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		// the model just sent the list, so the answer is a count, not the
		// list again; a wrong in_progress count stands and is fixed next call
		active, done := 0, 0
		for _, td := range out {
			switch td.Status {
			case store.TodoInProgress:
				active++
			case store.TodoDone:
				done++
			case store.TodoPending:
			}
		}
		note := ""
		if len(dropped) > 0 {
			note = fmt.Sprintf("; dropped %d cancelled", len(dropped))
		}
		if len(vanished) > 0 {
			shown, more := vanished, ""
			if len(shown) > 3 {
				shown, more = shown[:3], fmt.Sprintf(" +%d more", len(vanished)-3)
			}
			one, it := "items", "them"
			if len(vanished) == 1 {
				one, it = "item", "it"
			}
			note += fmt.Sprintf("; %d open %s left the list (\"%s\"%s): put %s back or mark %s done",
				len(vanished), one, strings.Join(shown, "\", \""), more, it, it)
		}
		if active > 1 {
			note += fmt.Sprintf("; %d items are in_progress; keep exactly one", active)
		}
		// the plan's own write is the moment to point at parallel runs: done is
		// 0 and the parts are all ahead of the model
		if pending := len(out) - done - active; done == 0 && pending > 1 {
			note += fmt.Sprintf("; %d items are pending: the ones that need little from each other can run as parallel agent calls in one message and wait, instead of working them one by one", pending)
		}
		return fantasy.NewTextResponse(fmt.Sprintf("updated todos: %d/%d done%s", done, len(out), note)), nil
	}
	rows, err := p.TodosRead()
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if len(rows) == 0 {
		return fantasy.NewTextResponse("(no todos)"), nil
	}
	return fantasy.NewTextResponse(TodoList(rows)), nil
}

// parseTodos reads the list a todos call sets, returning the contents it
// dropped as cancelled; ok is false for a call that only reads the list.
func parseTodos(items []todoItem) (out []store.Todo, dropped []string, ok bool, err error) {
	ok = items != nil
	for i, td := range items {
		status := td.Status
		// the names other harnesses taught models
		if status == "completed" {
			status = store.TodoDone
		}
		if status == "cancelled" {
			dropped = append(dropped, td.Content)
			continue
		}
		if td.Content == "" {
			return nil, nil, true, fmt.Errorf("todo %d: content is required", i+1)
		}
		if !slices.Contains(store.TodoStatuses, status) {
			return nil, nil, true, fmt.Errorf("todo %d: status must be pending, in_progress or done", i+1)
		}
		out = append(out, store.Todo{Content: td.Content, Status: status, ActiveForm: td.ActiveForm})
	}
	return out, dropped, ok, nil
}

// ParseTodos reads a todos call's input the run does.
func ParseTodos(in map[string]any) ([]store.Todo, int, bool, error) {
	list, ok := in["todos"].([]any)
	items := make([]todoItem, 0, len(list))
	for i, v := range list {
		m, isObj := v.(map[string]any)
		if !isObj {
			return nil, 0, true, fmt.Errorf("todo %d: not an object", i+1)
		}
		var td todoItem
		if err := remarshal(m, &td); err != nil {
			return nil, 0, true, fmt.Errorf("todo %d: %w", i+1, err)
		}
		items = append(items, td)
	}
	out, dropped, _, err := parseTodos(items)
	return out, len(dropped), ok, err
}

// TodoList is the list as the todos tool reports it.
func TodoList(rows []store.Todo) string {
	var b strings.Builder
	for _, td := range rows {
		// the enum words: the model sends them back, after compaction too
		fmt.Fprintf(&b, "- [%s] %s\n", td.Status, td.Content)
	}
	return b.String()
}

// PlanVerdict is the user's answer to a presented plan.
type PlanVerdict struct {
	Approved bool
	Hold     bool   // approved to keep, not to start: the user says when
	Feedback string // what to change, when not approved
	Path     string // where the plan was saved, "" when not saved
}

// ImplementPlan is what an approved plan's result tells the agent to do.
const ImplementPlan = "Implement it; track its steps with todos if it has several."

// The plan results PlanOutcome reads back.
const (
	planApproved = "The user approved the plan"
	planHeld     = "The user approved the plan; the user has not asked for it yet"
	planChanges  = "The user has not approved the plan and asked for changes: "
	planRevise   = "\nRevise the plan"
	planSaved    = "\nThe plan is saved at "
)

// PlanOutcome is the verdict in a plan result, which is written for the
// model, for its row to show: path is where an approved plan was saved, ""
// when it was not. Any other result comes back whole.
func PlanOutcome(output string) (verdict, path string) {
	if feedback, ok := strings.CutPrefix(output, planChanges); ok {
		feedback, _, _ = strings.Cut(feedback, planRevise)
		return "changes requested: " + feedback, ""
	}
	switch {
	case strings.HasPrefix(output, planHeld): // before planApproved, its prefix
		verdict = "approved (held)"
	case strings.HasPrefix(output, planApproved):
		verdict = "approved"
	default:
		return output, ""
	}
	_, path, _ = strings.Cut(output, planSaved)
	return verdict, strings.TrimSuffix(path, ".")
}

// planArgs is the plan tool's input and its schema source.
type planArgs struct {
	Plan string `json:"plan,omitempty" desc:"the plan in markdown"`
	File string `json:"file,omitempty" desc:"path of a file holding the plan, relative to the working directory or absolute; instead of retyping a plan you already wrote"`
}

// planFile reads a plan held in a file, under the file tools' path rules:
// no shell expansion, project-relative or absolute.
func (p *Pipeline) planFile(raw string) (string, error) {
	if unexpanded.MatchString(raw) {
		return "", errors.New(p.unexpandedText(raw))
	}
	path, err := perm.Resolve(raw)
	if err != nil {
		return "", err
	}
	f, fi, err := OpenRegular(path)
	if err != nil {
		return "", errors.New(p.notFound(path.String(), err))
	}
	defer f.Close()
	if fi.Size() > readMaxBytes {
		return "", fmt.Errorf("%s is %d bytes, over plan's %d byte limit: a plan is short; point at a shorter file", workRel(path.String()), fi.Size(), readMaxBytes)
	}
	data, err := io.ReadAll(f)
	return string(data), err
}

func runPlan(ctx context.Context, p *Pipeline, in planArgs) (fantasy.ToolResponse, error) {
	plan := strings.TrimSpace(in.Plan)
	if in.File != "" {
		if plan != "" {
			return fantasy.NewTextErrorResponse("pass the plan in plan, or its path in file, not both"), nil
		}
		var err error
		if plan, err = p.planFile(in.File); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}
	if plan == "" {
		return fantasy.NewTextErrorResponse("the plan is required: in plan, or a path to it in file"), nil
	}
	if p.ApprovePlan == nil {
		return fantasy.NewTextResponse("No user is available to approve the plan. Put the plan in your reply and stop."), nil
	}
	v, ok := p.ApprovePlan(plan)
	saved := ""
	if v.Path != "" {
		saved = planSaved + v.Path + "."
	}
	implement := " " + ImplementPlan + saved
	switch {
	case !ok:
		p.outcome(ctx, Outcome{Stopped: true})
		return stopTurn(userDismissed + " the plan without approving it. Stop and wait for them to say how to proceed."), nil
	case v.Approved && v.Hold:
		return endTurn(planHeld + ", so do not start implementing: stop here and wait for them to say when." + saved), nil
	case v.Approved:
		return fantasy.NewTextResponse(planApproved + "." + implement), nil
	default:
		return fantasy.NewTextResponse(planChanges + v.Feedback + planRevise + ", and call plan again."), nil
	}
}

// stopTurn is an error result that ends the turn: the user wants to speak next.
func stopTurn(text string) fantasy.ToolResponse {
	resp := fantasy.NewTextErrorResponse(text)
	resp.StopTurn = true
	return resp
}

// endTurn ends the turn on a success note: a plain reply with no error mark
// (an approved plan is not a failure), still stopping the loop.
func endTurn(text string) fantasy.ToolResponse {
	resp := fantasy.NewTextResponse(text)
	resp.StopTurn = true
	return resp
}

// QuestionOption is one answer choice for a question ask.
type QuestionOption struct {
	Label       string // plain text, without the " (Recommended)" the model marks its pick with
	Description string // markdown, shown under the options for the selected one
	Recommended bool
}

// QuestionType is how a question is asked; runQuestion maps it onto the
// overlay's list model.
type QuestionType string

const (
	TypeSingle   QuestionType = "single_choice"
	TypeMulti    QuestionType = "multi_choice"
	TypeFreeText QuestionType = "free_text"
)

var questionTypes = []QuestionType{TypeSingle, TypeMulti, TypeFreeText}

// Question is one structured ask to the user, drawn by a Go-owned overlay.
// single_choice picks one option, multi_choice marks several, free_text
// answers on a typed row. Options may carry a short Description, shown
// under the options for the selected one.
type Question struct {
	Type        QuestionType
	Question    string // markdown
	Description string // markdown, shown under the question
	Header      string // the tab label in a batch
	Options     []QuestionOption
}

func (q Question) Multi() bool { return q.Type == TypeMulti }

// ListRows is how many rows the selection list shows: the options plus a
// "type your own" tail row for the choice types; free_text has only the
// typed row.
func (q Question) ListRows() int {
	if q.Type == TypeFreeText {
		return 0
	}
	return len(q.Options) + 1
}

// Answer is the user's answer to one Question; the zero value leaves it open.
type Answer struct {
	Picked []string // option labels
	Typed  string   // the user's own answer instead of a pick
	Note   string   // the user's note on the picks
}

func (a Answer) Open() bool { return len(a.Picked) == 0 && a.Typed == "" }

const maxQuestions = 4

// questionArgs is the question tool's input and its schema source.
type questionArgs struct {
	Questions []questionAsk `json:"questions"`
}

// questionAsk is one structured ask; the omitted type reads from the shape:
// options to pick, no options to type.
type questionAsk struct {
	Type        QuestionType     `json:"type,omitempty" desc:"multi_choice lets the user mark several; free_text is typed; defaults to single_choice with options, free_text without"`
	Question    string           `json:"question" desc:"the complete question in markdown, ending with a question mark"`
	Description string           `json:"description,omitempty" desc:"markdown shown under the question"`
	Header      string           `json:"header,omitempty" desc:"a 1-3 word label, e.g. Auth method"`
	Options     []questionOption `json:"options,omitempty" desc:"required for single_choice and multi_choice"`
}

// questionOption is one answer choice.
type questionOption struct {
	Label       string `json:"label" desc:"plain text; put markdown in description"`
	Description string `json:"description,omitempty" desc:"markdown shown under the option while it is selected"`
}

func runQuestion(ctx context.Context, p *Pipeline, in questionArgs) (fantasy.ToolResponse, error) {
	raw := in.Questions
	if len(raw) == 0 || len(raw) > maxQuestions {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("questions must hold 1 to %d questions", maxQuestions)), nil
	}
	qs := make([]Question, 0, len(raw))
	for i, q := range raw {
		if q.Question == "" {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("question %d: the question text is required", i+1)), nil
		}
		ask := Question{
			Type:        q.Type,
			Question:    q.Question,
			Description: q.Description,
			Header:      q.Header,
		}
		// the omitted type reads from the shape: options to pick, no options to type
		if ask.Type == "" {
			if q.Options != nil {
				ask.Type = TypeSingle
			} else {
				ask.Type = TypeFreeText
			}
		}
		if !slices.Contains(questionTypes, ask.Type) {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("question %d: type must be single_choice, multi_choice or free_text", i+1)), nil
		}
		if ask.Type == TypeFreeText {
			qs = append(qs, ask)
			continue
		}
		for j, o := range q.Options {
			if o.Label == "" {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("question %d, option %d: label is required", i+1, j+1)), nil
			}
			label, rec := strings.CutSuffix(o.Label, " (Recommended)")
			ask.Options = append(ask.Options, QuestionOption{Label: label, Description: o.Description, Recommended: rec})
		}
		qs = append(qs, ask)
	}
	if p.Question == nil {
		return fantasy.NewTextResponse("No user is available to answer. Take the option you would recommend, say which assumption you made, and continue."), nil
	}
	answers, ok := p.Question(qs)
	if !ok || len(answers) != len(qs) {
		p.outcome(ctx, Outcome{Stopped: true})
		return stopTurn(userDismissed + " the questions. Stop and wait for them to say how to proceed."), nil
	}
	res := questionResult{Answers: make([]questionAnswer, len(qs))}
	for i, q := range qs {
		a := answers[i]
		res.Answers[i] = questionAnswer{Question: q.Question, Note: a.Note}
		switch {
		case a.Typed != "":
			res.Answers[i].Answer = a.Typed
		case a.Open():
			res.Open = "a null answer is yours to decide: take the option you recommended, or your best judgment if you gave none"
		case q.Multi():
			res.Answers[i].Answer = a.Picked
		default:
			res.Answers[i].Answer = a.Picked[0]
		}
	}
	out, err := json.Marshal(res, json.Deterministic(true))
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	return fantasy.NewTextResponse(string(out)), nil
}

// questionResult is the question tool's result: each answer a string, a
// multi_choice's marks, or null when left open, which Open explains.
type questionResult struct {
	Answers []questionAnswer `json:"answers"`
	Open    string           `json:"open,omitempty"`
}

type questionAnswer struct {
	Question string `json:"question"`
	Answer   any    `json:"answer"`
	Note     string `json:"note,omitempty"`
}

// QuestionAnswers is each question, its answer ("" when left open, marks
// joined with ", ") and its note in a question result, for its row to
// show. Any other result is nil.
func QuestionAnswers(output string) [][3]string {
	var res questionResult
	if json.Unmarshal([]byte(output), &res) != nil || len(res.Answers) == 0 {
		return nil
	}
	qa := make([][3]string, len(res.Answers))
	for i, a := range res.Answers {
		ans, _ := a.Answer.(string)
		if marks, ok := a.Answer.([]any); ok {
			labels := make([]string, len(marks))
			for j, m := range marks {
				labels[j], _ = m.(string)
			}
			ans = strings.Join(labels, ", ")
		}
		qa[i] = [3]string{a.Question, ans, a.Note}
	}
	return qa
}

// SkillInfo is one resolvable skill: name plus the directory holding
// SKILL.md (the host resolves enabled skills, DESIGN §10.1).
type SkillInfo struct {
	Name string
	Path string
	Note string // put after the body: what only the running process knows
	// File is the body file when it is not Path/SKILL.md (a Claude plugin's
	// command); Root and Data are what the body's ${CLAUDE_PLUGIN_ROOT} and
	// ${CLAUDE_PLUGIN_DATA} stand for, "" for the rest.
	File string
	Root string
	Data string
}

// SkillMaxOut caps a skill body; every shipped skill fits it whole
// (TestBuiltinSkillsFitTheSkillTool).
const SkillMaxOut = 64000

// SkillText is a skill as the model loads it: its directory, then the body.
func SkillText(s SkillInfo) (string, error) {
	path := filepath.Join(s.Path, "SKILL.md")
	if s.File != "" {
		path = s.File
	}
	body, err := skill.BodyFile(path)
	if err != nil {
		return "", err
	}
	if s.Root != "" {
		body = plugin.ExpandRootData(body, s.Root, s.Data)
	}
	// the body is the whole procedure, loaded once: a cut leaves the model
	// acting on half of it, so say where the rest is, cutting the file whole
	// so the marker's line numbers are read offsets
	if len(body) > SkillMaxOut {
		raw, _ := os.ReadFile(path)
		body = TruncOut(string(raw), SkillMaxOut) + "\nRead the cut lines with the read tool: " + path
	}
	return "Skill directory: " + s.Path + "\n\n" + body, nil
}

// skillArgs is the skill tool's input and its schema source.
type skillArgs struct {
	Name string `json:"name"`
}

func runSkill(ctx context.Context, p *Pipeline, in skillArgs) (fantasy.ToolResponse, error) {
	if p.Skills == nil {
		return fantasy.NewTextErrorResponse("skills unavailable"), nil
	}
	name := in.Name
	var known []string
	for _, s := range p.Skills() {
		known = append(known, s.Name)
		if s.Name == name {
			path := cmp.Or(s.File, filepath.Join(s.Path, "SKILL.md"))
			fi, _ := os.Stat(path)
			text, err := SkillText(s)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			// loading is reading: a recipe can be corrected without a read
			p.markRead(path, fi)
			return fantasy.NewTextResponse(text + s.Note), nil
		}
	}
	slices.Sort(known)
	if len(known) == 0 {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("no skills installed (asked for %q)", name)), nil
	}
	return fantasy.NewTextErrorResponse(fmt.Sprintf("no such skill %q; available: %s", name, strings.Join(known, ", "))), nil
}
