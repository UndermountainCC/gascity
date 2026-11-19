package herdr

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// testTown stands in for the city name the Provider holds as its herdr session.
const testTown = "my-city"

// TestPlacementFor covers how a runtime session name + reconciler env map to a
// herdr workspace and tab. The wisp cases are the reason this exists: their
// town-qualified, wisp-id runtime name must be lifted into the originating rig
// workspace under a themed tab.
func TestPlacementFor(t *testing.T) {
	tests := []struct {
		name     string
		sessName string
		town     string // the herdr session name (the city); "" means testTown
		env      map[string]string
		wantWS   string
		wantTab  string
	}{
		{
			name:     "wisp gets rig workspace and themed tab",
			sessName: "gastown__polecat-gc-wisp-3nvj3yx",
			env:      map[string]string{"GC_RIG": "webapp", "GC_ALIAS": "webapp/gastown.furiosa"},
			wantWS:   "webapp",
			wantTab:  "polecat-furiosa",
		},
		{
			name:     "wisp on mobile with bare alias",
			sessName: "gastown__polecat-gc-wisp-abc123",
			env:      map[string]string{"GC_RIG": "mobile", "GC_ALIAS": "nux"},
			wantWS:   "mobile",
			wantTab:  "polecat-nux",
		},
		{
			name:     "wisp falls back to GC_AGENT when no alias",
			sessName: "gastown__polecat-gc-wisp-abc123",
			env:      map[string]string{"GC_RIG": "webapp", "GC_AGENT": "webapp/gastown.slit"},
			wantWS:   "webapp",
			wantTab:  "polecat-slit",
		},
		{
			name:     "wisp with no alias yet keeps wisp tab but still moves to rig",
			sessName: "gastown__polecat-gc-wisp-abc123",
			env:      map[string]string{"GC_RIG": "webapp"},
			wantWS:   "webapp",
			wantTab:  "polecat-gc-wisp-abc123",
		},
		{
			name:     "alias that is itself the wisp identity is ignored",
			sessName: "gastown__polecat-gc-wisp-abc123",
			env:      map[string]string{"GC_RIG": "webapp", "GC_AGENT": "gastown.polecat-gc-wisp-abc123"},
			wantWS:   "webapp",
			wantTab:  "polecat-gc-wisp-abc123",
		},
		{
			// The wisp id is a bead id minted by the pouring scope, so it carries
			// that scope's beads prefix — "gc-" only when the city's HQ prefix is
			// literally "gc". Detection must be prefix-agnostic or every other
			// city keeps raw wisp ids in its tabs.
			name:     "wisp with non-gc city prefix gets themed tab",
			sessName: "gastown__polecat-az-wisp-z0pu0",
			env:      map[string]string{"GC_RIG": "gascity", "GC_ALIAS": "gascity/gastown.furiosa"},
			wantWS:   "gascity",
			wantTab:  "polecat-furiosa",
		},
		{
			name:     "wisp id with no role prefix is replaced whole",
			sessName: "gastown__az-wisp-z0pu0",
			env:      map[string]string{"GC_RIG": "gascity", "GC_ALIAS": "gascity/gastown.furiosa"},
			wantWS:   "gascity",
			wantTab:  "furiosa",
		},
		{
			// The deferred pool spawn blanks GC_ALIAS and leaves the session name
			// in GC_AGENT (build_desired_state_pool_info.go); once the alias
			// resolves the reconciler repopulates it.
			name:     "non-gc wisp falls back to GC_AGENT when alias not yet resolved",
			sessName: "gastown__polecat-az-wisp-g8ha3",
			env:      map[string]string{"GC_RIG": "gascity", "GC_ALIAS": "", "GC_AGENT": "gascity/gastown.nux"},
			wantWS:   "gascity",
			wantTab:  "polecat-nux",
		},
		{
			// Guard case: the deferred fallback hands back the session name
			// itself, which still carries the wisp id. Substituting it would
			// produce "polecat-gastown__polecat-az-wisp-z0pu0".
			name:     "non-gc alias that is itself the wisp identity is ignored",
			sessName: "gastown__polecat-az-wisp-z0pu0",
			env:      map[string]string{"GC_RIG": "gascity", "GC_ALIAS": "", "GC_AGENT": "gastown__polecat-az-wisp-z0pu0"},
			wantWS:   "gascity",
			wantTab:  "polecat-az-wisp-z0pu0",
		},
		{
			name:     "persistent rig agent unchanged",
			sessName: "webapp--gastown__witness",
			env:      map[string]string{"GC_RIG": "webapp", "GC_ALIAS": "webapp/gastown.witness"},
			wantWS:   "webapp",
			wantTab:  "witness",
		},
		{
			name:     "town-level agent has no rig and stays in town",
			sessName: "gastown__mayor",
			env:      map[string]string{"GC_ALIAS": "gastown.mayor"},
			wantWS:   "gastown",
			wantTab:  "mayor",
		},
		{
			name:     "no env falls back to structural name parsing",
			sessName: "webapp--gastown__refinery",
			env:      nil,
			wantWS:   "webapp",
			wantTab:  "refinery",
		},
		{
			// gascity#8933: the default session_template is {{.Agent}}, so a
			// town-level agent's runtime name carries neither "--" nor "__" and no
			// GC_RIG. It must land in the one shared town workspace (the city name),
			// not in a workspace named after itself.
			name:     "default-template town agent shares the town workspace",
			sessName: "mayor",
			env:      map[string]string{"GC_ALIAS": "mayor"},
			wantWS:   testTown,
			wantTab:  "mayor",
		},
		{
			name:     "second default-template town agent lands in the same workspace",
			sessName: "generalist-builder",
			env:      map[string]string{"GC_ALIAS": "generalist-builder"},
			wantWS:   testTown,
			wantTab:  "generalist-builder",
		},
		{
			name:     "default-template town agent with no env shares the town workspace",
			sessName: "mayor",
			env:      nil,
			wantWS:   testTown,
			wantTab:  "mayor",
		},
		{
			// Control: a bare name WITH a GC_RIG is a rig agent and keeps grouping
			// under its rig, exactly as before the town fallback existed.
			name:     "default-template rig agent still groups under its rig",
			sessName: "claude-1",
			env:      map[string]string{"GC_RIG": "my-rig", "GC_ALIAS": "my-rig/claude-1"},
			wantWS:   "my-rig",
			wantTab:  "claude-1",
		},
		{
			name:     "default-template rig wisp still groups under its rig with a themed tab",
			sessName: "claude-gm-wisp-sf4qr",
			env:      map[string]string{"GC_RIG": "my-rig", "GC_ALIAS": "my-rig/furiosa"},
			wantWS:   "my-rig",
			wantTab:  "claude-furiosa",
		},
		{
			name:     "town-qualified name keeps its structural town workspace",
			sessName: "gastown__mayor",
			town:     "other-city",
			env:      map[string]string{"GC_ALIAS": "gastown.mayor"},
			wantWS:   "gastown",
			wantTab:  "mayor",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			town := tt.town
			if town == "" {
				town = testTown
			}
			gotWS, gotTab := placementFor(tt.sessName, town, tt.env)
			if gotWS != tt.wantWS || gotTab != tt.wantTab {
				t.Errorf("placementFor(%q, %q, %v) = (%q, %q), want (%q, %q)",
					tt.sessName, town, tt.env, gotWS, gotTab, tt.wantWS, tt.wantTab)
			}
		})
	}
}

