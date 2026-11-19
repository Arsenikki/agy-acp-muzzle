package main

import (
	"bufio"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoginHeadlessRoundTrip(t *testing.T) {
	t.Setenv(fakeKernelEnv, "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- runLogin(slog.New(slog.NewTextHandler(io.Discard, nil)), []string{exe}, home, inR, outW)
		outW.Close()
	}()

	// Find the link the user is asked to open, and play the browser: Google
	// would redirect to its redirect_uri with a code.
	var redirect string
	sc := bufio.NewScanner(outR)
	for redirect == "" && sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "https://") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil {
			t.Fatal(err)
		}
		redirect = u.Query().Get("redirect_uri")
	}
	if redirect == "" {
		t.Fatal("no authorization link shown")
	}
	go func() { _, _ = io.Copy(io.Discard, outR) }()
	if _, err := io.WriteString(inW, redirect+"?code=good\n"); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("login: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("login did not finish")
	}
	b, err := os.ReadFile(filepath.Join(home, kernelSettingsRel))
	if err != nil || string(b) != defaultKernelSettings {
		t.Fatalf("settings = %q, %v", b, err)
	}
}

func TestDeliverCallbackOnlyLoopback(t *testing.T) {
	for _, bad := range []string{
		"https://localhost:1234/?code=x",
		"http://example.com/?code=x",
		"http://10.0.0.1:80/",
		"file:///etc/passwd",
		"not a url",
	} {
		if err := deliverCallback(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestEnsureAuthSettingsKeepsExisting(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, kernelSettingsRel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"auth":{"type":"gemini-api-key"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureAuthSettings(home); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != `{"auth":{"type":"gemini-api-key"}}` {
		t.Fatalf("existing settings overwritten: %s", b)
	}
}
