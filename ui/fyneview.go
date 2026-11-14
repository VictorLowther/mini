package ui

import (
	"fmt"
	"image/color"
	"runtime"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// isMacOS picks the platform-conventional modifier for the GUI's chords
// (Fyne's KeyModifierShortcutDefault already is ⌘ on macOS and Ctrl
// elsewhere; the copy-shortcut quirk below is the driver's own routing).
const isMacOS = runtime.GOOS == "darwin"

// thinkingMods is the thinking-toggle modifier chord per platform: Ctrl on
// its own everywhere except macOS, where plain ⌘T stays free for future tabs
// and the Control-Command form is used (Option chords would also insert their
// characters into the entry).
func thinkingMods() fyne.KeyModifier {
	if isMacOS {
		return fyne.KeyModifierShortcutDefault | fyne.KeyModifierControl
	}
	return fyne.KeyModifierShortcutDefault
}

// thinkingKeyName names the thinking-toggle chord the way the platform's
// conventions do, so the hidden-thinking marker always names the real key.
func thinkingKeyName() string {
	if isMacOS {
		return "⌃⌘T"
	}
	return "ctrl+t"
}

func hiddenThinkingMarker(reasoning string) string {
	return fmt.Sprintf("[thinking hidden: %d chars · %s]", len(reasoning), thinkingKeyName())
}

// guiView owns every widget. Its methods are called on the toolkit goroutine
// only: cross-goroutine work posts through GUI.post, and Fyne-originated
// callbacks (keys, menu actions) already run there. Never fyne.Do from
// inside a view method.
type guiView struct {
	gui *GUI
	win fyne.Window

	body   *fyne.Container // virtualized transcript: spacers plus the rows in view (see virtualLayout)
	dock   *fyne.Container // live streaming content, inside dockScroll (see centerLayout)
	center *fyne.Container // middle column: transcript, question card, live dock
	rootC  *fyne.Container // top border: refresh after dock changes
	scroll *container.Scroll
	// dockScroll and panelScroll cap what the live area and the question
	// card contribute to the window's MinSize. Without them the glfw
	// driver's fitContent grows the OS window to whatever the streaming
	// content's minimum height has become; with them the content scrolls
	// inside its card and only the user resizes the window.
	dockScroll  *container.Scroll
	panelScroll *container.Scroll
	panel       *fyne.Container // question panel content, swapped per prompt
	card        *widget.Card    // the bordered box holding the panel
	status      *widget.Label
	editor      *chatInput

	limitsLabel *widget.Label

	cancelItem *fyne.MenuItem
	wasRunning bool
	lastTitle  string

	// streaming coalescing: sushi emits a chunk every few milliseconds;
	// re-wrapping every live surface per chunk is a repaint storm that pegs
	// a core.
	paintGap     time.Duration
	lastFlush    time.Time
	flushPending bool

	// dialog is the prompt above the input: "" or "limits". Leaving the app
	// is never a dialog: /quit or the native quit hotkey only.
	dialog    string
	limitsIdx int
	limits    [2]float64

	// committed is the transcript source of truth the widgets are rendered
	// from; the thinking toggle rebuilds every block from it.
	committed []block
	live      *liveArea
	// pinned tracks whether the transcript was scrolled to the bottom before
	// the last content change, so new output keeps the view at the bottom and
	// scrolled-back history stays put.
	pinned bool

	// Virtualization state. Fyne walks the whole object tree every repaint
	// frame (EnsureMinSize + paint), and the driver repaints at frame rate no
	// matter what changed - so the live tree must stay proportional to the
	// viewport, not to the session. Only blocks inside the window
	// [winFirst, winLast) exist as widgets; blockH carries each block's
	// height - estimated until the row is measured, then actual - and two
	// spacer rectangles stand in for everything above and below the window.
	blockH   []float32
	winFirst int
	winLast  int
	// winRows holds the materialized rows, ascending by block index and
	// always exactly the window - rows enter and leave one at a time at the
	// edges, never rebuilt wholesale, so scrolling costs one widget tree per
	// exposed row and nothing for what stays in view. Fyne's per-object
	// caches expire a beat after detach (cache.canvasInfo), which is fine
	// for edge churn and fatal for rebuild storms.
	winRows []transcriptRow
	topPad  *canvas.Rectangle
	botPad  *canvas.Rectangle
	lastVP  fyne.Size

	inRecompute   bool
	recalcPending bool
}

func newGUIView(g *GUI, w fyne.Window) *guiView {
	v := &guiView{gui: g, win: w}
	v.topPad = canvas.NewRectangle(color.Transparent)
	v.botPad = canvas.NewRectangle(color.Transparent)
	v.body = container.New(virtualLayout{v}, v.topPad, v.botPad)
	v.scroll = container.NewVScroll(v.body)
	v.dock = container.NewVBox()
	v.dockScroll = container.NewVScroll(v.dock)
	v.editor = newChatInput(v)
	v.limitsLabel = widget.NewLabel(limitLabelSteps)
	v.panel = container.NewVBox()
	v.panelScroll = container.NewVScroll(v.panel)
	v.card = widget.NewCard("", "", v.panelScroll)
	v.card.Hide()
	v.status = widget.NewLabel("")
	v.status.Importance = widget.LowImportance
	v.pinned = true // new output follows the tail until the user scrolls up
	v.paintGap = 80 * time.Millisecond
	return v
}

func (v *guiView) root() fyne.CanvasObject {
	v.center = container.New(centerLayout{v}, v.scroll, v.card, v.dockScroll)
	fixed := container.NewVBox(v.status, v.editor)
	v.rootC = container.NewBorder(nil, fixed, nil, nil, v.center)
	// Every scroll movement may cross a window boundary; the window is the
	// tree, so the tree follows the thumb. Caveat from the scroller: a
	// programmatic ScrollToOffset assigns the offset without calling back -
	// everything mini does programmatically goes through ScrollToBottom
	// (scrollBy) instead, and so does all real input.
	v.scroll.OnScrolled = func(fyne.Position) {
		v.recomputeWindow()
		v.repaint(v.scroll)
	}
	return v.rootC
}

// virtualLayout lays the transcript window out between its spacers: the
// content's total height is always sum(blockH), so the scroll bars describe
// the whole session while only the rows in view exist as widgets. Heights are
// re-measured through the rows' own MinSize each pass - wrapping changes
// them, and the estimates only steer until the first real measurement.
type virtualLayout struct{ v *guiView }

func (l virtualLayout) Layout(o []fyne.CanvasObject, s fyne.Size) {
	v := l.v
	y := v.cumHeight(v.winFirst)
	v.topPad.Resize(fyne.NewSize(s.Width, y))
	v.topPad.Move(fyne.Position{})
	for _, ref := range v.winRows {
		h := ref.c.MinSize().Height
		if ref.idx < len(v.blockH) {
			v.blockH[ref.idx] = h
		}
		ref.c.Resize(fyne.NewSize(s.Width, h))
		ref.c.Move(fyne.NewPos(0, y))
		y += h
	}
	// Below the window: whatever history is not materialized, plus any
	// slack when the whole session is shorter than the viewport.
	below := max(v.totalHeight()-y, 0)
	v.botPad.Resize(fyne.NewSize(s.Width, max(below, s.Height-y)))
	v.botPad.Move(fyne.NewPos(0, y))
	// A viewport change (window drag) moves the window's edges without
	// touching the scroll offset: defer a recompute to after this layout.
	if vs := v.scroll.Size(); vs != v.lastVP {
		v.lastVP = vs
		v.scheduleRecompute()
	}
}

func (l virtualLayout) MinSize(o []fyne.CanvasObject) fyne.Size {
	return fyne.NewSize(0, l.v.totalHeight())
}

// cumHeight is the transcript offset where block i begins; totalHeight is
// where the virtual content ends. O(blocks) float adds per call - blocks are
// hundreds, and this replaces walking tens of thousands of widgets.
// transcriptRow binds a materialized row to its block index: the body's
// object slice is ordered by creation, not by position, so the window's own
// ordering is this slice's invariant.
type transcriptRow struct {
	idx int
	c   *fyne.Container
}

func (v *guiView) cumHeight(i int) float32 {
	if i > len(v.blockH) {
		i = len(v.blockH)
	}
	var h float32
	for _, b := range v.blockH[:i] {
		h += b
	}
	return h
}

func (v *guiView) totalHeight() float32 { return v.cumHeight(len(v.blockH)) }

// estimateBlockHeight is the scrollbar's placeholder opinion about a block
// it has not rendered: line counts against the theme's text metrics. The
// first real layout replaces it with the measured height.
func estimateBlockHeight(b block) float32 {
	lines := float32(len(b.transcript))
	if lines == 0 {
		text := strings.TrimRight(b.text, "\n")
		lines = float32(strings.Count(text, "\n") + 1)
	}
	cards := float32(1)
	if b.kind == "assistant" && b.reasoning != "" {
		cards = 2
	}
	pad := theme.Padding()
	return lines*theme.TextSize()*1.5 + cards*4*pad + 2*pad
}

// newRow materializes one block exactly as appendBlock used to do eagerly:
// its cards and the inter-block spacer, wrapped so the window has one object
// per block.
func (v *guiView) newRow(i int) *fyne.Container {
	cards := blockCards(v.committed[i], v.gui.showReasoning())
	return container.NewVBox(append(cards, blockSpacer())...)
}

// recomputeWindow rescans the cumulative heights against the viewport
// (+/- one viewport of margin) and rebuilds the materialized row set when
// the window moves. Scrolling back re-materializes rows from the block data
// - the store and committed remain the truth; widgets are disposable.
func (v *guiView) recomputeWindow() {
	if v.inRecompute {
		return
	}
	v.inRecompute = true
	defer func() { v.inRecompute = false }()

	n := len(v.committed)
	if n == 0 {
		v.setWindow(0, 0)
		return
	}
	vp := v.scroll.Size().Height
	off := v.scroll.Offset.Y
	if vp <= 0 {
		// Pre-show: the transcript opens pinned to the newest turn, so aim a
		// generous stand-in viewport at the tail; start() recomputes once the
		// real geometry exists.
		vp = 640
		off = max(v.totalHeight()-vp, 0)
	}
	top := max(off-vp, 0)
	bottom := off + 2*vp

	first, last, started := 0, 0, false
	cum := float32(0)
	for i, h := range v.blockH {
		if !started && cum+h > top {
			first = i
			started = true
		}
		if cum < bottom {
			last = i + 1
		}
		cum += h
	}
	if !started {
		first, last = n-1, n
	}
	if last < first {
		last = first
	}
	v.setWindow(first, last)
}

// setWindow moves the materialized window incrementally: rows still in
// range keep their widgets (and their measured heights, textures, rasterized
// glyphs), rows past either edge leave through Container.Remove, and only
// newly exposed blocks get newRow. A no-op when the window did not move.
func (v *guiView) setWindow(first, last int) {
	if first == v.winFirst && last == v.winLast {
		return
	}
	v.winFirst, v.winLast = first, last

	// Evict from the head and tail; the window is contiguous, so leaving
	// rows are always at the ends of winRows.
	for len(v.winRows) > 0 && v.winRows[0].idx < first {
		v.body.Remove(v.winRows[0].c)
		v.winRows = v.winRows[1:]
	}
	for len(v.winRows) > 0 && v.winRows[len(v.winRows)-1].idx >= last {
		v.body.Remove(v.winRows[len(v.winRows)-1].c)
		v.winRows = v.winRows[:len(v.winRows)-1]
	}
	// Fill any holes below/above what survives (a jump - rebuild, resize,
	// or a fast drag - degrades to a full materialize of the new window).
	var built []transcriptRow
	if len(v.winRows) == 0 {
		for i := first; i < last; i++ {
			built = append(built, v.buildRow(i))
		}
	} else {
		// After edge eviction, the survivors are exactly winRows[0].idx ...
		// winRows[last].idx, all inside [first, last); head/tail loops fill
		// whatever the new window gained beyond them.
		lo := v.winRows[0].idx
		hi := v.winRows[len(v.winRows)-1].idx + 1
		for i := first; i < lo; i++ {
			built = append(built, v.buildRow(i))
		}
		built = append(built, v.winRows...)
		for i := hi; i < last; i++ {
			built = append(built, v.buildRow(i))
		}
	}
	v.winRows = built
	// Rows were appended to body.Objects (creation order); our layout
	// positions from winRows, so slice order only affects paint order - and
	// the window never overlaps itself.
	v.body.Layout.Layout(v.body.Objects, v.body.Size())
}

func (v *guiView) buildRow(i int) transcriptRow {
	row := v.newRow(i)
	v.body.Add(row)
	return transcriptRow{idx: i, c: row}
}

// scheduleRecompute defers a window update out of a layout pass (a viewport
// resize mid-paint must not rebuild the tree it is measuring); one flight
// at a time.
func (v *guiView) scheduleRecompute() {
	if v.recalcPending {
		return
	}
	v.recalcPending = true
	time.AfterFunc(0, func() {
		v.gui.post(func() {
			v.recalcPending = false
			v.recomputeWindow()
			v.repaint(v.scroll)
		})
	})
}

// The middle column is where content grows, and growth must stop at the
// window edge: the glfw driver resizes the window up to the content tree's
// MinSize (fitContent), so nothing inside it may report a minimum that
// follows the transcript's length. centerLayout therefore distributes the
// height the window actually has: the question card keeps its natural height
// (it is the interactive surface), the live dock grows with the turn but only
// to what is left over, and the transcript takes the rest down to its own
// scroll minimum. Content outgrowing its share scrolls inside its card - the
// scroller's Layout sizes content to max(content min, viewport) - so the
// window changes size only when the user drags it.
type centerLayout struct{ v *guiView }

func (l centerLayout) Layout(o []fyne.CanvasObject, s fyne.Size) {
	scroll, card, dock := o[0], o[1], o[2]
	// A fresh RichText reports its minimum height at whatever width it last
	// saw, which for a live area that has never been laid out is zero. Give
	// the live box the viewport's width and lay it out now, so its measured
	// natural height is real before any of it is apportioned (an area that
	// starts mid-stream has never seen a layout pass at all).
	if l.v.live != nil {
		nat := l.v.dock.MinSize()
		l.v.dock.Resize(fyne.NewSize(max(nat.Width, s.Width), nat.Height))
		l.v.dock.Layout.Layout(l.v.dock.Objects, l.v.dock.Size())
	}
	floor := scroll.MinSize().Height

	cardH := min(l.v.questionCardHeight(), s.Height)
	rem := max(s.Height-cardH, 0)
	dockH := min(l.v.liveContentHeight(), max(rem-floor, 0))
	scrollH := max(rem-dockH, 0)

	scroll.Resize(fyne.NewSize(s.Width, scrollH))
	scroll.Move(fyne.Position{})
	card.Resize(fyne.NewSize(s.Width, cardH))
	card.Move(fyne.NewPos(0, scrollH))
	dock.Resize(fyne.NewSize(s.Width, dockH))
	dock.Move(fyne.NewPos(0, scrollH+cardH))
	// Grow the scrolled contents to their natural size by hand - the same
	// reason afterContentChange resizes the transcript body: the scroller
	// resizes content in its renderer Layout, which may not have run yet,
	// and an offset update against a not-yet-oversized content is a no-op.
	if dockH > 0 || l.v.live != nil {
		l.v.dock.Resize(fyne.NewSize(max(l.v.dock.MinSize().Width, s.Width),
			max(l.v.liveContentHeight(), dockH)))
	}
	if cardH > 0 && l.v.panel.MinSize().Height > cardH {
		l.v.panel.Resize(fyne.NewSize(max(l.v.panel.MinSize().Width, s.Width), l.v.panel.MinSize().Height))
	}
	// The live area is what streams: when its content outgrew the space the
	// window had for it, follow the tail inside the card.
	if dockH > 0 && l.v.liveContentHeight() > dockH {
		l.v.dockScroll.ScrollToBottom()
	}
}

func (l centerLayout) MinSize(o []fyne.CanvasObject) fyne.Size {
	var w float32
	h := float32(0)
	for _, c := range o {
		m := c.MinSize()
		if m.Width > w {
			w = m.Width
		}
		h += m.Height
	}
	return fyne.NewSize(w, h)
}

// liveContentHeight is the natural height of the streaming turn, 0 when no
// turn is in flight.
func (v *guiView) liveContentHeight() float32 {
	if v.live == nil {
		return 0
	}
	return v.dock.MinSize().Height
}

// questionCardHeight is the card's natural height for the current panel:
// the card's own chrome (measured around the empty panelScroll) plus what
// the panel content needs, never below the card's minimum and 0 while hidden.
func (v *guiView) questionCardHeight() float32 {
	if !v.card.Visible() {
		return 0
	}
	chrome := v.card.MinSize().Height - v.panelScroll.MinSize().Height
	return max(v.card.MinSize().Height, chrome+v.panel.MinSize().Height)
}

// start shows the window and puts the focus on the input - input must always
// work, the rule chatInput.FocusLost enforces by re-focusing even on Blur.
func (v *guiView) start() {
	v.win.Show()
	v.focusEditor()
	// The transcript was built before the scroll had a height, so it is
	// stacked above the fold; land on the newest turn.
	v.pinned = true
	v.scroll.ScrollToBottom()
	v.refreshStatus()
}

func (v *guiView) focusEditor() {
	v.win.Canvas().Focus(v.editor)
}

// ------------------------------------------------------------ the transcript --

func (v *guiView) noteLocal(text string) {
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line != "" {
			v.appendBlock(block{kind: "note", text: line})
		}
	}
}

