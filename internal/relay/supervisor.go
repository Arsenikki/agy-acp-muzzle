package relay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/Arsenikki/agy-acp-muzzle/internal/acp"
)

// Process is one running kernel.
type Process struct {
	Stdin  io.WriteCloser
	Stdout io.Reader
	// Stop terminates the kernel and waits for it to exit.
	Stop func() error
}

// Starter starts a kernel.
type Starter func() (*Process, error)

// svIDPrefix marks requests the supervisor sends while restoring a kernel.
// It extends the policy's prefix, so a policy also never forwards them.
const svIDPrefix = "agy-acp-muzzle-sv-"

// Supervisor relays one client connection to a kernel it starts on demand.
// It stops the kernel after IdleTimeout with nothing in flight and, on the
// next client request, restarts it and restores the connection: initialize,
// session/load for every session the client opened, and the client's mode
// and model settings. The client sees one continuous connection.
type Supervisor struct {
	ClientIn  io.Reader
	ClientOut io.Writer
	Start     Starter

	FromClient Filter // applied to client -> kernel lines
	FromKernel Filter // applied to kernel -> client lines

	// IdleTimeout stops an idle kernel; zero keeps it running.
	IdleTimeout time.Duration
	// RestoreTimeout bounds each request of a restore (default 2 minutes).
	RestoreTimeout time.Duration
	Log            *slog.Logger

	toClient *lineWriter
	k        *kernelConn
	seq      int

	// What the client set up, as forwarded to the kernel (after policy).
	init     []byte
	sessions map[string]*sessionRec
	order    []string // session ids in creation order
	pending  map[string]pendingSession

	inflightClient map[string]string // client request id -> method
	inflightKernel map[string]bool   // kernel request ids awaiting the client
}

type sessionRec struct {
	params   json.RawMessage // session/new|load|resume params (without sessionId for new)
	settings [][]byte        // forwarded set_mode / set_model / set_config_option lines
}

type pendingSession struct {
	method string
	params json.RawMessage
}

type kernelConn struct {
	proc  *Process
	lines chan []byte // closed when the kernel's output ends
	done  chan struct{}
}

