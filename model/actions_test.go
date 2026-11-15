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
	// The new parameters are extras on the bash tool and nothing else: an
	// unknown tool stays unknown however well-formed its arguments are.
	if text := parse("jobs", `{"timeout_s":60}`); !strings.Contains(text, "Unknown tool 'jobs'") {
		t.Errorf("rejection text = %q", text)
	}
	// And a call that only adds optional keys is not a problem.
	if a := parseOne(t, `{"command":"ls","timeout_s":42}`); a.TimeoutS != 42 {
		t.Errorf("action = %+v", a)
	}
}
