// Command agy-acp-muzzle runs Google's Antigravity ACP kernel as a child
// process and relays ACP (JSON-RPC over NDJSON) between it and the client on
// stdio. By default it is a transparent pass-through; with --locked (or
// AGY_LOCKDOWN=1) the session is locked down for untrusted input (see
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
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Arsenikki/agy-acp-muzzle/internal/policy"
	"github.com/Arsenikki/agy-acp-muzzle/internal/relay"
	"github.com/Arsenikki/agy-acp-muzzle/internal/sandbox"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == sandboxExecArg {
		os.Exit(sandboxExecMain(os.Args[2:]))
	}
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("agy-acp-muzzle", flag.ContinueOnError)
	kernel := fs.String("kernel", os.Getenv("AGY_ACP_MUZZLE_KERNEL"), "path to the official agy_acp_server binary (or .par)")
	locked := fs.Bool("locked", truthy(os.Getenv("AGY_LOCKDOWN")), "locked session for untrusted input: no tools, no MCP, empty workdir (default from $AGY_LOCKDOWN)")
	tmpBase := fs.String("tmpdir", os.TempDir(), "where locked connections create their private directories")
	var sb sandboxConfig
	fs.BoolVar(&sb.disabled, "no-sandbox", false, "locked mode without the Landlock sandbox (debugging only)")
	fs.Var(&sb.exec, "sandbox-exec", "extra paths the locked kernel may execute (repeatable, comma-separated)")
	fs.Var(&sb.readOnly, "sandbox-ro", "extra paths the locked kernel may read")
	fs.Var(&sb.readWrite, "sandbox-rw", "extra paths the locked kernel may write")
	fs.Var(&sb.ports, "sandbox-port", "TCP ports the locked kernel may connect to (default 443)")
	verbose := fs.Bool("v", truthy(os.Getenv("AGY_ACP_MUZZLE_DEBUG")), "debug logging, incl. every client request's method (default from $AGY_ACP_MUZZLE_DEBUG)")
	noCache := fs.Bool("no-cache", false, "always start the kernel for initialize and session/new (no probe fast path)")
	idle := fs.Duration("idle-timeout", envDuration("AGY_ACP_MUZZLE_IDLE_TIMEOUT", 3*time.Minute), "stop the kernel after this long with nothing in flight; the next request restores it (0 disables)")
	logFile := fs.String("log-file", os.Getenv("AGY_ACP_MUZZLE_LOG_FILE"), "append logs to this file instead of stderr (clients often discard stderr)")
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
	// stdout carries the protocol, so logs never go there.
	var logOut io.Writer = os.Stderr
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, "agy-acp-muzzle: open log file:", err)
			return 2
		}
		defer f.Close()
		logOut = f
	}
	log := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: level})).With("pid", os.Getpid())
	startReaper(log)

	argv, err := kernelArgv(*kernel, fs.Args())
	if err != nil {
		log.Error("no kernel to run", "err", err)
		return 2
	}

	o := options{argv: argv, locked: *locked, tmpBase: *tmpBase, sandbox: sb, idleTimeout: *idle, cache: !*noCache, stdin: os.Stdin, stdout: os.Stdout}
	if o.srcHome, err = sourceGeminiHome(); err != nil {
		log.Error("cannot resolve the Gemini home", "err", err)
		return 1
	}
	code, err := serve(log, o)
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

// options configures one serve call.
type options struct {
	argv        []string // kernel command line
	locked      bool     // apply the locked policy
	tmpBase     string   // parent of a locked connection's private directories
	srcHome     string   // Gemini home holding the kernel credentials (locked)
	sandbox     sandboxConfig
	idleTimeout time.Duration // stop an idle kernel after this long (0: never)
	cache       bool          // answer probes from the kernel's cached answers
	stdin       io.Reader     // client -> muzzle
	stdout      io.Writer     // muzzle -> client
}

// serve starts the kernel, relays until its output ends and returns its exit
// code.
func serve(log *slog.Logger, o options) (int, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	argv := o.argv
	var kernelEnv []string // nil: inherit
	var r struct{ FromClient, FromKernel relay.Filter }
	if o.locked {
		if !o.sandbox.disabled {
			// Fail closed before touching anything.
			if _, err := sandbox.Check(); err != nil {
				return 1, fmt.Errorf("locked mode needs Landlock (--no-sandbox runs without it): %w", err)
			}
		}
		env, err := prepareLocked(o.tmpBase, o.srcHome)
		if err != nil {
			return 1, err
		}
		defer func() {
			if err := env.Cleanup(); err != nil {
				log.Warn("cleanup of locked directories failed", "err", err)
			}
		}()
		kernelEnv = env.KernelEnv(os.Environ())
		if o.sandbox.disabled {
			log.Warn("locked session without Landlock sandbox")
		} else {
			rules, err := kernelRules(o.argv[0], env.root, o.sandbox)
			if err != nil {
				return 1, err
			}
			var rulesEnv string
			if argv, rulesEnv, err = sandboxedArgv(o.argv, rules); err != nil {
				return 1, err
			}
			kernelEnv = append(kernelEnv, rulesEnv)
			log.Debug("landlock rules", "exec", rules.Exec, "readWrite", rules.ReadWrite, "connectTCP", rules.ConnectTCP)
		}
		p := policy.NewLocked(env.Workdir)
		r.FromClient, r.FromKernel = p.FromClient, p.FromKernel
		log.Debug("locked session", "workdir", env.Workdir)
	}

	start := func() (*relay.Process, error) {
		return startKernel(log, argv, filepath.Dir(o.argv[0]), kernelEnv)
	}

	// The supervisor ends when the client's input ends; a termination
	// signal ends it the same way, so the kernel is stopped cleanly.
	clientIn, closeClient := io.Pipe()
	go func() {
		_, err := io.Copy(closeClient, o.stdin)
		closeClient.CloseWithError(err)
	}()
	go func() {
		<-ctx.Done()
		closeClient.Close()
	}()

	s := &relay.Supervisor{
		ClientIn:    clientIn,
		ClientOut:   o.stdout,
		Start:       start,
		FromClient:  r.FromClient,
		FromKernel:  r.FromKernel,
		IdleTimeout: o.idleTimeout,
		Log:         log,
	}
	if o.cache {
		s.Cache, s.Sessions = probeCache(log, o)
	}
	if log.Enabled(ctx, slog.LevelDebug) {
		s.FromClient = traced(log, s.FromClient)
	}
	if err := s.Run(); err != nil {
		return 1, err
	}
	return 0, nil
}

// envDuration reads a duration from the environment, falling back to def.
func envDuration(name string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(name)); err == nil {
		return d
	}
	return def
}
