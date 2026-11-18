package main

import (
	"time"

	"github.com/VictorLowther/mini/host"
	"github.com/VictorLowther/mini/job"
	"github.com/VictorLowther/mini/session"
	"github.com/VictorLowther/mini/ui"
)

// jobCards adapts the registry to the two questions the front ends ask about a
// call that handed off: what is this job doing, and how many are running.
//
// It lives here rather than in either package so ui never imports the registry's
// types and job never learns about the transcript. A card is rebuilt from the
// store on every read, which is what lets the view hold no job state of its own
// and therefore nothing that can go stale between the handoff and the exit.
type jobCards struct {
	m    *job.Manager
	proc *host.Process
}

// jobReader returns nil (not a zero-value adapter) when there is no registry, so
// the front ends take their "no registry attached" path instead of asking a nil
// manager a question and finding out at runtime.
func jobReader(m *job.Manager, proc *host.Process) ui.JobReader {
	if m == nil {
		return nil
	}
	return jobCards{m, proc}
}

func (j jobCards) JobCard(id string) (ui.JobCard, bool) {
	row, err := j.m.Session.GetJob(id)
	if err != nil {
		// Unknown here means the session has no such job. The card says so; it
		// does not fall back to a spinner, which would look like a job that
		// never ends.
		return ui.JobCard{ID: id}, false
	}
	card := ui.JobCard{
		ID:         row.ID,
		State:      row.State,
		Running:    row.State == session.JobRunning,
		ExitCode:   row.ExitCode,
		Label:      row.Label,
		Command:    row.Command,
		OutputPath: row.OutputPath,
	}
	end := time.Now().UnixMilli()
	if row.EndedAt != nil {
		end = *row.EndedAt
	}
	card.Elapsed = time.Duration(end-row.StartedAt) * time.Millisecond
	if card.Elapsed < 0 {
		card.Elapsed = 0
	}
	// The live handle knows why the run became a job; the row only knows how it
	// ended. Reading the handle first is what makes a promoted run's card say
	// "outran its 2s budget" while it is still burning.
	if run, err := j.m.Get(id); err == nil {
		if why := run.Reason(); why != "" {
			card.Reason = why
		}
	}
	if card.Reason == "" {
		card.Reason = row.ExitReason
	}
	if row.Kind == session.KindSubagent && j.proc != nil {
		// The child's live numbers come from the session, not its job row: the
		// row knows the run is going, the loop knows how far it has got. A child
		// from an earlier process is not in the in-memory edge, and the card then
		// says nothing about steps rather than guessing from the row.
		if child, ok := j.proc.ChildOfJob(row.SessionID, id); ok {
			snap := child.Loop.Snapshot()
			card.Kind = row.Kind
			card.Steps, card.Tokens, card.Cost = snap.Calls, snap.ModelTokens, snap.Cost
		}
	}
	if !card.Running {
		// An ended job whose firing nobody has collected yet: the human can see
		// the outcome, and the model will be told at the next turn boundary.
		card.Notified = j.m.HasUndeliveredFiring(id)
	}
	return card, true
}

// LiveJobs feeds the footer count, which stays truthful even when the transcript
// is scrolled up through history and the cards themselves are not on screen.
func (j jobCards) LiveJobs() (int, error) {
	return j.m.Session.CountRunningJobs()
}