// TestWorkspaceTabFor pins the structural half of placement. A runtime name
// with neither "--" nor "__" is a town-level agent under the default
// session_template ({{.Agent}}); it belongs in the town workspace (the herdr
// session name), not in a workspace named after the agent (gascity#8933). Names
// that do carry a separator keep the placement they always had.
func TestWorkspaceTabFor(t *testing.T) {
	tests := []struct {
		name     string
		sessName string
		town     string
		wantWS   string
		wantTab  string
	}{
		{name: "bare town agent goes to the town workspace", sessName: "mayor", town: testTown, wantWS: testTown, wantTab: "mayor"},
		{name: "another bare town agent goes to the same workspace", sessName: "sie-broker", town: testTown, wantWS: testTown, wantTab: "sie-broker"},
		{name: "town-qualified name keeps its town segment", sessName: "gastown__mayor", town: testTown, wantWS: "gastown", wantTab: "mayor"},
		{name: "rig-qualified name keeps its rig", sessName: "webapp--gastown__witness", town: testTown, wantWS: "webapp", wantTab: "witness"},
		{name: "rig-qualified name without a town segment keeps its rig", sessName: "webapp--witness", town: testTown, wantWS: "webapp", wantTab: "witness"},
		{name: "no town known falls back to the name, as before", sessName: "mayor", town: "", wantWS: "mayor", wantTab: "mayor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotWS, gotTab := workspaceTabFor(tt.sessName, tt.town)
			if gotWS != tt.wantWS || gotTab != tt.wantTab {
				t.Errorf("workspaceTabFor(%q, %q) = (%q, %q), want (%q, %q)",
					tt.sessName, tt.town, gotWS, gotTab, tt.wantWS, tt.wantTab)
			}
		})
	}
}

