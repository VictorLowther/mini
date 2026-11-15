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
func processGroupExecHandler(killTimeout time.Duration) interp.ExecHandlerFunc {
	return interp.DefaultExecHandler(killTimeout)
}
