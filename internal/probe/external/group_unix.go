//go:build unix

package external

import (
	"os/exec"
	"syscall"
)

// ownGroup puts the command in a process group of its own and makes
// cancellation kill the group.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
