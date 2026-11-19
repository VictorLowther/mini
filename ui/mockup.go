package ui

// The bubble mockup: a dev tool that lives in the library because the thing it
// shows is the library's rendering.
//
// It opens a window holding one card of every transcript kind, built through
// the same blockCards the running app uses, so what is on screen is the real
// path - the real theme, the real bubble polygon, the real markdown and command
// grid - and not a hand-drawing of it. The toolbar exposes the knobs a design
// decision turns on: outline weight, outline strength, light or dark, and the
// pre-redesign tinted fills for comparison.
//
//	go run ./cmd/bubblemock
//
// ShowMockup is exported, and this file is in the package rather than a _test
// file, for one reason and not aesthetics: a Fyne window on macOS must be built
// and run from the process's main thread, and a test function never is. The
// first cut of this tool was a test gated on an env var; it started, created its
// objects, and drew nothing at all - no window, no error, no output. So it is a
// command now, and mini itself never calls it.
//
// A mockup is only worth opening if it reproduces the constraints, not just the
// colours: see mockup.column and the gutter it keeps.

import (
	"fmt"
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// mockCard is one sample area plus the kind label its outline is keyed on. The
// sample tree is held flat on purpose: reaching a bubble through the widget tree
// means walking containers, and a tool that needs its own traversal is a tool
// that will quietly stop covering the cases.
type mockCard struct {
	area  *area
	label string
}

// mockup is the window's content and state, built apart from the event loop so
// the card column can be laid out and measured by a test.
type mockup struct {
	cards []mockCard
	// column is what the transcript scroll shows: the cards, inset on the right
	// by exactly the strip production gives them.
	column *fyne.Container
}

// newMockup renders the samples into a scrollable column.
//
// The right inset is not decoration, and this file got it wrong once: production
// narrows every transcript row by scrollbarGutter() so a card's own scrollbar
// can never stack into the transcript's, and a column of cards dropped straight
// into a VScroll runs the full viewport width. Both bars then live in the same
// pixel column - grabbing the transcript bar becomes a coin flip on which one
// answers, which is the mistake the gutter exists to prevent, here at full size
// and with no session in sight. The inset comes from scrollbarGutter itself
// rather than a number, so the mockup cannot drift from production again without
// the layout changing under it; TestGUIMockupLeavesScrollbarGutter holds it.
func newMockup() *mockup {
	m := &mockup{cards: mockSamples()}
	list := container.NewVBox()
	for _, c := range m.cards {
		list.Add(c.area)
	}
	m.column = container.New(m.gutterLayout(), list)
	return m
}

func (m *mockup) gutterLayout() fyne.Layout {
	return layout.CustomPaddedLayout{RightPadding: scrollbarGutter()}
}

// syncGutter re-reads the gutter against the live theme. Only two stock themes
// are on offer here and their scrollbar metrics match, but a gutter read once at
// startup is a gutter that silently stops matching the moment that stops being
// true.
func (m *mockup) syncGutter() {
	m.column.Layout = m.gutterLayout()
	m.column.Refresh()
}

// apply repaints every sample bubble. It re-runs the production paint first and
// only then deviates from it if a knob says to, and the order is load-bearing:
// refreshing an area re-lays out its bubble, and a bubble's Layout sets the
// outline from the theme - so an override that landed before the refresh would
// be drawn and immediately painted over. (Which also means a resize during a
// comparison snaps the bubbles back to production values. The real look is the
// one worth preserving.)
func (m *mockup) apply(stroke, strength float32, tinted bool) {
	m.syncGutter()
	for _, c := range m.cards {
		c.area.Refresh()
		sh := c.area.bubble.shape
		if tinted {
			sh.FillColor = mockOldTint(c.label)
			sh.StrokeColor = nil
			sh.StrokeWidth = 0
			// A tinted card has no frame at all, so nothing may pretend
			// otherwise: leaving a stroke width set with a nil colour draws
			// whatever the last theme's hue was on a repaint.
		} else {
			sh.FillColor = bubbleSurface()
			sh.StrokeColor = mockStrength(bubbleOutline(c.label), strength)
			sh.StrokeWidth = stroke
		}
		canvas.Refresh(sh)
	}
}

// ShowMockup opens the mockup window and blocks in the Fyne event loop until it
// closes.
func ShowMockup() {
	a := app.New()
	dark := true
	a.Settings().SetTheme(theme.DarkTheme())
	w := a.NewWindow("mini bubble mockup - coloured outlines")
	w.Resize(fyne.NewSize(820, 900))

	m := newMockup()

	// The knobs, each starting at the value the production code computes so the
	// window opens showing what the code actually does.
	stroke := max(theme.Size(theme.SizeNamePadding)/2, 1)
	strength := float32(1)
	tinted := false

	themeBtn := widget.NewButton("", nil)
	setThemeLabel := func() {
		name := "light"
		if dark {
			name = "dark"
		}
		themeBtn.SetText("Theme: " + name)
	}
	themeBtn.OnTapped = func() {
		dark = !dark
		if dark {
			a.Settings().SetTheme(theme.DarkTheme())
		} else {
			a.Settings().SetTheme(theme.LightTheme())
		}
		setThemeLabel()
		m.apply(stroke, strength, tinted)
	}
	setThemeLabel()

	strokeLabel := widget.NewLabel(fmt.Sprintf("%.1f pt", stroke))
	strokeSlider := widget.NewSlider(1, 6)
	strokeSlider.Step = 0.5
	strokeSlider.Value = float64(stroke)
	strokeSlider.OnChanged = func(v float64) {
		stroke = float32(v)
		strokeLabel.SetText(fmt.Sprintf("%.1f pt", stroke))
		m.apply(stroke, strength, tinted)
	}

	strengthLabel := widget.NewLabel("100%")
	strengthSlider := widget.NewSlider(25, 100)
	strengthSlider.Step = 5
	strengthSlider.Value = 100
	strengthSlider.OnChanged = func(v float64) {
		strength = float32(v) / 100
		strengthLabel.SetText(fmt.Sprintf("%.0f%%", v))
		m.apply(stroke, strength, tinted)
	}

	beforeCheck := widget.NewCheck("old look (tinted fills)", func(on bool) {
		tinted = on
		if on {
			w.SetTitle("mini bubble mockup - BEFORE: tinted fills")
		} else {
			w.SetTitle("mini bubble mockup - AFTER: coloured outlines")
		}
		m.apply(stroke, strength, tinted)
	})

	toolbar := container.NewHBox(themeBtn,
		container.NewBorder(nil, nil, widget.NewLabel("outline"), strokeLabel, strokeSlider),
		container.NewBorder(nil, nil, widget.NewLabel("strength"), strengthLabel, strengthSlider),
		beforeCheck)
	hint := widget.NewLabel("Cmd-Q or the close button quits. The five coloured kinds are " +
		"user (blue), harness (amber), thinking (slate), tool call (green), LLM output (violet); " +
		"note, error, exit and compaction take the neutral frame. Cards stop short of the " +
		"scrollbar by the transcript's own gutter.")
	hint.Wrapping = fyne.TextWrapWord

	w.SetContent(container.NewBorder(
		container.NewVBox(toolbar, canvas.NewRectangle(theme.Color(theme.ColorNameSeparator))),
		hint, nil, nil, container.NewVScroll(m.column)))

	m.apply(stroke, strength, tinted)
	w.ShowAndRun()
}

// mockSamples builds one of everything through the production renderer, with the
// bodies that actually occur: a wrapped human turn, a command grid, reasoning,
// markdown with a fence, a rejection, steering, an error, a compaction boundary,
// the exit marker. Two of them are deliberately long-lined so the card's own
// scrollbars appear and their distance from the transcript's can be judged.
func mockSamples() []mockCard {
	samples := []block{
		{kind: "note", text: "Session started. model=gpt-4o ctx=128k - the neutral frame is UI feedback, not message text."},
		{kind: "user", text: "Inventory the GUI architecture and save it to a report, then read the whole thing back to me before we change anything."},
		{kind: "tool", tcid: "call_1", state: "finished",
			transcript: []string{"$ grep -rn bubbleOutline ui/ | head", "ui/fyneview.go:1257:func bubbleOutline(label string) color.Color {", "exit 0"}},
		{kind: "tool", tcid: "call_2", state: "finished",
			transcript: longTranscript(24)},
		{kind: "assistant",
			reasoning: "The fills were five pale tints. Turn them into outlines and the interior still has to stay opaque, because the scroll paints stand-in rectangles under a row.",
			text:      "The report is `docs/ux-audit-2026-11.md` - 548 lines.\n\nWhat it holds, in short:\n\n- **section 1** four owners, two front ends, one seam\n- **section 7** is where nothing will catch you\n\n```go\nsh.StrokeColor = bubbleOutline(label)\nsh.StrokeWidth = bubbleStrokeWidth(w)\n```\n\nThe hues stay; the intensity goes up.",
			stats:     metrics{Tokens: 254, Elapsed: 3.2, Cost: 0.07}},
		{kind: "user", subtype: "UserRejection", text: "Commands not executed. The user rejected your commands with the following message: do not grep outside the working directory."},
		{kind: "user", subtype: "UserSteering", text: "We are going to do some redesign. Colored backgrounds are out, colored outlines are in."},
		{kind: "error", text: "model stream failed: unexpected EOF - the text most likely to be copied somewhere else."},
		{kind: "compaction", title: "compaction - 41 messages", text: "Earlier turns were summarised to stay inside the context window."},
		{kind: "exit", text: "run finished - 21 turns, 3 tool calls, $0.42"},
	}
	var out []mockCard
	for _, b := range samples {
		for _, o := range blockCards(b, true) {
			if ar, ok := o.(*area); ok {
				out = append(out, mockCard{area: ar, label: ar.label})
			}
		}
	}
	return out
}

// longTranscript is a command result that overflows a card's ten-line cap, so
// the card grows its own scrollbar - the bar that must not line up with the
// transcript's.
func longTranscript(lines int) []string {
	out := make([]string, 0, lines+1)
	for i := 0; i < lines; i++ {
		out = append(out, fmt.Sprintf("%2d: build step output line, wide enough to be worth a scroll of its own", i+1))
	}
	return append(out, "exit 0")
}

// mockStrength scales an outline's alpha, which is the "how much does the frame
// assert itself" knob. It only ever dims: the accent hues are already opaque,
// and a stroke cannot be more than fully on.
func mockStrength(c color.Color, k float32) color.Color {
	if k >= 1 {
		return c
	}
	n := color.NRGBAModel.Convert(c).(color.NRGBA)
	n.A = uint8(float32(n.A) * k)
	return n
}

// mockOldTint is the fill the bubbles carried before outlines: the same accent
// blended 20% onto the theme background (16% for LLM output, 9% for the neutral
// kinds). blendOnto was deleted with bubbleFill, so the blend is restated here,
// mockup-only, so the window can show the before picture instead of describing
// it. It is a copy of the old behaviour on purpose - it must not track changes
// to the new code.
func mockOldTint(label string) color.Color {
	accent, ok := bubbleAccent[label]
	alpha := uint8(0x33)
	switch {
	case !ok:
		accent, alpha = color.NRGBA{R: 0x6b, G: 0x72, B: 0x80, A: 0xff}, 0x18
	case label == "LLM output":
		alpha = 0x2a
	}
	b := color.NRGBAModel.Convert(theme.Color(theme.ColorNameBackground)).(color.NRGBA)
	n := color.NRGBAModel.Convert(accent).(color.NRGBA)
	mix := func(x, y uint8) uint8 {
		return uint8((uint32(x)*(255-uint32(alpha)) + uint32(y)*uint32(alpha)) / 255)
	}
	return color.NRGBA{R: mix(b.R, n.R), G: mix(b.G, n.G), B: mix(b.B, n.B), A: 255}
}
