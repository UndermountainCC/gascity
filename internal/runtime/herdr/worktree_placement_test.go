package herdr

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The tests below drive Provider.start against a fake herdr that models
// herdr 0.9.5's worktree workspaces: `worktree list --cwd` answers with the
// worktrees.json the test writes, and `worktree open` opens workspace w2 under
// the source workspace it is given. Start places a session whose work dir is a
// linked git worktree in that worktree workspace, and every other session in a
// tab of its rig workspace, as before.
//
// Several cases capture os.Stderr, so no test in this file runs in parallel.

// fakeWorktreeHerdr is a stand-in herdr CLI with worktree support. Each
// invocation appends its argv (minus the --session pair) to calls.log in
// state. Files in state drive it:
//
//   - worktrees.json: the whole `worktree list` reply
//   - unsupported: every `worktree` verb fails as an older herdr does
//   - open_error: `worktree open` prints the file's contents on stderr and exits 1
//   - open_ws: the worktree workspace w2 is open, so `worktree open` reports
//     already_open with w2's active tab
//   - rig_ws: `workspace list` reports workspace w1 with the file's contents as label
//   - tabs_<ws>: the `tab list --workspace <ws>` reply
//   - ws_gone_<ws>: the next `tab list --workspace <ws>` fails with
//     workspace_not_found, once
//   - tab_create_error_<ws>: `tab create --workspace <ws>` fails
//   - busy: the agent pane runs a foreground process
//   - panes_<ws>: the `pane list --workspace <ws>` reply
//   - split_unsupported, rename_unsupported: `pane split` or `pane rename`
//     fails as an older herdr does
//   - split_gone: the next `pane split` fails with pane_not_found, once
//   - record_tabs: `tab create --workspace <ws>` rewrites tabs_<ws> and
//     panes_<ws> to hold just the tab it created and its root pane
//
// `pane split <ws>:<pane>` answers with the new pane <ws>:p8.
type fakeWorktreeHerdr struct {
	bin   string
	state string
}

