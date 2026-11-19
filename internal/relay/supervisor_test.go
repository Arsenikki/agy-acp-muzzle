package relay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKernels starts in-process kernels and records what each one received.
type fakeKernels struct {
	mu        sync.Mutex
	started   int
	stopped   int
	received  [][]map[string]any // per kernel instance, the messages received
	initDelay time.Duration      // delay before answering initialize (a slow kernel start)
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
		sc.Buffer(make([]byte, 64<<10), 64<<20)
		var sendMu sync.Mutex
		send := func(v any) {
			b, _ := json.Marshal(v)
			sendMu.Lock()
			fmt.Fprintf(outW, "%s\n", b)
			sendMu.Unlock()
		}
		var askID any
		var slowID any // a "slow-cancel" prompt waiting for session/cancel
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
			case "initialize":
				time.Sleep(f.initDelay)
				reply(map[string]any{})
			case "session/cancel":
				// Like the real kernel, take a while, emit a late update and
				// answer the cancelled prompt with an error.
				if slowID != nil {
					pid := slowID
					slowID = nil
					go func() {
						time.Sleep(300 * time.Millisecond)
						update("late chunk of the cancelled turn")
						send(map[string]any{"jsonrpc": "2.0", "id": pid, "error": map[string]any{"code": -32603, "message": "The request was cancelled by the client."}})
					}()
				}
			case "session/new":
				reply(map[string]any{"sessionId": "s1"})
			case "session/load":
				update("replayed history")
				reply(map[string]any{})
			case "session/prompt":
				if strings.HasPrefix(promptText(m), "size:") {
					update(fmt.Sprint(len(promptText(m))))
					reply(map[string]any{"stopReason": "end_turn"})
					continue
				}
				switch promptText(m) {
				case "kernel-error":
					update("Agent execution error: model output error: boom")
					reply(map[string]any{"stopReason": "end_turn"})
				case "empty":
					reply(map[string]any{"stopReason": "end_turn"})
				case "flood":
					for i := range 200 {
						update(fmt.Sprint("chunk ", i))
					}
					reply(map[string]any{"stopReason": "end_turn"})
				case "slow-cancel":
					slowID = id
				case "self-cancel":
					send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": "Internal error", "data": map[string]any{"details": "The request was cancelled by the client."}}})
				case "hang":
					// never answers
				case "ask":
					askID = id
					send(map[string]any{"jsonrpc": "2.0", "id": "k1", "method": "session/request_permission", "params": map[string]any{"sessionId": "s1"}})
				default:
					update("hi")
					reply(map[string]any{"stopReason": "end_turn"})
				}
			case "ask":
				// Ask the client and answer only once it replies.
				askID = id
				send(map[string]any{"jsonrpc": "2.0", "id": "k1", "method": "session/request_permission", "params": map[string]any{}})
			case "crash":
				return
			case nil:
				// a response from the client
				if m["id"] == "k1" && askID != nil {
					update("done")
					send(map[string]any{"jsonrpc": "2.0", "id": askID, "result": map[string]any{"stopReason": "end_turn"}})
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

type memCache struct {
	mu        sync.Mutex
	init, new json.RawMessage
}

func (c *memCache) Load() (json.RawMessage, json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.init, c.new
}
func (c *memCache) SaveInitialize(r json.RawMessage) { c.mu.Lock(); c.init = r; c.mu.Unlock() }
func (c *memCache) SaveSessionNew(r json.RawMessage) {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(r, &m)
	delete(m, "sessionId")
	b, _ := json.Marshal(m)
	c.mu.Lock()
	c.new = b
	c.mu.Unlock()
}

type memStore struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memStore) Lookup(ph string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[ph]
	return r, ok
}
func (s *memStore) Store(ph, real string) { s.mu.Lock(); s.m[ph] = real; s.mu.Unlock() }

func warmCache() *memCache {
	return &memCache{
		init: json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}`),
		new:  json.RawMessage(`{"modes":{"currentModeId":"default"},"models":{"currentModelId":"m1"}}`),
	}
}

func runLazy(t *testing.T, f *fakeKernels, cache Cache, store SessionStore, idle time.Duration) *testClient {
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
	s := &Supervisor{ClientIn: inR, ClientOut: outW, Start: f.start, Cache: cache, Sessions: store,
		IdleTimeout: idle, RestoreTimeout: 5 * time.Second}
	go func() {
		c.done <- s.Run()
		outW.Close()
	}()
	t.Cleanup(func() { inW.Close() })
	return c
}

func placeholderOf(t *testing.T, r map[string]any) string {
	t.Helper()
	res, _ := r["result"].(map[string]any)
	ph, _ := res["sessionId"].(string)
	if ph == "" || ph == "s1" {
		t.Fatalf("want a placeholder session id, got %v", r)
	}
	return ph
}

func TestProbeAnsweredWithoutKernel(t *testing.T) {
	f := &fakeKernels{}
	c := runLazy(t, f, warmCache(), &memStore{m: map[string]string{}}, 0)
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if r, _ := c.response(1); r["result"].(map[string]any)["agentCapabilities"] == nil {
		t.Fatalf("initialize from cache: %v", r)
	}
	c.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w"}}`)
	r, _ := c.response(2)
	placeholderOf(t, r)
	if r["result"].(map[string]any)["models"] == nil {
		t.Fatalf("cached models missing: %v", r)
	}
	c.in.Close()
	<-c.done
	if started, _ := f.counts(); started != 0 {
		t.Fatalf("a probe started %d kernels", started)
	}
}

