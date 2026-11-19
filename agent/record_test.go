package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

// fakeRecorder is the session store's shadow: it stamps extra["turn"] exactly
// like *session.Session does and collects every interaction for assertions.
type fakeRecorder struct {
	dir   string
	next  int64
	seen  []model.Message
	opens []fakeOpen
	comps []fakeComp
	progs [][2]float64
	calls int
}

type fakeOpen struct {
	turn   int64
	idx    int
	action model.Action
	tc     model.ToolCall
}

type fakeComp struct {
	number int
	out    model.ToolOutcome
}

func (f *fakeRecorder) RecordMessage(m model.Message) (int64, error) {
	f.next++
	if e, ok := m["extra"].(map[string]any); ok {
		e["turn"] = f.next
	}
	cp := model.Message{}
	for k, v := range m {
		cp[k] = v
	}
	f.seen = append(f.seen, cp)
	return f.next, nil
}

func (f *fakeRecorder) RecordProgress(cost float64, nCalls int) error {
	f.calls = nCalls
	f.progs = append(f.progs, [2]float64{cost, float64(nCalls)})
	return nil
}

func (f *fakeRecorder) OpenToolCall(msg model.Message, idx int, action model.Action) (model.ToolCall, error) {
	extra, ok := msg["extra"].(map[string]any)
	if !ok {
		return model.ToolCall{}, errors.New("session store: issuing turn not recorded")
	}
	turn, ok := extra["turn"].(int64)
	if !ok {
		return model.ToolCall{}, errors.New("session store: issuing turn not recorded")
	}
	dir, err := os.MkdirTemp(f.dir, "call-")
	if err != nil {
		return model.ToolCall{}, err
	}
	path := filepath.Join(dir, "out.log")
	fh, err := os.Create(path)
	if err != nil {
		return model.ToolCall{}, err
	}
	tc := model.ToolCall{Number: len(f.opens) + 1, Path: path, File: fh, Action: action}
	f.opens = append(f.opens, fakeOpen{turn: turn, idx: idx, action: action, tc: tc})
	return tc, nil
}

func (f *fakeRecorder) CompleteToolCall(c model.ToolCall, out model.ToolOutcome) error {
	f.comps = append(f.comps, fakeComp{number: c.Number, out: out})
	return nil
}

// spyEnv is the real interpreter with a memory of the writer each command was
// handed. The lock is not decoration: a batch of actions is executed
// concurrently, so Execute is now reached from one goroutine per command and the
// slice it grows would otherwise be written by all of them at once.
type spyEnv struct {
	inner   *env.Local
	mu      sync.Mutex
	writers []io.Writer
}

func (s *spyEnv) Execute(ctx context.Context, command, cwd string, out io.Writer) (env.Result, error) {
	s.mu.Lock()
	s.writers = append(s.writers, out)
	s.mu.Unlock()
	return s.inner.Execute(ctx, command, cwd, out)
}

// seen returns the writers in the order they were recorded, which under
// concurrent execution is the order the commands happened to start in and not
// the order they were issued. Assert against orderedWriters instead when the
// pairing matters.
func (s *spyEnv) seenWriters() []io.Writer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]io.Writer(nil), s.writers...)
}

func (s *spyEnv) TemplateVars() map[string]any { return s.inner.TemplateVars() }
func (s *spyEnv) Serialize() map[string]any    { return s.inner.Serialize() }

func newRecorderLoop(t *testing.T, m model.Model, e env.Environment) (*Loop, *fakeRecorder) {
	t.Helper()
	cfg := agentConfig(t)
	rec := &fakeRecorder{dir: t.TempDir()}
	return &Loop{Cfg: cfg, Model: m, Env: e, Recorder: rec}, rec
}

