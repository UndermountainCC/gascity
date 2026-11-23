// Package poolplan contains pure planning policy for agent pools.
package poolplan

import (
	"sort"
	"sync"
)

// Demand describes one pool template's fresh-session demand. Each template
// must appear at most once, and slice order defines seed-rotation order.
type Demand struct {
	Template     string
	FreshCreates int
	// HasFloor reports that at least one fresh create satisfies a configured
	// floor. It reserves at most one floor token; remaining demand participates
	// in the general round-robin rather than the elastic reserve.
	HasFloor bool
	// Deferred counts the consecutive planning cycles in which the budget
	// refused this template a fresh create. A deferred template gets one
	// token before floor, elastic and round-robin shares, most-deferred
	// first, so a pool that keeps losing the rotation still starts a session
	// within a bounded number of cycles. Zero leaves the shares unchanged.
	Deferred int
}

// CreateBudget coordinates a shared limit across concurrent pool creates.
type CreateBudget struct {
	mu                sync.Mutex
	remaining         int
	templateRemaining map[string]int
	spare             int
}

// NewCreateBudget returns a budget with limit tokens. A non-positive limit
// disables budgeting; methods on the returned nil budget preserve unlimited
// behavior.
func NewCreateBudget(limit int) *CreateBudget {
	if limit <= 0 {
		return nil
	}
	return &CreateBudget{remaining: limit}
}

// ConfigureFairShare reserves the remaining tokens across unique, ordered pool
// templates. Deferred templates are served first; seed rotation then keeps
// stable input order from favoring the same template on every planning cycle.
// ConfigureFairShare must run before claims; reconfiguration distributes only
// the capacity that remains.
func (b *CreateBudget) ConfigureFairShare(demands []Demand, seed uint64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.templateRemaining, b.spare = fairShares(demands, b.remaining, seed)
}

// TryClaim atomically claims a token assigned to template or an unassigned
// spare token.
func (b *CreateBudget) TryClaim(template string) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.remaining <= 0 {
		return false
	}
	if b.templateRemaining != nil {
		switch {
		case b.templateRemaining[template] > 0:
			b.templateRemaining[template]--
		case b.spare > 0:
			b.spare--
		default:
			return false
		}
	}
	b.remaining--
	return true
}

// Release refunds one successfully claimed token as fungible capacity reusable
// by any template. Callers must release exactly once per failed claimed create;
// CreateBudget does not guard against over-release.
func (b *CreateBudget) Release() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.remaining++
	if b.templateRemaining != nil {
		b.spare++
	}
}

func fairShares(demands []Demand, limit int, seed uint64) (map[string]int, int) {
	if limit <= 0 {
		return nil, 0
	}
	active := make([]Demand, 0, len(demands))
	for _, demand := range demands {
		if demand.FreshCreates > 0 {
			active = append(active, demand)
		}
	}
	if len(active) <= 1 {
		return nil, 0
	}

	shares := make(map[string]int, len(active))
	remaining := limit
	start := int(seed % uint64(len(active)))

	// Give one token to each deferred template, most-deferred first and
	// seed-rotated among equals, before any other share. A token given here
	// counts as a floor template's floor token.
	floorUsed := 0
	deferred := make([]Demand, 0, len(active))
	for offset := 0; offset < len(active); offset++ {
		if demand := active[(start+offset)%len(active)]; demand.Deferred > 0 {
			deferred = append(deferred, demand)
		}
	}
	sort.SliceStable(deferred, func(i, j int) bool { return deferred[i].Deferred > deferred[j].Deferred })
	for _, demand := range deferred {
		if remaining <= 0 {
			break
		}
		shares[demand.Template]++
		remaining--
		if demand.HasFloor {
			floorUsed++
		}
	}

	// Reserve one quarter of the limit for non-floor demand. Floor-bearing
	// templates retain priority while large floor sets cannot starve elastic
	// pools. Limits below four preserve strict floor-first behavior.
	elasticDemand := 0
	for _, demand := range active {
		if !demand.HasFloor {
			elasticDemand += demand.FreshCreates
		}
	}
	elasticReserve := limit / 4
	if elasticReserve > elasticDemand {
		elasticReserve = elasticDemand
	}

	// Reserve one token per floor-bearing template in seed-rotated order.
	floorBudget := limit - elasticReserve
	for offset := 0; offset < len(active); offset++ {
		if floorUsed >= floorBudget || remaining <= 0 {
			break
		}
		demand := active[(start+offset)%len(active)]
		if demand.HasFloor && shares[demand.Template] == 0 {
			shares[demand.Template]++
			remaining--
			floorUsed++
		}
	}

	// Give the reserved elastic slice only to non-floor demand before the
	// general round-robin can consume it.
	elasticGiven := 0
	for elasticGiven < elasticReserve && remaining > 0 {
		progressed := false
		for offset := 0; offset < len(active) && remaining > 0 && elasticGiven < elasticReserve; offset++ {
			demand := active[(start+offset)%len(active)]
			if demand.HasFloor || shares[demand.Template] >= demand.FreshCreates {
				continue
			}
			shares[demand.Template]++
			remaining--
			elasticGiven++
			progressed = true
		}
		if !progressed {
			break
		}
	}

	// Distribute the rest round-robin, capped by each template's demand.
	for remaining > 0 {
		progressed := false
		for offset := 0; offset < len(active) && remaining > 0; offset++ {
			demand := active[(start+offset)%len(active)]
			if shares[demand.Template] >= demand.FreshCreates {
				continue
			}
			shares[demand.Template]++
			remaining--
			progressed = true
		}
		if !progressed {
			break
		}
	}
	return shares, remaining
}