// Run serves the client until its input ends.
func (s *Supervisor) Run() error {
	if s.Log == nil {
		s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.RestoreTimeout == 0 {
		s.RestoreTimeout = 2 * time.Minute
	}
	s.toClient = &lineWriter{w: s.ClientOut}
	s.sessions = map[string]*sessionRec{}
	s.pending = map[string]pendingSession{}
	s.inflightClient = map[string]string{}
	s.inflightKernel = map[string]bool{}

	clientCh := make(chan []byte)
	go readLines(s.ClientIn, clientCh, nil)

	idle := time.NewTimer(time.Hour)
	idle.Stop()
	defer s.stopKernel("client gone")

	for {
		var kernelLines chan []byte
		if s.k != nil {
			kernelLines = s.k.lines
		}
		select {
		case line, ok := <-clientCh:
			if !ok {
				return nil
			}
			if err := s.fromClient(line); err != nil {
				return err
			}
		case line, ok := <-kernelLines:
			if !ok {
				s.kernelExited()
				break
			}
			if err := s.fromKernel(line); err != nil {
				return err
			}
		case <-idle.C:
			if s.idle() {
				s.stopKernel("idle")
			}
		}
		s.armIdle(idle)
	}
}

func (s *Supervisor) idle() bool {
	return s.k != nil && len(s.inflightClient) == 0 && len(s.inflightKernel) == 0
}

func (s *Supervisor) armIdle(t *time.Timer) {
	t.Stop()
	if s.IdleTimeout > 0 && s.idle() {
		t.Reset(s.IdleTimeout)
	}
}

func (s *Supervisor) fromClient(line []byte) error {
	d := Decision{Forward: line}
	if s.FromClient != nil {
		d = s.FromClient(line)
	}
	if d.Reply != nil {
		if err := s.toClient.WriteLine(d.Reply); err != nil {
			return err
		}
	}
	if d.Forward == nil {
		return nil
	}
	env, err := acp.Parse(d.Forward)
	if err != nil {
		return s.toKernel(d.Forward)
	}
	id := string(env.ID)
	switch env.Kind() {
	case acp.KindResponse:
		delete(s.inflightKernel, id)
		if s.k == nil {
			return nil // answers a kernel that has since been stopped
		}
		return s.toKernel(d.Forward)
	case acp.KindNotification:
		if s.k == nil {
			return nil // e.g. session/cancel: nothing is running
		}
		return s.toKernel(d.Forward)
	case acp.KindRequest:
	default:
		if s.k == nil {
			return nil
		}
		return s.toKernel(d.Forward)
	}

	if s.k == nil {
		if err := s.startKernel(env.Method != "initialize"); err != nil {
			s.Log.Error("kernel unavailable", "err", err)
			return s.replyError(env.ID, acp.CodeInternalError, "agy-acp-muzzle: kernel unavailable: "+err.Error())
		}
	}
	s.track(env, d.Forward)
	s.inflightClient[id] = env.Method
	return s.toKernel(d.Forward)
}

// track records what a restore must replay.
func (s *Supervisor) track(env *acp.Envelope, forwarded []byte) {
	switch env.Method {
	case "initialize":
		s.init = forwarded
	case "session/new", "session/load", "session/resume":
		s.pending[string(env.ID)] = pendingSession{method: env.Method, params: env.Params}
	case "session/set_mode", "session/set_model", "session/set_config_option":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(env.Params, &p)
		if rec := s.sessions[p.SessionID]; rec != nil {
			rec.settings = append(rec.settings, forwarded)
		}
	}
}

func (s *Supervisor) fromKernel(line []byte) error {
	env, err := acp.Parse(line)
	if err == nil && env.Kind() == acp.KindResponse {
		id := string(env.ID)
		if strings.HasPrefix(id, `"`+svIDPrefix) {
			return nil // late answer to a timed-out restore request
		}
		delete(s.inflightClient, id)
		if p, ok := s.pending[id]; ok {
			delete(s.pending, id)
			if len(env.Result) > 0 {
				s.rememberSession(p, env.Result)
			}
		}
	}

	d := Decision{Forward: line}
	if s.FromKernel != nil {
		d = s.FromKernel(line)
	}
	if d.Reply != nil {
		if err := s.toKernel(d.Reply); err != nil {
			return err
		}
	}
	if d.Forward == nil {
		return nil
	}
	if fe, err := acp.Parse(d.Forward); err == nil && fe.Kind() == acp.KindRequest {
		s.inflightKernel[string(fe.ID)] = true
	}
	return s.toClient.WriteLine(d.Forward)
}

func (s *Supervisor) rememberSession(p pendingSession, result json.RawMessage) {
	var params map[string]json.RawMessage
	if json.Unmarshal(p.params, &params) != nil {
		params = map[string]json.RawMessage{}
	}
	var sid string
	if raw, ok := params["sessionId"]; ok {
		_ = json.Unmarshal(raw, &sid)
	}
	if sid == "" {
		var r struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(result, &r)
		sid = r.SessionID
	}
	if sid == "" {
		return
	}
	delete(params, "sessionId")
	raw, _ := json.Marshal(params)
	if _, seen := s.sessions[sid]; !seen {
		s.order = append(s.order, sid)
	}
	s.sessions[sid] = &sessionRec{params: raw}
}

// startKernel starts a kernel and, if restore is set and the client had
// initialized, replays the connection state into it.
func (s *Supervisor) startKernel(restore bool) error {
	proc, err := s.Start()
	if err != nil {
		return err
	}
	k := &kernelConn{proc: proc, lines: make(chan []byte), done: make(chan struct{})}
	go readLines(proc.Stdout, k.lines, k.done)
	s.k = k
	if !restore || s.init == nil {
		return nil
	}
	started := time.Now()
	if err := s.restore(); err != nil {
		s.stopKernel("restore failed")
		return err
	}
	s.Log.Info("kernel restored", "sessions", len(s.sessions), "took", time.Since(started).Round(time.Millisecond))
	return nil
}

func (s *Supervisor) restore() error {
	if _, err := s.call(s.init); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	for _, sid := range s.order {
		rec := s.sessions[sid]
		var params map[string]json.RawMessage
		if json.Unmarshal(rec.params, &params) != nil {
			params = map[string]json.RawMessage{}
		}
		params["sessionId"], _ = json.Marshal(sid)
		req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 0, "method": "session/load", "params": params})
		if _, err := s.call(req); err != nil {
			s.Log.Warn("session not restored", "session", sid, "err", err)
			continue
		}
		for _, setting := range rec.settings {
			if _, err := s.call(setting); err != nil {
				s.Log.Warn("setting not restored", "session", sid, "err", err)
			}
		}
	}
	return nil
}

