package ui

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// shell is the window: one Fyne app, one canvas, one tab strip, and N sessions
// living in it. It exists because everything a session needs to be *alone* - the
// transcript accumulator, the pending question, the ask queue, the cancel
// function, the running-card inventory - was held once per process, and a second
// session in the same window would have written into the first one's transcript
// and answered the first one's confirmation prompt (docs/parallel-sessions.md §7
// step 7.2, decision 15).
//
// The split is by ownership, not by convenience: a GUI holds a session's state and
// its view; the shell holds the canvas, the strip, the menu bar, the sticky window
// size, and the single event loop. Only the shell may quit the app, and only the
// selected session may move the window's title or take the keyboard.
type shell struct {
	app      fyne.App
	window   fyne.Window
	logWrite *LogTarget
	settings Settings
	// post delivers a closure to the thread that owns the canvas; see
	// GUIOptions.Post for why tests replace it.
	post func(fn func())

	// sessions is the tab order, and tabs is its mirror in the strip. They are
	// kept in step by addTab alone - DocTabs has no way to reorder, so insertion
	// order is the tab order (decision 15, consequence 3) - and both belong to the
	// toolkit goroutine like the widget tree they mirror. sh.mu guards only the
	// two preferences below, which a background goroutine may read.
	sessions []*GUI
	tabs     *container.DocTabs
	// cancelItem and the menu closures resolve against the *selected* session at
	// fire time: a menu built for one session and fired during another is a
	// cancel that stops the wrong turn.
	cancelItem *fyne.MenuItem

	// opener assembles a session for another directory; it is the window's
	// capability, taken from whichever GUIOptions first carried one, so every tab's
	// "+" opens a session the same way and a spawned child needs none of it.
	opener func(dir string) (GUIOptions, error)
	// picker is the "+" tab awaiting a directory, or nil. It is not a session, so
	// it lives outside sh.sessions and is the only thing removeItem may take out of
	// the strip on its own.
	picker *container.TabItem

	// lastTitle is the window title last applied. It lives on the shell because
	// it describes the window: a tab that is not visible may not mark its own
	// title as applied, or it would never publish it when the human selects it.
	lastTitle string

	mu       sync.Mutex
	thinking bool
	quitting bool
}

// newShell builds the window and its strip. The first session's GUI is added by
// newSessionGUI, which is what makes `GUI == one tab` true from construction
// rather than by a later step; a shell with no sessions is a window nothing can
// type into, so ShowError's error path builds one anyway.
func newShell(o GUIOptions) *shell {
	a := o.App
	if a == nil {
		a = app.New()
	}
	post := o.Post
	if post == nil {
		post = fyne.Do
	}
	sh := &shell{
		app:      a,
		post:     post,
		settings: o.Settings,
		logWrite: o.LogWriter,
		// The thinking toggle comes back from the store; with nothing stored the
		// transcript opens collapsed, as it always did.
		thinking: loadThinkingShown(o.Settings),
		opener:   o.Opener,
	}
	sh.window = a.NewWindow("mini")
	sh.tabs = container.NewDocTabs()
	sh.tabs.SetTabLocation(container.TabLocationTop)
	// CloseIntercept is a refusal, not a teardown: DocTabs puts an × on every tab
	// whether or not anything handles it, and close() runs the intercept
	// *instead of* removing the item (decision 15, consequence 1). So the intercept
	// owns the whole of closing - the guard, the removal and the cascade - and
	// nothing else in this file may remove a tab.
	sh.tabs.CloseIntercept = sh.closeTab
	// The "+" is a trigger, not a factory: createTab returns nil, so DocTabs
	// appends nothing, and the tab appears when the session behind it does.
	sh.tabs.CreateTab = sh.createTab
	// Selecting a tab - by click, by ⌥→, by a spawn - is the moment the window's
	// two shared resources change hands: the keyboard and the title. Without this
	// the newly visible transcript has no focus owner (its predecessor was
	// removed from the canvas, which clears focus), and the human's next keystroke
	// goes to whichever input happens to hold it.
	sh.tabs.OnSelected = func(item *container.TabItem) {
		g := sh.sessionOf(item)
		if g == nil {
			return
		}
		// Looking at a tab is how a prompt gets answered, so the marker comes off
		// the moment the human arrives.
		g.setAttention(false)
		g.v.focusEditor()
		g.v.refreshStatus()
	}
	sh.window.SetContent(sh.tabs)
	sh.window.SetMainMenu(sh.menus())
	// The saved size is the user's, applied before Show so the window opens at the
	// size it closed at instead of cascading one resize in front of them.
	sh.window.Resize(loadWindowSize(o.Settings))
	sh.window.SetOnClosed(sh.quit)
	return sh
}

