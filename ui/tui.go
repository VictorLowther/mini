package ui

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/model"
)

// TUIOptions wires the terminal UI to a configured loop.
type TUIOptions struct {
	Cfg        *agent.Config
	Loop       *agent.Loop
	Model      *model.Sushi
	ConfigName string
	// Width/Height override the terminal size (tests).
	Width, Height int
	// LogWriter is where the process logger writes. The pane mutes it while it
	// owns the terminal; see LogTarget.
	LogWriter *LogTarget
}

// TUI is the bubbletea front end. It is both the UI the agent talks to and the
// owner of the program: the agent loop runs in one goroutine and every callback
// reaches the screen through program.Send, so there is exactly one producer and
// no per-chunk command plumbing.
type TUI struct {
	cfg        *agent.Config
	loop       *agent.Loop
	modelName  string
	baseURL    string
	configName string

	program *tea.Program
	// printer delivers committed transcript lines above the pane.
	printer *printer
	// logWriter is muted for the duration of Run (nil in tests).
	logWriter *LogTarget
	runCtx    context.Context

	mu       sync.Mutex
	pending  chan Answer
	lastQ    Question
	stream   streamState
	blocks   []block
	width    int
	height   int
	runInfo  map[string]any
	runError error
	runDone  chan struct{}
	finished chan struct{}
	// fallback delivers messages when no program is running (tests drive the
	// model directly).
	fallback   func(tea.Msg)
	programErr error
	// reasoningVisible is toggled with `t`; hidden by default so the useful
	// text stays on screen.
	reasoningVisible bool
	// task is the run's task text, shown on one line in place of the rendered
	// instance template.
	task string
	// resume/replay into the pane at startup (see TUI.Resume); sessionID is
	// the sessions row this run writes, shown in the header.
	resume     []model.Message
	resumeNote string
	sessionID  string
	// loopGID is the goroutine bubbletea runs Update on, captured in Init. The
	// one hard rule of this UI is that Send must never be called from it:
	// program.Send is an unbuffered hand-off, so doing so deadlocks the pane
	// permanently (the symptom users report as "input focus is gone forever").
	// In-loop code appends to the scrollback directly instead.
	loopGID atomic.Uint64
	onLoop  atomic.Bool
	// loopMu serialises delivery in tests that drive the model without a
	// program, reproducing bubbletea's single-update-goroutine guarantee.
	loopMu sync.Mutex
}

// goid identifies the current goroutine. It is only consulted while the update
// loop is executing, so the cost is irrelevant.
func goid() uint64 {
	var b [32]byte
	n := runtime.Stack(b[:], false)
	if n < len("goroutine ") {
		return 0
	}
	var id uint64
	for _, c := range b[len("goroutine "):n] {
		if c == ' ' {
			break
		}
		id = id*10 + uint64(c-'0')
	}
	return id
}

// kick delivers a message to the model the way the event loop would (tests
// only). Commands returned by Update are deliberately NOT executed: widget
// commands (cursor focus, blink) block until a real program consumes them.
// Tests that need a repaint get it from scheduleRepaint's synchronous path.
func (t *TUI) kick(m *tuiModel, msg tea.Msg) {
	t.loopMu.Lock()
	defer t.loopMu.Unlock()
	m.Update(msg)
}

// doOn runs fn with the update loop quiescent, so tests can poke at the model
// without racing the fallback delivery.
func (t *TUI) doOn(fn func()) {
	t.loopMu.Lock()
	defer t.loopMu.Unlock()
	fn()
}

func (t *TUI) setInLoop(v bool) {
	if v {
		t.loopGID.CompareAndSwap(0, goid())
		t.onLoop.Store(true)
		return
	}
	t.onLoop.Store(false)
}

type streamState struct {
	reasoning strings.Builder
	content   strings.Builder
}

