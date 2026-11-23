package herdrmirror

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

const testSession = "city"

// exitError is a non-zero exit as the production runner reports one: an
// error with an ExitCode method, as *exec.ExitError has.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e exitError) ExitCode() int { return e.code }

type fakeTab struct{ id, label, pane string }

type fakeWorkspace struct {
	id, label string
	tabs      []fakeTab
}

type recordedCall struct {
	bin  string
	args []string
	ctx  context.Context
}

// fakeHerdr is an in-memory herdr session plus a kubectl-herdr attach that
// creates a tab in it. Every argv it receives is recorded and checked against
// the mirror's allowlist when the test ends.
type fakeHerdr struct {
	mu         sync.Mutex
	calls      []recordedCall
	workspaces []*fakeWorkspace
	next       int
	// attach overrides the default attach, which creates a tab labeled by
	// --label in --workspace and answers its ids as JSON.
	attach func(ctx context.Context, f *fakeHerdr, args []string) ([]byte, []byte, error)
	// fail answers a herdr verb ("workspace list", "tab close", ...) with a
	// herdr error envelope carrying this code.
	fail map[string]string
	// block makes a herdr verb wait for its context to end.
	block map[string]bool
}

func newFakeHerdr(t *testing.T) *fakeHerdr {
	t.Helper()
	f := &fakeHerdr{fail: map[string]string{}, block: map[string]bool{}}
	t.Cleanup(func() { assertAllowedArgv(t, f.allCalls()) })
	return f
}

func (f *fakeHerdr) allCalls() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// argvs renders every recorded call as "bin arg arg ...".
func (f *fakeHerdr) argvs() []string {
	var out []string
	for _, c := range f.allCalls() {
		out = append(out, strings.Join(append([]string{c.bin}, c.args...), " "))
	}
	return out
}

// herdrVerbs returns the herdr verbs called, in order, with their operands.
func (f *fakeHerdr) herdrVerbs() []string {
	var out []string
	for _, c := range f.allCalls() {
		if c.bin == herdrBinary {
			out = append(out, strings.Join(c.args[2:], " "))
		}
	}
	return out
}

