//go:build linux

package sandbox

import (
	"errors"
	"os"
	"sync"
	"syscall"
)

// netnsProbe caches whether this kernel lets an unprivileged process create a
// user + network namespace. Some distributions and hardened kernels forbid
// unprivileged user namespaces outright.
var netnsProbe = struct {
	once sync.Once
	err  error
}{}

// netnsAttr returns the process attributes that put the landlock helper in a
// fresh, empty network namespace, or nil when this kernel cannot do it.
//
// Landlock's own network support (RestrictNet) covers TCP bind and connect
// only: UDP, ICMP and anything else remain reachable, so "no network" was a
// false guarantee. An empty network namespace has no route to anywhere and
// no interface but a down loopback, which is the same confinement bwrap gets
// from --unshare-all.
//
// The user namespace maps the caller's own uid and gid unchanged, so files in
// the workspace keep their owner and git does not see "dubious ownership";
// execve drops the capabilities the new namespace grants, so the payload
// gains nothing from it.
func netnsAttr() *syscall.SysProcAttr {
	if err := netnsAvailable(); err != nil {
		return nil
	}
	uid, gid := os.Getuid(), os.Getgid()
	return &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
	}
}

// netnsAvailable reports why an unprivileged network namespace cannot be
// created here, or nil. It is measured, not guessed: a child is cloned with
// the same flags the helper would use and pointed at a path that cannot
// exist, so the clone is what is being tested and the exec never happens.
func netnsAvailable() error {
	netnsProbe.once.Do(func() {
		uid, gid := os.Getuid(), os.Getgid()
		attr := &syscall.ProcAttr{Sys: &syscall.SysProcAttr{
			Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
		}}
		const probePath = "/nonexistent/wright-netns-probe"
		pid, err := syscall.ForkExec(probePath, []string{probePath}, attr)
		switch {
		case errors.Is(err, syscall.ENOENT):
			// The clone succeeded; only the deliberate exec failed.
		case err != nil:
			netnsProbe.err = err
		}
		if pid > 0 {
			var ws syscall.WaitStatus
			_, _ = syscall.Wait4(pid, &ws, 0, nil)
		}
	})
	return netnsProbe.err
}
