package core

import "time"

// Lock is written to lock.json by the process that owns a run. The owner
// renews HeartbeatAt periodically; readers treat a lock whose owning process is
// gone, or whose heartbeat has expired, as orphaned and safe to reclaim.
type Lock struct {
	PID      int    `json:"pid"`
	Hostname string `json:"hostname"`
	RunID    string `json:"run_id"`
	// Token is a unique id minted each time the lock is acquired. Heartbeat and
	// Release only act when the on-disk token still matches, so a process that
	// lost an orphaned lock never overwrites or deletes the new owner's lock.
	Token       string    `json:"token,omitempty"`
	AcquiredAt  time.Time `json:"acquired_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
}

// Expired reports whether the lock's heartbeat is older than ttl.
func (l Lock) Expired(now time.Time, ttl time.Duration) bool {
	return now.Sub(l.HeartbeatAt) > ttl
}
