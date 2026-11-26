package relay

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Arsenikki/agy-acp-muzzle/internal/acp"
)

// Turn guards: the kernel reports some failures as ordinary assistant text
// ("Agent execution error: ...") followed by a successful end_turn, and can
// end a turn having produced nothing. Clients then record a failed turn as a
// success. With TurnGuard set, both become JSON-RPC errors. TurnTimeout
// fails (and cancels) a turn whose kernel has gone silent. AutoContinue
// resumes a turn the kernel's harness crashed in (see continueTurn).

// kernelErrorPrefix starts the assistant text the kernel emits for an error.
const kernelErrorPrefix = "Agent execution error:"

// CodeTurnFailed is the JSON-RPC error code for a turn the guards failed.
const CodeTurnFailed = -32003

// harnessCrashMessage is how the kernel words a crash of its harness process,
// which ends the turn half done. The kernel's next prompt in the session finds
// the connection closed and rebuilds the agent from the persisted trajectory,
// so the history survives (kernel server.py, _rebuild_session_agent).
const harnessCrashMessage = "Harness process exited unexpectedly"

// contIDPrefix marks the prompts the muzzle sends to continue a crashed turn.
// It extends the policy's prefix but not svIDPrefix: their answers end the
// client's turn.
const contIDPrefix = "agy-acp-muzzle-cont-"

// continuePrompt asks the kernel to pick up a turn its harness crashed in.
const continuePrompt = "Your previous turn was cut off by an internal error (the harness process exited). " +
	"Continue the task from where you left off; do not repeat steps that already completed."

// turn tracks one session/prompt in flight.
type turn struct {
	session   string // kernel session id
	client    string // the client's prompt request id (the map key, unless continued)
	content   bool   // any agent message text or tool call seen
	errText   strings.Builder
	answered  bool // already failed by the watchdog; drop the kernel's answer
	continues int  // continuation prompts sent after harness crashes
}

func (s *Supervisor) startTurn(id, session string) {
	s.turns[id] = &turn{session: session, client: id}
}

// observeUpdate records a kernel session/update for the turns of its session.
func (s *Supervisor) observeUpdate(env *acp.Envelope) {
	if env.Method != "session/update" || len(s.turns) == 0 {
		return
	}
	var p struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Kind    string `json:"sessionUpdate"`
			Content struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	}
	if json.Unmarshal(env.Params, &p) != nil {
		return
	}
	for _, t := range s.turns {
		if t.session != p.SessionID {
			continue
		}
		switch p.Update.Kind {
		case "agent_message_chunk":
			text := p.Update.Content.Text
			if text == "" {
				continue
			}
			t.content = true
			// The kernel's error text ends the turn, possibly after other text.
			if strings.HasPrefix(strings.TrimSpace(text), kernelErrorPrefix) {
				t.errText.Reset()
				t.errText.WriteString(text)
			} else if t.errText.Len() > 0 && t.errText.Len() < 512 {
				t.errText.WriteString(text)
			}
		case "tool_call", "tool_call_update":
			t.content = true
			t.errText.Reset() // the turn went on: that was not the kernel's error
		}
	}
}

// finishTurn checks a kernel response to a prompt. It returns the line to
// forward (possibly rewritten into an error) and whether to drop it. The
// answer to a continuation is rewritten (env too) to answer the client's
// prompt.
func (s *Supervisor) finishTurn(env *acp.Envelope, line []byte) ([]byte, bool) {
	id := string(env.ID)
	t, ok := s.turns[id]
	if !ok {
		return line, false
	}
	delete(s.turns, id)
	if t.client != id {
		env.ID = json.RawMessage(t.client)
		line = withID(line, t.client)
	}
	if t.answered {
		// Already answered (cancelled or timed out): the kernel is done with
		// the turn now, so a prompt held behind it may go.
		s.endDrain(t.session)
		return nil, true
	}
	if len(env.Error) > 0 && isKernelCancellation(env.Error) {
		// ACP wants stopReason "cancelled", not an error.
		if out, err := acp.ResultResponse(env.ID, json.RawMessage(`{"stopReason":"cancelled"}`)); err == nil {
			return out, false
		}
	}
	if s.continueTurn(t, env) {
		return nil, true
	}
	if !s.TurnGuard || len(env.Error) > 0 {
		return line, false
	}
	var r struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(env.Result, &r)
	if r.StopReason != "end_turn" {
		return line, false
	}
	text := strings.TrimSpace(t.errText.String())
	var msg string
	switch {
	case text != "":
		msg = "kernel error: " + strings.TrimSpace(strings.TrimPrefix(text, kernelErrorPrefix))
	case !t.content:
		msg = "turn ended without any output"
	default:
		return line, false
	}
	s.Log.Warn("turn failed by guard", "session", t.session, "reason", msg)
	out, err := acp.ErrorResponse(env.ID, acp.RPCError{Code: CodeTurnFailed, Message: "agy-acp-muzzle: " + msg})
	if err != nil {
		return line, false
	}
	return out, false
}

