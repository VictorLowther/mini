package agent

import (
	"context"

	"github.com/VictorLowther/mini/model"
)

// The "there is a step to take" signal, and the questions a waiter asks with it.
//
// Both halves of the harness's debt are *armed* work: something outside the loop
// writes a row (a job's exit firing) or sets a flag (a requested compaction), and
// the loop is meant to discharge it at the next safe step boundary. The boundary
// was always there. What was missing is a way to notice the work arrived while
// the loop is parked - and a parked loop is not an exotic state, it is the two
// places a session spends most of its idle life: blocked in Hooks.OnSubmit at the
// "Agent is ready" prompt, and blocked in Hooks.NextHumanCommand in human mode.
//
// Those waits run *inside* Run, on the agent goroutine, which is what made the
// miss so total: the step-boundary drains in Run (the jobWake at the top of each
// turn and the deliverOwed before an exit breaks the loop) are downstream of the
// wait, so an idle session holding an undelivered result never reaches them. The
// firing sits in a durable queue, the queue is the source of truth that a job is
// over, and the only thing that drained it was the next keystroke. A subagent
// that finished in ninety seconds reported twenty minutes later, when a human
// happened to type.
//
// So the signal is a hint and the truth stays in the store: NotifyWork says
// "ask again", PendingWork is the answer, and a waiter that acts on the hint
// without checking the truth will simply go back to sleep. That ordering is what
// makes a spurious or duplicate notify harmless, which matters because a notify
// comes from a watcher goroutine that cannot know whether anyone is listening.
//
// Queued steering messages (QueueMessage) are deliberately NOT part of this. They
// already have a delivery point that works - the top of every Query - and a run
// parked at the ready prompt has no use for one more way to start a turn it was
// not asked for. This signal carries the harness's own debt and nothing else.

// NotifyWork tells a waiting front end that the harness has armed work for this
// loop and a blocked prompt should stop blocking. Safe from any goroutine; never
// blocks, so a watcher recording a job's last word cannot be delayed by a UI that
// is not reading.
func (l *Loop) NotifyWork() {
	l.mu.Lock()
	ch := l.work
	if ch == nil {
		ch = make(chan struct{}, 1)
		l.work = ch
	}
	l.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
		// Already pending. A full channel means "there is an unread event",
		// which is all a waiter needs; the count is never read from here.
	}
}

// WorkSignal returns the channel a waiter selects on alongside its own sources.
// It always returns a usable channel, because a select on a nil channel simply
// never fires, and a waiter that silently cannot be woken is this file's whole
// reason to exist.
func (l *Loop) WorkSignal() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.work == nil {
		l.work = make(chan struct{}, 1)
	}
	return l.work
}

// ClearWorkSignal drops a hint that has been acted on, so a waiter re-arming the
// same signal in a loop does not spin. It does not change what is owed: the debt
// lives in the job queue and in manualCompact, and those are retired by delivering
// them, not by draining a channel.
func (l *Loop) ClearWorkSignal() {
	l.mu.Lock()
	ch := l.work
	l.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case <-ch:
	default:
	}
}

// OwedDeliveries is the harness's side of the job state machine: finished jobs
// whose result has not reached the conversation. It is a count and not a bool
// because a waiter that wakes to deliver one firing may as well know that three
// arrived while it was getting there.
func (l *Loop) OwedDeliveries() int {
	if l.Jobs == nil {
		return 0
	}
	return l.Jobs.OwedDeliveries()
}

// CompactionPending reports whether a compaction has been requested and not yet
// run. Read under mu: RequestCompaction comes from the UI goroutine.
func (l *Loop) CompactionPending() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.manualCompact
}

// PendingWork is the truth behind the hint: is there an armed step the loop has
// not discharged? This is the question a blocked prompt asks when the signal
// fires, and the answer a waiter must re-check before it takes a turn.
func (l *Loop) PendingWork() bool {
	return l.OwedDeliveries() > 0 || l.CompactionPending()
}

// DeliverOwedNow discharges the job debt into the history from a hook, which is
// the agent goroutine and therefore the only goroutine allowed to append. It is
// the exported half of deliverOwed, for the one caller that cannot wait for the
// loop's next boundary because it *is* the boundary: human mode, whose Query
// returns early on a user command and so never passes the drain at the top of the
// turn.
func (l *Loop) DeliverOwedNow() bool { return l.deliverOwed() }

// CompactHere runs an armed compaction at the point it was asked for.
//
// The safe step boundary is the thing compaction has to wait for, not the next
// model call: a context between turns, with no streamed request in flight and the
// whole transcript settled, is exactly as safe as the first boundary inside
// Query. Recognising that is what makes /compact mean "now" when the session is
// idle instead of "sometime, if another turn ever happens" - and it costs the user
// nothing, because the side-query rides the same warm prefix either way.
//
// Call only from the agent goroutine (a hook), since it appends to the history.
// It reports whether a compaction row was written; every failure path leaves the
// history exactly as it was, as everywhere else in this file.
func (l *Loop) CompactHere(ctx context.Context) bool {
	proj := model.ProjectContext(l.messages)
	est, _ := model.ContextTokens(proj)
	// Consume the request first: a compaction that cannot run must not fire the
	// waiter again in a loop, and the auto path still has its own trigger.
	l.consumeCompactionRequest()
	if !l.compact(ctx, proj, est, true) {
		// compact() re-arms an explicit request on its failure paths, which is
		// right for a running loop - the next turn retries it - and a hot spin
		// here: this boundary *is* the retry, so an armed flag we just acted on
		// would wake the same prompt again immediately, fail again, and re-arm
		// forever. Consume it again and leave the automatic trigger, or a fresh
		// /compact, to be the retry.
		l.consumeCompactionRequest()
		return false
	}
	l.ClearWorkSignal()
	return true
}

func (l *Loop) consumeCompactionRequest() {
	l.mu.Lock()
	l.manualCompact = false
	l.mu.Unlock()
}
