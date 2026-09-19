package app

import (
	"context"
	"fmt"
	"time"

	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/model"
	"github.com/richardwooding/wright/internal/redact"
	"github.com/richardwooding/wright/internal/session"
	"github.com/richardwooding/wright/internal/snapshot"
)

// modelAndSession picks the model, opens the session store and the
// per-session audit log and snapshot store. The redactor is created here
// because the audit log needs it before any tool runs.
func (b *builder) modelAndSession() error {
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
	if b.settings.RedactionEnabled() {
		b.redactor = redact.New()
	} else {
		b.warn("secret redaction is OFF (settings.redaction=false)")
	}
	if b.auditLog, err = audit.Open(store.AuditPath(b.sessionID), b.redactor); err != nil {
		return err
	}
	if b.snaps, err = snapshot.Open(store.SnapshotDir(b.sessionID)); err != nil {
		return err
	}
	return nil
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
		return b.o.Resume, nil
	case b.o.Continue:
		m, found, err := b.store.Latest(b.ctx)
		if err != nil {
			return "", err
		}
		if found {
			return m.ID, nil
		}
		b.warn("no previous session in this workspace; starting a new one")
	}
	return session.NewID(time.Now()), nil
}
