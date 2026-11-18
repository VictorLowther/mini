package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"

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
	// TabLabel names this session's tab when the caller has a better name than the
	// model: a spawned subagent is known by the name its parent gave it.
	TabLabel string
	// Dir is the working directory this session runs in. It is what tells two
	// top-level tabs apart, and what the strip's "+" opens a new session beside.
	Dir string
	// Closer ends this session - its turn, its jobs, its children - and blocks
	// through the unwind, so its caller runs it off the canvas thread. nil means
	// the window has no way to stop what it is showing, and closing the tab then
	// only takes the tab away.
	Closer func() error
	// JobEnd stops one run the registry still tracks - a handed-off command or a
	// subagent - and waits for the unwind, so its caller keeps it off the canvas
	// thread. nil means the window has no way to end a job, and running job cards
	// then carry no control at all rather than one that lies.
	JobEnd func(id string) error
	// Exporter writes one session's trajectory out of the store and returns the
	// path it wrote - the only way a trajectory file appears, since running a
	// session saves nothing but the store. nil means this window has nowhere to
	// export through, and export then says so instead of writing something nobody
	// asked for to a path nobody chose.
	Exporter func(id, path string) (string, error)
	// Opener assembles a session for another working directory. It belongs to the
	// window rather than to the session that carried it first: opening a session
	// writes to the session store and asks the server for its context window, so
	// the caller must run it off the canvas thread and post what comes back.
	Opener func(dir string) (GUIOptions, error)
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

// GUI is ONE session's Fyne front end: the interactive face of mini for one
// agent.Loop, and the owner of that session's transcript, input, question and
// streaming state. The window it is drawn in belongs to a shell, which holds the
// canvas, the tab strip, the menu bar and the single event loop, and may host any
// number of these - one per top-level session and one per subagent (decision 15).
//
// It is both the UI the agent talks to and the state behind it: the agent runs in
// a goroutine and every callback reaches the screen through shell.post, so all
// widget access happens on the toolkit goroutine. State shared with the agent
// goroutine is guarded by one mutex - this session's.
//
// The transcript model is ui/blocks.go: same blocks, same questions, same answer
// semantics, same notes. Only the key names in the "how to leave" notes speak
// desktop (esc for ctrl-c).
type GUI struct {
	// sh is the window this session is drawn in.
	sh *shell
	// item is this session's tab in sh.tabs; the strip has no way to reorder, so
	// the shell keeps sessions and items in the same order (consequence 3).
	item *container.TabItem
	// tabLabel is the session's own name for its tab, when its caller has a better
	// one than the model: a subagent is known by the name its parent gave it.
	tabLabel string
	// needsAttention marks a tab holding a prompt nobody has answered, which the
	// label says with a leading marker because a tab is the only surface a
	// background session has.
	needsAttention bool
	// dir, closer and jobEnd come from GUIOptions: where this session runs, how to
	// stop it, and how to stop one of its runs without stopping the session.
	dir    string
	closer func() error
	jobEnd func(id string) error
	// closeArmed is when × last refused to close a working session; a second press
	// inside the window closes it (see shell.closeTab).
	closeArmed time.Time
	v          *guiView

	cfg        *agent.Config
	loop       *agent.Loop
	modelName  string
	baseURL    string
	configName string
	// contextNote is the resolved context window and its source, from the
	// model client (see model.Sushi.DiscoverContextWindow).
	contextNote string

	// jobs backs the live cards for calls that handed off. See GUIOptions.Jobs.
	jobs JobReader
	// asks serializes prompts so exactly one question is on screen at a time *in
	// this session*. Per-session is the point: one queue for the window would
	// make an unrelated repository's confirmation wait behind the session the
	// human is not even looking at (decision 15). See askQueue and §9.4.
	asks askQueue

	mu       sync.Mutex
	pending  chan Answer
	lastQ    Question
	stream   streamState
	runInfo  map[string]any
	runError error
	runDone  chan struct{}
	// cancelRun stops this session's run (quit, esc).
	cancelRun context.CancelFunc
	// task is the run's task text, shown on one line in place of the rendered
	// instance template.
	task string
	// resume/replay into the transcript at startup (see GUI.Resume);
	// sessionID is the sessions row this run writes, shown in the header.
	resume     []model.Message
	resumeNote string
	sessionID  string
}

