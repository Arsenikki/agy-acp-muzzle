package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Arsenikki/agy-acp-muzzle/internal/policy"
	"github.com/Arsenikki/agy-acp-muzzle/internal/sandbox"
)

// fakeKernelEnv makes the test binary act as a minimal ACP kernel.
const fakeKernelEnv = "AGY_ACP_MUZZLE_FAKE_KERNEL"

// orphanEnv makes the test binary a short-lived grandchild of the muzzle.
const orphanEnv = "AGY_ACP_MUZZLE_ORPHAN"

// The fake kernel tries these from inside its sandbox (if any) and reports the
// results in its initialize response.
const (
	probeSecretEnv = "AGY_ACP_MUZZLE_PROBE_SECRET" // a file outside the allowed paths
	probeAddrEnv   = "AGY_ACP_MUZZLE_PROBE_ADDR"   // a local TCP listener
)

func TestMain(m *testing.M) {
	// The launcher must win over the fake kernel: it is exec'd with the
	// fake kernel's environment and then execs the fake kernel itself.
	if len(os.Args) > 1 && os.Args[1] == sandboxExecArg {
		os.Exit(sandboxExecMain(os.Args[2:]))
	}
	if os.Getenv(orphanEnv) == "1" {
		time.Sleep(300 * time.Millisecond) // outlive the fake kernel
		os.Exit(0)
	}
	if os.Getenv(fakeKernelEnv) == "1" {
		os.Exit(fakeKernel(os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

// probes reports, per action, "ok" or the error the fake kernel hit.
func probes() map[string]string {
	res := map[string]string{}
	report := func(name string, err error) {
		if err == nil {
			res[name] = "ok"
		} else {
			res[name] = err.Error()
		}
	}
	if p := os.Getenv(probeSecretEnv); p != "" {
		_, err := os.ReadFile(p)
		report("readSecret", err)
	}
	if a := os.Getenv(probeAddrEnv); a != "" {
		c, err := net.DialTimeout("tcp", a, 2*time.Second)
		if err == nil {
			c.Close()
		}
		report("connect", err)
	}
	report("execShell", exec.Command("/bin/sh", "-c", "true").Run())
	return res
}

// fakeKernel answers initialize and session/new by echoing what it received
// (plus its environment), and on session/prompt asks permission for a shell
// command, reports the answer it got in a session/update, then ends the turn.
func fakeKernel(in io.Reader, out io.Writer) int {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(out, "%s\n", b)
	}
	for sc.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		reply := func(result any) {
			send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		}
		switch msg.Method {
		case "initialize":
			wd, _ := os.Getwd()
			reply(map[string]any{
				"echo":       msg.Params,
				"geminiHome": os.Getenv("GEMINI_HOME"),
				"forceFile":  os.Getenv("AGY_ACP_FORCE_FILE_STORAGE"),
				"kernelCwd":  wd,
				"probes":     probes(),
			})
		case "orphan":
			// Start a child that outlives this kernel, like a harness.
			exe, _ := os.Executable()
			child := exec.Command(exe)
			child.Env = append(os.Environ(), orphanEnv+"=1")
			_ = child.Start()
			reply(map[string]any{"pid": child.Process.Pid})
		case "authenticate":
			// Like the kernel: print a link whose redirect_uri is a local
			// server, and finish once the redirect arrives with a code.
			l, _ := net.Listen("tcp", "127.0.0.1:0")
			codes := make(chan string, 1)
			go func() {
				_ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					codes <- r.URL.Query().Get("code")
				}))
			}()
			redirect := url.QueryEscape("http://localhost:" + strconv.Itoa(l.Addr().(*net.TCPAddr).Port) + "/")
			fmt.Fprintf(os.Stderr, "some log line\nOpen the following link to authenticate the ACP server: https://accounts.example.test/auth?redirect_uri=%s&scope=x\n", redirect)
			if code := <-codes; code == "good" {
				reply(map[string]any{})
			} else {
				send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32000, "message": "bad code " + code}})
			}
		case "session/new":
			reply(map[string]any{"sessionId": "s1", "modes": map[string]any{"currentModeId": "default"}, "echo": msg.Params})
		case "session/prompt":
			send(map[string]any{"jsonrpc": "2.0", "id": "perm-1", "method": "session/request_permission", "params": map[string]any{
				"sessionId": "s1",
				"toolCall":  map[string]any{"toolCallId": "t1", "title": "cat /etc/passwd", "kind": "execute"},
				"options": []any{
					map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
					map[string]any{"optionId": "deny", "name": "Deny", "kind": "reject_once"},
				},
			}})
			if !sc.Scan() {
				return 1
			}
			var answer any
			_ = json.Unmarshal(sc.Bytes(), &answer)
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": "s1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "permissionAnswer": answer},
			}})
			reply(map[string]any{"stopReason": "end_turn"})
		default:
			reply(map[string]any{})
		}
	}
	return 0
}

