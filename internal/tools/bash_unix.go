//go:build unix

package tools

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the command in its own process group and kills the
// whole group on cancellation, so a `sleep` spawned by the script does not
// outlive the tool call and keep the output pipes open.
func setProcessGroup(cmd *exec.Cmd) {
	// Add to whatever the sandbox backend already put there. Replacing the
	// struct threw away the clone flags that put the command in its own
	// network and mount namespaces, which is most of what the landlock
	// backend's confinement *is* — and it did so silently.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}
