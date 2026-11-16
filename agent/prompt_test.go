package agent

// TestShippedPromptsStateTheWindow guards the prose against the mechanism.
//
// The system prompt and the observation template both tell the model how much
// of a command's output it can ever see, and that number lives in
// model.MaxViewLines / MaxViewChars. The shipped example used to advertise
// reading 200 lines through a 100-line window, which taught the model to spend
// a turn on a read that came back truncated again - so the prompts interpolate
// the constants (agent.TemplateVars) instead of repeating them, and this test
// fails if either side drifts or if the dead advice comes back.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/VictorLowther/mini/config"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

func TestShippedPromptsStateTheWindow(t *testing.T) {
	m, err := config.FromSpec("../config/configs/mini.yaml")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	l := newLoop(t, &model.Deterministic{}, &env.Local{})
	l.Cfg = ConfigFromMap(config.Sub(m, "agent"))

	vars := l.TemplateVars()
	if vars["view_lines"] != model.MaxViewLines || vars["view_chars"] != model.MaxViewChars {
		t.Fatalf("view bounds not exposed to templates: %v / %v", vars["view_lines"], vars["view_chars"])
	}

	sys, err := l.render("system_template", l.Cfg.SystemTemplate)
	if err != nil {
		t.Fatalf("render system: %v", err)
	}
	for _, want := range []string{"last 100 lines", "10000 characters", "wc -l <path>", "sed -n '1,100p'", "'101,200p'", "grep -c"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	for _, line := range strings.Split(sys, "\n") {
		if strings.Contains(line, "output_path") || strings.Contains(line, "capped") {
			t.Logf("SYS LINE: %.160s", strings.TrimSpace(line))
		}
	}
	// The directive that replaced the wait tool. It lives in the prompt because
	// it is about turn discipline, and it is asserted here because a model that
	// is not told results arrive on their own will poll logs in a loop - the
	// exact behaviour the delivery path exists to remove.
	for _, want := range []string{"Background jobs report themselves", "the run continues until it has been delivered",
		"never poll a log", "not a result owed", "end it with job_kill"} {
		if !strings.Contains(sys, want) {
			t.Errorf("the system prompt is missing %q", want)
		}
	}
	tool, err := json.Marshal(model.BASH_TOOL)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"result reaches you automatically", "never poll a log",
		"outlives a turn", "job_kill when"} {
		if !strings.Contains(string(tool), want) {
			t.Errorf("the bash description is missing %q", want)
		}
	}

	for _, bad := range []string{"<no value>", "sed -n '1,200p'", "{{"} {
		if strings.Contains(sys, bad) {
			t.Errorf("system prompt contains %q", bad)
		}
	}

	// A handed-off call must be reported as a job that is running, with no
	// returncode in the observation at all: 0 would read as success for work
	// that has not finished, and that number is what a tool card would then
	// print as "exit 0".
	l.extraVars = map[string]any{"output": map[string]any{
		"pending": true, "returncode": 0, "output": "started in the background as job j1",
		"job_id": "j1", "job_output_path": "/tmp/job-j1.log",
		"exception_info": "", "output_error": "",
		"extra": map[string]any{"job_id": "j1", "job_output_path": "/tmp/job-j1.log"},
	}}
	pendSrc := config.String(m, "model.observation_template", "")
	pending, perr := l.render("observation_template", pendSrc)
	if perr != nil {
		t.Fatalf("render pending observation: %v", perr)
	}
	for _, want := range []string{`"status": "running"`, `"job_id": "j1"`, "/tmp/job-j1.log", "started in the background"} {
		if !strings.Contains(pending, want) {
			t.Errorf("pending observation missing %q: %s", want, pending)
		}
	}
	if strings.Contains(pending, "returncode") {
		t.Errorf("a running job was given a return code: %s", pending)
	}

	obsSrc := config.String(m, "model.observation_template", "")
	if obsSrc == "" {
		t.Fatal("no observation_template in the shipped config")
	}
	l.extraVars = map[string]any{"output": map[string]any{
		"truncated": true, "returncode": 1, "output": "tail text",
		"elided_lines": 4211, "bytes": 400000, "path": "/tmp/s/7.log",
	}}
	obs, err := l.render("observation_template", obsSrc)
	if err != nil {
		t.Fatalf("render observation: %v", err)
	}
	for _, want := range []string{"last 100 lines / 10000 characters", "wc -l <path>", "sed -n '1,100p' <path>"} {
		if !strings.Contains(obs, want) {
			t.Errorf("observation warning missing %q", want)
		}
	}
	for _, bad := range []string{"<no value>", "sed -n '1,200p'"} {
		if strings.Contains(obs, bad) {
			t.Errorf("observation warning contains %q", bad)
		}
	}
}
