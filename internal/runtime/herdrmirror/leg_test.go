package herdrmirror

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TS-3: the four optional interfaces of the k8s leaf reach the inner leaf
// unchanged, and Relaunch does not touch the mirror (OQ-9).
func TestLegForwardsTheLeafCapabilities(t *testing.T) {
	h := newHarness(t)
	if err := h.start(t, "pod-worker"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	callsBefore := len(h.herdr.allCalls())
	if err := h.leg.Relaunch(context.Background(), "pod-worker", runtime.Config{}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	inner := runtime.NewFakeProfile(runtime.ProfileK8s, runtime.FullFake{Fake: runtime.NewFake()})
	if got, want := h.leg.SleepCapability("pod-worker"), inner.(runtime.SleepCapabilityProvider).SleepCapability("pod-worker"); got != want {
		t.Errorf("SleepCapability = %v, want the leaf's %v", got, want)
	}
	if got, want := h.leg.ListRunningComplete(), inner.(runtime.ListingAttestation).ListRunningComplete(); got != want {
		t.Errorf("ListRunningComplete = %t, want the leaf's %t", got, want)
	}
	if got, want := h.leg.ListRunningShared(), inner.(runtime.SharedListing).ListRunningShared(); got != want {
		t.Errorf("ListRunningShared = %t, want the leaf's %t", got, want)
	}
	var methods []string
	for _, c := range h.leaf.Calls {
		methods = append(methods, c.Method)
	}
	if !slices.Contains(methods, "Relaunch") {
		t.Errorf("leaf calls = %q, want Relaunch forwarded", methods)
	}
	if after := len(h.herdr.allCalls()); after != callsBefore {
		t.Errorf("Relaunch ran mirror calls: %q", h.herdr.argvs()[callsBefore:])
	}
	// The rest of runtime.Provider is the leaf's own.
	running, err := h.leg.ListRunning("")
	if err != nil || !slices.Contains(running, "pod-worker") {
		t.Errorf("ListRunning = %q, %v, want the leaf's listing", running, err)
	}
}

// Stop returns the leaf's own error value, not a wrapped or replaced one.
func TestLegStopReturnsTheLeafError(t *testing.T) {
	h := newHarness(t)
	h.leaf.StopErrors["pod-worker"] = errBoom
	if err := h.leg.Stop("pod-worker"); err != errBoom { //nolint:errorlint // the leaf's error must come back unchanged
		t.Fatalf("Stop = %v, want the leaf's error unchanged", err)
	}
}

type providerOnly struct{ runtime.Provider }

type sleepOnly struct{ providerOnly }

func (sleepOnly) SleepCapability(string) runtime.SessionSleepCapability {
	return runtime.SessionSleepCapabilityTimedOnly
}

type sleepRelaunch struct{ sleepOnly }

func (sleepRelaunch) Relaunch(context.Context, string, runtime.Config) error { return nil }

type sleepRelaunchComplete struct{ sleepRelaunch }

func (sleepRelaunchComplete) ListRunningComplete() bool { return true }

func TestWrapRejectsALeafMissingAForwardedInterface(t *testing.T) {
	m := New(Config{HerdrSession: testSession, Namespace: "gc"})
	base := providerOnly{runtime.NewFake()}
	for _, tc := range []struct {
		inner   runtime.Provider
		missing string
	}{
		{base, "SleepCapabilityProvider"},
		{sleepOnly{base}, "RelaunchProvider"},
		{sleepRelaunch{sleepOnly{base}}, "ListingAttestation"},
		{sleepRelaunchComplete{sleepRelaunch{sleepOnly{base}}}, "SharedListing"},
	} {
		_, err := Wrap(tc.inner, m)
		if err == nil || !strings.Contains(err.Error(), tc.missing) {
			t.Errorf("Wrap(%T) = %v, want an error naming %s", tc.inner, err, tc.missing)
		}
	}
	if _, err := Wrap(nil, m); err == nil {
		t.Error("Wrap(nil) succeeded")
	}
	leaf := runtime.NewFakeProfile(runtime.ProfileK8s, runtime.FullFake{Fake: runtime.NewFake()})
	if _, err := Wrap(leaf, nil); err == nil {
		t.Error("Wrap with a nil mirror succeeded")
	}
	if _, err := Wrap(leaf, m); err != nil {
		t.Errorf("Wrap(k8s profile) = %v, want nil", err)
	}
}
