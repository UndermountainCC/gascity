// Package herdrmirror opens a herdr "mirror tab" for each session a remote
// (k8s) runtime starts, and closes it when the session stops.
//
// The mirror tab is not a gc session. It is a herdr tab that the external
// kubectl-herdr plugin creates with `kubectl-herdr attach`: kubectl-herdr
// runs a supervisor in the tab's pane that attaches to the pod's tmux session
// and reports the in-pod agent's state to herdr. This package owns only the
// lifecycle around it: one attach after a successful remote Start, and one
// close after the remote Stop.
//
// A mirror never decides whether a session starts or stops. Every open runs
// off the Start path under its own total budget, every close runs under one
// total budget, and every mirror failure is logged and never returned. Which
// tab belongs to which session is read from herdr's live listing by label;
// in-process memory is only a hint.
package herdrmirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	// DefaultOpenTimeout is the total budget for one open: the workspace
	// lookup or creation, the tab listing and the attach together.
	DefaultOpenTimeout = 45 * time.Second
	// DefaultCloseTimeout is the total budget for one close: waiting for an
	// in-flight open and every herdr call the close makes, together.
	DefaultCloseTimeout = 10 * time.Second

	attachBinary = "kubectl-herdr"
	herdrBinary  = "herdr"
)

// Runner runs bin with args under ctx and returns its standard output and
// standard error. A non-zero exit returns an error with an ExitCode() int
// method, as *exec.ExitError has. When ctx ends the process is killed.
type Runner func(ctx context.Context, bin string, args ...string) (stdout, stderr []byte, err error)

// Config configures a Mirror.
type Config struct {
	// HerdrSession is the herdr session the mirror tabs go in: the city's
	// own herdr session, the one its herdr runtime uses.
	HerdrSession string
	// Namespace is the Kubernetes namespace of the session pods.
	Namespace string
	// KubeContext is the kubectl context of the session pods; "" omits
	// --context and kubectl-herdr uses the current one.
	KubeContext string
	// LookPath resolves the kubectl-herdr binary; nil selects exec.LookPath.
	LookPath func(string) (string, error)
	// Run runs the mirror's commands; nil selects an exec-based runner.
	Run Runner
	// Log receives the mirror's diagnostics, one line each; nil selects
	// os.Stderr.
	Log io.Writer
	// OpenTimeout bounds one open in total; zero selects DefaultOpenTimeout.
	OpenTimeout time.Duration
	// CloseTimeout bounds one close in total; zero selects
	// DefaultCloseTimeout.
	CloseTimeout time.Duration
	// Spawn runs an open off the caller's path; nil selects `go f()`.
	Spawn func(f func())
}

// mirrorPane is what an attach reported for one session's mirror tab. It is
// a hint for closing the tab's own pane, never proof the tab still exists.
type mirrorPane struct {
	tabID, paneID string
}

// pendingOpen is one session's open that has been scheduled and not yet
// finished.
type pendingOpen struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}
}

var (
	errCanceledByStop = errors.New("canceled by stop")
	errSuperseded     = errors.New("superseded by a later start")
)

// Mirror opens and closes herdr mirror tabs for remote sessions.
type Mirror struct {
	cfg Config

	// wsMu serializes the mirror workspace's find-or-create so concurrent
	// opens never create two workspaces. It is held only across those herdr
	// calls, never across an attach or a close.
	wsMu sync.Mutex

	mu       sync.Mutex              // guards panes, roots and inflight; never held across a command
	panes    map[string]mirrorPane   // session name -> the attach's tab and pane
	roots    map[string]string       // workspace id -> its root tab, closed after the first successful attach
	inflight map[string]*pendingOpen // session name -> its running open
}

// New returns a Mirror for cfg, with defaults filled in for its zero fields.
func New(cfg Config) *Mirror {
	if cfg.LookPath == nil {
		cfg.LookPath = exec.LookPath
	}
	if cfg.Run == nil {
		cfg.Run = execRunner
	}
	if cfg.Log == nil {
		cfg.Log = os.Stderr
	}
	if cfg.OpenTimeout <= 0 {
		cfg.OpenTimeout = DefaultOpenTimeout
	}
	if cfg.CloseTimeout <= 0 {
		cfg.CloseTimeout = DefaultCloseTimeout
	}
	if cfg.Spawn == nil {
		cfg.Spawn = func(f func()) { go f() }
	}
	return &Mirror{
		cfg:      cfg,
		panes:    make(map[string]mirrorPane),
		roots:    make(map[string]string),
		inflight: make(map[string]*pendingOpen),
	}
}

// WorkspaceLabel is the label of the herdr workspace that holds the mirror
// tabs of a namespace's sessions.
func WorkspaceLabel(namespace string) string {
	return "k8s:" + namespace
}

// execRunner is the production Runner. The context kills the child; WaitDelay
// bounds the wait for output pipes a killed child's descendants still hold.
func execRunner(ctx context.Context, bin string, args ...string) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// logf writes one diagnostic line about session name's mirror tab.
func (m *Mirror) logf(name, format string, args ...any) {
	fmt.Fprintf(m.cfg.Log, "gc: hybrid: herdr mirror tab for session %q: %s\n", name, fmt.Sprintf(format, args...)) //nolint:errcheck // best-effort diagnostics
}