func (f *fakeHerdr) attachCalls() []recordedCall {
	var out []recordedCall
	for _, c := range f.allCalls() {
		if c.bin != herdrBinary {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeHerdr) addWorkspace(label string) *fakeWorkspace {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addWorkspaceLocked(label)
}

func (f *fakeHerdr) addWorkspaceLocked(label string) *fakeWorkspace {
	f.next++
	ws := &fakeWorkspace{id: fmt.Sprintf("w%d", f.next), label: label}
	f.workspaces = append(f.workspaces, ws)
	return ws
}

func (f *fakeHerdr) addTab(ws *fakeWorkspace, label string) fakeTab {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addTabLocked(ws, label)
}

func (f *fakeHerdr) addTabLocked(ws *fakeWorkspace, label string) fakeTab {
	f.next++
	tb := fakeTab{id: fmt.Sprintf("%s:t%d", ws.id, f.next), label: label, pane: fmt.Sprintf("%s:p%d", ws.id, f.next)}
	ws.tabs = append(ws.tabs, tb)
	return tb
}

// workspaceByIDLocked is workspaceByID for callers not holding f.mu.
func (f *fakeHerdr) workspaceByIDLocked(id string) *fakeWorkspace {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.workspaceByID(id)
}

func (f *fakeHerdr) workspaceByID(id string) *fakeWorkspace {
	for _, ws := range f.workspaces {
		if ws.id == id {
			return ws
		}
	}
	return nil
}

// tabLabels lists the tab labels of the workspace labeled label.
func (f *fakeHerdr) tabLabels(label string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ws := range f.workspaces {
		if ws.label == label {
			var out []string
			for _, tb := range ws.tabs {
				out = append(out, tb.label)
			}
			return out
		}
	}
	return nil
}

// answer is a herdr success envelope carrying result.
func answer(result any) []byte {
	b, err := json.Marshal(map[string]any{"id": "cli", "result": result})
	if err != nil {
		panic(err)
	}
	return b
}

// herdrFailure answers as herdr does on an error: the envelope on stderr and
// exit status 1.
func herdrFailure(code string) (stderr []byte, err error) {
	b, _ := json.Marshal(map[string]any{"id": "cli", "error": map[string]string{"code": code, "message": code}})
	return b, exitError{code: 1}
}

func (f *fakeHerdr) run(ctx context.Context, bin string, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{bin: bin, args: slices.Clone(args), ctx: ctx})
	f.mu.Unlock()
	if bin != herdrBinary {
		if f.attach != nil {
			return f.attach(ctx, f, args)
		}
		return f.defaultAttach(args)
	}
	if len(args) < 4 || args[0] != "--session" {
		return nil, []byte("bad argv"), exitError{code: 2}
	}
	verb := args[2] + " " + args[3]
	if f.block[verb] {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	if code, bad := f.fail[verb]; bad {
		stderr, err := herdrFailure(code)
		return nil, stderr, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch verb {
	case "workspace list":
		var list []map[string]string
		for _, ws := range f.workspaces {
			list = append(list, map[string]string{"workspace_id": ws.id, "label": ws.label})
		}
		return answer(map[string]any{"type": "workspace_list", "workspaces": list}), nil, nil
	case "workspace create":
		label := flagValue(args, "--label")
		ws := f.addWorkspaceLocked(label)
		root := f.addTabLocked(ws, "1")
		return answer(map[string]any{
			"type":      "workspace_created",
			"workspace": map[string]string{"workspace_id": ws.id, "label": label},
			"tab":       map[string]string{"tab_id": root.id, "label": root.label},
			"root_pane": map[string]string{"pane_id": root.pane},
		}), nil, nil
	case "tab list":
		ws := f.workspaceByID(flagValue(args, "--workspace"))
		if ws == nil {
			stderr, err := herdrFailure("workspace_not_found")
			return nil, stderr, err
		}
		var tabs []map[string]string
		for _, tb := range ws.tabs {
			tabs = append(tabs, map[string]string{"tab_id": tb.id, "label": tb.label})
		}
		return answer(map[string]any{"type": "tab_list", "tabs": tabs}), nil, nil
	case "tab close", "pane close":
		target := args[4]
		for _, ws := range f.workspaces {
			for i, tb := range ws.tabs {
				if (verb == "tab close" && tb.id == target) || (verb == "pane close" && tb.pane == target) {
					ws.tabs = slices.Delete(ws.tabs, i, i+1)
					return answer(map[string]string{"type": "ok"}), nil, nil
				}
			}
		}
		if verb == "tab close" {
			stderr, err := herdrFailure("tab_not_found")
			return nil, stderr, err
		}
		stderr, err := herdrFailure("pane_not_found")
		return nil, stderr, err
	}
	return nil, []byte("unknown verb " + verb), exitError{code: 2}
}

func (f *fakeHerdr) defaultAttach(args []string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ws := f.workspaceByID(flagValue(args, "--workspace"))
	if ws == nil {
		return nil, []byte("error: workspace not found"), exitError{code: 1}
	}
	tb := f.addTabLocked(ws, flagValue(args, "--label"))
	b, _ := json.Marshal(map[string]string{"tab_id": tb.id, "pane_id": tb.pane, "herdr_session": testSession, "namespace": "gc", "pod": "p"})
	return b, nil, nil
}

func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// assertAllowedArgv is AC-7: the mirror issues only kubectl-herdr attach and
// the herdr workspace/tab/pane lookups and closes, never a state bridge call,
// an exec, or kubectl itself.
func assertAllowedArgv(t *testing.T, calls []recordedCall) {
	t.Helper()
	allowed := []string{"workspace list", "workspace create", "tab list", "tab close", "pane close"}
	for _, c := range calls {
		argv := strings.Join(append([]string{c.bin}, c.args...), " ")
		for _, banned := range []string{"report-agent", "report-agent-session", "release-agent", "exec"} {
			if slices.Contains(c.args, banned) {
				t.Errorf("forbidden argv %q contains %q", argv, banned)
			}
		}
		switch {
		case c.bin == herdrBinary:
			if len(c.args) < 4 || c.args[0] != "--session" || c.args[1] != testSession || !slices.Contains(allowed, c.args[2]+" "+c.args[3]) {
				t.Errorf("herdr argv %q is not an allowed mirror call", argv)
			}
		case strings.HasSuffix(c.bin, attachBinary):
			if len(c.args) == 0 || c.args[0] != "attach" {
				t.Errorf("kubectl-herdr argv %q is not an attach", argv)
			}
		default:
			t.Errorf("argv %q runs a binary the mirror must not run", argv)
		}
	}
}

// syncBuffer is a log writer safe for the open goroutine and the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := strings.TrimSpace(b.buf.String())
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// harness is a Leg over a k8s-profile fake leaf with a fake herdr.
type harness struct {
	leaf    *runtime.Fake
	herdr   *fakeHerdr
	log     *syncBuffer
	mirror  *Mirror
	leg     *Leg
	spawned []func()
}

// harnessOpt adjusts the mirror's Config before the harness builds it.
type harnessOpt func(h *harness, c *Config)

func withContext(kctx string) harnessOpt {
	return func(_ *harness, c *Config) { c.KubeContext = kctx }
}

// captureSpawn makes the harness capture each open instead of running it.
func captureSpawn(h *harness, c *Config) {
	c.Spawn = func(f func()) { h.spawned = append(h.spawned, f) }
}

func newHarness(t *testing.T, opts ...harnessOpt) *harness {
	t.Helper()
	h := &harness{leaf: runtime.NewFake(), herdr: newFakeHerdr(t), log: &syncBuffer{}}
	cfg := Config{
		HerdrSession: testSession,
		Namespace:    "gc",
		LookPath:     func(string) (string, error) { return "/usr/local/bin/" + attachBinary, nil },
		Run:          h.herdr.run,
		Log:          h.log,
		Spawn:        func(f func()) { f() },
	}
	for _, o := range opts {
		o(h, &cfg)
	}
	h.mirror = New(cfg)
	leg, err := Wrap(runtime.NewFakeProfile(runtime.ProfileK8s, runtime.FullFake{Fake: h.leaf}), h.mirror)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	h.leg = leg
	return h
}

func (h *harness) start(t *testing.T, name string) error {
	t.Helper()
	return h.leg.Start(context.Background(), name, runtime.Config{})
}

func (h *harness) runSpawned() {
	spawned := h.spawned
	h.spawned = nil
	for _, f := range spawned {
		f()
	}
}

var errBoom = errors.New("boom")