// updatePinned records whether the view sat at the bottom before the next
// content change. A scroll that has not been laid out yet (zero height, e.g.
// during the pre-Show transcript build) must not count as "scrolled up" - the
// transcript starts pinned so it follows new turns, which is the whole point.
func (v *guiView) updatePinned() {
	view := v.scroll.Size().Height
	if view <= 0 {
		v.pinned = true
		return
	}
	bottom := v.body.MinSize().Height - view
	v.pinned = v.scroll.Offset.Y >= bottom-4
}

// afterContentChange absorbs a structural change (a block committed): grow
// the scroll content to its new minimum by resizing it - existing children
// early-return from Resize (same size), so only the new widgets lay out -
// then follow the tail (which also refreshes the scroll bars via the
// renderer). Deliberately NOT scroll.Refresh(): that Refresh()es every
// committed bubble - re-wrapping all prose and re-parsing every TextGrid -
// the repaint storm the profile caught (textRenderer.Refresh + setCellRune +
// wrapWordLines dominating, hundreds of MB/s on a long transcript).
func (v *guiView) afterContentChange() {
	v.recomputeWindow()
	if vs := v.scroll.Size(); vs.Height > 0 {
		min := v.body.MinSize()
		v.body.Resize(fyne.NewSize(max(min.Width, vs.Width), max(min.Height, vs.Height)))
	}
	if v.pinned {
		v.scroll.ScrollToBottom()
	}
	v.repaint(v.scroll)
	v.refreshStatus()
}

