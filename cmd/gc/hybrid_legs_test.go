package main

import (
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionherdr "github.com/gastownhall/gascity/internal/runtime/herdr"
	"github.com/gastownhall/gascity/internal/runtime/registry"
)

// legCall records one leg factory invocation.
type legCall struct {
	name     string
	sc       config.SessionConfig
	cityName string
	cityPath string
	provider runtime.Provider
}

// legRecorder is a test registry whose builtin-named factories return a
// distinct fake per call and record their arguments, so the tests can tell
// which factory built each hybrid leg without constructing real backends.
type legRecorder struct {
	reg   *registry.Registry
	calls []legCall
}

func newLegRecorder(t *testing.T) *legRecorder {
	t.Helper()
	rec := &legRecorder{reg: registry.New()}
	for _, name := range []string{"tmux", "k8s", "herdr", "fake", "fail"} {
		if err := rec.reg.Register(name, func(n string, sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error) {
			p := runtime.NewFake()
			rec.calls = append(rec.calls, legCall{name: n, sc: sc, cityName: cityName, cityPath: cityPath, provider: p})
			return p, nil
		}); err != nil {
			t.Fatalf("Register(%q): %v", name, err)
		}
	}
	return rec
}

// built returns the provider the recorder built for name, failing the test
// when that factory was not called exactly once.
func (rec *legRecorder) built(t *testing.T, name string) legCall {
	t.Helper()
	var found []legCall
	for _, c := range rec.calls {
		if c.name == name {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("factory %q called %d times, want 1 (calls: %+v)", name, len(found), rec.callNames())
	}
	return found[0]
}

func (rec *legRecorder) callNames() []string {
	names := make([]string, 0, len(rec.calls))
	for _, c := range rec.calls {
		names = append(names, c.name)
	}
	return names
}

func hybridRouter(t *testing.T, p runtime.Provider) runtime.Router {
	t.Helper()
	r, ok := p.(runtime.Router)
	if !ok {
		t.Fatalf("hybrid provider %T does not implement runtime.Router", p)
	}
	return r
}

func assertRoute(t *testing.T, r runtime.Router, session, wantLabel string, want runtime.Provider) {
	t.Helper()
	got := r.RouteFor(session)
	if got.Label != wantLabel {
		t.Errorf("RouteFor(%q).Label = %q, want %q", session, got.Label, wantLabel)
	}
	if got.Provider != want {
		t.Errorf("RouteFor(%q).Provider = %p, want %p", session, got.Provider, want)
	}
}

func TestNewHybridProviderDefaultLegs(t *testing.T) {
	t.Setenv("GC_HYBRID_REMOTE_MATCH", "")
	rec := newLegRecorder(t)
	sc := config.SessionConfig{Provider: "hybrid", RemoteMatch: "worker", Socket: "sock"}
	p, err := newHybridProvider(rec.reg, sc, "city", "/city/path")
	if err != nil {
		t.Fatalf("newHybridProvider: %v", err)
	}
	if got, want := strings.Join(rec.callNames(), ","), "tmux,k8s"; got != want {
		t.Fatalf("factory calls = %s, want %s (local leg first, then remote)", got, want)
	}
	local := rec.built(t, "tmux")
	remote := rec.built(t, "k8s")
	for _, c := range []legCall{local, remote} {
		if c.sc != sc || c.cityName != "city" || c.cityPath != "/city/path" {
			t.Errorf("factory %q got (sc=%+v, city=%q, path=%q), want the hybrid's own arguments", c.name, c.sc, c.cityName, c.cityPath)
		}
	}
	r := hybridRouter(t, p)
	assertRoute(t, r, "rig--worker-1", "remote", remote.provider)
	assertRoute(t, r, "rig--lead", "local", local.provider)
}

func TestNewHybridProviderConfiguredLegs(t *testing.T) {
	t.Setenv("GC_HYBRID_REMOTE_MATCH", "")
	t.Run("both legs", func(t *testing.T) {
		rec := newLegRecorder(t)
		sc := config.SessionConfig{RemoteMatch: "pod-", Hybrid: config.HybridSessionConfig{Local: "fake", Remote: "fail"}}
		p, err := newHybridProvider(rec.reg, sc, "city", "/city/path")
		if err != nil {
			t.Fatalf("newHybridProvider: %v", err)
		}
		r := hybridRouter(t, p)
		assertRoute(t, r, "rig--pod-a", "remote", rec.built(t, "fail").provider)
		assertRoute(t, r, "rig--lead", "local", rec.built(t, "fake").provider)
	})
	t.Run("only remote set keeps tmux local", func(t *testing.T) {
		rec := newLegRecorder(t)
		sc := config.SessionConfig{RemoteMatch: "pod-", Hybrid: config.HybridSessionConfig{Remote: "fail"}}
		p, err := newHybridProvider(rec.reg, sc, "city", "/city/path")
		if err != nil {
			t.Fatalf("newHybridProvider: %v", err)
		}
		r := hybridRouter(t, p)
		assertRoute(t, r, "rig--pod-a", "remote", rec.built(t, "fail").provider)
		assertRoute(t, r, "rig--lead", "local", rec.built(t, "tmux").provider)
	})
	t.Run("only local herdr keeps k8s remote", func(t *testing.T) {
		rec := newLegRecorder(t)
		sc := config.SessionConfig{RemoteMatch: "pod-", Hybrid: config.HybridSessionConfig{Local: "herdr"}}
		p, err := newHybridProvider(rec.reg, sc, "city", "/city/path")
		if err != nil {
			t.Fatalf("newHybridProvider: %v", err)
		}
		r := hybridRouter(t, p)
		assertRoute(t, r, "rig--pod-a", "remote", rec.built(t, "k8s").provider)
		assertRoute(t, r, "rig--lead", "local", rec.built(t, "herdr").provider)
	})
}

func TestNewHybridProviderHerdrLocalLegIsHerdr(t *testing.T) {
	t.Setenv("GC_HYBRID_REMOTE_MATCH", "")
	sc := config.SessionConfig{RemoteMatch: "pod-", Hybrid: config.HybridSessionConfig{Local: "herdr", Remote: "fake"}}
	p, err := newHybridProvider(buildRuntimeRegistry(), sc, "city", t.TempDir())
	if err != nil {
		t.Fatalf("newHybridProvider: %v", err)
	}
	r := hybridRouter(t, p)
	if got := r.RouteFor("rig--lead"); got.Label != "local" {
		t.Fatalf("RouteFor(rig--lead).Label = %q, want local", got.Label)
	} else if _, ok := got.Provider.(*sessionherdr.Provider); !ok {
		t.Errorf("local leg = %T, want *herdr.Provider", got.Provider)
	}
	if got := r.RouteFor("rig--pod-a"); got.Label != "remote" {
		t.Errorf("RouteFor(rig--pod-a).Label = %q, want remote", got.Label)
	} else if _, ok := got.Provider.(*runtime.Fake); !ok {
		t.Errorf("remote leg = %T, want *runtime.Fake", got.Provider)
	}
}

func TestNewHybridProviderRoutingPrecedence(t *testing.T) {
	cases := []struct {
		name        string
		env         *string
		remoteMatch string
		remote      []string
		local       []string
	}{
		{name: "env wins over remote_match", env: strPtr("lead"), remoteMatch: "worker", remote: []string{"rig--lead"}, local: []string{"rig--worker-1"}},
		{name: "remote_match only", remoteMatch: "worker", remote: []string{"rig--worker-1"}, local: []string{"rig--lead"}},
		{name: "neither set routes everything local", local: []string{"rig--worker-1", "rig--lead"}},
		{name: "empty env keeps remote_match", env: strPtr(""), remoteMatch: "worker", remote: []string{"rig--worker-1"}, local: []string{"rig--lead"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != nil {
				t.Setenv("GC_HYBRID_REMOTE_MATCH", *tc.env)
			} else {
				// t.Setenv restores the original value after the test.
				t.Setenv("GC_HYBRID_REMOTE_MATCH", "")
				if err := os.Unsetenv("GC_HYBRID_REMOTE_MATCH"); err != nil {
					t.Fatalf("Unsetenv: %v", err)
				}
			}
			rec := newLegRecorder(t)
			sc := config.SessionConfig{RemoteMatch: tc.remoteMatch, Hybrid: config.HybridSessionConfig{Local: "fake", Remote: "fail"}}
			p, err := newHybridProvider(rec.reg, sc, "city", "/city/path")
			if err != nil {
				t.Fatalf("newHybridProvider: %v", err)
			}
			r := hybridRouter(t, p)
			for _, name := range tc.remote {
				assertRoute(t, r, name, "remote", rec.built(t, "fail").provider)
			}
			for _, name := range tc.local {
				assertRoute(t, r, name, "local", rec.built(t, "fake").provider)
			}
		})
	}
}

func TestNewHybridProviderRejectsInvalidLegs(t *testing.T) {
	t.Setenv("GC_HYBRID_REMOTE_MATCH", "")
	cases := []struct {
		name   string
		legs   config.HybridSessionConfig
		want   []string
		reason string
	}{
		{name: "hybrid local", legs: config.HybridSessionConfig{Local: "hybrid"}, want: []string{`[session.hybrid] local = "hybrid"`}, reason: "hybrid cannot be its own leg"},
		{name: "hybrid remote", legs: config.HybridSessionConfig{Remote: "hybrid"}, want: []string{`[session.hybrid] remote = "hybrid"`}, reason: "hybrid cannot be its own leg"},
		{name: "unknown local", legs: config.HybridSessionConfig{Local: "hrdr"}, want: []string{`[session.hybrid] local = "hrdr"`}, reason: "not a builtin session runtime"},
		{name: "unknown remote", legs: config.HybridSessionConfig{Remote: "k8z"}, want: []string{`[session.hybrid] remote = "k8z"`}, reason: "not a builtin session runtime"},
		{name: "whitespace", legs: config.HybridSessionConfig{Local: " herdr"}, want: []string{`[session.hybrid] local = " herdr"`}, reason: "surrounding whitespace"},
		{name: "whitespace before hybrid check", legs: config.HybridSessionConfig{Local: " hybrid"}, want: []string{`[session.hybrid] local = " hybrid"`}, reason: "surrounding whitespace"},
		{name: "same as defaulted remote", legs: config.HybridSessionConfig{Local: "k8s"}, want: []string{`[session.hybrid] local = "k8s"`, `remote = "k8s"`}, reason: "the legs must differ"},
		{name: "same configured legs", legs: config.HybridSessionConfig{Local: "fake", Remote: "fake"}, want: []string{`[session.hybrid] local = "fake"`, `remote = "fake"`}, reason: "the legs must differ"},
		{name: "pack-only name", legs: config.HybridSessionConfig{Local: "box"}, want: []string{`[session.hybrid] local = "box"`}, reason: "pack-declared"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newLegRecorder(t)
			p, err := newHybridProvider(rec.reg, config.SessionConfig{Hybrid: tc.legs}, "city", "/city/path")
			if err == nil {
				t.Fatalf("newHybridProvider succeeded, want an error")
			}
			if p != nil {
				t.Errorf("newHybridProvider returned provider %T with its error, want nil", p)
			}
			for _, want := range append(tc.want, tc.reason) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if len(rec.calls) != 0 {
				t.Errorf("factories called before validation failed: %v", rec.callNames())
			}
		})
	}
}

func TestRuntimeRegistryHybridResolvesLegsAgainstBuiltins(t *testing.T) {
	t.Setenv("GC_HYBRID_REMOTE_MATCH", "")
	cfg := &config.City{Runtimes: map[string]config.DiscoveredRuntime{
		"box": {Name: "box", Command: "/bin/true", PackName: "boxpack"},
	}}
	reg, err := runtimeRegistryForCity(cfg)
	if err != nil {
		t.Fatalf("runtimeRegistryForCity: %v", err)
	}
	if !reg.Resolves("box") {
		t.Fatalf("per-city registry does not resolve the pack runtime %q", "box")
	}
	sc := config.SessionConfig{Provider: "hybrid", Hybrid: config.HybridSessionConfig{Local: "box", Remote: "fake"}}
	p, err := reg.New("hybrid", sc, "city", t.TempDir())
	if err == nil {
		t.Fatalf("hybrid with pack-declared local leg built %T, want an error", p)
	}
	for _, want := range []string{`[session.hybrid] local = "box"`, "pack-declared"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}
