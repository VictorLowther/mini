package env

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func submitFile(t *testing.T, output string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.log")
	if err := os.WriteFile(path, []byte(output), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSubmitMarker(t *testing.T) {
	cases := []struct {
		name    string
		output  string
		code    int
		wantOK  bool
		wantSub string
	}{
		{"marker only", "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n", 0, true, ""},
		{"marker with submission", "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\nTask completed successfully\n", 0, true, "Task completed successfully\n"},
		{"leading blank lines are lstripped", "\n  COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\nx", 0, true, "x"},
		{"multiline submission keeps newlines", "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\nline1\nline2\n", 0, true, "line1\nline2\n"},
		{"failed command does not submit", "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n", 1, false, ""},
		{"marker on a later line does not submit", "echo hi\nCOMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n", 0, false, ""},
		{"marker mixed with another command", "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT && echo hi\n", 0, false, ""},
		{"empty output", "", 0, false, ""},
		{"crlf line ending", "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\r\nbody\r\n", 0, true, "body\r\n"},
		// The sentinel test reads only the first markerReadLimit bytes, so a
		// long sentinel-free first line can never match.
		{"sentinel-free head longer than the read limit", strings.Repeat("x", 6000) + "\nCOMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n", 0, false, ""},
		// The submission is read from the file up to maxSubmissionBytes.
		{"submission over the cap is truncated", "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n" + strings.Repeat("y", maxSubmissionBytes+1000), 0, true, strings.Repeat("y", maxSubmissionBytes)},
		{"submission past the head chunk is read from the file", "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n" + strings.Repeat("z", 4096) + "tail", 0, true, strings.Repeat("z", 4096) + "tail"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, ok := SubmitMarker(Result{OutputPath: submitFile(t, c.output), ReturnCode: c.code})
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if got := msg["extra"].(map[string]any)["submission"]; got != c.wantSub {
				t.Errorf("submission = %q, want %q", got, c.wantSub)
			}
			if msg["role"] != "exit" || msg["content"] != c.wantSub {
				t.Errorf("message = %#v", msg)
			}
			if s := msg["extra"].(map[string]any)["exit_status"]; s != "Submitted" {
				t.Errorf("exit_status = %v", s)
			}
		})
	}
}

// A result with no file cannot submit, whatever the exit code.
func TestSubmitMarkerNeedsAFile(t *testing.T) {
	if _, ok := SubmitMarker(Result{ReturnCode: 0}); ok {
		t.Error("empty OutputPath submitted")
	}
	if _, ok := SubmitMarker(Result{OutputPath: filepath.Join(t.TempDir(), "missing.log"), ReturnCode: 0}); ok {
		t.Error("missing file submitted")
	}
}

func TestResultAsMap(t *testing.T) {
	m := Result{View: "o", OutputBytes: 2, OutputPath: "/p", ViewOffset: 1, ElidedLines: 7, Truncated: true, ViewError: "ve", ReturnCode: 2, ExceptionInfo: "e"}.AsMap()
	// The view is the text under the "output" key, so the template's
	// non-truncated branch never noticed the output moved to disk.
	if m["output"] != "o" || m["returncode"] != 2 || m["exception_info"] != "e" ||
		m["elided_lines"] != 7 || m["bytes"] != int64(2) || m["path"] != "/p" ||
		m["truncated"] != true || m["output_error"] != "ve" {
		t.Errorf("AsMap = %#v", m)
	}
	if len(m) != 8 {
		t.Errorf("AsMap keys = %#v", m)
	}
	if _, ok := m["extra"]; ok {
		t.Error("extra must be absent unless set, so the observation template sees the same shape as Python")
	}
	withExtra := Result{Extra: map[string]any{"exception_type": "TimeoutExpired"}}.AsMap()
	if withExtra["extra"] == nil {
		t.Error("extra dropped")
	}
}