// repaint asks the canvas to redraw one object's region. After layout-only
// updates nothing has marked the new widgets dirty, and this is the cheap
// replacement for the Refresh() cascade: it re-rasterises the pixels without
// re-wrapping or re-celling a single widget.
func (v *guiView) repaint(o fyne.CanvasObject) {
	if c := fyne.CurrentApp().Driver().CanvasForObject(v.scroll); c != nil {
		c.Refresh(o)
	}
}

func (v *guiView) appendBlock(b block) {
	v.committed = append(v.committed, b)
	v.updatePinned()
	v.blockH = append(v.blockH, estimateBlockHeight(b))
	v.afterContentChange()
}

// commitBlock places a finished block in the transcript, replacing the
// in-flight turn's live area when it is the committed form of that same turn -
// the live-block rule, so a turn is rendered once and already carries its
// header while streaming.
func (v *guiView) commitBlock(b block) {
	if v.live != nil {
		if b.kind != "assistant" {
			// Something else ended the turn: keep what streamed as its own
			// entry rather than letting a later commit replace it.
			lb := v.finalizeLive()
			v.removeLive()
			v.appendBlock(lb)
		} else {
			v.removeLive()
		}
	}
	v.appendBlock(b)
}

func (v *guiView) removeLive() {
	v.dock.Remove(v.live.box)
	v.live = nil
	v.layoutCenter() // see flushLive: layout-only, and direct: the border's Resize of a constant-height center would early-return
}

