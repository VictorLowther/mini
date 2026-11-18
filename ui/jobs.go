package ui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/VictorLowther/mini/model"
)

// JobCard is what a tool card needs to know about the job its call handed off
// to. The front end deliberately does not know about the registry, the store, or
// pids: it asks one question per card and renders the answer, which is what lets
// the same card work in the GUI, in print mode, and in a test with a stub.
//
// Running is carried separately from State because the distinction the card
// makes is "still going" versus "over", and a front end that compared strings
// against a spelling owned by another package would be a rename away from
// claiming every job had finished.
type JobCard struct {
	ID         string
	State      string
	Running    bool
	ExitCode   *int
	Label      string
	Command    string
	OutputPath string
	Reason     string
	Elapsed    time.Duration
	// Kind tells a card what it is tracking. A subagent's card answers a
	// different question from a command's: not what it printed (that is in the
	// child's own tab) but how far it has got - steps taken and tokens spent.
	Kind   string
	Steps  int
	Tokens int
	Cost   float64
	// Notified reports that the run ended and nothing has told the model yet,
	// which is the difference between a card that says "completed" and one that
	// says "completed - waiting to be reported".
	Notified bool
}

// JobReader answers for one job id. found is false when the registry has no such
// job, which the card renders as "no longer tracked" rather than as a completed
// run - the honest reading of a row that was swept at a restart.
type JobReader interface {
	JobCard(id string) (card JobCard, found bool)
}

// jobCardLines renders a handed-off call's card. The call never completed, so
// nothing here may read like an exit status until the job has one: the first
// line is the job's state and age, the body is the live tail of the log it is
// still appending to, and the tail is bounded by the same window the model is
// given so the card cannot grow without limit while a training run writes.
func jobCardLines(c JobCard, found bool) []string {
	var out []string
	if !found {
		// The row is gone or was swept at a restart. Say that rather than
		// leaving a card that looks like a command which never ended.
		return []string{fmt.Sprintf("job %s: no longer tracked by this session", c.ID)}
	}
	if c.Running {
		out = append(out, fmt.Sprintf("job %s is still running (%s)", c.ID, c.Elapsed.Truncate(time.Second)))
		if c.Kind == "subagent" {
			// Progress, not prose: the parent needs to know whether the child is
			// thinking, spinning, or stuck - and its transcript is one click away
			// in its own tab.
			out = append(out, fmt.Sprintf("  %d steps \u00b7 %d tokens \u00b7 $%.2f so far",
				c.Steps, c.Tokens, c.Cost))
		}
	} else {
		head := fmt.Sprintf("job %s %s after %s", c.ID, c.State, c.Elapsed.Truncate(time.Second))
		switch {
		case c.ExitCode != nil && *c.ExitCode != 0:
			head += fmt.Sprintf(" (exit %d)", *c.ExitCode)
		case c.ExitCode != nil:
			head += " (exit 0)"
		}
		out = append(out, head)
		if c.Notified {
			out = append(out, "  ending the job has not been reported to the model yet")
		}
	}
	if c.Label != "" {
		out = append(out, "  "+c.Label)
	}
	if c.Reason != "" && c.Running {
		out = append(out, "  why: "+c.Reason)
	}
	out = append(out, "  output: "+c.OutputPath)
	// The live tail. Read on every rebuild, so the card follows the job without
	// the front end holding any state of its own.
	if body, err := tailFile(c.OutputPath, model.MaxViewLines); err == nil && body != "" {
		out = append(out, strings.Split(strings.TrimRight(body, "\n"), "\n")...)
	} else if c.Running {
		out = append(out, "  (no output yet)")
	}
	return out
}

// pendingCardLines renders a handed-off call for a front end that may or may
// not have the registry attached. With one, the card is live: it reads the
// job's state and the tail of the log it is still writing, on every rebuild.
// Without one (print mode, a replay of a saved session), it says the same thing
// from the observation alone and stops short of pretending to be current - the
// failure mode this whole path exists to avoid is a card that reads "exit 0"
// while the job is still holding memory.
func pendingCardLines(obs observation, jobs JobReader) []string {
	if jobs != nil {
		card, found := jobs.JobCard(obs.JobID)
		if card.ID == "" {
			card.ID = obs.JobID
		}
		if card.OutputPath == "" {
			card.OutputPath = obs.Path
		}
		return jobCardLines(card, found)
	}
	out := []string{fmt.Sprintf("job %s: started from this call and still running when it returned", obs.JobID)}
	if obs.Path != "" {
		out = append(out, "  output: "+obs.Path)
	}
	return out
}

// tailFile returns the last n lines of a file that may still be being written.
// model.ViewOf already computes exactly this bounded tail, so the card shows the
// same text the model would get for the same path - one window, one arithmetic.
func tailFile(path string, n int) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	view, _, _, err := model.ViewOf(path)
	return view, err
}
