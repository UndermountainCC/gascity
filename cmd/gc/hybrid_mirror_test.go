package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/herdrmirror"
	"github.com/gastownhall/gascity/internal/runtime/registry"
)

// AC-1: the herdr mirror is active only for a herdr local leg with a k8s
// remote leg.
func TestHybridMirrorActive(t *testing.T) {
	for _, tc := range []struct {
		local, remote string
		want          bool
	}{
		{"herdr", "k8s", true},
		{"tmux", "k8s", false},
		{"herdr", "ssh:host", false},
		{"herdr", "exec:x", false},
		{"tmux", "ssh:host", false},
	} {
		if got := hybridMirrorActive(tc.local, tc.remote); got != tc.want {
			t.Errorf("hybridMirrorActive(%q, %q) = %t, want %t", tc.local, tc.remote, got, tc.want)
		}
	}
}

// The mirror's herdr session is the one the herdr runtime uses for the city.
func TestHerdrSessionForCity(t *testing.T) {
	if got := herdrSessionForCity("city"); got != "city" {
		t.Errorf("herdrSessionForCity(city) = %q, want city", got)
	}
	if got := herdrSessionForCity(""); got != "default" {
		t.Errorf("herdrSessionForCity(\"\") = %q, want default", got)
	}
}

// AC-1: newHybridProvider wraps the k8s remote leg in a herdr mirror leg only
// when the local leg is herdr; otherwise the remote backend is the k8s leaf
// itself. A leaf the mirror cannot wrap is kept unwrapped, so the mirror never
// decides whether the provider builds (REQ-007). The mirror is never
// exercised: nothing here starts a session.
func TestNewHybridProviderMirrorsTheK8sLegOnlyUnderHerdr(t *testing.T) {
	k8sProfile := func() runtime.Provider {
		return runtime.NewFakeProfile(runtime.ProfileK8s, runtime.FullFake{Fake: runtime.NewFake()})
	}
	plainFake := func() runtime.Provider { return runtime.NewFake() }
	for _, tc := range []struct {
		name     string
		local    string
		kube     func() runtime.Provider
		mirrored bool
	}{
		{name: "herdr local", local: "herdr", kube: k8sProfile, mirrored: true},
		{name: "tmux local", local: "tmux", kube: k8sProfile, mirrored: false},
		{name: "herdr local, unwrappable leaf", local: "herdr", kube: plainFake, mirrored: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var kube runtime.Provider
			reg := registry.New()
			for _, name := range []string{"tmux", "herdr"} {
				if err := reg.Register(name, func(string, config.SessionConfig, string, string) (runtime.Provider, error) {
					return runtime.NewFake(), nil
				}); err != nil {
					t.Fatalf("Register(%q): %v", name, err)
				}
			}
			if err := reg.Register("k8s", func(string, config.SessionConfig, string, string) (runtime.Provider, error) {
				kube = tc.kube()
				return kube, nil
			}); err != nil {
				t.Fatalf("Register(k8s): %v", err)
			}
			sc := config.SessionConfig{RemoteMatch: "pod-", Hybrid: config.HybridSessionConfig{Local: tc.local, Remote: "k8s"}}
			p, err := newHybridProvider(reg, sc, "city", "/city/path")
			if err != nil {
				t.Fatalf("newHybridProvider: %v", err)
			}
			lister, ok := p.(interface{ Backends() []runtime.Backend })
			if !ok {
				t.Fatalf("hybrid provider %T lists no backends", p)
			}
			var remote runtime.Provider
			for _, b := range lister.Backends() {
				if b.Label == "remote" {
					remote = b.Provider
				}
			}
			leg, mirrored := remote.(*herdrmirror.Leg)
			switch {
			case mirrored != tc.mirrored:
				t.Fatalf("remote backend = %T, mirrored = %t, want %t", remote, mirrored, tc.mirrored)
			case mirrored && leg.Provider != kube:
				t.Fatalf("mirror leg wraps %T, want the k8s leaf", leg.Provider)
			case !mirrored && remote != kube:
				t.Fatalf("remote backend = %T, want the k8s leaf itself", remote)
			}
		})
	}
}
