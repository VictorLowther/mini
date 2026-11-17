package ui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/model"
)

// GUIOptions wires the desktop UI to a configured loop. App is the injection
// point for the headless test app; nil means the real one.
type GUIOptions struct {
	Cfg        *agent.Config
	Loop       *agent.Loop
	Model      *model.Sushi
	ConfigName string
	// LogWriter is muted for the duration of Run: the window owns the screen,
	// and a stray log write to the launching terminal is noise nobody reads.
	LogWriter *LogTarget
	App       fyne.App
	// Settings carries the state that survives a quit - the thinking toggle
	// and the window's size - and is read once, at construction. nil is a
	// valid value (a GUI with no store opens collapsed at the default size).
	Settings Settings
	// Jobs reads the registry for the cards of calls that handed off, so the
	// window can keep them live until the job ends. nil leaves the cards
	// truthful but static.
	Jobs JobReader
	// Post delivers a closure to the thread that owns the canvas. Production
	// leaves it nil and gets fyne.Do; a test replaces it because the software
	// driver runs fyne.Do on whichever goroutine called it (test/driver.go:53),
	// while this package defers work from time.AfterFunc callbacks. It is
	// installed here rather than assigned afterwards because building the window
	// already schedules a deferred recompute, and that callback reads the field -
	// which made the assignment itself a data race.
	Post func(fn func())
}

// GUI is the Fyne desktop front end, the interactive face of mini. It is both
// the UI the agent talks to and the owner of the event loop: the agent runs in
// a goroutine and every callback reaches the screen through fyne.Do (the post
// field), so all widget access happens on the toolkit goroutine. State shared
// with the agent goroutine is guarded by one mutex.
//
// The transcript model is ui/blocks.go: same blocks, same questions, same
// answer semantics, same notes. Only the key names in the "how to leave" notes
// speak desktop (esc for ctrl-c).
type GUI struct {
	cfg        *agent.Config
	loop       *agent.Loop
	modelName  string
	baseURL    string
	configName string
	// contextNote is the resolved context window and its source, from the
	// model client (see model.Sushi.DiscoverContextWindow).
	contextNote string

	app       fyne.App
	window    fyne.Window
	logWriter *LogTarget
	// tabs is the session strip. It holds exactly one item until the per-session
	// state moves out of GUI (docs/parallel-sessions.md §5.15). The container is
	// DocTabs and not AppTabs because, verified against Fyne 2.8.2, AppTabs has
	// no close affordance, no create button and no scroller, and a tab per
	// session needs all three; its mutation API is otherwise identical to
	// DocTabs', which is why this was a cheap bet to make at the seam.
	tabs *container.DocTabs
	tab  *container.TabItem
	v    *guiView // one session's view; N of these is what tabs is for
	// jobs backs the live cards for calls that handed off. See GUIOptions.Jobs.
	jobs JobReader
	// asks serializes prompts so exactly one question is on screen at a time.
	// See askQueue and docs/parallel-sessions.md §9.4.
	asks askQueue

	// post delivers a closure to the thread that owns the canvas, and it is the
	// only way anything here may reach the view: the deferred passes are
	// scheduled from time.AfterFunc callbacks, which run on the Go timer
	// goroutine, and from the run and status goroutines. Production is fyne.Do.
	// Tests inject a queue drained by the test goroutine, because the software
	// driver runs Do inline on the calling goroutine and would otherwise let a
	// timer mutate view state beside the canvas pass that is measuring it.
	post func(fn func())

	// inflight are the commands the current step has started but not finished,
	// and inflightSince when they began. See Issuer.
	inflight []string

	mu       sync.Mutex
	pending  chan Answer
	lastQ    Question
	stream   streamState
	runInfo  map[string]any
	runError error
	runDone  chan struct{}
	// cancelRun stops the run (quit).
	cancelRun context.CancelFunc
	// reasoningVisible is toggled with ctrl+t / /t, and is sticky across
	// launches (see ui/prefs.go); hidden until someone asks otherwise.
	reasoningVisible bool
	// task is the run's task text, shown on one line in place of the rendered
	// instance template.
	task string
	// resume/replay into the transcript at startup (see GUI.Resume);
	// sessionID is the sessions row this run writes, shown in the header.
	resume     []model.Message
	resumeNote string
	sessionID  string

	// settings is the sticky-state store handed in through GUIOptions; nil
	// means this GUI has nowhere to remember anything. It is written from the
	// toolkit goroutine only, and never read after construction.
	settings Settings
}

