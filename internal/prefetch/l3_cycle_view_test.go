package prefetch

import (
	"strings"
	"testing"
	"time"
)

// TestL3CycleSnapshot_VisibleOnStatus is the regression test for "I enabled L3
// NP but /debug shows nothing for L3". Before the fix the scheduler tracked a
// live cycle snapshot for L2 (l2Cycles) but not for the now-standalone L3
// layer, so between L1-triggered cycles the L3 panel showed only Phase "—" and
// the operator couldn't tell L3 was scheduled. recordL3CycleStart must surface
// the scheduled wave plan through Status().L3Cycles, advanceL3Wave must move the
// pointer, and dropL3Cycle must clear it on completion.
func TestL3CycleSnapshot_VisibleOnStatus(t *testing.T) {
	s := &Scheduler{Events: NewEventLog(10), queue: make(chan *workItem, 1)}

	offsets := []time.Duration{time.Hour, 3 * time.Hour, 6 * time.Hour, 9 * time.Hour, 11 * time.Hour}
	chunks := []int{10, 8, 10, 10, 10}
	cycleStart := time.Now().Add(-30 * time.Minute)
	const cid = "L3:day:golang:1781703653"

	s.recordL3CycleStart("day", "golang", 82, chunks, cycleStart, 12*time.Hour, offsets, cid)

	cy := s.Status().L3Cycles
	if len(cy) != 1 {
		t.Fatalf("Status().L3Cycles = %d entries, want 1 (L3 cycle must be visible on /debug)", len(cy))
	}
	c := cy[0]
	if c.TF != "day" || c.Sub != "golang" {
		t.Errorf("cycle tf/sub = %q/%q, want day/golang", c.TF, c.Sub)
	}
	if c.WaveCount != 5 {
		t.Errorf("WaveCount = %d, want 5", c.WaveCount)
	}
	if c.PostCount != 82 {
		t.Errorf("PostCount = %d, want 82", c.PostCount)
	}
	if c.CycleID != cid {
		t.Errorf("CycleID = %q, want %q (must be the decoupled L3: lineage)", c.CycleID, cid)
	}
	if c.CurrentWave != 0 {
		t.Errorf("fresh cycle CurrentWave = %d, want 0", c.CurrentWave)
	}
	// Per-wave chunk sizes must ride the interval labels (e.g. "…×10") so the
	// operator sees the ≤10 cap in effect.
	if joined := strings.Join(c.WaveIntervals, " "); !strings.Contains(joined, "×10") {
		t.Errorf("WaveIntervals %q should embed per-wave chunk sizes (×N)", joined)
	}

	// advanceL3Wave with the matching cycle id bumps the pointer.
	s.advanceL3Wave("day", "golang", cid, 3)
	if got := s.Status().L3Cycles[0].CurrentWave; got != 3 {
		t.Errorf("after advance, CurrentWave = %d, want 3", got)
	}
	// A stale cycle id must NOT move a newer cycle's pointer.
	s.advanceL3Wave("day", "golang", "L3:day:golang:OLD", 5)
	if got := s.Status().L3Cycles[0].CurrentWave; got != 3 {
		t.Errorf("stale-cycle advance moved pointer to %d, want 3 (guard failed)", got)
	}

	// dropL3Cycle clears the snapshot so /debug only shows in-flight cycles.
	s.dropL3Cycle("day", "golang", cid)
	if cy := s.Status().L3Cycles; len(cy) != 0 {
		t.Errorf("after drop, L3Cycles = %d entries, want 0", len(cy))
	}
}

// TestL3CycleSnapshot_IndependentOfL2 confirms the L3 and L2 cycle snapshots are
// separate maps — recording one does not appear under the other. This is the
// decoupling guarantee at the /debug layer.
func TestL3CycleSnapshot_IndependentOfL2(t *testing.T) {
	s := &Scheduler{Events: NewEventLog(10), queue: make(chan *workItem, 1)}
	offsets := []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour, 4 * time.Hour, 5 * time.Hour}

	s.recordL2CycleStart("day", "golang", 50, []int{10, 10, 10, 10, 10}, true, time.Now(), 12*time.Hour, offsets, "day:golang:1")
	s.recordL3CycleStart("day", "golang", 50, []int{10, 8, 10, 10, 10}, time.Now(), 12*time.Hour, offsets, "L3:day:golang:2")

	st := s.Status()
	if len(st.L2Cycles) != 1 || st.L2Cycles[0].CycleID != "day:golang:1" {
		t.Errorf("L2Cycles = %+v, want one entry with cycle_id day:golang:1", st.L2Cycles)
	}
	if len(st.L3Cycles) != 1 || st.L3Cycles[0].CycleID != "L3:day:golang:2" {
		t.Errorf("L3Cycles = %+v, want one entry with cycle_id L3:day:golang:2", st.L3Cycles)
	}

	// Dropping the L2 cycle must leave the L3 cycle intact.
	s.dropL2Cycle("day", "golang", "day:golang:1")
	st = s.Status()
	if len(st.L2Cycles) != 0 {
		t.Errorf("after dropL2Cycle, L2Cycles = %d, want 0", len(st.L2Cycles))
	}
	if len(st.L3Cycles) != 1 {
		t.Errorf("dropping L2 cycle wrongly removed the L3 cycle (L3Cycles=%d)", len(st.L3Cycles))
	}
}
