package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// HeartbeatInterval is how often a lock owner renews its heartbeat. It is a var
// (not a const) so tests can shrink it.
var HeartbeatInterval = 5 * time.Second

// HeartbeatTTL is how stale a heartbeat may be before the lock is considered
// orphaned. It must comfortably exceed HeartbeatInterval.
const HeartbeatTTL = 30 * time.Second

// ErrLocked is returned when a run is already held by a live owner.
var ErrLocked = errors.New("run is locked by a live process")

// ErrLockLost is returned by Heartbeat when the on-disk lock no longer carries
// this handle's token — i.e. another process reclaimed the run.
var ErrLockLost = errors.New("lock ownership lost")

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
// otherwise it returns ErrLocked. Acquisition is atomic: the lock file is
// hard-linked into place, so two racing processes can never both believe they
// own the run.
func (s *Store) AcquireLock(runID string) (*Handle, error) {
	host, _ := os.Hostname()
	now := time.Now().UTC()
	lk := core.Lock{
		PID:         os.Getpid(),
		Hostname:    host,
		RunID:       runID,
		Token:       randSuffix(16),
		AcquiredAt:  now,
		HeartbeatAt: now,
	}

	// Fast path: create the lock exclusively. Exactly one racer wins.
	err := s.acquireLockFile(runID, &lk)
	if err == nil {
		return &Handle{store: s, runID: runID, lock: lk}, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return nil, err
	}

	// A lock already exists — reclaim it only if it is orphaned.
	status, ierr := s.InspectLock(runID)
	if ierr != nil {
		return nil, ierr
	}
	if status.Present && !status.Orphaned {
		return nil, ErrLocked
	}
	// Orphaned: drop it and re-create exclusively. If another process reclaims
	// first, the exclusive create fails and we report the run as locked rather
	// than stealing it from the new owner.
	if rerr := os.Remove(s.lockPath(runID)); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return nil, rerr
	}
	if err := s.acquireLockFile(runID, &lk); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return &Handle{store: s, runID: runID, lock: lk}, nil
}

// acquireLockFile atomically creates the lock file with lk's content, failing
// with fs.ErrExist if a lock already exists. It writes a temp file then
// hard-links it into place; os.Link is atomic and refuses to clobber an
// existing target, which is what makes acquisition race-free.
func (s *Store) acquireLockFile(runID string, lk *core.Lock) error {
	if err := os.MkdirAll(s.RunDir(runID), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(lk, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.RunDir(runID), ".lock-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, werr := tmp.Write(append(data, '\n')); werr != nil {
		_ = tmp.Close()
		return werr
	}
	if cerr := tmp.Close(); cerr != nil {
		return cerr
	}
	return os.Link(tmpName, s.lockPath(runID))
}

// Heartbeat renews the lock's heartbeat timestamp on disk — but only while this
// handle still owns the lock. If another process reclaimed it (the on-disk token
// changed), it returns ErrLockLost instead of overwriting the new owner's lock.
func (h *Handle) Heartbeat() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.stillOwned() {
		return ErrLockLost
	}
	h.lock.HeartbeatAt = time.Now().UTC()
	return writeJSON(h.store.lockPath(h.runID), &h.lock)
}

// stillOwned reports whether the on-disk lock still carries this handle's token.
func (h *Handle) stillOwned() bool {
	var cur core.Lock
	if err := readJSON(h.store.lockPath(h.runID), &cur); err != nil {
		return false
	}
	return cur.Token != "" && cur.Token == h.lock.Token
}

// StartHeartbeat renews the lock on an interval until ctx is canceled. If the
// lock is lost to another process (Heartbeat returns ErrLockLost), it invokes
// onLost once and stops — the caller uses this to cancel the run rather than keep
// working without ownership. onLost may be nil. Run it in a goroutine for the
// lifetime of a run.
func (h *Handle) StartHeartbeat(ctx context.Context, onLost func()) {
	t := time.NewTicker(HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if errors.Is(h.Heartbeat(), ErrLockLost) {
				if onLost != nil {
					onLost()
				}
				return
			}
		}
	}
}

// Release removes the lock file, but only if this handle still owns it — a
// process whose orphaned lock was reclaimed must NOT delete the new owner's
// lock. Safe to call multiple times.
func (h *Handle) Release() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var cur core.Lock
	if err := readJSON(h.store.lockPath(h.runID), &cur); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if cur.Token != h.lock.Token {
		return nil // a different owner holds it now — never delete their lock
	}
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
