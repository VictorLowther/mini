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
// including the /y /c /u /h /m slash commands, so the interactive state machine
// is testable without a terminal.
type Print struct {
	Out io.Writer
	Cfg *agent.Config
	// Loop supplies the step/cost figures for the message headers.
	Loop *agent.Loop

	in *bufio.Reader
	// Terminal reports whether a human is on stdin. Set from os.Stdin's mode;
	// tests override it.
	Terminal bool
	// streamed tracks text already written for the message currently being
	// added, so a streamed turn is not printed twice.
	streamed       bool
	reasoningOpen  bool
	reasoningKnown bool
}

func NewPrint(out io.Writer, in io.Reader, cfg *agent.Config) *Print {
	fi, err := os.Stdin.Stat()
	return &Print{Out: out, Cfg: cfg, in: bufio.NewReader(in), Terminal: err == nil && fi.Mode()&os.ModeCharDevice != 0}
}

// Interactive reports whether stdin is a terminal worth prompting on.
func (p *Print) Interactive() bool { return p.Terminal }

func (p *Print) Reasoning(text string) {
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

func (p *Print) closeReasoning() {
	if p.reasoningOpen {
		fmt.Fprint(p.Out, "\n")
		p.reasoningOpen = false
	}
	p.reasoningKnown = true
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
		p.closeReasoning()
		// A model reply that never streamed (a replayed turn, or a non-streaming
		// server) still shows its thinking, so the transcript says what the
		// model considered rather than nothing.
		if !p.reasoningKnown {
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
		for _, line := range obs.lines() {
			fmt.Fprintln(p.Out, line)
		}
		return
	}
	if role == "user" || role == "tool" || role == "exit" {
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
	if q.Kind == AskLimits {
		return p.askLimits(ctx, q)
	}
	for {
		fmt.Fprintf(p.Out, "\n%s\n%s", q.Title, promptLine())
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

func (p *Print) askLimits(ctx context.Context, q Question) (Answer, error) {
	fmt.Fprint(p.Out, "\n"+q.Title)
	stepsText, err := p.readLine(ctx)
	if err != nil {
		return Answer{}, err
	}
	fmt.Fprint(p.Out, "New cost limit: ")
	costText, err := p.readLine(ctx)
	if err != nil {
		return Answer{}, err
	}
	return Answer{NewLimits: true, Steps: int(ParseLimitLine(stepsText)), Cost: ParseLimitLine(costText)}, nil
}