// ------------------------------------------------------------------ sessions --

// selectTab shows a session and gives its input the keyboard, which is the only
// way the newly visible tab is typeable - focus does not follow the content.
func (sh *shell) selectTab(g *GUI) {
	if g == nil {
		return
	}
	// Selecting a tab is a window-level event with two per-session consequences:
	// the keyboard goes to the input now visible, and that session's status - which
	// its own ticks were not allowed to publish - finally reaches the title. The
	// strip's OnSelected callback does both on a change, and a DocTabs select is a
	// no-op when the item is already current, so this is done here as well rather
	// than relied on.
	sh.tabs.Select(g.item)
	g.v.focusEditor()
	g.v.refreshStatus()
}

// selected is the session the strip is showing, or nil while the window has no
// sessions at all. DocTabs reports the selected *item*, not an index, so the
// session is found by identity - which is also the only mapping that survives
// SetItems being called with a reordered slice.
func (sh *shell) selected() *GUI { return sh.sessionOf(sh.tabs.Selected()) }

// sessionOf maps a tab item back to the session it shows.
func (sh *shell) sessionOf(item *container.TabItem) *GUI {
	if item == nil {
		return nil
	}
	for _, g := range sh.sessions {
		if g.item == item {
			return g
		}
	}
	return nil
}

// liveSessions counts the tabs still standing, which is how a session that just
// finished knows whether it is allowed to take the window down with it.
func (sh *shell) liveSessions() int { return len(sh.sessions) }

// isCurrent reports whether g is the visible session. Window-level side effects -
// the title, the keyboard, the strip - belong to it and only to it, or a
// background session's status tick would rename the window and steal typing.
func (sh *shell) isCurrent(g *GUI) bool { return sh.selected() == g }

// ------------------------------------------------------------------- running --

// run shows the window, starts the session's loop, and blocks in the toolkit
// event loop until the human quits. It must be called on the main goroutine:
// Fyne's driver owns it from app.Run on.
//
// The mute and the sticky-state write are the shell's, not the session's: the log
// target is process-global, and one session finishing must not unmute the logger
// under the others.
func (sh *shell) run(g *GUI, ctx context.Context, task string) (map[string]any, error) {
	undo := sh.logWrite.Mute()
	defer undo()

	g.prepareTranscript()
	sh.window.Show()
	sh.selectTab(g)
	// The transcript was built before the scroll had a height, so it is stacked
	// above the fold; land on the newest turn.
	g.v.pinned = true
	g.v.scroll.ScrollToBottom()
	g.v.refreshStatus()

	runCtx, cancelRun := context.WithCancel(ctx)
	g.mu.Lock()
	g.cancelRun = cancelRun
	g.mu.Unlock()

	stop := make(chan struct{})
	defer close(stop)
	go g.drive(runCtx, task)
	go sh.tickAll(stop)

	sh.app.Run()
	// Give every session a moment to finish saving its trajectory, then report the
	// one the CLI handed the task to.
	select {
	case <-g.runDone:
	case <-time.After(2 * time.Second):
	}
	return g.result()
}

// tickAll refreshes every session's footer once a second while the window is up.
//
// One timer for N sessions, not N timers, because the work is per-window anyway
// (the title) and a session's card has to keep following its handed-off jobs
// through the idle gap between turns - which is where a training run spends
// nearly all of its life. A session whose run ended and whose jobs are all done
// stops being worth ticking, so it is skipped rather than kept alive.
func (sh *shell) tickAll(stop <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		sh.post(func() {
			for _, g := range sh.sessions {
				if g.idle() {
					continue
				}
				g.v.refreshStatus()
				if g.jobs != nil {
					g.v.refreshJobCards()
				}
			}
		})
	}
}

// idle reports a session with no turn in flight and nothing running: the footer
// cannot change, so the tick has nothing to do for it.
func (g *GUI) idle() bool { return !g.loopRunning() && g.liveJobs() == 0 }