func newFakeWorktreeHerdr(t *testing.T) *fakeWorktreeHerdr {
	t.Helper()
	f := &fakeWorktreeHerdr{state: t.TempDir()}
	if goruntime.GOOS == "windows" {
		f.bin = newGoFakeHerdr(t, fakeHerdrSpec{Mode: "worktree", State: f.state})
		return f
	}
	f.bin = filepath.Join(t.TempDir(), "herdr")
	script := `#!/bin/sh
STATE='` + f.state + `'
shift 2 # drop --session <name>
printf '%s\n' "$*" >> "$STATE/calls.log"
if [ "$1" = worktree ] && [ -e "$STATE/unsupported" ]; then
	echo "unknown command: worktree" >&2
	exit 2
fi
case "$1 $2" in
"worktree list") cat "$STATE/worktrees.json" ;;
"worktree open")
	if [ -e "$STATE/open_error" ]; then cat "$STATE/open_error" >&2; exit 1; fi
	if [ -e "$STATE/open_ws" ]; then
		printf '%s' '{"result":{"type":"worktree_opened","already_open":true,"workspace":{"workspace_id":"w2"},"tab":{"tab_id":"w2:t-active"},"root_pane":{"pane_id":"w2:p-active"}}}'
	else
		: > "$STATE/open_ws"
		printf '%s' '{"result":{"type":"worktree_opened","already_open":false,"workspace":{"workspace_id":"w2"},"tab":{"tab_id":"w2:t1"},"root_pane":{"pane_id":"w2:p1"}}}'
	fi ;;
"workspace list")
	if [ -e "$STATE/rig_ws" ]; then
		printf '{"result":{"workspaces":[{"workspace_id":"w1","label":"%s"}]}}' "$(cat "$STATE/rig_ws")"
	else
		printf '%s' '{"result":{"workspaces":[]}}'
	fi ;;
"workspace create") printf '%s' '{"result":{"workspace":{"workspace_id":"w1"},"tab":{"tab_id":"w1:t1"},"root_pane":{"pane_id":"w1:p1"}}}' ;;
"tab list")
	if [ -e "$STATE/ws_gone_$4" ]; then
		rm -f "$STATE/ws_gone_$4"
		echo '{"error":{"code":"workspace_not_found","message":"workspace not found"}}' >&2
		exit 1
	elif [ -e "$STATE/tabs_$4" ]; then
		cat "$STATE/tabs_$4"
	else
		printf '%s' '{"result":{"tabs":[]}}'
	fi ;;
"tab create")
	if [ -e "$STATE/tab_create_error_$4" ]; then
		echo '{"error":{"code":"workspace_not_found","message":"workspace not found"}}' >&2
		exit 1
	fi
	if [ -e "$STATE/record_tabs" ]; then
		printf '{"result":{"tabs":[{"tab_id":"%s:t9","label":"%s"}]}}' "$4" "$6" > "$STATE/tabs_$4"
		printf '{"result":{"panes":[{"pane_id":"%s:p9","tab_id":"%s:t9"}]}}' "$4" "$4" > "$STATE/panes_$4"
	fi
	printf '{"result":{"tab":{"tab_id":"%s:t9"},"root_pane":{"pane_id":"%s:p9"}}}' "$4" "$4" ;;
"pane list")
	if [ -e "$STATE/panes_$4" ]; then cat "$STATE/panes_$4"; else printf '%s' '{"result":{"panes":[]}}'; fi ;;
"pane split")
	if [ -e "$STATE/split_unsupported" ]; then echo "unknown command: split" >&2; exit 2; fi
	if [ -e "$STATE/split_gone" ]; then
		rm -f "$STATE/split_gone"
		echo '{"error":{"code":"pane_not_found","message":"pane not found"}}' >&2
		exit 1
	fi
	printf '{"result":{"pane":{"pane_id":"%s:p8"}}}' "${3%%:*}" ;;
"pane rename")
	if [ -e "$STATE/rename_unsupported" ]; then echo "unknown command: rename" >&2; exit 2; fi
	printf '%s' '{"result":{}}' ;;
"agent start") printf '{"result":{"agent":{"name":"%s","pane_id":"%s"}}}' "$3" "$7" ;;
"agent get") printf '%s' '{"error":{"code":"agent_not_found","message":"agent target not found"}}' ;;
"agent list") printf '%s' '{"result":{"agents":[]}}' ;;
"pane process-info")
	if [ -e "$STATE/busy" ]; then
		printf '%s' '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[{"pid":4243,"name":"claude"}]}}}'
	else
		printf '%s' '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[]}}}'
	fi ;;
*) printf '%s' '{"result":{}}' ;;
esac
`
	if err := os.WriteFile(f.bin, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake herdr: %v", err)
	}
	return f
}

// set writes the state file flag with contents.
func (f *fakeWorktreeHerdr) set(t *testing.T, flag, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.state, flag), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// calls returns the fake's invocation log, one line per call.
func (f *fakeWorktreeHerdr) calls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.state, "calls.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading fake herdr log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// listWorktrees writes the `worktree list` reply: the primary checkout at repo
// plus entries.
func (f *fakeWorktreeHerdr) listWorktrees(t *testing.T, repo string, entries ...worktreeEntry) {
	t.Helper()
	all := append([]worktreeEntry{{Path: repo}}, entries...)
	reply := map[string]any{"result": map[string]any{
		"type":       "worktree_list",
		"source":     map[string]any{"repo_root": repo, "source_checkout_path": repo},
		"worktrees":  all,
		"repo_label": filepath.Base(repo),
	}}
	b, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	f.set(t, "worktrees.json", string(b))
}

// worktreeSessionName is the runtime name of the session these tests start:
// rig "myrig", tab "chat".
const worktreeSessionName = "myrig--city__chat"

func newWorktreeProvider(t *testing.T, f *fakeWorktreeHerdr) *Provider {
	t.Helper()
	p := New("testwt", t.TempDir(), t.TempDir(), 0, 0)
	p.c.bin = f.bin
	return p
}

