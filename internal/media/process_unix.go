//go:build !windows

package media

import (
	"os/exec"
	"syscall"
)

// configureProcess puts ffmpeg in its own process group so Ctrl+C does not
// kill it behind the pipeline's back.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
