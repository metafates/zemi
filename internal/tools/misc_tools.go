// misc_tools.go ships todos, question, plan and skill (DESIGN §6.4):
// session todo list, structured user questions, plan approval and
// on-demand SKILL.md loading. They reach the host through pipeline hooks;
// a nil hook is a result the model can act on (no user to ask), never a
// panic.

package tools

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/skill"
	"github.com/metafates/zemi/internal/store"
)

func runTodos(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	if p.TodosRead == nil {
		return fantasy.NewTextErrorResponse("there is no todo list in this run: keep the steps in your reply instead"), nil
	}
	if out, cancelled, ok, err := ParseTodos(input); ok {
		if p.TodosWrite == nil {
			return fantasy.NewTextErrorResponse("there is no todo list in this run: keep the steps in your reply instead"), nil
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
		out = append(out, store.Todo{Content: content, Status: status, ActiveForm: str(m["active_form"])})
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
	Label       string // without the " (Recommended)" the model marks its pick with
	Description string
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
// answers on a typed row. Options may carry a short Description shown
// beside their label.
type Question struct {
	Type        QuestionType
	Question    string
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
			return fantasy.NewTextErrorResponse(fmt.Sprintf("question %d: type must be single_choice, multi_choice or free_text", i+1)), nil
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
			label, rec := strings.CutSuffix(str(om["label"]), " (Recommended)")
			q.Options = append(q.Options, QuestionOption{Label: label, Description: str(om["description"]), Recommended: rec})
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
			return fantasy.NewTextResponse(text + s.Note), nil
		}
	}
	slices.Sort(known)
	if len(known) == 0 {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("no skills installed (asked for %q)", name)), nil
	}
	return fantasy.NewTextErrorResponse(fmt.Sprintf("no such skill %q; available: %s", name, strings.Join(known, ", "))), nil
}
