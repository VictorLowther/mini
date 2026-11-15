package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uuid"

	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/job"
	"github.com/VictorLowther/mini/model"
	"github.com/VictorLowther/mini/session"
)

// deadlineEnv is the interpreter's shadow: it records the budget the loop put on
// each command's context, which is the only way a per-call timeout reaches an
// Environment whose Execute signature carries no timeout.
type deadlineEnv struct{ budgets []time.Duration }

func (d *deadlineEnv) Execute(ctx context.Context, command, cwd string, sink io.Writer) (env.Result, error) {
	if dl, ok := ctx.Deadline(); ok {
		d.budgets = append(d.budgets, time.Until(dl).Round(100*time.Millisecond))
	} else {
		d.budgets = append(d.budgets, 0)
	}
	fmt.Fprintln(sink, "ran")
	return env.Result{ReturnCode: 0}, nil
}

func (d *deadlineEnv) TemplateVars() map[string]any { return nil }
func (d *deadlineEnv) Serialize() map[string]any    { return nil }

func newJobLoop(t *testing.T, m model.Model, e env.Environment) (*Loop, *job.Manager) {
	t.Helper()
	dir := t.TempDir()
	st, err := session.Open(filepath.Join(dir, "sessions.db"), filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.Create(session.Row{ID: uuid.NewV7(), Workdir: dir, Project: dir, ModelName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	l, _ := newRecorderLoop(t, m, e)
	jm := &job.Manager{Session: sess, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	l.Jobs = jm
	t.Cleanup(func() { jm.KillAll(); st.Close() })
	return l, jm
}

// observation returns the loop's tool-result messages, oldest first.
func observations(l *Loop) []string {
	var out []string
	for _, msg := range l.Messages {
		if model.IsToolResult(msg) {
			out = append(out, fmt.Sprint(msg["content"]))
		}
	}
	return out
}

func TestBackgroundReturnsAHandoffInsteadOfWaiting(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("start the soak", model.Action{
			Command: "sleep 30; echo should-never-be-seen", Background: true}),
		done("submitted"),
	}}
	l, jm := newJobLoop(t, m, &env.Local{Timeout: 5 * time.Second})

	start := time.Now()
	if _, err := run(t, l, "start a soak test"); err != nil {
		t.Fatal(err)
	}
	// The whole point of the parameter: a 30-second command must not make the
	// turn take 30 seconds.
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("the backgrounded call blocked the loop for %v", d)
	}

	obs := observations(l)
	if len(obs) == 0 {
		t.Fatal("no observation recorded")
	}
	last := obs[len(obs)-1]
	for _, want := range []string{"started in the background as job j1", "job-j1.log", "tail -n 100", "killed when the session closes"} {
		if !strings.Contains(last, want) {
			t.Errorf("handoff missing %q:\n%s", want, last)
		}
	}
	if strings.Contains(last, "should-never-be-seen") || strings.Contains(last, "ran\n") {
		t.Errorf("the backgrounded command's output leaked into the observation: %s", last)
	}

	// The job is real, running, and recorded in the session - not just mentioned
	// in text the model will eventually lose to compaction.
	list, err := jm.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "j1" || list[0].State != session.JobRunning {
		t.Fatalf("registry = %+v", list)
	}
	if list[0].TCNO == nil || *list[0].TCNO != 1 {
		t.Errorf("job did not record the call that started it: %+v", list[0])
	}
	if _, err := os.Stat(list[0].OutputPath); err != nil {
		t.Errorf("job output file: %v", err)
	}
	// And the call's own result file holds the harness's note, which is what the
	// transcript will keep pointing at.
	b, err := os.ReadFile(jm.Session.JobPath("j1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 0 {
		t.Logf("job log so far: %q", b)
	}
}

func TestBackgroundWithoutARegistryReportsInsteadOfRunningForeground(t *testing.T) {
	// A Loop built without a registry (a test harness, or a front end that has
	// not wired one) must refuse the request rather than quietly run a 30-second
	// command in the foreground and pretend it was backgrounded.
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("try to background", model.Action{Command: "echo nope", Background: true}),
		done("submitted"),
	}}
	l, _ := newRecorderLoop(t, m, &env.Local{Timeout: 5 * time.Second})
	if _, err := run(t, l, "background this"); err != nil {
		t.Fatal(err)
	}
	obs := observations(l)
	last := obs[len(obs)-1]
	if !strings.Contains(last, "no job registry") {
		t.Errorf("observation = %s", last)
	}
	if strings.Contains(last, "nope") {
		t.Errorf("the command ran anyway: %s", last)
	}
}

func TestTimeoutSReachesTheCommandAsADeadline(t *testing.T) {
	d := &deadlineEnv{}
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("ask for a big budget", model.Action{Command: "make", TimeoutS: 7}),
		turnActions("and then do not", model.Action{Command: "ls"}),
		done("submitted"),
	}}
	l := newLoop(t, m, d)
	if _, err := run(t, l, "build then list"); err != nil {
		t.Fatal(err)
	}
	if len(d.budgets) != 2 {
		t.Fatalf("commands = %d", len(d.budgets))
	}
	// 7s asked for, and the budget arrives as a deadline on the context rather
	// than as an argument the Environment would have to know about.
	if d.budgets[0] < 6*time.Second || d.budgets[0] > 7*time.Second {
		t.Errorf("timeout_s budget = %v, want ~7s", d.budgets[0])
	}
	// No parameter means no opinion, which leaves the environment's default in
	// charge - the loop must not invent a deadline of its own.
	if d.budgets[1] != 0 {
		t.Errorf("unbudgeted command got a deadline of %v", d.budgets[1])
	}
}