// quit ends the app: the window closes and run returns. Every running loop is
// cancelled first, so leaving never orphans a command mid-execution in any
// session, and the sticky state is written while the window still has a size to
// read.
func (sh *shell) quit() {
	sh.mu.Lock()
	if sh.quitting {
		sh.mu.Unlock()
		return
	}
	sh.quitting = true
	sessions := append([]*GUI(nil), sh.sessions...)
	sh.mu.Unlock()

	for _, g := range sessions {
		g.cancelRunTurn()
	}
	sh.saveStickyState()
	sh.app.Quit()
}

// ------------------------------------------------------------------- sticky ---

// saveStickyState writes what the next launch restores: the thinking toggle and
// the window's content size (the same unit Resize takes, which is why reading the
// canvas rather than the content tree is the honest measurement). Safe on the
// toolkit goroutine, the only place it is called.
func (sh *shell) saveStickyState() {
	if sh.settings == nil {
		return
	}
	saveThinkingShown(sh.settings, sh.showReasoning())
	if sh.window != nil {
		if size := sh.window.Canvas().Size(); size.Width > 0 && size.Height > 0 {
			saveWindowSize(sh.settings, size)
		}
	}
}

// showReasoning is the window's thinking state; every session's transcript renders
// with it, because the toggle rebuilds all of them at once anyway.
func (sh *shell) showReasoning() bool {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.thinking
}

// toggleThinking flips the collapsed state of thinking blocks in every session and
// remembers the flip: quitting and coming back leaves it where the user left it.
// Called on the toolkit goroutine, from the menu and from /t.
func (sh *shell) toggleThinking() bool {
	sh.mu.Lock()
	sh.thinking = !sh.thinking
	on := sh.thinking
	sessions := append([]*GUI(nil), sh.sessions...)
	sh.mu.Unlock()
	// Written outside the lock, because the write is a database call: one upsert
	// on a local WAL file is microseconds, and the settings layer caps itself at
	// two seconds, so the worst a locked database can cost is a slow keystroke
	// rather than a hung window.
	sh.saveStickyState()
	for _, g := range sessions {
		g.v.rebuildAll()
		g.v.noteLocal(map[bool]string{true: "thinking shown", false: "thinking hidden"}[on])
	}
	return on
}

// ------------------------------------------------------- opening and closing --

// closeArmWindow is how long the first × stays remembered: long enough to press
// again after reading the note, short enough that an old press cannot close a run
// that started later.
const closeArmWindow = 10 * time.Second

// createTab is the strip's "+", and it returns a *picker* tab rather than a
// session: opening one writes to the session store and asks the server for its
// context window, none of which belongs on the canvas thread inside a widget
// callback (decision 15, consequence 4). So the tab carries the form, and the
// session opens from here once the human commits - which also means a failed open
// leaves somewhere to say so, instead of a dialog that has already vanished.
//
// (A `dialog.NewForm` would be two fewer widgets and no picker state, but Fyne's
// dialog package drags in a whole virtual-filesystem dependency for a one-field
// form; the tab is the cheaper neighbour.)
func (sh *shell) createTab() *container.TabItem {
	if sh.opener == nil {
		if g := sh.selected(); g != nil {
			g.v.noteLocal("this window cannot open another session")
		}
		return nil
	}
	if sh.picker != nil {
		sh.tabs.Select(sh.picker) // one picker at a time; show the one already open
		return nil
	}
	dir := widget.NewEntry()
	dir.PlaceHolder = "/path/to/another/checkout"
	if g := sh.selected(); g != nil {
		dir.Text = g.dir
	}
	open := func() {
		text := strings.TrimSpace(dir.Text)
		if text == "" {
			dir.SetText("")
			dir.PlaceHolder = "a directory is needed to open a session"
			return
		}
		item := sh.picker
		sh.picker = nil
		sh.removeItem(item)
		go sh.openSessionAt(text)
	}
	dir.OnSubmitted = func(string) { open() }
	buttons := container.NewHBox(
		widget.NewButtonWithIcon("Open", theme.ConfirmIcon(), open),
		widget.NewButtonWithIcon("Cancel", theme.CancelIcon(), func() {
			item := sh.picker
			sh.picker = nil
			sh.removeItem(item)
			sh.restoreFocus()
		}),
	)
	content := container.NewBorder(nil, buttons, nil, nil,
		container.NewVBox(widget.NewLabel("Open a session in this working directory:"), dir))
	sh.picker = container.NewTabItem("new session", content)
	return sh.picker
}