func TestLazySessionCreatedOnFirstUse(t *testing.T) {
	f := &fakeKernels{}
	store := &memStore{m: map[string]string{}}
	c := runLazy(t, f, warmCache(), store, 0)
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	c.response(1)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w","mcpServers":[]}}`)
	r, _ := c.response(2)
	ph := placeholderOf(t, r)

	c.send(`{"jsonrpc":"2.0","id":3,"method":"session/set_model","params":{"sessionId":"` + ph + `","modelId":"m2"}}`)
	if r, _ := c.response(3); r["result"] == nil {
		t.Fatalf("set_model: %v", r)
	}
	if got := f.methods(0); fmt.Sprint(got) != "[initialize session/new session/set_model]" {
		t.Fatalf("kernel got %v", got)
	}
	if p := f.message(0, "session/set_model")["params"].(map[string]any); p["sessionId"] != "s1" {
		t.Fatalf("set_model not mapped to the kernel session: %v", p)
	}
	if p := f.message(0, "session/new")["params"].(map[string]any); p["cwd"] != "/w" {
		t.Fatalf("session/new params lost: %v", p)
	}

	c.send(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"` + ph + `","prompt":[]}}`)
	_, notes := c.response(4)
	if len(notes) == 0 || notes[0]["params"].(map[string]any)["sessionId"] != ph {
		t.Fatalf("notification not mapped back to the placeholder: %v", notes)
	}
	if real, ok := store.Lookup(ph); !ok || real != "s1" {
		t.Fatalf("placeholder not persisted: %q %v", real, ok)
	}
}

func TestCacheFilledByRealKernel(t *testing.T) {
	f := &fakeKernels{}
	cache := &memCache{}
	c := runLazy(t, f, cache, nil, 0)
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	c.response(1)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w"}}`)
	if r, _ := c.response(2); r["result"].(map[string]any)["sessionId"] != "s1" {
		t.Fatalf("cold session/new should be the kernel's: %v", r)
	}
	init, sn := cache.Load()
	if init == nil || sn == nil || strings.Contains(string(sn), "sessionId") {
		t.Fatalf("cache = %s / %s", init, sn)
	}
}

