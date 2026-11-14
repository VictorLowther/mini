package ui

import (
	"context"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
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

	app       fyne.App
	window    fyne.Window
	logWriter *LogTarget
	v         *guiView

	// post delivers a closure to the toolkit goroutine. Production is
	// fyne.Do; tests replace it with a serialized direct call so deliveries
	// land in order.
	post func(fn func())

	mu       sync.Mutex
	pending  chan Answer
	lastQ    Question
	stream   streamState
	runInfo  map[string]any
	runError error
	runDone  chan struct{}
	// cancelRun stops the run (quit).
	cancelRun context.CancelFunc
	// reasoningVisible is toggled with ctrl+t / /t; hidden by default.
	reasoningVisible bool
	// task is the run's task text, shown on one line in place of the rendered
	// instance template.
	task string
	// resume/replay into the transcript at startup (see GUI.Resume);
	// sessionID is the sessions row this run writes, shown in the header.
	resume     []model.Message
	resumeNote string
	sessionID  string
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
	g := &GUI{
		cfg:        cfg,
		loop:       o.Loop,
		configName: o.ConfigName,
		logWriter:  o.LogWriter,
		app:        a,
		runDone:    make(chan struct{}),
		post:       fyne.Do,
	}
	if o.Model != nil {
		g.modelName = o.Model.Model
		g.baseURL = o.Model.BaseURL
	}
	w := a.NewWindow("mini")
	g.window = w
	g.v = newGUIView(g, w)
	w.SetContent(g.v.root())
	w.SetMainMenu(g.menus())
	w.Resize(fyne.NewSize(960, 720))
	// Closing the window is the deliberate way out: same path as /quit,
	// cancelling the run if one is in flight.
	w.SetOnClosed(g.quit)
	if o.Loop != nil {
		o.Loop.Handler = &model.Handler{Content: g.Content, Reasoning: g.Reasoning}
	}
	return g
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
	g.v.noteLocal(sessionHeaderLine(g.modelName, g.baseURL, g.configName, g.sessionID))
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

// tickStatus refreshes the footer while a command runs and the loop is quiet.
func (g *GUI) tickStatus(stop <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-g.runDone:
			return
		case <-t.C:
			g.post(func() { g.v.refreshStatus() })
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
// cancelled first, so leaving never orphans a command mid-execution.
func (g *GUI) quit() {
	g.mu.Lock()
	cancel := g.cancelRun
	g.mu.Unlock()
	if g.loopRunning() && cancel != nil {
		cancel()
	}
	g.app.Quit()
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
	})
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

// ---------------------------------------------------------------- the ask loop ---

// Ask shows a question and blocks until the keyboard answers it.
func (g *GUI) Ask(ctx context.Context, q Question) (Answer, error) {
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

// ToggleReasoning flips the collapsed state of thinking blocks.
func (g *GUI) ToggleReasoning() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reasoningVisible = !g.reasoningVisible
	return g.reasoningVisible
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

// RunStats renders the footer.
func (g *GUI) RunStats() string {
	return runStatsText(g.snapshot(), g.cfg.Mode, g.showReasoning())
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
