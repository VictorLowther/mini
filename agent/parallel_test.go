package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"uuid"

	"github.com/VictorLowther/mini/config"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/job"
	"github.com/VictorLowther/mini/model"
	"github.com/VictorLowther/mini/session"
)

// The tests here are the evidence for docs/parallel-sessions.md §4: the loop,
// the interpreter, the registry and the store already tolerate many sessions at
// once, so the prerequisite work is in the wiring rather than the core. They
// are worth keeping as regression tests for exactly that reason - the day a
// `Loop` grows a process-global, one of these fails and says which assumption
// the design doc was written against.

// parallelSession wires one complete session - its own config, interpreter,
// registry and durable transcript - inside a store shared with every other
// session the test is running. This is what host.Open will do for a living
// reason (docs/parallel-sessions.md §5), and doing it by hand here is the
// point: it proves the six objects can be assembled concurrently with nothing
// but arguments.
type parallelSession struct {
	loop *Loop
	sess *session.Session
	dir  string
}

func newParallelSession(t *testing.T, st *session.Store, merged map[string]any, base string, idx int, outputs []model.Message, budget time.Duration) (*parallelSession, error) {
	// One directory per session, which is the whole point: it is the stand-in
	// for the second repository of feature 2 and the shared tree of a subagent.
	repo := filepath.Join(base, fmt.Sprintf("repo%d", idx))
	if err := os.MkdirAll(repo, 0o755); err != nil {
		return nil, err
	}
	sess, err := st.Create(session.Row{ID: uuid.NewV7(), Workdir: repo, Project: repo, ModelName: "test"})
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	// One Config per session, as buildRun does: the interactive mode and the
	// raised limits are mutable run state, so sharing the pointer between
	// sessions would let one run change another's ledger.
	cfg := ConfigFromMap(config.Sub(merged, "agent"))
	cfg.OutputPath = ""
	e := &env.Local{CWD: repo, Timeout: 5 * time.Second}
	logr := slog.New(slog.NewTextHandler(io.Discard, nil))
	jm := &job.Manager{Session: sess, Local: e, Logger: logr}
	jm.Sweep()
	l := &Loop{
		Cfg: cfg, Model: &model.Deterministic{CostPerCall: 1, Outputs: outputs},
		Env: e, Recorder: sess, Jobs: jm, CommandBudget: budget, Logger: logr,
	}
	t.Cleanup(func() { jm.KillAll() })
	return &parallelSession{loop: l, sess: sess, dir: repo}, nil
}

