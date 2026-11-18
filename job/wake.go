package job

import (
	"github.com/VictorLowther/mini/session"
)

// This file is the wake queue: the durable record of things that happened to
// jobs, and the two operations anyone needs on it - look, and claim.
//
// Nothing here blocks, and there is no wait tool. job_wait existed for one
// commit; the parked run that replaced it existed for one more. Both were
// discarded once the job's state machine was written down: a job that has
// finished without reaching the conversation is an obligation, and an obligation
// is met by taking the step that discharges it, not by waiting to be told the
// step is allowed.

// Firing is a claimed event together with the job it happened to, which is all
// either consumer of the queue needs: the wait reports one, the step-boundary
// delivery reports the whole pending set, and neither has to know how the row was
// found.
type Firing struct {
	Event session.JobEventRow
	Job   session.JobRow
}

// ClaimFirings takes up to limit undelivered firings, oldest first, and marks
// them delivered in the same step. At-most-once is a property of the queue
// rather than a protocol between consumers, which is why the hold and the
// step-boundary drain can both run, in any order, without a wake ever being
// reported twice.
//
// The queue is durable, so an event nobody consumed - a job that ended while the
// run was shutting down, or after the model had already submitted - is still
// here at the next run's first boundary instead of being lost with the process.
func (m *Manager) ClaimFirings(limit int) ([]Firing, error) {
	events, err := m.Session.PendingJobEvents(limit)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(events))
	var out []Firing
	for _, e := range events {
		row, err := m.Session.GetJob(e.JobID)
		if err != nil {
			// Claim it anyway: an event whose job cannot be read must not
			// wedge the queue behind it forever.
			m.logger().Warn("job: firing for an unreadable job", "job", e.JobID, "err", err)
			ids = append(ids, e.ID)
			continue
		}
		ids = append(ids, e.ID)
		out = append(out, Firing{Event: e, Job: *row})
	}
	if err := m.Session.MarkJobEventsDelivered(ids); err != nil {
		return nil, err
	}
	return out, nil
}

// HasUndeliveredFiring reports whether a result from this job is still
// sitting in the queue undelivered - any result, the exit firing or (when
// monitors land) a match, because queued firings are the debt and the reason
// does not matter. The card uses it for "finished, not yet reported": the state
// a human needs to see in the gap between a job ending and the boundary that
// says so.
//
// It used to answer by scanning PendingJobEvents(32) for the id, which put a
// bounded probe where the question has an exact answer (A-10). Past 32
// undelivered firings - a held session, or any long run where jobs end faster
// than boundaries arrive, and now an entirely ordinary situation because the job
// count is unbounded - it reported "settled" for a result genuinely still owed,
// in the one place a human reads to decide whether the harness has told the
// model anything yet.
//
// An unreadable queue reports owed rather than settled. The failure being fixed
// is a false "nothing outstanding", so the tie goes to honest pessimism and the
// reason is logged instead of swallowed.
func (m *Manager) HasUndeliveredFiring(id string) bool {
	n, err := m.Session.CountUndeliveredFiringsFor(id)
	if err != nil {
		m.logger().Warn("job: could not read the queue; reporting the result as still owed",
			"job", id, "err", err)
		return true
	}
	return n > 0
}

// StillRunning counts the jobs that are burning right now, which is what lets a
// wait report "one finished, two still going" rather than leaving the model to
// work out whether the set it asked about was bigger than the answer.
func (m *Manager) StillRunning() int {
	n, err := m.Session.CountRunningJobs()
	if err != nil {
		return 0
	}
	return n
}

// OwedDeliveries counts results that have happened but not been said. It is the
// last state of the machine expressed as a question: finished jobs with no
// delivered firing are exactly what the run still owes, and whether any exist is
// the only thing the loop needs to know to decide if the conversation is over.
func (m *Manager) OwedDeliveries() int {
	n, err := m.Session.UndeliveredFirings()
	if err != nil {
		return 0
	}
	return n
}
