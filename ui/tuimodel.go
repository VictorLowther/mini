package ui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/VictorLowther/mini/agent"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Messages sent to the program from the agent goroutine.
type (
	streamMsg   struct{ kind, text string }
	blockMsg    struct{ b block }
	questionMsg struct{ q Question }
	runDoneMsg  struct{}
	errMsg      struct{ err error }
	// repaintMsg flushes a dirty scrollback. Streaming sets dirty instead of
	// re-rendering synchronously: sushi emits a chunk every few milliseconds,
	// and rebuilding the whole pane per chunk starves the update loop (the
	// terminal sees a repaint storm and the UI stops answering keys).
	repaintMsg struct{}
)

// repaintInterval bounds how often the scrollback is rebuilt while streaming.
const repaintInterval = 80 * time.Millisecond

// tuiKeyMap is the binding registry; dispatch happens in one handleKey.
type tuiKeyMap struct {
	Send           key.Binding
	Newline        key.Binding
	Quit           key.Binding
	Cancel         key.Binding
	Confirm        key.Binding
	ToggleThinking key.Binding
	Help           key.Binding
}

func newKeyMap() tuiKeyMap {
	return tuiKeyMap{
		Send:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "send")),
		Newline: key.NewBinding(key.WithKeys("ctrl+j"), key.WithHelp("ctrl+j", "newline")),
		// ctrl+c only: a bare "q" or "t" would steal the first letter of any
		// prose typed at an empty prompt.
		Quit:           key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "cancel/quit")),
		Cancel:         key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel turn")),
		Confirm:        key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "confirm")),
		ToggleThinking: key.NewBinding(key.WithKeys("ctrl+t"), key.WithHelp("ctrl+t", "thinking")),
		Help:           key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	}
}

func (k tuiKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Send, k.Cancel, k.Quit, k.Help}
}

func (k tuiKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Send, k.Newline, k.Cancel, k.Quit, k.ToggleThinking}}
}

// tuiModel is the bubbletea model. It owns no agent state: everything it shows
// comes from the TUI it is attached to.
type tuiModel struct {
	t    *TUI
	task string

	editor textarea.Model
	keymap tuiKeyMap
	help   help.Model

	width, height int
	ready         bool

	// blocks is the transcript source of truth; rows holds every *finished*
	// block rendered to lines. Rows scroll out of the fixed-height frame into
	// the terminal's own scrollback (see syncWindow), and printed counts the rows
	// that already went. The in-flight turn is rendered on demand and never lives
	// in rows, so streaming cannot disturb the printed history.
	blocks  []block
	rows    []string
	printed int
	// rowCounts is how many rows each finalized block contributed, so the print
	// frontier can be aligned to block boundaries and the unprinted tail re-wrapped
	// when the terminal is resized.
	rowCounts   []int
	printedMark int // block index the printed prefix ends at
	// live is the index of the in-flight turn's block, or -1. Streaming updates
	// that block in place, so a turn is rendered once, by one code path, whether
	// it is halfway through or committed. (A separate streaming overlay made the
	// pane show bare output while a turn streamed and then show it again with its
	// header when the message landed.)
	live       int // index into blocks/lines of the in-flight turn, or -1
	dirty      bool
	refreshing bool

	// dialog is the modal on top of everything else
	dialog    string // "", "quit", "limits"
	limitsIdx int
	limits    [2]float64

	showFullHelp bool
	multiline    bool

	cancelRun context.CancelFunc
}

func newTUIModel(t *TUI, task string, cancelRun context.CancelFunc) *tuiModel {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.CharLimit = -1
	ta.DynamicHeight = true
	ta.MinHeight = 1
	ta.MaxHeight = 5
	ta.Prompt = "│ "
	ta.Placeholder = "message · enter sends · \\ + enter or /m for multi-line · /h commands"
	ta.SetWidth(t.width - 2)
	ta.Focus()
	m := &tuiModel{
		t: t, task: task, live: -1, editor: ta,
		keymap: newKeyMap(), help: help.New(),
		width: t.width, height: t.height,
		cancelRun: cancelRun,
	}
	// First thing in the transcript: like a shell's banner, it scrolls away with
	// the history instead of occupying a row of the pane - the frame's top rows
	// are where printing above the pane lands.
	m.sessionHeader()
	return m
}

