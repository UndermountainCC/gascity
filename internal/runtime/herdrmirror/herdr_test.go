package herdrmirror

import (
	"context"
	"errors"
	"testing"
)

func TestHerdrCallDecodesResultAndErrors(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if id, err := h.mirror.findWorkspace(ctx); err != nil || id != "" {
		t.Fatalf("findWorkspace on an empty session = (%q, %v), want (\"\", nil)", id, err)
	}
	ws := h.herdr.addWorkspace(WorkspaceLabel("gc"))
	if id, err := h.mirror.findWorkspace(ctx); err != nil || id != ws.id {
		t.Fatalf("findWorkspace = (%q, %v), want (%q, nil)", id, err, ws.id)
	}

	h.herdr.fail["tab close"] = "tab_not_found"
	err := h.mirror.closeTab(ctx, "w1:t9")
	if err == nil || !isNotFound(err) {
		t.Fatalf("closeTab of a gone tab = %v, want a not-found herdr error", err)
	}
	var he *herdrError
	if !errors.As(err, &he) || he.Code != "tab_not_found" {
		t.Fatalf("closeTab error = %#v, want herdr code tab_not_found", err)
	}

	h.herdr.fail["tab list"] = "server_busy"
	if _, err := h.mirror.listTabs(ctx, ws.id); err == nil || isNotFound(err) {
		t.Fatalf("listTabs = %v, want a herdr error that is not not-found", err)
	}
}

func TestIsNotFound(t *testing.T) {
	for code, want := range map[string]bool{
		"tab_not_found":       true,
		"pane_not_found":      true,
		"workspace_not_found": true,
		"agent_not_found":     true,
		"server_busy":         false,
		"":                    false,
	} {
		if got := isNotFound(&herdrError{Code: code}); got != want {
			t.Errorf("isNotFound(%q) = %t, want %t", code, got, want)
		}
	}
	if isNotFound(errBoom) || isNotFound(nil) {
		t.Error("isNotFound of a non-herdr error must be false")
	}
}

func TestDecodeEnvelopeFromStderrOrStdout(t *testing.T) {
	if he := envelopeError([]byte(`{"error":{"code":"pane_not_found","message":"pane x not found"},"id":"cli:pane:close"}`)); he == nil || he.Code != "pane_not_found" {
		t.Fatalf("envelopeError = %#v, want pane_not_found", he)
	}
	if he := envelopeError([]byte("Error: connection refused")); he != nil {
		t.Fatalf("envelopeError of plain text = %#v, want nil", he)
	}
}