// call sends req (its id replaced by a supervisor id) to the kernel and waits
// for the answer. Notifications meanwhile are dropped: during session/load
// they are the history replay the client already has. Kernel requests
// meanwhile are refused.
func (s *Supervisor) call(req []byte) (json.RawMessage, error) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(req, &msg); err != nil {
		return nil, err
	}
	s.seq++
	id := fmt.Sprintf("%s%d", svIDPrefix, s.seq)
	msg["id"], _ = json.Marshal(id)
	out, _ := json.Marshal(msg)
	if err := s.toKernel(out); err != nil {
		return nil, err
	}
	want, _ := json.Marshal(id)
	deadline := time.After(s.RestoreTimeout)
	for {
		select {
		case line, ok := <-s.k.lines:
			if !ok {
				return nil, errors.New("kernel exited")
			}
			env, err := acp.Parse(line)
			if err != nil {
				continue
			}
			switch env.Kind() {
			case acp.KindResponse:
				if !bytes.Equal(env.ID, want) {
					continue
				}
				if len(env.Error) > 0 {
					return nil, fmt.Errorf("%s", env.Error)
				}
				return env.Result, nil
			case acp.KindRequest:
				reply, _ := acp.ErrorResponse(env.ID, acp.RPCError{Code: acp.CodeInternalError, Message: "agy-acp-muzzle: restoring"})
				_ = s.toKernel(reply)
			}
		case <-deadline:
			return nil, errors.New("timed out")
		}
	}
}

// kernelExited handles a kernel that stopped on its own: every request still
// waiting for it gets an error instead of hanging.
func (s *Supervisor) kernelExited() {
	s.Log.Warn("kernel exited", "inflight", len(s.inflightClient))
	for id, method := range s.inflightClient {
		_ = s.replyError(json.RawMessage(id), acp.CodeInternalError, "agy-acp-muzzle: kernel exited during "+method)
	}
	close(s.k.done)
	_ = s.k.proc.Stop() // reaps it and releases the pipes
	s.reset()
}

func (s *Supervisor) stopKernel(reason string) {
	if s.k == nil {
		return
	}
	s.Log.Info("stopping kernel", "reason", reason)
	close(s.k.done)
	if err := s.k.proc.Stop(); err != nil {
		s.Log.Debug("kernel stop", "err", err)
	}
	s.reset()
}

func (s *Supervisor) reset() {
	s.k = nil
	clear(s.inflightClient)
	clear(s.inflightKernel)
	clear(s.pending)
}

func (s *Supervisor) toKernel(line []byte) error {
	if s.k == nil {
		return nil
	}
	buf := make([]byte, 0, len(line)+1)
	if _, err := s.k.proc.Stdin.Write(append(append(buf, line...), '\n')); err != nil {
		// The exit is noticed when its output ends; don't fail the client.
		s.Log.Debug("write to kernel failed", "err", err)
	}
	return nil
}

func (s *Supervisor) replyError(id json.RawMessage, code int, msg string) error {
	out, err := acp.ErrorResponse(id, acp.RPCError{Code: code, Message: msg})
	if err != nil {
		return err
	}
	return s.toClient.WriteLine(out)
}

// readLines sends each non-empty line of r (without its newline) to ch and
// closes ch at EOF. It gives up once done is closed.
func readLines(r io.Reader, ch chan<- []byte, done <-chan struct{}) {
	defer close(ch)
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if line = bytes.TrimRight(line, "\r\n"); len(line) > 0 {
			select {
			case ch <- line:
			case <-done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}