func (v *guiView) finalizeLive() block {
	reasoning, content := v.gui.StreamText()
	return block{kind: "assistant", text: content, reasoning: reasoning}
}

// streamChunk updates the live surfaces from the stream buffers cheaply (one
// string copy per segment) and coalesces the expensive repaint - full re-wrap
// plus the scroll relayout cascade - to at most one per paintGap, leading
// edge first so the first chunk of a turn paints immediately.
func (v *guiView) streamChunk() {
	g := v.gui
	reasoning, content := g.StreamText()
	if reasoning == "" && content == "" {
		return
	}
	if v.live == nil {
		v.live = newLiveArea()
		v.updatePinned()
		v.dock.Add(v.live.box)
	}
	v.live.setText(reasoning, content, g.showReasoning())
	if v.paintGap <= 0 || time.Since(v.lastFlush) >= v.paintGap {
		v.flushLive()
		return
	}
	if v.flushPending {
		return
	}
	v.flushPending = true
	wait := v.paintGap - time.Since(v.lastFlush)
	time.AfterFunc(wait, func() { g.post(func() { v.flushLive() }) })
}

// flushLive repaints the streaming turn only: the live surfaces re-wrap, the
// dock grows to the share of the window's height centerLayout has for it, and
// the scroll's committed content stays untouched. rootC.Layout re-runs the
// border and the center layout; every committed child early-returns from
// Resize because its size did not change - that is what keeps a flush
// O(live) instead of O(transcript).
func (v *guiView) flushLive() {
	v.flushPending = false
	v.lastFlush = time.Now()
	if v.live != nil {
		v.live.refresh()
	}
	// Layout, never Refresh: Container.Refresh cascades Refresh() into every
	// committed bubble (full re-wrap + TextGrid re-cell). Re-running the
	// center layout sizes the live dock from its content's MinSize against
	// the height the column actually has; size changes propagate down
	// through Resize (whose children that did not change size early-return)
	// - live-only work, O(transcript) avoided.
	v.layoutCenter()
	if v.pinned {
		v.scroll.ScrollToBottom() // viewport shrank under the dock: keep the tail visible
	}
	v.repaint(v.scroll)
	v.refreshStatus()
}

// layoutCenter re-runs centerLayout against the column's current size. It
// goes straight to the center instead of re-laying the border: the border
// sizes its center with Resize, which early-returns when the height did not
// change - and with the fixed chrome moved out of the center, a streaming
// flush rarely changes it. That early-return is what left the live area's
// size stale between turns.
func (v *guiView) layoutCenter() {
	if v.center != nil {
		v.center.Layout.Layout(v.center.Objects, v.center.Size())
	}
}

// rebuildAll re-renders every block (the thinking toggle changes how past
// turns look): heights return to estimates - visibility was exactly what
// changed them - and the window rebuilds its rows from the new state.
func (v *guiView) rebuildAll() {
	for i := range v.committed {
		v.blockH[i] = estimateBlockHeight(v.committed[i])
	}
	v.live = nil
	v.dock.Objects = nil
	// Every materialized row is stale now (visibility changed what it
	// renders): evict all of them and let the window rebuild from the new
	// state.
	for _, ref := range v.winRows {
		v.body.Remove(ref.c)
	}
	v.winRows = nil
	v.winFirst, v.winLast = -1, -1 // defeat setWindow's no-change shortcut
	v.recomputeWindow()
	v.layoutCenter()
	v.repaint(v.scroll)
	// Re-stream the turn in flight from its buffers, if any.
	v.streamChunk()
	if v.pinned {
		v.scroll.ScrollToBottom()
	}
}

func (v *guiView) toggleThinking() {
	if v.dialog != "" {
		return
	}
	on := v.gui.ToggleReasoning()
	v.rebuildAll()
	v.noteLocal(map[bool]string{true: "thinking shown", false: "thinking hidden"}[on])
}

// ------------------------------------------------------------------ the view --

func dimStyle() widget.RichTextStyle {
	return widget.RichTextStyle{ColorName: theme.ColorNameDisabled}
}

func plainRT(text string) *widget.RichText {
	rt := widget.NewRichText(&widget.TextSegment{Text: text})
	rt.Wrapping = fyne.TextWrapWord
	return rt
}

func dimRT(text string) *widget.RichText {
	rt := widget.NewRichText(&widget.TextSegment{Text: text, Style: dimStyle()})
	rt.Wrapping = fyne.TextWrapWord
	return rt
}

func errorRT(text string) *widget.RichText {
	rt := widget.NewRichText(&widget.TextSegment{Text: text, Style: widget.RichTextStyle{ColorName: theme.ColorNameError}})
	rt.Wrapping = fyne.TextWrapWord
	return rt
}

func markdownRT(text string) *widget.RichText {
	rt := widget.NewRichTextFromMarkdown(text)
	rt.Wrapping = fyne.TextWrapWord
	return rt
}

// messageKindLabel names a message block's area and picks its side: text the
// human typed (task, steering, human commands) sits right-justified like a
// sent chat bubble; harness-authored feedback (rejections, interruption
// records, format-error prompts, new-task wrappers) and everything else stay
// on the left.
func messageKindLabel(b block) (label string, align fyne.TextAlign) {
	switch {
	case len(b.transcript) > 0:
		return "tool call", fyne.TextAlignLeading
	case b.kind == "assistant":
		return "LLM output", fyne.TextAlignLeading
	case b.kind == "user":
		if b.subtype == "" || b.subtype == "UserSteering" {
			return "user", fyne.TextAlignTrailing
		}
		return "harness", fyne.TextAlignLeading
	default:
		return b.kind, fyne.TextAlignLeading
	}
}

// quitKeyName names the native quit hotkey the notes point at.
func quitKeyName() string {
	if isMacOS {
		return "⌘Q"
	}
	return "Ctrl+Q"
}

