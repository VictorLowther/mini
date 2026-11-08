package env

import "testing"

func TestSubmitMarker(t *testing.T) {
	cases := []struct {
		name    string
		result  Result
		wantOK  bool
		wantSub string
	}{
		{"marker only", Result{Output: "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n", ReturnCode: 0}, true, ""},
		{"marker with submission", Result{Output: "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\nTask completed successfully\n", ReturnCode: 0}, true, "Task completed successfully\n"},
		{"leading blank lines are lstripped", Result{Output: "\n  COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\nx", ReturnCode: 0}, true, "x"},
		{"multiline submission keeps newlines", Result{Output: "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\nline1\nline2\n", ReturnCode: 0}, true, "line1\nline2\n"},
		{"failed command does not submit", Result{Output: "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n", ReturnCode: 1}, false, ""},
		{"marker on a later line does not submit", Result{Output: "echo hi\nCOMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n", ReturnCode: 0}, false, ""},
		{"marker mixed with another command", Result{Output: "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT && echo hi\n", ReturnCode: 0}, false, ""},
		{"empty output", Result{Output: "", ReturnCode: 0}, false, ""},
		{"crlf line ending", Result{Output: "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\r\nbody\r\n", ReturnCode: 0}, true, "body\r\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, ok := SubmitMarker(c.result)
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

func TestResultAsMap(t *testing.T) {
	m := Result{Output: "o", ReturnCode: 2, ExceptionInfo: "e"}.AsMap()
	if len(m) != 3 || m["output"] != "o" || m["returncode"] != 2 || m["exception_info"] != "e" {
		t.Errorf("AsMap = %#v", m)
	}
	if _, ok := m["extra"]; ok {
		t.Error("extra must be absent unless set, so the observation template sees the same shape as Python")
	}
	withExtra := Result{Extra: map[string]any{"exception_type": "TimeoutExpired"}}.AsMap()
	if withExtra["extra"] == nil {
		t.Error("extra dropped")
	}
}
