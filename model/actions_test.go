package model

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// parseOne runs one bash tool call through the parser and returns the action,
// failing the test if the response was rejected.
func parseOne(t *testing.T, arguments string) Action {
	t.Helper()
	actions, err := parseToolCallActions(
		[]map[string]any{{
			"id":       "call-1",
			"function": map[string]any{"name": "bash", "arguments": arguments},
		}},
		"{{ .error }}", map[string]any{"finish_reason": "tool_calls"})
	if err != nil {
		t.Fatalf("arguments %s: %v", arguments, err)
	}
	if len(actions) != 1 {
		t.Fatalf("actions = %d", len(actions))
	}
	return actions[0]
}

// The optional parameters are permissive by design: a value the parser cannot
// make sense of means "the model did not ask", never a rejected call. A job that
// failed to start because a number was quoted is strictly worse than one that
// ran on the default budget (docs/jobs-research.md §4).
func TestParseOptionalBashParameters(t *testing.T) {
	cases := []struct {
		arguments      string
		wantTimeout    int
		wantBackground bool
	}{
		{`{"command":"ls"}`, 0, false},
		{`{"command":"make","timeout_s":900}`, 900, false},
		{`{"command":"make","timeout_s":"900"}`, 900, false},
		{`{"command":"make","timeout_s":900.0}`, 900, false},
		{`{"command":"make","timeout_s":0}`, 0, false},
		{`{"command":"make","timeout_s":-5}`, 0, false},
		{`{"command":"make","timeout_s":"soon"}`, 0, false},
		{`{"command":"make","timeout_s":null}`, 0, false},
		{`{"command":"make","timeout_s":[]}`, 0, false},
		{`{"command":"train","background":true}`, 0, true},
		{`{"command":"train","background":"true"}`, 0, true},
		{`{"command":"train","background":"yes"}`, 0, true},
		{`{"command":"train","background":"false"}`, 0, false},
		{`{"command":"train","background":1}`, 0, false},
		{`{"command":"train","background":true,"timeout_s":60}`, 60, true},
		// An unknown sibling must not cost the call either.
		{`{"command":"ls","shelL":"zsh","timeout_s":42}`, 42, false},
	}
	for _, c := range cases {
		got := parseOne(t, c.arguments)
		if got.Command == "" || got.ToolCallID != "call-1" {
			t.Errorf("%s: action = %+v", c.arguments, got)
		}
		if got.TimeoutS != c.wantTimeout || got.Background != c.wantBackground {
			t.Errorf("%s: timeout_s=%d background=%v, want %d/%v",
				c.arguments, got.TimeoutS, got.Background, c.wantTimeout, c.wantBackground)
		}
	}
}

// timeout_s has a floor and no ceiling: 0 means "no opinion", and a big number
// must survive intact, because the interesting commands here are hours long and
// any ceiling the harness picks is a bug that fires in the middle of a run.
func TestTimeoutSIsUncapped(t *testing.T) {
	for _, n := range []int{1, 100, 3600, 86400, 1 << 30} {
		b, _ := json.Marshal(map[string]any{"command": "x", "timeout_s": n})
		if got := parseOne(t, string(b)).TimeoutS; got != n {
			t.Errorf("timeout_s %d came back as %d", n, got)
		}
	}
}

// The things that must still fail loudly, with the texts that tell a model what
// to fix. These strings are worth their exactness because a malformed turn gets
// nothing else: they are the lesson, not a log line.
func TestParseStillRejectsBrokenCalls(t *testing.T) {
	// A rejection is an Interrupt carrying the rendered template as the message
	// the model reads, so the assertion belongs on that text.
	parse := func(name, arguments string) string {
		t.Helper()
		_, err := parseToolCallActions(
			[]map[string]any{{"function": map[string]any{"name": name, "arguments": arguments}}},
			"{{ .error }}", map[string]any{"finish_reason": "tool_calls"})
		if err == nil {
			return ""
		}
		var in *Interrupt
		if !errors.As(err, &in) || len(in.Messages) == 0 {
			t.Fatalf("%s %s: %v is not a format interrupt", name, arguments, err)
		}
		if in.Kind != KindFormatError {
			t.Errorf("kind = %v, want %v", in.Kind, KindFormatError)
		}
		text, _ := in.Messages[0]["content"].(string)
		return text
	}
	if text := parse("bash", `{"timeout_s":60}`); !strings.Contains(text, "Missing 'command' argument in bash tool call.") {
		t.Errorf("rejection text = %q", text)
	}
	// An unknown tool stays unknown however well-formed its arguments are.
	if text := parse("shell_start", `{}`); !strings.Contains(text, "Unknown tool 'shell_start'") {
		t.Errorf("rejection text = %q", text)
	}
	// And a call that only adds optional keys is not a problem.
	if a := parseOne(t, `{"command":"ls","timeout_s":42}`); a.TimeoutS != 42 {
		t.Errorf("action = %+v", a)
	}
}

