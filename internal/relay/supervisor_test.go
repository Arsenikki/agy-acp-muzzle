package relay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKernels starts in-process kernels and records what each one received.
type fakeKernels struct {
	mu       sync.Mutex
	started  int
	stopped  int
	received [][]map[string]any // per kernel instance, the messages received
}

func (f *fakeKernels) start() (*Process, error) {
	f.mu.Lock()
	f.started++
	n := len(f.received)
	f.received = append(f.received, nil)
	f.mu.Unlock()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer outW.Close()
		sc := bufio.NewScanner(inR)
		send := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(outW, "%s\n", b)
		}
		var askID any
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			f.mu.Lock()
			f.received[n] = append(f.received[n], m)
			f.mu.Unlock()
			id := m["id"]
			reply := func(result any) { send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) }
			update := func(text string) {
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
					"sessionId": "s1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"text": text}},
				}})
			}
			switch m["method"] {
			case "session/new":
				reply(map[string]any{"sessionId": "s1"})
			case "session/load":
				update("replayed history")
				reply(map[string]any{})
			case "session/prompt":
				update("hi")
				reply(map[string]any{"stopReason": "end_turn"})
			case "ask":
				// Ask the client and answer only once it replies.
				askID = id
				send(map[string]any{"jsonrpc": "2.0", "id": "k1", "method": "session/request_permission", "params": map[string]any{}})
			case "crash":
				return
			case nil:
				// a response from the client
				if m["id"] == "k1" && askID != nil {
					send(map[string]any{"jsonrpc": "2.0", "id": askID, "result": map[string]any{}})
					askID = nil
				}
			default:
				reply(map[string]any{})
			}
		}
	}()
	return &Process{Stdin: inW, Stdout: outR, Stop: func() error {
		inW.Close()
		<-done
		f.mu.Lock()
		f.stopped++
		f.mu.Unlock()
		return nil
	}}, nil
}

func (f *fakeKernels) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started, f.stopped
}

func (f *fakeKernels) methods(instance int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.received[instance] {
		if s, ok := m["method"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeKernels) message(instance int, method string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.received[instance] {
		if m["method"] == method {
			return m
		}
	}
	return nil
}

type testClient struct {
	t    *testing.T
	in   *io.PipeWriter
	out  chan string
	done chan error
}

func runSupervisor(t *testing.T, f *fakeKernels, idle time.Duration) *testClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &testClient{t: t, in: inW, out: make(chan string, 64), done: make(chan error, 1)}
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			c.out <- sc.Text()
		}
		close(c.out)
	}()
	s := &Supervisor{ClientIn: inR, ClientOut: outW, Start: f.start, IdleTimeout: idle, RestoreTimeout: 5 * time.Second}
	go func() {
		c.done <- s.Run()
		outW.Close()
	}()
	t.Cleanup(func() { inW.Close() })
	return c
}

func (c *testClient) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

func (c *testClient) next() map[string]any {
	c.t.Helper()
	select {
	case line, ok := <-c.out:
		if !ok {
			c.t.Fatal("supervisor output ended")
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			c.t.Fatalf("bad line %q", line)
		}
		return m
	case <-time.After(5 * time.Second):
		c.t.Fatal("timed out waiting for the supervisor")
	}
	return nil
}