// post delivers a closure to the thread that owns the canvas, and it is the only
// way anything here may reach the view: the deferred passes are scheduled from
// time.AfterFunc callbacks, which run on the Go timer goroutine, and from the run
// goroutine. Production is fyne.Do; tests inject a queue drained by the test
// goroutine, because the software driver runs Do inline on the calling goroutine
// and would otherwise let a timer mutate view state beside the canvas pass that is
// measuring it. A method rather than a field so that every existing call site
// stays valid while the function itself belongs to the window.
func (g *GUI) post(fn func()) { g.sh.post(fn) }

// NewGUI builds a session's front end and, when the options carry no window yet,
// the window around it. Cfg and Model fields may be zero on the startup-error
// path; the GUI must still be able to show a message.
func NewGUI(o GUIOptions) *GUI {
	sh := newShell(o)
	// The window's first session is attached on the spot: NewGUI runs on the main
	// goroutine before app.Run, where touching widgets is already legal.
	return sh.attach(newSession(sh, o))
}

// OpenSession adds one more session to this window: the whole of what a second
// repository or a spawned subagent needs is a GUIOptions and a tab. Safe from any
// goroutine, which is what lets `host` call it from a spawn - the state and the
// loop bindings are made here, synchronously, and only the widgets are posted to
// the canvas thread. So the child is attendable the moment its loop can run, and
// its tab appears as soon as the window gets a turn.
func (g *GUI) OpenSession(o GUIOptions) (*GUI, error) { return g.sh.openSession(o) }

func (sh *shell) openSession(o GUIOptions) (*GUI, error) {
	g := newSession(sh, o)
	if err := g.InstallHooks(); err != nil {
		return nil, err
	}
	sh.post(func() { sh.attach(g) })
	return g, nil
}

