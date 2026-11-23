package herdrmirror

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func wantOneLogLine(t *testing.T, h *harness, contains ...string) {
	t.Helper()
	lines := h.log.lines()
	if len(lines) != 1 {
		t.Fatalf("log lines = %q, want exactly one", lines)
	}
	for _, c := range contains {
		if !strings.Contains(lines[0], c) {
			t.Errorf("log line %q does not contain %q", lines[0], c)
		}
	}
}

func wantNoLog(t *testing.T, h *harness) {
	t.Helper()
	if lines := h.log.lines(); len(lines) != 0 {
		t.Fatalf("log lines = %q, want none", lines)
	}
}

// REQ-007 / plan review B-1: Start hands the open off and returns at once.
// The open runs on a context that is not Start's, so the caller canceling
// its start context after Start returns does not cancel the attach.
func TestStartSchedulesTheOpenOffTheStartPath(t *testing.T) {
	h := newHarness(t, captureSpawn)
	startCtx, cancel := context.WithCancel(context.Background())
	if err := h.leg.Start(startCtx, "pod-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start = %v, want the leaf's nil", err)
	}
	if calls := h.herdr.allCalls(); len(calls) != 0 {
		t.Fatalf("runner calls during Start = %q, want none", h.herdr.argvs())
	}
	if len(h.spawned) != 1 {
		t.Fatalf("spawned opens = %d, want 1", len(h.spawned))
	}
	cancel() // the caller cancels startCtx as soon as Start returns
	var attachCtxErr error
	h.herdr.attach = func(ctx context.Context, f *fakeHerdr, args []string) ([]byte, []byte, error) {
		attachCtxErr = ctx.Err()
		if _, has := ctx.Deadline(); !has {
			t.Error("the open's context has no deadline: one total OpenTimeout must bound it")
		}
		return f.defaultAttach(args)
	}
	h.runSpawned()
	if len(h.herdr.attachCalls()) != 1 {
		t.Fatalf("attach calls = %d, want 1", len(h.herdr.attachCalls()))
	}
	if attachCtxErr != nil {
		t.Fatalf("attach ran on a canceled context (%v): the open must not derive from the Start context", attachCtxErr)
	}
	wantNoLog(t, h)
}

// AC-2: a failed leaf Start opens nothing and returns the error unchanged.
func TestStartErrorOpensNoTab(t *testing.T) {
	h := newHarness(t, captureSpawn)
	h.leaf.StartErrors["pod-worker"] = errBoom
	if err := h.start(t, "pod-worker"); !errors.Is(err, errBoom) {
		t.Fatalf("Start = %v, want the leaf's error", err)
	}
	if len(h.spawned) != 0 || len(h.herdr.allCalls()) != 0 {
		t.Fatalf("a failed Start scheduled %d opens and ran %q", len(h.spawned), h.herdr.argvs())
	}
}

