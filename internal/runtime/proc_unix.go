//go:build !windows

package runtime

import "syscall"

// processAlive reports whether a process with the given pid exists. Signal 0
// performs error checking without delivering a signal: ESRCH means no such
// process, EPERM means it exists but we can't signal it (still alive).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}
