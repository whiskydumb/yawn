//go:build windows

package ytdlp

import (
	"os/exec"
	"syscall"
)

// configureProcess detaches the child from the console group. Otherwise
// Ctrl+C reaches yt-dlp before it reaches us, and the process dies before
// the pipeline can release its lease and save progress.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}