// The pane-group tests drive Provider.start against the worktree fake
// (worktree_placement_test.go), which also models `pane list`, `pane split`
// and `pane rename`. Several capture os.Stderr, so none runs in parallel.

// groupStart starts worktreeSessionName (rig "myrig", tab "chat") in workDir
// with GC_HERDR_PANE_GROUP=control and returns the fake's call log and what
// Start wrote to stderr.
func groupStart(t *testing.T, f *fakeWorktreeHerdr, p *Provider, workDir string) ([]string, string) {
	t.Helper()
	cfg := worktreeStartConfig(workDir)
	cfg.Env[envPaneGroup] = "control"
	var err error
	diag := captureStderr(t, func() { err = p.start(context.Background(), worktreeSessionName, cfg) })
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return f.calls(t), diag
}

func wantBinding(t *testing.T, p *Provider, ws, tab, pane string) {
	t.Helper()
	for key, want := range map[string]string{metaBoundWorkspace: ws, metaBoundTab: tab, metaBoundPane: pane} {
		if got, _ := p.GetMeta(worktreeSessionName, key); got != want {
			t.Errorf("binding %s = %q, want %q", key, got, want)
		}
	}
}

// TestPaneGroupUnsetKeepsTabPlacement: without GC_HERDR_PANE_GROUP a session
// gets its own tab and no pane verb runs.
func TestPaneGroupUnsetKeepsTabPlacement(t *testing.T) {
	f := newFakeWorktreeHerdr(t)
	f.set(t, "rig_ws", "myrig")
	p := newWorktreeProvider(t, f)
	if err := p.start(context.Background(), worktreeSessionName, worktreeStartConfig(t.TempDir())); err != nil {
		t.Fatalf("start: %v", err)
	}
	calls := f.calls(t)
	if callIndex(calls, "tab create --workspace w1 --label chat") < 0 {
		t.Errorf("no own tab; log:\n%s", strings.Join(calls, "\n"))
	}
	for _, verb := range []string{"pane list", "pane split", "pane rename"} {
		if callIndex(calls, verb) >= 0 {
			t.Errorf("ungrouped Start ran %q; log:\n%s", verb, strings.Join(calls, "\n"))
		}
	}
	wantBinding(t, p, "", "w1:t9", "w1:p9")
}

// TestPaneGroupFirstSessionCreatesGroupTab: the first grouped session of a
// workspace creates the group tab, with its cwd and env in the root pane, and
// labels that pane with its tab label. In a new workspace the default tab
// becomes the group tab.
func TestPaneGroupFirstSessionCreatesGroupTab(t *testing.T) {
	t.Run("existing workspace", func(t *testing.T) {
		f := newFakeWorktreeHerdr(t)
		f.set(t, "rig_ws", "myrig")
		p := newWorktreeProvider(t, f)
		dir := t.TempDir()
		calls, _ := groupStart(t, f, p, dir)
		log := strings.Join(calls, "\n")
		createIdx := callIndex(calls, "tab create --workspace w1 --label control --cwd "+dir)
		if createIdx < 0 {
			t.Fatalf("no group tab created; log:\n%s", log)
		}
		for _, kv := range []string{"GC_SESSION_ID=sess-1", envPaneGroup + "=control"} {
			if !strings.Contains(calls[createIdx], "--env "+kv) {
				t.Errorf("group tab create lacks --env %s: %s", kv, calls[createIdx])
			}
		}
		if callIndex(calls, "pane rename w1:p9 chat") < 0 || callIndex(calls, "pane split") >= 0 {
			t.Errorf("root pane not labeled, or a split ran; log:\n%s", log)
		}
		wantBinding(t, p, "w1", "w1:t9", "w1:p9")
	})
	t.Run("new workspace", func(t *testing.T) {
		f := newFakeWorktreeHerdr(t)
		p := newWorktreeProvider(t, f)
		calls, _ := groupStart(t, f, p, t.TempDir())
		log := strings.Join(calls, "\n")
		if callIndex(calls, "workspace create --label myrig") < 0 || callIndex(calls, "tab rename w1:t1 control") < 0 || callIndex(calls, "pane rename w1:p1 chat") < 0 {
			t.Errorf("default tab not made the group tab; log:\n%s", log)
		}
		wantBinding(t, p, "w1", "w1:t1", "w1:p1")
	})
}

