package runtime

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

const (
	// HeartbeatInterval is how often a lock owner renews its heartbeat.
	HeartbeatInterval = 5 * time.Second
	// HeartbeatTTL is how stale a heartbeat may be before the lock is considered
	// orphaned. It must comfortably exceed HeartbeatInterval.
	HeartbeatTTL = 30 * time.Second
)

// ErrLocked is returned when a run is already held by a live owner.
var ErrLocked = errors.New("run is locked by a live process")

// LockStatus describes the current lock on a run without acquiring it.
type LockStatus struct {
	Present  bool
	Orphaned bool // present but the owner is gone or its heartbeat expired
	Lock     core.Lock
}

// InspectLock reports the lock state of a run without acquiring it. Used by
// `vichu status` to surface orphaned locks.
func (s *Store) InspectLock(runID string) (LockStatus, error) {
	var lk core.Lock
	if err := readJSON(s.lockPath(runID), &lk); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return LockStatus{Present: false}, nil
		}
		return LockStatus{}, err
	}
	orphaned := lk.Expired(time.Now().UTC(), HeartbeatTTL) || !ownerAlive(lk)
	return LockStatus{Present: true, Orphaned: orphaned, Lock: lk}, nil
}

// Handle is an acquired lock. The owner must call Release when done and may run
// StartHeartbeat to keep it fresh for the duration of a run.
type Handle struct {
	store *Store
	runID string

	mu   sync.Mutex
	lock core.Lock
}

// AcquireLock takes the lock for a run. It succeeds if no lock exists or the
// existing lock is orphaned (owner process gone or heartbeat expired);
// otherwise it returns ErrLocked.
func (s *Store) AcquireLock(runID string) (*Handle, error) {
	status, err := s.InspectLock(runID)
	if err != nil {
		return nil, err
	}
	if status.Present && !status.Orphaned {
		return nil, ErrLocked
	}

	host, _ := os.Hostname()
	now := time.Now().UTC()
	lk := core.Lock{
		PID:         os.Getpid(),
		Hostname:    host,
		RunID:       runID,
		AcquiredAt:  now,
		HeartbeatAt: now,
	}
	if err := writeJSON(s.lockPath(runID), &lk); err != nil {
		return nil, err
	}
	return &Handle{store: s, runID: runID, lock: lk}, nil
}

// Heartbeat renews the lock's heartbeat timestamp on disk.
func (h *Handle) Heartbeat() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lock.HeartbeatAt = time.Now().UTC()
	return writeJSON(h.store.lockPath(h.runID), &h.lock)
}

// StartHeartbeat renews the lock on an interval until ctx is canceled. Run it in
// a goroutine for the lifetime of a run.
func (h *Handle) StartHeartbeat(ctx context.Context) {
	t := time.NewTicker(HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = h.Heartbeat()
		}
	}
}

// Release removes the lock file. Safe to call multiple times.
func (h *Handle) Release() error {
	err := os.Remove(h.store.lockPath(h.runID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ownerAlive reports whether the process that holds a lock appears to still be
// running. It is only meaningful when the lock was taken on this host; for a
// lock from another host we conservatively assume the owner is alive and rely on
// heartbeat expiry (HeartbeatTTL) to reclaim it.
func ownerAlive(lk core.Lock) bool {
	host, _ := os.Hostname()
	if lk.Hostname != "" && lk.Hostname != host {
		return true
	}
	return processAlive(lk.PID)
}