func TestPlaceholderLoadAfterReconnect(t *testing.T) {
	f := &fakeKernels{}
	store := &memStore{m: map[string]string{"11111111-2222-4333-8444-555555555555": "s1"}}
	c := runLazy(t, f, warmCache(), store, 0)
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	c.response(1)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"session/load","params":{"sessionId":"11111111-2222-4333-8444-555555555555","cwd":"/w"}}`)
	r, notes := c.response(2)
	if r["result"] == nil {
		t.Fatalf("load: %v", r)
	}
	if p := f.message(0, "session/load")["params"].(map[string]any); p["sessionId"] != "s1" {
		t.Fatalf("load not mapped: %v", p)
	}
	// The client asked for this load, so it gets the history replay, under
	// its own id.
	if len(notes) == 0 || notes[0]["params"].(map[string]any)["sessionId"] != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("replay = %v", notes)
	}
}

func TestUnusedPlaceholderCannotBeLoaded(t *testing.T) {
	f := &fakeKernels{}
	c := runLazy(t, f, warmCache(), &memStore{m: map[string]string{}}, 0)
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	c.response(1)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{}}`)
	r, _ := c.response(2)
	ph := placeholderOf(t, r)
	c.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"` + ph + `"}}`)
	c.send(`{"jsonrpc":"2.0","id":3,"method":"session/load","params":{"sessionId":"` + ph + `"}}`)
	if r, _ := c.response(3); r["error"] == nil {
		t.Fatalf("load of an unused placeholder: %v", r)
	}
	if started, _ := f.counts(); started != 0 {
		t.Fatalf("started %d kernels", started)
	}
}

func TestLazySessionSurvivesIdleRestore(t *testing.T) {
	f := &fakeKernels{}
	c := runLazy(t, f, warmCache(), &memStore{m: map[string]string{}}, 50*time.Millisecond)
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	c.response(1)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w"}}`)
	r, _ := c.response(2)
	ph := placeholderOf(t, r)
	c.send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"` + ph + `","prompt":[]}}`)
	c.response(3)
	waitFor(t, func() bool { _, stopped := f.counts(); return stopped == 1 })
	c.send(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"` + ph + `","prompt":[]}}`)
	c.response(4)
	if got := f.methods(1); fmt.Sprint(got) != "[initialize session/load session/prompt]" {
		t.Fatalf("restored kernel got %v", got)
	}
	if p := f.message(1, "session/prompt")["params"].(map[string]any); p["sessionId"] != "s1" {
		t.Fatalf("prompt after restore not mapped: %v", p)
	}
}

func TestColdCacheFilledOnce(t *testing.T) {
	f := &fakeKernels{}
	cache := &FileCache{Path: filepath.Join(t.TempDir(), "cache.json"), Key: "k", TTL: time.Hour}
	c1 := runLazy(t, f, cache, nil, 0)
	c2 := runLazy(t, f, cache, nil, 0)

	c1.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	c1.response(1) // c1 holds the fill lock and started the only kernel
	c2.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	time.Sleep(100 * time.Millisecond) // c2 is now waiting on the lock

	c1.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w"}}`)
	c1.response(2) // fills the cache and releases the lock
	c2.response(1)
	c2.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w"}}`)
	r, _ := c2.response(2)
	placeholderOf(t, r)
	if started, _ := f.counts(); started != 1 {
		t.Fatalf("started %d kernels for a cold cache, want 1", started)
	}
}

func promptText(m map[string]any) string {
	params, _ := m["params"].(map[string]any)
	blocks, _ := params["prompt"].([]any)
	if len(blocks) == 0 {
		return ""
	}
	b, _ := blocks[0].(map[string]any)
	s, _ := b["text"].(string)
	return s
}

func runWith(t *testing.T, f *fakeKernels, configure func(*Supervisor)) *testClient {
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
	s := &Supervisor{ClientIn: inR, ClientOut: outW, Start: f.start, RestoreTimeout: 5 * time.Second}
	configure(s)
	go func() {
		c.done <- s.Run()
		outW.Close()
	}()
	t.Cleanup(func() { inW.Close() })
	return c
}

func prompt(id int, text string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":%q}]}}`, id, text)
}

func openSession(c *testClient) {
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	c.response(1)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w"}}`)
	c.response(2)
}

func TestTurnGuard(t *testing.T) {
	cases := []struct {
		text     string
		guard    bool
		wantFail bool
	}{
		{"hello", true, false},
		{"kernel-error", true, true},
		{"empty", true, true},
		{"kernel-error", false, false},
		{"empty", false, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/guard=%v", tc.text, tc.guard), func(t *testing.T) {
			f := &fakeKernels{}
			c := runWith(t, f, func(s *Supervisor) { s.TurnGuard = tc.guard })
			openSession(c)
			c.send(prompt(3, tc.text))
			r, _ := c.response(3)
			failed := r["error"] != nil
			if failed != tc.wantFail {
				t.Fatalf("failed=%v, want %v: %v", failed, tc.wantFail, r)
			}
			if failed && r["error"].(map[string]any)["code"] != float64(CodeTurnFailed) {
				t.Fatalf("error code: %v", r)
			}
		})
	}
}