// ------------------------------------------------------------------ bubbles --
//
// The transcript's message areas are speech bubbles in the style of
// apptrix.org/components/widget/chat (the Fyne add-ons gallery chat widget
// that defines this UI's reference look): an opaque rounded body with a tail
// flaring to a point on the sender's side. The fills here are theme-derived
// instead of hardcoded so light and dark variants both read. Opaque matters
// as much as the widget: the first cut used low-alpha fills on a raw canvas
// rectangle and rendered effectively invisible.

// bubbleFill is an opaque, clearly-distinct surface per message kind. It
// blends a fixed accent hue onto the theme background rather than using
// theme colour names directly: ColorNamePrimary (user) and
// ColorNameHyperlink (LLM output) are both blue in the default theme and the
// faint foreground tints of thinking/tool blended into the background until
// they read as "no bubble at all". These hues are far apart (blue, violet,
// green, amber, slate) so the five kinds are tellable at a glance on both
// light and dark variants, and blended at an alpha that reads as a raised
// surface, not wallpaper.
var bubbleAccent = map[string]color.NRGBA{
	"user":       {R: 0x3b, G: 0x82, B: 0xf6}, // blue
	"harness":    {R: 0xe0, G: 0x8a, B: 0x14}, // amber
	"thinking":   {R: 0x8a, G: 0x93, B: 0xa6}, // slate
	"tool call":  {R: 0x2f, G: 0xa9, B: 0x5c}, // green
	"LLM output": {R: 0x8b, G: 0x5c, B: 0xf6}, // violet
}

// bubbleFill's blend weights: the response surface is the most common, so it
// stays a touch subtler; the coloured kinds get a bit more presence.
func bubbleFill(label string) color.Color {
	bg := theme.Color(theme.ColorNameBackground)
	accent, ok := bubbleAccent[label]
	if !ok {
		return blendOnto(bg, color.NRGBA{R: 0x6b, G: 0x72, B: 0x80}, 0x18)
	}
	alpha := uint8(0x33)
	if label == "LLM output" {
		alpha = 0x2a
	}
	return blendOnto(bg, accent, alpha)
}

// blendOnto composites accent over base at alpha (0-255) and returns an
// opaque colour.
func blendOnto(base, accent color.Color, alpha uint8) color.NRGBA {
	b := color.NRGBAModel.Convert(base).(color.NRGBA)
	a := color.NRGBAModel.Convert(accent).(color.NRGBA)
	mix := func(x, y uint8) uint8 {
		return uint8((uint32(x)*(255-uint32(alpha)) + uint32(y)*uint32(alpha)) / 255)
	}
	return color.NRGBA{R: mix(b.R, a.R), G: mix(b.G, a.G), B: mix(b.B, a.B), A: 255}
}

// bubbleTailDrop/tailBase: how far the tail hangs below the body and how much
// of the bottom edge it flares from; content must stay clear of the drop.
func bubbleTailDrop(w fyne.Widget) float32 {
	return theme.SizeForWidget(theme.SizeNamePadding, w) * 1.5
}

func bubbleTailBase(w fyne.Widget) float32 {
	return theme.SizeForWidget(theme.SizeNamePadding, w) * 2
}

// bubble is the speech-bubble background: a rounded body with a tail to a
// point on the sender's side, filled with an opaque colour. It is a real
// widget (not a raw canvas shape) so the container renderers treat it like
// any other object.
type bubble struct {
	widget.BaseWidget
	label string
	mine  bool
	shape *canvas.ArbitraryPolygon
}

func newBubble(label string, mine bool) *bubble {
	b := &bubble{label: label, mine: mine, shape: canvas.NewArbitraryPolygon(nil, bubbleFill(label))}
	b.ExtendBaseWidget(b)
	return b
}

func (b *bubble) points(s fyne.Size) ([]fyne.Position, []float32) {
	w, h := s.Width, s.Height
	drop := min(bubbleTailDrop(b), h)
	base := min(bubbleTailBase(b), w)
	body := h - drop
	radius := theme.SizeForWidget(theme.SizeNamePadding, b)
	// The tip is barely softened; rounding it would blunt the tail.
	tip := min(radius/4, 1)
	if b.mine {
		return []fyne.Position{
			{X: 0, Y: 0},
			{X: w, Y: 0},
			{X: w, Y: h},           // the point, below the body, on our side
			{X: w - base, Y: body}, // the tail flares from here
			{X: 0, Y: body},
		}, []float32{radius, radius, tip, 0, radius}
	}
	return []fyne.Position{
		{X: 0, Y: 0},
		{X: w, Y: 0},
		{X: w, Y: body},
		{X: base, Y: body}, // the tail flares from here
		{X: 0, Y: h},       // the point, below the body, on the sender's side
	}, []float32{radius, radius, radius, 0, tip}
}

func (b *bubble) CreateRenderer() fyne.WidgetRenderer {
	return &bubbleRenderer{b: b}
}

type bubbleRenderer struct {
	b    *bubble
	laid fyne.Size
}

func (r *bubbleRenderer) Layout(s fyne.Size) {
	// Parents re-run Layout on every container refresh (each streaming flush
	// relayouts the whole transcript); re-rasterising an unchanged polygon is
	// pure CPU burn, so only repaint when the size actually moved.
	if s == r.laid {
		return
	}
	r.laid = s
	r.b.shape.FillColor = bubbleFill(r.b.label)
	r.b.shape.Points, r.b.shape.CornerRadii = r.b.points(s)
	r.b.shape.Resize(s)
	canvas.Refresh(r.b.shape)
}

func (r *bubbleRenderer) MinSize() fyne.Size { return fyne.Size{} }

func (r *bubbleRenderer) Objects() []fyne.CanvasObject { return []fyne.CanvasObject{r.b.shape} }

func (r *bubbleRenderer) Refresh() {
	r.laid = fyne.Size{}
	r.Layout(r.b.Size())
}

func (r *bubbleRenderer) Destroy() {}

// area is one message's text area: the bubble behind, the header/metrics line
// and the body in front, inset so nothing sits over the tail. It is a real
// widget, not a bare container wrapper: the GL painter's type-switch handles
// fyne.Widget and *fyne.Container, and a struct that merely embeds a container
// matches neither, so it draws nothing on screen while the forgiving software
// test painter makes it look fine headless.
type area struct {
	widget.BaseWidget
	label  string
	header *widget.RichText
	body   fyne.CanvasObject
	bubble *bubble
	stack  *fyne.Container
}

func (a *area) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(a.stack)
}

// areaColor is each kind's header colour. Headers use the bright theme
// foreground rather than the bubble's own hue: a tinted fill under
// same-hue text (blue on blue, amber on amber, disabled-grey on slate) was
// near-unreadable, and the reference look keeps the header legible above the
// body while the fill carries the colour. Foreground reads on every tint and
// is one step brighter than the dimmed body text.
func areaColor(string) fyne.ThemeColorName { return theme.ColorNameForeground }

