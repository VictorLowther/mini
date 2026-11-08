// Package env executes the agent's commands. Local is the one environment that
// ships: it interprets commands in-process with mvdan.cc/sh's bash interpreter
// instead of forking /bin/sh, so the bash grammar the prompts teach ([[ ]],
// arrays, heredocs, $'…') works identically on every machine.
package env

import (
	"context"
	"os"
	"strings"
)

// Result is one executed action: the merged stdout+stderr buffer, the exit
// code, and the exception details when the command could not run to
// completion. It is always returned as data, never as a Go error, so a broken
// command reaches the model as an observation (that is the whole point of the
// agent) instead of unwinding the loop.
type Result struct {
	Output        string
	ReturnCode    int
	ExceptionInfo string
	Extra         map[string]any
}

// AsMap is the "output" object the observation template renders.
func (r Result) AsMap() map[string]any {
	m := map[string]any{
		"output":         r.Output,
		"returncode":     r.ReturnCode,
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
	Execute(ctx context.Context, command, cwd string) (Result, error)
	TemplateVars() map[string]any
	Serialize() map[string]any
}

// SubmitMarker is the sentinel the prompts instruct the model to echo when it
// is done: if the first line of the output is that command and the command
// succeeded, everything after that line is the submission. Checked on the
// success and the error path alike.
func SubmitMarker(r Result) (map[string]any, bool) {
	if r.ReturnCode != 0 {
		return nil, false
	}
	lines := splitKeepEnds(strings.TrimLeft(r.Output, " \t\n\r\v\f"))
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT" {
		return nil, false
	}
	submission := strings.Join(lines[1:], "")
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
