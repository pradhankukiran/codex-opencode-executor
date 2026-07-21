//go:build unix

package runner

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

const killGrace = 2 * time.Second

// configureChild puts the child in its own process group so the entire tree
// can be signalled together.
func configureChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	setPdeathsig(cmd)
}

// stopChild signals the process group and waits for waitDone, escalating to
// SIGKILL if the process has not exited after grace.
func stopChild(cmd *exec.Cmd, waitDone <-chan error, grace time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		select {
		case err := <-waitDone:
			return err
		default:
			return nil
		}
	}
	if grace <= 0 {
		grace = killGrace
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)

	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case err := <-waitDone:
		return err
	case <-timer.C:
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return <-waitDone
	}
}

// signalProcessGroup delivers sig to the child's process group.
func signalProcessGroup(cmd *exec.Cmd, sig os.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	s, ok := sig.(syscall.Signal)
	if !ok {
		return cmd.Process.Signal(sig)
	}
	return syscall.Kill(-cmd.Process.Pid, s)
}
