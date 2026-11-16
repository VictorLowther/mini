package model

import (
	"context"
	"sync"
	"time"
)

// DefaultMaxConcurrentRequests is how many chat completions one mini process
// may have in flight at once, absent any other opinion.
//
// The bound exists because mini serves one model from one box: parallel agents
// divide throughput rather than adding it, and N sessions each streaming a long
// generation is how every one of them becomes slow at once. Four is the
// starting value - enough that a subagent fan-out and a human's own turn are not
// permanently crosswise, low enough that the queue is short and legible when
// they are (docs/parallel-sessions.md §5.7, §9.1).
//
// A slot is held for the whole request *and response*, streaming included, so
// the number means "answers in progress", not "sockets open". That is the
// property worth having on a GPU - it bounds the work actually being done - and
// it is why a 40-minute reasoning trace holds a slot for 40 minutes, which was
// ruled an accepted consequence rather than a problem to schedule around.
const DefaultMaxConcurrentRequests = 4

// Gate is a resizable bound on concurrent work, cancellable by context.
//
// Two properties make it a Gate rather than a buffered channel, and both come
// from being shared by every session in the process. It is **resizable**,
// because the value is a stored preference a human can change at any time and
// an unbounded channel cannot shrink; and a wait is **selectable on ctx**,
// because a quit that hangs behind a queued request is a bug - the same trap
// zero documents as "a launcher that blocks in Launch until its context is
// cancelled would deadlock" (swarm/lifecycle.go:38-49). A channel semaphore
// gets the first wrong and the second for free, which is why this is a counter
// plus a broadcast token instead.
//
// Waiting is deliberate and it is the whole reason this can be a bound at all:
// a background job's result arrives by push at a step boundary, so a queued
// request costs the model nothing, where refusing it would cost a turn and a
// confusing error (docs/parallel-sessions.md §3.6.2).
type Gate struct {
	mu    sync.Mutex
	limit int
	busy  int
	// waiting is a gauge for the UI and the log, never a control path: it is
	// decremented on every exit from the wait loop, including cancellation.
	waiting int
	// token is closed whenever a slot frees or the limit rises, and replaced.
	// Waiters select on the token they were handed, so a release wakes them
	// without a per-waiter channel and a spurious wakeup is just another trip
	// round the loop.
	token chan struct{}
}

// NewGate returns a Gate allowing n concurrent holders. n < 1 means
// DefaultMaxConcurrentRequests.
func NewGate(n int) *Gate {
	if n < 1 {
		n = DefaultMaxConcurrentRequests
	}
	return &Gate{limit: n, token: make(chan struct{})}
}

// DefaultGate is the process-wide bound. A Sushi uses it unless another is
// supplied, so a caller cannot opt out of the limit by forgetting to wire one
// in - which is the only way a bound on a single GPU is worth anything
// (docs/parallel-sessions.md §5.7).
var DefaultGate = NewGate(DefaultMaxConcurrentRequests)

// Acquire takes a slot, waiting for one if necessary, and hands back the
// release. The release is idempotent, because the honest way to use it is a
// defer on a path that also returns early. ctx.Err() ends the wait and nothing
// is taken.
func (g *Gate) Acquire(ctx context.Context) (release func(), err error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		g.mu.Lock()
		if g.busy < g.limit {
			g.busy++
			g.mu.Unlock()
			return g.hold(), nil
		}
		t := g.token
		g.waiting++
		g.mu.Unlock()

		select {
		case <-t:
			g.unwait()
		case <-ctx.Done():
			// The slot was never taken, but the wakeup that would have moved
			// this waiter may never come, so leave the gauge honest.
			g.unwait()
			return nil, ctx.Err()
		}
	}
}

func (g *Gate) unwait() {
	g.mu.Lock()
	g.waiting--
	g.mu.Unlock()
}

// hold returns the release for one taken slot. sync.Once is not paranoia: a
// double release would let one request occupy a slot that nobody holds, which
// permanently shrinks the bound and is exactly the kind of slow leak that is
// invisible until a session stops making progress.
func (g *Gate) hold() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if g.busy > 0 {
				g.busy--
			}
			g.mu.Unlock()
			g.bump()
		})
	}
}

// bump wakes everyone waiting, because either a slot opened or the ceiling moved
// under them. Replacing the token after closing it is what keeps a waiter from
// selecting on an already-closed channel and spinning.
func (g *Gate) bump() {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.token)
	g.token = make(chan struct{})
}

// SetLimit moves the ceiling at runtime. Raising it wakes whoever was parked
// underneath the old one; lowering it never preempts a request already running -
// the box does not get faster by asking, and killing an in-flight generation to
// obey a setting would be the harness losing an argument with the user's own
// patience. Busy simply drains back down to the new limit.
func (g *Gate) SetLimit(n int) {
	if n < 1 {
		n = DefaultMaxConcurrentRequests
	}
	g.mu.Lock()
	changed := g.limit != n
	g.limit = n
	g.mu.Unlock()
	if changed {
		g.bump()
	}
}

// Limit reports the current ceiling.
func (g *Gate) Limit() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit
}

// Stats reports how many slots are held and how many callers are waiting. The
// pair is what a UI needs: occupancy alone cannot tell a saturated box from an
// idle one, and qwen's reason for exposing `claimed/cap` rather than a queue
// depth is that the depth is not the thing a user is deciding about
// (docs/parallel-sessions.md §3.3).
func (g *Gate) Stats() (busy, waiting, limit int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.busy, g.waiting, g.limit
}

// GateWait is what Sushi.OnGateWait reports about a request that had to queue.
type GateWait struct {
	Waited  time.Duration
	Busy    int
	Waiting int
	Limit   int
}
