package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/VictorLowther/mini/agent"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
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

	// blocks is the transcript source of truth; lines holds it rendered. Lines
	// are *printed* to the terminal as they finish (see printNew), so the pane
	// only ever holds what has not been printed yet and the terminal's own
	// scrollback is the history.
	blocks  []block
	lines   []string
	printed int
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
	ta.MaxHeight = 15
	ta.Prompt = "│ "
	ta.Placeholder = "message · enter sends · \\ + enter or /m for multi-line · /h commands"
	ta.SetWidth(t.width - 2)
	ta.Focus()
	return &tuiModel{
		t: t, task: task, editor: ta,
		keymap: newKeyMap(), help: help.New(),
		width: t.width, height: t.height,
		cancelRun: cancelRun,
		live:      -1,
	}
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
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.t.mu.Lock()
		m.t.width, m.t.height = msg.Width, msg.Height
		m.t.mu.Unlock()
		m.editor.SetWidth(msg.Width - 2)
		m.help.SetWidth(msg.Width)
		m.ready = true
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

// commit places a message in the scrollback, replacing the in-flight turn's
// block when it is the committed form of that same turn.
func (m *tuiModel) commit(b block) {
	show := m.t.showReasoning()
	switch {
	case b.kind == "assistant" && m.live >= 0:
		m.blocks[m.live] = b
		m.lines[m.live] = renderBlock(b, m.width-2, show)
		m.live = -1
	default:
		// Something else is ending the turn: release the in-flight block so the
		// pane stops showing a half-streamed turn, and never let a later commit
		// overwrite text that already reached the scrollback.
		if m.live >= 0 {
			m.live = -1
			m.printNew()
		}
		m.blocks = append(m.blocks, b)
		m.lines = append(m.lines, renderBlock(b, m.width-2, show))
	}
	m.printNew()
}

// printNew sends every finished line above the pane, into the terminal's own
// scrollback. bubbletea's Printf scrolls the real screen and writes there, so
// committed output behaves like shell output: reachable with the wheel or
// shift-pageup, selectable, and still on screen after mini exits. (A viewport
// of the transcript could offer none of that: it shows only what fits, and what
// fell off its top was gone for good.)
func (m *tuiModel) printNew() {
	if m.printed >= len(m.lines) {
		return
	}
	body := strings.TrimRight(strings.Join(m.lines[m.printed:], "\n\n"), "\n")
	m.printed = len(m.lines)
	m.t.printer.Print(body)
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
		m.lines = append(m.lines, "")
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
// look) and replaces the printed history with the result. The in-flight turn is
// left unprinted: it is still the pane's job.
func (m *tuiModel) renderAll() {
	show := m.t.showReasoning()
	m.lines = m.lines[:0]
	for _, b := range m.blocks {
		m.lines = append(m.lines, renderBlock(b, m.width-2, show))
	}
	upTo := len(m.lines)
	if m.live >= 0 {
		upTo = m.live
	}
	m.printed = upTo
	body := strings.TrimRight(strings.Join(m.lines[:upTo], "\n\n"), "\n")
	// shortcut: this rewrites the screen but cannot retract the terminal's own
	// scrollback, so the previous rendering stays above as history (like any
	// re-printed shell output). Full fidelity lives in the trajectory file.
	m.t.printer.reprint(body)
}

// flushRepaint re-renders the in-flight turn. Printed lines keep their render:
// they are in the terminal's history now.
func (m *tuiModel) flushRepaint() {
	m.dirty = false
	if m.live >= 0 {
		m.lines[m.live] = renderBlock(m.blocks[m.live], m.width-2, m.t.showReasoning())
	}
}

// --------------------------------------------------------------- the view ---

// liveView returns the unprinted tail of the transcript: the turn in flight.
// It is capped to the room left above the prompt because anything older has
// already been printed, or is still arriving.
func (m *tuiModel) liveView() string {
	avail := m.height - m.footerHeight() - 2
	if avail < 1 {
		avail = 1
	}
	var raw []string
	for _, l := range m.lines[m.printed:] {
		if l != "" {
			raw = append(raw, strings.Split(l, "\n")...)
		}
	}
	if hidden := len(raw) - avail; hidden > 0 && avail > 1 {
		// Keep the newest lines and make room for the marker among them: the
		// beginning of a long turn is already unreachable by other means.
		marker := tuiStyler.dim.Render(fmt.Sprintf("⋮ %d earlier lines of this turn above", hidden))
		raw = append([]string{marker}, raw[len(raw)-avail+1:]...)
	}
	return strings.Join(raw, "\n")
}

func (m *tuiModel) footerHeight() int {
	lines := 1 // status
	lines += 1 // help
	if m.dialog != "" {
		lines += 2
	}
	if q := m.pendingVisible(); q != "" {
		lines += 2
	}
	if m.editor.Value() == "" {
		lines += 1
	} else {
		lines += 1 + strings.Count(m.editor.Value(), "\n")
	}
	return lines
}

// pendingVisible describes the question being answered, for the status area.
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
	v.Content = m.content()
	// The cursor is always the editor's: the user must be able to see that
	// typing goes somewhere, even (and especially) while a prompt is waiting.
	v.Cursor = m.editor.Cursor()
	return v
}

func (m *tuiModel) content() string {
	var sb strings.Builder
	header := fmt.Sprintf("mini-go %s · %s @ %s · config %s", agent.Version, m.t.modelName, m.t.baseURL, m.t.configName)
	sb.WriteString(tuiStyler.dim.Render(header) + "\n")
	sb.WriteString(m.liveView())
	sb.WriteString("\n")
	if panel := m.pendingPanel(); panel != "" {
		sb.WriteString(panel + "\n")
	}
	if m.dialog == "quit" {
		sb.WriteString(tuiStyler.prompt.Render("Quit? y/enter to leave, esc to stay") + "\n")
	}
	if m.dialog == "limits" {
		label := "New step limit: "
		if m.limitsIdx == 1 {
			label = "New cost limit: "
		}
		sb.WriteString(tuiStyler.prompt.Render(label) + m.editor.View() + "\n")
		sb.WriteString(tuiStyler.status.Render(m.t.RunStats()) + "\n")
		return sb.String()
	}
	if q := m.pendingVisible(); q != "" && m.dialog == "" {
		sb.WriteString(tuiStyler.prompt.Render(q) + "\n")
	}
	sb.WriteString(m.editor.View() + "\n")
	sb.WriteString(tuiStyler.status.Render(m.t.RunStats()) + "\n")
	m.help.ShowAll = m.showFullHelp
	sb.WriteString(tuiStyler.dim.Render(m.help.View(m.keymap)) + "\n")
	return sb.String()
}

// pendingPanel lists the actions awaiting confirmation: the plan's pending
// action panel, which is BeforeExecute blocking on the answer channel.
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
	for _, a := range q.Actions {
		sb.WriteString("  " + truncateVisible(a.Command, 6) + "\n")
	}
	sb.WriteString(tuiStyler.dim.Render("enter to confirm · type a comment and enter to reject · /y to go yolo · /h for commands"))
	return tuiStyler.panel.Render(sb.String())
}
