package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime/registry"
)

// Default legs of the hybrid session provider, used when the matching
// [session.hybrid] key is empty.
const (
	hybridDefaultLocalLeg  = "tmux"
	hybridDefaultRemoteLeg = "k8s"
)

// hybridLeg is one effective leg of the hybrid session provider: its
// [session.hybrid] key, the runtime name the registry is asked for, and
// whether the operator set it.
type hybridLeg struct {
	key        string // "local" or "remote"
	name       string // configured value verbatim, else the default
	configured bool
}

// hybridLegs returns the effective legs, remote first: the order the v2
// latch reports them in. It never validates; callers that run before
// construction (readiness, the v2 latch, provider swap) must tolerate
// invalid names.
func hybridLegs(sc config.SessionConfig) (remote, local hybridLeg) {
	remote = hybridLeg{key: "remote", name: hybridDefaultRemoteLeg}
	if sc.Hybrid.Remote != "" {
		remote.name, remote.configured = sc.Hybrid.Remote, true
	}
	local = hybridLeg{key: "local", name: hybridDefaultLocalLeg}
	if sc.Hybrid.Local != "" {
		local.name, local.configured = sc.Hybrid.Local, true
	}
	return remote, local
}

// validateHybridLegs checks both effective legs against reg before any
// provider is built. A leg must be a name reg resolves (an exact or prefix
// registration, never the tmux fallback), must not be "hybrid" or carry
// surrounding whitespace, and the two legs must differ.
func validateHybridLegs(reg *registry.Registry, sc config.SessionConfig) error {
	remote, local := hybridLegs(sc)
	for _, leg := range []hybridLeg{local, remote} {
		if err := validateHybridLeg(reg, leg); err != nil {
			return err
		}
	}
	if local.name == remote.name {
		return fmt.Errorf("[session.hybrid] local = %q and remote = %q name the same runtime: the legs must differ", local.name, remote.name)
	}
	return nil
}

func validateHybridLeg(reg *registry.Registry, leg hybridLeg) error {
	var reason string
	switch {
	case leg.name != strings.TrimSpace(leg.name):
		reason = "the name has surrounding whitespace: remove it"
	case leg.name == "hybrid":
		reason = "hybrid cannot be its own leg"
	case !reg.Resolves(leg.name):
		reason = "not a builtin session runtime (pack-declared [runtimes.<name>] runtimes are not supported as hybrid legs)"
	default:
		return nil
	}
	return fmt.Errorf("[session.hybrid] %s = %q: %s", leg.key, leg.name, reason)
}