// The job tools are their own calls now: bash's required 'command' must not be
// demanded of them, and job_kill must not be allowed to mean "guess".
func TestParseJobToolCalls(t *testing.T) {
	parse := func(name, arguments string) (Action, error) {
		actions, err := parseToolCallActions(
			[]map[string]any{{"id": "c1", "function": map[string]any{"name": name, "arguments": arguments}}},
			"{{ .error }}", map[string]any{"finish_reason": "tool_calls"})
		if err != nil || len(actions) == 0 {
			return Action{}, err
		}
		return actions[0], nil
	}

	a, err := parse("jobs", `{}`)
	if err != nil {
		t.Fatalf("a bare jobs call must parse: %v", err)
	}
	if a.ToolName() != ToolJobs || a.Command != "" {
		t.Errorf("action = %+v", a)
	}
	a, err = parse("jobs", `{"id":"j7"}`)
	if err != nil || a.Command != "j7" {
		t.Errorf("scoped listing = %+v, %v", a, err)
	}

	a, err = parse("job_kill", `{"id":"j7"}`)
	if err != nil || a.Command != "j7" || a.ToolName() != ToolJobKill {
		t.Errorf("action = %+v, %v", a, err)
	}
	a, err = parse("job_kill", `{"all":true}`)
	if err != nil {
		t.Fatalf("kill all: %v", err)
	}
	if !a.Bool("all") {
		t.Errorf("all flag lost: %+v", a)
	}
	// No target at all is the one thing the tool cannot do: all:true ends every
	// job in the session, so guessing is not an option.
	if _, err := parse("job_kill", `{}`); err == nil {
		t.Error("job_kill with no target was accepted")
	} else if !strings.Contains(rejectionText(t, err), "job_kill needs an 'id'") {
		t.Errorf("rejection text = %q", rejectionText(t, err))
	}

	// job_wait takes nothing at all, so `{}` is the whole call - and an id a
	// model passes anyway is ignored rather than made into an error, since the
	// semantics are "any job" whatever the model thought it was narrowing.
	a, err = parse("job_wait", `{}`)
	if err != nil || a.ToolName() != ToolJobWait {
		t.Errorf("action = %+v, %v", a, err)
	}
	if a, err = parse("job_wait", `{"id":"j1","timeout_ms":5}`); err != nil || a.ToolName() != ToolJobWait {
		t.Errorf("job_wait rejected stray arguments: %+v, %v", a, err)
	}
}

// rejectionText pulls the message the model would actually read out of a format
// interrupt: the error's own text is just "FormatError".
func rejectionText(t *testing.T, err error) string {
	t.Helper()
	var in *Interrupt
	if !errors.As(err, &in) || len(in.Messages) == 0 {
		t.Fatalf("%v is not a format interrupt", err)
	}
	text, _ := in.Messages[0]["content"].(string)
	return text
}

// The gate policy in one table, because it is the part a reviewer will not
// believe: ending a job asks for nothing.
func TestNeedsConfirmationIsOnlyAboutStartingWork(t *testing.T) {
	cases := []struct {
		tool string
		want bool
	}{
		{"", true},           // human-mode and legacy rows: bash
		{ToolBash, true},     // starts work
		{ToolJobs, false},    // reads a table
		{ToolJobKill, false}, // ends work already permitted
		{ToolJobWait, false}, // reads the registry, blocking on nothing dangerous
	}
	for _, c := range cases {
		a := Action{Tool: c.tool, Command: "anything"}
		if got := a.NeedsConfirmation(); got != c.want {
			t.Errorf("%q: NeedsConfirmation = %v, want %v", c.tool, got, c.want)
		}
	}
	// Display names the tool for the job calls and stays the raw script for
	// bash, so every existing panel and ledger line is unchanged.
	if got := (Action{Command: "make && make install"}).Display(); got != "make && make install" {
		t.Errorf("bash Display = %q", got)
	}
	if got := (Action{Tool: ToolJobKill, Command: "j1"}).Display(); got != "job_kill j1" {
		t.Errorf("job_kill Display = %q", got)
	}
	if got := (Action{Tool: ToolJobs}).Display(); got != "jobs" {
		t.Errorf("jobs Display = %q", got)
	}
}
