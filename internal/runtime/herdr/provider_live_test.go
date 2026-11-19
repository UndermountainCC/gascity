package herdr

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/herdr/herdrtest"
)

// TestProviderLive drives the herdr Provider against a real herdr binary in an
// isolated session. Opt-in live tier: see requireLiveHerdr.
func TestProviderLive(t *testing.T) {
	requireLiveHerdr(t)

	p := New("gctest-live", t.TempDir(), t.TempDir(), 0, 0)
	_ = p.Stop("smoke") // clear any leftover from a crashed prior run
	t.Cleanup(func() { _ = p.Stop("smoke"); _ = p.TeardownServer() })

	ctx := context.Background()
	cfg := runtime.Config{
		WorkDir: t.TempDir(),
		Command: liveTickCommand,
		Env:     map[string]string{"GC_SESSION_ID": "gctest-live-session"},
	}
	if err := p.Start(ctx, "smoke", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Start must persist GC_SESSION_ID to the meta sidecar (tmux parity):
	// ProcessAlive's session-scoped tree-walk widening has nothing to read
	// without it.
	if v, err := p.GetMeta("smoke", "GC_SESSION_ID"); err != nil || v != "gctest-live-session" {
		t.Errorf("GetMeta(GC_SESSION_ID) = %q, %v; want %q, nil", v, err, "gctest-live-session")
	}

	if !p.IsRunning("smoke") {
		t.Error("IsRunning = false after Start")
	}
	if names, err := p.ListRunning("smo"); err != nil || len(names) != 1 || names[0] != "smoke" {
		t.Errorf("ListRunning(smo) = %v, %v; want [smoke]", names, err)
	}

	// Peek the current screen ("visible") — wait for output to render.
	var got string
	for i := 0; i < 20; i++ {
		time.Sleep(300 * time.Millisecond)
		got, _ = p.Peek("smoke", 10)
		if strings.Contains(got, liveTickOutput) {
			break
		}
	}
	if !strings.Contains(got, liveTickOutput) {
		t.Errorf("Peek did not capture %q; got %q", liveTickOutput, got)
	}

	// ProcessAlive: nil → true; matching name → true; bogus → false.
	if !p.ProcessAlive("smoke", nil) {
		t.Error("ProcessAlive(nil) = false")
	}
	if !p.ProcessAlive("smoke", liveProcessNames) {
		t.Errorf("ProcessAlive(%v) = false", liveProcessNames)
	}
	if p.ProcessAlive("smoke", []string{"definitely-not-a-real-process"}) {
		t.Error("ProcessAlive([bogus]) = true")
	}

	// Metadata sidecar roundtrip.
	if err := p.SetMeta("smoke", "drain", "1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if v, err := p.GetMeta("smoke", "drain"); err != nil || v != "1" {
		t.Errorf("GetMeta(drain) = %q, %v; want 1", v, err)
	}
	if v, err := p.GetMeta("smoke", "absent"); err != nil || v != "" {
		t.Errorf("GetMeta(absent) = %q, %v; want empty,nil", v, err)
	}

	// Stop → no longer running.
	if err := p.Stop("smoke"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for i := 0; i < 10 && p.IsRunning("smoke"); i++ {
		time.Sleep(200 * time.Millisecond)
	}
	if p.IsRunning("smoke") {
		t.Error("IsRunning = true after Stop")
	}
}

// TestProviderLivePaneGroup pins GC_HERDR_PANE_GROUP against a real herdr:
// two grouped sessions share one tab as two panes, and stopping both leaves
// no group tab behind (herdr closes a tab whose last pane closes).
func TestProviderLivePaneGroup(t *testing.T) {
	requireLiveHerdr(t)

	const town, group = "gctest-live-group", "gctest-group"
	names := []string{"grp-a", "grp-b"}
	p := New(town, t.TempDir(), t.TempDir(), 0, 0)
	for _, n := range names {
		_ = p.Stop(n) // clear any leftover from a crashed prior run
	}
	t.Cleanup(func() {
		for _, n := range names {
			_ = p.Stop(n)
		}
		_ = p.TeardownServer()
	})

	ctx := context.Background()
	for _, n := range names {
		cfg := runtime.Config{
			WorkDir: t.TempDir(),
			Command: liveTickCommand,
			Env:     map[string]string{"GC_SESSION_ID": "gctest-" + n, envPaneGroup: group},
		}
		if err := p.Start(ctx, n, cfg); err != nil {
			t.Fatalf("Start %s: %v", n, err)
		}
	}
	groupPanes := func() (tabs int, panes []paneEntry) {
		t.Helper()
		ws, err := p.c.findWorkspace(ctx, town)
		if err != nil || ws == "" {
			return 0, nil
		}
		all, err := p.c.listTabs(ctx, ws)
		if err != nil {
			t.Fatalf("tab list: %v", err)
		}
		var groupTab string
		for _, tb := range all {
			if tb.Label == group {
				tabs++
				groupTab = tb.TabID
			}
		}
		listed, err := p.c.listPanes(ctx, ws)
		if err != nil {
			t.Fatalf("pane list: %v", err)
		}
		for _, pn := range listed {
			if pn.TabID == groupTab {
				panes = append(panes, pn)
			}
		}
		return tabs, panes
	}
	if tabs, panes := groupPanes(); tabs != 1 || len(panes) != 2 || panes[0].Label == panes[1].Label {
		t.Fatalf("group tabs = %d, panes = %+v; want one tab holding grp-a and grp-b", tabs, panes)
	}
	for _, n := range names {
		if !p.IsRunning(n) {
			t.Errorf("IsRunning(%s) = false after Start", n)
		}
		if err := p.Stop(n); err != nil {
			t.Fatalf("Stop %s: %v", n, err)
		}
	}
	herdrtest.Poll(t, "the group tab closing", 5*time.Second, func() (bool, string) {
		tabs, panes := groupPanes()
		return tabs == 0, fmt.Sprintf("%d group tabs, panes %+v", tabs, panes)
	})
}
