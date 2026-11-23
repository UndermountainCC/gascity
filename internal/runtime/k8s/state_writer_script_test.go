package k8s

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// The agent image installs the state writer and the Claude Code managed
// settings that call it at these paths.
const (
	stateWriterImagePath     = "/usr/local/bin/kubectl-herdr-state.sh"
	managedSettingsImagePath = "/etc/claude-code/managed-settings.json"
)

// contribK8sPath is the path of a file in contrib/k8s.
func contribK8sPath(t *testing.T, name string) string {
	t.Helper()
	if root := bazeltest.OverrideRoot(); root != "" {
		return filepath.Join(root, "contrib", "k8s", name)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "contrib", "k8s", name))
}

type stateWriterRun struct {
	stdout, stderr string
	err            error
}

// runStateWriter runs the state writer under sh with stateFile as its
// KUBECTL_HERDR_STATE_FILE, set on the child alone.
func runStateWriter(t *testing.T, stateFile string, args ...string) stateWriterRun {
	t.Helper()
	cmd := exec.Command("sh", append([]string{contribK8sPath(t, "kubectl-herdr-state.sh")}, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "KUBECTL_HERDR_STATE_FILE=" + stateFile}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stateWriterRun{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

// TS-5: one line "<state> <unix-seconds>", written by rename so no temp file
// is left, a later write replacing the earlier one, nothing on stdout.
func TestStateWriterWritesTheStateLineAtomically(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kubectl-herdr")
	stateFile := filepath.Join(dir, "state")
	line := regexp.MustCompile(`^(idle|working|blocked) [0-9]+\n$`)
	for _, state := range []string{"working", "blocked", "idle"} {
		run := runStateWriter(t, stateFile, state)
		if run.err != nil || run.stdout != "" || run.stderr != "" {
			t.Fatalf("state writer %s: err=%v stdout=%q stderr=%q, want exit 0 and no output", state, run.err, run.stdout, run.stderr)
		}
		got, err := os.ReadFile(stateFile)
		if err != nil {
			t.Fatalf("reading the state file: %v", err)
		}
		if !line.Match(got) || !strings.HasPrefix(string(got), state+" ") {
			t.Fatalf("state file = %q, want %q followed by unix seconds", got, state)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "state" {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("state directory holds %q, want only the state file", names)
		}
	}
}

// TS-5: a failed write and a bad argument still exit 0 with nothing on
// stdout, saying why on stderr, so a hook never fails the agent's turn.
func TestStateWriterNeverFailsTheHook(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	okFile := filepath.Join(t.TempDir(), "state")
	for _, tc := range []struct {
		name      string
		stateFile string
		args      []string
	}{
		{name: "unwritable path", stateFile: filepath.Join(blocker, "dir", "state"), args: []string{"working"}},
		{name: "unknown state", stateFile: okFile, args: []string{"busy"}},
		{name: "no state", stateFile: okFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runStateWriter(t, tc.stateFile, tc.args...)
			if run.err != nil {
				t.Fatalf("state writer exit = %v, want 0", run.err)
			}
			if run.stdout != "" {
				t.Fatalf("stdout = %q, want none", run.stdout)
			}
			if strings.TrimSpace(run.stderr) == "" {
				t.Fatal("stderr is empty, want the reason")
			}
		})
	}
	if _, err := os.Stat(okFile); !os.IsNotExist(err) {
		t.Fatalf("a bad argument wrote the state file (stat err %v)", err)
	}
}

// TS-5: without KUBECTL_HERDR_STATE_FILE the writer uses kubectl-herdr's
// default path. Read from the script, so the test never writes there.
func TestStateWriterDefaultPath(t *testing.T) {
	script, err := os.ReadFile(contribK8sPath(t, "kubectl-herdr-state.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "${KUBECTL_HERDR_STATE_FILE:-/tmp/kubectl-herdr/state}") {
		t.Fatal("the state writer does not default KUBECTL_HERDR_STATE_FILE to /tmp/kubectl-herdr/state")
	}
}

// REQ-010: the image's managed settings hold only hooks, mapping exactly
// these events (and matchers) to states, each through the image's writer.
func TestManagedSettingsMapHookEventsToStates(t *testing.T) {
	raw, err := os.ReadFile(contribK8sPath(t, "claude-managed-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("managed settings are not JSON: %v", err)
	}
	if len(settings) != 1 || settings["hooks"] == nil {
		t.Fatalf("managed settings keys = %v, want only hooks", settingsKeys(settings))
	}
	var hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(settings["hooks"], &hooks); err != nil {
		t.Fatalf("decoding hooks: %v", err)
	}
	got := map[string]string{}
	for event, groups := range hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				state, ok := strings.CutPrefix(h.Command, "sh "+stateWriterImagePath+" ")
				if h.Type != "command" || !ok {
					t.Errorf("%s hook = %+v, want a command running %s", event, h, stateWriterImagePath)
				}
				got[event+"["+g.Matcher+"]"] += state
			}
		}
	}
	want := map[string]string{
		"UserPromptSubmit[]":                 "working",
		"PreToolUse[]":                       "working",
		"PostToolUse[]":                      "working",
		"PostToolUseFailure[]":               "working",
		"Notification[permission_prompt]":    "blocked",
		"Stop[]":                             "idle",
		"StopFailure[]":                      "idle",
		"SessionStart[startup|resume|clear]": "idle",
	}
	if len(got) != len(want) {
		t.Errorf("hook mapping = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("hook %s = %q, want %q", k, got[k], v)
		}
	}
}

// The agent image copies both files, as root, to the paths the settings and
// Claude Code's Linux managed-settings lookup use.
func TestAgentImageInstallsTheStateWriter(t *testing.T) {
	raw, err := os.ReadFile(contribK8sPath(t, "Dockerfile.agent"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	index := func(line string) int {
		return slices.IndexFunc(lines, func(l string) bool { return strings.TrimSpace(l) == line })
	}
	root, user := index("USER root"), index("USER gcagent")
	for _, copyLine := range []string{
		"COPY contrib/k8s/kubectl-herdr-state.sh " + stateWriterImagePath,
		"COPY contrib/k8s/claude-managed-settings.json " + managedSettingsImagePath,
	} {
		i := index(copyLine)
		if i < 0 {
			t.Errorf("Dockerfile.agent lacks %q", copyLine)
			continue
		}
		if root < 0 || user < 0 || i < root || i > user {
			t.Errorf("%q is not between USER root and USER gcagent", copyLine)
		}
	}
}

func settingsKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
