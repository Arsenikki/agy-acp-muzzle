// Command acp-smoke drives an ACP agent over stdio with a scripted session:
// initialize, session/new, one session/prompt per -prompt, and prints a
// one-line summary per message plus the final assistant text of each turn.
//
// Usage:
//
//	acp-smoke [flags] -- agent-argv...
//
// Permission requests are answered per -permit (reject by default), so the
// tool can exercise both locked and open sessions.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, " | ") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

func main() { os.Exit(run()) }

func run() int {
	var prompts listFlag
	flag.Var(&prompts, "prompt", "prompt to send (repeatable, one turn each)")
	cwd := flag.String("cwd", mustGetwd(), "session cwd")
	model := flag.String("model", "", "model id to select via session/set_model (optional)")
	permit := flag.String("permit", "reject", "answer to permission requests: reject or allow")
	timeout := flag.Duration("timeout", 3*time.Minute, "overall timeout")
	loadSession := flag.String("load", "", "session/load this session id instead of session/new")
	authMethod := flag.String("authenticate", "", "call authenticate with this methodId after initialize (e.g. oauth-personal)")
	sessions := flag.Int("sessions", 1, "number of sessions to create on the one connection (each gets every prompt)")
	pause := flag.Duration("pause", 0, "wait this long before each prompt after the first")
	hold := flag.Duration("hold", 0, "keep the agent running this long after the last turn (for inspection)")
	raw := flag.Bool("raw", false, "print every raw line received")
	flag.Parse()
	argv := flag.Args()
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "usage: acp-smoke [flags] -- agent-argv...")
		return 2
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		return 1
	}
	c := &conn{in: in, pending: map[string]chan message{}, permit: *permit, raw: *raw, start: time.Now()}
	go c.read(out)
	go func() {
		time.Sleep(*timeout)
		fmt.Fprintln(os.Stderr, "acp-smoke: timeout")
		_ = cmd.Process.Kill()
		os.Exit(124)
	}()

	code := c.script(*cwd, *model, *loadSession, *authMethod, prompts, *sessions, *pause)
	if *hold > 0 {
		c.logf("holding for %s", *hold)
		time.Sleep(*hold)
	}
	_ = in.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
	}
	return code
}

func (c *conn) script(cwd, model, load, auth string, prompts []string, sessions int, pause time.Duration) int {
	if _, err := c.call("initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		"clientInfo":         map[string]string{"name": "acp-smoke", "version": "0"},
	}); err != nil {
		return c.fail("initialize", err)
	}
	if auth != "" {
		if _, err := c.call("authenticate", map[string]any{"methodId": auth}); err != nil {
			return c.fail("authenticate", err)
		}
	}
	code := 0
	for n := 0; n < max(sessions, 1); n++ {
		if c := c.session(cwd, model, load, prompts, pause); c != 0 {
			code = c
		}
	}
	return code
}

// session opens (or loads) one session and runs every prompt in it.
func (c *conn) session(cwd, model, load string, prompts []string, pause time.Duration) int {
	var sessionID string
	if load != "" {
		if _, err := c.call("session/load", map[string]any{"sessionId": load, "cwd": cwd, "mcpServers": []any{}}); err != nil {
			return c.fail("session/load", err)
		}
		sessionID = load
	} else {
		res, err := c.call("session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
		if err != nil {
			return c.fail("session/new", err)
		}
		var r struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(res, &r)
		sessionID = r.SessionID
	}
	c.logf("session %s", sessionID)
	if model != "" {
		if _, err := c.call("session/set_model", map[string]any{"sessionId": sessionID, "modelId": model}); err != nil {
			return c.fail("session/set_model", err)
		}
	}
	code := 0
	for i, p := range prompts {
		if i > 0 && pause > 0 {
			c.logf("pausing %s", pause)
			time.Sleep(pause)
		}
		c.resetText()
		res, err := c.call("session/prompt", map[string]any{
			"sessionId": sessionID,
			"prompt":    []any{map[string]string{"type": "text", "text": p}},
		})
		text := c.text()
		if err != nil {
			c.logf("TURN FAILED: %v", err)
			code = 3
		} else {
			c.logf("turn result: %s", res)
		}
		fmt.Printf("ASSISTANT: %s\n", strings.TrimSpace(text))
	}
	return code
}

type conn struct {
	in     io.Writer
	permit string
	raw    bool
	start  time.Time

	mu      sync.Mutex
	nextID  int
	pending map[string]chan message
	buf     strings.Builder
}

func (c *conn) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[%6.1fs] %s\n", time.Since(c.start).Seconds(), fmt.Sprintf(format, args...))
}

