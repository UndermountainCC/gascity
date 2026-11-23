package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// bdRecordedStatusStore is a work store shaped like a controller's cached bd
// leg. bd records its own parked statuses (blocked, deferred, ...) and Gas
// City reads them folded to "open", keeping the raw one in
// Bead.SourceStatus. A live status-filtered list is `bd list --status=<s>`,
// which matches the raw status, so it leaves a recorded-blocked row out of
// "open". The cache (CachedList) filters on the folded status, so it lists
// the same row as "open".
type bdRecordedStatusStore struct {
	beads.Store
}

func (s bdRecordedStatusStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(q)
	if err != nil || !q.Live || q.Status == "" {
		return rows, err
	}
	var out []beads.Bead
	for _, b := range rows {
		if b.SourceStatus == "" || b.SourceStatus == q.Status {
			out = append(out, b)
		}
	}
	return out, nil
}

func (s bdRecordedStatusStore) CachedList(q beads.ListQuery) ([]beads.Bead, bool) {
	q.Live = false
	rows, err := s.Store.List(q)
	return rows, err == nil
}

// TestSingletonPoolAsleepSeatHoldingOnlyBlockedWorkGivesWayToNewDemand is the
// field outage on a canonical singleton pool (max_active_sessions = 1). The
// pool's only seat went to sleep while it held one claim, which its worker
// had parked with bd status "blocked". New ready work was then routed to the
// pool, and for 35 minutes every tick logged:
//
//	pool session name unavailable: canonical pool template "<pool>" is held by
//	session <seat> (slot stalled on its own runtime name; retrying next tick)
//	close of <seat> refused (has-work): <blocked> was assigned to it since
//	this tick's read
//
// The seat freed only when an operator closed it by hand.
//
// The cause is that the two reads of the pool-slot close disagree. The tick's
// live read (`bd list --status=open`) leaves the recorded-blocked claim out,
// so the seat holds no work and the pool-slot close runs. The close-time
// re-read lists the same claim from the cache as "open" and refuses the close
// as a claim made since the tick's read. No other lane frees the seat: the
// blocked claim is no demand, so nothing wakes the seat either.
//
// With the re-read matching the recorded status, as the live read and the
// cache's own stale sweep do, the seat closes and a fresh seat takes the
// canonical name for the new work within a bounded number of ticks. The
// blocked claim keeps its status and its assignee, the pool's canonical
// identity, which the fresh seat carries: it is that seat's work once it
// unblocks.
func TestSingletonPoolAsleepSeatHoldingOnlyBlockedWorkGivesWayToNewDemand(t *testing.T) {
	for _, tt := range []struct {
		name string
		meta map[string]string
	}{
		{"asleep idle", map[string]string{"state": "asleep", "sleep_reason": "idle"}},
		{"asleep drained", map[string]string{"state": "asleep", "sleep_reason": "drained"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 11, 23, 18, 50, 0, 0, time.UTC)
			clk := &clock.Fake{Time: now}
			cityDir := t.TempDir()
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Session:   config.SessionConfig{Provider: "fake"},
				Agents: []config.Agent{{
					Name:              "worker",
					StartCommand:      "true",
					MinActiveSessions: intPtr(0),
					MaxActiveSessions: intPtr(1),
				}},
			}
			template := cfg.Agents[0].QualifiedName()
			store := bdRecordedStatusStore{Store: beads.NewMemStore()}
			sp := runtime.NewFake()

			seat := createCanonicalPoolSession(t, store, &cfg.Agents[0], now.Add(-3*time.Hour), 0)
			// A seat that ran and went to sleep: no pending create left on it.
			seatMeta := map[string]string{
				"pending_create_claim":      "",
				"pending_create_started_at": "",
				"slept_at":                  now.Add(-2 * time.Hour).Format(time.RFC3339),
			}
			for k, v := range tt.meta {
				seatMeta[k] = v
			}
			if err := store.SetMetadataBatch(seat.ID, seatMeta); err != nil {
				t.Fatalf("putting the seat to sleep: %v", err)
			}
			if got := strings.TrimSpace(seat.Metadata["alias"]); got != template {
				t.Fatalf("seat alias = %q, want the canonical identity %q", got, template)
			}

			// The seat's only claim, which its worker parked with bd status
			// "blocked", held under the pool's canonical identity as a
			// singleton seat's `gc hook --claim` leaves it.
			blocked, err := store.Create(beads.Bead{
				Title:        "claim parked blocked",
				Type:         "task",
				Status:       "open",
				SourceStatus: "blocked",
				Assignee:     template,
				Metadata:     map[string]string{"gc.routed_to": template},
			})
			if err != nil {
				t.Fatalf("seeding the blocked claim: %v", err)
			}
			fresh := createRoutedReadyBeadForReplacement(t, store, template, "new ready work")

			const maxTicks = 3
			var log bytes.Buffer
			tick := func(n int) (served bool) {
				clk.Advance(time.Minute)
				ds := buildDesiredState("test-city", cityDir, clk.Now(), cfg, sp, store, &log)
				sessions, err := loadSessionBeads(store)
				if err != nil {
					t.Fatalf("tick %d: loading session beads: %v", n, err)
				}
				reconcileSessionBeadsAtPath(
					context.Background(), cityDir, sessions, ds.State, configuredSessionNames(cfg, "", store),
					cfg, sp, store, nil, nil, nil, nil, newDrainTracker(), ds.PoolDesiredCounts, false, nil, "test-city",
					nil, clk, events.Discard, 0, 0, &log, &log,
				)
				for _, tp := range ds.State {
					if tp.TemplateName == template && tp.Env["GC_SESSION_ID"] != seat.ID && sp.IsRunning(tp.SessionName) {
						served = true
					}
				}
				return served
			}
			var served bool
			n := 0
			for n < maxTicks && !served {
				n++
				served = tick(n)
			}
			if !served {
				t.Fatalf("no fresh seat started for the new work %s within %d ticks; log:\n%s", fresh.ID, maxTicks, log.String())
			}
			// Later ticks leave the blocked claim where it is: no sweep or
			// release moves it off the identity the fresh seat carries.
			for extra := 0; extra < 3; extra++ {
				n++
				tick(n)
			}

			old, err := store.Get(seat.ID)
			if err != nil {
				t.Fatalf("re-reading the old seat: %v", err)
			}
			if old.Status != "closed" {
				t.Fatalf("old seat status = %q, want closed: it held the canonical name; log:\n%s", old.Status, log.String())
			}
			if strings.Contains(log.String(), "refused (has-work)") {
				t.Fatalf("log has a has-work refusal for a seat whose only claim is recorded blocked:\n%s", log.String())
			}

			// The blocked claim is not dropped: it keeps its recorded status and
			// stays with the pool's canonical identity.
			got, err := store.Get(blocked.ID)
			if err != nil {
				t.Fatalf("re-reading the blocked claim: %v", err)
			}
			if got.SourceStatus != "blocked" || got.Status != "open" || got.Assignee != template {
				t.Fatalf("blocked claim = status %q (recorded %q) assignee %q, want open (recorded blocked) assigned to %q",
					got.Status, got.SourceStatus, got.Assignee, template)
			}
			var successor beads.Bead
			rows, err := loadSessionBeads(store)
			if err != nil {
				t.Fatalf("loading session beads: %v", err)
			}
			for _, b := range rows {
				if b.ID != seat.ID && b.Status != "closed" && b.Metadata["template"] == template {
					successor = b
				}
			}
			if successor.ID == "" {
				t.Fatalf("no open successor seat for %s; rows=%v", template, rows)
			}
			if alias := strings.TrimSpace(successor.Metadata["alias"]); alias != template {
				t.Fatalf("successor alias = %q, want %q: the blocked claim's assignee must be its identity", alias, template)
			}
		})
	}
}

