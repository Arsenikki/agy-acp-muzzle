//go:build !linux

package sandbox

// ABI reports that Landlock is unavailable outside Linux.
func ABI() (int, error) { return 0, ErrUnsupported }

// Exec always fails outside Linux.
func Exec(Rules, []string, []string) error { return ErrUnsupported }
