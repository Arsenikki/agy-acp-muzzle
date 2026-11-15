package acp

import (
	"encoding/json"
	"testing"
)

func TestParseKind(t *testing.T) {
	tests := []struct {
		name string
		line string
		want Kind
	}{
		{"request", `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{}}`, KindRequest},
		{"string id request", `{"jsonrpc":"2.0","id":"a","method":"initialize"}`, KindRequest},
		{"notification", `{"jsonrpc":"2.0","method":"session/update","params":{}}`, KindNotification},
		{"result response", `{"jsonrpc":"2.0","id":1,"result":{}}`, KindResponse},
		{"error response", `{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"x"}}`, KindResponse},
		{"null id is a notification", `{"jsonrpc":"2.0","id":null,"method":"x"}`, KindNotification},
		{"wrong version", `{"jsonrpc":"1.0","id":1,"method":"x"}`, KindInvalid},
		{"response without payload", `{"jsonrpc":"2.0","id":1}`, KindInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := Parse([]byte(tt.line))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := e.Kind(); got != tt.want {
				t.Fatalf("Kind() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseRejectsNonJSON(t *testing.T) {
	if _, err := Parse([]byte("not json")); err == nil {
		t.Fatal("Parse accepted a non-JSON line")
	}
}

func TestErrorResponse(t *testing.T) {
	line, err := ErrorResponse(json.RawMessage(`7`), RPCError{Code: CodeMethodNotFound, Message: "nope"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := Parse(line)
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind() != KindResponse || string(e.ID) != "7" {
		t.Fatalf("got %s", line)
	}
}

func TestResultResponse(t *testing.T) {
	line, err := ResultResponse(json.RawMessage(`"x"`), map[string]string{"outcome": "cancelled"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"jsonrpc":"2.0","id":"x","result":{"outcome":"cancelled"}}`
	if string(line) != want {
		t.Fatalf("got %s, want %s", line, want)
	}
}