// AC-3 (REQ-004): a tab already labeled with the session in the mirror
// workspace means no second attach.
func TestOpenIsIdempotentByLiveListing(t *testing.T) {
	h := newHarness(t)
	ws := h.herdr.addWorkspace(WorkspaceLabel("gc"))
	h.herdr.addTab(ws, "pod-worker")
	if err := h.start(t, "pod-worker"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if n := len(h.herdr.attachCalls()); n != 0 {
		t.Fatalf("attach calls = %d, want 0 when the tab exists", n)
	}
	// A second Start of a session whose first open succeeded attaches once.
	if err := h.start(t, "pod-other"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.leaf.Stop("pod-other"); err != nil { // the leaf alone, so the tab stays
		t.Fatalf("leaf Stop: %v", err)
	}
	if err := h.start(t, "pod-other"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if n := len(h.herdr.attachCalls()); n != 1 {
		t.Fatalf("attach calls = %d, want 1 across two Starts", n)
	}
	if got := h.herdr.tabLabels(WorkspaceLabel("gc")); !slices.Equal(got, []string{"pod-worker", "pod-other"}) {
		t.Fatalf("mirror tabs = %q, want one per session", got)
	}
	wantNoLog(t, h)
}

// The mirror workspace is created once per city, its root tab closed after
// the first successful attach, and reused by later sessions.
func TestWorkspaceCreatedOnceAndRootTabClosedAfterFirstSuccess(t *testing.T) {
	h := newHarness(t)
	failFirst := true
	h.herdr.attach = func(_ context.Context, f *fakeHerdr, args []string) ([]byte, []byte, error) {
		if failFirst {
			failFirst = false
			return nil, []byte("error: pod not found"), exitError{code: 1}
		}
		return f.defaultAttach(args)
	}
	if err := h.start(t, "pod-a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The failed first attach leaves the root tab pending, not leaked.
	if got := h.herdr.tabLabels(WorkspaceLabel("gc")); !slices.Equal(got, []string{"1"}) {
		t.Fatalf("tabs after a failed first attach = %q, want the root tab", got)
	}
	if err := h.start(t, "pod-b"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := h.herdr.tabLabels(WorkspaceLabel("gc")); !slices.Equal(got, []string{"pod-b"}) {
		t.Fatalf("tabs after the first success = %q, want the root tab closed", got)
	}
	if err := h.start(t, "pod-c"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := h.herdr.tabLabels(WorkspaceLabel("gc")); !slices.Equal(got, []string{"pod-b", "pod-c"}) {
		t.Fatalf("tabs = %q, want both sessions in one workspace", got)
	}
	var creates, rootCloses int
	for _, v := range h.herdr.herdrVerbs() {
		if strings.HasPrefix(v, "workspace create") {
			creates++
			if v != "workspace create --label k8s:gc --no-focus" {
				t.Errorf("workspace create = %q", v)
			}
		}
		if strings.HasPrefix(v, "tab close") {
			rootCloses++
		}
	}
	if creates != 1 || rootCloses != 1 {
		t.Fatalf("workspace creates = %d, root tab closes = %d, want 1 and 1 (verbs %q)", creates, rootCloses, h.herdr.herdrVerbs())
	}
	if n := len(h.log.lines()); n != 1 {
		t.Fatalf("log lines = %q, want one for the failed attach", h.log.lines())
	}
}

// AC-5 (REQ-006): without kubectl-herdr on PATH the session starts, nothing
// runs, and exactly one line names the session and the binary.
func TestMissingBinarySkipsTheMirrorWithOneLine(t *testing.T) {
	h := newHarness(t, func(_ *harness, c *Config) {
		c.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	})
	if err := h.start(t, "pod-worker"); err != nil {
		t.Fatalf("Start = %v, want nil", err)
	}
	if calls := h.herdr.allCalls(); len(calls) != 0 {
		t.Fatalf("runner calls = %q, want none", h.herdr.argvs())
	}
	wantOneLogLine(t, h, `"pod-worker"`, "kubectl-herdr", "not found on PATH", "skipping")
}

// AC-6 (REQ-007): every attach failure is one log line naming the session,
// and Start returns the leaf's result.
func TestAttachFailuresAreLoggedNotReturned(t *testing.T) {
	for _, tc := range []struct {
		name   string
		attach func(ctx context.Context, f *fakeHerdr, args []string) ([]byte, []byte, error)
		want   []string
	}{
		{
			name: "exit 1",
			attach: func(context.Context, *fakeHerdr, []string) ([]byte, []byte, error) {
				return nil, []byte("error: no pod matches gc-session=pod-worker\n"), exitError{code: 1}
			},
			want: []string{"kubectl-herdr attach exit 1:", "no pod matches"},
		},
		{
			name: "exit 2",
			attach: func(context.Context, *fakeHerdr, []string) ([]byte, []byte, error) {
				return nil, []byte("usage: kubectl herdr attach [flags]\n"), exitError{code: 2}
			},
			want: []string{"kubectl-herdr attach exit 2:", "usage:"},
		},
		{
			name: "bad json",
			attach: func(context.Context, *fakeHerdr, []string) ([]byte, []byte, error) {
				return []byte("attached!\n"), nil, nil
			},
			want: []string{"kubectl-herdr attach: bad JSON"},
		},
		{
			name: "binary did not run",
			attach: func(context.Context, *fakeHerdr, []string) ([]byte, []byte, error) {
				return nil, nil, errors.New("fork/exec: permission denied")
			},
			want: []string{"kubectl-herdr attach:", "permission denied"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.herdr.attach = tc.attach
			if err := h.start(t, "pod-worker"); err != nil {
				t.Fatalf("Start = %v, want the leaf's nil", err)
			}
			wantOneLogLine(t, h, append([]string{`gc: hybrid: herdr mirror tab for session "pod-worker"`}, tc.want...)...)
			if err := h.leg.Stop("pod-worker"); err != nil {
				t.Fatalf("Stop = %v, want the leaf's nil", err)
			}
		})
	}
}

func TestAttachStderrExcerptIsBounded(t *testing.T) {
	h := newHarness(t)
	h.herdr.attach = func(context.Context, *fakeHerdr, []string) ([]byte, []byte, error) {
		return nil, []byte(strings.Repeat("e", 4096)), exitError{code: 1}
	}
	if err := h.start(t, "pod-worker"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	lines := h.log.lines()
	if len(lines) != 1 || strings.Count(lines[0], "e") > stderrExcerptLimit+20 {
		t.Fatalf("log line is not bounded: %d bytes", len(strings.Join(lines, "")))
	}
}

// AC-6: an attach that outlives the open's total budget is killed and logged
// as a timeout; Start returned long before.
func TestAttachTimeoutIsLogged(t *testing.T) {
	h := newHarness(t, func(_ *harness, c *Config) { c.OpenTimeout = 20 * time.Millisecond })
	h.herdr.attach = func(ctx context.Context, _ *fakeHerdr, _ []string) ([]byte, []byte, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	if err := h.start(t, "pod-worker"); err != nil {
		t.Fatalf("Start = %v, want nil", err)
	}
	wantOneLogLine(t, h, `"pod-worker"`, "kubectl-herdr attach timed out after 20ms")
}

func TestOpenWorkspaceErrorIsLogged(t *testing.T) {
	h := newHarness(t)
	h.herdr.fail["workspace list"] = "server_busy"
	if err := h.start(t, "pod-worker"); err != nil {
		t.Fatalf("Start = %v, want nil", err)
	}
	if n := len(h.herdr.attachCalls()); n != 0 {
		t.Fatalf("attach calls = %d, want 0 without a workspace", n)
	}
	wantOneLogLine(t, h, `"pod-worker"`, "server_busy")
}

// AC-4: close by the remembered pane when the live listing confirms its tab.
func TestStopClosesTheHintedPaneWhenTheListingConfirmsIt(t *testing.T) {
	h := newHarness(t)
	if err := h.start(t, "pod-worker"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pane := h.mirror.panes["pod-worker"].paneID
	if err := h.leg.Stop("pod-worker"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	verbs := h.herdr.herdrVerbs()
	if last := verbs[len(verbs)-1]; last != "pane close "+pane {
		t.Fatalf("last herdr call = %q, want pane close %s (verbs %q)", last, pane, verbs)
	}
	if _, has := h.mirror.panes["pod-worker"]; has {
		t.Fatal("the hint outlived the close")
	}
	wantNoLog(t, h)
}

// AC-4: after a controller restart (a fresh mirror, no hint) Stop finds the
// tab by label and closes the tab.
func TestStopClosesByLabelAfterRestart(t *testing.T) {
	h := newHarness(t)
	ws := h.herdr.addWorkspace(WorkspaceLabel("gc"))
	tb := h.herdr.addTab(ws, "pod-worker")
	h.herdr.addTab(ws, "pod-other")
	if err := h.leg.Stop("pod-worker"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	verbs := h.herdr.herdrVerbs()
	want := []string{"workspace list", "tab list --workspace " + ws.id, "tab close " + tb.id}
	if !slices.Equal(verbs, want) {
		t.Fatalf("herdr calls = %q, want %q", verbs, want)
	}
	if got := h.herdr.tabLabels(WorkspaceLabel("gc")); !slices.Equal(got, []string{"pod-other"}) {
		t.Fatalf("tabs = %q, want only the other session's", got)
	}
	wantNoLog(t, h)
}

// AC-4: the tab is already gone (the user or the supervisor closed it):
// no close call and nothing logged, though a hint remains.
func TestStopWithTheTabAlreadyGoneIsSilent(t *testing.T) {
	h := newHarness(t)
	if err := h.start(t, "pod-worker"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ws := h.herdr.workspaces[0]
	ws.tabs = nil // closed by hand
	before := len(h.herdr.herdrVerbs())
	if err := h.leg.Stop("pod-worker"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, v := range h.herdr.herdrVerbs()[before:] {
		if strings.HasPrefix(v, "pane close") || strings.HasPrefix(v, "tab close") {
			t.Fatalf("close call %q with the tab already gone", v)
		}
	}
	wantNoLog(t, h)
}

// A whole mirror workspace already gone (its last tab closed) is silent too,
// and close never creates the workspace.
func TestStopWithoutAMirrorWorkspaceIsSilent(t *testing.T) {
	h := newHarness(t)
	if err := h.leg.Stop("pod-worker"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if verbs := h.herdr.herdrVerbs(); !slices.Equal(verbs, []string{"workspace list"}) {
		t.Fatalf("herdr calls = %q, want only the lookup", verbs)
	}
	wantNoLog(t, h)
}

// AC-4 / OQ-6: a hint whose tab id is not the listed tab's is never closed
// by pane id (herdr may have reused it); the listed tab is closed instead.
func TestStopNeverClosesAHintedPaneTheListingDoesNotConfirm(t *testing.T) {
	h := newHarness(t)
	if err := h.start(t, "pod-worker"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stale := h.mirror.panes["pod-worker"]
	ws := h.herdr.workspaces[0]
	ws.tabs = nil
	fresh := h.herdr.addTab(ws, "pod-worker") // a different tab now carries the label
	if err := h.leg.Stop("pod-worker"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, v := range h.herdr.herdrVerbs() {
		if v == "pane close "+stale.paneID {
			t.Fatalf("closed the unconfirmed hinted pane %s", stale.paneID)
		}
	}
	verbs := h.herdr.herdrVerbs()
	if last := verbs[len(verbs)-1]; last != "tab close "+fresh.id {
		t.Fatalf("last herdr call = %q, want tab close %s", last, fresh.id)
	}
}

// AC-4 / OQ-11 / plan review T-2: the tab is kept when the leaf's Stop fails
// with anything but a gone session, and Stop returns the leaf's error.
func TestStopKeepsTheTabUnlessTheSessionIsGone(t *testing.T) {
	notFound := fmt.Errorf("deleting pod: %w", runtime.ErrSessionNotFound)
	for _, tc := range []struct {
		name      string
		stopErr   error
		wantClose bool
	}{
		{name: "nil", stopErr: nil, wantClose: true},
		{name: "wraps not found", stopErr: notFound, wantClose: true},
		{name: "other error", stopErr: errBoom, wantClose: false},
		{name: "not found joined with not confirmed dead", stopErr: errors.Join(runtime.ErrSessionNotFound, runtime.ErrProcessNotConfirmedDead), wantClose: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if err := h.start(t, "pod-worker"); err != nil {
				t.Fatalf("Start: %v", err)
			}
			before := len(h.herdr.allCalls())
			if tc.stopErr != nil {
				h.leaf.StopErrors["pod-worker"] = tc.stopErr
			}
			err := h.leg.Stop("pod-worker")
			if err != tc.stopErr { //nolint:errorlint // the leaf's error must come back unchanged, not merely match
				t.Fatalf("Stop = %v, want the leaf's %v unchanged", err, tc.stopErr)
			}
			closed := len(h.herdr.tabLabels(WorkspaceLabel("gc"))) == 0
			if closed != tc.wantClose {
				t.Fatalf("tab closed = %t, want %t (verbs %q)", closed, tc.wantClose, h.herdr.herdrVerbs())
			}
			if !tc.wantClose {
				if after := len(h.herdr.allCalls()); after != before {
					t.Fatalf("a kept tab still ran herdr: %q", h.herdr.argvs()[before:])
				}
				if _, has := h.mirror.panes["pod-worker"]; !has {
					t.Fatal("a kept tab lost its hint")
				}
			}
		})
	}
}

// Plan review B-1: a Stop during an in-flight open cancels it, waits for it,
// and then closes the tab the killed attach had already created, by label.
func TestStopCancelsAnInFlightOpenAndClosesItsTab(t *testing.T) {
	h := newHarness(t, captureSpawn)
	attaching := make(chan struct{})
	var attachCause error
	h.herdr.attach = func(ctx context.Context, f *fakeHerdr, args []string) ([]byte, []byte, error) {
		ws := f.workspaceByIDLocked(flagValue(args, "--workspace"))
		f.addTab(ws, flagValue(args, "--label")) // the tab exists before the attach is killed
		close(attaching)
		<-ctx.Done()
		attachCause = context.Cause(ctx)
		return nil, nil, ctx.Err()
	}
	if err := h.start(t, "pod-worker"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	opened := make(chan struct{})
	go func() {
		defer close(opened)
		h.runSpawned()
	}()
	<-attaching
	if err := h.leg.Stop("pod-worker"); err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
	select {
	case <-opened:
	default:
		t.Fatal("Stop returned before the in-flight open finished")
	}
	if !errors.Is(attachCause, errCanceledByStop) {
		t.Fatalf("attach context cause = %v, want canceled by stop", attachCause)
	}
	// The workspace's root tab stays pending: no attach has succeeded yet.
	if got := h.herdr.tabLabels(WorkspaceLabel("gc")); !slices.Equal(got, []string{"1"}) {
		t.Fatalf("tabs after Stop = %q, want the attach's tab closed and the root pending", got)
	}
	wantOneLogLine(t, h, `"pod-worker"`, "canceled by stop")
}

// A Stop whose Start's open has not begun cancels it before it runs a thing.
func TestStopBeforeAScheduledOpenRunsCancelsIt(t *testing.T) {
	h := newHarness(t, captureSpawn)
	if err := h.start(t, "pod-worker"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- h.leg.Stop("pod-worker") }()
	// Stop waits for the open to finish; run it now, as the scheduler would.
	waitForCancel(t, h, "pod-worker")
	h.runSpawned()
	if err := <-stopped; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if n := len(h.herdr.attachCalls()); n != 0 {
		t.Fatalf("attach calls = %d, want 0 for an open canceled before it ran", n)
	}
}

// waitForCancel blocks until name's in-flight open has been canceled.
func waitForCancel(t *testing.T, h *harness, name string) {
	t.Helper()
	h.mirror.mu.Lock()
	p := h.mirror.inflight[name]
	h.mirror.mu.Unlock()
	if p == nil {
		t.Fatalf("no in-flight open for %q", name)
	}
	<-p.ctx.Done()
}

// Plan review T-1: one CloseTimeout bounds every herdr call of a close, and
// its expiry is logged, never returned.
func TestCloseIsBoundedByOneTotalBudget(t *testing.T) {
	h := newHarness(t, func(_ *harness, c *Config) { c.CloseTimeout = 20 * time.Millisecond })
	h.herdr.block["workspace list"] = true
	done := make(chan error, 1)
	go func() { done <- h.leg.Stop("pod-worker") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop = %v, want the leaf's nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return within its close budget")
	}
	wantOneLogLine(t, h, `"pod-worker"`, "close timed out after 20ms")
}

// A close's herdr errors other than not-found are logged with the session.
func TestCloseHerdrErrorIsLogged(t *testing.T) {
	h := newHarness(t)
	if err := h.start(t, "pod-worker"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.herdr.fail["pane close"] = "server_busy"
	if err := h.leg.Stop("pod-worker"); err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
	wantOneLogLine(t, h, `"pod-worker"`, "server_busy")
}

// A not-found answer to the close itself (the supervisor closed the pane
// between the listing and the close) is silent.
func TestCloseNotFoundIsSilent(t *testing.T) {
	h := newHarness(t)
	if err := h.start(t, "pod-worker"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.herdr.fail["pane close"] = "pane_not_found"
	if err := h.leg.Stop("pod-worker"); err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
	wantNoLog(t, h)
}

// Close never looks kubectl-herdr up: it needs only herdr.
func TestCloseDoesNotNeedKubectlHerdr(t *testing.T) {
	looked := 0
	h := newHarness(t, func(_ *harness, c *Config) {
		c.LookPath = func(string) (string, error) { looked++; return "", exec.ErrNotFound }
	})
	ws := h.herdr.addWorkspace(WorkspaceLabel("gc"))
	h.herdr.addTab(ws, "pod-worker")
	if err := h.leg.Stop("pod-worker"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if looked != 0 {
		t.Fatalf("LookPath calls on Stop = %d, want 0", looked)
	}
	if got := h.herdr.tabLabels(WorkspaceLabel("gc")); len(got) != 0 {
		t.Fatalf("tabs = %q, want closed", got)
	}
}

// A second open for a session supersedes the first: the first is canceled
// and the second waits for it before it lists or attaches.
func TestASecondOpenSupersedesTheFirst(t *testing.T) {
	h := newHarness(t, captureSpawn)
	if err := h.start(t, "pod-worker"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.mirror.mu.Lock()
	first := h.mirror.inflight["pod-worker"]
	h.mirror.mu.Unlock()
	h.mirror.openAsync("pod-worker")
	if !errors.Is(context.Cause(first.ctx), errSuperseded) {
		t.Fatalf("first open's cause = %v, want superseded", context.Cause(first.ctx))
	}
	h.runSpawned()
	if n := len(h.herdr.attachCalls()); n != 1 {
		t.Fatalf("attach calls = %d, want 1", n)
	}
	h.mirror.mu.Lock()
	left := len(h.mirror.inflight)
	h.mirror.mu.Unlock()
	if left != 0 {
		t.Fatalf("in-flight opens left = %d, want 0", left)
	}
}

func TestWorkspaceLabel(t *testing.T) {
	if got := WorkspaceLabel("gc"); got != "k8s:gc" {
		t.Fatalf("WorkspaceLabel = %q, want k8s:gc", got)
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	m := New(Config{HerdrSession: "s", Namespace: "gc"})
	if m.cfg.OpenTimeout != DefaultOpenTimeout || m.cfg.CloseTimeout != DefaultCloseTimeout {
		t.Fatalf("timeouts = %s/%s, want %s/%s", m.cfg.OpenTimeout, m.cfg.CloseTimeout, DefaultOpenTimeout, DefaultCloseTimeout)
	}
	if m.cfg.LookPath == nil || m.cfg.Run == nil || m.cfg.Spawn == nil || m.cfg.Log == nil {
		t.Fatal("New left a nil default")
	}
}
