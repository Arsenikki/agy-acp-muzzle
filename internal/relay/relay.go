// Package relay moves NDJSON lines between an ACP client (stdio of this
// process) and the kernel child process, passing each line through optional
// filters that may forward, rewrite, drop or answer it. The Supervisor owns
// the kernel's lifecycle.
package relay

import (
	"io"
	"time"
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

// writeQueue is how many lines may wait for a slow reader before the writer
// blocks.
const writeQueue = 4096

// lineWriter queues whole lines and writes them in order from its own
// goroutine, so the caller never blocks on a peer that is itself blocked
// writing to us (a deadlock both directions of the relay can otherwise hit).
type lineWriter struct {
	ch   chan []byte
	done chan struct{}
}

func newLineWriter(w io.Writer) *lineWriter {
	l := &lineWriter{ch: make(chan []byte, writeQueue), done: make(chan struct{})}
	go func() {
		defer close(l.done)
		for line := range l.ch {
			_, _ = w.Write(line) // a vanished reader is noticed on its input
		}
	}()
	return l
}

// WriteLine queues line followed by a newline.
func (l *lineWriter) WriteLine(line []byte) error {
	buf := make([]byte, 0, len(line)+1)
	l.ch <- append(append(buf, line...), '\n')
	return nil
}

// Close flushes queued lines (waiting up to timeout) and stops the writer.
func (l *lineWriter) Close(timeout time.Duration) {
	close(l.ch)
	select {
	case <-l.done:
	case <-time.After(timeout):
	}
}
