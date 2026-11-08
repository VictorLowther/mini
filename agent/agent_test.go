package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VictorLowther/mini/config"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

const submitCommand = "echo COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT"

func agentConfig(t *testing.T) *Config {
	t.Helper()
	m, err := config.FromSpec("mini")
	if err != nil {
		t.Fatal(err)
	}
	c := ConfigFromMap(config.Sub(m, "agent"))
	if c.Mode != "confirm" || c.CostLimit != 3.0 {
		t.Fatalf("mini.yaml defaults not picked up: %+v", c)
	}
	c.OutputPath = ""
	return c
}

func newLoop(t *testing.T, m model.Model, e env.Environment, mutate ...func(*Config)) *Loop {
	t.Helper()
	cfg := agentConfig(t)
	for _, fn := range mutate {
		fn(cfg)
	}
	return &Loop{Cfg: cfg, Model: m, Env: e}
}

// turn builds one canned assistant message with a single action.
func turn(content, command string) model.Message {
	return turnActions(content, model.Action{Command: command})
}

func turnActions(content string, actions ...model.Action) model.Message {
	return model.Message{
		"role":    "assistant",
		"content": content,
		"extra": map[string]any{
			"actions": actions,
			"cost":    1.0,
		},
	}
}

func submitted(extra string) string {
	return submitCommand + "\necho '" + extra + "'"
}

func run(t *testing.T, l *Loop, task string) (map[string]any, error) {
	t.Helper()
	return l.Run(context.Background(), task)
}

func TestSuccessfulCompletion(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("I'll echo a message", "echo 'hello world'"),
		turn("Now finishing", submitted("Task completed successfully")),
	}}
	l := newLoop(t, m, &env.Local{Timeout: 5 * time.Second})
	info, err := run(t, l, "Echo hello world then finish")
	if err != nil {
		t.Fatal(err)
	}
	if info["exit_status"] != "Submitted" {
		t.Fatalf("exit_status = %v", info["exit_status"])
	}
	if info["submission"] != "Task completed successfully\n" {
		t.Errorf("submission = %q", info["submission"])
	}
	if l.NCalls != 2 {
		t.Errorf("n_calls = %d, want 2", l.NCalls)
	}
	// system, user, assistant, observation, assistant, exit: the submitting
	// action produces no observation, because DefaultAgent raises out of
	// execute_actions before format_observation_messages runs.
	if got := roles(l.Messages); got != "system,user,assistant,user,assistant,exit" {
		t.Errorf("roles = %s", got)
	}
	if l.Messages[3]["content"] != "<returncode>0</returncode>\n<output>\nhello world\n</output>" {
		t.Errorf("observation = %q", l.Messages[3]["content"])
	}
	if l.Cost != 2.0 {
		t.Errorf("cost = %v, want 2.0", l.Cost)
	}
}

func TestHooksPathRecordsPaddedObservationOnSubmit(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("finishing", submitted("done")),
	}}
	l := newLoop(t, m, &env.Local{Timeout: 5 * time.Second})
	l.Hooks = &stubHooks{}
	info, err := run(t, l, "finish immediately")
	if err != nil {
		t.Fatal(err)
	}
	if info["exit_status"] != "Submitted" {
		t.Fatalf("info = %#v", info)
	}
	// InteractiveAgent formats observations in its finally block, so the
	// submitting action shows up as "action was not executed".
	if got := roles(l.Messages); got != "system,user,assistant,user,exit" {
		t.Errorf("roles = %s", got)
	}
	obs, _ := l.Messages[3]["content"].(string)
	if !strings.Contains(obs, "action was not executed") {
		t.Errorf("observation = %q", obs)
	}
}

func roles(msgs []model.Message) string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		r, _ := m["role"].(string)
		out = append(out, r)
	}
	return strings.Join(out, ",")
}

