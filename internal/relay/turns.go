package relay

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/Arsenikki/agy-acp-muzzle/internal/acp"
)

// Turn guards: the kernel reports some failures as ordinary assistant text
// ("Agent execution error: ...") followed by a successful end_turn, and can
// end a turn having produced nothing. Clients then record a failed turn as a
// success. With TurnGuard set, both become JSON-RPC errors. TurnTimeout
// fails (and cancels) a turn whose kernel has gone silent.

// kernelErrorPrefix starts the assistant text the kernel emits for an error.
const kernelErrorPrefix = "Agent execution error:"

// CodeTurnFailed is the JSON-RPC error code for a turn the guards failed.
const CodeTurnFailed = -32003

// turn tracks one session/prompt in flight.
type turn struct {
	session  string // kernel session id
	content  bool   // any agent message text or tool call seen
	text     strings.Builder
	answered bool // already failed by the watchdog; drop the kernel's answer
}

func (s *Supervisor) startTurn(id, session string) {
	s.turns[id] = &turn{session: session}
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
			if p.Update.Content.Text != "" {
				t.content = true
				if t.text.Len() < 512 {
					t.text.WriteString(p.Update.Content.Text)
				}
			}
		case "tool_call", "tool_call_update":
			t.content = true
		}
	}
}

// finishTurn checks a kernel response to a prompt. It returns the line to
// forward (possibly rewritten into an error) and whether to drop it.
func (s *Supervisor) finishTurn(env *acp.Envelope, line []byte) ([]byte, bool) {
	id := string(env.ID)
	t, ok := s.turns[id]
	if !ok {
		return line, false
	}
	delete(s.turns, id)
	if t.answered {
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
	text := strings.TrimSpace(t.text.String())
	var msg string
	switch {
	case strings.HasPrefix(text, kernelErrorPrefix):
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

// checkStalledTurns fails turns whose kernel has produced no output for
// TurnTimeout while nothing waits on the client, and cancels them in the
// kernel. Their late answers are dropped.
func (s *Supervisor) checkStalledTurns(now time.Time) {
	if s.TurnTimeout <= 0 || s.k == nil || len(s.inflightKernel) > 0 {
		return
	}
	if now.Sub(s.lastKernelOutput) < s.TurnTimeout {
		return
	}
	for id, t := range s.turns {
		if t.answered {
			continue
		}
		t.answered = true
		delete(s.inflightClient, id)
		s.Log.Warn("turn timed out", "session", t.session, "silentFor", now.Sub(s.lastKernelOutput).Round(time.Second))
		cancel, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": t.session}})
		_ = s.toKernel(cancel)
		_ = s.replyError(json.RawMessage(id), CodeTurnFailed, "agy-acp-muzzle: turn timed out: no output from the kernel for "+s.TurnTimeout.String())
	}
}

// watchdogInterval is how often stalled turns are checked.
func watchdogInterval(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return time.Hour // effectively off; checkStalledTurns ignores it
	}
	return min(max(timeout/4, 10*time.Millisecond), 15*time.Second)
}
