package herdrmirror

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// herdrError is an error answer from the herdr CLI: the "error" member of
// its JSON envelope.
type herdrError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *herdrError) Error() string { return e.Code + ": " + e.Message }

// envelope is the herdr CLI's JSON answer.
type envelope struct {
	Result json.RawMessage `json:"result"`
	Error  *herdrError     `json:"error"`
}

// envelopeError returns the herdr error an output carries, or nil when the
// output is not a herdr error envelope.
func envelopeError(out []byte) *herdrError {
	var env envelope
	if json.Unmarshal(out, &env) != nil || env.Error == nil || env.Error.Code == "" {
		return nil
	}
	return env.Error
}

// isNotFound reports whether err is herdr saying its target does not exist
// (tab_not_found, pane_not_found, workspace_not_found, ...).
func isNotFound(err error) bool {
	he, ok := err.(*herdrError) //nolint:errorlint // herdr calls return the envelope error itself, never wrapped
	return ok && he != nil && strings.HasSuffix(he.Code, "_not_found")
}

// herdr runs one herdr CLI verb in the mirror's herdr session and decodes the
// envelope's result into out when out is non-nil.
func (m *Mirror) herdr(ctx context.Context, out any, args ...string) error {
	argv := append([]string{"--session", m.cfg.HerdrSession}, args...)
	stdout, stderr, err := m.cfg.Run(ctx, herdrBinary, argv...)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("herdr %s: %w", strings.Join(args[:2], " "), ctxErr)
	}
	if err != nil {
		if he := envelopeError(stderr); he != nil {
			return he
		}
		if he := envelopeError(stdout); he != nil {
			return he
		}
		return fmt.Errorf("herdr %s: %w: %s", strings.Join(args[:2], " "), err, excerpt(stderr))
	}
	var env envelope
	if err := json.Unmarshal(stdout, &env); err != nil {
		return fmt.Errorf("herdr %s: decoding answer: %w", strings.Join(args[:2], " "), err)
	}
	if env.Error != nil {
		return env.Error
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("herdr %s: decoding result: %w", strings.Join(args[:2], " "), err)
	}
	return nil
}

// workspaceInfo and tabInfo are the members of herdr's listings the mirror
// reads.
type workspaceInfo struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
}

type tabInfo struct {
	TabID string `json:"tab_id"`
	Label string `json:"label"`
}

// findWorkspace returns the id of the mirror workspace, or "" when the herdr
// session has none.
func (m *Mirror) findWorkspace(ctx context.Context) (string, error) {
	var res struct {
		Workspaces []workspaceInfo `json:"workspaces"`
	}
	if err := m.herdr(ctx, &res, "workspace", "list"); err != nil {
		return "", err
	}
	label := WorkspaceLabel(m.cfg.Namespace)
	for _, ws := range res.Workspaces {
		if ws.Label == label {
			return ws.WorkspaceID, nil
		}
	}
	return "", nil
}

// ensureWorkspace returns the mirror workspace's id, creating the workspace
// when the herdr session has none. A created workspace's root tab is
// remembered, to be closed once a mirror tab is in it.
func (m *Mirror) ensureWorkspace(ctx context.Context) (string, error) {
	m.wsMu.Lock()
	defer m.wsMu.Unlock()
	id, err := m.findWorkspace(ctx)
	if err != nil || id != "" {
		return id, err
	}
	var res struct {
		Workspace workspaceInfo `json:"workspace"`
		Tab       tabInfo       `json:"tab"`
	}
	if err := m.herdr(ctx, &res, "workspace", "create", "--label", WorkspaceLabel(m.cfg.Namespace), "--no-focus"); err != nil {
		return "", err
	}
	if res.Workspace.WorkspaceID == "" {
		return "", fmt.Errorf("herdr workspace create: no workspace id in the answer")
	}
	if res.Tab.TabID != "" {
		m.mu.Lock()
		m.roots[res.Workspace.WorkspaceID] = res.Tab.TabID
		m.mu.Unlock()
	}
	return res.Workspace.WorkspaceID, nil
}

// listTabs lists the tabs of workspace wsID.
func (m *Mirror) listTabs(ctx context.Context, wsID string) ([]tabInfo, error) {
	var res struct {
		Tabs []tabInfo `json:"tabs"`
	}
	if err := m.herdr(ctx, &res, "tab", "list", "--workspace", wsID); err != nil {
		return nil, err
	}
	return res.Tabs, nil
}

// closeTab closes tab tabID and every pane in it.
func (m *Mirror) closeTab(ctx context.Context, tabID string) error {
	return m.herdr(ctx, nil, "tab", "close", tabID)
}

// closePane closes pane paneID; herdr closes its tab with its last pane.
func (m *Mirror) closePane(ctx context.Context, paneID string) error {
	return m.herdr(ctx, nil, "pane", "close", paneID)
}
