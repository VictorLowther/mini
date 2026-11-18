package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
)

// The transcript's job is to keep the working edge of the conversation on screen
// while it moves, and to stop doing that the moment the human scrolls back to read
// something - and only then. These are the two halves of that promise, with real
// geometry: a scroll with no viewport reports "at the bottom" by definition, which
// is the pre-Show case rather than the one under test.

// laidOut gives the view the size a shown window would have.
func laidOut(t *testing.T, g *GUI, w, h float32) {
	t.Helper()
	g.v.rootC.Resize(fyne.NewSize(w, h))
	drainPosts()
	if g.v.scroll.Size().Height <= 0 {
		t.Fatalf("the transcript got no viewport: center %v scroll %v",
			g.v.center.Size(), g.v.scroll.Size())
	}
}

// fillWithCards overflows the viewport, following the tail the way a live session
// does.
func fillWithCards(t *testing.T, v *guiView, n int) {
	t.Helper()
	for i := range n {
		v.appendBlock(block{kind: "note", text: fmt.Sprintf("card %d: something happened", i)})
	}
	v.follow()
	if !v.atBottom() {
		t.Fatalf("setup: not at the bottom after following (off=%v content=%v view=%v)",
			v.scroll.Offset.Y, v.body.MinSize().Height, v.scroll.Size().Height)
	}
}

// TestGUITranscriptKeepsTheWorkingEdge covers the two ways the view used to lose
// the tail out from under a reader who never touched the scrollbar: a card measured
// taller than the estimate it was placed by - which every card now is, since they
// all carry chrome - and the question or running card taking a strip of viewport
// away from the transcript.
func TestGUITranscriptKeepsTheWorkingEdge(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	v := a.v
	laidOut(t, a, 420, 320)
	fillWithCards(t, v, 12)

	// Growth: the visibility pass replaces estimates with measured heights. The
	// bottom moves down; a view holding it must move with it.
	for i := range v.blockH {
		v.blockH[i] += 40
	}
	v.afterContentChange()
	if !v.pinned {
		t.Error("cards growing past the bottom un-pinned a transcript nobody scrolled")
	}
	if !v.atBottom() {
		t.Errorf("the view stopped following the tail after growth (off=%v content=%v view=%v)",
			v.scroll.Offset.Y, v.body.MinSize().Height, v.scroll.Size().Height)
	}

	// The panel: a prompt takes part of the center column, so
	// the same offset is no longer the bottom - and the next commit is where that
	// used to be read as "the human scrolled up".
	before := v.scroll.Size().Height
	v.showQuestion(Question{Kind: AskConfirm, Title: "run this?", Cfg: a.cfg})
	if v.scroll.Size().Height >= before {
		t.Fatalf("setup: the panel did not shrink the viewport (%v -> %v)", before, v.scroll.Size().Height)
	}
	v.appendBlock(block{kind: "note", text: "a turn lands while the card is up"})
	if !v.pinned {
		t.Error("the question card un-pinned the transcript")
	}
	if !v.atBottom() {
		t.Errorf("the transcript stopped following the working edge (off=%v content=%v view=%v)",
			v.scroll.Offset.Y, v.body.MinSize().Height, v.scroll.Size().Height)
	}

	v.hideQuestion()
	v.appendBlock(block{kind: "note", text: "and the next one"})
	if !v.pinned || !v.atBottom() {
		t.Errorf("putting the panel away lost the hold: pinned=%v off=%v content=%v view=%v",
			v.pinned, v.scroll.Offset.Y, v.body.MinSize().Height, v.scroll.Size().Height)
	}
}

// TestGUIUserScrollReleasesAndRestoresTheHold is the other half: reading history
// must not be fought, and returning to the tail must take the follow back up.
// The scroll callback is invoked directly because that is what a drag does - the
// scroller's own gesture path ends in exactly this call, with no programmatic
// follow in flight.
func TestGUIUserScrollReleasesAndRestoresTheHold(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	v := a.v
	laidOut(t, a, 420, 320)
	fillWithCards(t, v, 12)

	v.scroll.Offset.Y -= 200
	v.scroll.OnScrolled(v.scroll.Offset)
	if v.pinned {
		t.Fatal("scrolling up kept following the tail")
	}
	back := v.scroll.Offset.Y
	v.appendBlock(block{kind: "note", text: "new work the reader is not asking about"})
	if v.scroll.Offset.Y != back {
		t.Errorf("a scrolled-back transcript jumped to %v (was %v)", v.scroll.Offset.Y, back)
	}

	// The reader drags to the bottom: the follow is theirs again.
	v.scroll.Offset.Y = v.body.MinSize().Height - v.scroll.Size().Height
	v.scroll.OnScrolled(v.scroll.Offset)
	if !v.pinned {
		t.Fatal("returning to the bottom did not restore the hold")
	}
	v.appendBlock(block{kind: "note", text: "and the next turn is on screen"})
	if !v.atBottom() {
		t.Errorf("the restored hold did not follow (off=%v content=%v view=%v)",
			v.scroll.Offset.Y, v.body.MinSize().Height, v.scroll.Size().Height)
	}
}

// TestGUIJobCardTickUpdatesInPlace is the once-a-second jump: a backgrounded job's
// card changes its lines on every tick, and that must not evict and rebuild the
// materialized rows. The rebuild is what moved the scroll offset under a reader who
// had not touched the scrollbar, and it is also the expensive path - every card
// re-wrapped, the whole window re-measured - for a change to one text segment.
func TestGUIJobCardTickUpdatesInPlace(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	reg := &jobLister{id: "j1", live: true, elapsed: 10 * time.Second}
	b := openWithOptions(t, a, GUIOptions{TabLabel: "worker", Jobs: reg})
	b.v.rootC.Resize(fyne.NewSize(420, 320))
	for i := range 8 {
		b.v.appendBlock(block{kind: "note", text: strings.Repeat("context ", 12) + string(rune('a'+i))})
	}
	b.v.appendBlock(block{kind: "tool", title: "train", tcid: "c1", state: "running", job: "j1",
		transcript: []string{"$ python train.py", "job j1 is still running (10s)"}})
	b.v.follow()
	drainPosts()
	if len(b.v.winRows) == 0 {
		t.Fatal("setup: nothing materialized to update")
	}
	if b.v.winRows[len(b.v.winRows)-1].body == nil {
		t.Fatal("setup: the job card has no in-place text handle")
	}
	before := len(b.v.winRows)
	oldest := b.v.winRows[0].c

	reg.mu.Lock()
	reg.elapsed = 11 * time.Second // the change a real second brings
	reg.mu.Unlock()
	b.v.refreshJobCards()
	drainPosts()

	if len(b.v.winRows) != before || b.v.winRows[0].c != oldest {
		t.Errorf("the tick rebuilt the window (%d rows, first row replaced: %v)", len(b.v.winRows), b.v.winRows[0].c != oldest)
	}
	if got := transcriptText(b.v.root()); !strings.Contains(got, "11s") {
		t.Errorf("the new lines never reached the card:\n%s", got)
	}
}
