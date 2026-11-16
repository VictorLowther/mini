package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

// stubJobs is the registry as the front end sees it: a card's answer, changed by
// the test to play a job through its life.
type stubJobs struct {
	mu     sync.Mutex
	cards  map[string]JobCard
	found  map[string]bool
	live   int
	reads  int
	tailOf map[string]string
}

func (s *stubJobs) JobCard(id string) (JobCard, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	c, ok := s.cards[id]
	return c, ok && s.found[id]
}

func (s *stubJobs) LiveJobs() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live, nil
}

func (s *stubJobs) set(id string, c JobCard, found bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cards[id] = c
	s.found[id] = found
}

func newStub() *stubJobs {
	return &stubJobs{cards: map[string]JobCard{}, found: map[string]bool{}, tailOf: map[string]string{}}
}

// The regression this whole change exists for: a call that handed off has no
// exit status, and nothing may print one.
func TestPendingObservationCarriesNoExit(t *testing.T) {
	m := model.Message{"role": "tool", "content": "handed off", "extra": map[string]any{
		"returncode": 0, "pending": true, "job_id": "j1",
		"job_output_path": "/tmp/job.log", "command": "sleep 99",
	}}
	obs, ok := observationOf(m)
	if !ok {
		t.Fatal("not an observation")
	}
	if !obs.Pending || obs.JobID != "j1" {
		t.Fatalf("observation = %+v", obs)
	}
	if obs.HasExit {
		t.Error("a handed-off call read as finished: HasExit is set")
	}
	for _, line := range obs.lines() {
		if strings.Contains(line, "exit") {
			t.Errorf("plain render still claims an exit: %q", line)
		}
	}

	done := model.Message{"role": "tool", "content": "x", "extra": map[string]any{
		"returncode": 0, "pending": false, "command": "echo hi"}}
	if o, _ := observationOf(done); !o.HasExit {
		t.Error("an ordinary completed call lost its exit status")
	}
}

func TestJobCardLinesWhileRunning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job-j1.log")
	if err := os.WriteFile(path, []byte("epoch 1\nepoch 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines := jobCardLines(JobCard{
		ID: "j1", State: "running", Running: true, Elapsed: 41 * time.Second,
		Label: "finetune", Command: "python train.py", OutputPath: path,
		Reason: "still running after 30s",
	}, true)
	got := strings.Join(lines, "\n")
	for _, want := range []string{"job j1 is still running (41s)", "finetune", "why: still running after 30s", "output: " + path, "epoch 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("running card missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "exit") {
		t.Errorf("a running card claimed an exit:\n%s", got)
	}
	if strings.Contains(got, "no output yet") {
		t.Errorf("a card with output said it had none:\n%s", got)
	}
}

func TestJobCardLinesAfterExit(t *testing.T) {
	code := 3
	lines := jobCardLines(JobCard{
		ID: "j1", State: "failed", ExitCode: &code, Elapsed: 4 * time.Second,
		OutputPath: "/nope", Reason: "exited 3",
	}, true)
	got := strings.Join(lines, "\n")
	if !strings.Contains(got, "job j1 failed after 4s (exit 3)") {
		t.Errorf("ended card = %q", got)
	}
	if strings.Contains(got, "still running") {
		t.Errorf("an ended job still reads as running:\n%s", got)
	}

	// A killed run has no exit code, and saying so is the honest shape.
	killed := strings.Join(jobCardLines(JobCard{
		ID: "j2", State: "killed", Elapsed: 12 * time.Second, OutputPath: "/nope",
	}, true), "\n")
	if !strings.Contains(killed, "job j2 killed after 12s") || strings.Contains(killed, "exit") {
		t.Errorf("killed card = %q", killed)
	}

	// Swept at a restart: no spinner, and no suggestion that it is still ours.
	gone := strings.Join(jobCardLines(JobCard{ID: "j3"}, false), "\n")
	if !strings.Contains(gone, "no longer tracked") {
		t.Errorf("untracked card = %q", gone)
	}
}

// Without a registry attached - print mode, a replayed session - the card still
// has to refuse the lie.
func TestPendingCardWithoutReader(t *testing.T) {
	got := strings.Join(pendingCardLines(observation{
		JobID: "j1", Pending: true, Path: "/tmp/job.log", Command: "sleep 99",
	}, nil), "\n")
	if !strings.Contains(got, "still running when it returned") || !strings.Contains(got, "/tmp/job.log") {
		t.Errorf("no-reader card = %q", got)
	}
	if strings.Contains(got, "exit") {
		t.Errorf("a card with no job state invented an exit:\n%s", got)
	}
}

