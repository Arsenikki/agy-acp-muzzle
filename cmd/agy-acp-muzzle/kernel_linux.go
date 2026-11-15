package main

import (
	"os/exec"
	"syscall"
)

// setParentDeathSignal kills the kernel if the muzzle dies without stopping
// it (e.g. SIGKILL from the client), so no kernel outlives its connection.
func setParentDeathSignal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