// areaCard builds one transcript text area for a message kind: an opaque
// speech bubble under a header line that names the kind and the message's
// tokens, generation time and cost, in the body's own font and a kind colour.
func areaCard(label string, m metrics, body fyne.CanvasObject, align fyne.TextAlign) *area {
	mine := align == fyne.TextAlignTrailing
	header := widget.NewRichText(&widget.TextSegment{
		Text:  cardHeaderText(label, m),
		Style: widget.RichTextStyle{ColorName: areaColor(label), Alignment: align},
	})
	header.Wrapping = fyne.TextWrapWord
	if rt, ok := body.(*widget.RichText); ok {
		alignRT(rt, align)
	}
	b := newBubble(label, mine)
	pad := theme.Size(theme.SizeNamePadding)
	content := container.New(layout.NewCustomPaddedLayout(pad, bubbleTailDrop(b)+pad, pad, pad),
		container.NewVBox(header, body))
	a := &area{
		label:  label,
		header: header,
		body:   body,
		bubble: b,
		stack:  container.NewStack(b, content),
	}
	a.ExtendBaseWidget(a)
	return a
}

// cardHeaderText is an area's header: the message kind, then its generated
// token count, generation time and cost; zero parts are omitted.
func cardHeaderText(label string, m metrics) string {
	parts := []string{label}
	if m.Tokens > 0 {
		parts = append(parts, fmt.Sprintf("%d tok", m.Tokens))
	}
	if m.Elapsed > 0 {
		parts = append(parts, fmt.Sprintf("%.1fs", m.Elapsed))
	}
	if m.Cost > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", m.Cost))
	}
	return strings.Join(parts, " · ")
}

// alignRT sets the row alignment of every text segment (user entries trail).
func alignRT(rt *widget.RichText, align fyne.TextAlign) {
	if align == fyne.TextAlignLeading {
		return
	}
	for _, s := range rt.Segments {
		if ts, ok := s.(*widget.TextSegment); ok {
			st := ts.Style
			st.Alignment = align
			ts.Style = st
		}
	}
}

// monoBlock renders fixed-width lines as one monospace RichText that scrolls
// sideways instead of reflowing: the columns of `$ cmd` / `in <cwd>` /
// `exit N` are the point. The block is deliberately a single widget -
// widget.TextGrid materializes a canvas.Text and a rectangle per *cell*, and
// Fyne walks every object in the tree on every repaint frame (EnsureMinSize
// + paint), so a screenful of output rendered as a grid measured at roughly
// a fifth of a core sitting idle. Wrapping off means MinSize reports the
// widest real line; the horizontal scroller owns the overflow, so long
// output never pushes the transcript (or the window) wider than it is.
func monoBlock(lines []string) fyne.CanvasObject {
	rt := widget.NewRichText(&widget.TextSegment{
		Text: strings.Join(lines, "\n"),
		Style: widget.RichTextStyle{
			TextStyle: fyne.TextStyle{Monospace: true},
			Alignment: fyne.TextAlignLeading,
		},
	})
	rt.Wrapping = fyne.TextWrapOff
	return container.NewHScroll(rt)
}

// transcriptGrid renders a tool block's lines as fixed-width text: see
// monoBlock for the widget choice.
func transcriptGrid(lines []string) fyne.CanvasObject {
	return monoBlock(lines)
}

func exitRT(b block) *widget.RichText {
	rt := widget.NewRichText(
		&widget.TextSegment{Text: "✓ " + b.title + "\n", Style: widget.RichTextStyle{ColorName: theme.ColorNameSuccess}},
		&widget.TextSegment{Text: b.text, Style: dimStyle()})
	rt.Wrapping = fyne.TextWrapWord
	return rt
}

// blockCards renders one transcript entry as the bordered text areas it
// contains: an assistant turn becomes a "thinking" area and an "LLM output"
// area, a tool result is a "tool call" grid, and input-box entries are
// right-justified "user" areas. Notes, errors and the exit marker stay plain
// rows: they are UI feedback, not message text.
func blockCards(b block, showThinking bool) []fyne.CanvasObject {
	if len(b.transcript) > 0 || b.kind == "user" || b.kind == "system" {
		label, align := messageKindLabel(b)
		if len(b.transcript) > 0 {
			return []fyne.CanvasObject{areaCard(label, b.stats, transcriptGrid(b.transcript), align)}
		}
		return []fyne.CanvasObject{areaCard(label, b.stats, markdownRT(b.text), align)}
	}
	switch b.kind {
	case "assistant":
		var cards []fyne.CanvasObject
		if b.reasoning != "" {
			// Inside a coloured bubble, foreground text only: the dim styling
			// that reads well on the plain background is illegible on the tint.
			body := plainRT(collapseSpaces(b.reasoning))
			if !showThinking {
				body = plainRT(hiddenThinkingMarker(b.reasoning))
			}
			cards = append(cards, areaCard("thinking", b.stats, body, fyne.TextAlignLeading))
		}
		// A tool-call-only turn has empty content: no prose to show, so emit
		// no LLM output bubble rather than an empty violet box. The turn's
		// real output is the tool call result that follows.
		if b.text != "" {
			cards = append(cards, areaCard("LLM output", b.stats, markdownRT(b.text), fyne.TextAlignLeading))
		}
		return cards
	case "exit":
		return []fyne.CanvasObject{exitRT(b)}
	case "error":
		return []fyne.CanvasObject{errorRT(b.text)}
	default: // notes: UI feedback, the session header, command results
		return []fyne.CanvasObject{dimRT(b.text)}
	}
}

// blockSpacer is the blank row between transcript blocks, the GUI's form of
// the separating row blockRows appends.
func blockSpacer() fyne.CanvasObject {
	s := widget.NewLabel(" ")
	s.Importance = widget.LowImportance
	return s
}

// liveArea is the streaming turn: a bordered area per kind ("thinking" and
// "LLM output"), each one mutable segment so a chunk costs a re-wrap and
// nothing else. Markdown is parsed once, on commit - AppendMarkdown documents
// that one document must not be parsed piecewise. The areas appear as their
// kinds start streaming; the header gains its metrics when the message lands.
type liveArea struct {
	box      *fyne.Container
	thinking *widget.RichText
	thinkSeg *widget.TextSegment
	content  *widget.RichText
	contSeg  *widget.TextSegment
}

func newLiveArea() *liveArea {
	return &liveArea{box: container.NewVBox()}
}