func TestWatchdogFailsSilentTurn(t *testing.T) {
	f := &fakeKernels{}
	c := runWith(t, f, func(s *Supervisor) { s.TurnTimeout = 100 * time.Millisecond })
	openSession(c)
	start := time.Now()
	c.send(prompt(3, "hang"))
	r, _ := c.response(3)
	if r["error"] == nil {
		t.Fatalf("silent turn: %v", r)
	}
	if took := time.Since(start); took < 100*time.Millisecond {
		t.Fatalf("failed after %s, before the timeout", took)
	}
	waitFor(t, func() bool {
		return fmt.Sprint(f.methods(0)) == "[initialize session/new session/prompt session/cancel]"
	})
}

func TestWatchdogWaitsWhileKernelAsksClient(t *testing.T) {
	f := &fakeKernels{}
	c := runWith(t, f, func(s *Supervisor) { s.TurnTimeout = 50 * time.Millisecond; s.TurnGuard = true })
	openSession(c)
	c.send(prompt(3, "ask"))
	if req := c.next(); req["id"] != "k1" {
		t.Fatalf("want the permission request, got %v", req)
	}
	select {
	case line := <-c.out:
		t.Fatalf("watchdog fired while the client was deciding: %s", line)
	case <-time.After(300 * time.Millisecond):
	}
	c.send(`{"jsonrpc":"2.0","id":"k1","result":{"outcome":{"outcome":"cancelled"}}}`)
	if r, _ := c.response(3); r["result"] == nil {
		t.Fatalf("turn after the client answered: %v", r)
	}
}

func TestLargeLinesPassIntact(t *testing.T) {
	f := &fakeKernels{}
	c := runWith(t, f, func(s *Supervisor) {})
	openSession(c)
	text := "size:" + strings.Repeat("A", 12<<20) // like a base64 image
	c.send(prompt(3, text))
	r, notes := c.response(3)
	if r["result"] == nil || len(notes) == 0 {
		t.Fatalf("large prompt: %v", r)
	}
	got := notes[0]["params"].(map[string]any)["update"].(map[string]any)["content"].(map[string]any)["text"]
	if got != fmt.Sprint(len(text)) {
		t.Fatalf("kernel saw a %v-byte prompt, want %d", got, len(text))
	}
}

func TestIdleStopNotPostponedByWatchdogTicks(t *testing.T) {
	// The watchdog ticks every 10ms here; the idle timer (150ms) must still
	// fire. Re-arming it on every tick postponed idle shutdown forever.
	f := &fakeKernels{}
	c := runWith(t, f, func(s *Supervisor) {
		s.IdleTimeout = 150 * time.Millisecond
		s.TurnTimeout = 40 * time.Millisecond
	})
	openSession(c)
	waitFor(t, func() bool { _, stopped := f.counts(); return stopped == 1 })
}

func cancelLine(session string) string {
	return `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"` + session + `"}}`
}

func TestCancelAnsweredAtOnceAndNextPromptHeld(t *testing.T) {
	f := &fakeKernels{}
	c := runWith(t, f, func(s *Supervisor) { s.TurnGuard = true })
	openSession(c)
	c.send(prompt(3, "slow-cancel"))
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	c.send(cancelLine("s1"))
	r, _ := c.response(3)
	if took := time.Since(start); took > 150*time.Millisecond {
		t.Fatalf("cancel answered after %s; clients give up after ~2s and the kernel takes longer", took)
	}
	if r["result"].(map[string]any)["stopReason"] != "cancelled" {
		t.Fatalf("cancelled prompt: %v", r)
	}

	// The next prompt goes in right away (as Paseo does) and is held until
	// the kernel has finished the cancelled turn; none of that turn's late
	// output may reach the client.
	c.send(prompt(4, "hello"))
	r, notes := c.response(4)
	if r["result"] == nil {
		t.Fatalf("prompt after cancel: %v", r)
	}
	for _, n := range notes {
		if strings.Contains(fmt.Sprint(n), "late chunk") || n["id"] == float64(3) {
			t.Fatalf("output of the cancelled turn leaked: %v", n)
		}
	}
	methods := f.methods(0)
	if got := fmt.Sprint(methods[len(methods)-3:]); got != "[session/prompt session/cancel session/prompt]" {
		t.Fatalf("kernel saw %v", methods)
	}
}

