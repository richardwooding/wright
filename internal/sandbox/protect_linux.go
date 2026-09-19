//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// prctl operations for the ambient capability set (linux/prctl.h).
const (
	prCapAmbient         = 47
	prCapAmbientClearAll = 4
	// capabilityVersion3 is _LINUX_CAPABILITY_VERSION_3, the 64-bit layout
	// every kernel since 2.6.26 speaks.
	capabilityVersion3 = 0x20080522
)

// capHeader and capData mirror the capset(2) structures. There is no
// x/sys dependency here on purpose: this is two syscalls.
type capHeader struct {
	version uint32
	pid     int32
}

type capData struct {
	effective   uint32
	permitted   uint32
	inheritable uint32
}

// protect makes the paths that decide what happens *outside* the sandbox
// read-only inside it, then gives up the capability that made that possible.
//
// It runs in the helper, in the mount namespace nsAttr asked for, before
// Landlock is applied and before the payload is executed. Landlock cannot
// express "this root is writable except that subtree" — its rules are
// additive — so the workspace root is granted read-write normally and these
// bind mounts carry the exception. Whether the mounts succeeded or not, the
// capability is dropped: a payload holding CAP_SYS_ADMIN could simply
// remount them read-write again.
func protect(rw []string) error {
	mountErr := mountProtected(ProtectedPaths(rw))
	if err := dropCapabilities(); err != nil {
		// Refuse rather than run the payload with CAP_SYS_ADMIN: the caller
		// executes nothing when this returns anything but
		// ErrNoMountProtection.
		return fmt.Errorf("dropping capabilities: %w", err)
	}
	return mountErr
}

// mountProtected bind-mounts each existing path read-only over itself. A
// failure is wrapped as ErrNoMountProtection so the helper knows it may
// still run the payload.
func mountProtected(paths []string) error {
	for _, p := range paths {
		if err := roBind(p); err != nil {
			return fmt.Errorf("%w: %v", ErrNoMountProtection, err)
		}
	}
	return nil
}

// roBind remounts p read-only. The bind has to happen first: MS_RDONLY is a
// property of the mount, so a fresh bind of the subtree is what there is to
// make read-only without touching the filesystem underneath it.
func roBind(p string) error {
	if err := syscall.Mount(p, p, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind %s: %w", p, err)
	}
	flags := uintptr(syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY | syscall.MS_REC)
	if err := syscall.Mount("", p, "", flags|lockedFlags(p), ""); err != nil {
		return fmt.Errorf("remount %s read-only: %w", p, err)
	}
	return nil
}

// lockedMountFlags are the per-mount flags a user namespace locks: a mount
// inherited from the parent namespace may not have them cleared, and a
// remount that omits one is asking to clear it, so it fails with EPERM. The
// ST_* bits statfs reports share their values with the MS_* bits mount
// takes, which is why they can be passed straight back.
const lockedMountFlags = syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC |
	syscall.MS_SYNCHRONOUS | syscall.MS_NOATIME | syscall.MS_NODIRATIME | syscall.MS_RELATIME

// lockedFlags reports the flags already in force on the mount holding p.
func lockedFlags(p string) uintptr {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return 0
	}
	return uintptr(st.Flags) & lockedMountFlags
}

// dropCapabilities empties the ambient, effective, permitted and inheritable
// sets of the calling thread. The caller must already hold the thread it
// will execve on (capabilities are per-thread and execve takes the calling
// thread's), and no_new_privs — which Landlock sets straight after — keeps a
// setuid or file-capability binary from handing any of it back.
func dropCapabilities() error {
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientClearAll, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("clearing ambient capabilities: %w", e)
	}
	hdr := capHeader{version: capabilityVersion3}
	var data [2]capData // all zero: nothing effective, permitted or inheritable
	if _, _, e := syscall.RawSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); e != 0 {
		return fmt.Errorf("capset: %w", e)
	}
	return nil
}

// CurrentNS identifies the calling process's network and mount namespaces.
// The backend records its own before re-exec and the helper compares: if
// they still match, the namespaces the backend asked for were not created,
// and everything that depends on them — the read-only bind mounts and the
// whole of "no network" — is silently absent. That is exactly what happened
// when a caller replaced the command's SysProcAttr instead of adding to it,
// so the helper refuses rather than running unconfined while the status bar
// says otherwise.
func CurrentNS() string {
	net, err1 := os.Readlink("/proc/self/ns/net")
	mnt, err2 := os.Readlink("/proc/self/ns/mnt")
	if err1 != nil || err2 != nil {
		return ""
	}
	return net + "," + mnt
}

// probeProtection reports whether the read-only bind mounts actually work
// in dir, by making one on a scratch directory inside it and checking that a
// write is refused. It runs as `wright __sandbox --probe`, launched with the
// same namespace attributes as the real helper, so what it measures is what
// the helper will get. The mount lives in the probe's own mount namespace
// and disappears with it.
//
// The workspace is measured rather than /tmp because the answer is a
// property of the *mount* the workspace sits on: a volume a container
// runtime bind-mounted in is locked by the user namespace that owns it, and
// a nested user namespace may not change its flags, however well the same
// call works on the container's own filesystem.
func probeProtection(dir string) error {
	if dir == "" {
		dir = os.TempDir()
	}
	scratch, err := os.MkdirTemp(dir, ".wright-mount-probe-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	if err := roBind(scratch); err != nil {
		return err
	}
	err = os.WriteFile(filepath.Join(scratch, "probe"), []byte("x"), 0o600)
	if err == nil {
		return errors.New("a read-only bind mount was still writable")
	}
	return nil
}