// NewGUI builds the window and installs the GUI as the loop's streaming
// handler. Cfg and Model fields may be zero on the startup-error path; the
// GUI must still be able to show a message.
func NewGUI(o GUIOptions) *GUI {
	cfg := o.Cfg
	if cfg == nil {
		cfg = &agent.Config{}
	}
	a := o.App
	if a == nil {
		a = app.New()
	}
	post := o.Post
	if post == nil {
		post = fyne.Do
	}
	g := &GUI{
		cfg:        cfg,
		post:       post,
		loop:       o.Loop,
		configName: o.ConfigName,
		logWriter:  o.LogWriter,
		app:        a,
		runDone:    make(chan struct{}),
		settings:   o.Settings,
		jobs:       o.Jobs,
		// The thinking toggle comes back from the store; with nothing stored
		// the window opens collapsed, as it always did.
		reasoningVisible: loadThinkingShown(o.Settings),
	}
	if o.Model != nil {
		g.modelName = o.Model.Model
		g.baseURL = o.Model.BaseURL
		// The header states the window only once discovery has settled it:
		// "window unknown" in a transcript line nobody asked about is noise,
		// and by the time a run exists the CLI has already resolved one way or
		// the other (model.DiscoverContextWindow).
		if o.Model.ContextWindow > 0 {
			g.contextNote = o.Model.ContextWindowNote()
		}
	}
	w := a.NewWindow("mini")
	g.window = w
	g.v = newGUIView(g, w)
	// The tab strip wraps the existing view unchanged, which is what makes this
	// increment behaviour-neutral: nothing else in the file reaches for the
	// window's content object, so every current path - banner, transcript,
	// question panel, live cards - runs inside the one tab exactly as before.
	g.tabs = container.NewDocTabs(container.NewTabItem(g.tabLabel(), g.v.root()))
	g.tab = g.tabs.Items[0]
	g.tabs.SetTabLocation(container.TabLocationTop)
	// CloseIntercept is set, and it refuses. This is the §5.15 gotcha that turns
	// a half-wired close into a lost session: DocTabs' renderer puts an × on
	// every tab, and close() calls the intercept *instead of* removing the item
	// and firing OnClosed. With no intercept at all, a stray hover click would
	// delete the only tab and leave an empty window holding a live session -
	// worse than any way mini currently has of losing work. The per-tab quit
	// guard that makes × mean "stop this session, or cancel" is a later step, so
	// until it exists the × says so rather than acting.
	g.tabs.CloseIntercept = func(*container.TabItem) {
		g.v.noteLocal("a tab closes its session once the per-tab quit guard lands; leave with /quit for now")
	}
	w.SetContent(g.tabs)
	w.SetMainMenu(g.menus())
	// The saved size is the user's, and it is applied before Show so the
	// window opens at the size it closed at instead of cascading one resize
	// in front of them.
	w.Resize(loadWindowSize(o.Settings))
	// Closing the window is the deliberate way out: same path as /quit,
	// cancelling the run if one is in flight.
	w.SetOnClosed(g.quit)
	if o.Loop != nil {
		o.Loop.Handler = &model.Handler{Content: g.Content, Reasoning: g.Reasoning}
	}
	return g
}

