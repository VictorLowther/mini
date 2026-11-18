//go:build windows

package host

// ProcessAlive cannot be answered portably without OpenProcess, and a wrong
// "alive" would lock a user out of resuming their own session forever. So this
// platform reports "not alive", which is today's behaviour: the owner column is
// still written and the in-process duplicate check still holds, and only the
// cross-process refusal is unix-only. Widening it is a small, additive change.
func ProcessAlive(pid int) bool { return false }
