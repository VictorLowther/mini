package ui

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"
)

// TestAskQueueHoldsOneTurnAndKeepsOrder is the queue's own contract: exactly one
// holder at a time, and waiters served in arrival order, because a human reading
// prompts in an unpredictable sequence is a worse interface than a slower one.
func TestAskQueueHoldsOneTurnAndOrder(t *testing.T) {
	var q askQueue

	release, err := q.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if q.waiting() != 0 {
		t.Fatalf("waiting = %d with no waiter yet", q.waiting())
	}

	// Queue three, one at a time, so arrival order is the fact under test rather
	// than a race the scheduler happens to win.
	order := make([]int, 0, 3)
	var mu sync.Mutex
	done := make(chan struct{}, 3)
	for i := 1; i <= 3; i++ {
		i := i
		go func() {
			r, err := q.acquire(context.Background())
			if err != nil {
				t.Errorf("waiter %d: %v", i, err)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			r()
			done <- struct{}{}
		}()
		// Wait for this exact count, not merely "someone": `> 0` is satisfied by
		// an earlier waiter still in line, which would make the arrival order
		// under test a race the scheduler wins instead.
		want := i
		waitFor(t, "the queue to grow", func() bool { return q.waiting() == want })
	}

	if busy := q.waiting(); busy != 3 {
		t.Fatalf("waiting = %d, want all three queued", busy)
	}
	release()
	for i := 0; i < 3; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a queued waiter never got its turn")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Errorf("order = %v, want 1,2,3", order)
	}
	// The queue is reusable, not spent.
	r2, err := q.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r2()
}

// TestAskQueueWaiterCanBeCancelled is why this is not a sync.Mutex: a quit that
// cannot interrupt waiting for an unrelated question is the same class of bug the
// queue exists to fix.
func TestAskQueueWaiterCanBeCancelled(t *testing.T) {
	var q askQueue
	holder, err := q.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := q.acquire(ctx)
		got <- err
	}()
	waitFor(t, "the cancellation target to enqueue", func() bool { return q.waiting() == 1 })

	cancel()
	select {
	case err := <-got:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a cancelled waiter could not leave the queue")
	}
	if q.waiting() != 0 {
		t.Errorf("waiting = %d after the cancel, want 0 - the queue kept a dead ticket", q.waiting())
	}

	// The holder still releases cleanly and the next caller gets in, so the
	// departed waiter left no gap in the handoff chain.
	handed := make(chan struct{})
	go func() {
		r, err := q.acquire(context.Background())
		if err != nil {
			t.Errorf("next acquire: %v", err)
			return
		}
		close(handed)
		r()
	}()
	waitFor(t, "the next waiter to be queued", func() bool { return q.waiting() == 1 })
	holder()
	select {
	case <-handed:
	case <-time.After(3 * time.Second):
		t.Fatal("the queue was left wedged by a cancelled waiter")
	}
}

// TestGUIAsksTakeTurns is the bug from docs/parallel-sessions.md §9.4, written
// against the front end that had it. GUI.Ask used to overwrite g.pending
// unconditionally, so the second call replaced the first one's channel and the
// first goroutine waited for an answer that could never arrive. Three properties
// are asserted: the second question is not published over the first, both get
// their own answers, and neither hangs.
func TestGUIAsksTakeTurns(t *testing.T) {
	q := newTestQueue(t)
	g := NewGUI(GUIOptions{App: test.NewApp(), Post: q.add})
	attachPane(g, q)

	type result struct {
		a   Answer
		err error
	}
	first := make(chan result, 1)
	second := make(chan result, 1)

	go func() {
		a, err := g.Ask(context.Background(), Question{Kind: AskConfirm, Title: "run this?"})
		first <- result{a, err}
	}()
	waitFor(t, "the first question to reach the screen", g.HasQuestion)

	go func() {
		a, err := g.Ask(context.Background(), Question{Kind: AskConfirm, Title: "and this one?"})
		second <- result{a, err}
	}()
	waitFor(t, "the second caller to queue", func() bool { return g.asks.waiting() == 1 })

	// The old code failed exactly here: the slot would already hold the second
	// question, and the first asker would be unreachable.
	if got := g.PendingQuestion().Title; got != "run this?" {
		t.Fatalf("visible question = %q, want the first one still showing", got)
	}

	g.resolve(Answer{Text: "answer-one"})
	select {
	case r := <-first:
		if r.err != nil {
			t.Fatalf("first ask: %v", r.err)
		}
		if r.a.Text != "answer-one" {
			t.Errorf("first got %q, want answer-one", r.a.Text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first Ask never got its answer - the pending-slot clobber is back")
	}

	waitFor(t, "the queued question to become visible", func() bool {
		return g.HasQuestion() && g.PendingQuestion().Title == "and this one?"
	})
	g.resolve(Answer{Text: "answer-two"})
	select {
	case r := <-second:
		if r.err != nil {
			t.Fatalf("second ask: %v", r.err)
		}
		if r.a.Text != "answer-two" {
			t.Errorf("second got %q, want answer-two", r.a.Text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the queued Ask never completed")
	}

	if g.HasQuestion() {
		t.Error("a question is still pending after both were answered")
	}
	if n := g.asks.waiting(); n != 0 {
		t.Errorf("waiting = %d after everyone was answered", n)
	}
}

// TestGUIAskHonoursContextKeepsQueueUsable covers the other half of the fix: an
// asker that gives up mid-question must not leave the queue holding a turn
// nobody will release, which would stall every later question - and it must not
// leave a phantom prompt on screen either.
func TestGUIAskHonoursContextKeepsQueueUsable(t *testing.T) {
	q := newTestQueue(t)
	g := NewGUI(GUIOptions{App: test.NewApp(), Post: q.add})
	attachPane(g, q)

	// An ask whose session is quit while its question is on screen.
	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() {
		_, err := g.Ask(ctx, Question{Kind: AskConfirm, Title: "cancelled mid-question"})
		gaveUp <- err
	}()
	waitFor(t, "the ask to publish its question", g.HasQuestion)
	cancel()

	select {
	case err := <-gaveUp:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an asker that was cancelled could not leave its own question")
	}
	if g.HasQuestion() {
		t.Error("the cancelled question is still on screen")
	}
	if n := g.asks.waiting(); n != 0 {
		t.Errorf("waiting = %d, want 0", n)
	}

	// The turn came back, so the next session gets asked normally. This is the
	// assertion the whole test exists for: the previous one would deadlock here
	// if the abandoned holder had kept the queue busy.
	next := make(chan error, 1)
	go func() {
		_, err := g.Ask(context.Background(), Question{Kind: AskConfirm, Title: "after"})
		next <- err
	}()
	waitFor(t, "the next ask to publish", func() bool {
		return g.HasQuestion() && g.PendingQuestion().Title == "after"
	})
	g.resolve(Answer{Text: "fine"})
	select {
	case err := <-next:
		if err != nil {
			t.Errorf("ask after a cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the queue was left unusable by a cancelled ask")
	}
}
