package herdrmirror

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime/k8s"
)

// stderrExcerptLimit bounds the stderr bytes a failure log line quotes.
const stderrExcerptLimit = 512

// attachArgs is the kubectl-herdr argv that mirrors session name's pod in
// workspace wsID. It names the pod and selector exactly as the k8s runtime
// does, and the agent container's tmux session, and passes no command, so
// the supervisor can only attach and never creates a tmux session.
func (m *Mirror) attachArgs(name, wsID string) []string {
	args := []string{"attach", "-n", m.cfg.Namespace}
	if m.cfg.KubeContext != "" {
		args = append(args, "--context", m.cfg.KubeContext)
	}
	return append(args,
		k8s.SanitizeName(name),
		"-l", "gc-session="+k8s.SanitizeLabel(name),
		"-c", k8s.AgentContainer,
		"--tmux-session", k8s.PodTmuxSession,
		"--herdr-session", m.cfg.HerdrSession,
		"--workspace", wsID,
		"--label", name,
		"--focus=false",
		"--output", "json",
	)
}

// decodeAttach reads the tab and pane ids from kubectl-herdr attach's JSON.
func decodeAttach(out []byte) (mirrorPane, error) {
	var res struct {
		TabID  string `json:"tab_id"`
		PaneID string `json:"pane_id"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return mirrorPane{}, err
	}
	if res.TabID == "" {
		return mirrorPane{}, errors.New("no tab_id")
	}
	return mirrorPane{tabID: res.TabID, paneID: res.PaneID}, nil
}

// excerpt renders a command's stderr for one log line: trimmed, its lines
// joined by spaces, and at most stderrExcerptLimit bytes.
func excerpt(stderr []byte) string {
	s := strings.Join(strings.Fields(string(stderr)), " ")
	if s == "" {
		return "(no stderr)"
	}
	if len(s) > stderrExcerptLimit {
		s = strings.ToValidUTF8(s[:stderrExcerptLimit], "") + "…"
	}
	return s
}
