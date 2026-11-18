package host

import (
	"os"
	"testing"
)

func alwaysAlive(int) bool { return true }
func alwaysDead(int) bool  { return false }

// A-6's rule, as a matrix. The interesting rows are the last two: a foreign
// process that is running blocks the resume, and one that has died does not -
// that is the difference between refusing to destroy another driver's live jobs
// and still recovering a session whose process crashed.
func TestResumeBlocked(t *testing.T) {
	const mine, other = 1000, 2000
	cases := []struct {
		name   string
		holder int
		alive  func(int) bool
		want   bool
	}{
		{"unowned", 0, alwaysAlive, false},
		{"our own stale stamp", mine, alwaysAlive, false},
		{"foreign and alive", other, alwaysAlive, true},
		{"foreign and dead", other, alwaysDead, false},
	}
	for _, c := range cases {
		if got := resumeBlocked(c.holder, mine, c.alive); got != c.want {
			t.Errorf("%s: resumeBlocked = %v, want %v", c.name, got, c.want)
		}
	}
}

// A second Open of one session id used to build a second Session object over the
// same transcript - leaving a duplicate in the registry, so Close closed one
// object twice and never reaped the other's jobs. Refusing is the fix; releasing
// on close is what keeps the refusal from becoming a lockout.
func TestOpenRefusesASecondDriverAndAllowsResumeAfterClose(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	first := openSession(t, p, merged, base, 0, nil)

	row, err := p.Store.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Open(Spec{Config: merged, Resume: row, Model: scripted(1)}); err == nil {
		t.Fatal("a second driver of one session was allowed")
	}

	// The refusal must not have half-opened anything: the first handle closes
	// cleanly, releases the row, and only then may the session be resumed.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if holder, err := p.Store.Owner(first.ID.String()); err != nil || holder != 0 {
		t.Fatalf("close left owner %d (err %v), want 0", holder, err)
	}
	row, err = p.Store.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Open(Spec{Config: merged, Resume: row, Model: scripted(2)}); err != nil {
		t.Fatalf("resume after close was refused: %v", err)
	}
	// A fresh session in the same process is never blocked by this rule.
	if resumeBlocked(os.Getpid(), os.Getpid(), alwaysAlive) {
		t.Fatal("our own pid must not block us")
	}
}