// tabLabel is the tab's title. It names the model because that is the only
// per-session identity GUI holds today: agent.Loop carries no session pointer,
// and the name and working directory that would actually tell three tabs apart
// live on host.Session, arriving with the steps that let a human open a second
// one (§5.15). Until then a second tab would read the same, which is honest
// about what is wired rather than a placeholder someone forgets to replace.
func (g *GUI) tabLabel() string {
	if g.modelName != "" {
		return g.modelName
	}
	return "session"
}

// Interactive: the GUI always has a human on the other end.
func (g *GUI) Interactive() bool { return true }

// Run builds the transcript, shows the window, drives the loop and blocks in
// the toolkit event loop until the user quits. It must be called on the main
// goroutine: Fyne's driver owns it from app.Run on (see cmd/mini/gui.go).
func (g *GUI) Run(ctx context.Context, task string) (map[string]any, error) {
	undo := g.logWriter.Mute()
	defer undo()

	runCtx, cancelRun := context.WithCancel(ctx)
	g.mu.Lock()
	g.cancelRun = cancelRun
	g.mu.Unlock()
	g.prepareTranscript()
	g.v.start()

	stopTicker := make(chan struct{})
	defer close(stopTicker)
	go g.drive(runCtx, task)
	go g.tickStatus(stopTicker)

	g.app.Run()
	// Give the loop a moment to finish saving its trajectory.
	select {
	case <-g.runDone:
	case <-time.After(2 * time.Second):
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runInfo, g.runError
}

// ShowError presents a setup failure inside the window - the only surface the
// user gets after startup - and runs until the window is closed. It returns
// the exit code.
func (g *GUI) ShowError(err error) int {
	g.v.noteLocal("mini-go " + agent.Version)
	g.v.appendBlock(block{kind: "error", text: "mini: " + err.Error()})
	g.v.appendBlock(block{kind: "note", text: "close the window or type /quit to leave"})
	g.v.start()
	g.app.Run()
	return 1
}

// prepareTranscript fills the header, the resume replay and the resume note.
// Call on the main goroutine before app.Run: widget access is safe there and
// fyne.Do would only queue onto a loop nothing runs yet.
func (g *GUI) prepareTranscript() {
	g.v.noteLocal(sessionHeaderLine(g.modelName, g.baseURL, g.configName, g.sessionID, g.contextNote))
	for _, msg := range g.resume {
		// The stored system and instance rows are prompt plumbing, exactly as
		// in a live run: they never reach the transcript.
		if _, built := msg["extra"]; !built {
			continue
		}
		if b, ok, _ := g.blockFor(msg); ok {
			g.v.appendBlock(b)
		}
	}
	if g.resumeNote != "" {
		g.v.noteLocal(g.resumeNote)
	}
	g.v.refreshStatus()
}

// tickStatus refreshes the footer once a second while a command runs, and keeps
// going after the turn ends for as long as the registry has anything running.
//
// The extension is the point: a call that handed off is not finished, and the
// card that tracks it has to keep following the job through the idle window
// between turns - which is where a training run spends nearly all of its life.
// The lifetime stays the run's own (this goroutine is stopped by the same defer
// that closes stopTicker), so the view never gains a timer outliving its window.
func (g *GUI) tickStatus(stop <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		g.post(func() {
			g.v.refreshStatus()
			if g.jobs != nil {
				g.v.refreshJobCards()
			}
		})
		select {
		case <-g.runDone:
			// The turn is over. Stop only once nothing is running: quitting the
			// watch here would freeze every handed-off card at "still running"
			// and never show the outcome the wait is for.
			if g.liveJobs() == 0 {
				return
			}
		default:
		}
	}
}

// drive asks for a task when needed, then runs the loop.
func (g *GUI) drive(ctx context.Context, task string) {
	defer close(g.runDone)
	if task == "" {
		ans, err := g.Ask(ctx, Question{Kind: AskTask, Title: TaskPrompt, Cfg: g.cfg})
		if err != nil {
			g.finish(nil, err)
			return
		}
		task = ans.Text
	}
	g.mu.Lock()
	g.task = task
	g.mu.Unlock()
	info, err := g.loop.Run(ctx, task)
	g.finish(info, err)
}

