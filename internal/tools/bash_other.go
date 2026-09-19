//go:build !unix

package tools

import "os/exec"

// setProcessGroup is a no-op where process groups are unavailable; the
// exec.CommandContext default kills the direct child only.
func setProcessGroup(*exec.Cmd) {}
