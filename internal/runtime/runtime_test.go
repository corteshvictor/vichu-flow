package runtime

import (
	"os"
	"testing"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

func newState(runID string) *core.State {
	return &core.State{
		RunID:        runID,
		Status:       core.StatusActive,
		Workflow:     "quick",
		Task:         "do a thing",
		CurrentStage: "explore",
		Stages:       map[string]core.StageStatus{"explore": core.StageActive},
	}
}

func TestStateRoundTrip(t *testing.T) {
	s := Open(t.TempDir())
	st := newState("run-1")
	if err := s.CreateRun(st); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if st.SchemaVersion != core.SchemaVersion {
		t.Fatalf("schema version not stamped: %d", st.SchemaVersion)
	}
	if st.CreatedAt.IsZero() || st.UpdatedAt.IsZero() {
		t.Fatal("timestamps not stamped")
	}

	got, err := s.LoadState("run-1")
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.RunID != "run-1" || got.Workflow != "quick" || got.Status != core.StatusActive {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestLoadMissingRun(t *testing.T) {
	s := Open(t.TempDir())
	if _, err := s.LoadState("nope"); err != ErrRunNotFound {
		t.Fatalf("want ErrRunNotFound, got %v", err)
	}
}

func TestEventsAppendOnly(t *testing.T) {
	s := Open(t.TempDir())
	for i := 0; i < 3; i++ {
		if err := s.AppendEvent(core.Event{Run: "run-1", Event: core.EventStageStarted}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	evs, err := s.ReadEvents("run-1")
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("want 3 events, got %d", len(evs))
	}
	for _, ev := range evs {
		if ev.TS.IsZero() {
			t.Fatal("event timestamp not stamped")
		}
	}
}

func TestListRunsNewestFirst(t *testing.T) {
	s := Open(t.TempDir())
	for _, id := range []string{"run-20240101-000001-aa", "run-20240101-000002-bb"} {
		if err := s.CreateRun(newState(id)); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := s.ListRuns()
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(ids) != 2 || ids[0] != "run-20240101-000002-bb" {
		t.Fatalf("want newest first, got %v", ids)
	}
}

func TestLockAcquireAndConflict(t *testing.T) {
	s := Open(t.TempDir())
	if err := s.CreateRun(newState("run-1")); err != nil {
		t.Fatal(err)
	}
	h, err := s.AcquireLock("run-1")
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	// A second acquire against a live owner (this very process) must fail.
	if _, err := s.AcquireLock("run-1"); err != ErrLocked {
		t.Fatalf("want ErrLocked, got %v", err)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// After release, acquisition succeeds again.
	if _, err := s.AcquireLock("run-1"); err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
}

func TestLockOrphanReclaimedByExpiredHeartbeat(t *testing.T) {
	s := Open(t.TempDir())
	if err := s.CreateRun(newState("run-1")); err != nil {
		t.Fatal(err)
	}
	// Write a lock from a dead pid with a stale heartbeat.
	stale := core.Lock{
		PID:         999999,
		Hostname:    hostname(),
		RunID:       "run-1",
		AcquiredAt:  time.Now().Add(-time.Hour),
		HeartbeatAt: time.Now().Add(-time.Hour),
	}
	if err := writeJSON(s.lockPath("run-1"), &stale); err != nil {
		t.Fatal(err)
	}
	st, err := s.InspectLock("run-1")
	if err != nil {
		t.Fatalf("InspectLock: %v", err)
	}
	if !st.Present || !st.Orphaned {
		t.Fatalf("stale lock should be present+orphaned, got %+v", st)
	}
	// Acquiring over an orphaned lock must succeed.
	if _, err := s.AcquireLock("run-1"); err != nil {
		t.Fatalf("acquire over orphan: %v", err)
	}
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}
