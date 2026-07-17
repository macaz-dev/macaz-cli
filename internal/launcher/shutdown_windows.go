//go:build windows

package launcher

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

const claudeShutdownGracePeriod = 4 * time.Second

func configureGracefulShutdown(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
	}
	command.Cancel = func() error {
		err := windows.GenerateConsoleCtrlEvent(
			windows.CTRL_BREAK_EVENT,
			uint32(command.Process.Pid),
		)
		if errors.Is(err, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = claudeShutdownGracePeriod
}