// Init runs on bubbletea's update goroutine, which is the identity the send
// guard needs.
func (m *tuiModel) Init() tea.Cmd {
	m.t.setInLoop(true)
	m.t.setInLoop(false)
	return nil
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.t.setInLoop(true)
	defer m.t.setInLoop(false)
	// Any message can change the chrome (a panel opening, a draft growing a
	// line), which changes how many transcript rows fit - so the window is
	// reconciled on every pass, not only where a block was committed.
	defer m.syncWindow()
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.t.mu.Lock()
		m.t.width, m.t.height = msg.Width, msg.Height
		m.t.mu.Unlock()
		m.editor.SetWidth(msg.Width - 2)
		m.help.SetWidth(msg.Width)
		m.ready = true
		m.rewrap()
		return m, nil

	case streamMsg:
		m.updateLive()
		return m, m.scheduleRepaint()

	case blockMsg:
		m.commit(msg.b)
		return m, nil

	case repaintMsg:
		m.refreshing = false
		if m.dirty {
			m.flushRepaint()
		}
		return m, nil

	case tea.FocusMsg:
		// Input must always work: the editor is the only focusable widget.
		m.editor.Focus()
		return m, nil

	case tea.BlurMsg:
		m.editor.Focus()
		return m, nil

	case questionMsg:
		m.dialog = ""
		m.multiline = false
		if msg.q.Kind == AskLimits {
			// Two numeric answers in a row: step limit, then cost limit.
			m.openDialog("limits")
		}
		m.editor.Focus()
		return m, nil

	case runDoneMsg:
		// A clean finish leaves immediately: the final frame stays in the
		// terminal history (no alternate screen), so nothing is lost. A failed
		// run keeps the pane open so the error stays readable and the user
		// decides when to leave.
		if m.finishRun() {
			return m, m.drainAndQuit()
		}
		return m, nil

	case errMsg:
		m.appendBlock(block{kind: "error", text: msg.err.Error()})
		return m, nil

	case tea.KeyPressMsg:
		return m, m.handleKey(msg)

	default:
		// Let the textarea handle anything else (paste, mouse, ...).
		var cmd tea.Cmd
		m.editor, cmd = m.editor.Update(msg)
		return m, cmd
	}
}

// handleKey dispatches one key press.
func (m *tuiModel) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	// Modal dialogs intercept first.
	switch m.dialog {
	case "quit":
		return m.handleQuitDialog(msg)
	case "limits":
		return m.handleLimitsDialog(msg)
	}
	if m.t.HasQuestion() && m.pendingKind() == AskConfirm && m.editor.Value() == "" {
		// Enter or y confirms; anything typed becomes the rejection comment.
		if key.Matches(msg, m.keymap.Send) || key.Matches(msg, m.keymap.Confirm) {
			m.answerConfirm("")
			return nil
		}
	}
	switch {
	case key.Matches(msg, m.keymap.Cancel):
		if m.t.loopRunning() {
			m.t.CancelCurrentTurn()
			m.noteLocal("cancelling this turn…")
			return nil
		}
		m.openDialog("quit")
		return nil
	case key.Matches(msg, m.keymap.Quit) && m.editor.Value() == "":
		m.openDialog("quit")
		return nil
	case key.Matches(msg, m.keymap.Newline):
		m.editor.InsertString("\n")
		return nil
	case key.Matches(msg, m.keymap.ToggleThinking):
		m.toggleThinking()
		return nil
	case key.Matches(msg, m.keymap.Help) && m.editor.Value() == "":
		m.showFullHelp = !m.showFullHelp
		return nil
	case key.Matches(msg, m.keymap.Send):
		return m.send()
	}
	var cmd tea.Cmd
	m.editor, cmd = m.editor.Update(msg)
	return cmd
}

