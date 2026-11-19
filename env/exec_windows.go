//go:build windows

package env

import (
	"time"

	"mvdan.cc/sh/v3/interp"
)

// KillGrace is the INT-to-KILL window; kept so the call site in local.go is
// platform-neutral, and shared with the job manager for the same reason.
const KillGrace = 2 * time.Second

// processGroupExecHandler falls back to the stock handler: setsid and
// negative-PID signalling have no Windows equivalent, and mvdan's default
// handler already uses CREATE_NEW_PROCESS_GROUP.
//
// shortcut: no process-group kill on Windows, so a cancelled command can
// orphan grandchildren there; upgrade by killing the job object if anyone runs
// the agent on Windows.
//
// The guard is accepted and unused here. It exists to keep a fork off a
// descriptor somebody else is closing, and hooking it requires owning the code
// around exec.Cmd.Start - which DefaultExecHandler does not offer. The half of
// the race that was reachable from this file, the per-command /dev/null that
// Execute used to open and close, is gone platform-neutrally: stdin is one
// shared handle that is never closed at all (see env/fdguard.go). What remains
// Windows-only is a background statement forking after its shell ended, which
// is a narrower window than the one that was closed, and this port is not the
// platform the agent runs on.
func processGroupExecHandler(killTimeout time.Duration, guard *fdGuard) interp.ExecHandlerFunc {
	_ = guard
	return interp.DefaultExecHandler(killTimeout)
}
