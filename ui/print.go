package ui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/model"
)

// Print is the headless front end used by --print and whenever stdout is not a
// terminal. It streams the model's text to stdout and prompts on stdin,
// including the /y /c /u /h /t /m slash commands, so the interactive state
// machine is testable without a terminal. The thinking toggle is the same
// stored preference the window reads (ui/prefs.go).
type Print struct {
	Out io.Writer
	Cfg *agent.Config
	// Loop supplies the step/cost figures for the message headers.
	Loop *agent.Loop
	// Jobs reads the registry so a card for a handed-off call can report the
	// job's state instead of an exit status the command never produced. nil is
	// a front end with no registry: the card then says what the observation
	// knows and nothing more.
	Jobs JobReader
	// settings is where /t writes the thinking toggle so the next launch -
	// this one or the window's - starts where the user left it. nil (tests,
	// no store) keeps the toggle for the session only.
	settings Settings

	in *bufio.Reader
	// asks serializes prompts; see askQueue and docs/parallel-sessions.md §9.4.
	asks askQueue
	// Terminal reports whether a human is on stdin. Set from os.Stdin's mode;
	// tests override it.
	Terminal bool
	// streamed tracks text already written for the message currently being
	// added, so a streamed turn is not printed twice.
	streamed       bool
	reasoningOpen  bool
	reasoningKnown bool
	// showThinking is the printed half of the /t toggle and of the sticky
	// preference behind it: thinking is collapsed until someone asks for it,
	// in --print exactly as in the window.
	showThinking bool
}

func NewPrint(out io.Writer, in io.Reader, cfg *agent.Config, settings Settings) *Print {
	fi, err := os.Stdin.Stat()
	return &Print{
		Out: out, Cfg: cfg, settings: settings,
		in:           bufio.NewReader(in),
		Terminal:     err == nil && fi.Mode()&os.ModeCharDevice != 0,
		showThinking: loadThinkingShown(settings),
	}
}

// SetThinkingShown applies a thinking preference held by something other than
// the store (a test, or a caller that owns the state). It does not write the
// store: toggleThinking is the user-facing act that does.
func (p *Print) SetThinkingShown(shown bool) { p.showThinking = shown }

// toggleThinking is --print's /t, which the help text has always advertised:
// the same collapse the window's chord performs, the same wording, and the
// same sticky write - so a preference set headlessly is waiting in the window
// next time, and vice versa.
func (p *Print) toggleThinking() {
	p.showThinking = !p.showThinking
	saveThinkingShown(p.settings, p.showThinking)
	fmt.Fprintf(p.Out, "\nthinking %s\n", map[bool]string{true: "shown", false: "hidden"}[p.showThinking])
}

// Interactive reports whether stdin is a terminal worth prompting on.
func (p *Print) Interactive() bool { return p.Terminal }

func (p *Print) Reasoning(text string) {
	if !p.showThinking {
		return
	}
	if !p.reasoningOpen {
		fmt.Fprint(p.Out, "\n[thinking] ")
		p.reasoningOpen = true
		p.reasoningKnown = true
	}
	fmt.Fprint(p.Out, text)
}

func (p *Print) Content(text string) {
	p.closeReasoning()
	if !p.streamed {
		p.assistantHeader()
		p.streamed = true
	}
	fmt.Fprint(p.Out, text)
}

// closeReasoning ends an open streamed thinking block. It deliberately does
// NOT mark the turn's reasoning as known - Reasoning already does that when it
// opens the block - because this runs for every message, including the system
// and user turns that precede the first reply. Marking it there made the
// replayed-thinking branch in Message ask a question that was always no, which
// is why --print showed no thinking at all even when the model's reasoning was
// on the turn.
func (p *Print) closeReasoning() {
	if p.reasoningOpen {
		fmt.Fprint(p.Out, "\n")
		p.reasoningOpen = false
	}
}

// Resume prints a stored transcript before the run continues, so a resumed
// -p session is not a blank screen. note names where the history came from.
func (p *Print) Resume(msgs []model.Message, note string) {
	if note != "" {
		fmt.Fprintf(p.Out, "%s\n", note)
	}
	for _, m := range msgs {
		p.Message(m)
	}
}

// assistantHeader is the add_messages banner:
// "mini-swe-agent (step N, $X.XX):"
func (p *Print) assistantHeader() {
	snap := agent.Snapshot{}
	if p.Loop != nil {
		snap = p.Loop.Snapshot()
	}
	fmt.Fprintf(p.Out, "\nmini-swe-agent (step %d, $%.2f):\n", snap.Calls, snap.Cost)
}

