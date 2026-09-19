//go:build linux

package sandbox

import (
	"errors"
	"os"
	"sync"
	"syscall"
)

// capSysAdmin is CAP_SYS_ADMIN, the capability mount(2) needs. The helper is
// given it as an *ambient* capability, which is the only kind that survives
// the execve that starts the helper: a user namespace grants full
// capabilities at clone time, but execve of an ordinary file by a non-root
// uid drops them all, which is why the mounts used to fail with EPERM.
const capSysAdmin = 21

// nsProbe caches whether this kernel lets an unprivileged process create the
// namespaces the landlock backend needs. Hardened kernels and some container
// runtimes forbid unprivileged user namespaces outright.
var nsProbe = struct {
	once sync.Once
	err  error
}{}

// nsAttr returns the process attributes for the landlock helper, or nil when
// this kernel cannot create the namespaces.
//
// The helper gets three things from them. A fresh *network* namespace, when
// the spec asks for no network: Landlock's own RestrictNet covers TCP bind
// and connect only, so UDP, ICMP and raw sockets stayed open without it. A
// fresh *mount* namespace, so the helper can bind-mount `.git/hooks`,
// `.git/config` and `.wright` read-only over themselves — Landlock rules are
// additive and can never subtract a subtree from a writable root, so a mount
// is the only way to keep those read-only *and* leave the workspace root
// writable. And a user namespace, which is what makes the other two possible
// unprivileged.
//
// The uid and gid are mapped to themselves, so files keep their owner, git
// does not see "dubious ownership" and the payload is not pretending to be
// root. The ambient CAP_SYS_ADMIN exists only for the mounts and is dropped
// before the payload is executed.
func nsAttr(network bool) *syscall.SysProcAttr {
	if err := nsAvailable(); err != nil {
		return nil
	}
	attr := namespaceAttr()
	if network {
		attr.Cloneflags &^= syscall.CLONE_NEWNET
	}
	return attr
}

// namespaceAttr builds the full attribute set the probe and nsAttr share.
func namespaceAttr() *syscall.SysProcAttr {
	uid, gid := os.Getuid(), os.Getgid()
	return &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		// Unshareflags rather than Cloneflags for the mount namespace: the Go
		// runtime then also remounts / as MS_REC|MS_PRIVATE in the child, so
		// the bind mounts the helper makes cannot propagate back to the host.
		Unshareflags: syscall.CLONE_NEWNS,
		UidMappings:  []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings:  []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
		AmbientCaps:  []uintptr{capSysAdmin},
	}
}

// nsAvailable reports why the namespaces cannot be created here, or nil. It
// is measured, not guessed: a child is cloned with exactly the attributes
// the helper would use and pointed at a path that cannot exist, so the clone,
// the unshare and the ambient capability are what is being tested and the
// exec never happens.
func nsAvailable() error {
	nsProbe.once.Do(func() {
		const probePath = "/nonexistent/wright-namespace-probe"
		pid, err := syscall.ForkExec(probePath, []string{probePath}, &syscall.ProcAttr{Sys: namespaceAttr()})
		if err != nil && !errors.Is(err, syscall.ENOENT) {
			nsProbe.err = err
		}
		if pid > 0 {
			var ws syscall.WaitStatus
			_, _ = syscall.Wait4(pid, &ws, 0, nil)
		}
	})
	return nsProbe.err
}
