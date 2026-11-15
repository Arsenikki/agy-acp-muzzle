// Package relay moves NDJSON lines between an ACP client (stdio of this
// process) and the kernel child process, passing each line through an optional
// filter that may forward, rewrite, drop or answer it.
package relay

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"sync"
)

// Decision is what a Filter does with one line.
type Decision struct {
	// Forward is written on to the line's destination; nil drops the line.
	Forward []byte
	// Reply, if non-nil, is written back to the line's sender (e.g. an error
	// response to a request the muzzle refuses), before Forward is written.
	Reply []byte
}

// Filter inspects one line (without its trailing newline). A nil Filter
// forwards every line unchanged.
type Filter func(line []byte) Decision

// Relay connects a client and a kernel. Run returns when the kernel's output
// ends, which is when the kernel has exited or closed stdout.
type Relay struct {
	ClientIn  io.Reader      // lines from the client (our stdin)
	ClientOut io.Writer      // lines to the client (our stdout)
	KernelIn  io.WriteCloser // lines to the kernel (its stdin)
	KernelOut io.Reader      // lines from the kernel (its stdout)

	FromClient Filter // applied to client -> kernel lines
	FromKernel Filter // applied to kernel -> client lines
}

// Run relays until the kernel's output ends. When the client's input ends the
// kernel's stdin is closed so the kernel can shut down on its own.
func (r *Relay) Run() error {
	toClient := &lineWriter{w: r.ClientOut}
	toKernel := &lineWriter{w: r.KernelIn, c: r.KernelIn}

	clientDone := make(chan error, 1)
	go func() {
		err := pump(r.ClientIn, toKernel, toClient, r.FromClient)
		if cerr := toKernel.Close(); err == nil {
			err = cerr
		}
		clientDone <- err
	}()

	err := pump(r.KernelOut, toClient, toKernel, r.FromKernel)
	if err != nil {
		return err
	}
	select {
	case cerr := <-clientDone:
		return cerr
	default:
		// The client side may still be blocked reading stdin; the caller
		// exits once the kernel has gone, so it is not waited for.
		return nil
	}
}

// pump reads lines from src and applies f, writing forwarded lines to dst and
// replies to back.
func pump(src io.Reader, dst, back *lineWriter, f Filter) error {
	br := bufio.NewReader(src)
	for {
		line, readErr := br.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			d := Decision{Forward: line}
			if f != nil {
				d = f(line)
			}
			// The reply goes first: when a filter both forwards a line and
			// sends something back (e.g. a follow-up request to the kernel),
			// the sender sees the reply before the forwarded line can trigger
			// anything new from the other side.
			if d.Reply != nil {
				if err := back.WriteLine(d.Reply); err != nil {
					return err
				}
			}
			if d.Forward != nil {
				if err := dst.WriteLine(d.Forward); err != nil {
					return err
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

// lineWriter serialises whole-line writes from both pumps onto one stream and
// drops writes after Close (a late reply to an exiting kernel is not an error).
type lineWriter struct {
	mu     sync.Mutex
	w      io.Writer
	c      io.Closer
	closed bool
}

func (l *lineWriter) WriteLine(line []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	buf := make([]byte, 0, len(line)+1)
	buf = append(append(buf, line...), '\n')
	_, err := l.w.Write(buf)
	return err
}

func (l *lineWriter) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.c == nil {
		l.closed = true
		return nil
	}
	l.closed = true
	return l.c.Close()
}