// Message prints everything that enters the history.
func (p *Print) Message(m model.Message) {
	role, _ := m["role"].(string)
	content, _ := m["content"].(string)
	if role == "assistant" {
		// A model reply that never streamed (a replayed turn, or --print,
		// which no one wires a streaming handler for) shows its thinking when
		// the preference says to, so the transcript says what the model
		// considered rather than nothing. reasoningKnown is the marker for
		// "this turn's thinking already reached the output" - the streaming
		// path sets it when it opens the block, and this branch clears it at
		// the end - so the question is asked here, where it still means what
		// it says.
		showStored := p.showThinking && !p.reasoningKnown
		p.closeReasoning()
		if showStored {
			if r, ok := m["reasoning_content"].(string); ok && r != "" {
				fmt.Fprintf(p.Out, "\n[thinking] %s\n", r)
			}
		}
		if !p.streamed {
			p.assistantHeader()
			fmt.Fprintln(p.Out, content)
		} else {
			fmt.Fprint(p.Out, "\n")
		}
		p.streamed = false
		p.reasoningKnown = false
		return
	}
	p.closeReasoning()
	if role == "system" {
		return // prompt plumbing, not transcript
	}
	if obs, ok := observationOf(m); ok {
		// What ran, where, how it ended, what it printed. The message content is
		// the JSON the model was promised; nobody reads that in a transcript.
		fmt.Fprintf(p.Out, "\n%s:\n", capitalize(role))
		lines := obs.lines()
		if obs.Pending && obs.JobID != "" {
			// Handed off: the job's state, not an exit status it does not have.
			lines = pendingCardLines(obs, p.Jobs)
		}
		for _, line := range lines {
			fmt.Fprintln(p.Out, line)
		}
		return
	}
	if role == "user" || role == "tool" || role == "exit" || role == "compaction" {
		fmt.Fprintf(p.Out, "\n%s:\n", capitalize(role))
		fmt.Fprintln(p.Out, content)
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

var errNoInput = errors.New("no input available (stdin is not a terminal): pass -y to run unattended")

// readLine returns one line without its newline, or an error at EOF. The
// context cancels a blocked read by closing the run rather than the pipe.
func (p *Print) readLine(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	line, err := p.in.ReadString('\n')
	if line == "" && err != nil {
		// A cancellation while blocked in the read is the reason we have no
		// answer; report that rather than the EOF it unblocks with.
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, io.EOF) {
			return "", errNoInput
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readMultiline collects lines until a blank one, standing in for
// prompt_toolkit's multiline editor.
func (p *Print) readMultiline(ctx context.Context) string {
	fmt.Fprint(p.Out, "(multi-line comment, empty line to finish)\n")
	var lines []string
	for {
		line, err := p.readLine(ctx)
		if err != nil || line == "" {
			break
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// Ask prints the question and interprets the answer, slash commands included.
func (p *Print) Ask(ctx context.Context, q Question) (Answer, error) {
	// The same turn-taking as the window (§9.4). Here the assumption was
	// structural rather than a field being overwritten: this writes the prompt
	// straight to Out and reads one shared stdin, so two concurrent asks would
	// interleave their text and compete for the same line - a garbled prompt is
	// better than the window's silent hang, but neither is acceptable once a
	// second session can ask.
	release, err := p.asks.acquire(ctx)
	if err != nil {
		return Answer{}, err
	}
	defer release()

	for {
		fmt.Fprintf(p.Out, "\n%s\n%s", displayTitle(q), promptLine())
		line, err := p.readLine(ctx)
		if errors.Is(err, errNoInput) {
			// Nobody is left to answer. The exit prompt no longer quits on an
			// empty enter, so an exhausted stdin there would re-ask forever:
			// treat EOF as the explicit /quit and end the run cleanly. Anywhere
			// else the run stops with an error instead of spinning on a
			// rejection that will never change.
			if q.Kind == AskNewTask {
				return Answer{Text: quitCommand}, nil
			}
			return Answer{}, errNoInput
		}
		if err != nil {
			return Answer{}, err
		}
		if line == "/t" {
			p.toggleThinking()
			continue // re-prompt: a toggle is not an answer to the question
		}
		text, reprompt := slash(p.Cfg, line, p.Out, func() string { return p.readMultiline(ctx) })
		if reprompt {
			continue
		}
		return p.answerFor(q, text)
	}
}

func (p *Print) answerFor(q Question, text string) (Answer, error) {
	switch q.Kind {
	case AskTask:
		if text == "" {
			return Answer{}, errors.New("a task is required")
		}
		return Answer{Text: text}, nil
	case AskConfirm:
		if text == "" || text == "/y" {
			return Answer{}, nil // confirmed
		}
		return Answer{Reject: true, Text: text}, nil
	default: // new-task, human, interruption
		return Answer{Text: text}, nil
	}
}
