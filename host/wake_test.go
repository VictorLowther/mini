package host

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/VictorLowther/mini/job"
	"github.com/VictorLowther/mini/session"
)

// The wake has to be wired by the assembly path, not merely available: every
// session this process opens is a session that can be parked at a prompt, and a
// parked prompt is only useful if the registry can reach it. This is the test
// that fails if someone moves the job registry, or builds a session without
// going through Open.
func TestOpenWiresTheFiringWake(t *testing.T) {
	p, base := newProcess(t)
	s := openSession(t, p, testConfig(t), base, 0, nil)

	if s.Jobs.OnFiring == nil {
		t.Fatal("Open left the registry with no route to a parked session")
	}

	release := make(chan struct{})
	if _, err := s.Jobs.Start(job.Spec{
		Command: "held open by the test",
		Label:   "waker",
		Kind:    session.KindSubagent,
		Run: func(ctx context.Context, out io.Writer) job.Result {
			<-release
			code := 0
			return job.Result{ExitCode: &code, Reason: "reported"}
		},
	}); err != nil {
		t.Fatal(err)
	}
	if s.Loop.OwedDeliveries() != 0 {
		t.Fatal("nothing has ended yet, so nothing is owed")
	}

	close(release)

	deadline := time.After(10 * time.Second)
	for {
		// The debt is the truth and the signal is only the hint; both must
		// arrive, in that order, for a parked prompt to act on them.
		if s.Loop.OwedDeliveries() > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the job ended but its result was never owed to the session")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	select {
	case <-s.Loop.WorkSignal():
	case <-time.After(10 * time.Second):
		t.Fatal("a written firing never raised the work signal")
	}
	if !s.Loop.PendingWork() {
		t.Fatal("the session cannot see that it owes itself a step")
	}
}
