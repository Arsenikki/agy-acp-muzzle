// Package relay moves NDJSON lines between an ACP client (stdio of this
// process) and the kernel child process, passing each line through optional
// filters that may forward, rewrite, drop or answer it. The Supervisor owns
// the kernel's lifecycle.
package relay

import (
	"io"
	"sync"
)

// Decision is what a Filter does with one line.
type Decision struct {
	// Forward is written on to the line's destination; nil drops the line.
	Forward []byte
	// Reply, if non-nil, is written back to the line's sender (e.g. an error
	// response to a request the muzzle refuses), before Forward is written,
	// so the sender sees the reply before the forwarded line can trigger
	// anything new from the other side.
	Reply []byte
}

// Filter inspects one line (without its trailing newline). A nil Filter
// forwards every line unchanged.
type Filter func(line []byte) Decision

// lineWriter serialises whole-line writes onto one stream.
type lineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lineWriter) WriteLine(line []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	buf := make([]byte, 0, len(line)+1)
	_, err := l.w.Write(append(append(buf, line...), '\n'))
	return err
}
