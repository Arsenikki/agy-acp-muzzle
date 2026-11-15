package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// consumerTokenRel is where the kernel keeps its consumer OAuth token, relative
// to the Gemini home.
const consumerTokenRel = "antigravity-acp/acp_token.json"

// lockedEnv is the private filesystem of one locked connection.
type lockedEnv struct {
	root    string // removed on cleanup
	Workdir string // empty working directory pinned as every session's cwd
	Home    string // GEMINI_HOME for the kernel: credentials only
}

// truthy reports whether an environment value switches a feature on.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// sourceGeminiHome is the Gemini home the operator logged in with.
func sourceGeminiHome() (string, error) {
	if h := strings.TrimSpace(os.Getenv("GEMINI_HOME")); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini"), nil
}

// prepareLocked creates the connection's private directories under tmpBase and
// copies only the kernel's credential file into the new Gemini home, so the
// kernel sees no global MCP config, skills, hooks, settings or other sessions.
func prepareLocked(tmpBase, srcHome string) (*lockedEnv, error) {
	root, err := os.MkdirTemp(tmpBase, "agy-acp-muzzle-")
	if err != nil {
		return nil, err
	}
	env := &lockedEnv{
		root:    root,
		Workdir: filepath.Join(root, "work"),
		Home:    filepath.Join(root, "gemini"),
	}
	fail := func(err error) (*lockedEnv, error) {
		_ = os.RemoveAll(root)
		return nil, err
	}
	for _, dir := range []string{env.Workdir, env.Tmp()} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return fail(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(env.Home, filepath.Dir(consumerTokenRel)), 0o700); err != nil {
		return fail(err)
	}
	src := filepath.Join(srcHome, consumerTokenRel)
	if err := copyFile(src, filepath.Join(env.Home, consumerTokenRel), 0o600); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fail(fmt.Errorf("no kernel credentials at %s: log in first", src))
		}
		return fail(err)
	}
	return env, nil
}

// Cleanup removes the connection's directories.
func (e *lockedEnv) Cleanup() error {
	return os.RemoveAll(e.root)
}

// Tmp is the kernel's private TMPDIR.
func (e *lockedEnv) Tmp() string { return filepath.Join(e.root, "tmp") }

// KernelEnv returns base with HOME, TMPDIR, the XDG directories and the Gemini
// home pointed into the connection's private root, and file credential
// storage forced (there is no keychain to fall back to in a temp home).
func (e *lockedEnv) KernelEnv(base []string) []string {
	set := []string{
		"HOME=" + e.root,
		"TMPDIR=" + e.Tmp(),
		"XDG_CACHE_HOME=" + filepath.Join(e.root, "cache"),
		"XDG_CONFIG_HOME=" + filepath.Join(e.root, "config"),
		"XDG_DATA_HOME=" + filepath.Join(e.root, "data"),
		"XDG_STATE_HOME=" + filepath.Join(e.root, "state"),
		"GEMINI_HOME=" + e.Home,
		"AGY_ACP_FORCE_FILE_STORAGE=1",
	}
	replaced := map[string]bool{}
	for _, kv := range set {
		k, _, _ := strings.Cut(kv, "=")
		replaced[k] = true
	}
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		if k, _, _ := strings.Cut(kv, "="); replaced[k] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, set...)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