// newSession is a session's front end without any widgets: the state, and the two
// bindings that must exist before its loop runs anywhere - the streaming handler
// and, for a caller that opens a session through OpenSession, nothing else is
// needed to be attendable.
func newSession(sh *shell, o GUIOptions) *GUI {
	cfg := o.Cfg
	if cfg == nil {
		cfg = &agent.Config{}
	}
	g := &GUI{
		sh:         sh,
		cfg:        cfg,
		loop:       o.Loop,
		configName: o.ConfigName,
		jobs:       o.Jobs,
		tabLabel:   o.TabLabel,
		dir:        o.Dir,
		closer:     o.Closer,
		jobEnd:     o.JobEnd,
		runDone:    make(chan struct{}),
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
	if o.Loop != nil {
		o.Loop.Handler = &model.Handler{Content: g.Content, Reasoning: g.Reasoning}
	}
	return g
}

// attach builds the session's view and its tab. Call on the toolkit goroutine.
func (sh *shell) attach(g *GUI) *GUI {
	g.v = newGUIView(g, sh.window)
	g.item = container.NewTabItem(g.label(), g.v.root())
	sh.sessions = append(sh.sessions, g)
	sh.tabs.Append(g.item)
	return g
}

// InstallHooks makes this session's front end the one its loop asks: the
// confirmation gate, the submit prompt, the message announcements. A method
// rather than a line of construction because its two callers fail differently -
// the CLI reports a bad whitelist and exits, a spawn refuses to run a child nobody
// can answer (decision 13) - and in both cases it must land before the loop runs.
func (g *GUI) InstallHooks() error {
	if g.loop == nil {
		return nil
	}
	h, err := New(g, g.cfg, g.loop)
	if err != nil {
		return err
	}
	g.loop.Hooks = h
	return nil
}

// label is the tab's title: what actually tells two tabs apart. A top-level
// session names its model until the host hands over a working directory and a
// name (§5.15, and increment 3 is what makes a second top-level tab possible);
// a subagent is named by the spawner, and that name is the only thing the human
// has to go on.
func (g *GUI) label() string {
	text := g.tabLabel
	if text == "" {
		// The repository is what tells two top-level tabs apart; the model is
		// already in the transcript header and the footer, and a strip that
		// repeats it tells nobody which window is which checkout.
		if base := filepath.Base(g.dir); base != "" && base != "." && base != string(filepath.Separator) {
			text = base
		} else {
			text = g.modelName
		}
	}
	if text == "" {
		text = "session"
	}
	if g.needsAttention {
		// The strip's labels are text only - TabItem has no slot for a widget and
		// its jump menu cannot carry an icon - so attention has to be a character
		// (decision 15, consequence 2).
		text = "⚠ " + text
	}
	return text
}

// statusLabel is what the tab carries right now: the name, plus how much work this
// session has handed off and cannot finish without a while. Decision 15 kept the
// roster's one real virtue - seeing every session's state at once - by moving it
// into the labels, and a subagent running a twenty-minute job with nothing on
// screen to say so is indistinguishable from one that has gone quiet.
func (g *GUI) statusLabel(liveJobs int) string {
	text := g.label()
	if g.paused() {
		text = "⏸ " + text
	}
	if liveJobs > 0 {
		text += fmt.Sprintf(" \u00b7 %d %s", liveJobs, map[bool]string{true: "job", false: "jobs"}[liveJobs == 1])
	}
	return text
}

// setAttention raises or drops the tab's "somebody is waiting here" marker. It
// runs on the toolkit goroutine, like everything that touches the strip; before a
// session's tab exists (OpenSession posts the attach) it only records the state,
// which attach then renders with the tab itself.
func (g *GUI) setAttention(on bool) {
	if g.needsAttention == on {
		return
	}
	g.needsAttention = on
	if g.v == nil {
		return // no tab yet: attach renders the label with the marker already on it
	}
	g.refreshLabel(g.liveJobs())
}

// Interactive reports whether a human is on the other end. A GUI exists because a
// human opened a window, so the answer is always yes - and it is per session: the
// limits a session may cross are the ones its own tab can be asked about.
func (g *GUI) Interactive() bool { return true }

// Run shows the window, drives this session's loop and blocks in the toolkit event
// loop until the user quits. It must be called on the main goroutine: Fyne's driver
// owns it from app.Run on. The window's event loop is the shell's; this is the
// entry point for the session the CLI handed the task to.
func (g *GUI) Run(ctx context.Context, task string) (map[string]any, error) {
	return g.sh.run(g, ctx, task)
}

// cancelRunTurn stops this session's run in flight, leaving every other session in
// the window alone. Quit calls it for each of them.
func (g *GUI) cancelRunTurn() {
	g.mu.Lock()
	cancel := g.cancelRun
	g.mu.Unlock()
	if g.loopRunning() && cancel != nil {
		cancel()
	}
}

// ShowError presents a setup failure inside the window - the only surface the
// user gets after startup - and runs until the window is closed. It returns
// the exit code.
func (g *GUI) ShowError(err error) int {
	g.v.noteLocal("mini-go " + agent.Version)
	g.v.appendBlock(block{kind: "error", text: "mini: " + err.Error()})
	g.v.appendBlock(block{kind: "note", text: "close the window or type /quit to leave"})
	g.v.start()
	g.sh.app.Run()
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

// note appends UI-generated text - help, mode changes, retry notices - as one card
// per message, on the canvas thread. Splitting it per line would give a card, and
// its header and bubble, to every line of one message.
func (g *GUI) note(text string) {
	if text = strings.TrimRight(text, "\n"); text == "" {
		return
	}
	g.post(func() { g.v.appendBlock(block{kind: "note", text: text}) })
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

// IssueCalls implements Issuer, and it is where a tool card is born: as soon as
// the model's tool calls are known, before any job that would back them exists.
// Each call gets a card in the transcript in the `created` state, because the
// alternative - a bottom panel that appears and vanishes - turned an ordinary
// long-running command into a second UI that could fill the window and cover the
// tab strip.
//
// Why the card exists at all is unchanged: the transcript is derived from
// messages, so a call that runs for two minutes would otherwise show nothing for
// two minutes, and "thinking" and "hung" would look identical to the one human
// watching.
func (g *GUI) IssueCalls(actions []model.Action) {
	cards := make([]block, 0, len(actions))
	for _, a := range actions {
		if a.Display() == "" {
			continue
		}
		cards = append(cards, block{kind: "tool", title: a.Display(), tcid: a.ToolCallID, cmd: a.Command,
			state: "created", transcript: []string{"$ " + a.Command, "issued"}})
	}
	if len(cards) == 0 {
		return
	}
	g.post(func() {
		for _, c := range cards {
			g.v.issueCard(c)
		}
	})
}

// CallStarted implements the agent.CallOpener half: the loop has opened the result
// file for one of the calls this session announced, so that call's card becomes a
// live one and starts following the command.
func (g *GUI) CallStarted(action model.Action, path string) {
	g.post(func() { g.v.markCardRunning(action, path) })
}

// SettleCalls implements Issuer. A call whose result never arrived - a cancelled
// step, a refused batch, a run that ended - is marked as ended rather than left
// claiming that work is happening. A card backed by a live job is left alone: it
// *is* running, and the registry says so.
func (g *GUI) SettleCalls() {
	g.post(func() { g.v.settleCards() })
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
	g.post(func() {
		g.v.showQuestion(q)
		g.setAttention(true)
	})
	select {
	case ans := <-reply:
		return ans, nil
	case <-ctx.Done():
		g.mu.Lock()
		g.pending = nil
		g.mu.Unlock()
		g.post(func() {
			g.v.hideQuestion()
			g.setAttention(false)
		})
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
	g.setAttention(false)
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

// ToggleReasoning and showReasoning reach the window's thinking state: one
// preference applied to every session's transcript, because the toggle rebuilds
// all of them in a single pass anyway (ui/prefs.go keeps it across launches).
func (g *GUI) ToggleReasoning() bool { return g.sh.toggleThinking() }

// showReasoning is the window's thinking state, read while this session's cards
// are being built.
func (g *GUI) showReasoning() bool { return g.sh.showReasoning() }

// setPause holds this session at its next step boundary, or releases it. The turn
// in flight is left to finish - abandoning a command half-written is what esc and
// `Run ▸ Cancel turn` are for, and pause means the opposite: keep everything, take
// no next step. §5.7's remedy beside kill.
func (g *GUI) setPause(on bool) {
	if g.loop == nil {
		return
	}
	if on {
		g.loop.Pause()
		g.v.noteLocal("paused: this session stops at its next step boundary. /resume, or Session ▸ Pause/resume this session, carries on.")
	} else {
		g.loop.Unpause()
		g.v.noteLocal("resumed: this session continues at the step it stopped at.")
	}
	g.refreshLabel(g.liveJobs())
}

// togglePause is the menu's one control for both directions; it reports the state
// the tab now shows.
func (g *GUI) togglePause() bool {
	g.setPause(!g.paused())
	return g.paused()
}

// paused reports a session being held, including the window between asking and
// actually stopping at the boundary - the ⏸ says "will stop", which is what the
// human needs to know either way.
func (g *GUI) paused() bool { return g.loop != nil && g.loop.Paused() }

// export writes this session's trajectory where the human asked, and says so in the
// tab. Export is an act the human takes: nothing writes a transcript file because a
// session ran, which is why the document is rebuilt from the store rather than from
// a per-step file, and why a subagent's tab can export itself as easily as its
// parent's.
func (g *GUI) export(path string) {
	path = strings.TrimSpace(path)
	if g.sh.exporter == nil {
		g.v.noteLocal("this window cannot export a trajectory")
		return
	}
	if g.sessionID == "" {
		g.v.noteLocal("this session has no stored id to export yet")
		return
	}
	written, err := g.sh.exporter(g.sessionID, path)
	if err != nil {
		g.v.noteLocal("could not export this session: " + err.Error())
		return
	}
	g.v.noteLocal("exported this session's trajectory to " + written)
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
	if g.paused() {
		// The pause belongs next to the mode: both describe what this session will
		// do on its own, and neither is visible in the token counts around them.
		text += " \u00b7 paused"
	}
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
