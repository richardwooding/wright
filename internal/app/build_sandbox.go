package app

import (
	"github.com/richardwooding/wright/internal/sandbox"
)

// sandboxing picks the OS sandbox backend and builds the base Spec every
// bash call starts from: workspace roots and build caches read-write, extra
// mounts from settings, an allowlisted environment and no network unless the
// flag or settings grant it.
func (b *builder) sandboxing() error {
	want := b.o.Sandbox
	if want == "" || want == "auto" {
		want = b.settings.Sandbox.Backend
	}
	backend, warns := sandbox.Detect(b.ctx, want)
	for _, w := range warns {
		b.warn("sandbox: %s", w)
	}
	b.backend = backend

	rw := append(append([]string(nil), b.ws.Roots...), sandbox.Caches()...)
	rw = append(rw, expandAll(b.ws, b.settings.Sandbox.ExtraRW)...)
	var pass []string
	if b.trusted {
		pass = b.settings.Sandbox.PassEnv
	}
	// The protected paths have to exist before the sandbox is built: one
	// that does not cannot be bound read-only, and a payload would simply
	// create it and write into the directory it just made.
	sandbox.EnsureProtected(b.ws.Roots)
	b.spec = sandbox.Spec{
		Dir:       b.ws.Root(),
		Env:       sandbox.Env(pass, nil),
		ReadWrite: rw,
		Roots:     b.ws.Roots,
		ReadOnly:  expandAll(b.ws, b.settings.Sandbox.ExtraRO),
		Network:   b.o.AllowNetwork || b.settings.Sandbox.AllowNetwork,
	}
	// What the backend can enforce depends on this workspace, not just on
	// the machine, so the limits are asked for with the spec in hand.
	for _, w := range sandbox.SpecWarnings(backend, b.spec) {
		b.warn("sandbox: %s", w)
	}
	return nil
}
