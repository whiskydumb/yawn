//go:build windows

package media

import (
	"os/exec"
	"syscall"
)

// configureProcess detaches ffmpeg from the console group so Ctrl+C reaches
// the pipeline first and lets it shut down cleanly.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}
