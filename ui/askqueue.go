package ui

import (
	"context"
	"sync"
)

// askQueue serializes prompts so a front end shows one question at a time,
// which is what both of them already assume and neither of them enforced
// (docs/parallel-sessions.md §9.4).
//
// GUI.Ask published a question by overwriting g.pending with the caller's
// channel and nothing rejected a second concurrent call, so the first asker's
// channel became unreferenced - resolve only ever answers whoever holds the slot
// now - and that goroutine sat in its select until its run context ended. Not a
// wrong answer: a silent hang, on the goroutine that owns a conversation, with
// somebody else's prompt on the screen. Print.Ask had the same assumption as
// interleaved writes to one output and one shared stdin.
//
// Neither was reachable while a process held exactly one session, which is why
// it survived. It stops being unreachable the moment one session can ask on
// behalf of another, and a queue is the smallest change that makes a second Ask
// well-defined - so the queue is the mechanism, and *what a second question
// should look like* (per-session surfaces, an origin label, unread counts) stays
// a step-7 design question.
//
// It is a ticket queue rather than a sync.Mutex for one reason: a waiter must be
// cancellable. A quit that cannot interrupt a wait for an unrelated question is
// the same class of bug this file exists to fix, and a plain Lock cannot be
// selected over. Tickets also make the order strict FIFO, which a mutex does not
// promise and a human watching prompts reasonably expects.
type askQueue struct {
	mu   sync.Mutex
	busy bool
	// tail holds waiters in arrival order; each is handed ownership by closing
	// its channel, so busy stays true across the handoff rather than toggling.
	tail []chan struct{}
}

// acquire waits for the right to ask exactly one question. The returned release
// must be called once; ctx ending before the turn arrives returns ctx.Err() and
// leaves the queue consistent, which is the whole reason callers may pass a
// cancellable context here.
func (q *askQueue) acquire(ctx context.Context) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ticket := make(chan struct{})

	q.mu.Lock()
	if !q.busy {
		q.busy = true
		q.mu.Unlock()
		return q.releaseFunc(), nil
	}
	q.tail = append(q.tail, ticket)
	q.mu.Unlock()

	select {
	case <-ticket:
		return q.releaseFunc(), nil
	case <-ctx.Done():
		q.mu.Lock()
		for i, w := range q.tail {
			if w == ticket {
				q.tail = append(q.tail[:i], q.tail[i+1:]...)
				q.mu.Unlock()
				return nil, ctx.Err()
			}
		}
		q.mu.Unlock()
		// Not in the queue any more means ownership was handed to us a moment
		// before the cancellation. Take it and give it straight back: pretending
		// we never had it would strand the next waiter behind a lock nobody
		// holds.
		<-ticket
		release = q.releaseFunc()
		release()
		return nil, ctx.Err()
	}
}

// release hands the turn to the next waiter or opens the queue again.
func (q *askQueue) releaseFunc() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			q.mu.Lock()
			if len(q.tail) > 0 {
				next := q.tail[0]
				q.tail = q.tail[1:]
				q.mu.Unlock()
				close(next)
				return
			}
			q.busy = false
			q.mu.Unlock()
		})
	}
}

// waiting reports how many callers are queued behind the current question. It is
// the number a future roster shows ("2 more waiting") and it is exported to the
// package only for that reason - nothing here decides anything with it.
func (q *askQueue) waiting() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.tail)
}
