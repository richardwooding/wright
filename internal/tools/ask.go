package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/richardwooding/agentkit"

	"github.com/richardwooding/wright/internal/policy"
)

type askArgs struct {
	Question string   `json:"question" jsonschema:"the question to put to the user"`
	Options  []string `json:"options,omitempty" jsonschema:"choices to offer; omit for a free-text answer"`
}

func (d *Deps) askUser() agentkit.Tool {
	return &tool{
		Tool: agentkit.Func(NameAskUser,
			"Ask the user a question and wait for the answer. Offer options when there is a small set of sensible choices.",
			d.runAsk),
		describe:   describeAsk,
		sequential: true,
	}
}

func describeAsk(args json.RawMessage) (policy.Request, Preview, error) {
	var a askArgs
	if err := decode(args, &a); err != nil {
		return policy.Request{}, Preview{}, err
	}
	if strings.TrimSpace(a.Question) == "" {
		return policy.Request{}, Preview{}, errors.New("question is required")
	}
	return policy.Request{Tool: NameAskUser, Args: args}, Preview{Title: NameAskUser, Body: a.Question}, nil
}

func (d *Deps) runAsk(ctx context.Context, a askArgs) (agentkit.Output, error) {
	if strings.TrimSpace(a.Question) == "" {
		return agentkit.Output{}, errors.New("question is required")
	}
	ans, err := d.Asker.Ask(ctx, Question{Text: a.Question, Options: a.Options, FreeText: len(a.Options) == 0})
	if err != nil {
		return agentkit.Output{}, err
	}
	if ans.Text == "" && ans.Index >= 0 && ans.Index < len(a.Options) {
		ans.Text = a.Options[ans.Index]
	}
	return agentkit.Text(ans.Text), nil
}
