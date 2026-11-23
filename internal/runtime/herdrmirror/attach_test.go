package herdrmirror

import (
	"slices"
	"strings"
	"testing"
)

// AC-2: the exact attach argv, without and with a kube context.
func TestAttachArgv(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []harnessOpt
		want []string
	}{
		{
			name: "no context",
			want: []string{
				"attach", "-n", "gc", "pod-worker", "-l", "gc-session=pod-worker", "-c", "agent",
				"--tmux-session", "main", "--herdr-session", "city", "--workspace", "w1",
				"--label", "pod-worker", "--focus=false", "--output", "json",
			},
		},
		{
			name: "with context",
			opts: []harnessOpt{withContext("staging")},
			want: []string{
				"attach", "-n", "gc", "--context", "staging", "pod-worker", "-l", "gc-session=pod-worker", "-c", "agent",
				"--tmux-session", "main", "--herdr-session", "city", "--workspace", "w1",
				"--label", "pod-worker", "--focus=false", "--output", "json",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.opts...)
			if err := h.start(t, "pod-worker"); err != nil {
				t.Fatalf("Start: %v", err)
			}
			attaches := h.herdr.attachCalls()
			if len(attaches) != 1 {
				t.Fatalf("attach calls = %d, want 1 (argv %q)", len(attaches), h.herdr.argvs())
			}
			if got := attaches[0].bin; got != "/usr/local/bin/kubectl-herdr" {
				t.Errorf("attach binary = %q, want the LookPath result", got)
			}
			if !slices.Equal(attaches[0].args, tc.want) {
				t.Errorf("attach argv:\n got %q\nwant %q", attaches[0].args, tc.want)
			}
		})
	}
}

// The pod and selector are the k8s leaf's sanitized name and label, while the
// tab label is the session name verbatim; the attach never passes a command,
// so it cannot create a tmux session.
func TestAttachArgvSanitizesThePodTargetOnly(t *testing.T) {
	h := newHarness(t)
	if err := h.start(t, "Rig/Pod_Worker"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	args := h.herdr.attachCalls()[0].args
	if got := args[3]; got != "rig-pod-worker" {
		t.Errorf("pod = %q, want %q", got, "rig-pod-worker")
	}
	if got := flagValue(args, "-l"); got != "gc-session=Rig-Pod_Worker" {
		t.Errorf("selector = %q, want %q", got, "gc-session=Rig-Pod_Worker")
	}
	if got := flagValue(args, "--label"); got != "Rig/Pod_Worker" {
		t.Errorf("tab label = %q, want the session name", got)
	}
	for _, a := range args {
		if a == "--" || strings.Contains(a, "new-session") || a == "--agent" {
			t.Errorf("attach argv %q must not pass a command or --agent", args)
		}
	}
}

func TestDecodeAttach(t *testing.T) {
	for _, tc := range []struct {
		name    string
		out     string
		want    mirrorPane
		wantErr bool
	}{
		{name: "ids", out: `{"tab_id":"w1:t2","pane_id":"w1:p2","herdr_session":"city","namespace":"gc","pod":"p"}`, want: mirrorPane{tabID: "w1:t2", paneID: "w1:p2"}},
		{name: "trailing newline", out: "{\"tab_id\":\"w1:t2\",\"pane_id\":\"w1:p2\"}\n", want: mirrorPane{tabID: "w1:t2", paneID: "w1:p2"}},
		{name: "not json", out: "attached\n", wantErr: true},
		{name: "empty", out: "", wantErr: true},
		{name: "no tab id", out: `{"pane_id":"w1:p2"}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeAttach([]byte(tc.out))
			if (err != nil) != tc.wantErr {
				t.Fatalf("decodeAttach err = %v, wantErr %t", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("decodeAttach = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestExcerptBoundsStderr(t *testing.T) {
	long := strings.Repeat("x", 2000)
	if got := excerpt([]byte(long)); len(got) > stderrExcerptLimit+len("…") {
		t.Errorf("excerpt length = %d, want at most %d bytes plus the ellipsis", len(got), stderrExcerptLimit)
	}
	if got := excerpt([]byte("  usage: bad flag\n\n")); got != "usage: bad flag" {
		t.Errorf("excerpt = %q, want trimmed stderr", got)
	}
	if got := excerpt([]byte("line one\nline two\n")); got != "line one line two" {
		t.Errorf("excerpt = %q, want one log line", got)
	}
	if got := excerpt(nil); got != "(no stderr)" {
		t.Errorf("excerpt(nil) = %q", got)
	}
}
