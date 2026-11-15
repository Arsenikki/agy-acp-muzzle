// Package acp holds the minimal Agent Client Protocol (ACP) framing the muzzle
// needs: one JSON-RPC 2.0 message per line (NDJSON) on stdio, classified as a
// request, a response or a notification.
//
// Messages are kept as raw bytes and only the envelope is decoded, so a message
// the muzzle does not rewrite is forwarded byte-for-byte.
package acp

import (
	"bytes"
	"encoding/json"
)

// Kind classifies a JSON-RPC message.
type Kind int

const (
	// KindInvalid is a line that is not a JSON-RPC 2.0 object.
	KindInvalid Kind = iota
	// KindRequest has a method and an id and expects a response.
	KindRequest
	// KindNotification has a method and no id.
	KindNotification
	// KindResponse has an id and a result or an error, and no method.
	KindResponse
)

func (k Kind) String() string {
	switch k {
	case KindRequest:
		return "request"
	case KindNotification:
		return "notification"
	case KindResponse:
		return "response"
	default:
		return "invalid"
	}
}

// Envelope is the decoded JSON-RPC envelope of one message. Params, Result and
// Error stay raw so callers decode only what they inspect.
type Envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// Kind reports what sort of message the envelope is.
func (e *Envelope) Kind() Kind {
	if e.JSONRPC != "2.0" {
		return KindInvalid
	}
	hasID := len(e.ID) > 0 && !bytes.Equal(e.ID, []byte("null"))
	switch {
	case e.Method != "" && hasID:
		return KindRequest
	case e.Method != "":
		return KindNotification
	case hasID && (len(e.Result) > 0 || len(e.Error) > 0):
		return KindResponse
	default:
		return KindInvalid
	}
}

// Parse decodes the envelope of one NDJSON line. A line that is not a JSON
// object yields an error; callers in pass-through mode forward it unchanged.
func Parse(line []byte) (*Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(line, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Standard JSON-RPC error codes used by the muzzle.
const (
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInternalError  = -32603
)

// ErrorResponse builds a JSON-RPC error response line (without the trailing
// newline) answering the request with the given id.
func ErrorResponse(id json.RawMessage, rpcErr RPCError) ([]byte, error) {
	return json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   RPCError        `json:"error"`
	}{"2.0", id, rpcErr})
}

// ResultResponse builds a JSON-RPC success response line (without the trailing
// newline) answering the request with the given id.
func ResultResponse(id json.RawMessage, result any) ([]byte, error) {
	return json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{"2.0", id, result})
}