func (g *GUI) finish(info map[string]any, err error) {
	g.mu.Lock()
	g.runInfo, g.runError = info, err
	g.mu.Unlock()
	g.post(func() { g.v.showRunDone() })
}

// quit ends the app: the window closes and Run returns. A running loop is
// cancelled first, so leaving never orphans a command mid-execution, and the
// sticky state is written before the driver takes the window away - this is
// the one moment the size is guaranteed to still be readable and the last
// moment anybody cares.
func (g *GUI) quit() {
	g.mu.Lock()
	cancel := g.cancelRun
	g.mu.Unlock()
	if g.loopRunning() && cancel != nil {
		cancel()
	}
	g.saveStickyState()
	g.app.Quit()
}

// saveStickyState writes what the next launch restores: the thinking toggle
// and the window's content size (the same unit Resize takes, which is why
// reading the canvas rather than the content tree is the honest measurement).
// Safe from the toolkit goroutine, the only place it is called.
func (g *GUI) saveStickyState() {
	if g.settings == nil {
		return
	}
	saveThinkingShown(g.settings, g.showReasoning())
	if g.window != nil {
		if size := g.window.Canvas().Size(); size.Width > 0 && size.Height > 0 {
			saveWindowSize(g.settings, size)
		}
	}
}

var _ UI = (*GUI)(nil)

// ---------------------------------------------------------------- streaming --

func (g *GUI) Reasoning(text string) {
	g.mu.Lock()
	g.stream.reasoning.WriteString(text)
	g.mu.Unlock()
	g.post(func() { g.v.streamChunk() })
}

func (g *GUI) Content(text string) {
	g.mu.Lock()
	g.stream.content.WriteString(text)
	g.mu.Unlock()
	g.post(func() { g.v.streamChunk() })
}

// StreamText returns the text streamed so far for the turn in flight.
func (g *GUI) StreamText() (reasoning, content string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stream.reasoning.String(), g.stream.content.String()
}

// Message records a finished message in the transcript.
func (g *GUI) Message(m model.Message) {
	b, ok, taskNote := g.blockFor(m)
	if taskNote {
		// The task was typed into the input box, so it reads as a user entry:
		// the right-justified area of the full task text, standing in for the
		// rendered template, which is prompt plumbing and never displays.
		g.mu.Lock()
		task := g.task
		g.mu.Unlock()
		if task == "" {
			g.note("task: (no task text recorded yet)")
		} else {
			g.post(func() { g.v.appendBlock(block{kind: "user", text: task}) })
		}
		return
	}
	if !ok {
		return
	}
	// Steering is echoed the instant it is typed (a right-justified bubble);
	// the queued message reaching the history is the same text, so it does
	// not get a second card.
	if b.kind == "user" && b.subtype == "UserSteering" {
		return
	}
	g.post(func() { g.v.commitBlock(b) })
}

// blockFor maps one message through the shared messageBlock: live and resumed
// turns take one path.
func (g *GUI) blockFor(m model.Message) (block, bool, bool) {
	g.mu.Lock()
	task := g.task
	g.mu.Unlock()
	return messageBlock(m, g.AssistantHeader(), task, func() {
		// The live area held the streamed text; this message supersedes it.
		g.mu.Lock()
		g.stream = streamState{}
		g.mu.Unlock()
	}, g.jobs)
}

// Resume queues a stored transcript for replay, with a note naming where it
// came from. Call before Run.
func (g *GUI) Resume(msgs []model.Message, note string) { g.resume, g.resumeNote = msgs, note }

// SetSessionID labels the transcript header with the session this run writes
// to. Call before Run.
func (g *GUI) SetSessionID(id string) { g.sessionID = id }

// note appends UI-generated lines (help text, mode changes, errors).
func (g *GUI) note(text string) {
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line != "" {
			l := line
			g.post(func() { g.v.appendBlock(block{kind: "note", text: l}) })
		}
	}
}