// client drives serve over pipes.
type client struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Scanner
	done chan int
}

func startServe(t *testing.T, o options) *client {
	t.Helper()
	t.Setenv(fakeKernelEnv, "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	o.argv = []string{exe}
	o.stdin, o.stdout = inR, outW
	c := &client{t: t, in: inW, out: bufio.NewScanner(outR), done: make(chan int, 1)}
	c.out.Buffer(make([]byte, 1<<20), 1<<20)
	go func() {
		code, err := serve(slog.New(slog.NewTextHandler(io.Discard, nil)), o)
		if err != nil {
			t.Errorf("serve: %v", err)
		}
		_ = outW.Close()
		c.done <- code
	}()
	return c
}

func (c *client) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) next() map[string]any {
	c.t.Helper()
	got := make(chan bool, 1)
	go func() { got <- c.out.Scan() }()
	select {
	case ok := <-got:
		if !ok {
			c.t.Fatalf("muzzle output ended: %v", c.out.Err())
		}
	case <-time.After(10 * time.Second):
		c.t.Fatal("timed out waiting for the muzzle")
	}
	var m map[string]any
	if err := json.Unmarshal(c.out.Bytes(), &m); err != nil {
		c.t.Fatalf("bad line %q: %v", c.out.Bytes(), err)
	}
	return m
}

func (c *client) close() int {
	c.t.Helper()
	_ = c.in.Close()
	select {
	case code := <-c.done:
		return code
	case <-time.After(10 * time.Second):
		c.t.Fatal("muzzle did not exit")
		return -1
	}
}

const (
	initLine   = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":true,"writeTextFile":true},"terminal":true}}}`
	newLine    = `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/home/user/project","mcpServers":[{"name":"m","type":"http","url":"http://m"}]}}`
	promptLine = `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"hi"}]}}`
)

func TestLockedEndToEnd(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, consumerTokenRel), `{"refresh_token":"x"}`)
	writeFile(t, filepath.Join(src, "config", "mcp_config.json"), `{}`)
	tmpBase := t.TempDir()
	setProbes(t)

	_, sbErr := sandbox.Check()
	if sbErr != nil {
		if os.Getenv("AGY_ACP_MUZZLE_REQUIRE_LANDLOCK") == "1" {
			t.Fatalf("landlock required but unavailable: %v", sbErr)
		}
		t.Logf("landlock unavailable, testing the policy without it: %v", sbErr)
	}
	c := startServe(t, options{locked: true, tmpBase: tmpBase, srcHome: src, sandbox: sandboxConfig{disabled: sbErr != nil}})

	c.send(initLine)
	init := c.next()
	t.Logf("kernel probes: %v", path(init, "result", "probes"))
	if sbErr == nil {
		for _, probe := range []string{"readSecret", "connect", "execShell"} {
			if got := path(init, "result", "probes", probe); got == "ok" || got == nil {
				t.Errorf("sandboxed kernel %s: %v", probe, got)
			}
		}
	}
	if path(init, "result", "echo", "clientCapabilities", "fs") != nil || path(init, "result", "echo", "clientCapabilities", "terminal") != nil {
		t.Errorf("client IO capabilities reached the kernel: %v", init)
	}
	home, _ := path(init, "result", "geminiHome").(string)
	if home == "" || home == src || !strings.HasPrefix(home, tmpBase) {
		t.Errorf("kernel GEMINI_HOME = %q", home)
	}
	if path(init, "result", "forceFile") != "1" {
		t.Errorf("file storage not forced: %v", init)
	}
	if _, err := os.Stat(filepath.Join(home, consumerTokenRel)); err != nil {
		t.Errorf("credentials missing in kernel home: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "config", "mcp_config.json")); !os.IsNotExist(err) {
		t.Errorf("global MCP config leaked into kernel home: %v", err)
	}

	c.send(newLine)
	sess := c.next()
	cwd, _ := path(sess, "result", "echo", "cwd").(string)
	if cwd == "/home/user/project" || !strings.HasPrefix(cwd, tmpBase) {
		t.Errorf("session cwd = %q", cwd)
	}
	if mcp, _ := path(sess, "result", "echo", "mcpServers").([]any); len(mcp) != 0 {
		t.Errorf("mcpServers reached the kernel: %v", mcp)
	}
	if tools, ok := path(sess, "result", "echo", "_meta", "antigravity", "enabledTools").([]any); !ok || len(tools) != 0 {
		t.Errorf("enabledTools = %v", path(sess, "result", "echo", "_meta"))
	}

	// The permission request never reaches the client; the kernel gets a
	// rejection and the turn fails with the denial listed.
	c.send(promptLine)
	update := c.next()
	if update["method"] != "session/update" {
		t.Fatalf("client saw %v before the turn ended", update)
	}
	if got := path(update, "params", "update", "permissionAnswer", "result", "outcome", "optionId"); got != "deny" {
		t.Errorf("kernel got permission answer %v", got)
	}
	resp := c.next()
	if path(resp, "error", "code") != float64(policy.CodeDenied) {
		t.Fatalf("prompt response = %v", resp)
	}

	if code := c.close(); code != 0 {
		t.Errorf("exit code %d", code)
	}
	if entries, _ := os.ReadDir(tmpBase); len(entries) != 0 {
		t.Errorf("locked directories left behind: %d", len(entries))
	}
}

