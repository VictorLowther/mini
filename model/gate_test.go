package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestGateBoundsConcurrentHolders(t *testing.T) {
	g := NewGate(3)
	var mu sync.Mutex
	var busy, peak int
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := g.Acquire(context.Background())
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			mu.Lock()
			busy++
			if busy > peak {
				peak = busy
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			busy--
			mu.Unlock()
			release()
		}()
	}
	wg.Wait()
	if peak > 3 {
		t.Errorf("peak concurrency = %d, want <= the limit of 3", peak)
	}
	if peak < 2 {
		t.Errorf("peak concurrency = %d; a gate that never lets two through is a queue, not a bound", peak)
	}
	if b, w, _ := g.Stats(); b != 0 || w != 0 {
		t.Errorf("after the storm: busy=%d waiting=%d, want 0/0 - a slot or a gauge leaked", b, w)
	}
}

// TestGateWaitIsCancellable is the property that keeps `mini` quittable. A quit
// that hangs behind a queued request is the deadlock zero documents as the
// reason its admission protocol is shaped the way it is
// (swarm/lifecycle.go:38-49), and a plain channel semaphore would give it away.
func TestGateWaitIsCancellable(t *testing.T) {
	g := NewGate(1)
	hold, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := g.Acquire(ctx)
		done <- err
	}()

	// Let the waiter park, then cancel.
	time.Sleep(20 * time.Millisecond)
	_, waiting, _ := g.Stats()
	if waiting != 1 {
		t.Fatalf("waiting = %d, want 1", waiting)
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the cancelled acquire got a slot")
		}
		if ctx.Err() == nil || err != context.Canceled {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled acquire did not return - quitting mini would hang here")
	}

	// The cancelled waiter took nothing, so releasing the holder must let the
	// next caller in immediately.
	hold()
	got := make(chan struct{})
	go func() {
		r, err := g.Acquire(context.Background())
		if err != nil {
			t.Errorf("acquire after cancel: %v", err)
			return
		}
		close(got)
		r()
	}()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled waiter left the gate wedged")
	}
	if _, w, _ := g.Stats(); w != 0 {
		t.Errorf("waiting = %d after everyone left, want 0", w)
	}
}

func TestGateResizeWakesWaitersButNeverPreempts(t *testing.T) {
	g := NewGate(1)
	hold, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan struct{})
	go func() {
		r, err := g.Acquire(context.Background())
		if err != nil {
			return
		}
		close(acquired)
		defer r()
		time.Sleep(10 * time.Millisecond)
	}()
	time.Sleep(20 * time.Millisecond)

	// Raising the ceiling must wake the waiter without the holder releasing.
	g.SetLimit(2)
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("raising the limit did not wake a parked waiter")
	}

	// Lowering it must not disturb work already running - the box does not get
	// faster by asking, and killing an in-flight generation to obey a setting is
	// the harness losing to the user's own patience.
	if busy, _, limit := g.Stats(); busy < 1 || limit != 2 {
		t.Fatalf("stats during the overlap = %+v", []int{busy, limit})
	}
	g.SetLimit(1)
	if _, _, limit := g.Stats(); limit != 1 {
		t.Errorf("limit = %d, want 1", limit)
	}
	hold() // the running request finishes on its own schedule
}

// TestGateReleaseIsIdempotent guards the slow kind of leak: one request
// releasing twice frees a slot it no longer occupies, and the bound quietly
// stops meaning anything - a bug that shows up as a box being oversubscribed
// weeks later, not as a failure.
func TestGateReleaseIsIdempotent(t *testing.T) {
	g := NewGate(1)
	hold, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	hold()
	hold()
	hold()
	if busy, _, _ := g.Stats(); busy != 0 {
		t.Errorf("busy = %d after three releases, want 0", busy)
	}
	// And the gate still admits exactly one at a time.
	first, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := g.Acquire(ctx); err == nil {
		t.Error("a second holder got in while the limit was 1")
	}
	first()
}

// TestSushiCapsConcurrentRequests is the end-to-end proof that the knob is real:
// a server that counts how many requests it is serving at once, eight queries
// fired concurrently, and a gate of two. Anything the client does with the slot
// - releasing early, holding it through a backoff, forgetting one path - shows up
// here as a peak the server can see.
func TestSushiCapsConcurrentRequests(t *testing.T) {
	const limit = 2
	var mu sync.Mutex
	serving, peak := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		serving++
		if serving > peak {
			peak = serving
		}
		mu.Unlock()

		time.Sleep(40 * time.Millisecond) // the "generation"

		mu.Lock()
		serving--
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write(fixture("submit.sse"))
	}))
	defer srv.Close()

	g := NewGate(limit)
	s := &Sushi{Model: "test-model", BaseURL: srv.URL, Gate: g}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Query(context.Background(), []Message{{"role": "user", "content": "go"}}, nil); err != nil {
				t.Errorf("query: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	observed := peak
	mu.Unlock()
	if observed > limit {
		t.Errorf("the server saw %d concurrent requests with the gate set to %d", observed, limit)
	}
	if observed < 2 {
		t.Errorf("peak = %d; nothing was ever concurrent, which means the test proved nothing", observed)
	}
	if busy, waiting, _ := g.Stats(); busy != 0 || waiting != 0 {
		t.Errorf("gate left dirty: busy=%d waiting=%d", busy, waiting)
	}
}

// TestSushiDefaultsToTheSharedGate is the unskippable half of the ruling: a
// client built without a gate joins the process-wide bound rather than running
// without one, so the way to escape the limit is not "forget to pass it".
func TestSushiDefaultsToTheSharedGate(t *testing.T) {
	if got := (&Sushi{Model: "m"}).gate(); got != DefaultGate {
		t.Error("a Sushi with no Gate did not fall back to DefaultGate")
	}
	g := NewGate(7)
	if got := (&Sushi{Model: "m", Gate: g}).gate(); got != g {
		t.Error("an explicit Gate was ignored")
	}
	if DefaultGate.Limit() != DefaultMaxConcurrentRequests {
		t.Errorf("DefaultGate limit = %d, want %d", DefaultGate.Limit(), DefaultMaxConcurrentRequests)
	}
}
