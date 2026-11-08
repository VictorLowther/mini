// Package env executes the agent's commands. Local is the one environment that
// ships: it interprets commands in-process with mvdan.cc/sh's bash interpreter
// instead of forking /bin/sh, so the bash grammar the prompts teach ([[ ]],
// arrays, heredocs, $'…') works identically on every machine.
package env

import (
	"context"
	"io"
	"os"
	"strings"
)

// Result is one executed action: where its merged stdout+stderr went, the
// exit code, and the exception details when the command could not run to
// completion. It is always returned as data, never as a Go error, so a broken
// command reaches the model as an observation (that is the whole point of the
// agent) instead of unwinding the loop.
type Result struct {
	// OutputPath is the file holding the complete merged stdout+stderr and
	// OutputBytes its size. View is the bounded text the model reads, with
	// ViewOffset the byte of the file it starts at and ElidedLines the lines in
	// front of it; Truncated is ViewOffset > 0. All four are filled by the loop
	// after the command finishes: nothing here holds the whole output.
	OutputPath  string
	OutputBytes int64
	View        string
	ViewOffset  int64
	ElidedLines int
	Truncated   bool
	// ViewError is set by the loop when the view could not be read from the
	// file; the observation reports it instead of silently showing nothing.
	ViewError     string
	ReturnCode    int
	ExceptionInfo string
	Extra         map[string]any
	// CWD is the directory the command actually ran in, resolved by Execute.
	// It is transcript metadata for the UI, never part of the observation the
	// model reads.
	CWD string
}

// AsMap is the "output" object the observation template renders. The "output"
// key carries the bounded view, so the non-truncated template branch does not
// change when the full text moves to disk.
func (r Result) AsMap() map[string]any {
	m := map[string]any{
		"output":         r.View,
		"elided_lines":   r.ElidedLines,
		"bytes":          r.OutputBytes,
		"path":           r.OutputPath,
		"truncated":      r.Truncated,
		"returncode":     r.ReturnCode,
		"output_error":   r.ViewError,
		"exception_info": r.ExceptionInfo,
	}
	if r.Extra != nil {
		m["extra"] = r.Extra
	}
	return m
}

// Environment executes actions and contributes prompt variables. It exists so
// a docker/singularity environment can be dropped in later without touching
// the loop, exactly as in the Python original.
type Environment interface {
	// Execute runs command, writing its merged stdout+stderr to out. Passing
	// the same *os.File as out is what makes the file byte-identical to
	// `cmd > file 2>&1`; a nil out discards.
	Execute(ctx context.Context, command, cwd string, out io.Writer) (Result, error)
	TemplateVars() map[string]any
	Serialize() map[string]any
}

const (
	// markerReadLimit bounds the head read that tests the sentinel line: the
	// sentinel is echoed on its own first line, so 4 KiB is generous.
	markerReadLimit = 4096
	// maxSubmissionBytes caps the submit text read back out of the result
	// file. shortcut: a submission over 1 MiB is truncated; mini's prompts ask
	// for a prose answer, and the cap only bounds the submit command's own
	// output.
	maxSubmissionBytes = 1 << 20
)

// SubmitMarker is the sentinel the prompts instruct the model to echo when it
// is done: if the first line of the output is that command and the command
// succeeded, everything after that line is the submission. Checked on the
// success and the error path alike, streaming from the result file so the
// whole output never sits in memory.
func SubmitMarker(r Result) (map[string]any, bool) {
	if r.ReturnCode != 0 || r.OutputPath == "" {
		return nil, false
	}
	f, err := os.Open(r.OutputPath)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	rawHead := make([]byte, markerReadLimit)
	n, _ := f.ReadAt(rawHead, 0) // a short file ends in io.EOF; that is fine
	head := strings.TrimLeft(string(rawHead[:n]), " \t\n\r\v\f")
	lines := splitKeepEnds(head)
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT" {
		return nil, false
	}
	// The submission is everything after the sentinel line, streamed from the
	// file up to maxSubmissionBytes. A head chunk whose first line is not the
	// sentinel cannot match: the sentinel is a whole line.
	sentinelEnd := n - len(head) + len(lines[0])
	sub := []byte{}
	chunk := make([]byte, 32*1024)
	// ReadAt leaves the file offset at 0, so seek just past the sentinel's
	// terminator before streaming the submission.
	if _, err := f.Seek(int64(sentinelEnd), io.SeekStart); err == nil {
		for len(sub) < maxSubmissionBytes {
			m, rerr := f.Read(chunk)
			sub = append(sub, chunk[:m]...)
			if m < len(chunk) || rerr != nil {
				break
			}
		}
	}
	if len(sub) > maxSubmissionBytes {
		sub = sub[:maxSubmissionBytes]
	}
	submission := string(sub)
	return map[string]any{
		"role":    "exit",
		"content": submission,
		"extra": map[string]any{
			"exit_status": "Submitted",
			"submission":  submission,
		},
	}, true
}

// splitKeepEnds is Python's str.splitlines(keepends=True): the line separators
// Python recognises here are \n, \r\n and a lone \r.
func splitKeepEnds(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '\n' && s[i] != '\r' {
			continue
		}
		end := i + 1
		if s[i] == '\r' && end < len(s) && s[end] == '\n' {
			end++
		}
		out = append(out, s[start:end])
		i = end - 1
		start = end
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// configDump mirrors LocalEnvironmentConfig.model_dump.
func (l *Local) configDump() map[string]any {
	return map[string]any{
		"cwd":     l.CWD,
		"env":     stringMap(l.Env),
		"timeout": int(l.timeout().Seconds()),
	}
}

// Serialize is the environment half of the trajectory file.
func (l *Local) Serialize() map[string]any {
	return map[string]any{
		"info": map[string]any{
			"config": map[string]any{
				"environment":      l.configDump(),
				"environment_type": "mini/env.Local",
			},
		},
	}
}

// TemplateVars merges the config dump, platform.uname(), and the whole process
// environment into the prompt context, in that precedence order: the instance
// template branches on {{system}}, and a template may reference any variable
// the user exported.
func (l *Local) TemplateVars() map[string]any {
	vars := map[string]any{"cwd": l.CWD, "env": stringMap(l.Env), "timeout": int(l.timeout().Seconds())}
	for k, v := range Uname() {
		vars[k] = v
	}
	for k, v := range EnvironMap() {
		vars[k] = v
	}
	return vars
}

// EnvironMap returns the process environment as a map; later duplicates win,
// like dict(os.environ).
func EnvironMap() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

func stringMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