// setText keeps the mutable segments current (string copies only); the
// re-wrap happens in refresh, driven by the streamChunk throttle.
func (l *liveArea) setText(reasoning, content string, show bool) {
	if reasoning != "" && l.thinking == nil {
		l.thinkSeg = &widget.TextSegment{}
		l.thinking = widget.NewRichText(l.thinkSeg)
		l.thinking.Wrapping = fyne.TextWrapWord
		l.box.Add(areaCard("thinking", metrics{}, l.thinking, fyne.TextAlignLeading))
	}
	if content != "" && l.content == nil {
		l.contSeg = &widget.TextSegment{}
		l.content = widget.NewRichText(l.contSeg)
		l.content.Wrapping = fyne.TextWrapWord
		l.box.Add(areaCard("LLM output", metrics{}, l.content, fyne.TextAlignLeading))
	}
	if l.thinking != nil {
		if show {
			l.thinkSeg.Text = collapseSpaces(reasoning)
		} else {
			l.thinkSeg.Text = hiddenThinkingMarker(reasoning)
		}
	}
	if l.content != nil {
		l.contSeg.Text = content
	}
}

// refresh repaints the live surfaces (the O(length) word-wrap pass).
func (l *liveArea) refresh() {
	if l.thinking != nil {
		l.thinking.Refresh()
	}
	if l.content != nil {
		l.content.Refresh()
	}
}

// -------------------------------------------------------------------- status --

func (v *guiView) refreshStatus() {
	g := v.gui
	// Only push strings that actually changed: this runs on every content
	// change (including the streaming flush), and SetText/SetTitle repaint
	// the label and the whole window frame.
	if text := g.RunStats(); text != v.status.Text {
		v.status.SetText(text)
	}
	if title := "mini · " + g.cfg.Mode; title != v.lastTitle {
		v.lastTitle = title
		v.win.SetTitle(title)
	}
	if running := g.loopRunning(); running != v.wasRunning {
		v.wasRunning = running
		// The menu bar draws its dropdowns when opened, so the field alone
		// carries the state; there is nothing to invalidate.
		if v.cancelItem != nil {
			v.cancelItem.Disabled = !running
		}
	}
}

// ---------------------------------------------------------------- questions ---

func (v *guiView) setPanel(objs ...fyne.CanvasObject) {
	v.panel.Objects = objs
	if len(objs) == 0 {
		v.card.Hide()
	} else {
		v.card.Show()
	}
	v.panel.Refresh()
	v.card.Refresh()
	// The card's share of the center column depends on whether it is up and
	// how tall its content is; layout-only, the same rule as flushLive.
	v.layoutCenter()
}

func (v *guiView) showQuestion(q Question) {
	v.dialog = ""
	switch q.Kind {
	case AskConfirm:
		v.setPanel(plainRT(q.Title), monoBlock(panelActionLines(q.Actions)), dimRT(panelConfirmHint))
	case AskLimits:
		v.openDialog("limits")
		return
	default: // new-task, human, interruption, task
		v.setPanel(plainRT(q.Title))
	}
	v.refreshStatus()
}

// questionResolved hides the panel: the answer came from this goroutine, so
// there is no message pump that would notice HasQuestion going false.
func (v *guiView) questionResolved() {
	v.dialog = ""
	v.setPanel()
}

func (v *guiView) hideQuestion() {
	if v.gui.HasQuestion() {
		return
	}
	v.questionResolved()
}

const (
	limitLabelSteps = "New step limit: "
	limitLabelCost  = "New cost limit: "
)

func (v *guiView) openDialog(name string) {
	v.dialog = name
	v.limitsIdx = 0
	v.limits = [2]float64{}
	v.editor.SetText("")
	switch name {
	case "limits":
		v.limitsLabel.SetText(limitLabelSteps)
		v.setPanel(plainRT(v.gui.PendingQuestion().Title), v.limitsLabel)
	}
	v.focusEditor()
}

// closeDialog dismisses the prompt. A pending question stays pending (there is
// no give-up answer; giving up means ending the run), so the limits banner
// hides with its input row and the next submit reopens it.
func (v *guiView) closeDialog() {
	v.dialog = ""
	q := v.gui.PendingQuestion()
	switch {
	case !v.gui.HasQuestion():
		v.setPanel()
	case q.Kind == AskLimits:
		v.setPanel()
	default:
		v.showQuestion(q)
	}
	v.focusEditor()
}

func (v *guiView) submitLimits(value string) {
	v.limits[v.limitsIdx] = ParseLimitLine(value)
	if v.limitsIdx == 0 {
		v.limitsIdx = 1
		v.limitsLabel.SetText(limitLabelCost)
		return
	}
	v.dialog = ""
	v.gui.resolve(Answer{NewLimits: true, Steps: int(v.limits[0]), Cost: v.limits[1]})
}

func (v *guiView) showRunDone() {
	info, err := v.gui.result()
	status, _ := info["exit_status"].(string)
	switch {
	case err != nil && status == "":
		v.appendBlock(block{kind: "error", text: "run ended: " + err.Error()})
		v.appendBlock(block{kind: "note", text: "type /quit or press " + quitKeyName() + " to leave"})
	default:
		if status == "" {
			status = "stopped"
		}
		v.appendBlock(block{kind: "note", text: "run finished: " + status})
		v.gui.app.Quit()
	}
	v.refreshStatus()
}

// esc is the cancel key: it interrupts the command in flight while the agent
// works and dismisses the limits prompt; at idle it does nothing, because
// leaving is deliberate - /quit, the native quit hotkey, or closing the
// window.
func (v *guiView) esc() {
	if v.dialog == "limits" {
		v.closeDialog()
		return
	}
	if v.gui.loopRunning() {
		v.gui.CancelCurrentTurn()
		v.noteLocal("cancelling this turn…")
	}
}

// ------------------------------------------------------------------ the loop --

// handleInput routes a submitted line to the pending question, or to a new
// turn. Mirror of tuiModel.handleInput; the answer semantics (RePrompt, mode
// switches) belong to Interactive, never to the view.
func (v *guiView) handleInput(text string) {
	g := v.gui
	q := g.PendingQuestion()
	if !g.HasQuestion() {
		// Input is never dropped: slash commands apply at once (they change
		// the mode the next prompt will use) and prose is queued as steering,
		// which lands in the history before the model's next call.
		if text == quitCommand {
			g.quit()
			return
		}
		if text == "" {
			if !g.loopRunning() {
				v.noteLocal("nothing is waiting for input")
			}
			return
		}
		if v.commandOrSteer(text) {
			return
		}
		if !g.loopRunning() {
			v.noteLocal("the run has ended: /quit or " + quitKeyName() + " to leave")
			return
		}
		g.queueSteering(text)
		v.appendBlock(block{kind: "user", text: text})
		v.noteLocal("(queued for the model's next turn)")
		return
	}
	switch q.Kind {
	case AskTask:
		if text == "" {
			v.noteLocal("please describe the task")
			return
		}
		g.resolve(Answer{Text: text})
	case AskLimits:
		v.openDialog("limits")
	case AskConfirm:
		// Empty input confirms; anything else rejects, after slash handling.
		resolved, reprompt := v.slash(text)
		if reprompt {
			return
		}
		// Python's match: "" and "/y" both confirm the batch.
		if resolved == "" || resolved == "/y" {
			g.resolve(Answer{})
			return
		}
		g.resolve(Answer{Reject: true, Text: resolved})
	case AskInterrup:
		g.resolve(Answer{Text: text})
	default: // new-task, human, interruption
		if v.commandOrSteer(text) {
			return
		}
		resolved, reprompt := v.slash(text)
		if reprompt {
			return
		}
		if resolved == "/m" {
			v.noteLocal(guiMultilineHint)
			return
		}
		g.resolve(Answer{Text: resolved})
	}
}