// block is one entry in the transcript. kind is a message role, or one of
// "note" / "error" for UI-generated lines. Blocks keep their source text so the
// pane can be re-rendered when the thinking toggle flips.
type block struct {
	kind  string
	title string
	text  string
	// reasoning is the model's thinking text for this turn, shown only while
	// the thinking toggle is on.
	reasoning string
	// transcript holds the rendered shell lines of an executed command.
	transcript []string
}

// NewTUI builds the UI and installs itself as the loop's streaming handler.
func NewTUI(o TUIOptions) (*TUI, error) {
	t := &TUI{
		cfg: o.Cfg, loop: o.Loop,
		modelName: o.Model.Model, baseURL: o.Model.BaseURL, configName: o.ConfigName,
		logWriter: o.LogWriter,
		width:     orDefault(o.Width, 80),
		height:    orDefault(o.Height, 24),
		runDone:   make(chan struct{}),
		finished:  make(chan struct{}),
	}
	if o.Loop != nil {
		o.Loop.Handler = &model.Handler{Content: t.Content, Reasoning: t.Reasoning}
	}
	return t, nil
}

func orDefault(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// send hands a message to the running program. Before Run (and in tests that
// drive the model directly) there is no program, and the update loop is fed by
// the test instead.
func (t *TUI) send(msg tea.Msg) {
	if t.program != nil {
		if t.onLoop.Load() && goid() == t.loopGID.Load() {
			panic("TUI.send called from the update loop: program.Send would deadlock the pane; use tuiModel.noteLocal")
		}
		t.program.Send(msg)
		return
	}
	if t.fallback != nil {
		// The test fallback owns the loop lock (it delivers through
		// t.deliver); locking here would deadlock.
		t.fallback(msg)
	}
}

// Interactive: the TUI always has a human on the other end.
func (t *TUI) Interactive() bool { return true }

// Run starts the program and the agent, returning when the user quits or the
// run ends. A non-empty task starts the loop immediately; an empty one asks the
// user first.
func (t *TUI) Run(ctx context.Context, task string) (map[string]any, error) {
	// Nothing may write to this terminal behind the renderer's back: a log line
	// that scrolls the screen moves the footer and leaves stale rows under it.
	undo := t.logWriter.Mute()
	defer undo()

	runCtx, cancelRun := context.WithCancel(ctx)
	t.runCtx = runCtx
	m := newTUIModel(t, task, cancelRun)
	t.program = tea.NewProgram(m)
	// Finished transcript lines are printed above the pane by this goroutine;
	// see printer.go for why they cannot be printed from Update.
	t.printer = newPrinterWith(
		func(body string) { t.program.Printf("%s", body) },
		func() { t.program.Send(tea.ClearScreen()) })

	go func() {
		_, err := t.program.Run()
		t.mu.Lock()
		t.programErr = err
		t.mu.Unlock()
		cancelRun()
		close(t.finished)
	}()
	go t.drive(runCtx, task)

	select {
	case <-t.finished:
	case <-ctx.Done():
	}
	// Give the loop a moment to finish saving its trajectory.
	select {
	case <-t.runDone:
	case <-time.After(2 * time.Second):
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	info := t.runInfo
	err := t.runError
	if err != nil && ctx.Err() == nil && t.programErr != nil {
		err = t.programErr
	}
	return info, err
}

// drive asks for a task when needed, then runs the loop.
func (t *TUI) drive(ctx context.Context, task string) {
	defer close(t.runDone)
	if task == "" {
		ans, err := t.Ask(ctx, Question{Kind: AskTask, Title: TaskPrompt, Cfg: t.cfg})
		if err != nil {
			t.finish(nil, err)
			return
		}
		task = ans.Text
	}
	t.mu.Lock()
	t.task = task
	t.mu.Unlock()
	info, err := t.loop.Run(ctx, task)
	t.finish(info, err)
}

func (t *TUI) finish(info map[string]any, err error) {
	t.mu.Lock()
	t.runInfo, t.runError = info, err
	t.mu.Unlock()
	if t.program != nil {
		t.send(runDoneMsg{})
	}
}

var _ UI = (*TUI)(nil)

// ---------------------------------------------------------------- streaming --

func (t *TUI) Reasoning(text string) {
	t.mu.Lock()
	t.stream.reasoning.WriteString(text)
	t.mu.Unlock()
	t.send(streamMsg{kind: "reasoning", text: text})
}

func (t *TUI) Content(text string) {
	t.mu.Lock()
	t.stream.content.WriteString(text)
	t.mu.Unlock()
	t.send(streamMsg{kind: "content", text: text})
}

// StreamText returns the text streamed so far for the turn in flight.
func (t *TUI) StreamText() (reasoning, content string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stream.reasoning.String(), t.stream.content.String()
}

// Message records a finished message in the transcript.
//
// The system message and the rendered instance template are skipped: they are
// prompt plumbing, they are enormous, and at the start of a run they bury
// everything the user cares about. The task itself is shown on one line
// instead. Only messages the agent produced (every one of them carries an
// "extra" map) reach the pane.
func (t *TUI) Message(m model.Message) {
	b, ok := t.blockFor(m)
	if !ok {
		return
	}
	t.send(blockMsg{b})
}

// blockFor turns one message into its transcript block, and says when the
// message is prompt plumbing that never reaches the pane. TUI.Message and the
// resume replay share it, so a resumed pane cannot drift from a live one.
func (t *TUI) blockFor(m model.Message) (block, bool) {
	role, _ := m["role"].(string)
	content, _ := m["content"].(string)
	if _, built := m["extra"]; !built {
		if role == "user" {
			t.mu.Lock()
			task := t.task
			t.mu.Unlock()
			t.note("task: " + firstLine(task))
		}
		return block{}, false
	}
	b := block{kind: role, text: content}
	if r, ok := m["reasoning_content"].(string); ok {
		b.reasoning = r
	}
	switch role {
	case "assistant":
		b.title = t.AssistantHeader()
		// The live block held the streamed text; this message supersedes it, so
		// the buffer starts fresh for the next turn instead of repeating.
		t.mu.Lock()
		t.stream = streamState{}
		t.mu.Unlock()
	case "tool", "user":
		if obs, ok := observationOf(m); ok {
			b.transcript = obs.lines()
		}
	case "exit":
		extra, _ := m["extra"].(map[string]any)
		status, _ := extra["exit_status"].(string)
		b.title = status
	}
	return b, true
}

// Resume queues a stored transcript for replay into the pane, with a note
// naming where it came from. Empty msgs and note (a fresh run) replay nothing.
// Call before the program starts.
func (t *TUI) Resume(msgs []model.Message, note string) {
	t.resume, t.resumeNote = msgs, note
}

// SetSessionID labels the pane header with the session this run writes to.
func (t *TUI) SetSessionID(id string) { t.sessionID = id }

func (t *TUI) showReasoning() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reasoningVisible
}

// note appends a UI-generated line (help text, mode changes, errors).
func (t *TUI) note(text string) {
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line != "" {
			t.send(blockMsg{block{kind: "note", text: line}})
		}
	}
}

