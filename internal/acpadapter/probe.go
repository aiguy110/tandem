package acpadapter

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aiguy110/tandem/internal/acp"
)

// ProbedSession is the portable subset of ACP session/list used by Tandem's
// resume picker.
type ProbedSession struct {
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Title     string `json:"title,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// ProbeSessions starts an agent only long enough to capability-check and call
// session/list. Probe failures intentionally degrade to an unsupported result:
// one broken or older agent must not break the complete resume catalog.
func ProbeSessions(ctx context.Context, launch acp.Config) (supportsList bool, sessions []ProbedSession) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
	}
	launch.Env = overlayEnvironment(launch.Env)
	tr, err := acp.Start(ctx, launch)
	if err != nil {
		return false, nil
	}
	defer tr.Close()
	var init struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			SessionCapabilities struct {
				List json.RawMessage `json:"list"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	if tr.Call(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": true, "writeTextFile": true}, "terminal": true},
		"clientInfo":         map[string]string{"name": "tandem", "version": "go"},
	}, &init) != nil || init.ProtocolVersion != 1 || len(init.AgentCapabilities.SessionCapabilities.List) == 0 || string(init.AgentCapabilities.SessionCapabilities.List) == "null" || string(init.AgentCapabilities.SessionCapabilities.List) == "false" {
		return false, nil
	}
	var result struct {
		Sessions []ProbedSession `json:"sessions"`
	}
	if tr.Call(ctx, "session/list", map[string]any{}, &result) != nil {
		return false, nil
	}
	out := result.Sessions[:0]
	for _, s := range result.Sessions {
		if s.SessionID != "" && s.CWD != "" {
			out = append(out, s)
		}
	}
	return true, out
}

func overlayEnvironment(overlay []string) []string {
	values := map[string]string{}
	for _, entry := range append(os.Environ(), overlay...) {
		if k, v, ok := strings.Cut(entry, "="); ok {
			values[k] = v
		}
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+values[k])
	}
	return out
}
