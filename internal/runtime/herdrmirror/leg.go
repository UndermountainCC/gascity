package herdrmirror

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// forwarded is the set of optional interfaces the k8s leaf implements, which
// a Leg forwards unchanged.
type forwarded interface {
	runtime.Provider
	runtime.SleepCapabilityProvider
	runtime.RelaunchProvider
	runtime.ListingAttestation
	runtime.SharedListing
}

// Leg is a remote runtime leaf whose sessions are mirrored in herdr: Start
// schedules a mirror tab after the leaf starts the session, and Stop closes
// it after the leaf stops it. Everything else is the leaf's own.
type Leg struct {
	runtime.Provider
	inner  forwarded
	mirror *Mirror
}

// Wrap returns inner with its sessions mirrored by m. inner must implement
// every optional interface a Leg forwards, so wrapping never hides one of
// the leaf's capabilities.
func Wrap(inner runtime.Provider, m *Mirror) (*Leg, error) {
	if inner == nil {
		return nil, errors.New("herdr mirror: nil leaf")
	}
	if m == nil {
		return nil, errors.New("herdr mirror: nil mirror")
	}
	for _, need := range []struct {
		name string
		ok   bool
	}{
		{"SleepCapabilityProvider", implements[runtime.SleepCapabilityProvider](inner)},
		{"RelaunchProvider", implements[runtime.RelaunchProvider](inner)},
		{"ListingAttestation", implements[runtime.ListingAttestation](inner)},
		{"SharedListing", implements[runtime.SharedListing](inner)},
	} {
		if !need.ok {
			return nil, fmt.Errorf("herdr mirror: leaf %T does not implement runtime.%s", inner, need.name)
		}
	}
	f, ok := inner.(forwarded)
	if !ok {
		return nil, fmt.Errorf("herdr mirror: leaf %T does not implement the forwarded interfaces", inner)
	}
	return &Leg{Provider: inner, inner: f, mirror: m}, nil
}

func implements[T any](p runtime.Provider) bool {
	_, ok := p.(T)
	return ok
}

// Start starts the session on the leaf and, when that succeeds, schedules
// its mirror tab. It returns the leaf's result; the mirror never fails it.
func (l *Leg) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := l.inner.Start(ctx, name, cfg); err != nil {
		return err
	}
	l.mirror.openAsync(name)
	return nil
}

// Stop stops the session on the leaf and then closes its mirror tab, unless
// the leaf's error says the session may still be running. It returns the
// leaf's error unchanged.
func (l *Leg) Stop(name string) error {
	err := l.inner.Stop(name)
	l.mirror.closeAfterStop(name, err)
	return err
}

// SleepCapability forwards to the leaf.
func (l *Leg) SleepCapability(name string) runtime.SessionSleepCapability {
	return l.inner.SleepCapability(name)
}

// Relaunch forwards to the leaf. The mirror tab is left as it is: its
// supervisor follows the pod.
func (l *Leg) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	return l.inner.Relaunch(ctx, name, cfg)
}

// ListRunningComplete forwards to the leaf.
func (l *Leg) ListRunningComplete() bool { return l.inner.ListRunningComplete() }

// ListRunningShared forwards to the leaf.
func (l *Leg) ListRunningShared() bool { return l.inner.ListRunningShared() }
