// misc_tools.go ships todos, question, plan and skill (DESIGN §6.4):
// session todo list, structured user questions, plan approval and
// on-demand SKILL.md loading. They reach the host through pipeline hooks;
// a nil hook is a result the model can act on (no user to ask), never a
// panic.

package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/skill"
	"github.com/metafates/zemi/internal/store"
)

func runTodos(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	if p.TodosRead == nil {
		return fantasy.NewTextErrorResponse("todos unavailable"), nil
	}
	if out, cancelled, ok, err := ParseTodos(input); ok {
		if p.TodosWrite == nil {
			return fantasy.NewTextErrorResponse("todos unavailable"), nil
		}
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
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
		if cancelled > 0 {
			note = fmt.Sprintf("; dropped %d cancelled", cancelled)
		}
		if active > 1 {
			note += fmt.Sprintf("; %d items are in_progress; keep exactly one", active)
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

// ParseTodos reads the list a todos call sets, dropping the cancelled items it
// counts; ok is false for a call that only reads the list.
func ParseTodos(input map[string]any) (out []store.Todo, cancelled int, ok bool, err error) {
	list, ok := input["todos"].([]any)
	for i, v := range list {
		m, isObj := v.(map[string]any)
		if !isObj {
			return nil, 0, true, fmt.Errorf("todo %d: not an object", i+1)
		}
		content := str(m["content"])
		status := store.TodoStatus(str(m["status"]))
		// the names other harnesses taught models
		if status == "completed" {
			status = store.TodoDone
		}
		if status == "cancelled" {
			cancelled++
			continue
		}
		if content == "" {
			return nil, 0, true, fmt.Errorf("todo %d: content is required", i+1)
		}
		if !slices.Contains(store.TodoStatuses, status) {
			return nil, 0, true, fmt.Errorf("todo %d: status must be pending, in_progress or done", i+1)
		}
		out = append(out, store.Todo{Content: content, Status: status, ActiveForm: str(m["active_form"]), Ord: len(out)})
	}
	return out, cancelled, ok, nil
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
	Profile  string // agent restored on approval, "" when unchanged
	Feedback string // what to change, when not approved
	Path     string // where the plan was saved, "" when not saved
}

// ImplementPlan is what an approved plan's result tells the agent to do.
const ImplementPlan = "Implement it; track its steps with todos if it has several."

// The plan results PlanOutcome reads back.
const (
	planApproved = "The user approved the plan"
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
	if !strings.HasPrefix(output, planApproved) {
		return output, ""
	}
	_, path, _ = strings.Cut(output, planSaved)
	return "approved", strings.TrimSuffix(path, ".")
}

func runPlan(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	plan := strings.TrimSpace(str(input["plan"]))
	if plan == "" {
		return fantasy.NewTextErrorResponse("plan is required"), nil
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
		return stopTurn("The user dismissed the plan without approving it. Stop and wait for them to say how to proceed."), nil
	case v.Approved && v.Profile != "":
		// the read-only turn that proposed this ends here; the run's planFollowup
		// starts implementation as the restored agent, so do not keep going
		return endTurn(planApproved + "; you are back as the " + v.Profile + " agent. Implementation runs in the next turn." + saved), nil
	case v.Approved:
		return fantasy.NewTextResponse(planApproved + "." + implement), nil
	}
	return fantasy.NewTextResponse(planChanges + v.Feedback + planRevise +
		", ask with the question tool if something is still unclear, and call plan again."), nil
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
	Label       string
	Description string
}

// QuestionType is how a question is asked; runQuestion maps it onto the
// overlay's list model.
type QuestionType string

const (
	TypeYesNo    QuestionType = "yes_no"
	TypeSingle   QuestionType = "single_choice"
	TypeMulti    QuestionType = "multi_choice"
	TypeFreeText QuestionType = "free_text"
)

var questionTypes = []QuestionType{TypeYesNo, TypeSingle, TypeMulti, TypeFreeText}

// Question is one structured ask to the user, drawn by a Go-owned overlay.
// Type must be one of the four: yes_no and single_choice ask one option,
// multi_choice marks several, free_text answers on a typed row. Options may
// carry a short Description shown under their label.
type Question struct {
	Type        QuestionType
	Question    string
	Description string // markdown, shown under the question
	Header      string // the tab label in a batch
	Options     []QuestionOption
}

func (q Question) Multi() bool { return q.Type == TypeMulti }

// ListRows is how many rows the selection list shows: the options plus a
// "type your own" tail row for the choice types; yes_no and free_text have
// no tail (yes_no is its two options, free_text only the typed row).
func (q Question) ListRows() int {
	if q.Type == TypeYesNo {
		return len(q.Options)
	}
	if q.Type == TypeFreeText {
		return 0
	}
	return len(q.Options) + 1
}

const maxQuestions = 4

func runQuestion(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	raw, _ := input["questions"].([]any)
	if len(raw) == 0 || len(raw) > maxQuestions {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("questions must hold 1 to %d questions", maxQuestions)), nil
	}
	qs := make([]Question, 0, len(raw))
	for i, v := range raw {
		m, _ := v.(map[string]any)
		if str(m["question"]) == "" {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("question %d: the question text is required", i+1)), nil
		}
		q := Question{
			Type:        QuestionType(str(m["type"])),
			Question:    str(m["question"]),
			Description: str(m["description"]),
			Header:      str(m["header"]),
		}
		if !slices.Contains(questionTypes, q.Type) {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("question %d: type must be yes_no, single_choice, multi_choice or free_text", i+1)), nil
		}
		if q.Type == TypeFreeText {
			qs = append(qs, q)
			continue
		}
		opts, _ := m["options"].([]any)
		for j, o := range opts {
			om, _ := o.(map[string]any)
			if str(om["label"]) == "" {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("question %d, option %d: label is required", i+1, j+1)), nil
			}
			q.Options = append(q.Options, QuestionOption{Label: str(om["label"]), Description: str(om["description"])})
		}
		if q.Type == TypeYesNo && len(q.Options) == 0 {
			q.Options = []QuestionOption{{Label: "Yes"}, {Label: "No"}}
		}
		qs = append(qs, q)
	}
	if p.Question == nil {
		return fantasy.NewTextResponse("No user is available to answer. Take the option you would recommend, say which assumption you made, and continue."), nil
	}
	answers, ok := p.Question(qs)
	if !ok || len(answers) != len(qs) {
		return stopTurn("The user dismissed the questions. Stop and wait for them to say how to proceed."), nil
	}
	var b strings.Builder
	b.WriteString(questionAnswered)
	for i, q := range qs {
		ans := strings.ReplaceAll(answers[i], " (Recommended)", "")
		if ans == "" {
			ans = questionOpen
		}
		fmt.Fprintf(&b, "\n- %q: %s", q.Question, ans)
	}
	return fantasy.NewTextResponse(b.String()), nil
}

