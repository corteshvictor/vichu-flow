//go:build windows

package runtime

// processAlive on Windows cannot cheaply determine liveness without extra
// syscalls, so it conservatively reports true. Orphaned locks are still
// reclaimed on every platform via heartbeat expiry (HeartbeatTTL).
func processAlive(pid int) bool {
	return true
}
