//go:build unix

package relay

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// LockFill takes the cache's fill lock (an exclusive flock on Path+".lock"),
// waiting up to timeout. While one muzzle holds it and fills an empty cache
// from a real kernel, concurrent probes wait and then answer from the cache
// instead of each starting a kernel.
func (c *FileCache) LockFill(timeout time.Duration) (func(), error) {
	if err := os.MkdirAll(dirOf(c.Path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(c.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			f.Close()
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
