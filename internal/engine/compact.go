package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/richardwooding/agentkit"

	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/prompt"
	"github.com/richardwooding/wright/internal/session"
)

// Compact summarises the session now and continues in a new session whose
// transcript starts from the summary. The old session stays intact (stores
// are lossless), so nothing is lost; only what the model sees shrinks.
func (e *Engine) Compact(ctx context.Context) error {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return ErrRunning
	}
	store, old, fast := e.opts.Store, e.session, e.fast
	e.mu.Unlock()
	if store == nil {
		return errors.New("engine: no session store")
	}
	msgs, err := store.Load(ctx, old)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		return errors.New("nothing to compact")
	}
	est := agentkit.CharEstimator{}
	before := est.Estimate(msgs)
	compactor := agentkit.Chain(agentkit.Summarize(fast, 0, agentkit.WithSummaryPrompt(prompt.Summary())), agentkit.StripReasoning())
	compacted, err := compactor.Compact(ctx, msgs, 1)
	if err != nil {
		return err
	}
	after := est.Estimate(compacted)
	id := session.NewID(e.now())
	if err := store.Append(ctx, id, compacted...); err != nil {
		return err
	}
	e.mu.Lock()
	oldMeta := e.meta
	e.session = id
	e.meta = session.Meta{ID: id, Title: oldMeta.Title, Model: oldMeta.Model, Workspace: oldMeta.Workspace, Created: e.now(), Mode: oldMeta.Mode}
	e.lastIn = after
	e.mu.Unlock()
	if err := e.touch(ctx); err != nil {
		return err
	}
	e.audit(audit.Event{Kind: audit.KindCompaction, Text: fmt.Sprintf("manual %d→%d, continued from %s", before, after, old)})
	e.emit(Event{
		Kind: KindCompact, Compact: &CompactInfo{Reason: "manual", Before: before, After: after},
		Text: fmt.Sprintf("context compacted: %s → %s tokens (continuing as session %s)", tokens(before), tokens(after), id),
	})
	return nil
}

// Undo restores every file the most recent run changed and returns their
// paths. Shell side effects are not reverted; the notice says so.
func (e *Engine) Undo() ([]string, error) {
	e.mu.Lock()
	running, snaps := e.running, e.opts.Snapshots
	e.mu.Unlock()
	if running {
		return nil, ErrRunning
	}
	if snaps == nil {
		return nil, errors.New("engine: snapshots are disabled")
	}
	runs := snaps.Runs()
	if len(runs) == 0 {
		return nil, errors.New("nothing to undo")
	}
	entries, err := snaps.RestoreRun(runs[0])
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, en := range entries {
		paths = append(paths, en.Path)
	}
	e.audit(audit.Event{Kind: audit.KindNotice, Run: runs[0], Text: fmt.Sprintf("undo restored %d file(s)", len(paths))})
	e.emit(Event{Kind: KindNotice, Text: fmt.Sprintf("restored %d file(s) from before run %s; commands that ran are not reverted", len(paths), runs[0])})
	return paths, nil
}