// queueSteering appends a user message to the loop's steering queue; the loop
// commits it to the history before its next model call.
func (t *TUI) queueSteering(text string) {
	if t.loop == nil {
		return
	}
	t.loop.QueueMessage(model.Message{
		"role":    "user",
		"content": text,
		"extra":   map[string]any{"interrupt_type": "UserSteering"},
	})
}

// RetryNotice tells the pane that a model call is being retried.
func (t *TUI) RetryNotice(attempt, max int, err error, wait time.Duration) {
	t.note(fmt.Sprintf("model call failed (attempt %d/%d), retrying in %s: %v", attempt, max, wait.Round(time.Second), err))
}

// Write lets ui.slash() print into the transcript.
func (t *TUI) Write(p []byte) (int, error) {
	t.note(string(p))
	return len(p), nil
}

// ------------------------------------------------------------ the ask loop ---

// Ask shows a question and blocks until the keyboard answers it.
func (t *TUI) Ask(ctx context.Context, q Question) (Answer, error) {
	reply := make(chan Answer, 1)
	t.mu.Lock()
	t.pending = reply
	t.lastQ = q
	t.mu.Unlock()
	t.send(questionMsg{q})
	select {
	case ans := <-reply:
		return ans, nil
	case <-ctx.Done():
		t.mu.Lock()
		t.pending = nil
		t.mu.Unlock()
		return Answer{}, ctx.Err()
	}
}