// openSessionAt opens one session in dir and puts it in a tab. Any goroutine: the
// opening blocks, so it is never called from the canvas, and only the widget work
// is posted back - the same shape a spawned child arrives by, except that the
// human asked for this tab and so it is selected.
func (sh *shell) openSessionAt(dir string) {
	o, err := sh.opener(dir)
	if err == nil {
		var g *GUI
		g, err = sh.openSession(o)
		if err == nil {
			sh.post(func() { sh.selectTab(g) })
			return
		}
	}
	msg := "could not open a session in " + dir + ": " + err.Error()
	sh.post(func() {
		sh.restoreFocus()
		if g := sh.selected(); g != nil {
			g.v.noteLocal(msg)
		}
	})
}

// closeTab is a tab's ×, and it owns the teardown: the guard, the removal, and the
// session's own ending. × on a session that is still working needs a second press,
// because closing cascades - the session's turn, its background jobs and its
// children all go with it (decision 11) - and a stray click must not end a run
// twenty minutes into a training job. The transcript survives either way: it is in
// the store, and the tab is only a view of it.
func (sh *shell) closeTab(item *container.TabItem) {
	g := sh.sessionOf(item)
	if g == nil {
		if item == sh.picker { // an abandoned picker closes like the form it is
			sh.picker = nil
			sh.removeItem(item)
			sh.restoreFocus()
		}
		return
	}
	if (g.loopRunning() || g.liveJobs() > 0) && time.Since(g.closeArmed) > closeArmWindow {
		g.closeArmed = time.Now()
		g.v.noteLocal("this session is still working: press × again to stop it and close its tab")
		return
	}
	sh.removeTab(g)
	if g.closer != nil {
		// Quit blocks through the unwind; the window stays responsive and the
		// session's ending is recorded by host, not by this view.
		go func() { _ = g.closer() }()
	}
}

// removeItem takes one item out of the strip. Every removal goes through here,
// including the ones DocTabs would otherwise do itself, so the strip and
// sh.sessions cannot drift apart (decision 15, consequence 1).
func (sh *shell) removeItem(item *container.TabItem) {
	if item == nil {
		return
	}
	sh.tabs.RemoveIndex(slices.Index(sh.tabs.Items, item))
}

// removeTab takes the tab out and lands the strip on a neighbour, or ends the
// window when nothing is left to show. Selecting before DocTabs has finished
// reshuffling is deliberate: consequence 3 of decision 15 is that a removal leaves
// the highlighted item and the content to be re-resolved by the caller.
func (sh *shell) removeTab(g *GUI) {
	i := slices.Index(sh.sessions, g)
	if i < 0 {
		return
	}
	sh.sessions = slices.Delete(sh.sessions, i, i+1)
	sh.removeItem(g.item)
	g.item = nil
	if len(sh.sessions) == 0 {
		sh.quit()
		return
	}
	sh.selectTab(sh.sessions[min(i, len(sh.sessions)-1)])
}

// restoreFocus puts the caret back in the visible session's input, which is what a
// dismissed dialog needs: Fyne leaves focus nowhere when the modal goes away, and
// the tab's own deferred reclaim cannot see a loss it did not cause.
func (sh *shell) restoreFocus() {
	if g := sh.selected(); g != nil {
		g.v.focusEditor()
	}
}

// selectOffset moves the strip by d tabs and wraps, so ⌥→ and ⌥← cycle instead of
// dead-ending at the last tab.
func (sh *shell) selectOffset(d int) {
	n := len(sh.sessions)
	if n < 2 {
		return
	}
	i := slices.Index(sh.sessions, sh.selected())
	if i < 0 {
		i = 0
	}
	sh.selectTab(sh.sessions[(i+d+n)%n])
}

// ------------------------------------------------------------------- menus --

