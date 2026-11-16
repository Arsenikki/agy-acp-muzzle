package main

import (
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// The kernel spawns a harness per session. When the kernel is stopped or
// dies, its children are re-parented, and a container's PID 1 often never
// reaps them, leaving zombies. As a child subreaper the muzzle inherits those
// orphans instead and reaps them; its own kernels are left to their Wait.

const (
	prSetChildSubreaper = 36

	pAll     = 0
	wExited  = 0x4
	wNoHang  = 0x1
	wNoWait  = 0x01000000
	siPidOff = 16 // offset of si_pid in siginfo_t on 64-bit Linux
)

var kernels sync.Map // pid -> struct{}: direct children reaped by exec's Wait

func registerKernel(pid int)   { kernels.Store(pid, struct{}{}) }
func unregisterKernel(pid int) { kernels.Delete(pid) }

// startReaper makes the muzzle a child subreaper and reaps orphans it
// inherits.
func startReaper(log *slog.Logger) {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		log.Debug("child subreaper unavailable", "err", errno)
		return
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGCHLD)
	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-sig:
			case <-tick.C:
			}
			reapOrphans(log)
		}
	}()
}

// reapOrphans reaps exited children that are not the muzzle's own kernels.
func reapOrphans(log *slog.Logger) {
	for {
		var info [128]byte
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, pAll, 0, uintptr(unsafe.Pointer(&info[0])), wExited|wNoHang|wNoWait, 0, 0)
		pid := int(*(*int32)(unsafe.Pointer(&info[siPidOff])))
		if errno != 0 || pid == 0 {
			return
		}
		if _, own := kernels.Load(pid); own {
			return // exec's Wait reaps it; look again on the next signal
		}
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(pid, &ws, wNoHang, nil); err != nil {
			return
		}
		log.Debug("reaped orphan", "pid", pid)
	}
}
