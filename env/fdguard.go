package env

import (
	"io"
	"os"
	"sync"
)

// fdGuard is the handshake between Local.Execute and the one exec handler every
// command that Execute started passes through on the way to exec.Cmd.Start.
//
// The problem it solves is a lifetime one, and mvdan.cc/sh creates it: a
// background statement (`make &`) runs on a goroutine of the library's own
// (interp/runner.go spawns it and Runner.Run never waits for it), so that
// goroutine can still be inside exec.Cmd.Start - reading the raw platform
// handle out of the *os.File we handed the interpreter - at the instant
// something upstream closes that file. os.File.Fd() is documented as returning
// something that "may or may not still be a valid platform handle" once the
// File is closed, and the consequence is not merely the race detector complaining:
// forkExec can dup a descriptor number that has already been closed, which
// either loses the command's output to EBADF or, once the number has been
// recycled by another open in this process, points a command's stdout at
// somebody else's file. The second is the one worth losing sleep over, and a
// batch of concurrent commands is exactly what recycles numbers fast.
//
// The protocol is an RWMutex and nothing cleverer:
//
//   - a handler holds the read side across cmd.Start(), so any close that wants
//     the write side waits for a Start already in flight to finish;
//   - Execute takes and releases the write side as its last act (shutdown), so
//     by the time it returns no Start is in flight and no later one can begin
//     against the inherited descriptors;
//   - a Start that arrives after that point is not refused and not allowed to
//     use a dead handle: it re-opens the same path instead. Its bytes append to
//     the file the transcript already names, which is what a command that
//     outlived its own shell should do, and the handle it gets is owned by the
//     goroutine that opened it and closed by nobody else.
//
// Holding the read side across cmd.Wait() instead would be simple and wrong:
// the write side is Execute's last act, so a `sleep 3600 &` would park the
// whole tool call for an hour. The lock covers the fork, never the wait.
type fdGuard struct {
	mu     sync.RWMutex
	closed bool

	// inherited is exactly the writer Execute handed the interpreter, kept for
	// identity: a command that redirected its own output has a stdout this
	// guard does not own and must not touch.
	inherited io.Writer
	// path is the file inherited names, or "" when it is not a file at all.
	// Anything without a descriptor (io.Discard, a test buffer) cannot go
	// stale, and "" is how the guard says "there is nothing to re-open".
	path string
}

func newFDGuard(out io.Writer) *fdGuard {
	g := &fdGuard{inherited: out}
	if f, ok := out.(*os.File); ok && f != nil {
		g.path = f.Name()
	}
	return g
}

// stale reports whether the writer a command is about to fork with is one this
// guard can no longer vouch for. Callers must hold the read lock.
func (g *fdGuard) stale(out, errw io.Writer) bool {
	return g.closed && g.path != "" && (out == g.inherited || errw == g.inherited)
}

// reopen rebuilds a write handle on the inherited path for a command that
// started after its shell ended.
//
// Append, never truncate: the file is already in the transcript and its recorded
// byte offsets have to keep describing the bytes they were computed against.
//
// And deliberately without O_CREATE. The file is always there in practice - the
// loop and the job watcher both create it before handing it here, and the
// transcript names the path for as long as the session lives - so creating one
// is not a case to design for, and a bare O_WRONLY|O_APPEND is the version that
// cannot be talked into opening a file that replaced this path in the gap. If the
// path really is gone, the command fails with a reported error instead of
// silently writing somewhere new.
func (g *fdGuard) reopen() (*os.File, error) {
	return os.OpenFile(g.path, os.O_WRONLY|os.O_APPEND, 0)
}

// shutdown is the write side: it waits for every Start in flight, then makes
// the inherited descriptors off-limits to any Start that follows. Deferred by
// Execute, so it runs before Execute's caller can close anything.
func (g *fdGuard) shutdown() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

// What this file does NOT change, recorded here because the descriptor lifetime
// and this one are easy to conflate: a command backgrounded with `&` still does
// not outlive the call that ran it. Execute's deferred cancel ends the context
// every child was started under, exec.CommandContext runs cmd.Cancel, and the
// group is signalled - which is the same machinery that enforces a timeout and a
// user's interrupt, and is why the work that must survive goes through the job
// registry with background: true instead (see docs/jobs-research.md). Verified
// directly: `sleep 1 & echo started` leaves no live child at Execute's return.
// The guard above only decides whether a fork that was already in progress hits a
// closed descriptor on the way; it does not extend any command's life. Making
// `&` mean what a shell means is a different change, with orphaned processes as
// its cost, and it is not one to make in passing.
//
// devNullOnce is the process-wide /dev/null handle every command reads its
// stdin from.
//
// It used to be opened and closed per command, and that close was one half of
// the race this file exists to prevent: the interpreter can hand the same
// handle to a background statement's goroutine, which is still reading its
// platform handle while Execute's deferred Close destroys it. A single handle
// that is never closed cannot race with anything, the way os.Stdin never can.
// It also removes an open/close pair from every command, which is worth more
// than it sounds now that a turn's commands run at the same time and fight over
// the descriptor table.
var devNullOnce = sync.OnceValues(func() (*os.File, error) {
	return os.Open(os.DevNull)
})

// devNull is the shared handle, or nil if /dev/null could not be opened at all;
// interp treats a nil stdin the way it always has.
func devNull() *os.File {
	f, _ := devNullOnce()
	return f
}