func TestOpenEndToEnd(t *testing.T) {
	setProbes(t)
	c := startServe(t, options{})

	c.send(initLine)
	init := c.next()
	if path(init, "result", "echo", "clientCapabilities", "fs") == nil {
		t.Errorf("open mode stripped fs: %v", init)
	}
	// The same probes succeed unsandboxed, so the locked test's denials are
	// the sandbox's doing.
	for _, probe := range []string{"readSecret", "connect"} {
		if got := path(init, "result", "probes", probe); got != "ok" {
			t.Errorf("open kernel %s: %v", probe, got)
		}
	}

	c.send(newLine)
	sess := c.next()
	if path(sess, "result", "echo", "cwd") != "/home/user/project" {
		t.Errorf("open mode rewrote cwd: %v", sess)
	}

	// The permission request goes to the client, which approves it.
	c.send(promptLine)
	req := c.next()
	if req["method"] != "session/request_permission" {
		t.Fatalf("want permission request, got %v", req)
	}
	c.send(`{"jsonrpc":"2.0","id":"perm-1","result":{"outcome":{"outcome":"selected","optionId":"allow"}}}`)
	update := c.next()
	if got := path(update, "params", "update", "permissionAnswer", "result", "outcome", "optionId"); got != "allow" {
		t.Errorf("kernel got %v", got)
	}
	if resp := c.next(); path(resp, "result", "stopReason") != "end_turn" {
		t.Errorf("prompt response = %v", resp)
	}
	if code := c.close(); code != 0 {
		t.Errorf("exit code %d", code)
	}
}

func path(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[k]
	}
	return cur
}

// setProbes plants a secret outside every allowed path and starts a local TCP
// listener on a port the sandbox does not allow, for the fake kernel to try.
func setProbes(t *testing.T) {
	t.Helper()
	secret := filepath.Join(t.TempDir(), "secret")
	writeFile(t, secret, "canary")
	// A fixed port below the ephemeral range, like a real service: the locked
	// kernel may connect into the ephemeral range (for its own harness).
	var l net.Listener
	for port := 20000; port < 30000 && l == nil; port++ {
		l, _ = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	}
	if l == nil {
		t.Fatal("no free fixed port for the probe listener")
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
	t.Setenv(probeSecretEnv, secret)
	t.Setenv(probeAddrEnv, l.Addr().String())
}

func TestOrphansReaped(t *testing.T) {
	startReaper(slog.New(slog.NewTextHandler(io.Discard, nil)))
	c := startServe(t, options{})
	c.send(`{"jsonrpc":"2.0","id":1,"method":"orphan","params":{}}`)
	r := c.next()
	pid := int(path(r, "result", "pid").(float64))
	if code := c.close(); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	// The kernel is gone; its child must not linger as a zombie.
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return // reaped
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d not reaped: %s", pid, b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
