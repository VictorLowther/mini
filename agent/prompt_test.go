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

// TestPromptListingFollowsTheToolSet is §5.6's mechanism, checked end to end:
// what the system prompt says about tools is generated from what the session was
// advertised, so a capability cannot be described in prose and withheld from the
// request, or handed over in the request while the instructions never mention it.
//
// The subagent case is the one that motivated it. Today a subagent's set differs
// from a top-level one only by the spawn tool that step 6 will add, so the two
// prompts are expected to match - and the assertion that matters is the one on a
// hand-built set, which proves the gate is live rather than decorative.
func TestPromptListingFollowsTheToolSet(t *testing.T) {
	render := func(set model.ToolSet) string {
		t.Helper()
		m, err := config.FromSpec("../config/configs/mini.yaml")
		if err != nil {
			t.Fatal(err)
		}
		l := newLoop(t, &model.Deterministic{}, &env.Local{})
		l.Cfg = ConfigFromMap(config.Sub(m, "agent"))
		l.Tools = set
		sys, err := l.render("system_template", l.Cfg.SystemTemplate)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		return sys
	}

	top := render(model.TopLevelTools())
	sub := render(model.SubagentTools())

	for _, want := range []string{"## Your tools", "`bash`", "`jobs`", "`job_kill`"} {
		if !strings.Contains(top, want) {
			t.Errorf("top-level prompt missing %q", want)
		}
	}
	if !strings.Contains(top, "the only ones you may call") {
		t.Error("the generated listing does not say it is exhaustive")
	}
	if strings.Contains(top, "<no value>") || strings.Contains(top, "{{") {
		t.Error("template debris leaked into the rendered prompt")
	}
	// The spawn tool and its send partner are named for the session that was
	// given them, and not for the one that was not. This is §5.6 in one
	// assertion: hiding a capability from the request array while the prose still
	// describes it is the drift the generated listing exists to prevent, and a
	// subagent told about `agent` will try to use it.
	for _, want := range []string{"`agent`", "`job_send`"} {
		if !strings.Contains(top, want) {
			t.Errorf("the top-level prompt does not list %s, which it was given", want)
		}
		if strings.Contains(sub, want) {
			t.Errorf("the subagent prompt names %s, which it was not given", want)
		}
	}
	if sub == top {
		t.Error("a subagent's prompt is identical to a top-level one; the set is not reaching the template")
	}

	// The identity paragraph §9.7 rules: a child must know its reader is another
	// agent and that its report is the whole of what survives, and a top-level
	// session must not be told it is somebody's subagent.
	for _, want := range []string{"## Working as a subagent", "read by that agent", "no session to come back to"} {
		if !strings.Contains(sub, want) {
			t.Errorf("the subagent prompt is missing %q", want)
		}
		if strings.Contains(top, want) {
			t.Errorf("the top-level prompt says %q to a session that is not a subagent", want)
		}
	}
	// And it must not be told to avoid a tool it was never given: §5.6's rule is
	// that an invisible capability stays invisible, including in the negation.
	for _, sys := range []string{sub, top} {
		for _, bad := range []string{"Do not start another agent", "cannot spawn", "no subagent tools"} {
			if strings.Contains(sys, bad) {
				t.Errorf("the prompt mentions the spawn tool in prose: %q", bad)
			}
		}
	}
	// And the parts that must survive for both, survive for both: a subagent that
	// cannot background its own builds would be a research assistant with a
	// 30-second attention span.
	for _, want := range []string{"`bash`", "`jobs`", "`job_kill`", "Background jobs report themselves"} {
		if !strings.Contains(sub, want) {
			t.Errorf("the subagent prompt lost %q", want)
		}
	}

	// A set with the jobs tools removed must lose the prose that depends on them.
	bashOnly := model.ToolSet{}
	for _, tool := range model.TopLevelTools() {
		if tool.Name == model.ToolBash {
			bashOnly = append(bashOnly, tool)
		}
	}
	trimmed := render(bashOnly)
	for _, gone := range []string{"Background jobs report themselves", "end it with job_kill"} {
		if strings.Contains(trimmed, gone) {
			t.Errorf("the prompt still says %q for a session that was not given the tool", gone)
		}
	}
	if !strings.Contains(trimmed, "`bash`") {
		t.Error("the trimmed listing lost bash, which it still has")
	}
	if strings.Contains(trimmed, "`jobs`") {
		t.Error("the trimmed listing still names jobs")
	}
}