// The question result QuestionAnswers reads back.
const (
	questionAnswered = "The user answered:"
	questionOpen     = "(not answered: use your recommended option, or your best judgment if you gave none)"
)

// QuestionAnswers is each question and its answer in a question result,
// which is written for the model, for its row to show: "" for a question the
// user left open. Any other result is nil.
func QuestionAnswers(output string) [][2]string {
	lines := strings.Split(output, "\n")
	if lines[0] != questionAnswered || len(lines) < 2 {
		return nil
	}
	qa := make([][2]string, 0, len(lines)-1)
	for _, l := range lines[1:] {
		l, dash := strings.CutPrefix(l, "- ")
		quoted, err := strconv.QuotedPrefix(l)
		ans, colon := strings.CutPrefix(l[len(quoted):], ": ")
		if !dash || err != nil || !colon {
			return nil
		}
		q, _ := strconv.Unquote(quoted)
		if ans == questionOpen {
			ans = ""
		}
		qa = append(qa, [2]string{q, ans})
	}
	return qa
}

// SkillInfo is one resolvable skill: name plus the directory holding
// SKILL.md (the host resolves enabled skills, DESIGN §10.1).
type SkillInfo struct {
	Name string
	Path string
}

// SkillMaxOut caps a skill body; every shipped skill fits it whole
// (TestBuiltinSkillsFitTheSkillTool).
const SkillMaxOut = 64000

// SkillText is a skill as the model loads it: its directory, then the body.
func SkillText(dir string) (string, error) {
	body, err := skill.Body(dir)
	if err != nil {
		return "", err
	}
	// the body is the whole procedure, loaded once: a cut leaves the model
	// acting on half of it, so say where the rest is, cutting the file whole
	// so the marker's line numbers are read offsets
	if len(body) > SkillMaxOut {
		path := filepath.Join(dir, "SKILL.md")
		raw, _ := os.ReadFile(path)
		body = TruncOut(string(raw), SkillMaxOut) + "\nRead the cut lines with the read tool: " + path
	}
	return "Skill directory: " + dir + "\n\n" + body, nil
}

func runSkill(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	if p.Skills == nil {
		return fantasy.NewTextErrorResponse("skills unavailable"), nil
	}
	name := str(input["name"])
	var known []string
	for _, s := range p.Skills() {
		known = append(known, s.Name)
		if s.Name == name {
			path := filepath.Join(s.Path, "SKILL.md")
			if v := p.Check(Request{Tool: ToolSkill, Subject: perm.SubjectRead, Arg: path}); v.Decision != perm.Allow {
				return fantasy.NewTextErrorResponse(DeniedText(v, "skill "+name)), nil
			}
			fi, _ := os.Stat(path)
			text, err := SkillText(s.Path)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			// loading is reading: a recipe can be corrected without a read
			p.markRead(path, fi)
			return fantasy.NewTextResponse(text), nil
		}
	}
	slices.Sort(known)
	if len(known) == 0 {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("no skills installed (asked for %q)", name)), nil
	}
	return fantasy.NewTextErrorResponse(fmt.Sprintf("no such skill %q; available: %s", name, strings.Join(known, ", "))), nil
}
