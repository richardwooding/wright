// Package enginetest holds the scripted model client the engine, headless
// and app tests share. It is a test helper that lives outside _test files so
// several packages can use one implementation instead of copying it.
package enginetest

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/richardwooding/llmkit/core"
)

// Scripted is a core.Chatter that returns canned responses in order and
// records every request it saw. Once the script is exhausted it answers with
// a plain "done" text, or with Err when that is set.
type Scripted struct {
	// Responses are handed out in order, one per Chat call.
	Responses []*core.Response
	// Block, when non-nil, makes the first call wait until it is closed (or
	// the context ends) so tests can observe an in-flight run.
	Block chan struct{}
	// Err is returned instead of "done" once Responses run out.
	Err error

	mu   sync.Mutex
	seen []*core.Request
}

// Chat implements core.Chatter.
func (s *Scripted) Chat(ctx context.Context, req *core.Request) (*core.Response, error) {
	s.mu.Lock()
	cp := *req
	s.seen = append(s.seen, &cp)
	n := len(s.seen)
	block := s.Block
	s.mu.Unlock()
	if block != nil && n == 1 {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if n > len(s.Responses) {
		if s.Err != nil {
			return nil, s.Err
		}
		return TextResp("done"), nil
	}
	return s.Responses[n-1], nil
}

// Requests returns a copy of every request seen so far.
func (s *Scripted) Requests() []*core.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*core.Request(nil), s.seen...)
}

// TextResp is a final assistant text turn with a fixed, non-zero usage so
// cost and context tests have numbers to check.
func TextResp(t string) *core.Response {
	return &core.Response{Message: core.Assistant(core.Text(t)), FinishReason: core.FinishStop, Usage: core.Usage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110}}
}

// CallResp is an assistant turn that calls one tool.
func CallResp(id, name, args string) *core.Response {
	return &core.Response{Message: core.Assistant(core.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}), FinishReason: core.FinishToolCalls, Usage: core.Usage{InputTokens: 120, OutputTokens: 5, TotalTokens: 125}}
}