// continueTurn handles a turn that ended in a crash of the kernel's harness:
// up to AutoContinue times per client prompt it tells the client, asks the
// kernel to continue, and keeps the client's prompt open until the
// continuation ends. It reports whether it did.
func (s *Supervisor) continueTurn(t *turn, env *acp.Envelope) bool {
	if t.continues >= s.AutoContinue {
		return false
	}
	var r struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(env.Result, &r)
	crashed := r.StopReason == "end_turn" && strings.Contains(t.errText.String(), harnessCrashMessage)
	if !crashed && !strings.Contains(string(env.Error), harnessCrashMessage) {
		return false
	}
	t.continues++
	t.errText.Reset() // content stays: the client saw the turn's output
	s.seq++
	id, _ := json.Marshal(fmt.Sprintf("%s%d", contIDPrefix, s.seq))
	s.turns[string(id)] = t
	s.lastKernelOutput = time.Now()
	s.Log.Warn("kernel harness crashed; continuing the turn", "session", t.session, "attempt", t.continues, "max", s.AutoContinue)

	session := t.session
	if ph, ok := s.realToLazy[session]; ok {
		session = ph
	}
	notice, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
		"sessionId": session,
		"update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{
			"type": "text",
			"text": fmt.Sprintf("\n\n[agy-acp-muzzle: the kernel's harness crashed; continuing (%d/%d)]\n\n", t.continues, s.AutoContinue),
		}},
	}})
	_ = s.toClient.WriteLine(notice)
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": "session/prompt", "params": map[string]any{
		"sessionId": t.session,
		"prompt":    []map[string]string{{"type": "text", "text": continuePrompt}},
	}})
	_ = s.toKernel(req)
	return true
}

// withID returns a JSON-RPC line with its id replaced by a raw JSON id.
func withID(line []byte, id string) []byte {
	var msg map[string]json.RawMessage
	if json.Unmarshal(line, &msg) != nil {
		return line
	}
	msg["id"] = json.RawMessage(id)
	out, err := json.Marshal(msg)
	if err != nil {
		return line
	}
	return out
}

// checkStalledTurns fails turns whose kernel has produced no output for
// TurnTimeout while nothing waits on the client, and cancels them in the
// kernel. Their late answers are dropped. It reports whether it failed any.
func (s *Supervisor) checkStalledTurns(now time.Time) bool {
	if s.TurnTimeout <= 0 || s.k == nil || len(s.inflightKernel) > 0 {
		return false
	}
	if now.Sub(s.lastKernelOutput) < s.TurnTimeout {
		return false
	}
	failed := false
	for _, t := range s.turns {
		if t.answered {
			continue
		}
		t.answered = true
		failed = true
		delete(s.inflightClient, t.client)
		s.Log.Warn("turn timed out", "session", t.session, "silentFor", now.Sub(s.lastKernelOutput).Round(time.Second))
		cancel, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": t.session}})
		_ = s.toKernel(cancel)
		_ = s.replyError(json.RawMessage(t.client), CodeTurnFailed, "agy-acp-muzzle: turn timed out: no output from the kernel for "+s.TurnTimeout.String())
	}
	return failed
}

// watchdogInterval is how often stalled turns and drains are checked.
func watchdogInterval(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 15 * time.Second // still expires drains; checkStalledTurns ignores it
	}
	return min(max(timeout/4, 10*time.Millisecond), 15*time.Second)
}
