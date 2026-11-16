package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/VictorLowther/mini/model"
)

// observation is one executed action in displayable form. Both front ends
// render from the message's extra rather than its content: content is the JSON
// (or XML) block the model was promised, which is unreadable as a transcript
// and, when the model is mid-mistake, wrong. Live and resumed turns take one
// path: the transcript shows the stored view, the same bytes the model was
// shown, read from the result file at its offset.
type observation struct {
	Command   string
	CWD       string
	Exit      int
	HasExit   bool
	Path      string
	Output    string // ReadView of Path from Offset
	Offset    int64  // byte of Path where Output starts
	End       int64  // recorded file size at completion
	Elided    int    // lines that came before Output
	Err       string // Path could not be read
	Exception string
	// JobID and Pending mark a call that handed off to the registry instead of
	// completing. Pending is what keeps the card honest: there is no exit status
	// yet, so HasExit stays false and nothing may print "exit 0".
	JobID   string
	Pending bool
}

func observationOf(m model.Message) (observation, bool) {
	extra, ok := m["extra"].(map[string]any)
	if !ok || !model.IsToolResult(m) {
		return observation{}, false
	}
	o := observation{
		Command:   str(extra["command"]),
		CWD:       str(extra["cwd"]),
		Path:      str(extra["tcr_path"]),
		Exception: str(extra["exception_info"]),
		Elided:    asIntDefault(extra["elided_lines"]),
		Offset:    int64(asIntDefault(extra["tcr_offset"])),
		End:       int64(asIntDefault(extra["tcr_bytes"])),
	}
	o.JobID = str(extra["job_id"])
	o.Pending, _ = extra["pending"].(bool)
	// A pending observation carries no returncode in the model-facing text, so
	// the card must not manufacture one from the zero value sitting in extra.
	if code, ok := extra["returncode"]; ok && !o.Pending {
		o.Exit, o.HasExit = asInt(code)
	}
	if o.Path != "" {
		var err error
		if o.Output, err = model.ReadView(o.Path, o.Offset, o.End); err != nil {
			o.Err = err.Error()
		}
	}
	return o, true
}

// lines renders the block as a shell transcript: what ran, where, how it ended,
// and what it printed. The view is already capped at the model's bounds, so no
// display-specific line arithmetic exists here.
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
	if o.Err != "" {
		return append(out, "(tool result unreadable: "+o.Path+")")
	}
	if o.Elided > 0 {
		out = append(out, fmt.Sprintf("… %d more lines (full output: %s)", o.Elided, o.Path))
	}
	body := strings.TrimRight(o.Output, "\n")
	if body == "" {
		if o.Command != "" || o.HasExit {
			out = append(out, "(no output)")
		}
		return out
	}
	return append(out, strings.Split(body, "\n")...)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	case string:
		n, err := strconv.Atoi(t)
		return n, err == nil
	}
	return 0, false
}

// asIntDefault reads an optional count key; absent or unparseable is 0.
func asIntDefault(v any) int {
	n, _ := asInt(v)
	return n
}
