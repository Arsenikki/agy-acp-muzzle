package relay

import (
	"bytes"
	"encoding/json"
)

// The kernel describes its models twice: the superseded flat `models` list,
// where every thinking level is its own model ("Gemini 4 Hydrogen (Medium)"),
// and session config options, with one `model` per family plus a separate
// `thinking_level`. Clients such as Paseo prefer the flat list when present
// but also show the thinking option, so a session showed two contradicting
// controls. With HideModelVariants the flat list is dropped, leaving one model
// per family and the thinking level as its own control.

// withoutModelList returns a result object without its `models` field, or
// result unchanged when there is none (or it does not decode).
func withoutModelList(result json.RawMessage) json.RawMessage {
	if !bytes.Contains(result, []byte(`"models"`)) {
		return result
	}
	var r map[string]json.RawMessage
	if json.Unmarshal(result, &r) != nil {
		return result
	}
	if _, ok := r["models"]; !ok {
		return result
	}
	delete(r, "models")
	out, err := json.Marshal(r)
	if err != nil {
		return result
	}
	return out
}

// lineWithoutModelList applies withoutModelList to a response line's result.
func lineWithoutModelList(line []byte) []byte {
	var msg map[string]json.RawMessage
	if json.Unmarshal(line, &msg) != nil || msg["result"] == nil {
		return line
	}
	stripped := withoutModelList(msg["result"])
	if len(stripped) == len(msg["result"]) {
		return line
	}
	msg["result"] = stripped
	out, err := json.Marshal(msg)
	if err != nil {
		return line
	}
	return out
}
