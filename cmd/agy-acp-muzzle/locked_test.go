package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareLockedCopiesOnlyCredentials(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, consumerTokenRel), `{"refresh_token":"x"}`)
	writeFile(t, filepath.Join(src, "config", "mcp_config.json"), `{"mcpServers":{}}`)
	writeFile(t, filepath.Join(src, "antigravity-acp", "conversations", "c.pb"), "old")

	env, err := prepareLocked(t.TempDir(), src)
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(env.Home, consumerTokenRel))
	if err != nil || string(got) != `{"refresh_token":"x"}` {
		t.Fatalf("token = %q, %v", got, err)
	}
	if fi, _ := os.Stat(filepath.Join(env.Home, consumerTokenRel)); fi.Mode().Perm() != 0o600 {
		t.Errorf("token mode = %v", fi.Mode().Perm())
	}
	var files []string
	_ = filepath.Walk(env.Home, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			rel, _ := filepath.Rel(env.Home, p)
			files = append(files, rel)
		}
		return nil
	})
	if !slices.Equal(files, []string{consumerTokenRel, kernelSettingsRel}) {
		t.Errorf("home contains %v", files)
	}
	if s, _ := os.ReadFile(filepath.Join(env.Home, kernelSettingsRel)); string(s) != defaultKernelSettings {
		t.Errorf("default settings = %q", s)
	}
	entries, err := os.ReadDir(env.Workdir)
	if err != nil || len(entries) != 0 {
		t.Errorf("workdir not empty: %v %v", entries, err)
	}

	if err := env.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(env.root); !os.IsNotExist(err) {
		t.Errorf("root survived cleanup: %v", err)
	}
}

func TestPrepareLockedFailsClosedWithoutCredentials(t *testing.T) {
	base := t.TempDir()
	if _, err := prepareLocked(base, t.TempDir()); err == nil || !strings.Contains(err.Error(), "log in first") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Errorf("left %d entries behind", len(entries))
	}
}

func TestKernelEnv(t *testing.T) {
	e := &lockedEnv{root: "/t", Home: "/t/gemini"}
	got := e.KernelEnv([]string{"PATH=/bin", "HOME=/home/u", "GEMINI_HOME=/real", "AGY_ACP_FORCE_FILE_STORAGE=0", "TMPDIR=/tmp", "X=1"})
	want := []string{
		"PATH=/bin", "X=1",
		"HOME=/t", "TMPDIR=/t/tmp",
		"XDG_CACHE_HOME=/t/cache", "XDG_CONFIG_HOME=/t/config", "XDG_DATA_HOME=/t/data", "XDG_STATE_HOME=/t/state",
		"GEMINI_HOME=/t/gemini", "AGY_ACP_FORCE_FILE_STORAGE=1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTruthy(t *testing.T) {
	for _, v := range []string{"1", "true", "YES", " on "} {
		if !truthy(v) {
			t.Errorf("truthy(%q) = false", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "locked"} {
		if truthy(v) {
			t.Errorf("truthy(%q) = true", v)
		}
	}
}

func TestPrepareLockedKeepsOperatorSettings(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, consumerTokenRel), `{}`)
	writeFile(t, filepath.Join(src, kernelSettingsRel), `{"auth":{"type":"gemini-api-key"}}`)
	env, err := prepareLocked(t.TempDir(), src)
	if err != nil {
		t.Fatal(err)
	}
	defer env.Cleanup()
	if s, _ := os.ReadFile(filepath.Join(env.Home, kernelSettingsRel)); string(s) != `{"auth":{"type":"gemini-api-key"}}` {
		t.Fatalf("settings = %q", s)
	}
}
