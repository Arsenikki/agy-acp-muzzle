package relay

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/Arsenikki/agy-acp-muzzle/internal/acp"
)

// Fast cancellation. Clients such as Paseo cancel a turn and wait only
// briefly (2 s) for the prompt to end before sending the next one; if the
// prompt is still open then, the client rejects its own next turn ("A
// foreground turn is already active"). The kernel can take longer than that,
// and while the muzzle restores a stopped kernel it would not even read the
// cancel. So the muzzle answers a cancelled prompt at once with stopReason
// "cancelled", forwards the cancel, and keeps the session "draining" until the
// kernel has really finished the old turn: late updates of that turn are
// dropped, its permission requests are declined, and a new prompt for the
// session is held and forwarded once the old turn is over.

// drainTimeout bounds how long a new prompt waits for a cancelled turn.
const drainTimeout = 30 * time.Second

// kernelCancelledMessage is how the kernel words a cancelled turn when it
// answers the prompt with an error instead of stopReason "cancelled".
const kernelCancelledMessage = "cancelled by the client"

// trigger is the client request being handled while the muzzle may block
// (restoring a kernel or creating a lazy session).
type trigger struct {
	id        json.RawMessage
	method    string
	session   string // client session id
	cancelled bool   // answered as cancelled; do not forward
}

// readClient reads a client line while the main loop is blocked (see call):
// a cancel for the prompt being handled is answered at once, and every line
// is queued for the main loop in order.
func (s *Supervisor) readClient(line []byte) {
	s.backlog = append(s.backlog, line)
	env, err := acp.Parse(line)
	if err != nil || env.Kind() != acp.KindNotification || env.Method != "session/cancel" {
		return
	}
	t := s.trigger
	if t == nil || t.cancelled || t.method != "session/prompt" || t.session != sessionIDOf(env.Params) {
		return
	}
	t.cancelled = true
	s.Log.Info("prompt cancelled before it reached the kernel", "session", t.session)
	_ = s.replyCancelled(t.id)
}

// cancelTurns answers every open prompt of a kernel session as cancelled and
// starts draining the session. Held prompts count as open: from the client's
// view they are the active turn.
func (s *Supervisor) cancelTurns(session string) {
	for id, t := range s.turns {
		if t.session != session || t.answered {
			continue
		}
		t.answered = true
		delete(s.inflightClient, id)
		s.draining[session] = time.Now()
		_ = s.replyCancelled(json.RawMessage(id))
	}
	for _, line := range s.held[session] {
		if env, err := acp.Parse(line); err == nil {
			delete(s.inflightClient, string(env.ID))
			_ = s.replyCancelled(env.ID)
		}
	}
	delete(s.held, session)
}

// drainingLine handles kernel output for a draining session: updates are
// dropped and requests declined. It reports whether the line was handled.
func (s *Supervisor) drainingLine(env *acp.Envelope) bool {
	if _, ok := s.draining[sessionIDOf(env.Params)]; !ok {
		return false
	}
	switch env.Kind() {
	case acp.KindNotification:
		return true
	case acp.KindRequest:
		var reply []byte
		if env.Method == "session/request_permission" {
			reply, _ = acp.ResultResponse(env.ID, map[string]any{"outcome": map[string]string{"outcome": "cancelled"}})
		} else {
			reply, _ = acp.ErrorResponse(env.ID, acp.RPCError{Code: acp.CodeInternalError, Message: "agy-acp-muzzle: turn cancelled"})
		}
		_ = s.toKernel(reply)
		return true
	}
	return false
}

// holdPrompt queues a prompt for a draining session. It reports whether it
// did.
func (s *Supervisor) holdPrompt(env *acp.Envelope, line []byte) bool {
	session := sessionIDOf(env.Params)
	if _, ok := s.draining[session]; !ok {
		return false
	}
	s.held[session] = append(s.held[session], line)
	s.inflightClient[string(env.ID)] = env.Method
	s.Log.Debug("prompt held until the cancelled turn ends", "session", session)
	return true
}

// endDrain ends a session's draining and forwards its held prompts.
func (s *Supervisor) endDrain(session string) {
	if _, ok := s.draining[session]; !ok {
		return
	}
	delete(s.draining, session)
	held := s.held[session]
	delete(s.held, session)
	for _, line := range held {
		env, err := acp.Parse(line)
		if err != nil {
			continue
		}
		s.startTurn(string(env.ID), session)
		s.lastKernelOutput = time.Now()
		_ = s.toKernel(line)
	}
}

// expireDrains ends drains that waited longer than drainTimeout for the
// kernel. It reports whether it ended any.
func (s *Supervisor) expireDrains(now time.Time) bool {
	ended := false
	for session, since := range s.draining {
		if now.Sub(since) >= drainTimeout {
			s.Log.Warn("cancelled turn never ended; releasing held prompts", "session", session)
			s.endDrain(session)
			ended = true
		}
	}
	return ended
}

// isKernelCancellation reports whether a prompt error is the kernel's way of
// saying the turn was cancelled.
func isKernelCancellation(rpcErr json.RawMessage) bool {
	var e struct {
		Message string `json:"message"`
		Data    any    `json:"data"`
	}
	_ = json.Unmarshal(rpcErr, &e)
	b, _ := json.Marshal(e.Data)
	return strings.Contains(e.Message, kernelCancelledMessage) || strings.Contains(string(b), kernelCancelledMessage)
}

func (s *Supervisor) replyCancelled(id json.RawMessage) error {
	return s.replyResult(id, json.RawMessage(`{"stopReason":"cancelled"}`))
}

// isCancelled is nil-safe.
func (t *trigger) isCancelled() bool { return t != nil && t.cancelled }
