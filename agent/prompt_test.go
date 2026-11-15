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
	for _, bad := range []string{"<no value>", "sed -n '1,200p'", "{{"} {
		if strings.Contains(sys, bad) {
			t.Errorf("system prompt contains %q", bad)
		}
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
