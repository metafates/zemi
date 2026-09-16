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
	"strings"

	"charm.land/fantasy"
	"github.com/aymanbagabas/go-udiff"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/skills"
	"github.com/metafates/zemi/internal/store"
)

var todoStatuses = map[string]bool{"pending": true, "in_progress": true, "done": true}

func runTodos(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	if p.TodosRead == nil {
		return fantasy.NewTextErrorResponse("todos unavailable"), nil
	}
	if list, ok := input["todos"].([]any); ok {
		if p.TodosWrite == nil {
			return fantasy.NewTextErrorResponse("todos unavailable"), nil
		}
		var out []store.Todo
		for i, v := range list {
			m, ok := v.(map[string]any)
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("todo %d: not an object", i)), nil
			}
			content := str(m["content"])
			status := str(m["status"])
			if status == "completed" { // the name other harnesses taught models
				status = "done"
			}
			if content == "" {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("todo %d: content is required", i)), nil
			}
			if !todoStatuses[status] {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("todo %d: status must be pending, in_progress or done", i)), nil
			}
			out = append(out, store.Todo{
				Content: content, Status: status, ActiveForm: str(m["active_form"]), Ord: i,
			})
		}
		if err := p.TodosWrite(out); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}
	rows, err := p.TodosRead()
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if len(rows) == 0 {
		return fantasy.NewTextResponse("(no todos)"), nil
	}
	var b strings.Builder
	for _, td := range rows {
		mark := " "
		switch td.Status {
		case "done":
			mark = "x"
		case "in_progress":
			mark = ">"
		}
		fmt.Fprintf(&b, "[%s] %s\n", mark, td.Content)
	}
	return fantasy.NewTextResponse(b.String()), nil
}

// PlanVerdict is the user's answer to a presented plan.
type PlanVerdict struct {
	Approved bool
	Mode     string // permission mode chosen on approval, "" when unchanged
	Feedback string // what to change, when not approved
	Path     string // where the plan was saved, "" when not saved
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
		saved = "\nThe plan is saved at " + v.Path + "."
	}
	implement := " Implement it now: track its steps with todos, carry them through, verify, and report." + saved
	switch {
	case !ok:
		return stopTurn("The user dismissed the plan without approving it. Stop and wait for them to say how to proceed."), nil
	case v.Approved && v.Mode != "":
		return fantasy.NewTextResponse("The user approved the plan and plan mode is off; the permission mode is now " + v.Mode + "." + implement), nil
	case v.Approved:
		return fantasy.NewTextResponse("The user approved the plan." + implement), nil
	}
	return fantasy.NewTextResponse("The user has not approved the plan and asked for changes: " + v.Feedback +
		"\nRevise the plan, ask with the question tool if something is still unclear, and call plan again."), nil
}

// stopTurn is an error result that ends the turn: the user wants to speak next.
func stopTurn(text string) fantasy.ToolResponse {
	resp := fantasy.NewTextErrorResponse(text)
	resp.StopTurn = true
	return resp
}

// QuestionOption is one answer choice for a question ask.
type QuestionOption struct {
	Label       string
	Description string
}

// Question is one structured ask to the user, drawn by a Go-owned overlay.
// The user picks options (several when Multi) or types an answer on a
// last row named Other ("" = the default name); no options means an open
// question.
type Question struct {
	Question string
	Header   string // the tab label in a batch
	Options  []QuestionOption
	Multi    bool
	Other    string
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
		q := Question{Question: str(m["question"]), Header: str(m["header"]), Multi: m["multi_select"] == true}
		opts, _ := m["options"].([]any)
		for j, o := range opts {
			om, _ := o.(map[string]any)
			if str(om["label"]) == "" {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("question %d, option %d: label is required", i+1, j+1)), nil
			}
			q.Options = append(q.Options, QuestionOption{Label: str(om["label"]), Description: str(om["description"])})
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
	b.WriteString("The user answered:")
	for i, q := range qs {
		ans := strings.ReplaceAll(answers[i], " (Recommended)", "")
		if ans == "" {
			ans = "(not answered)"
		}
		fmt.Fprintf(&b, "\n- %q: %s", q.Question, ans)
	}
	return fantasy.NewTextResponse(b.String()), nil
}

// runOpenInEditor hands the file to the user and reports what they changed.
// The user's own edit is not a model write, so it needs no fs.write ask,
// and the file is marked read at its new mtime so the next edit tool call
// is not refused as stale.
func runOpenInEditor(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	if p.Edit == nil {
		return fantasy.NewTextResponse("No editor is available in this run. Make the change yourself with edit or write, or ask the user."), nil
	}
	path, err := filepath.Abs(str(input["path"]))
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	line, _ := input["line"].(float64)
	created := false
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		created = true
	}
	before, _ := os.ReadFile(path)
	unlock := lockFile(path)
	err = p.Edit(ctx, path, int(line), str(input["reason"]))
	unlock()
	if err != nil {
		if created {
			_ = os.Remove(path)
		}
		return stopTurn("The editor did not run: " + err.Error() + ". Stop and wait for the user."), nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	after, _ := os.ReadFile(path)
	if created && len(after) == 0 {
		_ = os.Remove(path)
		return fantasy.NewTextResponse("The user closed the editor without writing anything; the new file was not kept."), nil
	}
	p.markRead(path, fi)
	if string(before) == string(after) {
		return fantasy.NewTextResponse("The user closed the editor without changing " + path + "."), nil
	}
	return fantasy.NewTextResponse("The user edited " + path + ":\n" + udiff.Unified(path, path, lf(string(before)), lf(string(after)))), nil
}

// SkillInfo is one resolvable skill: name plus the directory holding
// SKILL.md (the host resolves enabled skills, DESIGN §10.1).
type SkillInfo struct {
	Name string
	Path string
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
			if v := p.Check(Request{Subject: "fs.read", Arg: path, Reason: "load skill"}); v.Decision != perm.Allow {
				return fantasy.NewTextErrorResponse("permission denied: " + v.Reason), nil
			}
			body, err := skills.Body(s.Path)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(truncOut("Skill directory: " + s.Path + "\n\n" + body)), nil
		}
	}
	slices.Sort(known)
	if len(known) == 0 {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("no skills installed (asked for %q)", name)), nil
	}
	return fantasy.NewTextErrorResponse(fmt.Sprintf("no such skill %q; available: %s", name, strings.Join(known, ", "))), nil
}
