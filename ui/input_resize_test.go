package ui

import (
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
)

// paintedScroll reports the inner scroll of the renderer the object tree
// actually paints - the one reached through the widget in the container.
func paintedScroll(c *chatInput) (obj *container.Scroll, width float32) {
	for _, o := range test.WidgetRenderer(c).Objects() {
		if sc, ok := o.(*container.Scroll); ok {
			return sc, sc.Size().Width
		}
	}
	return nil, -1
}

// TestPaintedInputFollowsResize is the regression for the input box that never
// resized. chatInput and its embedded *Entry share one fyne.BaseWidget, and
// ExtendBaseWidget is first-write-wins: built with widget.NewMultiLineEntry()
// the entry had already claimed the identity, so the wrapper's claim was a
// no-op, internal/cache keyed the two renderer lookups apart, and each miss ran
// Entry.CreateRenderer again - which rebuilds e.content/e.scroll. Resize then
// laid out a scroll nobody painted (measured: width 1272 on one, 0 on the
// painted one) while the painted scroll kept the width the window was created
// at. Same cause for the missing caret, the text that never wrapped, and the box
// that never grew past a few lines: all of them live in that second renderer.
func TestPaintedInputFollowsResize(t *testing.T) {
	c := newChatInput(&guiView{})
	if _, w := paintedScroll(c); w != 0 {
		t.Fatalf("expected an unsized scroll before the first layout, got %v", w)
	}

	c.Resize(fyne.NewSize(1272, 73))
	sc, w := paintedScroll(c)
	if sc == nil {
		t.Fatal("the painted input has no inner scroll to size")
	}
	if w < 1200 {
		t.Errorf("after Resize(1272) the painted typing area is still %.1f wide - the box cannot follow the window", w)
	}
	t.Logf("painted typing area follows: %.1f wide", w)

	// And it keeps following, in both directions, with text in the box.
	c.SetText("the quick brown fox jumps over the lazy dog and then keeps running")
	for _, want := range []float32{700, 1500, 480} {
		c.Resize(fyne.NewSize(want, 73))
		if _, w := paintedScroll(c); w < want-60 || w > want {
			t.Errorf("typing area %v wide after resizing the input to %v", w, want)
		}
	}
}
