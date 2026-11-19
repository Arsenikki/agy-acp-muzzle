package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Arsenikki/agy-acp-muzzle/internal/acp"
)

// loginTimeout bounds the whole interactive login.
const loginTimeout = 10 * time.Minute

// authURL matches the kernel's authorization link on its stderr.
var authURL = regexp.MustCompile(`https://\S+`)

// runLogin performs the kernel's consumer OAuth login (ACP authenticate with
// methodId oauth-personal) on a headless machine. The kernel prints an
// authorization link and waits for Google's redirect on a local HTTP server.
// The user opens the link elsewhere, and pastes the localhost address the
// browser ends up on (it fails to load there); runLogin delivers it to the
// kernel's server. Afterwards the auth method is recorded in settings.json,
// which clients that never call authenticate (such as Paseo) need.
func runLogin(log *slog.Logger, argv []string, srcHome string, in io.Reader, out io.Writer) error {
	stderrR, stderrW := io.Pipe()
	env := append(os.Environ(), "PYTHONUNBUFFERED=1")
	proc, err := startKernel(log, argv, filepath.Dir(argv[0]), env, stderrW)
	if err != nil {
		return err
	}
	defer func() {
		_ = proc.Stop()
		stderrW.Close()
	}()

	// The kernel prints the link on stderr; prompt for the redirect once.
	links := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderrR)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		sent := false
		for sc.Scan() {
			if m := authURL.FindString(sc.Text()); m != "" && !sent && strings.Contains(m, "redirect_uri") {
				links <- m
				sent = true
			}
		}
	}()

	c := &loginClient{in: proc.Stdin, out: bufio.NewReader(proc.Stdout)}
	if _, err := c.call(1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.call(2, "authenticate", map[string]any{"methodId": "oauth-personal"})
		done <- err
	}()

	deadline := time.After(loginTimeout)
	for {
		select {
		case link := <-links:
			fmt.Fprintf(out, "\n1. Open this link in a browser and sign in:\n\n   %s\n\n"+
				"2. The browser then lands on a page that does not load (http://localhost:...).\n"+
				"   Copy that whole address and paste it here: ", link)
			callback, err := bufio.NewReader(in).ReadString('\n')
			if err != nil && callback == "" {
				return fmt.Errorf("reading the redirect address: %w", err)
			}
			if err := deliverCallback(strings.TrimSpace(callback)); err != nil {
				return err
			}
			fmt.Fprintln(out, "\nWaiting for the kernel to finish the login...")
		case err := <-done:
			if err != nil {
				return fmt.Errorf("authenticate: %w", err)
			}
			if err := ensureAuthSettings(srcHome); err != nil {
				return err
			}
			fmt.Fprintf(out, "Logged in. Credentials: %s\n", filepath.Join(srcHome, consumerTokenRel))
			return nil
		case <-deadline:
			return errors.New("login timed out")
		}
	}
}

// deliverCallback hands the OAuth redirect to the kernel's local server. Only
// loopback addresses are accepted, so a pasted value can't make the muzzle
// fetch anything else.
func deliverCallback(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return fmt.Errorf("not an http://localhost address: %q", raw)
	}
	switch host := u.Hostname(); host {
	case "localhost":
	default:
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("not a localhost address: %q", raw)
		}
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(u.String())
	if err != nil {
		return fmt.Errorf("delivering the redirect to the kernel: %w", err)
	}
	resp.Body.Close()
	return nil
}

// ensureAuthSettings records oauth-personal as the kernel's auth method unless
// a settings file already exists.
func ensureAuthSettings(home string) error {
	path := filepath.Join(home, kernelSettingsRel)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(defaultKernelSettings), 0o600)
}

// loginClient is a minimal synchronous JSON-RPC client over the kernel's stdio.
type loginClient struct {
	in  io.Writer
	out *bufio.Reader
}

func (c *loginClient) call(id int, method string, params any) (json.RawMessage, error) {
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if _, err := c.in.Write(append(req, '\n')); err != nil {
		return nil, err
	}
	want := fmt.Sprint(id)
	for {
		line, err := c.out.ReadBytes('\n')
		if len(line) > 0 {
			if env, perr := acp.Parse(line); perr == nil && env.Kind() == acp.KindResponse && string(env.ID) == want {
				if len(env.Error) > 0 {
					return nil, fmt.Errorf("%s", env.Error)
				}
				return env.Result, nil
			}
		}
		if err != nil {
			return nil, fmt.Errorf("kernel output ended: %w", err)
		}
	}
}
