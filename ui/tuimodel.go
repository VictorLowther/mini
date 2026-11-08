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
	"charm.land/bubbles/v2/viewport"
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
	ScrollUp       key.Binding
	ScrollDown     key.Binding
	PageUp         key.Binding
	PageDown       key.Binding
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
		ScrollUp:       key.NewBinding(key.WithKeys("up"), key.WithHelp("up", "scroll")),
		ScrollDown:     key.NewBinding(key.WithKeys("down"), key.WithHelp("down", "scroll")),
		PageUp:         key.NewBinding(key.WithKeys("pgup", "ctrl+u"), key.WithHelp("pgup", "page up")),
		PageDown:       key.NewBinding(key.WithKeys("pgdown", "ctrl+d"), key.WithHelp("pgdn", "page down")),
		ToggleThinking: key.NewBinding(key.WithKeys("ctrl+t"), key.WithHelp("ctrl+t", "thinking")),
		Help:           key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	}
}

func (k tuiKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Send, k.Cancel, k.Quit, k.Help}
}

func (k tuiKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Send, k.Newline, k.Cancel, k.Quit}, {k.ScrollUp, k.ScrollDown, k.PageUp, k.PageDown, k.ToggleThinking}}
}

// tuiModel is the bubbletea model. It owns no agent state: everything it shows
// comes from the TUI it is attached to.
type tuiModel struct {
	t    *TUI
	task string

	viewport viewport.Model
	editor   textarea.Model
	keymap   tuiKeyMap
	help     help.Model

	width, height int
	ready         bool

	// scrollback, already rendered to styled strings
	lines []string
	// streamLine is the rendered in-progress assistant block
	streaming  bool
	dirty      bool
	refreshing bool
	streamText string

	// dialog is the modal on top of everything else
	dialog    string // "", "quit", "limits"
	limitsIdx int
	limits    [2]float64

	showFullHelp bool
	multiline    bool

	cancelRun context.CancelFunc
}

func newTUIModel(t *TUI, task string, cancelRun context.CancelFunc) *tuiModel {
	vp := viewport.New(viewport.WithWidth(t.width), viewport.WithHeight(t.height))
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
		t: t, task: task, viewport: vp, editor: ta,
		keymap: newKeyMap(), help: help.New(),
		width: t.width, height: t.height,
		cancelRun: cancelRun,
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
		m.viewport.SetWidth(msg.Width)
		m.viewport.SetHeight(m.viewportHeight())
		m.editor.SetWidth(msg.Width - 2)
		m.help.SetWidth(msg.Width)
		m.ready = true
		m.rebuild()
		return m, nil

	case streamMsg:
		m.streaming = true
		_, m.streamText = m.t.StreamText()
		m.dirty = true
		return m, m.scheduleRepaint()

	case blockMsg:
		m.appendBlock(msg.b)
		m.streaming = false
		m.streamText = ""
		return m, nil

	case repaintMsg:
		m.refreshing = false
		if m.dirty {
			m.dirty = false
			m.refreshViewport()
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
		m.rebuild()
		m.editor.Focus()
		return m, nil

	case runDoneMsg:
		// A clean finish leaves immediately: the final frame stays in the
		// terminal history (no alternate screen), so nothing is lost. A failed
		// run keeps the pane open so the error stays readable and the user
		// decides when to leave.
		if m.finishRun() {
			return m, tea.Quit
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
	case key.Matches(msg, m.keymap.PageUp):
		m.viewport.HalfPageUp()
		return nil
	case key.Matches(msg, m.keymap.PageDown):
		m.viewport.HalfPageDown()
		return nil
	case key.Matches(msg, m.keymap.ScrollUp) && m.editor.Value() == "":
		m.viewport.ScrollUp(1)
		return nil
	case key.Matches(msg, m.keymap.ScrollDown) && m.editor.Value() == "":
		m.viewport.ScrollDown(1)
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
	m.noteLocal(map[bool]string{true: "thinking shown", false: "thinking hidden"}[on])
	m.rebuild()
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
	return tea.Quit
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

// appendBlock renders and stores one entry, keeping the viewport pinned to the
// bottom when the user is already there.
func (m *tuiModel) appendBlock(b block) {
	m.lines = append(m.lines, renderBlock(b, m.width-2))
	if b.reasoning {
		// The thinking text belongs to the assistant block above it.
		m.lines[len(m.lines)-1] = renderBlock(block{kind: "note", text: "thinking: " + b.text}, m.width-2)
	}
	m.refreshViewport()
}

// rebuild re-renders the viewport immediately (user-visible state changed
// outside the stream).
func (m *tuiModel) rebuild() {
	m.dirty = false
	m.refreshViewport()
}

// scheduleRepaint returns at most one pending repaint tick.
func (m *tuiModel) scheduleRepaint() tea.Cmd {
	if m.refreshing {
		return nil
	}
	m.refreshing = true
	return func() tea.Msg {
		time.Sleep(repaintInterval)
		return repaintMsg{}
	}
}

func (m *tuiModel) refreshViewport() {
	atBottom := m.viewport.AtBottom()
	var sb strings.Builder
	for _, l := range m.lines {
		sb.WriteString(l)
		sb.WriteString("\n\n")
	}
	if m.streaming {
		// Thinking text is collapsed by default: it is by far the largest
		// stream and burying the useful output under it is what makes the pane
		// unreadable mid-turn. `t` shows it.
		reasoning, _ := m.t.StreamText()
		if m.t.showReasoning() && reasoning != "" {
			sb.WriteString(tuiStyler.dim.Render("thinking: " + truncateVisible(reasoning, 40)))
			sb.WriteString("\n")
		} else if reasoning != "" {
			sb.WriteString(tuiStyler.dim.Render(fmt.Sprintf("thinking… %d chars (t to show)", len(reasoning))))
			sb.WriteString("\n")
		}
		if m.streamText != "" {
			sb.WriteString(m.streamText)
		}
	}
	m.viewport.SetContent(sb.String())
	if atBottom {
		m.viewport.GotoBottom()
	}
}

// --------------------------------------------------------------- the view ---

func (m *tuiModel) viewportHeight() int {
	h := m.height - m.footerHeight() - 2
	if h < 3 {
		h = 3
	}
	return h
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
	sb.WriteString(m.viewport.View())
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
