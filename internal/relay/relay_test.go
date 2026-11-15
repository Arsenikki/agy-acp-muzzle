package relay

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
)

// fakeKernel echoes every line it receives back on its stdout, prefixed with
// "k:", and exits (closes stdout) when its stdin is closed.
type fakeKernel struct {
	inR  *io.PipeReader
	inW  *io.PipeWriter
	outR *io.PipeReader
	outW *io.PipeWriter

	mu  sync.Mutex
	got []string
}

func newFakeKernel() *fakeKernel {
	k := &fakeKernel{}
	k.inR, k.inW = io.Pipe()
	k.outR, k.outW = io.Pipe()
	go func() {
		data, _ := io.ReadAll(k.inR)
		for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if l == "" {
				continue
			}
			k.mu.Lock()
			k.got = append(k.got, l)
			k.mu.Unlock()
			_, _ = k.outW.Write([]byte("k:" + l + "\n"))
		}
		_ = k.outW.Close()
	}()
	return k
}

func (k *fakeKernel) received() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.got...)
}

func run(t *testing.T, input string, fromClient, fromKernel Filter) (string, *fakeKernel) {
	t.Helper()
	k := newFakeKernel()
	var out bytes.Buffer
	r := &Relay{
		ClientIn:   strings.NewReader(input),
		ClientOut:  &syncBuffer{b: &out},
		KernelIn:   k.inW,
		KernelOut:  k.outR,
		FromClient: fromClient,
		FromKernel: fromKernel,
	}
	if err := r.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return out.String(), k
}

func TestPassThrough(t *testing.T) {
	out, k := run(t, "a\nb\r\nc", nil, nil)
	if got := strings.Join(k.received(), ","); got != "a,b,c" {
		t.Fatalf("kernel got %q", got)
	}
	if out != "k:a\nk:b\nk:c\n" {
		t.Fatalf("client got %q", out)
	}
}

func TestFilterDropsAndRewrites(t *testing.T) {
	f := func(line []byte) Decision {
		switch string(line) {
		case "drop":
			return Decision{}
		case "rewrite":
			return Decision{Forward: []byte("rewritten")}
		}
		return Decision{Forward: line}
	}
	_, k := run(t, "keep\ndrop\nrewrite\n", f, nil)
	if got := strings.Join(k.received(), ","); got != "keep,rewritten" {
		t.Fatalf("kernel got %q", got)
	}
}

func TestFilterRepliesToSender(t *testing.T) {
	f := func(line []byte) Decision {
		if string(line) == "refuse" {
			return Decision{Reply: []byte("refused")}
		}
		return Decision{Forward: line}
	}
	out, k := run(t, "refuse\nok\n", f, nil)
	if got := strings.Join(k.received(), ","); got != "ok" {
		t.Fatalf("kernel got %q", got)
	}
	if !strings.Contains(out, "refused\n") || !strings.Contains(out, "k:ok\n") {
		t.Fatalf("client got %q", out)
	}
}

func TestKernelFilterRepliesToKernel(t *testing.T) {
	// A kernel->client line answered by the muzzle goes back to the kernel and
	// never reaches the client; replies after the kernel's stdin closed are
	// dropped without failing the relay.
	f := func(line []byte) Decision {
		if string(line) == "k:ask" {
			return Decision{Reply: []byte("answer")}
		}
		return Decision{Forward: line}
	}
	out, _ := run(t, "ask\nhello\n", nil, f)
	if strings.Contains(out, "k:ask") {
		t.Fatalf("client saw the answered line: %q", out)
	}
	if !strings.Contains(out, "k:hello\n") {
		t.Fatalf("client got %q", out)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  *bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
