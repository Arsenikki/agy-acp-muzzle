package policy

import (
	"github.com/Arsenikki/agy-acp-muzzle/internal/acp"
	"github.com/Arsenikki/agy-acp-muzzle/internal/relay"
)

// Open is the policy for trusted sessions: everything passes, except that
// DisabledTools are added to every session's built-in tool filter (on top of
// whatever the client asks for). Use it to take tools away from a provider
// as a whole, e.g. run_workflow, which makes the kernel write a skill file
// into the workspace.
type Open struct {
	DisabledTools []string
}

// FromClient filters client -> kernel lines.
func (p *Open) FromClient(line []byte) relay.Decision {
	if len(p.DisabledTools) == 0 {
		return relay.Decision{Forward: line}
	}
	env, err := acp.Parse(line)
	if err != nil || env.Kind() != acp.KindRequest {
		return relay.Decision{Forward: line}
	}
	switch env.Method {
	case methodSessionNew, methodSessionLoad, methodSessionResume:
		return rewrite(line, env, p.disableTools)
	}
	return relay.Decision{Forward: line}
}

// disableTools merges DisabledTools into _meta.antigravity.disabledTools. The
// kernel subtracts disabledTools from a client's enabledTools allowlist, so a
// client cannot re-enable them.
func (p *Open) disableTools(params map[string]any) {
	meta, _ := params["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	ag, _ := meta["antigravity"].(map[string]any)
	if ag == nil {
		ag = map[string]any{}
	}
	seen := map[string]bool{}
	var out []any
	existing, _ := ag["disabledTools"].([]any)
	for _, v := range existing {
		if s, ok := v.(string); ok && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range p.DisabledTools {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	ag["disabledTools"] = out
	meta["antigravity"] = ag
	params["_meta"] = meta
}