func (c *conn) fail(step string, err error) int {
	c.logf("%s failed: %v", step, err)
	return 1
}

func (c *conn) send(v any) {
	b, _ := json.Marshal(v)
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = c.in.Write(append(b, '\n'))
}

func (c *conn) call(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := fmt.Sprint(c.nextID)
	ch := make(chan message, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	c.logf("-> %s", method)
	c.send(map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params})
	m := <-ch
	if len(m.Error) > 0 {
		return nil, fmt.Errorf("%s", m.Error)
	}
	return m.Result, nil
}

func (c *conn) resetText() {
	c.mu.Lock()
	c.buf.Reset()
	c.mu.Unlock()
}

func (c *conn) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *conn) read(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			c.handle(line)
		}
		if err != nil {
			c.logf("agent output ended: %v", err)
			c.mu.Lock()
			for id, ch := range c.pending {
				ch <- message{Error: json.RawMessage(`"agent exited"`)}
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
	}
}

func (c *conn) handle(line []byte) {
	if c.raw {
		c.logf("<< %s", strings.TrimSpace(string(line)))
	}
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		c.logf("non-JSON: %s", strings.TrimSpace(string(line)))
		return
	}
	switch {
	case m.Method != "" && len(m.ID) > 0:
		c.request(m)
	case m.Method != "":
		c.notification(m)
	default:
		id := strings.Trim(string(m.ID), `"`)
		c.mu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

func (c *conn) request(m message) {
	if m.Method != "session/request_permission" {
		c.logf("<- request %s (refused)", m.Method)
		c.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "not supported"}})
		return
	}
	var p struct {
		ToolCall struct {
			Title string `json:"title"`
			Kind  string `json:"kind"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(m.Params, &p)
	want := "reject"
	if c.permit == "allow" {
		want = "allow"
	}
	outcome := map[string]string{"outcome": "cancelled"}
	for _, o := range p.Options {
		if strings.HasPrefix(o.Kind, want) {
			outcome = map[string]string{"outcome": "selected", "optionId": o.OptionID}
			break
		}
	}
	c.logf("<- permission %q (%s) -> %v", p.ToolCall.Title, p.ToolCall.Kind, outcome)
	c.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"outcome": outcome}})
}

func (c *conn) notification(m message) {
	if m.Method != "session/update" {
		c.logf("<- %s", m.Method)
		return
	}
	var p struct {
		Update map[string]json.RawMessage `json:"update"`
	}
	_ = json.Unmarshal(m.Params, &p)
	var kind string
	_ = json.Unmarshal(p.Update["sessionUpdate"], &kind)
	switch kind {
	case "agent_message_chunk":
		var content struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(p.Update["content"], &content)
		c.mu.Lock()
		c.buf.WriteString(content.Text)
		c.mu.Unlock()
	case "tool_call", "tool_call_update":
		var title, status, tkind string
		_ = json.Unmarshal(p.Update["title"], &title)
		_ = json.Unmarshal(p.Update["status"], &status)
		_ = json.Unmarshal(p.Update["kind"], &tkind)
		c.logf("<- %s %q kind=%s status=%s", kind, title, tkind, status)
	case "agent_thought_chunk":
		// noisy; skip
	default:
		c.logf("<- update %s", kind)
	}
}

func mustGetwd() string {
	wd, _ := os.Getwd()
	return wd
}
