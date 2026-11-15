// Command agy-acp-muzzle runs Google's Antigravity ACP kernel as a child
// process and relays ACP (JSON-RPC over NDJSON) between it and the client on
// stdio. This first version is a transparent pass-through; per-session
// lockdown and sandboxing are layered onto the relay filters (see
// docs/DESIGN.md).
//
// Usage:
//
//	agy-acp-muzzle [flags] [-- kernel-argv...]
//
// Without a kernel argv the kernel path comes from --kernel or
// $AGY_ACP_MUZZLE_KERNEL.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Arsenikki/agy-acp-muzzle/internal/relay"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("agy-acp-muzzle", flag.ContinueOnError)
	kernel := fs.String("kernel", os.Getenv("AGY_ACP_MUZZLE_KERNEL"), "path to the official agy_acp_server binary (or .par)")
	verbose := fs.Bool("v", false, "debug logging on stderr")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println(version)
		return 0
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	// stdout carries the protocol, so logs go to stderr only.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	argv, err := kernelArgv(*kernel, fs.Args())
	if err != nil {
		log.Error("no kernel to run", "err", err)
		return 2
	}

	code, err := serve(log, argv)
	if err != nil {
		log.Error("relay failed", "err", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// kernelArgv resolves the kernel command line. An explicit argv after "--"
// wins; otherwise the kernel path is used, with the "--uid=" flag the official
// linux .par build expects.
func kernelArgv(kernel string, rest []string) ([]string, error) {
	if len(rest) > 0 {
		return rest, nil
	}
	kernel = strings.TrimSpace(kernel)
	if kernel == "" {
		return nil, errors.New("set --kernel or AGY_ACP_MUZZLE_KERNEL, or pass the kernel argv after --")
	}
	if strings.HasSuffix(filepath.Base(kernel), ".par") {
		return []string{kernel, "--uid="}, nil
	}
	return []string{kernel}, nil
}

// serve starts the kernel, relays until its output ends and returns its exit
// code.
func serve(log *slog.Logger, argv []string) (int, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = filepath.Dir(argv[0])
	cmd.Stderr = os.Stderr
	kin, err := cmd.StdinPipe()
	if err != nil {
		return 1, err
	}
	kout, err := cmd.StdoutPipe()
	if err != nil {
		return 1, err
	}
	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("start kernel: %w", err)
	}
	log.Debug("kernel started", "pid", cmd.Process.Pid, "argv0", argv[0])

	// Forward a termination signal to the kernel and let it exit on its own.
	go func() {
		<-ctx.Done()
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	}()

	r := &relay.Relay{
		ClientIn:  os.Stdin,
		ClientOut: os.Stdout,
		KernelIn:  kin,
		KernelOut: kout,
	}
	relayErr := r.Run()
	waitErr := cmd.Wait()

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		return 0, relayErr
	case errors.As(waitErr, &exitErr):
		return exitErr.ExitCode(), relayErr
	default:
		return 1, errors.Join(relayErr, waitErr)
	}
}