// Every message appended to the history is recorded exactly once in order;
// the writer handed to Execute IS the recorder's open file; the outcome the
// loop measured arrives; and the live observation carries no raw_output copy.
func TestRecorderSeesEveryTurn(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("echo", "echo hi"),
		done("all finished"),
	}}
	spy := &spyEnv{inner: &env.Local{Timeout: 5 * time.Second}}
	l, rec := newRecorderLoop(t, m, spy)
	if _, err := run(t, l, "echo hi then finish"); err != nil {
		t.Fatal(err)
	}

	if len(rec.seen) != len(l.messages) {
		t.Fatalf("recorded %d messages, history has %d", len(rec.seen), len(l.messages))
	}
	for i, want := range l.messages {
		got := rec.seen[i]
		if got["role"] != want["role"] {
			t.Errorf("recorded %d role = %v, want %v", i, got["role"], want["role"])
		}
		// Messages that carry an extra map (every agent message; the prompt
		// plumbing does not) get the turn number stamped by the recorder.
		if e, ok := want["extra"].(map[string]any); ok && len(e) > 0 {
			if e["turn"] != int64(i)+1 {
				t.Errorf("message %d extra[turn] = %v, want %d", i, e["turn"], i+1)
			}
		}
	}

	// The command's bytes went straight into the recorder's file: the writer
	// is the handle OpenToolCall returned.
	ws := spy.seenWriters()
	if len(ws) == 0 {
		t.Fatal("Execute was never handed a writer")
	}
	f, ok := ws[0].(*os.File)
	if !ok || len(rec.opens) == 0 || f != rec.opens[0].tc.File {
		t.Fatalf("Execute did not receive the call's file (writers[0] = %T)", ws[0])
	}
	b, err := os.ReadFile(rec.opens[0].tc.Path)
	if err != nil || string(b) != "hi\n" {
		t.Errorf("result file = %q, %v; want %q", b, err, "hi\n")
	}

	// Only the one command runs: the submitting turn has no actions, so the
	// recorder sees exactly one completed call.
	if len(rec.comps) != 1 {
		t.Fatalf("completes = %d, want 1", len(rec.comps))
	}
	c := rec.comps[0]
	if c.number != 1 || c.out.Bytes != 3 || c.out.ViewOffset != 0 || c.out.ViewElided != 0 || c.out.ReturnCode != 0 || c.out.CWD == "" {
		t.Errorf("outcome = %+v", c)
	}

	// The live observation message has no in-memory copy of the output.
	for _, msg := range rec.seen {
		if extra, _ := msg["extra"].(map[string]any); extra != nil {
			if _, ok := extra["raw_output"]; ok && model.IsToolResult(msg) {
				t.Error("live observation carries raw_output")
			}
		}
	}

	// The trajectory puts it back: byte-identical to what mini exported
	// before the database existed, key set included. Assert on JSON bytes.
	traj, err := l.Save("")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(traj["messages"])
	if err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]any
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatal(err)
	}
	var obs map[string]any
	for _, mm := range msgs {
		if model.IsToolResult(mm) {
			obs = mm["extra"].(map[string]any)
		}
	}
	if obs == nil {
		t.Fatal("no observation turn in the trajectory")
	}
	if obs["raw_output"] != "hi\n" {
		t.Errorf("trajectory raw_output = %v, want %q", obs["raw_output"], "hi\n")
	}
	if obs["tcr_path"] == "" {
		t.Error("trajectory observation lost tcr_path")
	}

	if len(rec.progs) == 0 {
		t.Error("RecordProgress never called")
	}
	if last := rec.progs[len(rec.progs)-1]; last[0] != l.Cost || last[1] != float64(l.NCalls) {
		t.Errorf("final progress = %v, want cost %v calls %d", last, l.Cost, l.NCalls)
	}
}

// One turn with two actions produces two call rows on the same issuing turn,
// at positions 0 and 1, with distinct files, completed in issue order.
func TestRecorderPerParallelCall(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("both", model.Action{Command: "echo one", ToolCallID: "call_1"},
			model.Action{Command: "echo two", ToolCallID: "call_2"}),
		done("bye"),
	}}
	l, rec := newRecorderLoop(t, m, &env.Local{Timeout: 5 * time.Second})
	if _, err := run(t, l, "two commands then finish"); err != nil {
		t.Fatal(err)
	}
	// The 2 actions of the parallel turn; the submit turn has nothing to run.
	if len(rec.opens) != 2 || len(rec.comps) != 2 {
		t.Fatalf("opens=%d comps=%d, want 2/2", len(rec.opens), len(rec.comps))
	}
	var assistantTurn int64
	for _, msg := range rec.seen {
		if msg["role"] == "assistant" {
			extra := msg["extra"].(map[string]any)
			assistantTurn, _ = extra["turn"].(int64)
			break
		}
	}
	if assistantTurn == 0 {
		t.Fatal("assistant turn never recorded")
	}
	for i := range 2 {
		o := rec.opens[i]
		if o.idx != i || o.turn != assistantTurn {
			t.Errorf("open %d = (turn %d, idx %d), want (turn %d, idx %d)", i, o.turn, o.idx, assistantTurn, i)
		}
	}
	if rec.opens[0].tc.Path == rec.opens[1].tc.Path {
		t.Error("the two calls share one result file")
	}
	if rec.comps[0].number != 1 || rec.comps[1].number != 2 {
		t.Errorf("completes out of issue order: %+v", rec.comps)
	}
	b0, _ := os.ReadFile(rec.opens[0].tc.Path)
	b1, _ := os.ReadFile(rec.opens[1].tc.Path)
	if string(b0) != "one\n" || string(b1) != "two\n" {
		t.Errorf("files = %q / %q, want one\\n / two\\n", b0, b1)
	}
	// Both offsets are stored per call, measured on that call's own file.
	if rec.comps[0].out.Bytes != 4 || rec.comps[1].out.Bytes != 4 {
		t.Errorf("sizes = %d / %d, want 4/4", rec.comps[0].out.Bytes, rec.comps[1].out.Bytes)
	}
}