func TestStepLimit(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("First command", "echo 'step1'"),
		turn("Second command", "echo 'step2'"),
	}}
	l := newLoop(t, m, &env.Local{Timeout: 5 * time.Second}, func(c *Config) { c.StepLimit = 1 })
	info, _ := run(t, l, "Run multiple commands")
	if info["exit_status"] != "LimitsExceeded" {
		t.Fatalf("exit_status = %v", info["exit_status"])
	}
	if l.NCalls != 1 {
		t.Errorf("n_calls = %d, want 1", l.NCalls)
	}
	last := l.Messages[len(l.Messages)-1]
	if last["content"] != "LimitsExceeded" {
		t.Errorf("last message = %#v", last)
	}
}

func TestCostLimit(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{turn("Test", "echo 'test'")}}
	l := newLoop(t, m, &env.Local{Timeout: 5 * time.Second}, func(c *Config) { c.CostLimit = 0.5 })
	info, _ := run(t, l, "Test cost limit")
	if info["exit_status"] != "LimitsExceeded" {
		t.Errorf("exit_status = %v", info["exit_status"])
	}
	if l.NCalls != 1 {
		t.Errorf("the first call is billed, the second must be refused: n_calls = %d", l.NCalls)
	}
}

func TestWallTimeLimit(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("slow", "/sleep 1.2"),
		turn("never", "echo never-reached"),
	}}
	l := newLoop(t, m, &env.Local{Timeout: 5 * time.Second}, func(c *Config) { c.WallTimeLimitSeconds = 1 })
	stub := &stubHooks{}
	l.Hooks = stub
	info, _ := run(t, l, "Test wall time limit")
	if info["exit_status"] != "TimeExceeded" {
		t.Fatalf("exit_status = %v", info["exit_status"])
	}
	if stub.limitPrompts != 0 {
		t.Errorf("a wall-clock limit must never prompt for new limits (it would loop forever)")
	}
	vars := l.TemplateVars()
	if vars["elapsed_seconds"].(int) < 1 {
		t.Errorf("elapsed_seconds = %v", vars["elapsed_seconds"])
	}
}

func TestTimeoutHandling(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("Long sleep", "sleep 5"),
		turn("Quick finish", submitted("recovered")),
	}}
	l := newLoop(t, m, &env.Local{Timeout: time.Second})
	info, err := run(t, l, "Test timeout handling")
	if err != nil {
		t.Fatal(err)
	}
	if info["exit_status"] != "Submitted" || info["submission"] != "recovered\n" {
		t.Fatalf("info = %#v", info)
	}
	timedOut := 0
	for _, msg := range l.Messages {
		if c, ok := msg["content"].(string); ok && strings.Contains(c, "timed out") {
			timedOut++
		}
	}
	if timedOut != 1 {
		t.Errorf("%d messages mention the timeout, want 1", timedOut)
	}
}

func TestTimeoutCapturesPartialOutput(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("Output then sleep", "echo $((111*9)); sleep 10"),
		turn("Quick finish", submitted("recovered")),
	}}
	l := newLoop(t, m, &env.Local{Timeout: time.Second})
	info, _ := run(t, l, "Test timeout with partial output")
	if info["submission"] != "recovered\n" {
		t.Fatalf("info = %#v", info)
	}
	var obs string
	for _, msg := range l.Messages {
		if c, ok := msg["content"].(string); ok && strings.Contains(c, "timed out") {
			obs = c
		}
	}
	if !strings.Contains(obs, "999") {
		t.Errorf("partial output missing from observation: %q", obs)
	}
	if !strings.Contains(obs, "999") {
		t.Errorf("partial output missing: %q", obs)
	}
	// Rule 4: the timeout reaches the message extra as exception_type, the way
	// tests/environments/test_local.py asserts it.
	found := false
	for _, msg := range l.Messages {
		if e, ok := msg["extra"].(map[string]any); ok && e["exception_type"] == "TimeoutExpired" {
			found = true
		}
	}
	if !found {
		t.Error("no observation message carries exception_type=TimeoutExpired")
	}
}

