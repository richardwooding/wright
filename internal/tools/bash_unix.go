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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
