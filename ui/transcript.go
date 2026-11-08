package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/VictorLowther/mini/model"
)

// maxTranscriptLines bounds how much command output the transcript shows. The
// full text stays in the trajectory.
const maxTranscriptLines = 200

// observation is one executed action in displayable form. Both front ends
// render from the message's extra rather than its content: content is the JSON
// (or XML) block the model was promised, which is unreadable as a transcript
// and, when the model is mid-mistake, wrong.
type observation struct {
	Command   string
	CWD       string
	Exit      int
	HasExit   bool
	Output    string
	Exception string
}

func observationOf(m model.Message) (observation, bool) {
	extra, ok := m["extra"].(map[string]any)
	if !ok {
		return observation{}, false
	}
	out, isOutput := extra["raw_output"]
	if !isOutput {
		return observation{}, false
	}
	o := observation{Output: str(out)}
	o.Command, _ = extra["command"].(string)
	o.CWD, _ = extra["cwd"].(string)
	o.Exception, _ = extra["exception_info"].(string)
	if code, ok := extra["returncode"]; ok {
		o.Exit, o.HasExit = asInt(code)
	}
	return o, true
}

// lines renders the block as a shell transcript: what ran, where, how it ended,
// and what it printed.
func (o observation) lines() []string {
	var out []string
	if o.Command != "" {
		for i, line := range strings.Split(strings.TrimRight(o.Command, "\n"), "\n") {
			if i == 0 {
				out = append(out, "$ "+line)
			} else {
				out = append(out, "> "+line)
			}
		}
	}
	if o.CWD != "" {
		out = append(out, "  in "+o.CWD)
	}
	switch {
	case o.HasExit && o.Exit != 0:
		out = append(out, fmt.Sprintf("exit %d", o.Exit))
	case o.HasExit:
		out = append(out, "exit 0")
	}
	if o.Exception != "" {
		out = append(out, o.Exception)
	}
	body := strings.TrimRight(o.Output, "\n")
	if body == "" {
		if o.Command != "" || o.HasExit {
			out = append(out, "(no output)")
		}
		return out
	}
	lines := strings.Split(body, "\n")
	if len(lines) > maxTranscriptLines {
		kept := lines[:maxTranscriptLines]
		kept = append(kept, fmt.Sprintf("… %d more lines (full output in the trajectory)", len(lines)-maxTranscriptLines))
		lines = kept
	}
	return append(out, lines...)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case float64:
		return int(t), true
	case string:
		n, err := strconv.Atoi(t)
		return n, err == nil
	}
	return 0, false
}
