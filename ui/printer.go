package ui

import (
	"sync"
	"time"
)

// printer writes finished transcript lines above the pane, into the terminal's
// own scrollback.
//
// It exists because printing cannot happen where the lines are produced. The
// transcript is committed inside bubbletea's Update, and Update runs *on* the
// event loop; program.Printf hands off to that same loop over an unbuffered
// channel, so calling it from Update would deadlock the program exactly like
// program.Send does. One goroutine therefore drains an unbounded queue and owns
// all Printf calls, which keeps lines in order without ever blocking the loop.
type printer struct {
	// out is program.Printf in production, or a recorder in tests. clear is
	// program.Send(tea.ClearScreen()).
	out   func(string)
	clear func()
	// sync delivers immediately: with no event loop running (tests drive the
	// model directly) there is nothing to hand off to and nothing to wait for.
	sync bool

	mu    sync.Mutex
	queue []func()
	wake  chan struct{}
}

func newPrinterWith(out func(string), clear func()) *printer {
	p := &printer{out: out, clear: clear, wake: make(chan struct{}, 1)}
	go p.run()
	return p
}

func newSyncPrinter(out func(string), clear func()) *printer {
	return &printer{out: out, clear: clear, sync: true}
}

// push queues a write. Tasks are closures so a print and the clear that must
// precede it stay in order.
func (p *printer) push(fn func()) {
	if p == nil {
		return
	}
	if p.sync {
		fn()
		return
	}
	p.mu.Lock()
	p.queue = append(p.queue, fn)
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Print appends a block above the pane. It never blocks the caller: the queue is
// unbounded on purpose, so a caller holding the update loop cannot stall on a
// terminal that is behind.
func (p *printer) Print(s string) {
	if s == "" {
		return
	}
	p.push(func() { p.out(s) })
}

// reprint replaces the printed history: the screen is cleared and `s` written in
// its place. It exists for the thinking toggle, which changes how *past* turns
// render and cannot rewrite lines the terminal already has otherwise.
func (p *printer) reprint(s string) {
	p.push(func() {
		if p.clear != nil {
			p.clear()
		}
		if s != "" {
			p.out(s)
		}
	})
}

func (p *printer) run() {
	for range p.wake {
		for {
			p.mu.Lock()
			if len(p.queue) == 0 {
				p.mu.Unlock()
				break
			}
			fn := p.queue[0]
			p.queue = p.queue[1:]
			p.mu.Unlock()
			fn()
		}
	}
}

// pending reports how many lines are still queued.
func (p *printer) pending() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue)
}

// flush waits for the queue to empty so the last lines of a session reach the
// terminal before the program quits and Printf would have no reader. Only safe
// off the update loop (it is used from a command).
func (p *printer) flush(d time.Duration) {
	if p == nil || p.sync {
		return
	}
	deadline := time.Now().Add(d)
	for p.pending() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}
