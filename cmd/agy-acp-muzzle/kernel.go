package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/Arsenikki/agy-acp-muzzle/internal/relay"
)

// startKernel starts one kernel process. It owns the pipe ends itself (rather
// than exec's StdinPipe/StdoutPipe) so the process can be reaped as soon as it
// exits without truncating output the relay has not read yet.
func startKernel(log *slog.Logger, argv []string, dir string, env []string) (*relay.Process, error) {
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = os.Stderr
	setParentDeathSignal(cmd)
	err = cmd.Start()
	stdinR.Close()
	stdoutW.Close()
	if err != nil {
		stdinW.Close()
		stdoutR.Close()
		return nil, fmt.Errorf("start kernel: %w", err)
	}
	log.Debug("kernel started", "kernelPid", cmd.Process.Pid, "argv0", argv[0])

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	stop := func() error {
		defer stdoutR.Close()
		stdinW.Close() // the kernel exits at EOF
		for _, step := range []struct {
			wait time.Duration
			sig  syscall.Signal
		}{{3 * time.Second, syscall.SIGTERM}, {5 * time.Second, syscall.SIGKILL}} {
			select {
			case err := <-exited:
				return err
			case <-time.After(step.wait):
				_ = cmd.Process.Signal(step.sig)
			}
		}
		return <-exited
	}
	return &relay.Process{Stdin: stdinW, Stdout: stdoutR, Stop: stop}, nil
}