// queueSteering appends a user message to the loop's steering queue.
func (g *GUI) queueSteering(text string) {
	if g.loop == nil {
		return
	}
	g.loop.QueueMessage(model.Message{
		"role":    "user",
		"content": text,
		"extra":   map[string]any{"interrupt_type": "UserSteering"},
	})
}

// RetryNotice tells the transcript that a model call is being retried.
func (g *GUI) RetryNotice(attempt, max int, err error, wait time.Duration) {
	g.note(retryNotice(attempt, max, err, wait))
}

// Write lets ui.slash() print into the transcript.
func (g *GUI) Write(p []byte) (int, error) {
	g.note(string(p))
	return len(p), nil
}

// ---------------------------------------------------------------- in-flight calls ---

// IssueCalls implements Issuer. It draws the card for work that has started and
// whose result cannot be on screen yet, then keeps the elapsed count honest once
// a second a tick until the step settles.
func (g *GUI) IssueCalls(actions []model.Action) {
	lines := make([]string, 0, len(actions))
	for _, a := range actions {
		if d := a.Display(); d != "" {
			lines = append(lines, d)
		}
	}
	g.mu.Lock()
	g.inflight = lines
	g.mu.Unlock()
	if len(lines) == 0 {
		return
	}
	g.post(func() { g.v.showRunning(g.runningSnapshot()) })
}

// SettleCalls implements Issuer.
func (g *GUI) SettleCalls() {
	g.mu.Lock()
	had := len(g.inflight) > 0
	g.inflight = nil
	g.mu.Unlock()
	if had {
		g.post(func() { g.v.hideRunning() })
	}
}

func (g *GUI) runningSnapshot() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.inflight) == 0 {
		return nil
	}
	out := make([]string, len(g.inflight))
	copy(out, g.inflight)
	return out
}

// ---------------------------------------------------------------- the ask loop ---

// Ask shows a question and blocks until the keyboard answers it.
func (g *GUI) Ask(ctx context.Context, q Question) (Answer, error) {
	// Take the turn first. Publishing without it is the bug §9.4 records: this
	// function used to overwrite g.pending unconditionally, so a second caller
	// silently orphaned the first one's channel and that goroutine waited for an
	// answer that could never arrive. Queued callers see nothing until their turn
	// comes, which is the "one at a time" the ruling settled on.
	release, err := g.asks.acquire(ctx)
	if err != nil {
		return Answer{}, err
	}
	defer release()

	reply := make(chan Answer, 1)
	g.mu.Lock()
	g.pending = reply
	g.lastQ = q
	g.mu.Unlock()
	g.post(func() { g.v.showQuestion(q) })
	select {
	case ans := <-reply:
		return ans, nil
	case <-ctx.Done():
		g.mu.Lock()
		g.pending = nil
		g.mu.Unlock()
		g.post(func() { g.v.hideQuestion() })
		return Answer{}, ctx.Err()
	}
}

// resolve answers the pending question. It runs on the toolkit goroutine (the
// only place keyboard input is handled), so the panel comes down right here.
func (g *GUI) resolve(a Answer) {
	g.mu.Lock()
	ch := g.pending
	g.pending = nil
	g.mu.Unlock()
	g.v.questionResolved()
	if ch != nil {
		ch <- a
	}
}

// HasQuestion reports whether a prompt is waiting for an answer.
func (g *GUI) HasQuestion() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pending != nil
}

// PendingQuestion returns the question being answered.
func (g *GUI) PendingQuestion() Question {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastQ
}

// ToggleReasoning flips the collapsed state of thinking blocks, and remembers
// the flip: quitting and coming back leaves it exactly where the user left it.
func (g *GUI) ToggleReasoning() bool {
	g.mu.Lock()
	g.reasoningVisible = !g.reasoningVisible
	on := g.reasoningVisible
	g.mu.Unlock()
	// Written outside the lock, because the write is a database call: one
	// upsert on a local WAL file is microseconds, and the settings layer caps
	// itself at two seconds, so the worst a locked database can cost is a slow
	// keystroke rather than a hung window.
	g.saveStickyState()
	return on
}

