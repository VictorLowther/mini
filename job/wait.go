package job

import (
	"context"
	"time"

	"github.com/VictorLowther/mini/session"
)

// WaitPoll is how often a wait re-reads the registry. It is a poll on purpose:
// the answer lives in the database, so a tick is enough, and the alternative -
// every waiter hanging off a channel that only the watcher knows about - is how
// a wakeup gets missed during the instant a job is ending. One indexed query
// four times a second, while nothing else is running, costs nothing on a box
// whose interesting work is a training run.
const WaitPoll = 250 * time.Millisecond

// WaitAny blocks until one of the jobs running when it was called leaves the
// running state, and reports that job. It takes no ids: the model is not asked
// to keep a list of its own background work in context to be told when any of it
// finished, which is the entire point of having a registry.
//
// ok is false when there was nothing to wait for, which is an answer and not an
// error - a model that asked while every job had already finished should hear
// that immediately instead of timing out on a question with no subject.
//
// What it waits for is the firing queue, not a set of ids: exits write a firing
// today, and when the monitor engine lands it writes match firings into the same
// queue - so "any job ends, or any monitor fires" becomes true with no change to
// this loop, this tool, or the description the model reads.
func (m *Manager) WaitAny(ctx context.Context) (row *session.JobRow, ok bool, err error) {
	// A firing already sitting there is the answer before anything else, and
	// this is the ordering that matters: a job that ended while the model was
	// doing something else must still be waiting when the model asks, or the
	// wait ignores the one event it exists to report and blocks on a run that
	// may never finish.
	if row, ok, err := m.takeFiring(); ok || err != nil {
		return row, ok, err
	}
	running, err := m.Session.RunningJobIDs()
	if err != nil {
		return nil, false, err
	}
	if len(running) == 0 {
		// Nothing running and nothing owed: an immediate, honest answer.
		return nil, false, nil
	}

	tick := time.NewTicker(WaitPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			// The human stopped waiting. Every run is untouched, and that has
			// to be true: a cancelled wait that read as a killed job would be
			// the worst lie this tool could tell. Returning ctx.Err() alone
			// matches env.Local.Execute's convention, so the loop's existing
			// interruption handling covers this path too.
			return nil, true, ctx.Err()
		case <-tick.C:
			if row, found, err := m.takeFiring(); err != nil {
				return nil, true, err
			} else if found {
				return row, true, nil
			}
			// Belt and braces: a run can only end by writing a firing, so an
			// empty running set with nothing owed means the events were lost
			// (a store failure). Returning beats waiting forever on an answer
			// that will not arrive.
			if left, err := m.Session.RunningJobIDs(); err == nil && len(left) == 0 {
				return nil, false, nil
			}
		}
	}
}

// takeFiring claims the oldest undelivered firing and resolves it to its job,
// marking it delivered in the same step. At-most-once lives here and nowhere
// else: the batch injector of the next step drains the same queue with the same
// claim, which is why a wake cannot be reported twice and why a wait that was
// never called still has its event queued for the next turn boundary.
func (m *Manager) takeFiring() (*session.JobRow, bool, error) {
	events, err := m.Session.PendingJobEvents(1)
	if err != nil {
		return nil, false, err
	}
	if len(events) == 0 {
		return nil, false, nil
	}
	e := events[0]
	if err := m.Session.MarkJobEventsDelivered([]int64{e.ID}); err != nil {
		return nil, false, err
	}
	row, err := m.Session.GetJob(e.JobID)
	if err != nil {
		// A firing whose job row is gone (session deleted under us). Claiming it
		// anyway is right: an event nobody can resolve must not wedge the queue.
		m.logger().Warn("job: firing for an unreadable job", "job", e.JobID, "err", err)
		return nil, false, nil
	}
	return row, true, nil
}

// PendingFirings reports whether anything is owed, which lets the jobs listing
// and the wake injector agree on what has not been delivered yet.
func (m *Manager) PendingFirings() int {
	events, err := m.Session.PendingJobEvents(1)
	if err != nil {
		return 0
	}
	return len(events)
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
