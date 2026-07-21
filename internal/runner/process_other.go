//go:build !unix

package runner

import (
	"os"
	"os/exec"
	"time"
)

const killGrace = 2 * time.Second

func configureChild(cmd *exec.Cmd) {}

func stopChild(cmd *exec.Cmd, waitDone <-chan error, grace time.Duration) error {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if grace <= 0 {
		grace = killGrace
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-waitDone:
		return err
	case <-timer.C:
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return <-waitDone
	}
}

func signalProcessGroup(cmd *exec.Cmd, sig os.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Signal(sig)
}
