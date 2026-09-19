package policy

import (
	"context"

	"github.com/richardwooding/wright/internal/git"
	"github.com/richardwooding/wright/internal/policy/shellclass"
	"github.com/richardwooding/wright/internal/workspace"
)

// shellWS adapts the real workspace (and git) to shellclass.Workspace, which
// is kept as a small interface so shellclass stays a leaf package.
type shellWS struct {
	ws       *workspace.Workspace
	tracked  func(abs string) bool
	branches []string
}

// NewShellWorkspace returns the shellclass view of ws. tracked answers
// "is this path in the git index"; nil uses the git package with its 2 s
// timeout. protectedBranches overrides the classifier's default set when
// non-empty.
func NewShellWorkspace(ws *workspace.Workspace, tracked func(abs string) bool, protectedBranches []string) shellclass.Workspace {
	if tracked == nil {
		root := ws.Root()
		tracked = func(abs string) bool { return git.IsTracked(context.Background(), root, abs) }
	}
	return shellWS{ws: ws, tracked: tracked, branches: protectedBranches}
}

func (s shellWS) Resolve(p string) (string, bool, error) { return s.ws.Resolve(p) }
func (s shellWS) IsSecretFile(p string) bool             { return s.ws.IsSecretFile(p) }
func (s shellWS) IsProtected(p string) bool              { return s.ws.IsProtected(p) }
func (s shellWS) IsTracked(p string) bool                { return s.tracked(p) }
func (s shellWS) Home() string                           { return s.ws.Home }
func (s shellWS) Roots() []string                        { return s.ws.Roots }
func (s shellWS) ProtectedBranches() []string            { return s.branches }