func TestMultipleStepsBeforeCompletion(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("Step 1", "echo 'first'"),
		turn("Step 2", "echo 'second'"),
		turn("Step 3", "echo 'third'"),
		turn("Final step", submitted("completed all steps")),
	}}
	l := newLoop(t, m, &env.Local{Timeout: 5 * time.Second}, func(c *Config) { c.CostLimit = 5 })
	info, _ := run(t, l, "Multi-step task")
	if info["exit_status"] != "Submitted" || info["submission"] != "completed all steps\n" || l.NCalls != 4 {
		t.Fatalf("info = %#v n_calls = %d", info, l.NCalls)
	}
}

func TestCustomTemplates(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{turn("Test response", submitted("custom config works"))}}
	l := newLoop(t, m, &env.Local{Timeout: 5 * time.Second}, func(c *Config) {
		c.SystemTemplate = "You are a test assistant."
		c.InstanceTemplate = "Task: {{.task}}. Return bash command."
		c.StepLimit = 2
		c.CostLimit = 1.0
	})
	info, _ := run(t, l, "Test custom config")
	if info["submission"] != "custom config works\n" {
		t.Fatalf("info = %#v", info)
	}
	if l.Messages[0]["content"] != "You are a test assistant." {
		t.Errorf("system = %v", l.Messages[0]["content"])
	}
	if !strings.Contains(l.Messages[1]["content"].(string), "Test custom config") {
		t.Errorf("instance = %v", l.Messages[1]["content"])
	}
}

func TestTemplateVarsReachTemplates(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("a", "echo one"), turn("b", "echo two"),
		turn("stop", submitted("x")),
	}}
	l := newLoop(t, m, &env.Local{Timeout: 5 * time.Second}, func(c *Config) {
		c.InstanceTemplate = `calls={{.n_model_calls}} cost={{.model_cost}} task={{.task}} system={{.system}} custom={{.custom}}`
		c.SystemTemplate = `{{ charlen "abcd" }}`
	})
	if _, err := l.Run(context.Background(), "T", map[string]any{"custom": "yes"}); err != nil {
		t.Fatal(err)
	}
	want := "calls=0 cost=0 task=T system=" + env.Uname()["system"] + " custom=yes"
	if got := l.Messages[1]["content"]; got != want {
		t.Errorf("instance template =\n%q\nwant\n%q", got, want)
	}
	if l.Messages[0]["content"] != "4" {
		t.Errorf("system = %v", l.Messages[0]["content"])
	}
	vars := l.TemplateVars(map[string]any{"kwargs_only": "v"})
	for _, k := range []string{"n_model_calls", "model_cost", "elapsed_seconds", "system", "machine", "task", "cwd", "kwargs_only"} {
		if _, ok := vars[k]; !ok {
			t.Errorf("template var %q missing", k)
		}
	}
	// The whole process environment is merged in (rule 5).
	if vars["PATH"] == nil {
		t.Error("process environment must reach the template vars")
	}
}

func TestEmptyActionsContinueTheRun(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("No actions here"),
		turn("Now with action", submitted("done")),
	}}
	l := newLoop(t, m, &env.Local{Timeout: 5 * time.Second})
	info, _ := run(t, l, "Test empty actions")
	if info["exit_status"] != "Submitted" || info["submission"] != "done\n" {
		t.Fatalf("info = %#v", info)
	}
	if l.NCalls != 2 {
		t.Errorf("n_calls = %d", l.NCalls)
	}
	if got := roles(l.Messages); got != "system,user,assistant,assistant,exit" {
		t.Errorf("roles = %s, want no observation for an actionless turn", got)
	}
}

func TestRepeatedFormatErrorsTerminate(t *testing.T) {
	outputs := make([]model.Message, 5)
	for i := range outputs {
		outputs[i] = formatErrorTurn()
	}
	m := &model.Deterministic{CostPerCall: 1, Outputs: outputs}
	l := newLoop(t, m, &env.Local{Timeout: 5 * time.Second}, func(c *Config) { c.MaxConsecutiveFormatErrors = 2 })
	info, _ := run(t, l, "Test repeated format errors")
	if info["exit_status"] != "RepeatedFormatError" {
		t.Fatalf("exit_status = %v", info["exit_status"])
	}
	if l.NCalls != 2 {
		t.Errorf("n_calls = %d, want to stop at the 2nd consecutive error", l.NCalls)
	}
	// instance message, one user message per format error, then the exit.
	if got := roles(l.Messages); got != "system,user,user,user,exit" {
		t.Errorf("roles = %s", got)
	}
}

