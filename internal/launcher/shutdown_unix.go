//go:build !windows

package launcher

import (
	"errors"
	"os"
	"os/exec"
	"time"
)

const claudeShutdownGracePeriod = 4 * time.Second

func configureGracefulShutdown(command *exec.Cmd) {
	command.Cancel = func() error {
		err := command.Process.Signal(os.Interrupt)
		if errors.Is(err, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = claudeShutdownGracePeriod
}
