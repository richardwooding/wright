package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/richardwooding/agentkit"

	"github.com/richardwooding/wright/internal/policy"
)

const (
	defaultSearchLimit = 5
	maxSearchLimit     = 20
)

type searchArgs struct {
	Query string `json:"query" jsonschema:"search query"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum results (default 5, max 20)"`
}

func (d *Deps) webSearch() agentkit.Tool {
	return &tool{
		Tool: agentkit.Func(NameWebSearch,
			"Search the web and return titles, URLs and snippets.",
			d.runSearch),
		describe: describeSearch,
	}
}

func describeSearch(args json.RawMessage) (policy.Request, Preview, error) {
	var a searchArgs
	if err := decode(args, &a); err != nil {
		return policy.Request{}, Preview{}, err
	}
	if strings.TrimSpace(a.Query) == "" {
		return policy.Request{}, Preview{}, errors.New("query is required")
	}
	return policy.Request{Tool: NameWebSearch, Args: args}, Preview{Title: NameWebSearch, Body: a.Query}, nil
}

func (d *Deps) runSearch(ctx context.Context, a searchArgs) (agentkit.Output, error) {
	if strings.TrimSpace(a.Query) == "" {
		return agentkit.Output{}, errors.New("query is required")
	}
	limit := a.Limit
	if limit < 1 {
		limit = defaultSearchLimit
	}
	limit = min(limit, maxSearchLimit)
	results, err := d.Search.Search(ctx, a.Query, limit)
	if err != nil {
		return agentkit.Output{}, err
	}
	if len(results) == 0 {
		return agentkit.Text("no results for " + a.Query), nil
	}
	var b strings.Builder
	for i, r := range results {
		if i == limit {
			break
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, strings.TrimSpace(r.Title), r.URL)
		if s := strings.TrimSpace(r.Snippet); s != "" {
			b.WriteString("   " + s + "\n")
		}
	}
	return agentkit.Text(d.redact(ctx, NameWebSearch, strings.TrimRight(b.String(), "\n"))), nil
}
