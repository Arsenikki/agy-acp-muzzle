package main

import (
	"os/exec"
	"syscall"
)

// setKernelProcAttr puts the kernel in its own process group, so stopping it
// can signal its harness too, and kills it if the muzzle dies without
// stopping it (e.g. SIGKILL from the client).
func setKernelProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
}

// signalKernelGroup signals the kernel's whole process group.
func signalKernelGroup(pid int, sig syscall.Signal) {
	_ = syscall.Kill(-pid, sig)
}
