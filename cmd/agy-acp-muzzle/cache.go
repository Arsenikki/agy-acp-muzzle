package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Arsenikki/agy-acp-muzzle/internal/relay"
)

// probeCacheTTL bounds how stale a cached model list may be.
const probeCacheTTL = 24 * time.Hour

// probeCache returns the cache that answers client probes without a kernel
// and, in open mode, the store that keeps placeholder session ids. Both live
// next to the kernel's own state in the Gemini home, so they persist exactly
// as long as the conversations they point at. Locked sessions live in a
// throwaway home, so their placeholders are kept in memory only.
func probeCache(log *slog.Logger, o options) (relay.Cache, relay.SessionStore) {
	mode := "open"
	if o.locked {
		mode = "locked"
	}
	key, err := kernelKey(o.argv[0], mode)
	if err != nil {
		log.Warn("probe cache disabled", "err", err)
		return nil, nil
	}
	dir := filepath.Join(o.srcHome, "antigravity-acp", "agy-acp-muzzle")
	cache := &relay.FileCache{Path: filepath.Join(dir, mode+"-cache.json"), Key: key, TTL: probeCacheTTL}
	if o.locked {
		return cache, nil
	}
	return cache, &relay.FileSessionStore{Dir: filepath.Join(dir, "sessions")}
}

// kernelKey identifies the kernel build, so a kernel upgrade invalidates the
// cached answers.
func kernelKey(kernel, mode string) (string, error) {
	path, err := filepath.EvalSymlinks(kernel)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s|%s|%d|%d", mode, path, fi.Size(), fi.ModTime().UnixNano()), nil
}