// linkedWorktreeLayout makes a primary checkout (a .git directory) with a
// linked worktree under it (a .git file), as `git worktree add` leaves them,
// and returns both paths.
func linkedWorktreeLayout(t *testing.T) (repo, wt string) {
	t.Helper()
	repo = t.TempDir()
	wt = filepath.Join(repo, "worktrees", "chat")
	if err := os.MkdirAll(filepath.Join(repo, ".git", "worktrees", "chat"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	gitdir := "gitdir: " + filepath.Join(repo, ".git", "worktrees", "chat") + "\n"
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte(gitdir), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo, wt
}

// worktreeStartConfig is a kind-launch config whose env must reach the
// agent's pane.
func worktreeStartConfig(workDir string) runtime.Config {
	return runtime.Config{
		WorkDir: workDir,
		Command: "claude",
		Env:     map[string]string{"GC_SESSION_ID": "sess-1", "GC_INSTANCE_TOKEN": "tok-1"},
	}
}

// captureStderr runs fn with os.Stderr redirected to a file and returns what
// fn wrote there.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	out, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = out
	defer func() { os.Stderr = saved }()
	fn()
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
	return string(b)
}

func callIndex(lines []string, prefix string) int {
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

func callCount(lines []string, prefix string) int {
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

// TestWorktreePlacementOpensUnderRigWorkspace pins AC-1: a session in a linked
// worktree opens it as a herdr worktree workspace under the rig workspace,
// labeled with the tab label, without focus or trust, and no tab lands in the
// rig workspace.
func TestWorktreePlacementOpensUnderRigWorkspace(t *testing.T) {
	repo, wt := linkedWorktreeLayout(t)
	f := newFakeWorktreeHerdr(t)
	f.set(t, "rig_ws", "myrig")
	f.listWorktrees(t, repo, worktreeEntry{Path: wt, IsLinkedWorktree: true})
	p := newWorktreeProvider(t, f)

	if err := p.start(context.Background(), worktreeSessionName, worktreeStartConfig(wt)); err != nil {
		t.Fatalf("start: %v", err)
	}
	calls := f.calls(t)
	log := strings.Join(calls, "\n")
	if callIndex(calls, "worktree list --cwd "+wt) < 0 {
		t.Errorf("no worktree list for the work dir; log:\n%s", log)
	}
	if callIndex(calls, "worktree open --workspace w1 --path "+wt+" --label chat --no-focus") < 0 {
		t.Errorf("no worktree open under the rig workspace; log:\n%s", log)
	}
	for _, banned := range []string{"--trust-repository", "--focus ", "--branch", "worktree create", "worktree remove"} {
		if strings.Contains(log+" ", banned) {
			t.Errorf("log carries %q; log:\n%s", banned, log)
		}
	}
	if callIndex(calls, "tab create --workspace w1") >= 0 || callIndex(calls, "workspace create") >= 0 {
		t.Errorf("the agent landed in the rig workspace; log:\n%s", log)
	}
}

// TestWorktreePlacementAgentPaneEnvAndBinding pins AC-2: the agent's pane is a
// fresh tab in the worktree workspace with the session's cwd and every env
// entry, the binding names that workspace, tab and pane, and the root tab
// `worktree open` spawned is closed only after the agent tab exists (herdr
// removes a worktree workspace when its last tab closes).
func TestWorktreePlacementAgentPaneEnvAndBinding(t *testing.T) {
	repo, wt := linkedWorktreeLayout(t)
	f := newFakeWorktreeHerdr(t)
	f.set(t, "rig_ws", "myrig")
	f.listWorktrees(t, repo, worktreeEntry{Path: wt, IsLinkedWorktree: true})
	p := newWorktreeProvider(t, f)
	cfg := worktreeStartConfig(wt)

	if err := p.start(context.Background(), worktreeSessionName, cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	calls := f.calls(t)
	log := strings.Join(calls, "\n")
	createIdx := callIndex(calls, "tab create --workspace w2 --label chat --cwd "+wt)
	if createIdx < 0 {
		t.Fatalf("no agent tab in the worktree workspace with the work dir as cwd; log:\n%s", log)
	}
	for k, v := range cfg.Env {
		if !strings.Contains(calls[createIdx], "--env "+k+"="+v) {
			t.Errorf("agent tab create lacks --env %s=%s: %s", k, v, calls[createIdx])
		}
	}
	closeIdx := callIndex(calls, "tab close w2:t1")
	if closeIdx < 0 || closeIdx < createIdx {
		t.Errorf("root tab must close after the agent tab is created; log:\n%s", log)
	}
	if idx := callIndex(calls, "agent start "+herdrAgentName(worktreeSessionName)+" "); idx < 0 || !strings.Contains(calls[idx], "--pane w2:p9") {
		t.Errorf("agent did not launch into the worktree pane; log:\n%s", log)
	}
	for key, want := range map[string]string{metaBoundWorkspace: "w2", metaBoundTab: "w2:t9", metaBoundPane: "w2:p9"} {
		if got, _ := p.GetMeta(worktreeSessionName, key); got != want {
			t.Errorf("binding %s = %q, want %q", key, got, want)
		}
	}
	if got, _ := p.GetMeta(worktreeSessionName, "GC_SESSION_ID"); got != "sess-1" {
		t.Errorf("meta GC_SESSION_ID = %q, want sess-1", got)
	}
}

// TestWorktreePlacementCreatesMissingRigWorkspace pins AC-3: with no rig
// workspace yet, Start creates it at the source checkout (no agent env, no
// focus) before opening the worktree under the new id.
func TestWorktreePlacementCreatesMissingRigWorkspace(t *testing.T) {
	repo, wt := linkedWorktreeLayout(t)
	f := newFakeWorktreeHerdr(t)
	f.listWorktrees(t, repo, worktreeEntry{Path: wt, IsLinkedWorktree: true})
	p := newWorktreeProvider(t, f)

	if err := p.start(context.Background(), worktreeSessionName, worktreeStartConfig(wt)); err != nil {
		t.Fatalf("start: %v", err)
	}
	calls := f.calls(t)
	log := strings.Join(calls, "\n")
	createIdx := callIndex(calls, "workspace create --label myrig --no-focus --cwd "+repo)
	if createIdx < 0 {
		t.Fatalf("rig workspace not created at the source checkout; log:\n%s", log)
	}
	if strings.Contains(calls[createIdx], "--env") {
		t.Errorf("rig workspace carries the agent's env: %s", calls[createIdx])
	}
	if openIdx := callIndex(calls, "worktree open --workspace w1 "); openIdx < createIdx {
		t.Errorf("worktree open must follow the rig workspace create and use its id; log:\n%s", log)
	}
	if callIndex(calls, "tab create --workspace w2") < 0 {
		t.Errorf("no agent tab in the worktree workspace; log:\n%s", log)
	}
}

// TestWorktreePlacementReusesOpenWorkspace pins AC-4: a restart whose worktree
// already has an open workspace reuses it, creates one agent tab there, and
// closes the stale same-label tab only after that.
func TestWorktreePlacementReusesOpenWorkspace(t *testing.T) {
	repo, wt := linkedWorktreeLayout(t)
	f := newFakeWorktreeHerdr(t)
	f.set(t, "rig_ws", "myrig")
	f.set(t, "open_ws", "")
	f.set(t, "tabs_w2", `{"result":{"tabs":[{"tab_id":"w2:t-old","label":"chat"},{"tab_id":"w2:t-other","label":"shell"}]}}`)
	f.listWorktrees(t, repo, worktreeEntry{Path: wt, IsLinkedWorktree: true, OpenWorkspaceID: "w2"})
	p := newWorktreeProvider(t, f)

	if err := p.start(context.Background(), worktreeSessionName, worktreeStartConfig(wt)); err != nil {
		t.Fatalf("start: %v", err)
	}
	calls := f.calls(t)
	log := strings.Join(calls, "\n")
	if callIndex(calls, "worktree open") >= 0 {
		t.Errorf("reopened a worktree that already has a workspace; log:\n%s", log)
	}
	if n := callCount(calls, "tab create"); n != 1 {
		t.Errorf("tab create calls = %d, want 1; log:\n%s", n, log)
	}
	createIdx := callIndex(calls, "tab create --workspace w2")
	closeIdx := callIndex(calls, "tab close w2:t-old")
	if createIdx < 0 || closeIdx < createIdx {
		t.Errorf("stale tab must close after the agent tab is created; log:\n%s", log)
	}
	if callIndex(calls, "tab close w2:t-other") >= 0 {
		t.Errorf("closed another label's tab; log:\n%s", log)
	}
	if got, _ := p.GetMeta(worktreeSessionName, metaBoundWorkspace); got != "w2" {
		t.Errorf("binding workspace = %q, want w2", got)
	}
}

// TestWorktreePlacementClosesStaleRigTab pins review finding 1: a session
// that last ran in a tab of the rig workspace has its stale same-label tab
// there closed once its worktree agent tab exists.
func TestWorktreePlacementClosesStaleRigTab(t *testing.T) {
	repo, wt := linkedWorktreeLayout(t)
	f := newFakeWorktreeHerdr(t)
	f.set(t, "rig_ws", "myrig")
	f.set(t, "tabs_w1", `{"result":{"tabs":[{"tab_id":"w1:t-old","label":"chat"},{"tab_id":"w1:t-other","label":"other"}]}}`)
	f.listWorktrees(t, repo, worktreeEntry{Path: wt, IsLinkedWorktree: true})
	p := newWorktreeProvider(t, f)

	if err := p.start(context.Background(), worktreeSessionName, worktreeStartConfig(wt)); err != nil {
		t.Fatalf("start: %v", err)
	}
	calls := f.calls(t)
	log := strings.Join(calls, "\n")
	createIdx := callIndex(calls, "tab create --workspace w2")
	closeIdx := callIndex(calls, "tab close w1:t-old")
	if createIdx < 0 || closeIdx < createIdx {
		t.Errorf("stale rig tab must close after the agent tab is created; log:\n%s", log)
	}
	if callIndex(calls, "tab close w1:t-other") >= 0 {
		t.Errorf("closed another session's rig tab; log:\n%s", log)
	}
}

// TestWorktreePlacementReopensVanishedWorkspace pins review finding 2: when
// the workspace `worktree list` reported open is gone by the time Start uses
// it, Start opens the worktree again rather than falling back to a tab.
func TestWorktreePlacementReopensVanishedWorkspace(t *testing.T) {
	repo, wt := linkedWorktreeLayout(t)
	f := newFakeWorktreeHerdr(t)
	f.set(t, "rig_ws", "myrig")
	f.set(t, "ws_gone_w2", "")
	f.listWorktrees(t, repo, worktreeEntry{Path: wt, IsLinkedWorktree: true, OpenWorkspaceID: "w2"})
	p := newWorktreeProvider(t, f)

	var err error
	diag := captureStderr(t, func() { err = p.start(context.Background(), worktreeSessionName, worktreeStartConfig(wt)) })
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	calls := f.calls(t)
	log := strings.Join(calls, "\n")
	if callIndex(calls, "worktree open --workspace w1 --path "+wt) < 0 {
		t.Errorf("vanished workspace was not reopened; log:\n%s", log)
	}
	if callIndex(calls, "tab create --workspace w2") < 0 || callIndex(calls, "tab create --workspace w1") >= 0 {
		t.Errorf("agent tab not in the reopened worktree workspace; log:\n%s", log)
	}
	if callIndex(calls, "tab close w2:t1") < 0 {
		t.Errorf("root tab of the reopened workspace not closed; log:\n%s", log)
	}
	if strings.Contains(diag, "worktree placement") {
		t.Errorf("unexpected fallback diagnostic: %q", diag)
	}
}

// TestWorktreePlacementClosesFreshWorkspaceWhenTabCreateFails pins the
// cleanup on the fallback path: a worktree workspace opened by this Start
// whose agent tab cannot be created is closed (through its root tab) before
// Start falls back to a rig tab with a diagnostic.
func TestWorktreePlacementClosesFreshWorkspaceWhenTabCreateFails(t *testing.T) {
	repo, wt := linkedWorktreeLayout(t)
	f := newFakeWorktreeHerdr(t)
	f.set(t, "rig_ws", "myrig")
	f.set(t, "tab_create_error_w2", "")
	f.listWorktrees(t, repo, worktreeEntry{Path: wt, IsLinkedWorktree: true})
	p := newWorktreeProvider(t, f)

	var err error
	diag := captureStderr(t, func() { err = p.start(context.Background(), worktreeSessionName, worktreeStartConfig(wt)) })
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	calls := f.calls(t)
	log := strings.Join(calls, "\n")
	closeIdx := callIndex(calls, "tab close w2:t1")
	fallbackIdx := callIndex(calls, "tab create --workspace w1")
	if closeIdx < 0 || fallbackIdx < closeIdx {
		t.Errorf("fresh worktree workspace must be closed before the tab fallback; log:\n%s", log)
	}
	if !strings.Contains(diag, `herdr: worktree placement for "`+worktreeSessionName+`" unavailable, using tab:`) {
		t.Errorf("missing fallback diagnostic; stderr:\n%s", diag)
	}
}

// TestWorktreePlacementFallsBackToTab pins AC-5: every session that is not in
// a linked worktree, and every herdr that cannot open one, gets today's tab
// placement. Only a herdr failure writes a diagnostic; the sessions the .git
// gate rules out make no worktree call at all.
func TestWorktreePlacementFallsBackToTab(t *testing.T) {
	for _, tc := range []struct {
		name         string
		setup        func(t *testing.T, f *fakeWorktreeHerdr) (workDir string)
		worktreeCall bool
		diagnostic   bool
	}{
		{
			name: "primary checkout",
			setup: func(t *testing.T, f *fakeWorktreeHerdr) string {
				repo, _ := linkedWorktreeLayout(t)
				f.listWorktrees(t, repo)
				return repo
			},
		},
		{
			name:  "non-git dir",
			setup: func(t *testing.T, _ *fakeWorktreeHerdr) string { return t.TempDir() },
		},
		{
			name:  "empty work dir",
			setup: func(*testing.T, *fakeWorktreeHerdr) string { return "" },
		},
		{
			name: "listed but not linked",
			setup: func(t *testing.T, f *fakeWorktreeHerdr) string {
				repo, wt := linkedWorktreeLayout(t)
				f.listWorktrees(t, repo, worktreeEntry{Path: wt})
				return wt
			},
			worktreeCall: true,
		},
		{
			name: "herdr without worktree",
			setup: func(t *testing.T, f *fakeWorktreeHerdr) string {
				_, wt := linkedWorktreeLayout(t)
				f.set(t, "unsupported", "")
				return wt
			},
			worktreeCall: true,
			diagnostic:   true,
		},
		{
			name: "worktree open error",
			setup: func(t *testing.T, f *fakeWorktreeHerdr) string {
				repo, wt := linkedWorktreeLayout(t)
				f.listWorktrees(t, repo, worktreeEntry{Path: wt, IsLinkedWorktree: true})
				f.set(t, "open_error", `{"error":{"code":"not_git_worktree","message":"worktree operations require a workspace inside a Git work tree"}}`)
				return wt
			},
			worktreeCall: true,
			diagnostic:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeWorktreeHerdr(t)
			f.set(t, "rig_ws", "myrig")
			workDir := tc.setup(t, f)
			p := newWorktreeProvider(t, f)

			var err error
			diag := captureStderr(t, func() {
				err = p.start(context.Background(), worktreeSessionName, worktreeStartConfig(workDir))
			})
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			calls := f.calls(t)
			log := strings.Join(calls, "\n")
			if idx := callIndex(calls, "tab create --workspace w1 --label chat"); idx < 0 {
				t.Errorf("no tab placement in the rig workspace; log:\n%s", log)
			}
			if got := callIndex(calls, "worktree ") >= 0; got != tc.worktreeCall {
				t.Errorf("worktree call made = %t, want %t; log:\n%s", got, tc.worktreeCall, log)
			}
			if callIndex(calls, "tab create --workspace w2") >= 0 {
				t.Errorf("agent tab in a worktree workspace; log:\n%s", log)
			}
			if got, _ := p.GetMeta(worktreeSessionName, metaBoundTab); got != "w1:t9" {
				t.Errorf("binding tab = %q, want w1:t9", got)
			}
			if got, _ := p.GetMeta(worktreeSessionName, metaBoundWorkspace); got != "" {
				t.Errorf("tab placement bound workspace %q", got)
			}
			if got := strings.Contains(diag, "herdr: worktree placement for"); got != tc.diagnostic {
				t.Errorf("diagnostic written = %t, want %t; stderr:\n%s", got, tc.diagnostic, diag)
			}
		})
	}
}

// TestWorktreePlacementStopKeepsCheckout pins AC-6: stopping a worktree
// session closes its pane and nothing else. No herdr worktree or git command
// runs, and a real `git worktree add` checkout and its registration survive.
func TestWorktreePlacementStopKeepsCheckout(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH; the real-checkout half of AC-6 needs it")
	}
	repo := t.TempDir()
	wt := filepath.Join(repo, "worktrees", "chat")
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(git, append([]string{"-c", "user.name=gc-test", "-c", "user.email=gc-test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	runGit(repo, "init", "-q")
	runGit(repo, "commit", "-q", "--allow-empty", "-m", "init")
	runGit(repo, "worktree", "add", "-q", "-b", "gc-chat", wt)

	f := newFakeWorktreeHerdr(t)
	f.set(t, "rig_ws", "myrig")
	f.listWorktrees(t, repo, worktreeEntry{Path: wt, IsLinkedWorktree: true})
	p := newWorktreeProvider(t, f)
	if err := p.start(context.Background(), worktreeSessionName, worktreeStartConfig(wt)); err != nil {
		t.Fatalf("start: %v", err)
	}
	if callIndex(f.calls(t), "tab create --workspace w2") < 0 {
		t.Fatalf("session was not placed in the worktree workspace; log:\n%s", strings.Join(f.calls(t), "\n"))
	}
	if err := p.Stop(worktreeSessionName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	calls := f.calls(t)
	log := strings.Join(calls, "\n")
	if callIndex(calls, "pane close w2:p9") < 0 {
		t.Errorf("Stop did not close the worktree pane; log:\n%s", log)
	}
	for _, banned := range []string{"worktree create", "worktree remove", "workspace close", "git "} {
		if callIndex(calls, banned) >= 0 {
			t.Errorf("log carries %q; log:\n%s", banned, log)
		}
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Errorf("worktree checkout gone after Stop: %v", err)
	}
	if list := runGit(repo, "worktree", "list"); !strings.Contains(list, filepath.Base(wt)) || !strings.Contains(list, "gc-chat") {
		t.Errorf("git worktree list no longer names the checkout:\n%s", list)
	}
}

// TestWorktreePlacementAttachAndLivenessUseBinding pins AC-7: every
// name-keyed operation reaches the worktree pane through the binding, with no
// workspace-specific code. Attach on Windows and Nudge resolve the pane this
// way; liveness reads the same binding.
func TestWorktreePlacementAttachAndLivenessUseBinding(t *testing.T) {
	repo, wt := linkedWorktreeLayout(t)
	f := newFakeWorktreeHerdr(t)
	f.set(t, "rig_ws", "myrig")
	f.listWorktrees(t, repo, worktreeEntry{Path: wt, IsLinkedWorktree: true})
	p := newWorktreeProvider(t, f)
	if err := p.start(context.Background(), worktreeSessionName, worktreeStartConfig(wt)); err != nil {
		t.Fatalf("start: %v", err)
	}

	pane, err := p.paneID(context.Background(), worktreeSessionName)
	if err != nil || pane != "w2:p9" {
		t.Fatalf("paneID = %q, %v; want the worktree pane w2:p9", pane, err)
	}
	if err := p.Nudge(worktreeSessionName, runtime.TextContent("hello")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if callIndex(f.calls(t), "agent prompt w2:p9") < 0 {
		t.Errorf("nudge did not target the worktree pane; log:\n%s", strings.Join(f.calls(t), "\n"))
	}
	// The busy verdict on Windows comes from the process table, which this
	// fake does not model; the pane resolution above is the same on both.
	if goruntime.GOOS != "windows" {
		f.set(t, "busy", "")
		if !p.IsRunning(worktreeSessionName) {
			t.Error("IsRunning = false for a busy worktree pane reached through the binding")
		}
	}
}

// TestLinkedWorktreeCandidate pins the gate that keeps every session outside
// a linked worktree from making a herdr call: only a .git file above the work
// dir qualifies.
func TestLinkedWorktreeCandidate(t *testing.T) {
	repo, wt := linkedWorktreeLayout(t)
	sub := filepath.Join(wt, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		dir  string
		want bool
	}{
		{"linked worktree", wt, true},
		{"subdir of a linked worktree", sub, true},
		{"primary checkout", repo, false},
		{"subdir of the primary checkout", filepath.Join(repo, "worktrees"), false},
		{"non-git dir", t.TempDir(), false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := linkedWorktreeCandidate(tc.dir); got != tc.want {
				t.Errorf("linkedWorktreeCandidate(%q) = %t, want %t", tc.dir, got, tc.want)
			}
		})
	}
}

// TestMatchWorktree pins how a work dir maps to herdr's worktree entries: the
// longest containing path wins (the primary checkout contains nested
// worktrees), and only a linked worktree matches.
func TestMatchWorktree(t *testing.T) {
	repo, wt := linkedWorktreeLayout(t)
	other := filepath.Join(repo, "worktrees", "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(wt, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	entries := []worktreeEntry{
		{Path: repo},
		{Path: wt, IsLinkedWorktree: true, OpenWorkspaceID: "w2"},
		{Path: other, IsLinkedWorktree: true},
	}
	for _, tc := range []struct {
		name    string
		workDir string
		want    string
	}{
		{"worktree path", wt, wt},
		{"subdir of a worktree", sub, wt},
		{"primary checkout", repo, ""},
		{"primary checkout subdir", filepath.Join(repo, "worktrees"), ""},
		{"sibling with a shared prefix", wt + "-x", ""},
		{"outside the repo", t.TempDir(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := matchWorktree(entries, tc.workDir)
			if ok != (tc.want != "") || got.Path != tc.want {
				t.Errorf("matchWorktree(%q) = %+v, %t; want path %q", tc.workDir, got, ok, tc.want)
			}
		})
	}
}

// TestMatchWorktreeResolvesSymlinks pins that a work dir reached through a
// symlink still matches the worktree herdr lists by its real path.
func TestMatchWorktreeResolvesSymlinks(t *testing.T) {
	repo, wt := linkedWorktreeLayout(t)
	link := filepath.Join(t.TempDir(), "via-link")
	if err := os.Symlink(wt, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	got, ok := matchWorktree([]worktreeEntry{{Path: repo}, {Path: wt, IsLinkedWorktree: true}}, link)
	if !ok || got.Path != wt {
		t.Errorf("matchWorktree(%q) = %+v, %t; want %q", link, got, ok, wt)
	}
}

// TestWorktreeClientDecoding pins the CLI shapes the client speaks: list
// decodes the source and entries, open passes exactly the documented flags and
// decodes the workspace and root tab, and a decode failure is an error.
func TestWorktreeClientDecoding(t *testing.T) {
	repo, wt := linkedWorktreeLayout(t)
	f := newFakeWorktreeHerdr(t)
	f.listWorktrees(t, repo, worktreeEntry{Path: wt, IsLinkedWorktree: true, OpenWorkspaceID: "w7"})
	c := newClient("testwt", "")
	c.bin = f.bin

	src, entries, err := c.worktreeList(context.Background(), wt)
	if err != nil {
		t.Fatalf("worktreeList: %v", err)
	}
	if src.RepoRoot != repo || len(entries) != 2 || entries[1] != (worktreeEntry{Path: wt, IsLinkedWorktree: true, OpenWorkspaceID: "w7"}) {
		t.Errorf("worktreeList = %+v, %+v", src, entries)
	}

	ws, tab, already, err := c.worktreeOpen(context.Background(), "w1", wt, "chat")
	if err != nil || ws != "w2" || tab != "w2:t1" || already {
		t.Errorf("worktreeOpen = %q, %q, %t, %v; want w2, w2:t1, false", ws, tab, already, err)
	}
	if got := f.calls(t); got[len(got)-1] != "worktree open --workspace w1 --path "+wt+" --label chat --no-focus" {
		t.Errorf("worktree open argv = %q", got[len(got)-1])
	}
	if _, _, already, err := c.worktreeOpen(context.Background(), "w1", wt, "chat"); err != nil || !already {
		t.Errorf("second worktreeOpen already_open = %t, %v; want true", already, err)
	}

	ws, tab, pane, err := c.workspaceCreateWithID(context.Background(), "myrig", repo, nil)
	if err != nil || ws != "w1" || tab != "w1:t1" || pane != "w1:p1" {
		t.Errorf("workspaceCreateWithID = %q, %q, %q, %v", ws, tab, pane, err)
	}

	f.set(t, "worktrees.json", `{"result":{"worktrees":"not a list"}}`)
	if _, _, err := c.worktreeList(context.Background(), wt); err == nil || !strings.Contains(err.Error(), "herdr worktree list: decode") {
		t.Errorf("worktreeList on a bad reply = %v; want a decode error", err)
	}
}
