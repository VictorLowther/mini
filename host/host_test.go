package host

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

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/config"
	"github.com/VictorLowther/mini/job"
	"github.com/VictorLowther/mini/model"
	"github.com/VictorLowther/mini/session"
)

// newProcess points the state directory at a temp dir and opens the store and a
// Process over it, the way cmd/mini does.
func newProcess(t *testing.T) (*Process, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("MINI_STATE_DIR", base)
	st, err := session.Open(filepath.Join(base, "sessions.db"), filepath.Join(base, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := New(st, context.Background())
	p.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	// Reap the process, not just the store. A test that spawned anything leaves a
	// child run in flight, and its goroutine keeps writing trajectory and result
	// files under t.TempDir() - which then fails cleanup with "directory not
	// empty" whenever the child wins the race against the test's last assertion.
	// Children close before parents, once, with a shared deadline (§9.2).
	t.Cleanup(func() { _ = p.Close() })
	return p, base
}

func testConfig(t *testing.T) map[string]any {
	t.Helper()
	merged, err := config.FromSpec("mini")
	if err != nil {
		t.Fatal(err)
	}
	return merged
}

// scripted returns the model-injection config and the outputs one session will
// replay: write a marker into its own directory, read it back, submit.
func scripted(i int) model.Model {
	return &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn(fmt.Sprintf("echo marker%d > marker.txt", i)),
		turn("cat marker.txt"),
		done(fmt.Sprintf("report-%d", i)),
	}}
}

func turn(cmd string) model.Message {
	return model.Message{"role": "assistant", "content": "run", "extra": map[string]any{
		"actions": []model.Action{{Command: cmd}}, "cost": 1.0,
	}}
}

func done(text string) model.Message {
	return model.Message{"role": "assistant", "content": text, "extra": map[string]any{"cost": 1.0}}
}

// yolo returns the shipped config with the interactive mode set to yolo, which
// is what `-y` does for a real run and what a spawning parent must be until
// confirmations can bubble to a human (§5.5): a child answers to nobody, so a
// confirm-mode parent may not create one.
func yolo(t *testing.T, merged map[string]any) map[string]any {
	t.Helper()
	return config.Merge(merged, map[string]any{"agent": map[string]any{"mode": "yolo"}})
}

func openSession(t *testing.T, p *Process, merged map[string]any, dir string, i int, spec func(*Spec)) *Session {
	t.Helper()
	s := Spec{Config: merged, Workdir: dir, Project: dir, Model: scripted(i)}
	s.OutputPath = "" // per-session default, which is what is under test
	if spec != nil {
		spec(&s)
	}
	if s.Model == nil {
		s.Model = scripted(i)
	}
	sess, err := p.Open(s)
	if err != nil {
		t.Fatalf("open session %d: %v", i, err)
	}
	return sess
}

// TestOpenRunsManySessionsAtOnce is the reason the package exists: several
// complete conversations, assembled concurrently through one host, each writing
// into its own directory. It is the four-session probe from agent/parallel_test.go
// driven through the real assembly path rather than by hand, so the tripwire
// covers the wiring and not only the core.
func TestOpenRunsManySessionsAtOnce(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)

	const n = 4
	dirs := make([]string, n)
	sessions := make([]*Session, n)
	for i := range dirs {
		dirs[i] = filepath.Join(base, fmt.Sprintf("repo%d", i))
		if err := os.MkdirAll(dirs[i], 0o755); err != nil {
			t.Fatal(err)
		}
		sessions[i] = openSession(t, p, merged, dirs[i], i, nil)
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, s := range sessions {
		wg.Add(1)
		go func(i int, s *Session) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if _, errs[i] = s.Run(ctx, fmt.Sprintf("task %d", i)); errs[i] != nil {
				return
			}
			b, err := os.ReadFile(filepath.Join(dirs[i], "marker.txt"))
			if err != nil {
				errs[i] = fmt.Errorf("s%d marker: %w", i, err)
				return
			}
			if got := strings.TrimSpace(string(b)); got != fmt.Sprintf("marker%d", i) {
				errs[i] = fmt.Errorf("s%d wrote %q", i, got)
			}
		}(i, s)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("session %d: %v", i, err)
		}
	}

	// The process enumerated all of them, and each row is a top-level one.
	live := p.Sessions()
	if len(live) != n {
		t.Fatalf("live sessions = %d, want %d", len(live), n)
	}
	for _, s := range live {
		if s.Kind != session.KindTop {
			t.Errorf("session %s kind = %q, want top", s.ID, s.Kind)
		}
	}
}