func TestFormatErrorCounterResetsOnSuccess(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		formatErrorTurn(),
		turn("listing", "echo hello"),
		formatErrorTurn(),
		turn("done", submitted("ok")),
	}}
	l := newLoop(t, m, &env.Local{Timeout: 5 * time.Second}, func(c *Config) { c.MaxConsecutiveFormatErrors = 2 })
	info, _ := run(t, l, "Test counter reset")
	if info["exit_status"] != "Submitted" || info["submission"] != "ok\n" {
		t.Fatalf("info = %#v", info)
	}
}

func TestFormatErrorsCountAgainstCostLimit(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: repeatTurn(billedFormatErrorTurn(), 10)}
	l := newLoop(t, m, &env.Local{Timeout: 5 * time.Second}, func(c *Config) {
		c.CostLimit = 2.5
		c.StepLimit = 8
		c.MaxConsecutiveFormatErrors = 0
	})
	info, _ := run(t, l, "Test billed format errors")
	if info["exit_status"] != "LimitsExceeded" {
		t.Fatalf("exit_status = %v", info["exit_status"])
	}
	if l.NCalls != 3 || l.Cost != 3.0 {
		t.Errorf("n_calls = %d cost = %v, want 3 / 3.0", l.NCalls, l.Cost)
	}
}

func formatErrorTurn() model.Message {
	return model.Message{
		"role":    "assistant",
		"content": "",
		"extra":   map[string]any{"format_error": true},
	}
}

// billedFormatErrorTurn is the real model's behaviour: the call is charged
// before parsing fails.
func billedFormatErrorTurn() model.Message {
	m := formatErrorTurn()
	model.Extra(m)["billed"] = true
	model.Extra(m)["cost"] = 1.0
	return m
}

func repeatTurn(m model.Message, n int) []model.Message {
	out := make([]model.Message, n)
	for i := range out {
		out[i] = m
	}
	return out
}

func TestUncaughtErrorEndsRun(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{{
		"role":    "assistant",
		"content": "",
		"extra":   map[string]any{"raise": "model exploded"},
	}}}
	l := newLoop(t, m, &env.Local{Timeout: 5 * time.Second})
	_, err := run(t, l, "Test uncaught")
	if err == nil || !strings.Contains(err.Error(), "model exploded") {
		t.Fatalf("err = %v", err)
	}
	last := l.Messages[len(l.Messages)-1]
	if last["role"] != "exit" {
		t.Fatalf("last = %#v", last)
	}
	extra := model.Extra(last)
	if extra["exit_status"] != "RuntimeError" || extra["exception_str"] != "model exploded" {
		t.Errorf("extra = %#v", extra)
	}
	if extra["traceback"] == "" || !strings.Contains(extra["traceback"].(string), "agent") {
		t.Errorf("traceback = %v", extra["traceback"])
	}
}

