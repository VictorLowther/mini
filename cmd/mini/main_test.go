package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/model"
)

func fixtureServer(t *testing.T, names ...string) *httptest.Server {
	t.Helper()
	return serveFixtures(t, nil, names...)
}

// serveFixtures is fixtureServer with an optional recorder: when roles is
// non-nil, every request's message roles are appended to it, so a test can
// assert what history a resumed run actually put on the wire.
func serveFixtures(t *testing.T, roles *[][]string, names ...string) *httptest.Server {
	t.Helper()
	var bodies [][]byte
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sushi", n))
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, b)
	}
	i := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if roles != nil {
			var req struct {
				Messages []struct {
					Role string `json:"role"`
				} `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			got := make([]string, 0, len(req.Messages))
			for _, m := range req.Messages {
				got = append(got, m.Role)
			}
			*roles = append(*roles, got)
		}
		body := bodies[len(bodies)-1]
		if i < len(bodies) {
			body = bodies[i]
		}
		i++
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(body)
	}))
}

func TestParseFlags(t *testing.T) {
	o, err := parseFlags([]string{
		"-t", "do the thing", "-y", "-m", "qwen", "--url", "http://h:1",
		"--max-tokens", "2048", "--thinking", "low",
		"-c", "mini", "-c", "agent.step_limit=4",
		"-o", "/tmp/t.json", "-l", "0", "--exit-immediately", "--print",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.task != "do the thing" || !o.yolo || o.modelName != "qwen" || o.url != "http://h:1" {
		t.Errorf("opts = %+v", o)
	}
	if o.maxTokens != 2048 || o.thinking != "low" || o.output != "/tmp/t.json" {
		t.Errorf("opts = %+v", o)
	}
	if !o.exitImmediately || !o.printMode {
		t.Errorf("bool flags = %+v", o)
	}
	if strings.Join(o.configSpecs, ",") != "mini,agent.step_limit=4" {
		t.Errorf("configs = %v", o.configSpecs)
	}
	if !o.costLimit.set || o.costLimit.v != 0 {
		t.Errorf("cost limit must distinguish 0 from unset, got %+v", o.costLimit)
	}
}

func TestParseFlagsRejectsUnknown(t *testing.T) {
	if _, err := parseFlags([]string{"--nope"}); err == nil {
		t.Error("an unknown flag must fail")
	}
}

func TestBuildConfigWithoutSpecsUsesEmbeddedMini(t *testing.T) {
	t.Setenv("MINI_CONFIG_PATH", "")
	o, err := parseFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	merged, name, err := buildConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	if name != "mini" {
		t.Errorf("config name = %q", name)
	}
	cfg := agent.ConfigFromMap(sub(merged, "agent"))
	if cfg.Mode != "confirm" || cfg.CostLimit != 3.0 || cfg.SystemTemplate == "" {
		t.Errorf("agent config = %+v", cfg)
	}
	if config := sub(merged, "model"); config == nil {
		t.Error("model section missing")
	}
}

func TestBuildConfigSpecsReplaceTheDefault(t *testing.T) {
	// This is the loud typer semantic the reference warns about: -c X drops the
	// default file entirely.
	o, err := parseFlags([]string{"-c", "agent.step_limit=7"})
	if err != nil {
		t.Fatal(err)
	}
	merged, _, err := buildConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	cfg := agent.ConfigFromMap(sub(merged, "agent"))
	if cfg.StepLimit != 7 {
		t.Errorf("step_limit = %d", cfg.StepLimit)
	}
	if cfg.SystemTemplate != "" {
		t.Error("mini.yaml must not have been merged in")
	}
}

func TestBuildConfigCLIFlagsWin(t *testing.T) {
	o, err := parseFlags([]string{"-y", "-l", "1.25", "-m", "chosen", "--url", "http://sushi:1234", "-t", "task text"})
	if err != nil {
		t.Fatal(err)
	}
	merged, _, err := buildConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	cfg := agent.ConfigFromMap(sub(merged, "agent"))
	if cfg.Mode != "yolo" || cfg.CostLimit != 1.25 {
		t.Errorf("agent = %+v", cfg)
	}
	if str(sub(merged, "model"), "model_name") != "chosen" || str(sub(merged, "model"), "base_url") != "http://sushi:1234" {
		t.Errorf("model = %#v", sub(merged, "model"))
	}
	if str(sub(merged, "run"), "task") != "task text" {
		t.Errorf("run = %#v", sub(merged, "run"))
	}
}

func TestExitImmediatelySetsConfirmExitFalse(t *testing.T) {
	o, _ := parseFlags([]string{"--exit-immediately"})
	merged, _, err := buildConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	if agent.ConfigFromMap(sub(merged, "agent")).ConfirmExit {
		t.Error("--exit-immediately must disable the exit prompt")
	}
}

func TestExitCode(t *testing.T) {
	if got := exitCode(map[string]any{"exit_status": "Submitted"}, nil); got != 0 {
		t.Errorf("submitted exit code = %d", got)
	}
	if got := exitCode(map[string]any{"exit_status": "LimitsExceeded"}, nil); got != 1 {
		t.Errorf("limits exit code = %d", got)
	}
	if got := exitCode(nil, os.ErrClosed); got != 1 {
		t.Errorf("error exit code = %d", got)
	}
}

func TestNewEnvironmentReadsTheConfigSection(t *testing.T) {
	dir := t.TempDir()
	e := newEnvironment(map[string]any{
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

// TestRunPrintModeEndToEnd is the plan's CLI check: --print, --yolo, a fake
// sushi, and a trajectory whose exit_status is Submitted.
func TestRunPrintModeEndToEnd(t *testing.T) {
	srv := fixtureServer(t, "toolcall.sse", "submit.sse")
	t.Cleanup(srv.Close)
	cfgDir := t.TempDir()
	t.Setenv("MINI_STATE_DIR", t.TempDir())
	t.Setenv("MINI_GLOBAL_CONFIG_DIR", cfgDir)
	t.Setenv("MINI_CONFIGURED", "true")
	t.Setenv("MINI_MODEL_NAME", "test-model")
	t.Setenv("MINI_SILENT_STARTUP", "1")
	out := filepath.Join(cfgDir, "out.traj.json")

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout, err := os.CreateTemp(t.TempDir(), "mini-out")
	if err != nil {
		t.Fatal(err)
	}
	code := run([]string{
		"-p", "-y",
		"-t", "run 'echo hi', verify, then submit",
		"--url", srv.URL,
		"-o", out,
		"--exit-immediately",
	}, devnull, stdout, os.Stderr)
	stdout.Close()
	if code != 0 {
		data, _ := os.ReadFile(stdout.Name())
		t.Fatalf("exit code %d, output:\n%s", code, data)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var traj struct {
		Info struct {
			ExitStatus string `json:"exit_status"`
			Submission string `json:"submission"`
			ModelStats struct {
				APICalls float64 `json:"api_calls"`
			} `json:"model_stats"`
			Config map[string]any `json:"config"`
		} `json:"info"`
		Messages         []map[string]any `json:"messages"`
		TrajectoryFormat string           `json:"trajectory_format"`
	}
	if err := json.Unmarshal(data, &traj); err != nil {
		t.Fatal(err)
	}
	if traj.Info.ExitStatus != "Submitted" || traj.Info.Submission != "Everything verified; the task is complete." {
		t.Errorf("info = %+v", traj.Info)
	}
	if traj.TrajectoryFormat != "mini-swe-agent-1.1" {
		t.Errorf("format = %s", traj.TrajectoryFormat)
	}
	// system, user, assistant, tool, assistant, exit: the completion turn
	// makes no tool call, so it has no observation.
	if len(traj.Messages) != 6 {
		t.Errorf("%d messages:\n%s", len(traj.Messages), string(data))
	}
	if traj.Info.ModelStats.APICalls != 2 {
		t.Errorf("api_calls = %v", traj.Info.ModelStats.APICalls)
	}
	for _, key := range []string{"agent", "agent_type", "model", "model_type", "environment", "environment_type"} {
		if _, ok := traj.Info.Config[key]; !ok {
			t.Errorf("info.config.%s missing", key)
		}
	}
	if traj.Info.Config["model_type"] != "mini/model.Sushi" {
		t.Errorf("model_type = %v", traj.Info.Config["model_type"])
	}
	// The tool message must be a real tool result for sushi's template.
	if traj.Messages[3]["role"] != "tool" || traj.Messages[3]["tool_call_id"] == nil {
		t.Errorf("tool message = %#v", traj.Messages[3])
	}
	// Reasoning round-trips: turn 1 streamed reasoning, so the second request's
	// history carries it (proved by the recorded stream containing it).
	if _, ok := traj.Messages[2]["reasoning_content"]; !ok {
		t.Errorf("assistant message lost reasoning_content: %#v", traj.Messages[2])
	}
}

func TestConfigCommandSetUnsetPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MINI_GLOBAL_CONFIG_DIR", dir)
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	if err := runConfigCommand([]string{"set", "MINI_MODEL_NAME", "qwen/test"}, devnull, os.Stdout); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(dir, ".env")
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "MINI_MODEL_NAME='qwen/test'" {
		t.Errorf(".env = %q", data)
	}
	if err := runConfigCommand([]string{"unset", "MINI_MODEL_NAME"}, devnull, os.Stdout); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(envFile)
	if len(strings.TrimSpace(string(data))) != 0 {
		t.Errorf(".env after unset = %q", data)
	}
	if err := runConfigCommand([]string{"bogus"}, devnull, os.Stdout); err == nil {
		t.Error("an unknown config command must fail")
	}
}

func TestDefaultConfigSpecOverride(t *testing.T) {
	t.Setenv("MINI_CONFIG_PATH", "/somewhere/mini.yaml")
	if got := defaultConfigSpec(); got != "/somewhere/mini" {
		t.Errorf("defaultConfigSpec = %q", got)
	}
}

func TestModelDefaultBaseURL(t *testing.T) {
	if model.DefaultBaseURL != "http://127.0.0.1:1234" {
		t.Errorf("sushi runs on 1234 here, got %s", model.DefaultBaseURL)
	}
}

func sub(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
