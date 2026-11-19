package env

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestFDGuardOnlyRefusesWhatItOwns is the unit of the handshake: a writer stops
// being usable exactly when Execute shuts the guard, and a writer the guard never
// inherited - a command's own redirect - is never touched.
func TestFDGuardOnlyRefusesWhatItOwns(t *testing.T) {
	dir := t.TempDir()
	inherited, err := os.Create(filepath.Join(dir, "call.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer inherited.Close()
	other, err := os.Create(filepath.Join(dir, "redirect.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	g := newFDGuard(inherited)
	if g.stale(inherited, inherited) {
		t.Error("the inherited writer went stale before the guard shut down")
	}
	if g.stale(other, other) {
		t.Error("the guard claims a writer it never inherited")
	}
	g.shutdown()
	if !g.stale(inherited, inherited) {
		t.Error("the inherited writer is still live after shutdown")
	}
	if g.stale(other, other) {
		t.Error("a redirect this guard never owned was refused as well")
	}
	// A mixed pair - stdout redirected, stderr still the inherited file - is
	// still the guard's business. One half of it is a dead descriptor waiting to
	// be forked, and the substitute replaces both, so the command cannot end up
	// writing one stream to the transcript and the other to nothing.
	if !g.stale(other, inherited) {
		t.Error("a stderr the guard inherited was not refused after shutdown")
	}
	if !g.stale(inherited, other) {
		t.Error("a stdout the guard inherited was not refused after shutdown")
	}

	// A writer with no descriptor behind it cannot go stale, because there is
	// nothing to close and so nothing to race.
	buffered := newFDGuard(os.Stdout)
	buffered.shutdown()
	if !buffered.stale(os.Stdout, os.Stdout) {
		t.Error("os.Stdout is a file; it should be refused after shutdown")
	}
	nofile := newFDGuard(devNull())
	nofile.shutdown()
	if nofile.stale(nil, nil) {
		t.Error("a writer with no path was refused anyway")
	}
}

// TestFDGuardReopenAppendsToTheSameFile checks the substitute a late command gets
// is the transcript's own file, extended rather than replaced: the bytes the loop
// already measured must still be described by the offsets it recorded.
func TestFDGuardReopenAppendsToTheSameFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "call.log")
	if err := os.WriteFile(path, []byte("measured\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	g := newFDGuard(f)
	g.shutdown()
	late, err := g.reopen()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := late.WriteString("late\n"); err != nil {
		t.Fatal(err)
	}
	late.Close()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "measured\nlate\n" {
		t.Errorf("file = %q, want the earlier bytes preserved with the late ones appended", got)
	}
}

// TestFDGuardCloseWaitsForAnInFlightFork is the whole point of the protocol, and
// the only part of it that can be proven without winning a race by luck: while a
// command is inside the fork, the close that Execute's caller wants to perform
// cannot proceed, and the moment the fork releases it does.
func TestFDGuardCloseWaitsForAnInFlightFork(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "call.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	g := newFDGuard(f)

	// Stand in for a handler that has taken the read side and is in cmd.Start.
	g.mu.RLock()
	shut := make(chan struct{})
	go func() { g.shutdown(); close(shut) }()

	select {
	case <-shut:
		g.mu.RUnlock()
		t.Fatal("shutdown ran past a fork that had not finished")
	case <-time.After(100 * time.Millisecond):
	}

	g.mu.RUnlock()
	select {
	case <-shut:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown never completed once the fork released")
	}
	if !g.stale(f, f) {
		t.Error("the guard did not mark the inherited writer dead on the far side of the wait")
	}
}

// TestBackgroundStatementNeverForksAClosedFile is the regression test, and it is
// deliberately a stress rather than a clever interleaving: the shape that used to
// race is a command with a trailing `&`, because the interpreter runs that on a
// goroutine of its own that Runner.Run never waits for, while the caller closes
// the result file the instant Execute returns. Many of them at once, under -race,
// is what makes the window impossible to miss.
func TestBackgroundStatementNeverForksAClosedFile(t *testing.T) {
	dir := t.TempDir()
	const (
		commands = 24
		rounds   = 6
	)
	var wg sync.WaitGroup
	for i := 0; i < commands; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := filepath.Join(dir, fmt.Sprintf("c%d.log", i))
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				t.Error(err)
				return
			}
			l := &Local{Timeout: 10 * time.Second, CWD: dir}
			for r := 0; r < rounds; r++ {
				// The external command is the thing that forks, and the fork is
				// what reads the handle: /bin/echo because it is always there.
				if _, err := l.Execute(context.Background(), "/bin/echo "+fmt.Sprint(r)+" &", "", f); err != nil {
					t.Error(err)
					return
				}
			}
			// Closed exactly where the job watcher and the agent loop close it:
			// after Execute returns, with background forks possibly still due.
			f.Close()
		}(i)
	}
	wg.Wait()

	// Nothing above may have panicked or been reported by the race detector;
	// this only asserts the calls were well-behaved, since how much of a
	// backgrounded command survives its shell is a separate question.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != commands {
		t.Errorf("%d result files, want %d", len(entries), commands)
	}
}