func TestSaveWritesTrajectoryAfterEachStep(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "nested", "traj.json")
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("a", "echo one"), turn("b", submitted("two"))}}
	l := newLoop(t, m, &env.Local{Timeout: 5 * time.Second}, func(c *Config) { c.OutputPath = out })
	run(t, l, "save test")

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var traj map[string]any
	if err := json.Unmarshal(data, &traj); err != nil {
		t.Fatal(err)
	}
	info := traj["info"].(map[string]any)
	if info["exit_status"] != "Submitted" || info["submission"] != "two\n" {
		t.Errorf("info = %#v", info)
	}
	if info["mini_version"] != "mini-go/"+Version {
		t.Errorf("mini_version = %v", info["mini_version"])
	}
	if traj["trajectory_format"] != "mini-swe-agent-1.1" {
		t.Errorf("trajectory_format = %v", traj["trajectory_format"])
	}
	stats := info["model_stats"].(map[string]any)
	if stats["api_calls"].(float64) != 2 || stats["instance_cost"].(float64) != 2 {
		t.Errorf("model_stats = %#v", stats)
	}
	cfg := info["config"].(map[string]any)
	for _, k := range []string{"agent", "agent_type", "model", "model_type", "environment", "environment_type"} {
		if _, ok := cfg[k]; !ok {
			t.Errorf("info.config.%s missing", k)
		}
	}
	if cfg["agent_type"] != "mini/agent.Loop" || cfg["model_type"] != "mini/model.Deterministic" || cfg["environment_type"] != "mini/env.Local" {
		t.Errorf("types = %v %v %v", cfg["agent_type"], cfg["model_type"], cfg["environment_type"])
	}
	agentCfg := cfg["agent"].(map[string]any)
	if agentCfg["system_template"] == "" || agentCfg["mode"] != "confirm" {
		t.Errorf("agent config = %#v", agentCfg)
	}
	envCfg := cfg["environment"].(map[string]any)
	if envCfg["timeout"].(float64) != 5 {
		t.Errorf("environment config = %#v", envCfg)
	}
	msgs := traj["messages"].([]any)
	if len(msgs) != 6 {
		t.Errorf("%d messages", len(msgs))
	}
	// The submitting turn's raw output must still be in the trajectory for the
	// model message.
	assistant := msgs[2].(map[string]any)
	if _, ok := assistant["extra"].(map[string]any)["actions"]; !ok {
		t.Errorf("assistant extra lost its actions: %#v", assistant["extra"])
	}
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temporary file was left behind")
	}
}

func TestTurnCancellationInterruptsButKeepsTheRunAlive(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("long", "sleep 20"),
		turn("b", submitted("back again")),
	}}
	l := newLoop(t, m, &env.Local{Timeout: 30 * time.Second})
	l.Hooks = &stubHooks{}
	go func() {
		time.Sleep(150 * time.Millisecond)
		l.CancelTurn()
	}()
	info, err := run(t, l, "cancel mid-command")
	if err != nil {
		t.Fatal(err)
	}
	if info["exit_status"] != "Submitted" {
		t.Fatalf("info = %#v", info)
	}
	if got := roles(l.Messages); got != "system,user,assistant,user,user,assistant,user,exit" {
		t.Errorf("roles = %s", got)
	}
	if !strings.Contains(contents(l.Messages)[4], "Temporary interruption caught") {
		t.Errorf("messages = %v", contents(l.Messages))
	}
}

func TestWholeRunCancellationEndsTheLoop(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{turn("long", "sleep 20")}}
	l := newLoop(t, m, &env.Local{Timeout: 30 * time.Second})
	l.Hooks = &stubHooks{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, err := l.Run(ctx, "cancel the whole run")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v (%T)", err, err)
	}
	if !strings.Contains(contents(l.Messages)[4], "Temporary interruption caught") {
		t.Errorf("messages = %v", contents(l.Messages))
	}
	if !strings.Contains(contents(l.Messages)[3], "action was not executed") {
		t.Errorf("the partial observation is missing: %v", contents(l.Messages))
	}
}

// --- stub hooks: the loop's contract with the interactive layer ---

type stubHooks struct {
	limitPrompts  int
	beforeCalls   int
	submitPrompts int
	rejectWith    error
	submitWith    error
	rejectOnce    bool
	liftLimits    bool
}

func (s *stubHooks) NextHumanCommand(context.Context) (string, bool) { return "", false }
func (s *stubHooks) OnInterrupt(ctx context.Context) error {
	return &model.Interrupt{Kind: model.KindUserInterruption, Messages: []model.Message{{
		"role":    "user",
		"content": "Temporary interruption caught.",
		"extra":   map[string]any{"interrupt_type": model.KindUserInterruption},
	}}}
}
func (s *stubHooks) OnMessage(model.Message) {}

func (s *stubHooks) BeforeExecute(context.Context, []model.Action, map[string]any) error {
	s.beforeCalls++
	err := s.rejectWith
	if s.rejectOnce {
		s.rejectWith = nil
	}
	return err
}

