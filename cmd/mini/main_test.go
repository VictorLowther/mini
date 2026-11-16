package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/model"
	"github.com/VictorLowther/mini/session"
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
		"-c", "mini", "-c", "agent.context_window=4",
		"-o", "/tmp/t.json", "--exit-immediately", "--print",
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
	if strings.Join(o.configSpecs, ",") != "mini,agent.context_window=4" {
		t.Errorf("configs = %v", o.configSpecs)
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
	if cfg.Mode != "confirm" || cfg.ContextWindow != 262144 || cfg.SystemTemplate == "" {
		t.Errorf("agent config = %+v", cfg)
	}
	if config := sub(merged, "model"); config == nil {
		t.Error("model section missing")
	}
}

func TestBuildConfigSpecsReplaceTheDefault(t *testing.T) {
	// A deliberate and surprising semantic, pinned here so it stays chosen
	// rather than accidental: naming -c replaces the default config file, it
	// does not layer on top of it.
	o, err := parseFlags([]string{"-c", "agent.context_window=7"})
	if err != nil {
		t.Fatal(err)
	}
	merged, _, err := buildConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	cfg := agent.ConfigFromMap(sub(merged, "agent"))
	if cfg.ContextWindow != 7 {
		t.Errorf("context_window = %d", cfg.ContextWindow)
	}
	if cfg.SystemTemplate != "" {
		t.Error("mini.yaml must not have been merged in")
	}
}

func TestBuildConfigCLIFlagsWin(t *testing.T) {
	o, err := parseFlags([]string{"-y", "-m", "chosen", "--url", "http://sushi:1234", "-t", "task text"})
	if err != nil {
		t.Fatal(err)
	}
	merged, _, err := buildConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	cfg := agent.ConfigFromMap(sub(merged, "agent"))
	if cfg.Mode != "yolo" {
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

// TestProfileFlagsWritePprofFiles is the plan's CLI check for the
// investigation tooling: -cpuprofile and -memprofile must bracket the whole
// run and land as real pprof files (gzip-framed protobuf, what every pprof
// reader expects) whatever else the run does.
func TestProfileFlagsWritePprofFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MINI_STATE_DIR", t.TempDir())
	t.Setenv("MINI_GLOBAL_CONFIG_DIR", dir)
	t.Setenv("MINI_CONFIGURED", "true")
	t.Setenv("MINI_MODEL_NAME", "test-model")
	t.Setenv("MINI_SILENT_STARTUP", "1")
	srv := fixtureServer(t, "toolcall.sse", "submit.sse")
	defer srv.Close()

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout, err := os.CreateTemp(t.TempDir(), "mini-out")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()

	cpu := filepath.Join(dir, "cpu.pprof")
	heap := filepath.Join(dir, "heap.pprof")
	code := run([]string{"-p", "-y", "--exit-immediately", "-t", "profiled run",
		"--url", srv.URL, "-o", filepath.Join(dir, "traj.json"),
		"-cpuprofile", cpu, "-memprofile", heap}, devnull, stdout, os.Stderr)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	for _, path := range []string{cpu, heap} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		// The gzip frame is the only claim worth making without a decoder
		// import: content validation is pprof's own readers' job.
		if len(b) < 32 || b[0] != 0x1f || b[1] != 0x8b {
			t.Errorf("%s: %d bytes, magic %x: not a pprof profile", path, len(b), b[:min(2, len(b))])
		}
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

// modelInfoFixtureServer is fixtureServer with a /v1/models answer, so a run
// can be pointed at a server that reports a context window (or one that does
// not, which is what fixtureServer's 404 already is).
func modelInfoFixtureServer(t *testing.T, modelInfo string, names ...string) *httptest.Server {
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
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, modelInfo)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
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

// runToEndTraj runs --print to completion against srvURL and hands back the
// trajectory's info.config, where both halves of the context-window decision
// land: agent.context_window (what the loop compacted against) and
// model.context_window_source (who said so).
func runToEndTraj(t *testing.T, srvURL, out string, extraArgs ...string) map[string]any {
	t.Helper()
	t.Setenv("MINI_STATE_DIR", t.TempDir())
	t.Setenv("MINI_GLOBAL_CONFIG_DIR", t.TempDir())
	t.Setenv("MINI_CONFIGURED", "true")
	t.Setenv("MINI_MODEL_NAME", "test-model")
	t.Setenv("MINI_SILENT_STARTUP", "1")
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout, err := os.CreateTemp(t.TempDir(), "mini-out")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	code := run(append([]string{
		"-p", "-y", "-m", "test-model", "--url", srvURL,
		"-t", "run 'echo hi', verify, then submit", "-o", out, "--exit-immediately",
	}, extraArgs...), devnull, stdout, os.Stderr)
	data, _ := os.ReadFile(out)
	if code != 0 {
		t.Fatalf("exit code %d, output:\n%s", code, data)
	}
	var traj struct {
		Info struct {
			Config map[string]any `json:"config"`
		} `json:"info"`
	}
	if err := json.Unmarshal(data, &traj); err != nil {
		t.Fatal(err)
	}
	return traj.Info.Config
}

func section(cfg map[string]any, name string) map[string]any {
	m, _ := cfg[name].(map[string]any)
	return m
}

// TestRunTakesTheContextWindowFromTheServer: the shipped config's
// context_window is a fallback, not the answer. A session running against a
// server serving 1M has to compact against 1M, or the trigger fires while the
// context still has three quarters of the window left.
func TestRunTakesTheContextWindowFromTheServer(t *testing.T) {
	srv := modelInfoFixtureServer(t,
		`{"object":"list","data":[{"id":"test-model","context_length":1048576,"loaded":true,"state":"ready"}]}`,
		"toolcall.sse", "submit.sse")
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "server.traj.json")
	cfg := runToEndTraj(t, srv.URL, out)

	if got := section(cfg, "agent")["context_window"]; got != float64(1048576) {
		t.Errorf("agent.context_window = %v, want the server's 1048576", got)
	}
	if got := section(cfg, "model")["context_window"]; got != float64(1048576) {
		t.Errorf("model.context_window = %v", got)
	}
	if got := section(cfg, "model")["context_window_source"]; got != model.ContextFromServer {
		t.Errorf("model.context_window_source = %v, want %q", got, model.ContextFromServer)
	}
}

// TestRunFallsBackToTheConfiguredWindow: the servers that answer /v1/models
// with nothing but ids are the OpenAI standard, not a bug - and a server that
// is not listening at all must not stop a run from starting either.
func TestRunFallsBackToTheConfiguredWindow(t *testing.T) {
	srv := fixtureServer(t, "toolcall.sse", "submit.sse") // 404s /v1/models
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "config.traj.json")
	cfg := runToEndTraj(t, srv.URL, out)

	want := agent.ConfigFromMap(nil).ContextWindow
	if want != 0 {
		t.Fatalf("the default local config is no longer 0; this test's premise moved (%d)", want)
	}
	// mini.yaml's own value, which the embedded config sets on purpose.
	if got := section(cfg, "agent")["context_window"]; got != float64(262144) {
		t.Errorf("agent.context_window = %v, want the configured 262144", got)
	}
	if got := section(cfg, "model")["context_window_source"]; got != model.ContextFromConfig {
		t.Errorf("model.context_window_source = %v, want %q", got, model.ContextFromConfig)
	}
}

