package policy

import (
	"reflect"
	"testing"
)

func TestOpenAddsDisabledTools(t *testing.T) {
	p := &Open{DisabledTools: []string{"run_workflow", "run_command"}}
	m := forwardedOnly(t, p.FromClient([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/w","_meta":{"antigravity":{"enabledTools":["view_file","run_command"],"disabledTools":["run_command","schedule"]},"x":1}}}`)))
	got := path(m, "params", "_meta", "antigravity", "disabledTools")
	if !reflect.DeepEqual(got, []any{"run_command", "schedule", "run_workflow"}) {
		t.Fatalf("disabledTools = %v", got)
	}
	if !reflect.DeepEqual(path(m, "params", "_meta", "antigravity", "enabledTools"), []any{"view_file", "run_command"}) || path(m, "params", "_meta", "x") != float64(1) {
		t.Fatalf("other meta changed: %v", m)
	}
	if path(m, "params", "cwd") != "/w" {
		t.Fatalf("cwd changed: %v", m)
	}
}

func TestOpenWithoutToolsPassesThrough(t *testing.T) {
	p := &Open{}
	line := `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/w"}}`
	if d := p.FromClient([]byte(line)); string(d.Forward) != line {
		t.Fatalf("changed: %s", d.Forward)
	}
}

func TestOpenLeavesOtherRequests(t *testing.T) {
	p := &Open{DisabledTools: []string{"run_workflow"}}
	line := `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"s"}}`
	if d := p.FromClient([]byte(line)); string(d.Forward) != line {
		t.Fatalf("changed: %s", d.Forward)
	}
}