// TestGatedCloseReReadMatchesTheRecordedStatus pins the gated close's re-read
// rule on its own: a cached row bd records under a status the tick's read does
// not ask for is not a claim made since that read, while a row recorded under
// an asked status, or with no recorded status, still refuses the close. The
// raw re-read (closeTimeLister), which a releasing close's cascade uses, still
// lists every row the cache matches.
func TestGatedCloseReReadMatchesTheRecordedStatus(t *testing.T) {
	for _, tt := range []struct {
		name         string
		status       string
		sourceStatus string
		wantHit      bool
	}{
		{"recorded blocked", "open", "blocked", false},
		{"recorded deferred", "open", "deferred", false},
		{"recorded open", "open", "open", true},
		{"no recorded status", "open", "", true},
		{"in progress", "in_progress", "in_progress", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := bdRecordedStatusStore{Store: beads.NewMemStore()}
			claim, err := store.Create(beads.Bead{Title: "claim", Type: "task", SourceStatus: tt.sourceStatus, Assignee: "seat-1"})
			if err != nil {
				t.Fatal(err)
			}
			if tt.status != "open" {
				status := tt.status
				if err := store.Update(claim.ID, beads.UpdateOpts{Status: &status}); err != nil {
					t.Fatal(err)
				}
			}
			list := closeTimeLister(store, true)
			if list == nil {
				t.Fatal("closeTimeLister returned no lister for a cached work leg")
			}
			items, err := list(beads.ListQuery{Assignee: "seat-1", Status: tt.status, TierMode: beads.TierBoth})
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 {
				t.Fatalf("raw re-read listed %d rows, want the cached claim (rows %+v)", len(items), items)
			}
			hits := heldUnderRecordedStatus([]seatWorkHit{{store: store, bead: items[0]}}, seatWorkStatuses)
			if got := len(hits) > 0; got != tt.wantHit {
				t.Fatalf("gated re-read hit = %v, want %v (rows %+v)", got, tt.wantHit, items)
			}
		})
	}
}