func TestCancelDuringRestoreNeverReachesKernel(t *testing.T) {
	f := &fakeKernels{}
	c := runWith(t, f, func(s *Supervisor) { s.IdleTimeout = 50 * time.Millisecond })
	openSession(c)
	waitFor(t, func() bool { _, stopped := f.counts(); return stopped == 1 })

	f.mu.Lock()
	f.initDelay = 500 * time.Millisecond // the restore blocks for a while
	f.mu.Unlock()
	c.send(prompt(3, "hello"))
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	c.send(cancelLine("s1"))
	r, _ := c.response(3)
	if took := time.Since(start); took > 150*time.Millisecond {
		t.Fatalf("cancel during restore answered after %s", took)
	}
	if r["result"].(map[string]any)["stopReason"] != "cancelled" {
		t.Fatalf("cancelled prompt: %v", r)
	}
	// The connection still works, and the cancelled prompt never ran.
	c.send(prompt(4, "hello"))
	if r, _ := c.response(4); r["result"] == nil {
		t.Fatalf("prompt after cancel: %v", r)
	}
	prompts := 0
	for _, m := range f.methods(1) {
		if m == "session/prompt" {
			prompts++
		}
	}
	if prompts != 1 {
		t.Fatalf("restored kernel ran %d prompts, want only the second (%v)", prompts, f.methods(1))
	}
}

func TestKernelCancellationErrorBecomesStopReason(t *testing.T) {
	f := &fakeKernels{}
	c := runWith(t, f, func(s *Supervisor) { s.TurnGuard = true })
	openSession(c)
	c.send(prompt(3, "self-cancel"))
	r, _ := c.response(3)
	if r["error"] != nil || r["result"].(map[string]any)["stopReason"] != "cancelled" {
		t.Fatalf("kernel cancellation error not mapped: %v", r)
	}
}

func TestCancelHeldPrompt(t *testing.T) {
	f := &fakeKernels{}
	c := runWith(t, f, func(s *Supervisor) {})
	openSession(c)
	c.send(prompt(3, "slow-cancel"))
	time.Sleep(50 * time.Millisecond)
	c.send(cancelLine("s1"))
	c.response(3)
	c.send(prompt(4, "hello")) // held behind the cancelled turn
	c.send(cancelLine("s1"))   // the client cancels that one too
	r, _ := c.response(4)
	if r["result"].(map[string]any)["stopReason"] != "cancelled" {
		t.Fatalf("held prompt: %v", r)
	}
	time.Sleep(500 * time.Millisecond) // the kernel finishes the first turn
	if n := strings.Count(fmt.Sprint(f.methods(0)), "session/prompt"); n != 1 {
		t.Fatalf("cancelled held prompt reached the kernel: %v", f.methods(0))
	}
}

func TestNoDeadlockWhenBothSidesWrite(t *testing.T) {
	// The kernel floods output while the client sends a burst of requests.
	// Writing to the kernel from the main loop deadlocked here: the loop
	// blocked on the kernel's stdin while the kernel blocked on its stdout,
	// which only the loop reads.
	f := &fakeKernels{}
	c := runWith(t, f, func(s *Supervisor) {})
	openSession(c)
	c.send(prompt(3, "flood"))
	for i := range 50 {
		c.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/set_mode","params":{"sessionId":"s1","modeId":"default"}}`, 100+i))
	}
	seen := map[float64]bool{}
	for len(seen) < 51 {
		m := c.next()
		if id, ok := m["id"].(float64); ok {
			seen[id] = true
		}
	}
}