// openAsync schedules the open of name's mirror tab and returns at once. The
// open runs on its own context bounded by OpenTimeout, never on the caller's.
// A still-running open for name is canceled, and the new one waits for it
// to finish before it does anything.
func (m *Mirror) openAsync(name string) {
	base, cancel := context.WithCancelCause(context.Background())
	ctx, cancelTimeout := context.WithTimeout(base, m.cfg.OpenTimeout)
	p := &pendingOpen{ctx: ctx, cancel: cancel, done: make(chan struct{})}

	m.mu.Lock()
	prev := m.inflight[name]
	m.inflight[name] = p
	m.mu.Unlock()
	if prev != nil {
		prev.cancel(errSuperseded)
	}

	m.cfg.Spawn(func() {
		defer func() {
			cancelTimeout()
			cancel(nil)
			m.mu.Lock()
			if m.inflight[name] == p {
				delete(m.inflight, name)
			}
			m.mu.Unlock()
			close(p.done)
		}()
		if prev != nil {
			select {
			case <-prev.done:
			case <-ctx.Done():
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		m.open(ctx, name)
	})
}

// open attaches name's mirror tab unless the live listing already has one.
// It reports nothing: every failure is logged.
func (m *Mirror) open(ctx context.Context, name string) {
	bin, err := m.cfg.LookPath(attachBinary)
	if err != nil {
		fmt.Fprintf(m.cfg.Log, "gc: hybrid: %s not found on PATH; skipping herdr mirror tab for session %q\n", attachBinary, name) //nolint:errcheck // best-effort diagnostics
		return
	}
	wsID, err := m.ensureWorkspace(ctx)
	if err != nil {
		m.logOpenError(ctx, name, err)
		return
	}
	tabs, err := m.listTabs(ctx, wsID)
	if err != nil {
		m.logOpenError(ctx, name, err)
		return
	}
	for _, tb := range tabs {
		if tb.Label == name {
			return // already mirrored
		}
	}
	stdout, stderr, err := m.cfg.Run(ctx, bin, m.attachArgs(name, wsID)...)
	if err != nil {
		m.logAttachError(ctx, name, stderr, err)
		return
	}
	pane, err := decodeAttach(stdout)
	if err != nil {
		m.logf(name, "%s attach: bad JSON: %v", attachBinary, err)
		return
	}
	m.mu.Lock()
	m.panes[name] = pane
	root, pendingRoot := m.roots[wsID]
	delete(m.roots, wsID)
	m.mu.Unlock()
	if pendingRoot {
		if err := m.closeTab(ctx, root); err != nil && !isNotFound(err) {
			m.logf(name, "closing the mirror workspace's initial tab %s: %v", root, err)
		}
	}
}

// logOpenError logs a herdr failure during an open, naming a timeout or a
// cancellation for what it is.
func (m *Mirror) logOpenError(ctx context.Context, name string, err error) {
	if reason := m.openEnded(ctx); reason != "" {
		m.logf(name, "%s", reason)
		return
	}
	m.logf(name, "%v", err)
}

// logAttachError logs a failed kubectl-herdr attach: a timeout, a
// cancellation, its exit status with a bounded stderr excerpt, or the error
// that kept it from running.
func (m *Mirror) logAttachError(ctx context.Context, name string, stderr []byte, err error) {
	if reason := m.openEnded(ctx); reason != "" {
		m.logf(name, "%s attach %s", attachBinary, reason)
		return
	}
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) && exit.ExitCode() > 0 {
		m.logf(name, "%s attach exit %d: %s", attachBinary, exit.ExitCode(), excerpt(stderr))
		return
	}
	m.logf(name, "%s attach: %v", attachBinary, err)
}

// openEnded says why an open's context ended, or "" while it is live.
func (m *Mirror) openEnded(ctx context.Context) string {
	if ctx.Err() == nil {
		return ""
	}
	cause := context.Cause(ctx)
	if errors.Is(cause, context.DeadlineExceeded) {
		return fmt.Sprintf("timed out after %s", m.cfg.OpenTimeout)
	}
	return cause.Error()
}

// closeAfterStop closes name's mirror tab after the leaf's Stop returned
// stopErr. The tab is kept when stopErr says the session may still be
// running. Otherwise an in-flight open for name is canceled and waited
// for, and every tab labeled name in the mirror workspace is closed: by the
// remembered pane when the live listing confirms its tab, else by tab id.
// One CloseTimeout bounds all of it; failures are logged, never returned.
func (m *Mirror) closeAfterStop(name string, stopErr error) {
	if stopErr != nil && !runtime.IsSessionGone(stopErr) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.CloseTimeout)
	defer cancel()

	m.mu.Lock()
	p := m.inflight[name]
	m.mu.Unlock()
	if p != nil {
		p.cancel(errCanceledByStop)
		select {
		case <-p.done:
		case <-ctx.Done():
			m.logf(name, "close timed out after %s", m.cfg.CloseTimeout)
			return
		}
	}

	m.mu.Lock()
	hint, hinted := m.panes[name]
	delete(m.panes, name)
	m.mu.Unlock()

	if err := m.closeTabs(ctx, name, hint, hinted); err != nil {
		if ctx.Err() != nil {
			m.logf(name, "close timed out after %s", m.cfg.CloseTimeout)
			return
		}
		m.logf(name, "%v", err)
	}
}

// closeTabs closes every tab labeled name in the mirror workspace. A missing
// workspace or tab is not an error.
func (m *Mirror) closeTabs(ctx context.Context, name string, hint mirrorPane, hinted bool) error {
	wsID, err := m.findWorkspace(ctx)
	if err != nil || wsID == "" {
		return err
	}
	tabs, err := m.listTabs(ctx, wsID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	var errs []error
	for _, tb := range tabs {
		if tb.Label != name {
			continue
		}
		var err error
		if hinted && hint.paneID != "" && tb.TabID == hint.tabID {
			err = m.closePane(ctx, hint.paneID)
		} else {
			err = m.closeTab(ctx, tb.TabID)
		}
		if err != nil && !isNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
