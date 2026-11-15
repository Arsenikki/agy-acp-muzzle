//go:build linux

package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The test binary plays three roles, selected by this variable: the test
// itself, a launcher that applies the rules and execs the probe, and the
// sandboxed probe that tries things and reports the results.
const roleEnv = "AGY_ACP_MUZZLE_SANDBOX_TEST_ROLE"

func TestMain(m *testing.M) {
	switch os.Getenv(roleEnv) {
	case "launch":
		var rules Rules
		if err := json.Unmarshal([]byte(os.Getenv("SANDBOX_TEST_RULES")), &rules); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		exe, _ := os.Executable()
		// Replace (not append) the role: getenv returns the first match.
		var env []string
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, roleEnv+"=") {
				env = append(env, kv)
			}
		}
		env = append(env, roleEnv+"=probe")
		err := Exec(rules, []string{exe}, env)
		fmt.Fprintln(os.Stderr, "exec:", err)
		os.Exit(4)
	case "probe":
		os.Exit(probe())
	}
	os.Exit(m.Run())
}

// probe tries each action named in SANDBOX_TEST_ACTIONS and prints a JSON map
// of action -> "ok" or the error.
func probe() int {
	var actions []string
	_ = json.Unmarshal([]byte(os.Getenv("SANDBOX_TEST_ACTIONS")), &actions)
	results := map[string]string{}
	for _, a := range actions {
		results[a] = outcome(runAction(a))
	}
	_ = json.NewEncoder(os.Stdout).Encode(results)
	return 0
}

func outcome(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

func runAction(a string) error {
	kind, arg, _ := cut(a)
	switch kind {
	case "read":
		_, err := os.ReadFile(arg)
		return err
	case "write":
		return os.WriteFile(arg, []byte("x"), 0o600)
	case "exec":
		return exec.Command(arg).Run()
	case "connect":
		c, err := net.Dial("tcp", arg)
		if err == nil {
			c.Close()
		}
		return err
	case "listen":
		l, err := net.Listen("tcp", arg)
		if err == nil {
			l.Close()
		}
		return err
	}
	return fmt.Errorf("unknown action %q", a)
}

func cut(s string) (string, string, bool) {
	for i := range len(s) {
		if s[i] == ':' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func listen(t *testing.T) (string, uint16) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	p, _ := strconv.Atoi(port)
	return l.Addr().String(), uint16(p)
}

// requireLandlock skips (or, with AGY_ACP_MUZZLE_REQUIRE_LANDLOCK=1, fails)
// when the running kernel cannot enforce Rules, and returns its ABI.
func requireLandlock(t *testing.T) int {
	t.Helper()
	abi, err := Check()
	if err != nil {
		if os.Getenv("AGY_ACP_MUZZLE_REQUIRE_LANDLOCK") == "1" {
			t.Fatalf("landlock required but unavailable: %v", err)
		}
		t.Skipf("landlock unavailable: %v", err)
	}
	return abi
}

// probeExe returns the test binary and rules granting what it needs to run.
func probeExe(t *testing.T) (string, Rules) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, _ = filepath.EvalSymlinks(exe)
	rules := Rules{Exec: []string{exe}, ReadOnly: []string{"/proc/self", "/etc"}, ReadWrite: []string{"/dev/null"}}
	if interp, err := Interpreter(exe); err != nil {
		t.Fatal(err)
	} else if interp != "" {
		// A cgo test binary (e.g. -race) is dynamic: grant its loader and libs.
		rules.Exec = append(rules.Exec, interp)
		rules.ReadOnly = append(rules.ReadOnly, "/lib", "/lib64", "/usr/lib")
	}
	return exe, rules
}

// runProbes launches the sandboxed probe and returns action -> outcome.
func runProbes(t *testing.T, exe string, rules Rules, actions []string) map[string]string {
	t.Helper()
	rulesJSON, _ := json.Marshal(rules)
	actionsJSON, _ := json.Marshal(actions)
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), roleEnv+"=launch", "SANDBOX_TEST_RULES="+string(rulesJSON), "SANDBOX_TEST_ACTIONS="+string(actionsJSON))
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("launcher failed: %v\n%s", err, ee.Stderr)
		}
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("probe output %q: %v", out, err)
	}
	return got
}

func check(t *testing.T, got map[string]string, want map[string]bool) {
	t.Helper()
	for action, allowed := range want {
		res := got[action]
		if allowed && res != "ok" {
			t.Errorf("%s: want allowed, got %s", action, res)
		}
		if !allowed && res == "ok" {
			t.Errorf("%s: want denied, but it succeeded", action)
		}
	}
}

func TestExecConfines(t *testing.T) {
	abi := requireLandlock(t)
	exe, rules := probeExe(t)

	ro := t.TempDir()
	rw := t.TempDir()
	outside := t.TempDir()
	for _, p := range []string{filepath.Join(ro, "allowed"), filepath.Join(outside, "secret")} {
		if err := os.WriteFile(p, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	allowedAddr, allowedPort := listen(t)
	deniedAddr, _ := listen(t)
	rangeAddr, rangePort := listen(t)

	rules.ReadOnly = append(rules.ReadOnly, ro)
	rules.ReadWrite = append(rules.ReadWrite, rw)
	rules.ConnectTCP = []uint16{allowedPort}
	rules.ConnectTCPRanges = []PortRange{{Lo: rangePort, Hi: rangePort}}
	rules.BindEphemeral = true

	want := map[string]bool{ // action -> allowed
		"read:" + filepath.Join(ro, "allowed"):     true,
		"read:" + filepath.Join(outside, "secret"): false,
		"write:" + filepath.Join(rw, "new"):        true,
		"write:" + filepath.Join(ro, "new"):        false,
		"write:" + filepath.Join(outside, "new"):   false,
		"exec:/bin/sh":                             false,
		"exec:/usr/bin/env":                        false,
		"connect:" + allowedAddr:                   true,
		"connect:" + deniedAddr:                    false,
		"connect:" + rangeAddr:                     true,
		"listen:127.0.0.1:0":                       true,
	}
	const fixedBind = "listen:127.0.0.1:18081"
	actions := []string{fixedBind}
	for a := range want {
		actions = append(actions, a)
	}
	got := runProbes(t, exe, rules, actions)
	check(t, got, want)
	if got[fixedBind] == "ok" {
		// How a port-0 bind rule treats fixed ports differs between kernel
		// versions (some allow any bind). Binding is not exfiltration, so
		// this is recorded, not failed; TestBindDeniedByDefault holds
		// everywhere.
		t.Logf("landlock ABI %d: port-0 bind rule also allowed a fixed-port bind", abi)
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Errorf("a file was written outside the allowed paths")
	}
}

func TestBindDeniedByDefault(t *testing.T) {
	requireLandlock(t)
	exe, rules := probeExe(t)
	check(t, runProbes(t, exe, rules, []string{"listen:127.0.0.1:0", "listen:127.0.0.1:18082"}), map[string]bool{
		"listen:127.0.0.1:0":     false,
		"listen:127.0.0.1:18082": false,
	})
}

func TestInterpreter(t *testing.T) {
	if interp, err := Interpreter("/bin/sh"); err != nil || interp == "" {
		t.Skipf("no dynamic /bin/sh here (%q, %v)", interp, err)
	}
	script := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if interp, err := Interpreter(script); err != nil || interp != "" {
		t.Fatalf("script: %q, %v", interp, err)
	}
}