// response reads until the response with the given id, returning it and the
// notifications seen before it.
func (c *testClient) response(id float64) (map[string]any, []map[string]any) {
	c.t.Helper()
	var notes []map[string]any
	for {
		m := c.next()
		if m["id"] == id {
			return m, notes
		}
		notes = append(notes, m)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestIdleStopAndTransparentRestore(t *testing.T) {
	f := &fakeKernels{}
	c := runSupervisor(t, f, 50*time.Millisecond)

	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	c.response(1)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/work","mcpServers":[]}}`)
	if r, _ := c.response(2); r["result"].(map[string]any)["sessionId"] != "s1" {
		t.Fatalf("session/new: %v", r)
	}
	c.send(`{"jsonrpc":"2.0","id":3,"method":"session/set_model","params":{"sessionId":"s1","modelId":"m2"}}`)
	c.response(3)
	c.send(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"s1","prompt":[]}}`)
	c.response(4)

	waitFor(t, func() bool { _, stopped := f.counts(); return stopped == 1 })

	c.send(`{"jsonrpc":"2.0","id":5,"method":"session/prompt","params":{"sessionId":"s1","prompt":[]}}`)
	resp, notes := c.response(5)
	if resp["result"] == nil {
		t.Fatalf("prompt after restore: %v", resp)
	}
	for _, n := range notes {
		if strings.Contains(fmt.Sprint(n), "replayed history") {
			t.Errorf("history replay leaked to the client: %v", n)
		}
	}
	if started, _ := f.counts(); started != 2 {
		t.Fatalf("started %d kernels, want 2", started)
	}
	want := []string{"initialize", "session/load", "session/set_model", "session/prompt"}
	if got := f.methods(1); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("restored kernel got %v, want %v", got, want)
	}
	load := f.message(1, "session/load")["params"].(map[string]any)
	if load["sessionId"] != "s1" || load["cwd"] != "/work" {
		t.Fatalf("session/load params = %v", load)
	}
}

func TestNoIdleStopWhileWaitingOnClient(t *testing.T) {
	f := &fakeKernels{}
	c := runSupervisor(t, f, 20*time.Millisecond)
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	c.response(1)
	// The kernel asks the client (like a permission request) and waits; the
	// client takes longer than the idle timeout to answer.
	c.send(`{"jsonrpc":"2.0","id":2,"method":"ask","params":{}}`)
	if req := c.next(); req["id"] != "k1" {
		t.Fatalf("want the kernel request, got %v", req)
	}
	time.Sleep(100 * time.Millisecond)
	if _, stopped := f.counts(); stopped != 0 {
		t.Fatal("kernel stopped while a request was in flight")
	}
	c.send(`{"jsonrpc":"2.0","id":"k1","result":{"outcome":{"outcome":"cancelled"}}}`)
	if r, _ := c.response(2); r["result"] == nil {
		t.Fatalf("ask: %v", r)
	}
	waitFor(t, func() bool { _, stopped := f.counts(); return stopped == 1 })
}

func TestNotificationWhileStoppedDoesNotStartKernel(t *testing.T) {
	f := &fakeKernels{}
	c := runSupervisor(t, f, 20*time.Millisecond)
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	c.response(1)
	waitFor(t, func() bool { _, stopped := f.counts(); return stopped == 1 })
	c.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s1"}}`)
	time.Sleep(50 * time.Millisecond)
	if started, _ := f.counts(); started != 1 {
		t.Fatalf("a notification restarted the kernel")
	}
}

func TestKernelCrashFailsInflightAndRecovers(t *testing.T) {
	f := &fakeKernels{}
	c := runSupervisor(t, f, 0)
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	c.response(1)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"crash","params":{}}`)
	if r, _ := c.response(2); r["error"] == nil {
		t.Fatalf("in-flight request after crash: %v", r)
	}
	c.send(`{"jsonrpc":"2.0","id":3,"method":"session/new","params":{"cwd":"/w"}}`)
	if r, _ := c.response(3); r["result"] == nil {
		t.Fatalf("request after crash: %v", r)
	}
	if got := f.methods(1); fmt.Sprint(got) != "[initialize session/new]" {
		t.Fatalf("recovered kernel got %v", got)
	}
}

func TestFiltersApply(t *testing.T) {
	f := &fakeKernels{}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &Supervisor{ClientIn: inR, ClientOut: outW, Start: f.start,
		FromClient: func(line []byte) Decision {
			if strings.Contains(string(line), "refuse") {
				return Decision{Reply: []byte(`{"jsonrpc":"2.0","id":9,"error":{"code":1,"message":"no"}}`)}
			}
			return Decision{Forward: line}
		},
	}
	go func() { _ = s.Run(); outW.Close() }()
	sc := bufio.NewScanner(outR)
	io.WriteString(inW, `{"jsonrpc":"2.0","id":9,"method":"refuse"}`+"\n")
	if !sc.Scan() || !strings.Contains(sc.Text(), `"error"`) {
		t.Fatalf("got %q", sc.Text())
	}
	if started, _ := f.counts(); started != 0 {
		t.Fatalf("a refused request started the kernel")
	}
	inW.Close()
}
