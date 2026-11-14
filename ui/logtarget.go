package ui

import (
	"io"
	"sync"
)

// LogTarget is the writer the process logger goes to, so the GUI can mute it
// while the window owns the screen.
//
// The mute is not cosmetics. While a run is in flight the transcript is the
// only surface the user reads: debug lines scrolling past in the launching
// terminal are noise nobody sees, and on --print they would interleave with
// the piped output. The indirection lets a frontend silence the logger for
// the duration of a run without touching every slog call site.
//
// The indirection is here rather than in a frontend because the logger is
// wired up before any UI exists, and the agent loop keeps its own copy of
// the logger.
type LogTarget struct {
	mu sync.Mutex
	w  io.Writer
}

func NewLogTarget(w io.Writer) *LogTarget {
	if w == nil {
		w = io.Discard
	}
	return &LogTarget{w: w}
}

func (t *LogTarget) Write(p []byte) (int, error) {
	if t == nil {
		return len(p), nil // no target wired up: discard, like io.Discard
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.w.Write(p)
}

// retarget redirects future writes and returns the undo.
func (t *LogTarget) retarget(w io.Writer) func() {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev := t.w
	t.w = w
	return func() { t.retarget(prev) }
}

// Mute silences the logger for the duration of the returned func. The GUI
// reports the events worth showing (a failing model call becomes a transcript
// line), and --log-file still keeps the full detail, so nothing needed is lost.
func (t *LogTarget) Mute() func() {
	if t == nil {
		return func() {}
	}
	return t.retarget(io.Discard)
}
