// misc_tools.go ships todos, question and skill (DESIGN §6.4): session
// todo list, structured user questions and on-demand SKILL.md loading.
// Todos, questions and the skill index come from the host through the
// pipeline; a nil hook is an error result, never a panic.
package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
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
		b.WriteString(fmt.Sprintf("[%s] %s\n", mark, td.Content))
	}
	return fantasy.NewTextResponse(b.String()), nil
}

// QuestionOption is one answer choice for a question ask.
type QuestionOption struct {
	Label       string
	Description string
}

// Question is one structured ask to the user. The host renders it as a
// Go-owned overlay; the tool returns the chosen label.
type Question struct {
	Question    string
	Description string
	Options     []QuestionOption
}

func runQuestion(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	if p.Question == nil {
		return fantasy.NewTextErrorResponse("no UI available for questions"), nil
	}
	q := Question{Question: str(input["question"]), Description: str(input["description"])}
	opts, ok := input["options"].([]any)
	if !ok || len(opts) == 0 {
		return fantasy.NewTextErrorResponse("options must be a non-empty array"), nil
	}
	for i, v := range opts {
		m, ok := v.(map[string]any)
		if !ok {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("option %d: not an object", i)), nil
		}
		label := str(m["label"])
		if label == "" {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("option %d: label is required", i)), nil
		}
		q.Options = append(q.Options, QuestionOption{Label: label, Description: str(m["description"])})
	}
	answer, ok := p.Question(q)
	if !ok {
		return fantasy.NewTextErrorResponse("question declined by user"), nil
	}
	return fantasy.NewTextResponse("user answered: " + answer), nil
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
			if v := p.check(Request{Subject: "fs.read", Arg: path, Reason: "load skill"}); v.Decision != perm.Allow {
				return fantasy.NewTextErrorResponse("permission denied: " + v.Reason), nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(truncOut(string(data))), nil
		}
	}
	sort.Strings(known)
	if len(known) == 0 {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("no skills installed (asked for %q)", name)), nil
	}
	return fantasy.NewTextErrorResponse(fmt.Sprintf("no such skill %q; available: %s", name, strings.Join(known, ", "))), nil
}