// send submits the editor contents, honouring the trailing-backslash and
// multiline conventions.
func (m *tuiModel) send() tea.Cmd {
	value := m.editor.Value()
	// Enter inserts a newline while in multi-line mode (/m); a line containing
	// only "." ends the message.
	if m.multiline && !strings.HasSuffix(value, "\n.") {
		m.editor.InsertString("\n")
		return nil
	}
	if !m.multiline && strings.HasSuffix(value, `\`) {
		m.editor.SetValue(strings.TrimSuffix(value, `\`))
		m.editor.InsertString("\n")
		return nil
	}
	value = strings.TrimSuffix(value, "\n.")
	m.editor.Reset()
	m.multiline = false
	return m.handleInput(strings.TrimSpace(value))
}

// handleInput routes a submitted line to the pending question, or to a new turn.
func (m *tuiModel) handleInput(text string) tea.Cmd {
	q := m.t.PendingQuestion()
	if !m.t.HasQuestion() {
		// Input is never dropped: slash commands apply at once (they change the
		// mode the next prompt will use) and prose is queued as steering, which
		// lands in the history before the model's next call.
		if text == "" {
			if !m.t.loopRunning() {
				m.noteLocal("nothing is waiting for input")
			}
			return nil
		}
		if handled := m.commandOrSteer(text); handled {
			return nil
		}
		if !m.t.loopRunning() {
			m.noteLocal("the run has ended: press esc then y to leave")
			return nil
		}
		m.t.queueSteering(text)
		m.appendBlock(block{kind: "user", text: text})
		m.noteLocal("(queued for the model's next turn)")
		return nil
	}
	switch q.Kind {
	case AskTask:
		if text == "" {
			m.noteLocal("please describe the task")
			return nil
		}
		m.t.resolve(Answer{Text: text})
	case AskLimits:
		m.openDialog("limits")
	case AskConfirm:
		// Empty input confirms; anything else rejects, after slash handling.
		resolved, reprompt := m.slash(text)
		if reprompt {
			return nil
		}
		// Python's match: "" and "/y" both confirm the batch.
		if resolved == "" || resolved == "/y" {
			m.t.resolve(Answer{})
			return nil
		}
		m.t.resolve(Answer{Reject: true, Text: resolved})
	case AskInterrup:
		m.t.resolve(Answer{Text: text})
	default: // new-task, human, interruption
		if m.commandOrSteer(text) {
			return nil
		}
		resolved, reprompt := m.slash(text)
		if reprompt {
			return nil
		}
		if resolved == "/m" {
			m.multiline = true
			m.noteLocal(`multi-line mode: enter adds a newline; send "." on its own line to finish`)
			return nil
		}
		m.t.resolve(Answer{Text: resolved})
	}
	return nil
}

// slash applies the /y /c /u /h commands, printing their feedback into the
// scrollback. It returns the text to act on and whether the question was
// consumed by a re-prompt.
func (m *tuiModel) slash(text string) (string, bool) {
	out, reprompt := slash(m.t.cfg, text, modelWriter{m}, func() string {
		m.multiline = true
		return text
	})
	return out, reprompt
}

func (m *tuiModel) answerConfirm(text string) { m.t.resolve(Answer{Text: text}) }

// toggleThinking expands or collapses the model's reasoning text.
func (m *tuiModel) toggleThinking() {
	on := m.t.ToggleReasoning()
	// Every block renders differently now, and the printed history cannot be
	// edited in place, so replace it: clear the screen and write it again.
	m.renderAll()
	m.noteLocal(map[bool]string{true: "thinking shown", false: "thinking hidden"}[on])
}

// commandOrSteer handles the input affordances that work whether or not a
// prompt is pending: /y /c /u /h /t and /m.
func (m *tuiModel) commandOrSteer(text string) bool {
	switch text {
	case "/t":
		m.toggleThinking()
		return true
	case "/m":
		m.multiline = true
		m.editor.SetValue("")
		m.noteLocal(`multi-line mode: enter adds a newline; send "." on its own line to finish`)
		return true
	}
	if _, isMode := modeCommands[text]; !isMode && text != "/h" {
		return false
	}
	_, reprompt := m.slash(text)
	_ = reprompt
	return true
}

func (m *tuiModel) pendingKind() string { return m.t.PendingQuestion().Kind }

func (m *tuiModel) openDialog(name string) {
	m.dialog = name
	m.limitsIdx = 0
	m.limits = [2]float64{}
	m.editor.SetValue("")
	m.editor.Focus()
}

func (m *tuiModel) handleQuitDialog(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, m.keymap.Confirm):
		return m.quit()
	case key.Matches(msg, m.keymap.Cancel), key.Matches(msg, m.keymap.Quit):
		m.dialog = ""
		m.editor.Focus()
		return nil
	case key.Matches(msg, m.keymap.Send):
		return m.quit()
	}
	return nil
}

func (m *tuiModel) handleLimitsDialog(msg tea.Msg) tea.Cmd {
	if kp, ok := msg.(tea.KeyPressMsg); ok {
		switch {
		case key.Matches(kp, m.keymap.Cancel):
			m.dialog = ""
			return nil
		}
		if !key.Matches(kp, m.keymap.Send) {
			var cmd tea.Cmd
			m.editor, cmd = m.editor.Update(kp)
			return cmd
		}
		// Read before forwarding: the textarea's own enter handling would
		// otherwise clear the field first.
		value := m.editor.Value()
		m.editor.Reset()
		v := ParseLimitLine(value)
		m.limits[m.limitsIdx] = v
		if m.limitsIdx == 0 {
			m.limitsIdx = 1
			return nil
		}
		m.dialog = ""
		m.t.resolve(Answer{NewLimits: true, Steps: int(m.limits[0]), Cost: m.limits[1]})
		return nil
	}
	return nil
}

func (m *tuiModel) quit() tea.Cmd {
	if m.t.loopRunning() {
		m.cancelRun()
	}
	return m.drainAndQuit()
}

// drainAndQuit lets queued transcript lines land before the loop stops reading,
// since printing after that has no reader. Runs as a command, off the loop.
func (m *tuiModel) drainAndQuit() tea.Cmd {
	return func() tea.Msg {
		m.t.printer.flush(300 * time.Millisecond)
		return tea.Quit()
	}
}

// finishRun records the terminal state and reports whether the pane may close.
func (m *tuiModel) finishRun() (quit bool) {
	info, err := m.t.result()
	status, _ := info["exit_status"].(string)
	switch {
	case err != nil && status == "":
		m.appendBlock(block{kind: "error", text: "run ended: " + err.Error()})
		m.appendBlock(block{kind: "note", text: "press esc then y to quit"})
		return false
	default:
		if status == "" {
			status = "stopped"
		}
		m.appendBlock(block{kind: "note", text: "run finished: " + status})
		return true
	}
}

// ------------------------------------------------------------- scrollback ---

// noteLocal appends UI feedback straight into the scrollback. Code running on
// the update loop MUST use this instead of TUI.note: bubbletea's Send is an
// unbuffered channel hand-off, so sending from inside Update deadlocks the
// program forever (the pane renders but never reads another key).
// sessionHeader is the line a shell prints before its first prompt: session info
// once, into the transcript, where it can scroll away with the history.
func (m *tuiModel) sessionHeader() {
	m.noteLocal(tuiStyler.dim.Render(fmt.Sprintf("mini-go %s · %s @ %s · config %s",
		agent.Version, m.t.modelName, m.t.baseURL, m.t.configName)))
}

func (m *tuiModel) noteLocal(text string) {
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line != "" {
			m.appendBlock(block{kind: "note", text: line})
		}
	}
}

// modelWriter lets the shared slash() helper print into the pane from the
// update loop.
type modelWriter struct{ m *tuiModel }

func (w modelWriter) Write(p []byte) (int, error) {
	w.m.noteLocal(string(p))
	return len(p), nil
}

// appendBlock adds a block with no streaming predecessor: notes, tool results,
// UI feedback.
func (m *tuiModel) appendBlock(b block) { m.commit(b) }

// commit places a finished block in the transcript, replacing the in-flight
// turn's block when it is the committed form of that same turn.
func (m *tuiModel) commit(b block) {
	switch {
	case b.kind == "assistant" && m.live >= 0:
		m.blocks[m.live] = b
		m.live = -1
	default:
		// Something else ended the turn. Keep whatever streamed as its own entry
		// rather than letting a later commit overwrite rows that may already have
		// been printed.
		if m.live >= 0 {
			m.finalize(m.blocks[m.live])
			m.live = -1
		}
		m.blocks = append(m.blocks, b)
	}
	m.finalize(m.blocks[len(m.blocks)-1])
	m.syncWindow()
}

// finalize appends one block's rows to the transcript. Committed rows never
// change again, which is what makes printing them exactly once safe.
func (m *tuiModel) finalize(b block) {
	rows := blockRows(b, m.width-2, m.t.showReasoning())
	m.rows = append(m.rows, rows...)
	m.rowCounts = append(m.rowCounts, len(rows))
}

// blockRows renders a block into the rows the terminal will hold, plus the blank
// row that separates it from the next one.
func blockRows(b block, width int, showThinking bool) []string {
	return append(strings.Split(renderBlock(b, width, showThinking), "\n"), "")
}

// liveRows renders the turn in flight: the only transcript text that changes.
func (m *tuiModel) liveRows() []string {
	if m.live < 0 {
		return nil
	}
	return blockRows(m.blocks[m.live], m.width-2, m.t.showReasoning())
}

// syncWindow prints the transcript rows that no longer fit in the frame.
//
// The frame keeps a constant height for the whole session (see window), because
// bubbletea's inline renderer cannot handle a frame that shrinks while lines are
// printed above it: the footer creeps up the terminal and leaves rows of stale
// blank space under it. So rather than the pane resizing to its content, content
// that leaves the top of the pane is written into the terminal's own scrollback -
// reachable with the wheel or shift-pageup, selectable, and still on screen after
// mini exits. (A viewport that could only show what fits offered none of that,
// and what fell off its top was gone for good.)
func (m *tuiModel) syncWindow() {
	keep := m.avail() - len(m.liveRows())
	if keep < 0 {
		keep = 0
	}
	if target := len(m.rows) - keep; target > m.printed {
		start := m.printed
		m.advanceFrontier(target)
		if m.printed > start {
			m.print(m.rows[start:m.printed])
		}
	}
}

// allRows is the whole transcript, printed or not, plus the turn in flight. The
// terminal shows it as scrollback above the frame plus the frame's own window;
// tests and the reprint paths read it as one list.
func (m *tuiModel) allRows() []string {
	return append(slices.Clone(m.rows), m.liveRows()...)
}

// advanceFrontier moves the printed cursor forward to the largest block boundary at
// or before target, keeping printed (rows) and printedMark (blocks) in step. Rows
// are printed as they leave the window, and a block split across that frontier could
// not be re-rendered on resize; aligning to blocks keeps each printed unit exactly
// what renderBlock produced. The cursor only moves forward, so this is O(1)
// amortised rather than a rescan of the history on every repaint.
func (m *tuiModel) advanceFrontier(target int) {
	if target > len(m.rows) {
		target = len(m.rows)
	}
	for m.printedMark < len(m.rowCounts) && m.printed+m.rowCounts[m.printedMark] <= target {
		m.printed += m.rowCounts[m.printedMark]
		m.printedMark++
	}
}

// rowsThroughBlock is the number of rows the first n blocks contributed.
func (m *tuiModel) rowsThroughBlock(n int) int {
	total := 0
	for _, c := range m.rowCounts[:max(n, 0)] {
		total += c
	}
	return total
}

// rewrap re-renders the unprinted tail at the current width. Rows already in the
// terminal's history are fixed - they cannot be edited in place - but rows still
// in the frame must match the new width, or they wrap and make the frame taller
// than the screen again, which is the footer drifting all over.
func (m *tuiModel) rewrap() {
	if m.printed != m.rowsThroughBlock(m.printedMark) {
		// The frontier is block-aligned by construction; clamp rather than
		// corrupt if an earlier path ever breaks that.
		m.printed = m.rowsThroughBlock(m.printedMark)
	}
	kept := m.printed
	m.rows = slices.Clone(m.rows[:kept])
	m.rowCounts = m.rowCounts[:m.printedMark]
	show := m.t.showReasoning()
	for _, b := range m.finalizedBlocks()[m.printedMark:] {
		rows := blockRows(b, m.width-2, show)
		m.rows = append(m.rows, rows...)
		m.rowCounts = append(m.rowCounts, len(rows))
	}
}

// finalizedBlocks are the blocks whose rows live in m.rows; the in-flight turn is
// rendered on demand instead.
func (m *tuiModel) finalizedBlocks() []block {
	if m.live >= 0 && len(m.blocks) > m.live {
		return m.blocks[:m.live]
	}
	return m.blocks
}

// print hands rows to the printer goroutine. It must not call program.Printf
// itself: that hands off to the very loop this runs on.
func (m *tuiModel) print(rows []string) {
	m.t.printer.Print(strings.TrimRight(strings.Join(rows, "\n"), "\n"))
}

// updateLive re-renders the in-flight turn from whatever has streamed so far.
func (m *tuiModel) updateLive() {
	reasoning, content := m.t.StreamText()
	if reasoning == "" && content == "" {
		return
	}
	b := block{kind: "assistant", text: content, reasoning: reasoning}
	if m.live < 0 {
		b.title = m.t.AssistantHeader()
		m.blocks = append(m.blocks, b)
		m.live = len(m.blocks) - 1
	} else {
		b.title = m.blocks[m.live].title
		if b.title == "" {
			b.title = m.t.AssistantHeader()
		}
		m.blocks[m.live] = b
	}
	m.dirty = true
}

// scheduleRepaint coalesces repaints: at most one tick is in flight, and with
// no program running (tests drive the loop directly) the repaint happens right
// here instead, since nothing would ever deliver the tick.
func (m *tuiModel) scheduleRepaint() tea.Cmd {
	if !m.dirty {
		return nil
	}
	if m.t.program == nil {
		m.flushRepaint()
		return nil
	}
	if m.refreshing {
		return nil
	}
	m.refreshing = true
	return func() tea.Msg {
		time.Sleep(repaintInterval)
		return repaintMsg{}
	}
}

// renderAll re-renders every block (the thinking toggle changes how past turns
// look) and replaces the printed history with the result, keeping the newest
// rows in the frame so the pane does not go blank.
func (m *tuiModel) renderAll() {
	show := m.t.showReasoning()
	m.rows = nil
	m.rowCounts = nil
	m.printedMark = 0
	for i, b := range m.blocks {
		if i == m.live {
			continue // still streaming: the pane's job, not printed history
		}
		rows := blockRows(b, m.width-2, show)
		m.rows = append(m.rows, rows...)
		m.rowCounts = append(m.rowCounts, len(rows))
	}
	// The tail that still fits the frame stays unprinted and on screen; the
	// frontier lands on a block boundary, and blockBoundary already left
	// printedMark pointing there.
	m.advanceFrontier(max(len(m.rows)-m.avail(), 0))
	body := strings.TrimRight(strings.Join(m.rows[:m.printed], "\n"), "\n")
	// shortcut: this rewrites the screen but cannot retract the terminal's own
	// scrollback, so the previous rendering stays above as history. The full
	// fidelity copy is the trajectory file.
	m.t.printer.reprint(body)
}

// flushRepaint refreshes the frame for the turn in flight and prints whatever
// the new rows pushed out of the top.
func (m *tuiModel) flushRepaint() {
	m.dirty = false
	m.syncWindow()
}

// --------------------------------------------------------------- the view ---

// window returns exactly avail rows of transcript: the newest ones, padded with
// blanks while the session is short. Constant height is the point - the prompt
// must not move as output flows.
func (m *tuiModel) window() string {
	avail := m.avail()
	rows := slices.Clone(m.rows[m.printed:])
	rows = append(rows, m.liveRows()...)
	if hidden := len(rows) - avail; hidden > 0 {
		if avail > 1 {
			marker := tuiStyler.dim.Render(fmt.Sprintf("⋮ %d earlier lines of this turn above", hidden))
			rows = append([]string{marker}, rows[hidden:]...)
			if len(rows) > avail {
				rows = rows[:avail]
			}
		} else {
			rows = rows[hidden:]
		}
	}
	for len(rows) < avail {
		rows = append(rows, "")
	}
	return strings.Join(rows, "\n")
}

// avail is the transcript rows inside the frame: the screen minus the chrome
// actually being drawn, so a panel opening or a draft growing a line moves rows
// between the window and the chrome but never changes the frame's height.
//
// The frame is exactly as tall as the screen, and that is load-bearing. Printing
// above the pane inserts rows at the top of the frame region, so with a
// full-height frame the insert simply scrolls the terminal and the rows it
// consumes are the ones being printed. With a shorter frame the insert swallows
// the pane's own bottom rows and shifts the whole pane down, and the next repaint
// (a diff of unchanged content) never puts them back. Measured, not theorised:
// five 6-row inserts above a 7-row pane stranded the footer at row 7 of 24 with a
// stale copy of its header left behind, while the same inserts against a
// full-height pane kept the footer on row 24.
func (m *tuiModel) avail() int {
	h := m.height - m.chromeRows()
	if h < 1 {
		h = 1
	}
	return h
}

const windowPlaceholder = "\x00window\x00"

// chromeRows measures the frame's non-transcript rows by building the frame with
// a placeholder in the transcript slot. Measuring beats counting by hand: the
// confirm panel, the limits dialog and a multi-line draft all change the chrome,
// and a chrome estimate that is short by one row is a row the footer drifts.
func (m *tuiModel) chromeRows() int {
	bare := strings.Replace(m.frame(windowPlaceholder), windowPlaceholder+"\n", "", 1)
	return lipgloss.Height(strings.TrimSuffix(bare, "\n"))
}

// chromeBusy reports whether mandatory chrome (a panel, dialog, question or
// multi-line draft) has already claimed the space the expanded help would want.
func (m *tuiModel) chromeBusy() bool {
	return m.dialog != "" || m.pendingPanel() != "" || m.pendingVisible() != "" ||
		strings.Contains(m.editor.Value(), "\n")
}

func (m *tuiModel) pendingVisible() string {
	if !m.t.HasQuestion() {
		return ""
	}
	q := m.t.PendingQuestion()
	switch q.Kind {
	case AskConfirm:
		return q.Title
	case AskNewTask, AskHuman, AskInterrup, AskTask:
		return q.Title
	default:
		return ""
	}
}

func (m *tuiModel) View() tea.View {
	v := tea.NewView("")
	if !m.ready {
		// Nothing has been drawn at the real size yet. Rendering a frame at the
		// default height first would make the next WindowSizeMsg resize the frame,
		// and the inline renderer does not re-anchor after a height change: it
		// leaves the pane that far above the bottom of the terminal for the rest of
		// the session.
		return v
	}
	v.Content = m.viewContent()
	// The cursor is always the editor's: the user must be able to see that
	// typing goes somewhere, even (and especially) while a prompt is waiting.
	v.Cursor = m.editor.Cursor()
	return v
}

// viewContent is what the renderer is handed. The trailing newline is dropped:
// it would make the frame one row taller than the screen, and the top row - the
// header - then scrolls off the terminal for good.
func (m *tuiModel) viewContent() string {
	return strings.TrimSuffix(m.frame(m.window()), "\n")
}

// frame paints the pane: the transcript window, then the chrome under it. The
// frame is exactly the terminal's height, which is what makes printing above it
// safe - see avail. Its top row must therefore be transcript: printing above the
// pane writes into the top rows of the frame region, and with a full-height frame
// those rows are the ones the printed lines just left. Nothing session-specific
// belongs there, so the startup header is transcript (sessionHeader), not chrome.
func (m *tuiModel) frame(transcript string) string {
	var sb strings.Builder
	sb.WriteString(transcript + "\n")
	sb.WriteString("\n")
	if panel := m.pendingPanel(); panel != "" {
		sb.WriteString(panel + "\n")
	}
	if m.dialog == "quit" {
		sb.WriteString(tuiStyler.prompt.MaxWidth(m.width).Render("Quit? y/enter to leave, esc to stay") + "\n")
	}
	if m.dialog == "limits" {
		label := "New step limit: "
		if m.limitsIdx == 1 {
			label = "New cost limit: "
		}
		sb.WriteString(tuiStyler.prompt.MaxWidth(m.width).Render(label) + "\n" + m.editor.View() + "\n")
		sb.WriteString(tuiStyler.status.MaxWidth(m.width).Render(m.t.RunStats()) + "\n")
		return sb.String()
	}
	if q := m.pendingVisible(); q != "" && m.dialog == "" {
		sb.WriteString(tuiStyler.prompt.MaxWidth(m.width).Render(q) + "\n")
	}
	sb.WriteString(m.editor.View() + "\n")
	sb.WriteString(tuiStyler.status.Render(m.t.RunStats()) + "\n")
	// Expanded help, a confirm panel, a dialog and a tall draft cannot all fit
	// on one screen, and a frame that changes height is a frame that drifts: the
	// optional chrome yields to the mandatory.
	// A copy: View() must not mutate the model, which chromeRows can leave it
	// doing from another goroutine.
	h := m.help
	h.ShowAll = m.showFullHelp && !m.chromeBusy()
	// The help bubble lays its groups out for the width it was given but does not
	// break inside one, so it can still hand back a line wider than a narrow
	// terminal: wrap it here or the terminal wraps it for us and the frame is a
	// row taller than measured.
	sb.WriteString(tuiStyler.dim.MaxWidth(m.width).Render(h.View(m.keymap)) + "\n")
	return sb.String()
}

// pendingPanel lists the actions awaiting confirmation: the plan's pending
// action panel, which is BeforeExecute blocking on the answer channel.
const panelMaxActions = 4

func (m *tuiModel) pendingPanel() string {
	if !m.t.HasQuestion() {
		return ""
	}
	q := m.t.PendingQuestion()
	if q.Kind != AskConfirm || len(q.Actions) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(tuiStyler.badge.Render("commands") + "\n")
	shown := q.Actions
	if len(shown) > panelMaxActions {
		sb.WriteString(fmt.Sprintf("  +%d more commands\n", len(shown)-panelMaxActions))
		shown = shown[:panelMaxActions]
	}
	for _, a := range shown {
		// A command can be many lines and arbitrarily wide; the panel's box is
		// measured like any other row, so it has to fit before it is drawn.
		cmd := strings.Join(fitLines(strings.Split(truncateVisible(a.Command, 6), "\n"), m.width-6), "\n")
		sb.WriteString("  " + cmd + "\n")
	}
	sb.WriteString(tuiStyler.dim.Render("enter to confirm · type a comment and enter to reject · /y to go yolo · /h for commands"))
	return tuiStyler.panel.MaxWidth(m.width).Render(sb.String())
}
