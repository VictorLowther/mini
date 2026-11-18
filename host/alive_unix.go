//go:build !windows

package host

import "syscall"

// ProcessAlive reports whether a pid exists. Signal 0 does no damage: it runs
// the kernel's permission and existence checks and returns.
//
// EPERM means the process is there and belongs to somebody else, which for a
// single-user desktop tool is still "alive" - refusing is the correct
// direction, because the whole point is not to sweep a running process's jobs.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
