package runtime

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
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

// TestLockConcurrentAcquireOnlyOneWins races many goroutines on the same run;
// the atomic (link-based) acquisition must let exactly one win.
func TestLockConcurrentAcquireOnlyOneWins(t *testing.T) {
	s := Open(t.TempDir())
	if err := s.CreateRun(newState("run-1")); err != nil {
		t.Fatal(err)
	}
	const n = 24
	var wins int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.AcquireLock("run-1"); err == nil {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("exactly one concurrent acquire must win, got %d", wins)
	}
}

// TestReleaseDoesNotDeleteReclaimedLock: once an orphaned lock is reclaimed by a
// new owner (different token), the old handle's Release must not delete it.
func TestReleaseDoesNotDeleteReclaimedLock(t *testing.T) {
	s := Open(t.TempDir())
	if err := s.CreateRun(newState("run-1")); err != nil {
		t.Fatal(err)
	}
	h1, err := s.AcquireLock("run-1")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a second owner reclaiming: overwrite with a different token.
	reclaimed := core.Lock{
		PID: os.Getpid(), Hostname: hostname(), RunID: "run-1",
		Token: "other-owner-token", AcquiredAt: time.Now().UTC(), HeartbeatAt: time.Now().UTC(),
	}
	if err := writeJSON(s.lockPath("run-1"), &reclaimed); err != nil {
		t.Fatal(err)
	}
	if err := h1.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	st, err := s.InspectLock("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Present || st.Lock.Token != "other-owner-token" {
		t.Fatalf("Release must not delete a lock owned by a different token, got %+v", st)
	}
}

// TestHeartbeatReturnsLockLostOnReclaim: once another process reclaims the lock
// (different token), Heartbeat reports ErrLockLost instead of clobbering it.
func TestHeartbeatReturnsLockLostOnReclaim(t *testing.T) {
	s := Open(t.TempDir())
	if err := s.CreateRun(newState("run-1")); err != nil {
		t.Fatal(err)
	}
	h, err := s.AcquireLock("run-1")
	if err != nil {
		t.Fatal(err)
	}
	reclaimed := &core.Lock{RunID: "run-1", Token: "other-owner", HeartbeatAt: time.Now().UTC()}
	if err := writeJSON(s.lockPath("run-1"), reclaimed); err != nil {
		t.Fatal(err)
	}
	if err := h.Heartbeat(); !errors.Is(err, ErrLockLost) {
		t.Fatalf("want ErrLockLost after reclaim, got %v", err)
	}
}

// TestStartHeartbeatSignalsLockLoss: StartHeartbeat invokes onLost (so the engine
// can stop the run) when the lock is reclaimed by another process.
func TestStartHeartbeatSignalsLockLoss(t *testing.T) {
	old := HeartbeatInterval
	HeartbeatInterval = 5 * time.Millisecond
	defer func() { HeartbeatInterval = old }()

	s := Open(t.TempDir())
	if err := s.CreateRun(newState("run-1")); err != nil {
		t.Fatal(err)
	}
	h, err := s.AcquireLock("run-1")
	if err != nil {
		t.Fatal(err)
	}
	reclaimed := &core.Lock{RunID: "run-1", Token: "other-owner", HeartbeatAt: time.Now().UTC()}
	if err := writeJSON(s.lockPath("run-1"), reclaimed); err != nil {
		t.Fatal(err)
	}

	lost := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.StartHeartbeat(ctx, func() { lost <- struct{}{} })

	select {
	case <-lost:
	case <-time.After(2 * time.Second):
		t.Fatal("StartHeartbeat should have signaled lock loss")
	}
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}
