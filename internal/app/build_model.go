package app

import (
	"context"
	"fmt"
	"time"

	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/model"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/redact"
	"github.com/richardwooding/wright/internal/session"
	"github.com/richardwooding/wright/internal/snapshot"
)

// modelAndSession picks the model, opens the session store and the
// per-session audit log and snapshot store. The redactor is created here
// because the audit log needs it before any tool runs.
func (b *builder) modelAndSession() error {
	// Before Detect, which treats a name no provider claims as a hard error:
	// "ramalama/gpt-oss:20b" is unroutable until its endpoint is registered.
	warnings, err := model.Endpoints(b.settings.Model.Endpoints)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		b.warn("%s", w)
	}
	choice, err := model.Detect(b.ctx, b.settings.Model, b.o.Model, b.env, func(ctx context.Context) ([]string, error) {
		return model.ProbeOllama(ctx, "")
	})
	if err != nil {
		return err
	}
	b.choice = choice

	store, err := session.Open(b.layered.Paths.UserData, b.ws)
	if err != nil {
		return err
	}
	b.store = store
	if b.sessionID, err = b.pickSession(); err != nil {
		return err
	}
	b.restoreMode()
	if b.settings.RedactionEnabled() {
		// The resolved GitHub token goes in as a literal. The default
		// patterns already know the common gh spellings; this catches the
		// ones they do not (a legacy hex PAT matches nothing by shape) and
		// a token abutting other characters. It is a backstop, not a
		// boundary: a base64 defeats every pattern, which is why the
		// per-call network gate is the control that matters.
		var opts []redact.Option
		if b.githubToken != "" {
			opts = append(opts, redact.WithExtra(redact.Literal("github-token", b.githubToken, 4)))
		}
		b.redactor = redact.New(opts...)
	} else {
		b.warn("secret redaction is OFF (settings.redaction=false)")
		if b.githubToken != "" {
			b.warn("github auth is on with redaction off: the token can appear verbatim in tool output and the audit log")
		}
	}
	anchors := audit.OpenAnchors(b.layered.Paths.AuditAnchorDir())
	if b.auditLog, err = audit.OpenAnchored(store.AuditPath(b.sessionID), b.redactor, anchors); err != nil {
		return err
	}
	if b.snaps, err = snapshot.Open(store.SnapshotDir(b.sessionID)); err != nil {
		return err
	}
	return nil
}

// restoreMode puts a resumed session back in the permission mode it was left
// in. Resuming is meant to be continuing, and a session switched to plan and
// picked up the next day came back in default mode — silently, which is the
// wrong direction for a mode to move on its own.
//
// It runs here rather than in resolveMode because the session is not known
// until this phase; the policy engine already exists, so the mode is changed
// through it. An explicit --mode or WRIGHT_MODE still wins: someone naming a
// mode on the command line means it. A mode recorded as bypass is ignored —
// bypass is reachable only through its own flag, and policy.SetMode refuses
// it anyway.
func (b *builder) restoreMode() {
	if b.resumed == "" || b.o.Mode != "" || b.env("WRIGHT_MODE") != "" {
		return
	}
	m, found, err := b.store.Get(b.ctx, b.resumed)
	if err != nil || !found || m.Mode == "" {
		return
	}
	mode, err := policy.ParseMode(m.Mode)
	if err != nil || mode == b.mode {
		return
	}
	if err := b.pol.SetMode(mode); err != nil {
		return // bypass, or anything else the engine will not take
	}
	b.mode = mode
	b.warn("this session was left in %s mode and resumes in it; pass --mode to start it in another", mode)
}

// pickSession honours --resume, then --continue (falling back to a new
// session when there is nothing to continue), else starts fresh.
func (b *builder) pickSession() (string, error) {
	switch {
	case b.o.Resume != "":
		_, found, err := b.store.Get(b.ctx, b.o.Resume)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("app: session %q not found (see `wright sessions list`)", b.o.Resume)
		}
		b.resumed = b.o.Resume
		return b.o.Resume, nil
	case b.o.Continue:
		m, found, err := b.store.Latest(b.ctx)
		if err != nil {
			return "", err
		}
		if found {
			b.resumed = m.ID
			return m.ID, nil
		}
		b.warn("no previous session in this workspace; starting a new one")
	}
	return session.NewID(time.Now()), nil
}