// TestChildSessionLineage covers §5.3, §5.5 and §5.6 through the assembly path:
// a child carries the parent link, resolves its root to the top of the tree,
// keeps the name its parent gave it, and cannot itself have children.
func TestChildSessionLineage(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	parent := openSession(t, p, merged, base, 0, nil)

	child, err := p.Open(Spec{
		Config: merged, Workdir: base, Project: base, Model: scripted(1),
		Kind: session.KindSubagent, ParentID: parent.ID.String(), Name: "quantise 70b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.Kind != session.KindSubagent || child.Parent != parent.ID {
		t.Errorf("child = kind %q parent %v, want subagent/%v", child.Kind, child.Parent, parent.ID)
	}
	if child.Name != "quantise 70b" {
		t.Errorf("name = %q, want the label the parent chose", child.Name)
	}
	row, err := p.Store.Get(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.RootID != parent.ID.String() {
		t.Errorf("child root = %q, want the parent's id (it is its own root's child)", row.RootID)
	}
	if pr, _ := p.Store.Get(parent.ID); pr.RootID != parent.ID.String() {
		t.Errorf("top-level root = %q, want its own id", pr.RootID)
	}

	// Two levels, by construction, enforced at the store rather than by trusting
	// every caller to have hidden the spawn tool.
	if _, err := p.Open(Spec{
		Config: merged, Workdir: base, ParentID: child.ID.String(), Kind: session.KindSubagent,
	}); err == nil {
		t.Error("a grandchild was opened; the two-level rule is not holding")
	} else if !strings.Contains(err.Error(), "cannot have children") {
		t.Errorf("grandchild refused for the wrong reason: %v", err)
	}

	if kids := p.Children(parent.ID); len(kids) != 1 || kids[0].ID != child.ID {
		t.Errorf("Children = %+v, want the one child", kids)
	}
}

// TestTrajectoryFilesAreOnlyWhatWasAskedFor is the rule §4 item 3 used to encode,
// rewritten for the one that replaces it: nothing writes a trajectory because a
// session happened to run. An explicit -o (or agent.output_path, or a spec that
// names one) is honored and written; everything else gets an empty path, which
// Loop.Save treats as "nowhere to write". The anti-clobber property the old test
// defended is now stronger rather than weaker: a child has no path at all, so there
// is nothing to collide with, and the state directory gains no files.
func TestTrajectoryFilesAreOnlyWhatWasAskedFor(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)

	parent := openSession(t, p, merged, base, 0, func(s *Spec) {
		s.OutputPath = filepath.Join(base, "mine.traj.json")
	})
	if parent.Cfg.OutputPath != filepath.Join(base, "mine.traj.json") {
		t.Errorf("explicit path not honored: %q", parent.Cfg.OutputPath)
	}

	// A child asks for none of it: the parent's request does not reach a session
	// whose config was derived from it (§9.8), whatever the spec carries.
	child, err := p.Open(Spec{
		Config: merged, Workdir: base, Kind: session.KindSubagent, ParentID: parent.ID.String(),
		Model: scripted(1), OutputPath: parent.Cfg.OutputPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.Cfg.OutputPath != "" {
		t.Errorf("a child inherited a trajectory path: %q", child.Cfg.OutputPath)
	}

	// Two sessions with no request write nothing, and share nothing.
	a := openSession(t, p, merged, base, 2, nil)
	b := openSession(t, p, merged, base, 3, nil)
	if a.Cfg.OutputPath != "" || b.Cfg.OutputPath != "" {
		t.Errorf("unasked sessions were given trajectory files: %q and %q", a.Cfg.OutputPath, b.Cfg.OutputPath)
	}
	for _, s := range []*Session{parent, a, b, child} {
		if _, err := s.Run(context.Background(), "do the thing"); err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "trajectories")); !os.IsNotExist(err) {
		t.Errorf("a trajectories directory exists under the state dir (%v); nothing was asked to write there", err)
	}
	// The one file that was asked for is the one that exists.
	if _, err := os.Stat(parent.Cfg.OutputPath); err != nil {
		t.Errorf("the explicit trajectory was not written: %v", err)
	}
}

// TestProcessCloseReapsTheWholeTree is §9.2's ruling: abandoning a parent takes
// its child sessions and their jobs with it, and the registry says so rather
// than leaving rows claiming work is happening.
func TestProcessCloseReapsTheWholeTree(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	parent := openSession(t, p, merged, base, 0, nil)
	child, err := p.Open(Spec{
		Config: merged, Workdir: base, Kind: session.KindSubagent, ParentID: parent.ID.String(),
		Name: "soak", Model: scripted(1),
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := child.Jobs.Start(job.Spec{Command: "sleep 30; echo never", Label: "soak", TCNO: 7})
	if err != nil {
		t.Fatal(err)
	}
	if got := child.LiveJobs(); got != 1 {
		t.Fatalf("child live jobs = %d, want 1", got)
	}

	// The order is children first, so the child reaps its own registry before the
	// parent's reap has to describe it.
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-run.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the child's job outlived the process that started it")
	}
	if child.LiveJobs() != 0 {
		t.Errorf("child still reports %d live jobs after the reap", child.LiveJobs())
	}
	if !child.Closed() || !parent.Closed() {
		t.Error("Process.Close did not close the whole tree")
	}
	if n := len(p.Sessions()); n != 0 {
		t.Errorf("live sessions after Close = %d, want 0", n)
	}
}

// TestGoAndWaitIsAsyncAndSingleFlight covers the job-shaped lifecycle feature 1
// needs: Go returns before the work is done, Wait yields the report, and a second
// run on the same session is refused rather than interleaved.
func TestGoAndWaitIsAsyncAndSingleFlight(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	s := openSession(t, p, merged, base, 0, nil)
	// A command that keeps the run busy long enough to observe it from outside.
	s.Loop.Model = &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("sleep 1; echo hi"), done("the report"),
	}}

	s.Go("take your time")
	waitForHost(t, "the run to start", s.Running)

	if _, err := s.Run(context.Background(), "second"); err == nil {
		t.Error("a second Run was allowed while one was in progress")
	} else if !strings.Contains(err.Error(), "in progress") {
		t.Errorf("refusal = %v, want the single-flight error", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	info, err := s.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info["exit_status"] != model.KindSubmitted {
		t.Errorf("exit_status = %v, want %q", info["exit_status"], model.KindSubmitted)
	}
	if info["submission"] != "the report" {
		t.Errorf("submission = %v, want the child's final text", info["submission"])
	}
	if s.Running() {
		t.Error("still running after Wait returned")
	}
}

// TestResumeLoadsTheStoredConversation is the host half of `mini -r`: the
// transcript, and the cost ledger that goes with it.
func TestResumeLoadsTheStoredConversation(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	first := openSession(t, p, merged, base, 0, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := first.Run(ctx, "do the thing"); err != nil {
		t.Fatal(err)
	}
	row, err := p.Store.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.NCalls == 0 {
		t.Fatal("the run recorded no calls; the resume has nothing to restore")
	}

	// One session has one driver (A-6). A resume used to happily build a second
	// Session object over the same transcript, which left a duplicate in the
	// process registry and meant Close closed one object twice while never
	// reaping the other's jobs; `mini -r` only ever resumes from a *different*
	// process, so closing here is what a real resume looks like.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	row, err = p.Store.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}

	resumed, err := p.Open(Spec{Config: merged, Resume: row, Model: scripted(9)})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.ResumeMsgs) == 0 || resumed.ResumeNote == "" {
		t.Errorf("resume carried nothing: %d msgs, note %q", len(resumed.ResumeMsgs), resumed.ResumeNote)
	}
	if resumed.Loop.NCalls != row.NCalls || resumed.Loop.Cost != row.Cost {
		t.Errorf("ledger = %d/%v, want the stored %d/%v", resumed.Loop.NCalls, resumed.Loop.Cost, row.NCalls, row.Cost)
	}
	if resumed.ID != first.ID {
		t.Error("a resume opened a different session")
	}
}

func TestOpenAfterCloseIsRefused(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Open(Spec{Config: merged, Workdir: base, Model: scripted(0)}); err == nil {
		t.Fatal("a session was opened on a closed process")
	}
	// And the guards are honest about the config they need.
	if _, err := New(p.Store, nil).Open(Spec{}); err == nil {
		t.Error("Open accepted a nil config")
	}
}

func TestGlobalEnvIsAppliedOnce(t *testing.T) {
	p, _ := newProcess(t)
	t.Setenv("HOST_TEST_SENTINEL", "real")
	// A .env that tries to override a variable the process already has must not
	// win, and must not be re-read per session (docs/parallel-sessions.md §4
	// item 2): LoadGlobalEnv is guarded so a second Open cannot mutate the
	// environment a first session's shells are snapshotting.
	// This test TRUNCATES the global .env, so it has to point the config
	// directory somewhere disposable first. It did not, and every
	// `go test ./host/` overwrote the developer's real
	// ~/Library/Application Support/mini/.env with the one sentinel line -
	// deleting MINI_MODEL_NAME, MINI_BASE_URL and MINI_CONFIGURED, which is what
	// made the CLI ask for a model and URL on every single start. The wizard's
	// gate was innocent; the test erased the answer it had just saved.
	//
	// MINI_STATE_DIR is already redirected per test by newProcess; this is the
	// other half of the same isolation, and main_test.go now defaults it for the
	// whole package so a future test cannot repeat the mistake by forgetting.
	t.Setenv("MINI_GLOBAL_CONFIG_DIR", t.TempDir())
	if _, err := config.EnsureConfigDir(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.GlobalConfigFile(), []byte("HOST_TEST_SENTINEL=from-dotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.LoadGlobalEnv()
	p.LoadGlobalEnv()
	if got := os.Getenv("HOST_TEST_SENTINEL"); got != "real" {
		t.Errorf("sentinel = %q, want the pre-existing value to win", got)
	}
}

func waitForHost(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestHooksReceiveTheSession guarantees the seam a front end needs is actually
// wired: a session opened with Hooks drives them, and one opened without does
// not prompt, which is what a batch run and a subagent want.
func TestHooksReceiveTheSession(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	h := &countingHooks{}
	s := openSession(t, p, merged, base, 0, func(sp *Spec) { sp.Hooks = h })
	s.Loop.Model = &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{turn("echo hi"), done("done")}}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := s.Run(ctx, "task"); err != nil {
		t.Fatal(err)
	}
	if h.before == 0 || h.messages == 0 {
		t.Errorf("hooks saw before=%d messages=%d, want both non-zero", h.before, h.messages)
	}

	plain := openSession(t, p, merged, base, 1, nil)
	if plain.Loop.Hooks != nil {
		t.Error("a session opened without Hooks inherited one anyway")
	}
}

type countingHooks struct {
	mu       sync.Mutex
	before   int
	messages int
}

func (h *countingHooks) BeforeExecute(context.Context, []model.Action, map[string]any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.before++
	return nil
}
func (h *countingHooks) OnSubmit(context.Context, model.Message) error   { return nil }
func (h *countingHooks) NextHumanCommand(context.Context) (string, bool) { return "", false }
func (h *countingHooks) OnMessage(model.Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages++
}
func (h *countingHooks) OnInterrupt(context.Context) error { return nil }

// TestNewEnvironmentReadsTheConfigSection moved here with the function it
// covers: building the interpreter is host's job now, so the rule about which
// config keys it honors belongs beside it rather than in the CLI that no longer
// calls it.
func TestNewEnvironmentReadsTheConfigSection(t *testing.T) {
	dir := t.TempDir()
	e := NewEnvironment(map[string]any{
		"cwd":     dir,
		"timeout": float64(5),
		"env":     map[string]any{"PAGER": "cat", "COUNT": 3},
	})
	if e.CWD != dir || e.Timeout.Seconds() != 5 {
		t.Errorf("env = %+v", e)
	}
	if e.Env["PAGER"] != "cat" || e.Env["COUNT"] != "3" {
		t.Errorf("env vars = %#v", e.Env)
	}
}

// TestToolSetFollowsSessionKind is §5.6 reaching the assembly: the kind decides
// the advertised set, and one value drives the client and the loop together, so
// there is no way to give a subagent a tool its instructions do not mention - or
// to describe a tool in the prompt that its requests never carried.
//
// It uses a real Sushi against a port nothing listens on rather than the scripted
// model, for two reasons: only the HTTP client carries the set into the request
// body and the parser, and a server that will not answer must fall through the
// window chain rather than fail the open.
func TestToolSetFollowsSessionKind(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	// A base URL that refuses immediately, so discovery costs no timeout.
	dead := "http://127.0.0.1:1"
	merged = config.Merge(merged, map[string]any{
		"model": map[string]any{"model_name": "test-model", "base_url": dead},
	})

	parent, err := p.Open(Spec{Config: merged, Workdir: base, Project: base})
	if err != nil {
		t.Fatal(err)
	}
	sushi, ok := parent.Model.(*model.Sushi)
	if !ok {
		t.Fatalf("expected a sushi client, got %T", parent.Model)
	}
	if got, want := strings.Join(sushi.Tools.Names(), ","), strings.Join(model.TopLevelTools().Names(), ","); got != want {
		t.Errorf("parent tools = %q, want %q", got, want)
	}
	child, err := p.Open(Spec{
		Config: merged, Workdir: base, Kind: session.KindSubagent, ParentID: parent.ID.String(),
		Name: "explore",
	})
	if err != nil {
		t.Fatal(err)
	}
	csushi, ok := child.Model.(*model.Sushi)
	if !ok {
		t.Fatalf("expected a sushi client, got %T", child.Model)
	}
	if got, want := strings.Join(csushi.Tools.Names(), ","), strings.Join(model.SubagentTools().Names(), ","); got != want {
		t.Errorf("child tools = %q, want %q", got, want)
	}
	if got, want := strings.Join(child.Loop.Tools.Names(), ","), strings.Join(csushi.Tools.Names(), ","); got != want {
		t.Errorf("the child's prompt set %q differs from its request set %q", got, want)
	}
	// The two-level tree, seen from the tool sets rather than the store: only the
	// parent is offered the spawn tool and the send that follows it, and the
	// child keeps the job tools it needs for its own background work.
	if !parent.Loop.Tools.Has(model.ToolAgent) {
		t.Error("the parent was not offered the spawn tool")
	}
	if child.Loop.Tools.Has(model.ToolAgent) || child.Loop.Tools.Has(model.ToolJobSend) {
		t.Error("a subagent was offered the spawn tool, or a send with no possible target")
	}
	if !child.Loop.Tools.Has(model.ToolBash) || !child.Loop.Tools.Has(model.ToolJobs) {
		t.Error("a subagent lost a tool it needs")
	}
}

// TestSpawnRunsAChildAndReportsToItsParent is feature 1 end to end: a parent's
// `agent` call opens a child session, the child does its own work and submits,
// and the report reaches the parent's conversation as a background result - with
// the row in the parent's registry telling the truth about it the whole time.
func TestSpawnRunsAChildAndReportsToItsParent(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)

	// Each child gets its own scripted model: touch a file in the working
	// directory it was handed, and report.
	p.ChildModel = func(kind string) model.Model {
		if kind != session.KindSubagent {
			return nil
		}
		return &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
			turn(fmt.Sprintf("echo from-child > child.txt")),
			done("REPORT: wrote child.txt and verified it"),
		}}
	}

	parent, err := p.Open(Spec{Config: yolo(t, merged), Workdir: base, Project: base, Model: &model.Deterministic{
		CostPerCall: 1, Outputs: []model.Message{
			spawnTurn("explore repo", "Look at the working directory and report what you wrote."),
			done("waiting on the child"),
			done("parent finished"),
			// Slack, and it is load-bearing rather than tidy. Whether the
			// child's report is owed *before* the parent's own submission is
			// the race this test's own comment admits two lines below, and the
			// answer changes how many turns the first run spends: if the wake
			// lands inside it, `deliverOwed` takes the parent one turn further
			// and the later runChildTurn needs a call the script no longer has.
			// A three-output script is therefore a coin flip - it failed as
			// "deterministic model ran out of outputs after 3 calls" at
			// roughly one run in two under full-suite load, at commit 59e37ca
			// and with no UI change involved. The assertion is about the child
			// reporting and the row settling, not about how many times the
			// parent is asked, so the script has to cover both orderings.
			done("parent finished, extra wake"),
			done("parent finished, second extra wake"),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	info, err := parent.Run(ctx, "spawn a helper and wait for its report")
	if err != nil {
		t.Fatal(err)
	}
	// §9.10, in the flesh: the parent's run ends beside a child that is still
	// going, because the child is not an *owed* result yet and a top-level
	// session's exit semantics were not touched for anybody's subagents. What
	// must not happen is the parent inventing a report or the row claiming the
	// child finished.
	// The parent got its own turn done and stopped. Whether the child had
	// already reported by that instant is a race - a scripted child finishes in
	// milliseconds - so the assertion is about what is always true: exactly one
	// row, and no invented report in the parent's own submission.
	if rows, err := parent.Sess.ListJobs(); err != nil || len(rows) != 1 {
		t.Fatalf("parent jobs = %+v (%v), want exactly one", rows, err)
	}
	if got := fmt.Sprint(info["submission"]); strings.Contains(got, "REPORT") {
		t.Errorf("the parent reported on the child's behalf: %q", got)
	}

	// Wait for the child to end, then take the parent's next turn - the resume
	// the human would have triggered with `mini -r`, which is the delivery path
	// this design reuses rather than replaces.
	waitForHost(t, "the child's row to go terminal", func() bool {
		rows, err := parent.Sess.ListJobs()
		return err == nil && len(rows) == 1 && rows[0].State != session.JobRunning
	})
	if _, err := parent.runChildTurn(ctx, ""); err != nil {
		t.Fatal(err)
	}

	// The child really ran, in the parent's directory, and left its evidence.
	if _, err := os.Stat(filepath.Join(base, "child.txt")); err != nil {
		t.Errorf("the child never did its work: %v", err)
	}

	// The child's row lives in the parent's registry, named, and is terminal.
	rows, err := parent.Sess.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("parent jobs = %d, want the one subagent", len(rows))
	}
	row := rows[0]
	if row.Kind != session.KindSubagent || row.Label != "explore repo" {
		t.Errorf("row = kind %q label %q", row.Kind, row.Label)
	}
	if row.State == session.JobRunning {
		t.Fatal("the agent row is still running after the child reported")
	}

	// What reaches the parent's conversation is the record of the ending: the id,
	// how it ended, what it was called, where the report is, and how big it is.
	// It is not the report body. That was the old contract, argued on the cost of
	// a turn spent fetching the answer, and it held while a wake carried one job;
	// a boundary now carries the whole pending set, so eight of them inlining
	// their bodies would spend the parent's context on a message nobody asked
	// for. The counts are what make the read a decision instead of a guess.
	var saw string
	for _, m := range parent.Loop.Messages() {
		if c := fmt.Sprint(m["content"]); strings.Contains(c, "job "+row.ID+" ended") {
			saw = c
		}
	}
	if saw == "" {
		for _, m := range parent.Loop.Messages() {
			t.Logf("msg role=%v: %.140s", m["role"], m["content"])
		}
		t.Fatal("the subagent's ending never reached the parent's conversation")
	}
	if !strings.Contains(saw, "explore repo") {
		t.Errorf("the wake does not name the subagent it is about: %.200s", saw)
	}
	if !strings.Contains(saw, row.OutputPath) {
		t.Errorf("the wake must point at the report file: %s", saw)
	}
	if !strings.Contains(saw, "bytes,") {
		t.Errorf("the wake must size the output so the parent can choose a read: %s", saw)
	}
	if strings.Contains(saw, "REPORT: wrote child.txt") {
		t.Errorf("the wake inlined the report body; it carries a path now:\n%s", saw)
	}

	// And the full report is in the row's own file, where `jobs` points.
	body, err := os.ReadFile(row.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "REPORT: wrote child.txt") {
		t.Errorf("job file holds %.120q", body)
	}
}

// TestSubagentCannotSpawn is the two-level tree enforced at the seam as well as
// by tool visibility (§5.6) and by the store (§5.3): three layers, all agreeing,
// because the one that matters is the one a future caller forgets.
func TestSubagentCannotSpawn(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	parent, err := p.Open(Spec{Config: merged, Workdir: base, Project: base, Model: scripted(0)})
	if err != nil {
		t.Fatal(err)
	}
	child, err := p.Open(Spec{
		Config: merged, Workdir: base, Kind: session.KindSubagent, ParentID: parent.ID.String(),
		Name: "leaf", Model: scripted(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.Spawn(context.Background(), agent.SpawnRequest{Name: "grandchild", Prompt: "try"}); err == nil {
		t.Fatal("a subagent spawned a child")
	} else if !strings.Contains(err.Error(), "cannot spawn another subagent") {
		t.Errorf("refusal = %v", err)
	}
}

// TestSendRefusesNonAgentJobs is §9.11's flat refusal, and §9.5's claim that
// `job_send` needs no new listing: the id it takes is the one `jobs` prints.
func TestSendRefusesNonAgentJobs(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	parent, err := p.Open(Spec{Config: merged, Workdir: base, Project: base, Model: scripted(0)})
	if err != nil {
		t.Fatal(err)
	}
	run, err := parent.Jobs.Start(job.Spec{Command: "sleep 5; echo hi", Label: "build", TCNO: 3})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { parent.Jobs.KillAll() })

	if err := parent.Send(context.Background(), run.ID(), "stop that"); err == nil {
		t.Fatal("a bash job accepted a send")
	} else if !strings.Contains(err.Error(), "Cannot send to job") {
		t.Errorf("refusal = %v, want the §9.11 wording", err)
	}
	if err := parent.Send(context.Background(), "j999", "hello"); err == nil {
		t.Error("a send to an unknown job was accepted")
	}
}

// TestNoCountBoundOnTheParentsRegistry is A-8's regression, and it replaces a
// test that only proved agent jobs were *exempt* from a cap. The bug was that
// exemption was one-sided: subagents are registered as agent-kind jobs on the
// parent's own manager, so they occupied slots in the count the bash cap was
// compared against, and a parent with eight live children could not start so
// much as `ls` - while a long command that outran its budget was destroyed at
// the promotion point by handoff's kill.
//
// There is no count bound to trip now. This starts well past the old cap of 8,
// in both kinds, interleaved, and asserts nothing is refused.
func TestNoCountBoundOnTheParentsRegistry(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	p.ChildModel = func(string) model.Model {
		return &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{done("child report")}}
	}
	parent, err := p.Open(Spec{Config: yolo(t, merged), Workdir: base, Project: base, Model: scripted(0)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { parent.Jobs.KillAll() })

	// Well past the old cap, and past it again after the children arrive.
	const before, after = 10, 4
	for i := 0; i < before; i++ {
		if _, err := parent.Jobs.Start(job.Spec{Command: "sleep 30", Label: "hog", TCNO: 10 + i}); err != nil {
			t.Fatalf("bash job %d was refused: %v", i, err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := parent.Spawn(context.Background(),
			agent.SpawnRequest{Name: fmt.Sprintf("kid-%d", i), Prompt: "do it"}); err != nil {
			t.Fatalf("spawn %d was refused: %v", i, err)
		}
	}
	for i := 0; i < after; i++ {
		if _, err := parent.Jobs.Start(job.Spec{Command: "sleep 30", Label: "hog", TCNO: 50 + i}); err != nil {
			t.Fatalf("bash job %d was refused after the children: %v", i, err)
		}
	}
	if n := parent.Jobs.StillRunning(); n < before+after {
		t.Fatalf("live jobs = %d, want at least %d", n, before+after)
	}
}

// TestSubagentPromptsBubbleToTheParentsHuman is decision 13 in one test, and it
// replaces the guard that used to refuse this outright. A confirm-mode parent can
// now spawn, because the child's gate forwards to the same human who gates the
// parent - so there is no invisible widening left to refuse. What the child must
// NOT get is the parent's other hooks: those ask about the parent's run, and
// answering them on a child's behalf would be the parent's human making decisions
// about a conversation they were not shown.
func TestSubagentPromptsBubbleToTheParentsHuman(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	parentHooks := &recordingHooks{}
	parent, err := p.Open(Spec{
		Config: merged, Workdir: base, Project: base, Model: scripted(0), Hooks: parentHooks,
	})
	if err != nil {
		t.Fatal(err)
	}
	if parent.Cfg.Mode == "yolo" {
		t.Skip("the shipped config no longer defaults to confirm; nothing to bubble")
	}
	p.ChildModel = func(string) model.Model {
		return &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{done("child report")}}
	}

	child, err := p.Open(Spec{
		Config: merged, Workdir: base, Kind: session.KindSubagent, ParentID: parent.ID.String(),
		Name: "quantise 70b", Model: scripted(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Spawn wires the route; opening directly (as above) does not, because the
	// child the human never delegated has no gate to inherit yet.
	if _, err := parent.Spawn(context.Background(), agent.SpawnRequest{Name: "worker", Prompt: "go"}); err != nil {
		t.Fatal(err)
	}
	_ = child

	spawned := p.Children(parent.ID)
	var kid *Session
	for _, c := range spawned {
		if c.Name == "worker" {
			kid = c
		}
	}
	if kid == nil {
		t.Fatal("the spawned child is not in the parent's live set")
	}
	if kid.Loop.Hooks == nil {
		t.Fatal("a spawned child has no route to a human; it would run unattended")
	}
	if kid.Loop.Origin != "worker" {
		t.Errorf("child origin = %q, want the name its parent gave it", kid.Loop.Origin)
	}

	// The gate reaches the parent's human, carrying the child's attribution.
	tv := kid.Loop.TemplateVars()
	if got := fmt.Sprint(tv["asker"]); got != "worker" {
		t.Errorf("the child's prompt variables carry asker=%q, want its name", got)
	}
	if err := kid.Loop.Hooks.BeforeExecute(context.Background(), []model.Action{{Command: "rm -rf build/"}}, tv); err != nil {
		t.Fatalf("BeforeExecute: %v", err)
	}
	if parentHooks.before != 1 {
		t.Fatalf("parent hooks saw %d BeforeExecute calls, want 1", parentHooks.before)
	}
	if parentHooks.lastAsker != "worker" {
		t.Errorf("the parent was asked about a command whose asker was %q, want \"worker\" - an unattributed prompt is the §5.5 failure", parentHooks.lastAsker)
	}

	// The parent's run-shaping hooks are not reachable from a child: a submit
	// prompt answered on a child's behalf would end the wrong conversation.
	if err := kid.Loop.Hooks.OnSubmit(context.Background(), model.Message{}); err != nil {
		t.Errorf("OnSubmit returned %v, want nil: a child ends when it ends", err)
	}
	if parentHooks.submits != 0 {
		t.Errorf("the parent's human was asked to end the child's run: submits=%d", parentHooks.submits)
	}
}

// TestUnattendedParentSpawnsUnattendedChild is the other half of decision 13:
// with no human behind the parent there is nobody to bubble to, so the child
// inherits the unattendedness - which means its prompts reject through the
// loop's normal no-hooks path rather than hanging waiting for a person who will
// never arrive.
func TestUnattendedParentSpawnsUnattendedChild(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	parent, err := p.Open(Spec{Config: yolo(t, merged), Workdir: base, Project: base, Model: scripted(0)})
	if err != nil {
		t.Fatal(err)
	}
	p.ChildModel = func(string) model.Model {
		return &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{done("child report")}}
	}
	if parent.Loop.Hooks != nil {
		t.Fatal("this test assumes a hookless parent")
	}
	if _, err := parent.Spawn(context.Background(), agent.SpawnRequest{Name: "solo", Prompt: "go"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range p.Children(parent.ID) {
		if c.Name == "solo" && c.Loop.Hooks != nil {
			t.Error("a child was given a route to a human that does not exist")
		}
	}
}

// recordingHooks is a parent's stand-in front end: it records what a child asks
// on the parent's behalf and approves everything, so the routing is what is
// under test rather than the answer.
type recordingHooks struct {
	before    int
	submits   int
	lastAsker string
}

func (h *recordingHooks) BeforeExecute(_ context.Context, _ []model.Action, tvars map[string]any) error {
	h.before++
	if s, ok := tvars["asker"].(string); ok {
		h.lastAsker = s
	}
	return nil
}
func (h *recordingHooks) OnSubmit(context.Context, model.Message) error   { h.submits++; return nil }
func (h *recordingHooks) NextHumanCommand(context.Context) (string, bool) { return "", false }
func (h *recordingHooks) OnMessage(model.Message)                         {}
func (h *recordingHooks) OnInterrupt(context.Context) error               { return nil }

func spawnTurn(name, prompt string) model.Message {
	return model.Message{"role": "assistant", "content": "delegating", "extra": map[string]any{
		"actions": []model.Action{{Tool: model.ToolAgent, Args: map[string]any{"name": name, "prompt": prompt}}},
		"cost":    1.0,
	}}
}

// TestSendRevivesAFinishedSubagent is §9.3's whole claim - "reviving an agent that
// has already delivered results is restoring it like any other session, then
// sending it a message" - tested as the failure it prevents. Before this, a send
// to a finished child queued a message behind a goroutine that no longer existed:
// no error, no crash, and a parent waiting forever on a second report that nothing
// was left alive to write. A silent dead letter is the worst shape a bug can take
// in a system whose entire design is that results arrive unprompted.
func TestSendRevivesAFinishedSubagent(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	// One child, two runs' worth of script: the second run continues the same
	// transcript, which is the point of reviving rather than respawning.
	p.ChildModel = func(string) model.Model {
		return &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
			turn("echo one > pass1.txt"),
			done("REPORT-1: first pass done"),
			turn("echo two > pass2.txt"),
			done("REPORT-2: second pass done"),
		}}
	}
	parent, err := p.Open(Spec{Config: yolo(t, merged), Workdir: base, Project: base, Model: scripted(0)})
	if err != nil {
		t.Fatal(err)
	}

	reply, err := parent.Spawn(context.Background(), agent.SpawnRequest{Name: "worker", Prompt: "first pass"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	first := waitJobDone(t, parent, reply.JobID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(first.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "REPORT-1") {
		t.Fatalf("first run reported %.80q", body)
	}
	if _, err := os.Stat(filepath.Join(base, "pass2.txt")); err == nil {
		t.Fatal("the second pass ran before it was asked for")
	}

	if err := parent.Send(ctx, reply.JobID, "second pass, please"); err != nil {
		t.Fatalf("send to a finished subagent: %v", err)
	}

	// A second row, not the first one reopened: the state machine has no legal
	// trip from delivered back to running, and inventing one would cost the
	// structural termination the jobs design relies on.
	waitForHost(t, "the revival to register a second row", func() bool {
		rows, err := parent.Sess.ListJobs()
		return err == nil && len(rows) == 2
	})
	rows, _ := parent.Sess.ListJobs()
	var secondID string
	for _, r := range rows {
		if r.ID != reply.JobID {
			secondID = r.ID
		}
	}
	if secondID == "" {
		t.Fatal("no second row")
	}
	probe, err := parent.Sess.GetJob(secondID)
	if err != nil {
		t.Fatal(err)
	}
	if probe.Label != "worker" {
		t.Errorf("the revived row is labelled %q, want the same name the parent gave", probe.Label)
	}
	if !strings.Contains(probe.Description, "resumed") {
		t.Errorf("the revived row does not say it is a continuation: %q", probe.Description)
	}

	secondRow := waitJobDone(t, parent, secondID)
	body, err = os.ReadFile(secondRow.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "REPORT-2") {
		t.Errorf("the revived run reported %.120q, want REPORT-2", body)
	}
	if _, err := os.Stat(filepath.Join(base, "pass2.txt")); err != nil {
		t.Error("the revived run did not do its work")
	}

	// The revival continued the transcript rather than starting fresh - that is
	// what makes it a revival and not a second delegation.
	kid, ok := p.childOfJob(parent.ID.String(), secondID)
	if !ok {
		t.Fatal("the revived row is not mapped to a child")
	}
	var userTurns int
	for _, m := range kid.Loop.Messages() {
		if m["role"] == "user" && !model.IsToolResult(m) {
			userTurns++
		}
	}
	if userTurns < 2 {
		t.Errorf("the revived child has %d non-observation user turns; a revival should carry the first pass's", userTurns)
	}
	// And it is still one child session, not two.
	if n := len(p.Children(parent.ID)); n != 1 {
		t.Errorf("children after revival = %d, want 1", n)
	}
}

// waitJobDone polls one of a session's job rows until it leaves the running
// state, and returns the settled row. Deliberately a poll in the test rather than
// a Wait method on Session: the runtime's own contract is that results are pushed
// at step boundaries, and a production API that invited callers to block on a job
// would be the polling the design exists to remove, with a nicer name.
func waitJobDone(t *testing.T, s *Session, id string) *session.JobRow {
	t.Helper()
	var row *session.JobRow
	waitForHost(t, "job "+id+" to settle", func() bool {
		r, err := s.Sess.GetJob(id)
		if err != nil {
			return false
		}
		row = r
		return r.State != session.JobRunning
	})
	if row.State == session.JobRunning {
		t.Fatalf("job %s never settled", id)
	}
	return row
}

// TestSubagentRecordsItsCostWithoutBoundingIt keeps the distinction the budget
// ruling turns on: enforcement is gone, bookkeeping is not. A child's spend is
// still tallied turn by turn and reaches the parent's tree total, because "what
// did that delegation cost me" is a fair question even when nobody sends an
// invoice and nothing is allowed to stop the work over the answer
// (docs/parallel-sessions.md §5.14, §5.8).
func TestSubagentRecordsItsCostWithoutBoundingIt(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	parent, err := p.Open(Spec{Config: yolo(t, merged), Workdir: base, Project: base, Model: scripted(0)})
	if err != nil {
		t.Fatal(err)
	}
	p.ChildModel = func(string) model.Model {
		return &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
			turn("echo a > a.txt"), turn("echo b > b.txt"), turn("echo c > c.txt"),
			done("REPORT: three passes, nobody stopped me"),
		}}
	}
	reply, err := parent.Spawn(context.Background(), agent.SpawnRequest{Name: "unbounded", Prompt: "do all of it"})
	if err != nil {
		t.Fatal(err)
	}
	row := waitJobDone(t, parent, reply.JobID)
	if row.State != session.JobCompleted {
		t.Errorf("state = %s, want completed", row.State)
	}
	body := string(mustRead(t, row.OutputPath))
	if !strings.Contains(body, "REPORT: three passes") {
		t.Errorf("child reported %.120q", body)
	}
	if strings.Contains(strings.ToLower(body), "limit") {
		t.Errorf("a limit touched the child: %.160q", body)
	}
	kid, ok := p.childOfJob(parent.ID.String(), reply.JobID)
	if !ok {
		t.Fatal("child not mapped")
	}
	if kid.Loop.NCalls != 4 || kid.Loop.Cost <= 0 {
		t.Errorf("child ledger = %d calls / %v, want the four turns recorded", kid.Loop.NCalls, kid.Loop.Cost)
	}
	// And the rollup sees it: the parent's tree total includes the child's spend.
	total, calls, err := p.Store.TreeTotals(parent.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	// Four calls and four dollars of price-attribution, all of it the child's:
	// the parent never ran, so anything non-zero here can only have arrived
	// through the tree.
	if total != 4 || calls != 4 {
		t.Errorf("tree totals = %v/%v, want 4/4 attributed to the parent's tree", total, calls)
	}
}

// TestEndingOfNamesTheEnding keeps §9.9's promise for the endings a child can
// still meet without a budget: killed, or a run that stopped without submitting.
func TestEndingOfNamesTheEnding(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	parent, err := p.Open(Spec{Config: yolo(t, merged), Workdir: base, Project: base, Model: scripted(0)})
	if err != nil {
		t.Fatal(err)
	}
	kid, err := p.Open(Spec{
		Config: merged, Workdir: base, Kind: session.KindSubagent, ParentID: parent.ID.String(),
		Name: "worker", Model: scripted(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	kid.Loop.NCalls, kid.Loop.Cost = 4, 2.5
	got := endingOf(kid, map[string]any{"exit_status": model.KindRepeatedFormatErr})
	for _, want := range []string{"worker", "repeatedformaterr", "4 model calls", "$2.50"} {
		if !strings.Contains(got, want) {
			t.Errorf("ending does not name %q: %q", want, got)
		}
	}
	// An empty info map is not a crash and not a blank either.
	if !strings.Contains(endingOf(kid, nil), "without a report") {
		t.Error("the no-information ending lost its wording")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestKillStopsALiveSubagent is the control that replaced the budgets. With no
// step, cost or wall-clock ceiling left (§5.14), job_kill and Quit are the only
// brakes on a runaway - so the one thing that must unquestionably work is ending
// a child that is mid-turn, with its row settling honestly and the parent being
// told rather than left guessing.
func TestKillStopsALiveSubagent(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	parent, err := p.Open(Spec{Config: yolo(t, merged), Workdir: base, Project: base, Model: scripted(0)})
	if err != nil {
		t.Fatal(err)
	}
	p.ChildModel = func(string) model.Model {
		// Never submits: the only way to be still running when the parent
		// decides to stop it.
		return &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
			turn("sleep 30; echo never"),
		}}
	}
	reply, err := parent.Spawn(context.Background(), agent.SpawnRequest{Name: "stuck", Prompt: "take forever"})
	if err != nil {
		t.Fatal(err)
	}
	waitForHost(t, "the child to be running", func() bool {
		row, err := parent.Sess.GetJob(reply.JobID)
		return err == nil && row.State == session.JobRunning
	})

	run, err := parent.Jobs.Get(reply.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	row := waitJobDone(t, parent, reply.JobID)
	if row.State != session.JobKilled {
		t.Errorf("state = %s, want killed", row.State)
	}
	// The parent is told, and told something it can act on: the report of a
	// killed child names how far it got rather than going blank.
	kid, ok := p.childOfJob(parent.ID.String(), reply.JobID)
	if !ok {
		t.Fatal("child not mapped")
	}
	if kid.Running() {
		t.Error("the child's loop is still running after the kill")
	}
	body := string(mustRead(t, row.OutputPath))
	if strings.Contains(body, "ended without a report") {
		t.Errorf("a killed child produced the blank summary: %q", body)
	}
	// And the row is not counted as live work any more, which is what frees the
	// parent's registry view.
	if n := parent.LiveJobs(); n != 0 {
		t.Errorf("parent live jobs = %d after the kill, want 0", n)
	}
}

// TestOnChildAttendsASpawnedSession is step 7.4, and the ordering it exists to
// guarantee. A front end that can show a child gives the child its own hooks
// instead of bubbling into the parent's transcript (decision 15: "bubbling becomes
// routing"), and it gets the chance while the child is still not running - because
// a hook installed after the run starts is a gate the child may already have
// walked through.
func TestOnChildAttendsASpawnedSession(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	parentHooks := &recordingHooks{}
	parent, err := p.Open(Spec{
		Config: merged, Workdir: base, Project: base, Model: scripted(0), Hooks: parentHooks,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.ChildModel = func(string) model.Model {
		return &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{done("child report")}}
	}

	front := &recordingHooks{}
	var sawOrigin string
	var sawHooks agent.Hooks
	var alreadyRunning bool
	p.OnChild = func(cs *Session) error {
		sawOrigin, sawHooks, alreadyRunning = cs.Loop.Origin, cs.Loop.Hooks, cs.Running()
		cs.Loop.Hooks = front
		return nil
	}
	if _, err := parent.Spawn(context.Background(), agent.SpawnRequest{Name: "worker", Prompt: "go"}); err != nil {
		t.Fatal(err)
	}

	// The callback saw a child that had its identity and no route yet: that is the
	// window in which a front end must be allowed to install one. The wrapper is
	// installed by Spawn after the callback returns, so `sawHooks` stays nil.
	if sawOrigin != "worker" {
		t.Errorf("at the callback the child's origin was %q, want its name", sawOrigin)
	}
	if sawHooks != nil {
		t.Errorf("at the callback the child already had %T, want the callback to be first", sawHooks)
	}
	if alreadyRunning {
		t.Error("the child was already running when its front end was being asked to attend it")
	}

	kid := p.Children(parent.ID)[0]
	// The front end's hooks are there, wrapped: a child's endings belong to the
	// parent's job contract and not to whoever is watching its tab (§8.4 gave the
	// tab the gate and the transcript, and this wrapper is what keeps the run from
	// asking a new-task question nobody can answer on the parent's behalf).
	if attended, ok := kid.Loop.Hooks.(childHooks); !ok || attended.inner != agent.Hooks(front) {
		t.Errorf("the child kept %T, want its front end's hooks wrapped for endings", kid.Loop.Hooks)
	}
	// And a gate asked of the child reaches the child's own front end, not its
	// parent's human: the tab it is sitting in is the attribution now.
	if err := kid.Loop.Hooks.BeforeExecute(context.Background(),
		[]model.Action{{Command: "rm -rf build/"}}, kid.Loop.TemplateVars()); err != nil {
		t.Fatalf("BeforeExecute: %v", err)
	}
	if parentHooks.before != 0 {
		t.Errorf("the parent's human was asked about a child with a front end of its own (%d asks)", parentHooks.before)
	}
	if front.before != 1 {
		t.Errorf("the child's own front end saw %d gates, want 1", front.before)
	}
}

// TestSpawnRefusesAChildItCannotAttend is decision 13's rule with the tabbed
// front end in place: a child nobody can answer does not run. The refusal comes
// from the front end failing to open, and the half-built session is reaped rather
// than left in the roster with no window.
func TestSpawnRefusesAChildItCannotAttend(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	parent, err := p.Open(Spec{Config: yolo(t, merged), Workdir: base, Project: base, Model: scripted(0)})
	if err != nil {
		t.Fatal(err)
	}
	p.ChildModel = func(string) model.Model {
		return &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{done("never runs")}}
	}
	p.OnChild = func(*Session) error { return errors.New("the window is full") }

	_, err = parent.Spawn(context.Background(), agent.SpawnRequest{Name: "orphan", Prompt: "go"})
	if err == nil || !strings.Contains(err.Error(), "the window is full") {
		t.Fatalf("spawn err = %v, want the front end's reason", err)
	}
	if n := len(p.Children(parent.ID)); n != 0 {
		t.Errorf("%d children left after a refused spawn, want none reaped into the roster", n)
	}
}

// blockingSubmitHooks is a front end that behaves at the end of a run the way
// Interactive does: asked whether the session is over, it waits for a human who
// never answers. That is the §8.4 regression made testable. Giving a child its own
// tab also gave it this prompt, and a child parked inside it never returns from
// Loop.Run, never settles its agent row, and never reports - so its parent waits
// forever on a wake that cannot arrive.
type blockingSubmitHooks struct {
	asked chan struct{}
	hold  chan struct{}

	mu    sync.Mutex
	gates int
}

func (h *blockingSubmitHooks) BeforeExecute(_ context.Context, _ []model.Action, _ map[string]any) error {
	h.mu.Lock()
	h.gates++
	h.mu.Unlock()
	return nil
}

func (h *blockingSubmitHooks) OnSubmit(context.Context, model.Message) error {
	select {
	case <-h.asked:
	default:
		close(h.asked)
	}
	<-h.hold // the human of a delegated session is not coming
	return nil
}

func (h *blockingSubmitHooks) NextHumanCommand(context.Context) (string, bool) { return "", false }
func (h *blockingSubmitHooks) OnMessage(model.Message)                         {}
func (h *blockingSubmitHooks) OnInterrupt(context.Context) error               { return nil }

func (h *blockingSubmitHooks) askedForANewTask() bool {
	select {
	case <-h.asked:
		return true
	default:
		return false
	}
}

// TestChildNeverAsksItsOwnHumanForTheNextTask is the yield-back bug in one place:
// the child must report and settle its row even when the front end attending it
// would block on a prompt, and the gate must still reach that front end - the fix
// is about endings, not about quietly removing a child's route to a human.
func TestChildNeverAsksItsOwnHumanForTheNextTask(t *testing.T) {
	p, base := newProcess(t)
	merged := testConfig(t)
	p.ChildModel = func(string) model.Model {
		return &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
			done("REPORT: nothing left to do here"),
		}}
	}
	attending := &blockingSubmitHooks{asked: make(chan struct{}), hold: make(chan struct{})}
	p.OnChild = func(cs *Session) error {
		cs.Loop.Hooks = attending
		return nil
	}

	parent, err := p.Open(Spec{Config: yolo(t, merged), Workdir: base, Project: base, Model: scripted(0)})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := parent.Spawn(context.Background(), agent.SpawnRequest{Name: "worker", Prompt: "wrap it up"})
	if err != nil {
		t.Fatal(err)
	}

	// The row, not the manager's live handle: a scripted child finishes in
	// milliseconds and the registry forgets the run once it does.
	var row *session.JobRow
	waitForHost(t, "the child's agent row to settle", func() bool {
		got, err := parent.Sess.GetJob(reply.JobID)
		if err != nil {
			t.Fatal(err)
		}
		row = got
		return row.State != session.JobRunning
	})
	if row.State != session.JobCompleted {
		t.Errorf("agent row settled as %q, want %q", row.State, session.JobCompleted)
	}
	if attending.askedForANewTask() {
		t.Error("the child was asked for a new task at the end of its run")
	}

	// The report is where the parent will be pointed at it: the job's own file.
	body, err := os.ReadFile(row.OutputPath)
	if err != nil || !strings.Contains(string(body), "REPORT") {
		t.Errorf("the child's report is not in %s: %q (%v)", row.OutputPath, string(body), err)
	}

	kids := p.Children(parent.ID)
	if len(kids) != 1 {
		t.Fatalf("%d children, want 1", len(kids))
	}
	if err := kids[0].Loop.Hooks.BeforeExecute(context.Background(),
		[]model.Action{{Command: "rm -rf build/"}}, nil); err != nil {
		t.Fatalf("BeforeExecute: %v", err)
	}
	attending.mu.Lock()
	gates := attending.gates
	attending.mu.Unlock()
	if gates != 1 {
		t.Errorf("the attending front end saw %d gates, want 1: a child's confirmations still belong to it", gates)
	}
}