// openParallelStore makes the one store the sessions in these tests share,
// which is the point of them: a second session must not need a second database.
func openParallelStore(t *testing.T) (*session.Store, string) {
	t.Helper()
	base := t.TempDir()
	st, err := session.Open(filepath.Join(base, "sessions.db"), filepath.Join(base, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	return st, base
}

// parallelConfig resolves the shipped config once. Callers still copy it per
// session through ConfigFromMap, because the interactive mode and any raised
// limits are mutable run state.
func parallelConfig(t *testing.T) map[string]any {
	t.Helper()
	merged, err := config.FromSpec("mini")
	if err != nil {
		t.Fatal(err)
	}
	return merged
}

// runAsync starts a Run in a goroutine and hands back its waiter, so a test can
// act on a loop that is genuinely mid-turn rather than one it is standing in
// front of.
func runAsync(ps *parallelSession, task string) func() (map[string]any, error) {
	type outcome struct {
		info map[string]any
		err  error
	}
	ch := make(chan outcome, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		info, err := ps.loop.Run(ctx, task)
		ch <- outcome{info, err}
	}()
	return func() (map[string]any, error) {
		select {
		case o := <-ch:
			return o.info, o.err
		case <-time.After(60 * time.Second):
			return nil, errors.New("the run never finished")
		}
	}
}

// TestParallelSessionsShareOneStore runs four sessions at once in one process,
// in four different directories, against one *session.Store. Three things are
// checked per session, each of which would break a subagent or a second
// top-level session: the file the agent wrote landed in its own repository, the
// read of that file observed its own bytes rather than a neighbour's, and its
// transcript holds its own task and not the next session's.
func TestParallelSessionsShareOneStore(t *testing.T) {
	base := t.TempDir()
	st, err := session.Open(filepath.Join(base, "sessions.db"), filepath.Join(base, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	merged, err := config.FromSpec("mini")
	if err != nil {
		t.Fatal(err)
	}

	const n = 4
	errs := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = func() string {
				ps, err := newParallelSession(t, st, merged, base, i, []model.Message{
					turn("write", fmt.Sprintf("echo marker%d > marker.txt", i)),
					turn("read", "cat marker.txt"),
					done(fmt.Sprintf("done-%d", i)),
				}, 5*time.Second)
				if err != nil {
					return err.Error()
				}
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				if _, err := ps.loop.Run(ctx, fmt.Sprintf("task %d", i)); err != nil {
					return fmt.Sprintf("s%d run: %v", i, err)
				}
				b, err := os.ReadFile(filepath.Join(ps.dir, "marker.txt"))
				if err != nil {
					return fmt.Sprintf("s%d marker: %v", i, err)
				}
				if got := strings.TrimSpace(string(b)); got != fmt.Sprintf("marker%d", i) {
					return fmt.Sprintf("s%d marker crossed over: %q", i, got)
				}
				if obs := lastObservation(ps.loop); !strings.Contains(obs, fmt.Sprintf("marker%d", i)) {
					return fmt.Sprintf("s%d observed another session's output: %q", i, obs)
				}
				msgs, err := ps.sess.Transcript()
				if err != nil {
					return fmt.Sprintf("s%d transcript: %v", i, err)
				}
				var userSeen int
				for _, m := range msgs {
					if fmt.Sprint(m["content"]) == fmt.Sprintf("task %d", (i+1)%n) && (i+1)%n != i {
						return fmt.Sprintf("s%d transcript contains s%d's task", i, (i+1)%n)
					}
					if m["role"] == "user" {
						userSeen++
					}
				}
				if userSeen == 0 {
					return fmt.Sprintf("s%d transcript is empty", i)
				}
				return ""
			}()
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != "" {
			t.Errorf("session %d: %s", i, e)
		}
	}
}

// TestParallelSessionsWakeOwnJobs puts a real background job in each of three
// concurrent sessions and requires each to be woken by its own job's ending and
// never a sibling's. The wake path is what makes a subagent's report arrive, so
// this is the feature-1 prerequisite in miniature: the firing queue has to be
// per-session the same way the registry row already is.
//
// The foreground command in the middle is load-bearing, not padding. Delivery
// happens at step boundaries, and a job still running is not a result owed, so
// a run that submits before its job ends correctly reports nothing - which the
// first version of this test mistook for cross-talk until the timings were
// separated.
func TestParallelSessionsWakeOwnJobs(t *testing.T) {
	base := t.TempDir()
	st, err := session.Open(filepath.Join(base, "sessions.db"), filepath.Join(base, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	merged, err := config.FromSpec("mini")
	if err != nil {
		t.Fatal(err)
	}

	const n = 3
	wakes := make([]string, n)
	errs := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = func() string {
				ps, err := newParallelSession(t, st, merged, base, i, []model.Message{
					turnActions("bg", model.Action{
						Command:     fmt.Sprintf("sleep 0.2; echo woke%d", i),
						Background:  true,
						Description: fmt.Sprintf("soak %d", i),
					}),
					turn("wait", fmt.Sprintf("sleep 1.0; echo waited%d", i)),
					done(fmt.Sprintf("done-%d", i)),
					done(fmt.Sprintf("done-%d after wake", i)),
				}, 5*time.Second)
				if err != nil {
					return err.Error()
				}
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				if _, err := ps.loop.Run(ctx, fmt.Sprintf("task %d", i)); err != nil {
					return fmt.Sprintf("s%d run: %v", i, err)
				}
				var seen []string
				for _, m := range ps.loop.messages {
					if strings.Contains(fmt.Sprint(m["content"]), "woke") {
						seen = append(seen, fmt.Sprint(m["content"]))
					}
				}
				wakes[i] = strings.Join(seen, "\n")
				return ""
			}()
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != "" {
			t.Errorf("session %d: %s", i, errs[i])
			continue
		}
		if !strings.Contains(wakes[i], fmt.Sprintf("woke%d", i)) {
			t.Errorf("session %d never woke on its own job: %q", i, oneLine(wakes[i], 300))
		}
		for k := 0; k < n; k++ {
			if k != i && strings.Contains(wakes[i], fmt.Sprintf("woke%d", k)) {
				t.Errorf("session %d woke on session %d's job", i, k)
			}
		}
	}
}

// TestSecondRunOnALiveLoopIsRefused is the guard docs/parallel-sessions.md
// §3.5.6 says the core was missing: Running() was advisory, so nothing stopped a
// second caller from writing into a transcript that another goroutine was
// appending to. It is tested here rather than in agent_test.go because the only
// way to hit it honestly is to mean it concurrently.
func TestSecondRunOnALiveLoopIsRefused(t *testing.T) {
	st, dir := openParallelStore(t)
	defer st.Close()

	// A turn that blocks long enough for a second caller to arrive, followed by
	// the completion. The command is the wait: the loop is inside Execute, which
	// is where a UI thread would otherwise have punched a second run in.
	ps, err := newParallelSession(t, st, parallelConfig(t), dir, 99, []model.Message{
		turn("slow", "sleep 2; echo finished"),
		done("submitted"),
	}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	firstDone := runAsync(ps, "the task the first caller owns")

	entered := make(chan struct{})
	blocked := make(chan error, 1)
	go func() {
		// The first run publishes Running() before it queries, so wait for that
		// rather than racing it with a sleep.
		for !ps.loop.Running() {
			time.Sleep(5 * time.Millisecond)
		}
		close(entered)
		_, err := ps.loop.Run(context.Background(), "second attempt")
		blocked <- err
	}()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first run never became active")
	}

	select {
	case err := <-blocked:
		if err == nil {
			t.Fatal("the second Run was allowed in while the first was executing")
		}
		if !errors.Is(err, ErrRunInProgress) {
			t.Errorf("second Run err = %v, want ErrRunInProgress", err)
		}
		if !strings.Contains(err.Error(), "QueueMessage") {
			t.Errorf("refusal does not name the supported path: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second Run blocked instead of refusing")
	}

	// And the first run is unharmed: it goes on to its own submission, with the
	// refusal having appended nothing to the history.
	if _, err := firstDone(); err != nil {
		t.Fatalf("the first run was damaged by the refused one: %v", err)
	}
	// The task is the only user turn that is not an observation: mini speaks to
	// the model about tool results in the user role, so role alone is not the
	// measure here and IsToolResult is.
	var user int
	for _, m := range ps.loop.messages {
		if m["role"] == "user" && !model.IsToolResult(m) {
			user++
		}
	}
	if user != 1 {
		t.Errorf("user turns = %d, want the one task; a refused Run must write nothing", user)
	}
}
