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

	// TurnGuard fails turns that end in a kernel error text or with no
	// output; TurnTimeout fails and cancels a turn whose kernel is silent
	// for that long (zero disables). See turns.go.
	TurnGuard   bool
	TurnTimeout time.Duration

	// Cache, if set, answers initialize and session/new without a kernel
	// (see lazy.go); Sessions persists the placeholder ids it hands out.
	Cache    Cache
	Sessions SessionStore

	lazy             map[string]*lazySession // placeholder -> session
	fillRelease      func()                  // held while this connection fills a cold cache
	realToLazy       map[string]string       // kernel session id -> placeholder
	turns            map[string]*turn        // prompt request id -> turn
	lastKernelOutput time.Time
	blockedBy        string // last logged reason the kernel could not idle

	clientCh     chan []byte
	clientClosed bool                 // client input ended while the loop was blocked
	backlog      [][]byte             // client lines read while the loop was blocked
	trigger      *trigger             // client request being handled (see cancel.go)
	draining     map[string]time.Time // kernel session -> cancelled turn not yet over
	held         map[string][][]byte  // kernel session -> prompts waiting for the drain

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
	out   chan []byte // lines for the kernel, written by writeLines
	done  chan struct{}
}

// kernelQueue is how many lines may wait for the kernel to read its stdin
// before the main loop blocks.
const kernelQueue = 4096

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
	s.lazy = map[string]*lazySession{}
	s.realToLazy = map[string]string{}
	s.turns = map[string]*turn{}
	s.draining = map[string]time.Time{}
	s.held = map[string][][]byte{}

	s.clientCh = make(chan []byte)
	go readLines(s.ClientIn, s.clientCh, nil)

	idle := time.NewTimer(time.Hour)
	idle.Stop()
	watchdog := time.NewTicker(watchdogInterval(s.TurnTimeout))
	defer watchdog.Stop()
	defer s.stopKernel("client gone")
	defer s.releaseFill()

	for {
		// Client lines queued while the loop was blocked come first, in order.
		if len(s.backlog) > 0 {
			line := s.backlog[0]
			s.backlog = s.backlog[1:]
			if err := s.fromClient(line); err != nil {
				return err
			}
			s.armIdle(idle)
			continue
		}
		if s.clientClosed {
			return nil
		}
		var kernelLines chan []byte
		if s.k != nil {
			kernelLines = s.k.lines
		}
		select {
		case line, ok := <-s.clientCh:
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
		case now := <-watchdog.C:
			// A tick is not activity: re-arming the idle timer on every
			// tick would postpone idle shutdown forever.
			stalled := s.checkStalledTurns(now)
			if expired := s.expireDrains(now); !stalled && !expired {
				continue
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
		s.blockedBy = ""
		return
	}
	if s.k != nil && s.IdleTimeout > 0 {
		// Say once per change what keeps the kernel from idling.
		if why := fmt.Sprintf("client=%v kernel=%v", s.inflightClient, s.inflightKernel); why != s.blockedBy {
			s.blockedBy = why
			s.Log.Debug("idle blocked", "inflight", why)
		}
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
	// Decode params.sessionId once: params can be megabytes (images).
	sid := sessionIDOf(env.Params)
	if env.Kind() == acp.KindRequest {
		if handled, err := s.answerFromCache(env, d.Forward); handled {
			return err
		}
		// While this request is handled the loop may block (lazy session,
		// restore); a cancel for it is answered meanwhile (see cancel.go).
		s.trigger = &trigger{id: env.ID, method: env.Method, session: sid}
		defer func() { s.trigger = nil }()
	}
	forward, env, sid, skip, err := s.mapToKernel(env, d.Forward, sid)
	if skip || err != nil || s.trigger.isCancelled() {
		return err
	}
	d.Forward = forward
	id := string(env.ID)
	switch env.Kind() {
	case acp.KindResponse:
		_, known := s.inflightKernel[id]
		s.Log.Debug("client response", "id", id, "matched", known)
		delete(s.inflightKernel, id)
		// The kernel was waiting on the client; its silence starts now.
		s.lastKernelOutput = time.Now()
		if s.k == nil {
			return nil // answers a kernel that has since been stopped
		}
		return s.toKernel(d.Forward)
	case acp.KindNotification:
		if env.Method == "session/cancel" {
			s.cancelTurns(sid)
		}
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
		if s.trigger.isCancelled() {
			return nil
		}
	}
	if env.Method == "session/prompt" && s.holdPrompt(env, d.Forward, sid) {
		return nil
	}
	if env.Method == "session/prompt" {
		s.startTurn(id, sid)
		s.lastKernelOutput = time.Now() // waiting for the kernel from now
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
	s.lastKernelOutput = time.Now()
	if err == nil && env.Kind() != acp.KindResponse && s.drainingLine(env) {
		return nil // belongs to a turn the client already saw cancelled
	}
	if err == nil && env.Kind() == acp.KindResponse {
		id := string(env.ID)
		if strings.HasPrefix(id, `"`+svIDPrefix) {
			return nil // late answer to a timed-out restore request
		}
		var drop bool
		if line, drop = s.finishTurn(env, line); drop {
			return nil // already failed by the watchdog
		}
		method := s.inflightClient[id]
		delete(s.inflightClient, id)
		if method == "initialize" && len(env.Result) > 0 && s.Cache != nil {
			s.Cache.SaveInitialize(env.Result)
		}
		if p, ok := s.pending[id]; ok {
			delete(s.pending, id)
			if len(env.Result) > 0 {
				s.rememberSession(p, env.Result)
				if p.method == "session/new" && s.Cache != nil {
					s.Cache.SaveSessionNew(env.Result)
					s.releaseFill()
				}
			}
		}
	} else if err == nil {
		s.observeUpdate(env)
		// Kernel requests and notifications name kernel session ids; the
		// client knows lazily created sessions by their placeholders.
		if ph, ok := s.realToLazy[sessionIDOf(env.Params)]; ok {
			line = withSessionID(line, ph)
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
		s.Log.Debug("kernel request", "method", fe.Method, "id", string(fe.ID))
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
	k := &kernelConn{proc: proc, lines: make(chan []byte), out: make(chan []byte, kernelQueue), done: make(chan struct{})}
	go readLines(proc.Stdout, k.lines, k.done)
	go s.writeLines(k)
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
	result, err := s.call(s.init, false)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if s.Cache != nil {
		s.Cache.SaveInitialize(result)
	}
	for _, sid := range s.order {
		rec := s.sessions[sid]
		var params map[string]json.RawMessage
		if json.Unmarshal(rec.params, &params) != nil {
			params = map[string]json.RawMessage{}
		}
		params["sessionId"], _ = json.Marshal(sid)
		req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 0, "method": "session/load", "params": params})
		if _, err := s.call(req, false); err != nil {
			s.Log.Warn("session not restored", "session", sid, "err", err)
			continue
		}
		for _, setting := range rec.settings {
			if _, err := s.call(setting, false); err != nil {
				s.Log.Warn("setting not restored", "session", sid, "err", err)
			}
		}
	}
	return nil
}

// call sends req (its id replaced by a supervisor id) to the kernel and waits
// for the answer. Other kernel output meanwhile is either dropped (restore:
// notifications are the history replay the client already has, requests are
// refused) or, with passthrough, handled normally so concurrent sessions on
// the connection are unaffected.
func (s *Supervisor) call(req []byte, passthrough bool) (json.RawMessage, error) {
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
		// Keep reading the client meanwhile: a cancel must be answered fast.
		var clientCh chan []byte
		if !s.clientClosed {
			clientCh = s.clientCh
		}
		select {
		case line, ok := <-clientCh:
			if !ok {
				s.clientClosed = true
				continue
			}
			s.readClient(line)
		case line, ok := <-s.k.lines:
			if !ok {
				return nil, errors.New("kernel exited")
			}
			env, err := acp.Parse(line)
			if err == nil && env.Kind() == acp.KindResponse && bytes.Equal(env.ID, want) {
				if len(env.Error) > 0 {
					return nil, fmt.Errorf("%s", env.Error)
				}
				return env.Result, nil
			}
			if passthrough {
				if err := s.fromKernel(line); err != nil {
					return nil, err
				}
				continue
			}
			if err == nil && env.Kind() == acp.KindRequest {
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
	clear(s.draining)
	clear(s.held)
	clear(s.turns)
	s.releaseFill() // a kernel that died cannot fill the cache
	s.k = nil
	clear(s.inflightClient)
	clear(s.inflightKernel)
	clear(s.pending)
}

// toKernel queues a line for the kernel. Writing happens in writeLines, so the
// main loop never blocks on the kernel's stdin while the kernel may itself be
// blocked writing output only the main loop reads.
func (s *Supervisor) toKernel(line []byte) error {
	if s.k == nil {
		return nil
	}
	buf := make([]byte, 0, len(line)+1)
	select {
	case s.k.out <- append(append(buf, line...), '\n'):
	case <-s.k.done:
	}
	return nil
}

// writeLines writes queued lines to the kernel until it is stopped.
func (s *Supervisor) writeLines(k *kernelConn) {
	for {
		select {
		case line := <-k.out:
			if _, err := k.proc.Stdin.Write(line); err != nil {
				// The exit is noticed when its output ends; don't fail the client.
				s.Log.Debug("write to kernel failed", "err", err)
			}
		case <-k.done:
			return
		}
	}
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

// answerFromCache answers initialize (with no kernel running) and
// session/new from the cache, without starting a kernel.
func (s *Supervisor) answerFromCache(env *acp.Envelope, forwarded []byte) (bool, error) {
	if s.Cache == nil {
		return false, nil
	}
	initResult, newResult := s.Cache.Load()
	if env.Method == "initialize" && s.k == nil && (initResult == nil || newResult == nil) {
		initResult, newResult = s.awaitCacheFill()
	}
	switch {
	case env.Method == "initialize" && s.k == nil && initResult != nil:
		s.init = forwarded
		return true, s.replyResult(env.ID, initResult)
	case env.Method == "session/new" && newResult != nil:
		ph := newPlaceholder()
		s.lazy[ph] = &lazySession{params: env.Params}
		s.Log.Debug("session answered from cache", "placeholder", ph)
		return true, s.replyResult(env.ID, withResultSessionID(newResult, ph))
	}
	return false, nil
}

// mapToKernel rewrites a client message naming a placeholder session to the
// kernel's session id, creating the kernel session on first use. skip means
// the message was fully handled (dropped or answered). ph is the message's
// params.sessionId; the kernel session id is returned.
func (s *Supervisor) mapToKernel(env *acp.Envelope, line []byte, ph string) ([]byte, *acp.Envelope, string, bool, error) {
	ls, known := s.lazy[ph]
	if !known && ph != "" && s.Sessions != nil {
		// A placeholder from an earlier connection (e.g. before a restart).
		if real, ok := s.Sessions.Lookup(ph); ok {
			ls = &lazySession{real: real}
			s.lazy[ph], s.realToLazy[real] = ls, ph
			known = true
		}
	}
	if !known {
		return line, env, ph, false, nil
	}
	if ls.real == "" {
		switch {
		case env.Kind() != acp.KindRequest:
			return nil, env, ph, true, nil // e.g. session/cancel: nothing started yet
		case env.Method == "session/load" || env.Method == "session/resume":
			return nil, env, ph, true, s.replyError(env.ID, -32002, "agy-acp-muzzle: session "+ph+" was never used")
		}
		if err := s.materialize(ph, ls); err != nil {
			s.Log.Error("cannot create kernel session", "placeholder", ph, "err", err)
			return nil, env, ph, true, s.replyError(env.ID, acp.CodeInternalError, "agy-acp-muzzle: cannot create session: "+err.Error())
		}
	}
	line = withSessionID(line, ls.real)
	mapped, err := acp.Parse(line)
	if err != nil {
		return line, env, ls.real, false, nil
	}
	return line, mapped, ls.real, false, nil
}

// materialize creates the kernel session behind a placeholder.
func (s *Supervisor) materialize(ph string, ls *lazySession) error {
	if s.k == nil {
		if err := s.startKernel(true); err != nil {
			return err
		}
	}
	started := time.Now()
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 0, "method": "session/new", "params": ls.params})
	result, err := s.call(req, true)
	if err != nil {
		return err
	}
	var r struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(result, &r)
	real := r.SessionID
	if real == "" {
		return errors.New("kernel returned no session id")
	}
	ls.real = real
	s.realToLazy[real] = ph
	s.rememberSession(pendingSession{method: "session/new", params: ls.params}, result)
	if s.Sessions != nil {
		s.Sessions.Store(ph, real)
	}
	if s.Cache != nil {
		s.Cache.SaveSessionNew(result)
	}
	s.Log.Info("kernel session created", "placeholder", ph, "session", real, "took", time.Since(started).Round(time.Millisecond))
	return nil
}

func (s *Supervisor) replyResult(id json.RawMessage, result json.RawMessage) error {
	out, err := acp.ResultResponse(id, result)
	if err != nil {
		return err
	}
	return s.toClient.WriteLine(out)
}

// fillLockTimeout bounds how long a connection waits for another one to fill
// an empty cache before starting its own kernel.
const fillLockTimeout = 90 * time.Second

// awaitCacheFill handles a cold cache: it takes the cache's fill lock (waiting
// while another connection fills it), then re-reads the cache. If the cache is
// still cold this connection fills it and keeps the lock until session/new is
// cached; otherwise it answers from what the other connection stored.
func (s *Supervisor) awaitCacheFill() (json.RawMessage, json.RawMessage) {
	l, ok := s.Cache.(CacheLocker)
	if !ok || s.fillRelease != nil {
		return s.Cache.Load()
	}
	release, err := l.LockFill(fillLockTimeout)
	if err != nil {
		s.Log.Warn("cache fill lock unavailable", "err", err)
		return s.Cache.Load()
	}
	initResult, newResult := s.Cache.Load()
	if initResult != nil && newResult != nil {
		release()
		return initResult, newResult
	}
	s.fillRelease = release
	return initResult, newResult
}

// releaseFill drops the cache fill lock, if held.
func (s *Supervisor) releaseFill() {
	if s.fillRelease != nil {
		s.fillRelease()
		s.fillRelease = nil
	}
}
