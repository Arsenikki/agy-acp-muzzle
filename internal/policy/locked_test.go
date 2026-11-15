package policy

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Arsenikki/agy-acp-muzzle/internal/relay"
)

const workdir = "/tmp/muzzle-test-work"

func decode(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("decode %s: %v", line, err)
	}
	return m
}

func path(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[k]
	}
	return cur
}

func forwardedOnly(t *testing.T, d relay.Decision) map[string]any {
	t.Helper()
	if d.Forward == nil || d.Reply != nil {
		t.Fatalf("want forward only, got forward=%s reply=%s", d.Forward, d.Reply)
	}
	return decode(t, d.Forward)
}

func repliedOnly(t *testing.T, d relay.Decision) map[string]any {
	t.Helper()
	if d.Forward != nil || d.Reply == nil {
		t.Fatalf("want reply only, got forward=%s reply=%s", d.Forward, d.Reply)
	}
	return decode(t, d.Reply)
}

func TestInitializeStripsClientIO(t *testing.T) {
	p := NewLocked(workdir)
	m := forwardedOnly(t, p.FromClient([]byte(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":true,"writeTextFile":true},"terminal":true,"auth":{"x":1}}}}`)))
	caps := path(m, "params", "clientCapabilities").(map[string]any)
	if _, ok := caps["fs"]; ok {
		t.Error("fs capability survived")
	}
	if _, ok := caps["terminal"]; ok {
		t.Error("terminal capability survived")
	}
	if caps["auth"] == nil {
		t.Error("unrelated capability dropped")
	}
	if path(m, "params", "protocolVersion") != float64(1) {
		t.Error("protocolVersion lost")
	}
}

func TestSessionNewIsLocked(t *testing.T) {
	p := NewLocked(workdir)
	in := `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/home/user/project","additionalDirectories":["/etc"],` +
		`"mcpServers":[{"name":"x","type":"http","url":"http://x"}],` +
		`"_meta":{"antigravity":{"enabledTools":["run_command"],"disabledTools":["finish"],"other":true},"agy":{"enabledTools":["run_command"]},"client":"keep"}}}`
	m := forwardedOnly(t, p.FromClient([]byte(in)))

	if got := path(m, "params", "cwd"); got != workdir {
		t.Errorf("cwd = %v", got)
	}
	if got := path(m, "params", "mcpServers"); !reflect.DeepEqual(got, []any{}) {
		t.Errorf("mcpServers = %v", got)
	}
	if got := path(m, "params", "additionalDirectories"); got != nil {
		t.Errorf("additionalDirectories = %v", got)
	}
	if got := path(m, "params", "_meta", "antigravity", "enabledTools"); !reflect.DeepEqual(got, []any{}) {
		t.Errorf("enabledTools = %v", got)
	}
	if got := path(m, "params", "_meta", "antigravity", "disabledTools"); got != nil {
		t.Errorf("disabledTools = %v", got)
	}
	if got := path(m, "params", "_meta", "agy"); got != nil {
		t.Errorf("legacy agy meta = %v", got)
	}
	if path(m, "params", "_meta", "client") != "keep" || path(m, "params", "_meta", "antigravity", "other") != true {
		t.Error("unrelated _meta dropped")
	}
	if m["id"] != float64(1) || m["method"] != "session/new" {
		t.Errorf("envelope changed: %v", m)
	}
}

func TestSessionLoadWithoutParamsGetsLocked(t *testing.T) {
	p := NewLocked(workdir)
	m := forwardedOnly(t, p.FromClient([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/load"}`)))
	if path(m, "params", "cwd") != workdir {
		t.Fatalf("params not locked: %v", m)
	}
}

func TestUnreadableSessionParamsRefused(t *testing.T) {
	p := NewLocked(workdir)
	m := repliedOnly(t, p.FromClient([]byte(`{"jsonrpc":"2.0","id":3,"method":"session/new","params":[1,2]}`)))
	if m["error"] == nil {
		t.Fatalf("want error, got %v", m)
	}
}

func TestModeChanges(t *testing.T) {
	p := NewLocked(workdir)
	tests := []struct {
		name    string
		line    string
		refused bool
	}{
		{"set_mode yolo", `{"jsonrpc":"2.0","id":4,"method":"session/set_mode","params":{"sessionId":"s","modeId":"yolo"}}`, true},
		{"set_mode auto_edit", `{"jsonrpc":"2.0","id":5,"method":"session/set_mode","params":{"sessionId":"s","modeId":"auto_edit"}}`, true},
		{"set_mode default", `{"jsonrpc":"2.0","id":6,"method":"session/set_mode","params":{"sessionId":"s","modeId":"default"}}`, false},
		{"config mode yolo", `{"jsonrpc":"2.0","id":7,"method":"session/set_config_option","params":{"sessionId":"s","configId":"mode","value":"yolo"}}`, true},
		{"config mode default", `{"jsonrpc":"2.0","id":8,"method":"session/set_config_option","params":{"sessionId":"s","configId":"mode","value":"default"}}`, false},
		{"config model", `{"jsonrpc":"2.0","id":9,"method":"session/set_config_option","params":{"sessionId":"s","configId":"model","value":"x"}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := p.FromClient([]byte(tt.line))
			if tt.refused {
				if m := repliedOnly(t, d); m["error"] == nil {
					t.Fatalf("want error reply, got %v", m)
				}
				return
			}
			forwardedOnly(t, d)
		})
	}
}

func TestPermissionRequestRejectedInsideMuzzle(t *testing.T) {
	p := NewLocked(workdir)
	d := p.FromKernel([]byte(`{"jsonrpc":"2.0","id":"k1","method":"session/request_permission","params":{"sessionId":"s",` +
		`"toolCall":{"toolCallId":"t1","title":"cat /etc/passwd","kind":"execute"},` +
		`"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"},{"optionId":"deny","name":"Deny","kind":"reject_once"}]}}`))
	m := repliedOnly(t, d)
	if m["id"] != "k1" {
		t.Errorf("reply id = %v", m["id"])
	}
	if got := path(m, "result", "outcome"); !reflect.DeepEqual(got, map[string]any{"outcome": "selected", "optionId": "deny"}) {
		t.Errorf("outcome = %v", got)
	}
}

func TestPermissionWithoutRejectOptionIsCancelled(t *testing.T) {
	p := NewLocked(workdir)
	m := repliedOnly(t, p.FromKernel([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/request_permission","params":{"sessionId":"s","toolCall":{},"options":[{"optionId":"allow","kind":"allow_once"}]}}`)))
	if got := path(m, "result", "outcome", "outcome"); got != "cancelled" {
		t.Errorf("outcome = %v", got)
	}
}

func TestPromptWithDenialFails(t *testing.T) {
	p := NewLocked(workdir)
	forwardedOnly(t, p.FromClient([]byte(`{"jsonrpc":"2.0","id":10,"method":"session/prompt","params":{"sessionId":"s","prompt":[]}}`)))
	repliedOnly(t, p.FromKernel([]byte(`{"jsonrpc":"2.0","id":"k","method":"session/request_permission","params":{"sessionId":"s","toolCall":{"toolCallId":"t1","title":"curl","kind":"fetch"},"options":[]}}`)))

	m := forwardedOnly(t, p.FromKernel([]byte(`{"jsonrpc":"2.0","id":10,"result":{"stopReason":"end_turn"}}`)))
	if m["result"] != nil || path(m, "error", "code") != float64(CodeDenied) {
		t.Fatalf("want denial error, got %v", m)
	}
	denied := path(m, "error", "data", "deniedActions").([]any)
	if len(denied) != 1 || path(denied[0].(map[string]any), "title") != "curl" {
		t.Fatalf("deniedActions = %v", denied)
	}

	// The next turn starts clean.
	forwardedOnly(t, p.FromClient([]byte(`{"jsonrpc":"2.0","id":11,"method":"session/prompt","params":{"sessionId":"s","prompt":[]}}`)))
	m = forwardedOnly(t, p.FromKernel([]byte(`{"jsonrpc":"2.0","id":11,"result":{"stopReason":"end_turn"}}`)))
	if path(m, "result", "stopReason") != "end_turn" {
		t.Fatalf("clean turn rewritten: %v", m)
	}
}

func TestDenialsAreScopedToTheirSession(t *testing.T) {
	p := NewLocked(workdir)
	forwardedOnly(t, p.FromClient([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"a"}}`)))
	forwardedOnly(t, p.FromClient([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"b"}}`)))
	repliedOnly(t, p.FromKernel([]byte(`{"jsonrpc":"2.0","id":"k","method":"session/request_permission","params":{"sessionId":"b","toolCall":{},"options":[]}}`)))
	if m := forwardedOnly(t, p.FromKernel([]byte(`{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`))); m["error"] != nil {
		t.Fatalf("session a failed for b's denial: %v", m)
	}
	if m := forwardedOnly(t, p.FromKernel([]byte(`{"jsonrpc":"2.0","id":2,"result":{"stopReason":"end_turn"}}`))); m["error"] == nil {
		t.Fatalf("session b not failed: %v", m)
	}
}

func TestClientIORequestsRefused(t *testing.T) {
	p := NewLocked(workdir)
	for _, method := range []string{"fs/read_text_file", "fs/write_text_file", "terminal/create", "_x/unknown"} {
		m := repliedOnly(t, p.FromKernel([]byte(`{"jsonrpc":"2.0","id":5,"method":"`+method+`","params":{}}`)))
		if m["error"] == nil {
			t.Errorf("%s: want error, got %v", method, m)
		}
	}
}

func TestKernelNotificationsPass(t *testing.T) {
	p := NewLocked(workdir)
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk"}}}`
	if d := p.FromKernel([]byte(line)); string(d.Forward) != line || d.Reply != nil {
		t.Fatalf("notification changed: %+v", d)
	}
}

func TestNewSessionInOtherModeIsPinned(t *testing.T) {
	p := NewLocked(workdir)
	forwardedOnly(t, p.FromClient([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{}}`)))
	resp := `{"jsonrpc":"2.0","id":1,"result":{"sessionId":"s1","modes":{"currentModeId":"yolo","availableModes":[]}}}`
	d := p.FromKernel([]byte(resp))
	if string(d.Forward) != resp || d.Reply == nil {
		t.Fatalf("want forward + pin request, got %+v", d)
	}
	req := decode(t, d.Reply)
	if req["method"] != "session/set_mode" || path(req, "params", "modeId") != "default" || path(req, "params", "sessionId") != "s1" {
		t.Fatalf("pin request = %v", req)
	}
	// Its response is consumed.
	ack, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{}})
	if d := p.FromKernel(ack); d.Forward != nil || d.Reply != nil {
		t.Fatalf("muzzle response leaked: %+v", d)
	}
}

func TestLoadedSessionUsesRequestSessionID(t *testing.T) {
	p := NewLocked(workdir)
	forwardedOnly(t, p.FromClient([]byte(`{"jsonrpc":"2.0","id":3,"method":"session/load","params":{"sessionId":"old"}}`)))
	d := p.FromKernel([]byte(`{"jsonrpc":"2.0","id":3,"result":{"modes":{"currentModeId":"auto_edit"}}}`))
	if d.Reply == nil || path(decode(t, d.Reply), "params", "sessionId") != "old" {
		t.Fatalf("want pin for session old, got %+v", d)
	}
}

func TestDefaultModeSessionNotPinned(t *testing.T) {
	p := NewLocked(workdir)
	forwardedOnly(t, p.FromClient([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{}}`)))
	forwardedOnly(t, p.FromKernel([]byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"s1","modes":{"currentModeId":"default"}}}`)))
}

func TestNonJSONPasses(t *testing.T) {
	p := NewLocked(workdir)
	if d := p.FromClient([]byte("garbage")); string(d.Forward) != "garbage" {
		t.Fatalf("got %+v", d)
	}
	if d := p.FromKernel([]byte("garbage")); string(d.Forward) != "garbage" {
		t.Fatalf("got %+v", d)
	}
}
