// Package policy implements the relay filters for a muzzle session.
//
// Locked is the policy for untrusted input: no built-in tools, no MCP servers,
// an empty working directory, no client filesystem or terminal, every
// permission request rejected inside the muzzle (so client-side auto-accept
// never sees it), and a turn that hit a denial reported as failed.
package policy

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Arsenikki/agy-acp-muzzle/internal/acp"
	"github.com/Arsenikki/agy-acp-muzzle/internal/relay"
)

// ACP method names the policy acts on.
const (
	methodInitialize      = "initialize"
	methodSessionNew      = "session/new"
	methodSessionLoad     = "session/load"
	methodSessionResume   = "session/resume"
	methodSessionPrompt   = "session/prompt"
	methodSetMode         = "session/set_mode"
	methodSetConfigOption = "session/set_config_option"
	methodRequestPerm     = "session/request_permission"
)

// lockedMode is the only kernel mode a locked session may run in: the kernel
// asks permission for every tool, and the muzzle rejects every ask.
const lockedMode = "default"

// CodeDenied is the JSON-RPC error code for a prompt turn that ended after the
// muzzle denied one or more actions.
const CodeDenied = -32001

// muzzleIDPrefix marks requests the muzzle itself sends to the kernel; their
// responses are consumed and never reach the client.
const muzzleIDPrefix = "agy-acp-muzzle-"

// Denial is one action the muzzle refused during a turn.
type Denial struct {
	ToolCallID string `json:"toolCallId,omitempty"`
	Title      string `json:"title,omitempty"`
	Kind       string `json:"kind,omitempty"`
}

// Locked holds the per-connection state of the locked policy. The two filters
// run on different goroutines, so all state is guarded by mu.
type Locked struct {
	// Workdir replaces every session's cwd. It should be an empty directory
	// owned by this connection.
	Workdir string

	mu sync.Mutex
	// prompts maps an in-flight session/prompt request id to its session.
	prompts map[string]string
	// sessionReqs maps an in-flight session/new|load|resume request id to the
	// session id it names ("" for session/new, whose id is in the response).
	sessionReqs map[string]string
	// denials collects refused actions per session for the current turn.
	denials map[string][]Denial
	seq     int
}

// NewLocked returns a locked policy that pins sessions to workdir.
func NewLocked(workdir string) *Locked {
	return &Locked{
		Workdir:     workdir,
		prompts:     map[string]string{},
		sessionReqs: map[string]string{},
		denials:     map[string][]Denial{},
	}
}

// FromClient filters client -> kernel lines.
func (p *Locked) FromClient(line []byte) relay.Decision {
	env, err := acp.Parse(line)
	if err != nil || env.Kind() != acp.KindRequest {
		// Notifications (e.g. session/cancel) and client responses pass;
		// the kernel's requests to the client are answered by FromKernel,
		// so a client response here can only answer something we forwarded.
		return relay.Decision{Forward: line}
	}
	switch env.Method {
	case methodInitialize:
		return rewrite(line, env, stripClientCapabilities)
	case methodSessionNew, methodSessionLoad, methodSessionResume:
		var params struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(env.Params, &params)
		p.mu.Lock()
		p.sessionReqs[string(env.ID)] = params.SessionID
		p.mu.Unlock()
		return rewrite(line, env, p.lockSession)
	case methodSessionPrompt:
		var params struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(env.Params, &params)
		p.mu.Lock()
		p.prompts[string(env.ID)] = params.SessionID
		delete(p.denials, params.SessionID)
		p.mu.Unlock()
		return relay.Decision{Forward: line}
	case methodSetMode:
		var params struct {
			ModeID string `json:"modeId"`
		}
		_ = json.Unmarshal(env.Params, &params)
		if params.ModeID != lockedMode {
			return refuse(env.ID, fmt.Sprintf("locked session: mode %q is not allowed", params.ModeID))
		}
	case methodSetConfigOption:
		var params struct {
			ConfigID string          `json:"configId"`
			Value    json.RawMessage `json:"value"`
		}
		_ = json.Unmarshal(env.Params, &params)
		if params.ConfigID == "mode" && string(params.Value) != `"`+lockedMode+`"` {
			return refuse(env.ID, "locked session: changing the mode is not allowed")
		}
	}
	return relay.Decision{Forward: line}
}

