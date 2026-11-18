package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

// openViaPicker drives the `+` the way the strip does: the button gives a picker
// tab, the human types a directory and presses enter, and the opening runs off the
// canvas thread. withLoop=false reproduces a window whose opener hands back a
// session with no loop, which startSession must leave alone rather than panic on.
func openViaPicker(t *testing.T, a *GUI, withLoop bool) <-chan string {
	t.Helper()
	asked := make(chan string, 1)
	a.sh.opener = func(dir string) (GUIOptions, error) {
		asked <- dir
		o := GUIOptions{Dir: dir, TabLabel: "second repo", Model: &model.Sushi{Model: "beta"}}
		if withLoop {
			cfg := newConfig("yolo")
			o.Cfg = cfg
			o.Loop = &agent.Loop{Cfg: cfg, Model: &model.Deterministic{}, Env: &env.Local{}}
		}
		return o, nil
	}
	item := a.sh.createTab()
	if item == nil {
		t.Fatal("the + gave no tab")
	}
	a.sh.tabs.Append(item)
	a.sh.tabs.SelectIndex(len(a.sh.tabs.Items) - 1)
	entry := findEntry(t, item.Content)
	entry.SetText("/tmp/second/checkout")
	entry.OnSubmitted(entry.Text)
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the typed directory never reached the opener")
	}
	waitFor(t, "the new session's tab", func() bool { return len(a.sh.sessions) == 2 })
	return asked
}

// TestGUIPickerSessionTakesATask is the dead new tab: the `+` opens a session and
// the human types it a task, but nothing ever drove the session - a spawn arrives
// with a loop its parent is already running, so openSession starts nothing, and
// only the picker needed the extra step. handleInput's idle branch, correct for a
// run that finished, read the never-started session as an ended one: it dropped
// the task and said "the run has ended", which is the same words a human reads as
// "my prompt killed it".
func TestGUIPickerSessionTakesATask(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	openViaPicker(t, a, true)
	b := a.sh.sessions[1]

	// A tab the human just asked for waits to be told what to do, the way the
	// first session does when the CLI was given no -t.
	waitFor(t, "the new tab to ask for a task", func() bool {
		return b.HasQuestion() && b.PendingQuestion().Kind == AskTask
	})
	if !b.v.card.Visible() {
		t.Error("the task question never reached the new tab's panel")
	}
	if !strings.Contains(transcriptText(b.v.root()), "mini-go") {
		t.Error("the new tab never got its session header")
	}
	if got := transcriptText(b.v.root()); strings.Contains(got, "the run has ended") {
		t.Errorf("the new tab reported a run that never started:\n%s", got)
	}

	// Answering it starts the run, and the task lands in that tab's transcript.
	b.v.handleInput("count the files")
	waitFor(t, "the task to reach the transcript", func() bool {
		return strings.Contains(transcriptText(b.v.root()), "count the files")
	})
	if b.HasQuestion() {
		t.Error("the task question outlived its answer")
	}
	if got := transcriptText(b.v.root()); strings.Contains(got, "the run has ended") {
		t.Errorf("sending the prompt ended the session instead of starting it:\n%s", got)
	}
	a.sh.quit()
	drainPosts()
}

// TestGUIPickerSessionWithoutLoopDoesNotPanic covers the other half: a window whose
// opener returns no loop has nothing to drive, and startSession must notice rather
// than dereference it.
func TestGUIPickerSessionWithoutLoopDoesNotPanic(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	openViaPicker(t, a, false)
	b := a.sh.sessions[1]
	drainPosts()
	if b.HasQuestion() {
		t.Error("a session with no loop was asked a question nothing can answer")
	}
	a.sh.quit()
	drainPosts()
}

// TestGUITabCloseReleasesAParkedPrompt: closing a tab that is still waiting for its
// first task has to take that prompt down with it. loopRunning is false while a
// session is parked between turns, so cancelRunTurn refuses, and without an
// explicit release the parked Ask keeps the session's ask-queue slot and its
// goroutine until the process ends.
func TestGUITabCloseReleasesAParkedPrompt(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	openViaPicker(t, a, true)
	b := a.sh.sessions[1]
	waitFor(t, "the new tab to ask for a task", func() bool { return b.HasQuestion() })

	a.sh.closeTab(b.item)
	waitFor(t, "the parked prompt to come down", func() bool { return !b.HasQuestion() })
	if len(a.sh.sessions) != 1 {
		t.Errorf("%d sessions left; the closed tab did not go away", len(a.sh.sessions))
	}
	a.sh.quit()
	drainPosts()
}