func (s *stubHooks) OnSubmit(context.Context, model.Message) error {
	s.submitPrompts++
	if s.submitWith != nil {
		return s.submitWith
	}
	return s.rejectWith
}

func (s *stubHooks) OnLimits(_ context.Context, cfg *Config, _ *model.Interrupt) error {
	s.limitPrompts++
	if s.liftLimits {
		cfg.CostLimit = 10
		cfg.StepLimit = 10
	}
	return nil
}

func TestRejectionSkipsExecutionButRecordsObservations(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("a", "echo not-run"),
		turn("b", submitted("done")),
	}}
	l := newLoop(t, m, &env.Local{Timeout: 5 * time.Second})
	l.Hooks = &stubHooks{
		rejectOnce: true,
		rejectWith: &model.Interrupt{
			Kind: model.KindUserInterruption,
			Messages: []model.Message{{
				"role":    "user",
				"content": "Commands not executed. The user rejected your commands with the following message: nope",
				"extra":   map[string]any{"interrupt_type": "UserRejection"},
			}},
		},
	}
	info, err := run(t, l, "reject test")
	if err != nil {
		t.Fatal(err)
	}
	if info["exit_status"] != "Submitted" {
		t.Fatalf("info = %#v", info)
	}
	// instance, assistant, padded observation, rejection, assistant, padded
	// observation (the submitting turn gets one under the interactive path), exit
	if got := roles(l.Messages); got != "system,user,assistant,user,user,assistant,user,exit" {
		t.Errorf("roles = %s\nmessages: %v", got, contents(l.Messages))
	}
	if !strings.Contains(l.Messages[3]["content"].(string), "action was not executed") {
		t.Errorf("padded observation missing: %v", l.Messages[3]["content"])
	}
	if !strings.Contains(l.Messages[4]["content"].(string), "user rejected your commands") {
		t.Errorf("rejection message missing")
	}
	if strings.Contains(l.Messages[3]["content"].(string), "not-run") {
		t.Error("the rejected command must not have been executed")
	}
}

func contents(msgs []model.Message) []string {
	out := []string{}
	for _, m := range msgs {
		c, _ := m["content"].(string)
		out = append(out, stringOr(m["role"])+": "+c[:min(40, len(c))])
	}
	return out
}

func TestLimitsHookCanLiftTheCeiling(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("a", "echo one"), turn("b", submitted("two"))}}
	l := newLoop(t, m, &env.Local{Timeout: 5 * time.Second}, func(c *Config) { c.CostLimit = 0.5 })
	h := &stubHooks{liftLimits: true}
	l.Hooks = h
	info, err := run(t, l, "lift limits")
	if err != nil {
		t.Fatal(err)
	}
	if h.limitPrompts != 1 {
		t.Errorf("limit prompts = %d, want 1", h.limitPrompts)
	}
	if info["exit_status"] != "Submitted" {
		t.Errorf("info = %#v", info)
	}
}

func TestOnSubmitCanStartANewTask(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("a", submitted("first")),
		turn("b", submitted("second")),
	}}
	l := newLoop(t, m, &env.Local{Timeout: 5 * time.Second})
	h := &newTaskHooks{once: true}
	l.Hooks = h
	info, err := run(t, l, "continue test")
	if err != nil {
		t.Fatal(err)
	}
	if info["submission"] != "second\n" {
		t.Errorf("info = %#v", info)
	}
	if !strings.Contains(contents(l.Messages)[4], "The user added a new task") {
		t.Errorf("messages = %v", contents(l.Messages))
	}
}

type newTaskHooks struct {
	stubHooks
	once bool
}

func (h *newTaskHooks) OnSubmit(context.Context, model.Message) error {
	if !h.once {
		return nil
	}
	h.once = false
	return &model.Interrupt{Kind: model.KindUserInterruption, Messages: []model.Message{{
		"role":    "user",
		"content": "The user added a new task: do it again",
		"extra":   map[string]any{"interrupt_type": "UserNewTask"},
	}}}
}