// FromKernel filters kernel -> client lines.
func (p *Locked) FromKernel(line []byte) relay.Decision {
	env, err := acp.Parse(line)
	if err != nil {
		return relay.Decision{Forward: line}
	}
	switch env.Kind() {
	case acp.KindRequest:
		return p.kernelRequest(env)
	case acp.KindResponse:
		return p.kernelResponse(line, env)
	}
	return relay.Decision{Forward: line}
}

// kernelRequest answers every kernel -> client request the locked policy does
// not want the client to see.
func (p *Locked) kernelRequest(env *acp.Envelope) relay.Decision {
	if env.Method == methodRequestPerm {
		return p.rejectPermission(env)
	}
	if strings.HasPrefix(env.Method, "fs/") || strings.HasPrefix(env.Method, "terminal/") {
		// Capabilities were stripped at initialize; refuse anyway.
		return refuseToKernel(env.ID, acp.CodeMethodNotFound, "locked session: client "+env.Method+" is not available")
	}
	// Anything else (unknown extension methods) is refused rather than
	// forwarded: a locked session exposes nothing of the client.
	return refuseToKernel(env.ID, acp.CodeMethodNotFound, "locked session: "+env.Method+" is not available")
}

func (p *Locked) rejectPermission(env *acp.Envelope) relay.Decision {
	var params struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Kind       string `json:"kind"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(env.Params, &params)

	p.mu.Lock()
	p.denials[params.SessionID] = append(p.denials[params.SessionID], Denial{
		ToolCallID: params.ToolCall.ToolCallID,
		Title:      params.ToolCall.Title,
		Kind:       params.ToolCall.Kind,
	})
	p.mu.Unlock()

	outcome := map[string]string{"outcome": "cancelled"}
	for _, o := range params.Options {
		if o.Kind == "reject_once" || o.Kind == "reject_always" {
			outcome = map[string]string{"outcome": "selected", "optionId": o.OptionID}
			break
		}
	}
	reply, err := acp.ResultResponse(env.ID, map[string]any{"outcome": outcome})
	if err != nil {
		return relay.Decision{}
	}
	return relay.Decision{Reply: reply}
}

// kernelResponse turns a prompt turn with denials into an error, re-pins a
// restored session's mode, and swallows responses to the muzzle's own requests.
func (p *Locked) kernelResponse(line []byte, env *acp.Envelope) relay.Decision {
	id := string(env.ID)
	if strings.HasPrefix(id, `"`+muzzleIDPrefix) {
		return relay.Decision{}
	}

	p.mu.Lock()
	sessionID, isPrompt := p.prompts[id]
	restoredID, isSession := p.sessionReqs[id]
	var denied []Denial
	if isPrompt {
		delete(p.prompts, id)
		denied = p.denials[sessionID]
		delete(p.denials, sessionID)
	}
	if isSession {
		delete(p.sessionReqs, id)
	}
	p.mu.Unlock()

	if isPrompt && len(denied) > 0 && len(env.Error) == 0 {
		out, err := acp.ErrorResponse(env.ID, acp.RPCError{
			Code:    CodeDenied,
			Message: fmt.Sprintf("locked session: %d action(s) denied", len(denied)),
			Data:    map[string]any{"deniedActions": denied},
		})
		if err != nil {
			return relay.Decision{Forward: line}
		}
		return relay.Decision{Forward: out}
	}

	if isSession && len(env.Result) > 0 {
		return p.pinMode(line, env, restoredID)
	}
	return relay.Decision{Forward: line}
}

