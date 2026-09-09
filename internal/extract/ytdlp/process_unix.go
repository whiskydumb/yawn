//go:build !windows

package ytdlp

import (
	"os/exec"
	"syscall"
)

// configureProcess puts the child in its own process group so Ctrl+C does
// not kill it behind the pipeline's back.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
