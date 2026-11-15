package main

import (
	"encoding/json"
	"log/slog"

	"github.com/Arsenikki/agy-acp-muzzle/internal/acp"
	"github.com/Arsenikki/agy-acp-muzzle/internal/relay"
)

// traced wraps a client -> kernel filter with a debug log line per request:
// the method, and for session requests the cwd and session id as received
// from the client (before any policy rewrite). Prompts are not logged.
func traced(log *slog.Logger, next relay.Filter) relay.Filter {
	return func(line []byte) relay.Decision {
		if env, err := acp.Parse(line); err == nil && env.Method != "" {
			var p struct {
				Cwd       string `json:"cwd"`
				SessionID string `json:"sessionId"`
				ModeID    string `json:"modeId"`
			}
			_ = json.Unmarshal(env.Params, &p)
			log.Debug("client "+env.Kind().String(), "method", env.Method, "cwd", p.Cwd, "session", p.SessionID, "mode", p.ModeID)
		}
		if next == nil {
			return relay.Decision{Forward: line}
		}
		return next(line)
	}
}