// pinMode sends session/set_mode default to the kernel when a new or restored
// session reports any other current mode (e.g. a persisted "yolo"). The relay
// writes the reply before forwarding the response, so the kernel switches mode
// before the client can send its first prompt.
func (p *Locked) pinMode(line []byte, env *acp.Envelope, restoredID string) relay.Decision {
	var result struct {
		SessionID string `json:"sessionId"`
		Modes     *struct {
			CurrentModeID string `json:"currentModeId"`
		} `json:"modes"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil || result.Modes == nil ||
		result.Modes.CurrentModeID == lockedMode {
		return relay.Decision{Forward: line}
	}
	sessionID := result.SessionID
	if sessionID == "" {
		// session/load and session/resume responses carry no sessionId; it
		// came from the request.
		sessionID = restoredID
	}
	if sessionID == "" {
		// Fail closed rather than run unpinned.
		out, err := acp.ErrorResponse(env.ID, acp.RPCError{
			Code:    acp.CodeInternalError,
			Message: fmt.Sprintf("locked session: restored in mode %q, refusing", result.Modes.CurrentModeID),
		})
		if err != nil {
			return relay.Decision{}
		}
		return relay.Decision{Forward: out}
	}
	p.mu.Lock()
	p.seq++
	reqID := fmt.Sprintf("%s%d", muzzleIDPrefix, p.seq)
	p.mu.Unlock()
	req, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      reqID,
		"method":  methodSetMode,
		"params":  map[string]string{"sessionId": sessionID, "modeId": lockedMode},
	})
	if err != nil {
		return relay.Decision{Forward: line}
	}
	return relay.Decision{Forward: line, Reply: req}
}

// lockSession rewrites session/new|load|resume params: pinned cwd, no MCP
// servers, no additional directories, and every built-in tool disabled.
func (p *Locked) lockSession(params map[string]any) {
	params["cwd"] = p.Workdir
	params["mcpServers"] = []any{}
	delete(params, "additionalDirectories")

	meta, _ := params["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	// The kernel also reads the legacy "agy" block; drop it so it cannot
	// carry a competing tool filter.
	delete(meta, "agy")
	ag, _ := meta["antigravity"].(map[string]any)
	if ag == nil {
		ag = map[string]any{}
	}
	ag["enabledTools"] = []any{}
	delete(ag, "disabledTools")
	meta["antigravity"] = ag
	params["_meta"] = meta
}

// stripClientCapabilities removes the client filesystem and terminal from
// initialize, so the kernel never routes tool I/O through the client.
func stripClientCapabilities(params map[string]any) {
	caps, _ := params["clientCapabilities"].(map[string]any)
	if caps == nil {
		return
	}
	delete(caps, "fs")
	delete(caps, "terminal")
}

// rewrite applies edit to a request's params object and re-encodes the line.
// A request whose params cannot be decoded is refused, never forwarded as-is.
func rewrite(line []byte, env *acp.Envelope, edit func(map[string]any)) relay.Decision {
	params := map[string]any{}
	if len(env.Params) > 0 {
		if err := json.Unmarshal(env.Params, &params); err != nil {
			return refuse(env.ID, "locked session: unreadable params for "+env.Method)
		}
	}
	edit(params)
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return refuse(env.ID, "locked session: unreadable request")
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return refuse(env.ID, "locked session: cannot encode params")
	}
	msg["params"] = raw
	out, err := json.Marshal(msg)
	if err != nil {
		return refuse(env.ID, "locked session: cannot encode request")
	}
	return relay.Decision{Forward: out}
}

// refuse answers a client request with an error instead of forwarding it.
func refuse(id json.RawMessage, msg string) relay.Decision {
	out, err := acp.ErrorResponse(id, acp.RPCError{Code: acp.CodeInvalidRequest, Message: msg})
	if err != nil {
		return relay.Decision{}
	}
	return relay.Decision{Reply: out}
}

// refuseToKernel answers a kernel request with an error instead of forwarding
// it to the client.
func refuseToKernel(id json.RawMessage, code int, msg string) relay.Decision {
	out, err := acp.ErrorResponse(id, acp.RPCError{Code: code, Message: msg})
	if err != nil {
		return relay.Decision{}
	}
	return relay.Decision{Reply: out}
}