// TestRunTakesAnExplicitlyConfiguredWindowOnlyWhenAsked: -c on the command
// line is the local fallback, and the server still outranks it - the operator
// who wants the config number wins by pointing at a server that does not say.
// TestServerWindowOutranksAConfiguredOverride: -c agent.context_window=... is
// the local fallback for a server that will not say, not a pin on the server's
// answer. The operator who wants a smaller working set uses
// compact_threshold_ratio, which keeps the trigger's relation to the real
// window honest.
func TestServerWindowOutranksAConfiguredOverride(t *testing.T) {
	srv := modelInfoFixtureServer(t,
		`{"object":"list","data":[{"id":"test-model","context_length":1048576}]}`,
		"toolcall.sse", "submit.sse")
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "override.traj.json")
	cfg := runToEndTraj(t, srv.URL, out, "-c", "mini", "-c", "agent.context_window=32768")

	if got := section(cfg, "agent")["context_window"]; got != float64(1048576) {
		t.Errorf("agent.context_window = %v, want the server's number to outrank the config", got)
	}
	if got := section(cfg, "model")["context_window_source"]; got != model.ContextFromServer {
		t.Errorf("model.context_window_source = %v, want %q", got, model.ContextFromServer)
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

// TestApplyConcurrencyChain covers the knob's precedence: default, then the
// stored preference a UI writes, then an explicit config value for this run.
// It restores the shared DefaultGate afterwards because the bound is
// process-wide state and no other test should inherit this one's opinion.
func TestApplyConcurrencyChain(t *testing.T) {
	t.Cleanup(func() { model.DefaultGate.SetLimit(model.DefaultMaxConcurrentRequests) })

	dir := t.TempDir()
	store, err := session.Open(filepath.Join(dir, "sessions.db"), filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// 1. Nobody has said anything: the shipped default, and nothing written.
	applyConcurrency(store, map[string]any{})
	if got := model.DefaultGate.Limit(); got != model.DefaultMaxConcurrentRequests {
		t.Errorf("default limit = %d, want %d", got, model.DefaultMaxConcurrentRequests)
	}
	if _, ok, _ := store.Setting(settingMaxConcurrentRequests); ok {
		t.Error("a default got stored; a preference nobody expressed must stay absent")
	}

	// 2. The stored preference wins over the default.
	if err := store.SetSetting(settingMaxConcurrentRequests, "2"); err != nil {
		t.Fatal(err)
	}
	applyConcurrency(store, map[string]any{})
	if got := model.DefaultGate.Limit(); got != 2 {
		t.Errorf("limit from the stored setting = %d, want 2", got)
	}

	// 3. And an explicit config value wins over the stored preference: a script
	//    that passes -c model.max_concurrent_requests=1 means it for this run.
	applyConcurrency(store, map[string]any{"max_concurrent_requests": 1})
	if got := model.DefaultGate.Limit(); got != 1 {
		t.Errorf("limit from config = %d, want 1", got)
	}

	// 4. Garbage in the settings row is not an opinion, and it must not crash a
	//    startup or wedge the gate at zero.
	if err := store.SetSetting(settingMaxConcurrentRequests, "lots"); err != nil {
		t.Fatal(err)
	}
	applyConcurrency(store, map[string]any{})
	if got := model.DefaultGate.Limit(); got != model.DefaultMaxConcurrentRequests {
		t.Errorf("limit with an unparseable setting = %d, want the default", got)
	}
	if err := store.SetSetting(settingMaxConcurrentRequests, "0"); err != nil {
		t.Fatal(err)
	}
	applyConcurrency(store, map[string]any{})
	if got := model.DefaultGate.Limit(); got < 1 {
		t.Errorf("a stored zero produced limit %d; the gate must never be closed", got)
	}
}
