//go:build !linux

package sandbox

import (
	"context"
	"os/exec"
	"syscall"
)

// landlockBackend is Linux-only; elsewhere it reports ErrUnsupported.
type landlockBackend struct{ exe string }

func (landlockBackend) Name() string                    { return NameLandlock }
func (landlockBackend) Available(context.Context) error { return ErrUnsupported }

func (landlockBackend) Command(context.Context, Spec) (*exec.Cmd, error) {
	return nil, ErrUnsupported
}

// LandlockWithExecutable returns the (unsupported) landlock backend.
func LandlockWithExecutable(string) Backend { return landlockBackend{} }

// LandlockABI reports 0 and ErrUnsupported off Linux.
func LandlockABI() (int, error) { return 0, ErrUnsupported }

func enterLandlock([]string, []string, bool) (int, error) { return 0, ErrUnsupported }

func sysExec(path string, argv, env []string) error { return syscall.Exec(path, argv, env) }
