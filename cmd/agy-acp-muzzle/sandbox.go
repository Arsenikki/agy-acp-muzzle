package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Arsenikki/agy-acp-muzzle/internal/sandbox"
)

// sandboxExecArg is the hidden first argument that turns the muzzle into the
// launcher: it confines itself with the rules from sandboxRulesEnv and execs
// the kernel argv that follows "--".
const (
	sandboxExecArg  = "__sandbox-exec"
	sandboxRulesEnv = "AGY_ACP_MUZZLE_SANDBOX_RULES"
)

// systemReadOnly is what a typical dynamically linked program needs to start,
// resolve hosts and verify TLS. Missing paths are skipped. "/proc/self" is
// resolved by the launcher, which becomes the kernel process on exec, so it
// names the kernel's own /proc entry only.
var systemReadOnly = []string{
	"/usr", "/lib", "/lib64", "/lib32",
	"/etc/ssl", "/etc/ca-certificates", "/etc/pki",
	"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf", "/etc/gai.conf", "/etc/host.conf",
	"/etc/localtime", "/etc/passwd", "/etc/group", "/etc/os-release",
	"/proc/self", "/proc/cpuinfo", "/proc/meminfo", "/proc/stat", "/proc/sys",
	"/sys/devices/system/cpu", "/sys/fs/cgroup",
}

// devices the kernel may read and write.
var devices = []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom"}

// sandboxConfig is the operator's sandbox tuning.
type sandboxConfig struct {
	disabled  bool
	exec      listFlag
	readOnly  listFlag
	readWrite listFlag
	ports     listFlag
}

// kernelRules builds the Landlock rules for a locked kernel: execute only the
// kernel's own directory (and its ELF loader), read the system basics, write
// only the connection's private root, connect only to the allowed TCP ports.
func kernelRules(kernel, root string, cfg sandboxConfig) (sandbox.Rules, error) {
	kernel, err := filepath.EvalSymlinks(kernel)
	if err != nil {
		return sandbox.Rules{}, fmt.Errorf("resolve kernel path: %w", err)
	}
	r := sandbox.Rules{
		Exec:      append([]string{filepath.Dir(kernel)}, cfg.exec...),
		ReadOnly:  append(append([]string{}, systemReadOnly...), cfg.readOnly...),
		ReadWrite: append(append([]string{root}, devices...), cfg.readWrite...),
	}
	interp, err := sandbox.Interpreter(kernel)
	if err != nil {
		return sandbox.Rules{}, fmt.Errorf("read kernel ELF header: %w", err)
	}
	if interp != "" {
		r.Exec = append(r.Exec, interp)
	}
	// The kernel talks to its native harness over a websocket on a loopback
	// port the harness picks itself, so it must bind an ephemeral port and
	// connect into the ephemeral range. Landlock filters ports, not
	// addresses: fixed-port services (other than the allowed ones) stay
	// unreachable, listeners inside the ephemeral range do not.
	r.BindEphemeral = true
	r.ConnectTCPRanges = []sandbox.PortRange{ephemeralRange()}
	ports := cfg.ports
	if len(ports) == 0 {
		ports = listFlag{"443"}
	}
	for _, p := range ports {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return sandbox.Rules{}, fmt.Errorf("bad TCP port %q", p)
		}
		r.ConnectTCP = append(r.ConnectTCP, uint16(n))
	}
	return r, nil
}

// sandboxedArgv wraps the kernel argv in the launcher and returns the env entry
// carrying the rules.
func sandboxedArgv(argv []string, rules sandbox.Rules) ([]string, string, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, "", fmt.Errorf("locate own executable: %w", err)
	}
	b, err := json.Marshal(rules)
	if err != nil {
		return nil, "", err
	}
	return append([]string{self, sandboxExecArg, "--"}, argv...), sandboxRulesEnv + "=" + string(b), nil
}

// sandboxExecMain is the launcher: args are what follows sandboxExecArg.
func sandboxExecMain(args []string) int {
	if len(args) < 2 || args[0] != "--" {
		fmt.Fprintln(os.Stderr, "agy-acp-muzzle: usage: "+sandboxExecArg+" -- kernel-argv...")
		return 2
	}
	var rules sandbox.Rules
	if err := json.Unmarshal([]byte(os.Getenv(sandboxRulesEnv)), &rules); err != nil {
		fmt.Fprintln(os.Stderr, "agy-acp-muzzle: bad sandbox rules:", err)
		return 126
	}
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, sandboxRulesEnv+"=") {
			env = append(env, kv)
		}
	}
	err := sandbox.Exec(rules, args[1:], env)
	fmt.Fprintln(os.Stderr, "agy-acp-muzzle: sandboxed exec failed:", err)
	if errors.Is(err, os.ErrNotExist) {
		return 127
	}
	return 126
}

// listFlag is a repeatable, comma-separated string flag.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}

// ephemeralRange is the local port range the kernel hands out for port-0
// binds, with the Linux default as fallback.
func ephemeralRange() sandbox.PortRange {
	def := sandbox.PortRange{Lo: 32768, Hi: 60999}
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		return def
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return def
	}
	lo, err1 := strconv.ParseUint(f[0], 10, 16)
	hi, err2 := strconv.ParseUint(f[1], 10, 16)
	if err1 != nil || err2 != nil || lo == 0 || hi < lo {
		return def
	}
	return sandbox.PortRange{Lo: uint16(lo), Hi: uint16(hi)}
}