// resolve answers the pending question from the update loop.
func (t *TUI) resolve(a Answer) {
	t.mu.Lock()
	ch := t.pending
	t.pending = nil
	t.mu.Unlock()
	if ch != nil {
		ch <- a
	}
}

// HasQuestion reports whether a prompt is waiting for an answer.
func (t *TUI) HasQuestion() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pending != nil
}

// PendingQuestion returns the question being answered.
func (t *TUI) PendingQuestion() Question {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastQ
}

// ToggleReasoning flips the collapsed state of thinking blocks (t).
func (t *TUI) ToggleReasoning() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reasoningVisible = !t.reasoningVisible
	return t.reasoningVisible
}

// loopRunning reports whether the agent is inside Loop.Run.
func (t *TUI) loopRunning() bool { return t.loop != nil && t.loop.Running() }

// result returns how the run ended.
func (t *TUI) result() (map[string]any, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.runInfo, t.runError
}

// AssistantHeader is the title an assistant block carries, matching the
// reference's add_messages banner. The streaming preview uses it too, so a turn
// does not gain a header when it commits.
func (t *TUI) AssistantHeader() string {
	snap := t.snapshot()
	return fmt.Sprintf("mini-swe-agent (step %d, $%.2f)", snap.Calls, snap.Cost)
}

// CancelCurrentTurn interrupts the command in flight (esc).
func (t *TUI) CancelCurrentTurn() { t.loop.CancelTurn() }

// RunStats renders the footer: step, cost, tokens, throughput, mode.
func (t *TUI) RunStats() string {
	snap := t.snapshot()
	step, cost := snap.Calls, snap.Cost
	parts := []string{fmt.Sprintf("step %d", step), fmt.Sprintf("$%.2f", cost), t.cfg.Mode,
		map[bool]string{true: "thinking: on", false: "thinking: off"}[t.showReasoning()]}
	if snap.ModelTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d tok", snap.ModelTokens))
	}
	if snap.ModelTokPerSec > 0 {
		parts = append(parts, fmt.Sprintf("%.1f tok/s", snap.ModelTokPerSec))
	}
	return strings.Join(parts, "  ·  ")
}

func (t *TUI) snapshot() agent.Snapshot {
	if t.loop == nil {
		return agent.Snapshot{}
	}
	return t.loop.Snapshot()
}

// ---------------------------------------------------------------- rendering --

// renderCache memoises one glamour renderer per wrap width.
type renderCache struct {
	mu  sync.Mutex
	at  map[int]*glamour.TermRenderer
	err error
}

func (r *renderCache) markdown(s string, width int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.at == nil {
		r.at = map[int]*glamour.TermRenderer{}
	}
	t, ok := r.at[width]
	if !ok {
		t, r.err = glamour.NewTermRenderer(
			glamour.WithStyles(styles.DarkStyleConfig),
			glamour.WithWordWrap(width),
			glamour.WithPreservedNewLines(),
		)
		if r.err != nil {
			r.at[width] = nil
			return s
		}
		r.at[width] = t
	}
	if t == nil {
		return s
	}
	out, err := t.Render(s)
	if err != nil {
		return s
	}
	return strings.TrimRight(out, "\n")
}

var markdownCache renderCache

// tuiStyles holds the fixed colours of the chrome.
type tuiStyles struct {
	title  lipgloss.Style
	dim    lipgloss.Style
	error  lipgloss.Style
	prompt lipgloss.Style
	panel  lipgloss.Style
	ok     lipgloss.Style
	status lipgloss.Style
	badge  lipgloss.Style
}

