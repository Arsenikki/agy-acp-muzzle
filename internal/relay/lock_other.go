//go:build !unix

package relay

import "time"

// LockFill is a no-op where flock is unavailable.
func (c *FileCache) LockFill(time.Duration) (func(), error) { return func() {}, nil }