// TestPaneGroupSplitsIntoExistingGroupTab: a later session splits the group
// tab's last pane downward, without focus, with its own cwd and env, and
// creates no tab.
func TestPaneGroupSplitsIntoExistingGroupTab(t *testing.T) {
	f := newFakeWorktreeHerdr(t)
	f.set(t, "rig_ws", "myrig")
	f.set(t, "tabs_w1", `{"result":{"tabs":[{"tab_id":"w1:t1","label":"shell"},{"tab_id":"w1:t-ctl","label":"control"}]}}`)
	f.set(t, "panes_w1", `{"result":{"panes":[{"pane_id":"w1:p1","tab_id":"w1:t1"},{"pane_id":"w1:p-a","tab_id":"w1:t-ctl","label":"a"},{"pane_id":"w1:p-b","tab_id":"w1:t-ctl","label":"b"}]}}`)
	p := newWorktreeProvider(t, f)
	dir := t.TempDir()
	calls, _ := groupStart(t, f, p, dir)
	log := strings.Join(calls, "\n")
	splitIdx := callIndex(calls, "pane split w1:p-b --direction down --no-focus --cwd "+dir)
	if splitIdx < 0 {
		t.Fatalf("group tab's last pane not split; log:\n%s", log)
	}
	if !strings.Contains(calls[splitIdx], "--env GC_SESSION_ID=sess-1") {
		t.Errorf("split lacks the session env: %s", calls[splitIdx])
	}
	if callIndex(calls, "tab create") >= 0 || callIndex(calls, "pane close") >= 0 || callIndex(calls, "tab close") >= 0 {
		t.Errorf("split Start created a tab or closed something; log:\n%s", log)
	}
	if callIndex(calls, "pane rename w1:p8 chat") < splitIdx {
		t.Errorf("new pane not labeled after the split; log:\n%s", log)
	}
	wantBinding(t, p, "w1", "w1:t-ctl", "w1:p8")
}

// TestPaneGroupRestartClosesStaleOnlyAfterSplit: a restart closes the panes
// of its label in the group tab, and a tab of its label left by a tab-placed
// life, only once the new pane exists, and nothing of another label.
func TestPaneGroupRestartClosesStaleOnlyAfterSplit(t *testing.T) {
	f := newFakeWorktreeHerdr(t)
	f.set(t, "rig_ws", "myrig")
	f.set(t, "tabs_w1", `{"result":{"tabs":[{"tab_id":"w1:t-ctl","label":"control"},{"tab_id":"w1:t-old","label":"chat"}]}}`)
	f.set(t, "panes_w1", `{"result":{"panes":[{"pane_id":"w1:p-old","tab_id":"w1:t-ctl","label":"chat"},{"pane_id":"w1:p-a","tab_id":"w1:t-ctl","label":"a"},{"pane_id":"w1:p-x","tab_id":"w1:t-old","label":"chat"}]}}`)
	p := newWorktreeProvider(t, f)
	calls, _ := groupStart(t, f, p, t.TempDir())
	log := strings.Join(calls, "\n")
	splitIdx := callIndex(calls, "pane split w1:p-a ")
	renameIdx := callIndex(calls, "pane rename w1:p8 chat")
	if splitIdx < 0 || renameIdx < splitIdx {
		t.Fatalf("no split of the last group pane then label; log:\n%s", log)
	}
	if idx := callIndex(calls, "pane close w1:p-old"); idx < renameIdx {
		t.Errorf("stale pane must close after the new pane exists; log:\n%s", log)
	}
	if idx := callIndex(calls, "tab close w1:t-old"); idx < renameIdx {
		t.Errorf("stale tab must close after the new pane exists; log:\n%s", log)
	}
	for _, banned := range []string{"pane close w1:p-a", "pane close w1:p8", "tab close w1:t-ctl", "pane close w1:p-x"} {
		if callIndex(calls, banned) >= 0 {
			t.Errorf("log carries %q; log:\n%s", banned, log)
		}
	}
}