var tuiStyler = tuiStyles{
	title:  lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
	dim:    lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
	error:  lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
	prompt: lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
	panel:  lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("11")).Padding(0, 1),
	ok:     lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
	status: lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
	badge:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11")),
}

// renderBlock turns one transcript entry into styled text. showThinking decides
// whether a turn's reasoning text is part of it, so toggling re-renders cleanly.
//
// Everything here is wrapped to the pane width. The frame's height is measured
// from these rows, so a line the terminal wraps instead of us makes the pane one
// row taller than the measurement: the top row scrolls off the screen and the
// footer finishes the run above the bottom of the terminal.
func renderBlock(b block, width int, showThinking bool) string {
	st := tuiStyler
	thinking := ""
	if b.reasoning != "" {
		if showThinking {
			// Thinking wraps at word boundaries; it is prose, and the model's
			// sentences are the thing being read.
			wrapped := ansi.Wordwrap(collapseSpaces(b.reasoning), max(width-4, 20), " ")
			thinking = st.dim.Render("thinking:\n"+hangingIndent(wrapped, "  ")) + "\n"
		} else {
			thinking = st.dim.Render(fmt.Sprintf("[thinking hidden: %d chars · ctrl+t]", len(b.reasoning))) + "\n"
		}
	}
	// An executed action reads like a shell transcript: command, directory,
	// exit status, output. Output keeps its line structure but is broken to fit,
	// so a single long path cannot rewrap the pane.
	if len(b.transcript) > 0 {
		return thinking + st.dim.Render(strings.Join(fitLines(b.transcript, width), "\n"))
	}
	switch b.kind {
	case "assistant":
		head := ""
		if b.title != "" {
			head = st.title.MaxWidth(width).Render(b.title) + "\n"
		}
		return head + thinking + markdownCache.markdown(b.text, width)
	case "user", "system":
		return thinking + markdownCache.markdown(b.text, width)
	case "exit":
		return st.ok.MaxWidth(width).Render("✓ "+b.title) + "\n" +
			st.dim.MaxWidth(width).Render(b.text)
	// Every rendered row must be no wider than the pane: the frame's height is
	// measured from these strings, and a row the terminal wraps for us adds a row
	// the measurement never saw - the pane then overflows and the footer drifts.
	case "error":
		return st.error.MaxWidth(width).Render(strings.Join(fitLines(strings.Split(b.text, "\n"), width), "\n"))
	default: // notes: UI feedback, the session header, command results
		return st.dim.MaxWidth(width).Render(strings.Join(fitLines(strings.Split(b.text, "\n"), width), "\n"))
	}
}

// fitLines breaks any line longer than the pane, keeping existing line breaks
// and indenting continuations under the line they belong to, so a long path in
// the transcript stays readable instead of soft-wrapping at the terminal edge
// (which shifts every line below it).
func fitLines(lines []string, width int) []string {
	if width < 20 {
		return lines
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if ansi.StringWidth(l) <= width {
			out = append(out, l)
			continue
		}
		pad := strings.Repeat(" ", len(l)-len(strings.TrimLeft(l, " ")))
		parts := strings.Split(ansi.Wrap(strings.TrimLeft(l, " "), width, ""), "\n")
		out = append(out, pad+parts[0])
		for _, cont := range parts[1:] {
			if cont == "" {
				continue
			}
			out = append(out, pad+cont)
		}
	}
	return out
}

// hangingIndent aligns continuation lines under the first line's text.
func hangingIndent(s, pad string) string {
	return strings.ReplaceAll(s, "\n", "\n"+pad)
}

// collapseSpaces makes running text safe to wrap: model output arrives with
// soft line breaks sprinkled through it, and re-wrapping hard newlines
// mid-sentence is what made the thinking block ragged.
func collapseSpaces(s string) string {
	lines := strings.Split(strings.Trim(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.Join(lines, "\n")
}

// truncateVisible keeps long command output from flooding the pane.
func truncateVisible(s string, maxLines int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n") +
		fmt.Sprintf("\n… %d more lines", len(lines)-maxLines)
}
