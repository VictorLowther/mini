package ui

import (
	"io"
	"sync"
)

// LogTarget is the writer the process logger goes to, so the pane can retarget
// it while it owns the screen.
//
// This is not cosmetics. A bubbletea pane is cursor-addressed: it repaints the
// rows it believes it occupies. Anything else writing to the same terminal can
// scroll the screen, and the renderer has no way to learn about it, so its model
// of where the frame sits goes stale. The visible symptom is the footer bouncing
// up and down the screen and finishing mid-terminal, with rows of stale content
// left under it.
//
// The indirection is here rather than in the TUI because the logger is wired up
// before the pane exists, and the agent loop keeps its own copy of the logger.
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

// Mute silences the logger for the duration of the returned func. The pane
// reports the events worth showing (a failing model call becomes a transcript
// line), and --log-file still keeps the full detail, so nothing needed is lost.
func (t *LogTarget) Mute() func() {
	if t == nil {
		return func() {}
	}
	return t.retarget(io.Discard)
}