// TestPaneGroupAnchorGoneRelistsOnce: a split whose anchor closed after the
// list re-lists and splits again, once.
func TestPaneGroupAnchorGoneRelistsOnce(t *testing.T) {
	f := newFakeWorktreeHerdr(t)
	f.set(t, "rig_ws", "myrig")
	f.set(t, "split_gone", "")
	f.set(t, "tabs_w1", `{"result":{"tabs":[{"tab_id":"w1:t-ctl","label":"control"}]}}`)
	f.set(t, "panes_w1", `{"result":{"panes":[{"pane_id":"w1:p-a","tab_id":"w1:t-ctl","label":"a"}]}}`)
	p := newWorktreeProvider(t, f)
	calls, diag := groupStart(t, f, p, t.TempDir())
	if n := callCount(calls, "pane split"); n != 2 || callCount(calls, "pane list") != 2 {
		t.Errorf("splits = %d, lists = %d; want 2 each; log:\n%s", n, callCount(calls, "pane list"), strings.Join(calls, "\n"))
	}
	if diag != "" {
		t.Errorf("unexpected diagnostic: %q", diag)
	}
	wantBinding(t, p, "w1", "w1:t-ctl", "w1:p8")
}

// TestPaneGroupFallsBackToTab: a herdr without `pane split` or `pane rename`
// gets the session's own tab with one diagnostic; a split pane it could not
// label is closed first.
func TestPaneGroupFallsBackToTab(t *testing.T) {
	for _, tc := range []struct{ flag, closed string }{
		{flag: "split_unsupported"},
		{flag: "rename_unsupported", closed: "pane close w1:p8"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			f := newFakeWorktreeHerdr(t)
			f.set(t, "rig_ws", "myrig")
			f.set(t, tc.flag, "")
			f.set(t, "tabs_w1", `{"result":{"tabs":[{"tab_id":"w1:t-ctl","label":"control"}]}}`)
			f.set(t, "panes_w1", `{"result":{"panes":[{"pane_id":"w1:p-a","tab_id":"w1:t-ctl","label":"a"}]}}`)
			p := newWorktreeProvider(t, f)
			calls, diag := groupStart(t, f, p, t.TempDir())
			log := strings.Join(calls, "\n")
			fallbackIdx := callIndex(calls, "tab create --workspace w1 --label chat")
			if fallbackIdx < 0 {
				t.Fatalf("no tab fallback; log:\n%s", log)
			}
			if tc.closed != "" {
				if idx := callIndex(calls, tc.closed); idx < 0 || idx > fallbackIdx {
					t.Errorf("unlabeled pane not closed before the fallback; log:\n%s", log)
				}
			}
			if strings.Count(diag, "herdr: pane group \"control\" for \""+worktreeSessionName+"\" unavailable, using tab:") != 1 {
				t.Errorf("want one fallback diagnostic; stderr:\n%s", diag)
			}
			wantBinding(t, p, "", "w1:t9", "w1:p9")
		})
	}
}

// TestPaneGroupTakesPrecedenceOverWorktree: a grouped session whose work dir
// is a linked worktree lands in the group tab of the rig workspace, with the
// worktree as its cwd, and opens no worktree workspace.
func TestPaneGroupTakesPrecedenceOverWorktree(t *testing.T) {
	repo, wt := linkedWorktreeLayout(t)
	f := newFakeWorktreeHerdr(t)
	f.set(t, "rig_ws", "myrig")
	f.listWorktrees(t, repo, worktreeEntry{Path: wt, IsLinkedWorktree: true})
	p := newWorktreeProvider(t, f)
	calls, _ := groupStart(t, f, p, wt)
	log := strings.Join(calls, "\n")
	if callIndex(calls, "worktree ") >= 0 {
		t.Errorf("grouped Start made a worktree call; log:\n%s", log)
	}
	if callIndex(calls, "tab create --workspace w1 --label control --cwd "+wt) < 0 {
		t.Errorf("not placed in the rig workspace's group tab; log:\n%s", log)
	}
	wantBinding(t, p, "w1", "w1:t9", "w1:p9")
}

// TestPaneGroupConcurrentStartsShareOneTab: grouped Starts racing in one
// workspace create one group tab; the others split into it.
func TestPaneGroupConcurrentStartsShareOneTab(t *testing.T) {
	f := newFakeWorktreeHerdr(t)
	f.set(t, "rig_ws", "myrig")
	f.set(t, "record_tabs", "")
	p := newWorktreeProvider(t, f)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, name := range []string{"myrig--city__disp-a", "myrig--city__disp-b"} {
		cfg := worktreeStartConfig(t.TempDir())
		cfg.Env[envPaneGroup] = "control"
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			errs <- p.start(context.Background(), name, cfg)
		}(name)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("start: %v", err)
		}
	}
	calls := f.calls(t)
	if n, s := callCount(calls, "tab create"), callCount(calls, "pane split w1:p9 "); n != 1 || s != 1 {
		t.Errorf("tab creates = %d, splits of the group pane = %d; want 1 each; log:\n%s", n, s, strings.Join(calls, "\n"))
	}
}