// A padded "action was not executed" observation exports raw_output "" - a
// command that never ran claims no file. The padding comes from rejection
// today: a refused batch reaches format_observation_messages with no results.
func TestPaddedObservationHasNoFile(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("rejected batch",
			model.Action{Command: "echo one", ToolCallID: "call_1"},
			model.Action{Command: "echo never", ToolCallID: "call_2"}),
		done("bye"),
	}}
	l, rec := newRecorderLoop(t, m, &env.Local{Timeout: 5 * time.Second})
	l.Hooks = &stubHooks{rejectOnce: true, rejectWith: &model.Interrupt{
		Kind: model.KindUserInterruption,
		Messages: []model.Message{{
			"role":    "user",
			"content": "nope",
			"extra":   map[string]any{"interrupt_type": "UserRejection", model.UserTypedKey: true},
		}},
	}}
	if _, err := run(t, l, "reject the batch"); err != nil {
		t.Fatal(err)
	}
	if len(rec.opens) != 0 {
		t.Fatalf("opens = %d, want 0: a rejected action never starts, so no call row exists", len(rec.opens))
	}
	traj, _ := l.Save("")
	raw, _ := json.Marshal(traj["messages"])
	if strings.Contains(string(raw), `"raw_output":"echo never`) {
		t.Error("unexecuted action claims output")
	}
	var msgs []map[string]any
	json.Unmarshal(raw, &msgs)
	padded := 0
	for _, mm := range msgs {
		if model.IsToolResult(mm) {
			extra := mm["extra"].(map[string]any)
			if extra["exception_info"] == "action was not executed" {
				if extra["raw_output"] != "" || extra["tcr_path"] != "" {
					t.Errorf("padded observation claims a file: %#v", extra)
				}
				padded++
			}
		}
	}
	if padded != 2 {
		t.Errorf("padded observations = %d, want 2 (the whole rejected batch)", padded)
	}
}

// Without a Recorder nothing is persisted, but the capture still happens in a
// scratch file, the observation still renders, the trajectory still carries
// raw_output, and the scratch directory is gone when the run ends.
func TestNoRecorderScratchLifecycle(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("echo", "echo hi"),
		done("bye"),
	}}
	cfg := agentConfig(t)
	trajPath := filepath.Join(t.TempDir(), "traj.json")
	cfg.OutputPath = trajPath
	spy := &spyEnv{inner: &env.Local{Timeout: 5 * time.Second}}
	l := &Loop{Cfg: cfg, Model: m, Env: spy}
	peek := &scratchPeeker{seen: map[string]string{}, l: l, spy: spy}
	l.Hooks = peek
	if _, err := run(t, l, "echo hi then finish"); err != nil {
		t.Fatal(err)
	}
	scratchSeen := peek.seen["dir"]
	contents := peek.seen["first file"]
	trajDuringRun := peek.seen["traj"]
	if scratchSeen == "" {
		t.Fatal("no scratch directory was used")
	}
	if _, err := os.Stat(scratchSeen); !os.IsNotExist(err) {
		t.Errorf("scratch dir %s survived the run: %v", scratchSeen, err)
	}
	if contents != "hi\n" {
		t.Errorf("scratch file during run = %q, want %q", contents, "hi\n")
	}
	obs := l.messages[3]
	if !strings.Contains(obs["content"].(string), "hi") {
		t.Errorf("observation did not render the output: %q", obs["content"])
	}
	// The trajectory Run saved at the end of the last step (before the
	// deferred RemoveAll) carries raw_output read from the scratch file.
	if !strings.Contains(trajDuringRun, `"raw_output": "hi\n"`) {
		t.Error("trajectory lost raw_output without a Recorder")
	}
	if peek.seen["writer"] != "*os.File" {
		// The scratch writer was still a real file: the loop needs bytes on
		// disk even when nobody records.
		t.Errorf("scratch writer type = %s, want *os.File", peek.seen["writer"])
	}
}

// scratchPeeker captures, while the run is still in flight (before Run's
// defer clears the directory from disk), the scratch dir, the first result
// file's bytes, the trajectory as saved during the run, and the writer type
// the loop handed to Execute.
func (p *scratchPeeker) OnSubmit(_ context.Context, _ model.Message) error {
	// The completion turn appends no tool result, so this is the last moment
	// inside the run when the trajectory (saved after every step) can be read
	// before Run's defer clears the scratch directory.
	if b, err := os.ReadFile(p.l.Cfg.OutputPath); err == nil {
		p.seen["traj"] = string(b)
	}
	return nil
}

type scratchPeeker struct {
	stubHooks
	seen map[string]string
	l    *Loop
	spy  *spyEnv
}

func (p *scratchPeeker) OnMessage(m model.Message) {
	if _, ok := p.seen["dir"]; !ok {
		p.seen["dir"] = p.l.scratchDir
	}
	if model.IsToolResult(m) {
		ws := p.spy.seenWriters()
		if _, ok := p.seen["first file"]; !ok && len(ws) > 0 {
			if f, ok := ws[0].(*os.File); ok {
				p.seen["writer"] = "*os.File"
				b, _ := os.ReadFile(f.Name())
				p.seen["first file"] = string(b)
			} else {
				p.seen["writer"] = fmt.Sprintf("%T", ws[0])
			}
		}
		if b, err := os.ReadFile(p.l.Cfg.OutputPath); err == nil {
			p.seen["traj"] = string(b)
		}
	}
}
