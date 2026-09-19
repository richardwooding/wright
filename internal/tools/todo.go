package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/richardwooding/agentkit"

	"github.com/richardwooding/wright/internal/policy"
)

// Todo statuses.
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
)

type todoArgs struct {
	Todos []Todo `json:"todos" jsonschema:"the complete task list; replaces the previous one"`
}

func (d *Deps) todoWrite() agentkit.Tool {
	return &tool{
		Tool: agentkit.Func(NameTodoWrite,
			"Replace the task list shown to the user. Send the whole list every time with current statuses.",
			d.runTodoWrite),
		describe: describeTodo,
	}
}

func describeTodo(args json.RawMessage) (policy.Request, Preview, error) {
	var a todoArgs
	if err := decode(args, &a); err != nil {
		return policy.Request{}, Preview{}, err
	}
	return policy.Request{Tool: NameTodoWrite, Args: args}, Preview{Title: NameTodoWrite, Body: summarize(a.Todos)}, nil
}

func (d *Deps) runTodoWrite(_ context.Context, a todoArgs) (agentkit.Output, error) {
	seen := map[string]bool{}
	for i, t := range a.Todos {
		switch {
		case t.ID == "":
			return agentkit.Output{}, fmt.Errorf("todo %d has no id", i)
		case seen[t.ID]:
			return agentkit.Output{}, fmt.Errorf("duplicate todo id %q", t.ID)
		case t.Content == "":
			return agentkit.Output{}, fmt.Errorf("todo %q has no content", t.ID)
		case t.Status != StatusPending && t.Status != StatusInProgress && t.Status != StatusCompleted:
			return agentkit.Output{}, fmt.Errorf("todo %q has status %q; use pending, in_progress or completed", t.ID, t.Status)
		}
		seen[t.ID] = true
	}
	d.Todos.Replace(a.Todos)
	return agentkit.Text(summarize(a.Todos)), nil
}

// summarize renders the status counts.
func summarize(todos []Todo) string {
	var pending, active, done int
	for _, t := range todos {
		switch t.Status {
		case StatusInProgress:
			active++
		case StatusCompleted:
			done++
		default:
			pending++
		}
	}
	return fmt.Sprintf("%d todo%s: %d pending, %d in progress, %d completed", len(todos), plural(len(todos)), pending, active, done)
}