// guiMultilineHint is the GUI's answer to /m: multi-line input is not a mode
// here, it is a keystroke.
const guiMultilineHint = "multi-line input is not a mode here: shift+enter adds a newline, enter sends"

// slash applies the /y /c /u /h commands, printing their feedback into the
// transcript.
func (v *guiView) slash(text string) (string, bool) {
	return slash(v.gui.cfg, text, viewWriter{v}, func() string {
		return text
	})
}

type viewWriter struct{ v *guiView }

func (w viewWriter) Write(p []byte) (int, error) {
	w.v.noteLocal(string(p))
	return len(p), nil
}

// commandOrSteer handles the input affordances that work whether or not a
// prompt is pending: /y /c /u /h /t and /m.
func (v *guiView) commandOrSteer(text string) bool {
	switch text {
	case "/t":
		v.toggleThinking()
		return true
	case "/m":
		v.noteLocal(guiMultilineHint)
		return true
	}
	if _, isMode := modeCommands[text]; !isMode && text != "/h" {
		return false
	}
	_, reprompt := v.slash(text)
	_ = reprompt
	return true
}

// ---------------------------------------------------------------- chatInput --

// chatInput is the message entry. The input focus never leaves it: dialogs
// read from here rather than moving focus, so an answer is always typeable.
type chatInput struct {
	*widget.Entry
	v *guiView

	// Modifier state: fyne.KeyEvent carries no modifiers, but KeyDown/KeyUp
	// report the chord's modifier before TypedKey arrives.
	shift  bool
	option bool
}

func newChatInput(v *guiView) *chatInput {
	e := widget.NewMultiLineEntry()
	e.Wrapping = fyne.TextWrapWord
	e.PlaceHolder = "message · enter sends · shift+enter adds a newline · /h commands"
	c := &chatInput{Entry: e, v: v}
	c.ExtendBaseWidget(c)
	return c
}

func (c *chatInput) KeyDown(key *fyne.KeyEvent) {
	c.Entry.KeyDown(key)
	switch key.Name {
	case desktop.KeyShiftLeft, desktop.KeyShiftRight:
		c.shift = true
	case desktop.KeyAltLeft, desktop.KeyAltRight: // the macOS Option key
		c.option = true
	}
}

func (c *chatInput) KeyUp(key *fyne.KeyEvent) {
	c.Entry.KeyUp(key)
	switch key.Name {
	case desktop.KeyShiftLeft, desktop.KeyShiftRight:
		c.shift = false
	case desktop.KeyAltLeft, desktop.KeyAltRight:
		c.option = false
	}
}

// FocusLost reclaims the caret: tapping a non-focusable object (a bubble, the
// scroll bar, blank space) makes Fyne clear canvas focus, and an entry that
// never gets focus back is the "input box stopped accepting input" bug.
// Nothing else in this window is focusable, so the input always wins. The
// reclaim is deferred: FocusLost fires *before* the focus manager records the
// loss, so an inline Focus(c) would early-return against the still-registered
// focus.
func (c *chatInput) FocusLost() {
	c.Entry.FocusLost()
	time.AfterFunc(0, func() {
		c.v.gui.post(func() {
			if c.v.win.Canvas().Focused() == nil {
				c.v.win.Canvas().Focus(c)
			}
		})
	})
}

func (c *chatInput) TypedKey(key *fyne.KeyEvent) {
	v := c.v
	if v.dialog == "limits" {
		switch key.Name {
		case fyne.KeyReturn, fyne.KeyEnter:
			value := c.Text
			c.Entry.SetText("")
			v.submitLimits(value)
		case fyne.KeyEscape:
			v.closeDialog()
		default:
			c.Entry.TypedKey(key)
		}
		return
	}
	switch key.Name {
	case fyne.KeyEscape:
		// Never forward: the entry ignores Escape, and it is the run's
		// always-available cancel.
		v.esc()
		return
	case fyne.KeyReturn, fyne.KeyEnter:
		// Shift (+ ⌥ on macOS) gives the native newline; enter sends.
		if c.shift || (isMacOS && c.option) {
			c.Entry.TypedKey(key)
		} else {
			c.submit()
		}
		return
	}
	c.Entry.TypedKey(key)
}

func (c *chatInput) TypedRune(r rune) {
	v := c.v
	if v.dialog == "limits" {
		c.Entry.TypedRune(r)
		return
	}
	if r == 'y' && c.Text == "" && v.gui.HasQuestion() && v.gui.PendingQuestion().Kind == AskConfirm {
		// A bare y confirms an empty editor; anything typed is the rejection
		// comment.
		v.gui.resolve(Answer{Text: ""})
		return
	}
	c.Entry.TypedRune(r)
}

// TypedShortcut intercepts the platform-modifier chords the driver builds
// before the entry's editing bindings see them. Everything else must go
// through so copy, paste, undo and word movement keep working.
func (c *chatInput) TypedShortcut(s fyne.Shortcut) {
	v := c.v
	if cs, ok := s.(*desktop.CustomShortcut); ok {
		switch {
		case cs.KeyName == fyne.KeyT && cs.Modifier == thinkingMods():
			v.toggleThinking()
			return
		case (cs.KeyName == fyne.KeyReturn || cs.KeyName == fyne.KeyEnter) && cs.Modifier == fyne.KeyModifierShortcutDefault:
			c.Entry.TypedKey(&fyne.KeyEvent{Name: fyne.KeyReturn}) // native newline
			return
		}
	}
	c.Entry.TypedShortcut(s)
}

// submit sends the text. Enter always submits and Shift+Enter (or ⌥Enter on
// macOS) always inserts a newline at the cursor: multi-line input is a
// keystroke here, never a mode the user can get stuck in.
func (c *chatInput) submit() {
	value := c.Text
	c.Entry.SetText("")
	c.v.handleInput(strings.TrimSpace(value))
}