func (g *GUI) showReasoning() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reasoningVisible
}

// loopRunning reports whether the agent is inside Loop.Run.
func (g *GUI) loopRunning() bool { return g.loop != nil && g.loop.Running() }

// CancelCurrentTurn interrupts the command in flight (esc).
func (g *GUI) CancelCurrentTurn() { g.loop.CancelTurn() }

// result returns how the run ended.
func (g *GUI) result() (map[string]any, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runInfo, g.runError
}

// AssistantHeader is the title an assistant block carries. The streaming
// preview uses it too.
func (g *GUI) AssistantHeader() string {
	snap := g.snapshot()
	return assistantHeader(snap.Calls, snap.Cost)
}

// RunStats renders the footer, with the live job count appended. The count is
// the cheapest possible answer to "is anything still running", and it is the one
// piece of this feature that stays true even when the transcript is scrolled up
// through history and the cards themselves are not being repainted.
func (g *GUI) RunStats() string {
	text := runStatsText(g.snapshot(), g.cfg.Mode, g.showReasoning(), g.cfg.ContextWindow)
	if n := g.liveJobs(); n > 0 {
		text += fmt.Sprintf(" \u00b7 %d %s", n, map[bool]string{true: "job", false: "jobs"}[n == 1])
	}
	return text
}

// liveJobs asks the registry how many runs are still burning. An unreadable or
// unattached registry reports 0: a footer that silently dropped the count is a
// worse outcome than one that never showed it, but never a count that lies the
// other way, so a reader of the footer can trust "no jobs" only when a registry
// is wired up - which is why this reports through the same reader the cards use.
func (g *GUI) liveJobs() int {
	lister, ok := g.jobs.(jobCounter)
	if !ok {
		return 0
	}
	n, _ := lister.LiveJobs()
	return n
}

// jobCounter is the second half of what the footer needs. JobReader answers one
// card; a count needs the whole set, and asking for it separately keeps the card
// interface small.
type jobCounter interface {
	LiveJobs() (int, error)
}

func (g *GUI) snapshot() agent.Snapshot {
	if g.loop == nil {
		return agent.Snapshot{}
	}
	return g.loop.Snapshot()
}

// ------------------------------------------------------------------- menus --

func (g *GUI) menus() *fyne.MainMenu {
	run := fyne.NewMenu("Run")
	cancel := fyne.NewMenuItem("Cancel turn", func() {
		if !g.loopRunning() {
			return
		}
		g.CancelCurrentTurn()
		g.v.noteLocal("cancelling this turn…")
	})
	g.v.cancelItem = cancel
	run.Items = append(run.Items, cancel)
	run.Items = append(run.Items, fyne.NewMenuItem("Compact now", func() {
		if !g.loopRunning() {
			return
		}
		g.loop.RequestCompaction()
		g.v.noteLocal("compaction armed: runs before the model's next turn")
	}))
	run.Items = append(run.Items, fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Quit", g.quit))
	quit := run.Items[len(run.Items)-1]
	// Platform conventions: ⌘ on macOS, Ctrl elsewhere (KeyModifierShortcut
	// Default is the platform's primary modifier).
	quit.Shortcut = &desktop.CustomShortcut{KeyName: fyne.KeyQ, Modifier: fyne.KeyModifierShortcutDefault}
	thinking := fyne.NewMenuItem("Show thinking", func() { g.v.toggleThinking() })
	thinking.Shortcut = &desktop.CustomShortcut{KeyName: fyne.KeyT, Modifier: thinkingMods()}
	commands := fyne.NewMenuItem("Commands", func() {
		g.v.noteLocal("Current mode: " + g.cfg.Mode + "\n" + helpText)
	})
	return fyne.NewMainMenu(run, fyne.NewMenu("View", thinking), fyne.NewMenu("Help", commands))
}