func (sh *shell) menus() *fyne.MainMenu {
	run := fyne.NewMenu("Run")
	cancel := fyne.NewMenuItem("Cancel turn", func() {
		g := sh.selected()
		if g == nil || !g.loopRunning() {
			return
		}
		g.CancelCurrentTurn()
		g.v.noteLocal("cancelling this turn…")
	})
	sh.cancelItem = cancel
	run.Items = append(run.Items, cancel)
	run.Items = append(run.Items, fyne.NewMenuItem("Compact now", func() {
		g := sh.selected()
		if g == nil || !g.loopRunning() {
			return
		}
		g.loop.RequestCompaction()
		g.v.noteLocal("compaction armed: runs before the model's next turn")
	}))
	run.Items = append(run.Items, fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Quit", sh.quit))
	quit := run.Items[len(run.Items)-1]
	// Platform conventions: ⌘ on macOS, Ctrl elsewhere (KeyModifierShortcut
	// Default is the platform's primary modifier).
	quit.Shortcut = &desktop.CustomShortcut{KeyName: fyne.KeyQ, Modifier: fyne.KeyModifierShortcutDefault}
	thinking := fyne.NewMenuItem("Show thinking", func() { sh.toggleThinking() })
	thinking.Shortcut = &desktop.CustomShortcut{KeyName: fyne.KeyT, Modifier: thinkingMods()}

	// Edit exists for one reason: glfw hands a shortcut to the focused widget and
	// stops there, so a canvas-level ⌘C never runs while a card's text selection
	// holds the focus. Main-menu shortcuts fire *before* the focused widget
	// (internal/driver/glfw/window.go triggerMainMenuShortcut), which is what makes
	// Copy mean the same thing in the input and in any card body - of any tab.
	copyItem := fyne.NewMenuItem("Copy", sh.copySelection)
	copyItem.Shortcut = &fyne.ShortcutCopy{}

	commands := fyne.NewMenuItem("Commands", func() {
		g := sh.selected()
		if g == nil {
			return
		}
		g.v.noteLocal("Current mode: " + g.cfg.Mode + "\n" + helpText)
	})
	// Session is the strip's own commands. The chords exist because the labels
	// scroll out of sight on a narrow window, and closing needs a keyboard path
	// now that × answers to it (decision 13's "this session only, never a
	// sibling").
	next := fyne.NewMenuItem("Next session", func() { sh.selectOffset(1) })
	next.Shortcut = &desktop.CustomShortcut{KeyName: fyne.KeyRight, Modifier: fyne.KeyModifierAlt}
	prev := fyne.NewMenuItem("Previous session", func() { sh.selectOffset(-1) })
	prev.Shortcut = &desktop.CustomShortcut{KeyName: fyne.KeyLeft, Modifier: fyne.KeyModifierAlt}
	closeItem := fyne.NewMenuItem("Close session", func() {
		if g := sh.selected(); g != nil {
			sh.closeTab(g.item)
		}
	})
	closeItem.Shortcut = &desktop.CustomShortcut{KeyName: fyne.KeyW, Modifier: fyne.KeyModifierShortcutDefault}

	return fyne.NewMainMenu(run, fyne.NewMenu("Edit", copyItem),
		fyne.NewMenu("Session", next, prev, fyne.NewMenuItemSeparator(), closeItem),
		fyne.NewMenu("View", thinking), fyne.NewMenu("Help", commands))
}

// copySelection is Edit ▸ Copy, and the only copy path that works everywhere in
// this window: a selectable body copies itself while it holds focus, but taking
// that focus is exactly what a card selection does, so ⌘C has to be answered above
// the focus - see shell.menus.
//
// It routes by *focus*, not by the selected tab, and after copying out of a card it
// puts the caret in the visible session's input, because a card holding focus
// swallows every keystroke (widget.focusSelectable's TypedKey and TypedRune are
// no-ops) and the input being typeable is not optional. The highlight goes with the
// focus - Fyne draws a selection only on the focused widget - so ⌘C doubles as the
// "copied" feedback; both halves cannot be had.
func (sh *shell) copySelection() {
	focus := sh.window.Canvas().Focused()
	sel, ok := focus.(interface{ SelectedText() string })
	if !ok {
		return
	}
	text := sel.SelectedText()
	if text == "" {
		return
	}
	// The app clipboard, not the window's: widget.selectable writes here, and the
	// two are different objects (per-window on Unix, a single-use stub in the test
	// driver, where writing it is silently invisible).
	sh.app.Clipboard().SetContent(text)
	if g := sh.selected(); g != nil && focus != fyne.Focusable(g.v.editor) {
		// Not ClearSelection: the focused object is widget.focusSelectable, and
		// only the RichText above it carries that method, so it is unreachable from
		// outside package widget. Losing the focus hides the rectangles anyway.
		sh.window.Canvas().Focus(g.v.editor)
	}
}
