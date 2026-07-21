//go:build unix && !linux

package runner

import "os/exec"

func setPdeathsig(_ *exec.Cmd) {}
