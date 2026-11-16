//go:build !linux

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func setKernelProcAttr(*exec.Cmd) {}

func signalKernelGroup(pid int, sig syscall.Signal) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Signal(sig)
	}
}