func TestMessageBlockTracksTheJob(t *testing.T) {
	m := model.Message{"role": "tool", "content": "note", "extra": map[string]any{
		"returncode": 0, "pending": true, "job_id": "j1", "command": "sleep 99",
	}}
	stub := newStub()
	stub.set("j1", JobCard{ID: "j1", State: "running", Running: true, OutputPath: "/tmp/j1.log"}, true)
	b, show, _ := messageBlock(m, "hdr", "task", func() {}, stub)
	if !show {
		t.Fatal("the card was hidden")
	}
	if b.job != "j1" {
		t.Errorf("block does not track the job: %+v", b.job)
	}
	if !strings.Contains(strings.Join(b.transcript, "\n"), "still running") {
		t.Errorf("transcript = %q", b.transcript)
	}
}

// The live half: the card follows the job from running to ended without any new
// message arriving, and the ticker retires itself once nothing is tracked.
func TestJobCardRefreshesUntilTheJobEnds(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{Timeout: 10 * time.Second}}
	p := newGUIForTest(t, c, l)
	stub := newStub()
	path := filepath.Join(t.TempDir(), "j1.log")
	if err := os.WriteFile(path, []byte("line one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub.set("j1", JobCard{ID: "j1", State: "running", Running: true, OutputPath: path, Elapsed: time.Second}, true)
	stub.live = 1
	p.GUI.jobs = stub

	p.do(func() {
		p.v.appendBlock(block{kind: "tool", job: "j1", transcript: jobCardLines(JobCard{
			ID: "j1", State: "running", Running: true, OutputPath: path, Elapsed: time.Second,
		}, true)})
	})
	// The footer counts what is live, which is the part that stays true even
	// when the transcript is scrolled away from the card itself.
	if stats := p.GUI.RunStats(); !strings.Contains(stats, "1 job") {
		t.Errorf("footer = %q", stats)
	}

	// While the job runs, a refresh reports it as still worth ticking, and the
	// card grows with the log.
	p.do(func() {
		if err := os.WriteFile(path, []byte("line one\nline two\n"), 0o600); err != nil {
			t.Error(err)
		}
		if !p.v.refreshJobCards() {
			t.Error("refresh said nothing was running while the job was")
		}
		text := committedText(p.v)
		if !strings.Contains(text, "still running") || !strings.Contains(text, "line two") {
			t.Errorf("card did not follow the job:\n%s", text)
		}
	})

	// The job ends. The card flips to the outcome and the ticker's owner learns
	// it can stop.
	code := 0
	p.do(func() {
		stub.set("j1", JobCard{ID: "j1", State: "completed", ExitCode: &code, OutputPath: path, Elapsed: 9 * time.Second}, true)
		stub.live = 0
		if p.v.refreshJobCards() {
			t.Error("refresh kept ticking after the only job ended")
		}
		text := committedText(p.v)
		if !strings.Contains(text, "job j1 completed after") || !strings.Contains(text, "(exit 0)") {
			t.Errorf("card never showed the outcome:\n%s", text)
		}
		if strings.Contains(text, "still running") {
			t.Errorf("the card still claims the job is running:\n%s", text)
		}
	})
	if stats := p.GUI.RunStats(); strings.Contains(stats, "job") {
		t.Errorf("footer still counts jobs: %q", stats)
	}
	// Every refresh re-read the registry rather than caching a status string.
	if stub.reads == 0 {
		t.Error("the card never asked the registry anything")
	}
}

// committedText joins every committed card's transcript: the model the renderer
// reads. Asserting here rather than on widgets keeps the test about whether the
// card was refreshed, which is the change; how a refreshed card is painted is
// already covered by the transcript tests.
func committedText(v *guiView) string {
	var b strings.Builder
	for _, blk := range v.committed {
		b.WriteString(strings.Join(blk.transcript, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

func TestJobCardLinesBoundsTheTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.log")
	var b strings.Builder
	for i := 0; i < model.MaxViewLines*3; i++ {
		fmt.Fprintf(&b, "progress line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	lines := jobCardLines(JobCard{ID: "j1", State: "running", Running: true, OutputPath: path}, true)
	// A training run printing a progress bar must not turn its card into the
	// whole log: the card is capped by the same window the model is given.
	if len(lines) > model.MaxViewLines+8 {
		t.Errorf("card grew with the log: %d lines", len(lines))
	}
	if !strings.Contains(strings.Join(lines, "\n"), fmt.Sprintf("progress line %d", model.MaxViewLines*3-1)) {
		t.Error("the card shows the head of the log instead of where the job is now")
	}
}
