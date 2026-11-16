package env

import (
	"testing"
)

func TestResultAsMap(t *testing.T) {
	m := Result{View: "o", OutputBytes: 2, OutputPath: "/p", ViewOffset: 1, ElidedLines: 7, Truncated: true, ViewError: "ve", ReturnCode: 2, ExceptionInfo: "e"}.AsMap()
	// The view is the text under the "output" key, so the template's
	// non-truncated branch never noticed the output moved to disk.
	if m["output"] != "o" || m["returncode"] != 2 || m["exception_info"] != "e" ||
		m["elided_lines"] != 7 || m["bytes"] != int64(2) || m["path"] != "/p" ||
		m["truncated"] != true || m["output_error"] != "ve" || m["pending"] != false {
		t.Errorf("AsMap = %#v", m)
	}
	if len(m) != 9 {
		t.Errorf("AsMap keys = %#v", m)
	}
	if _, ok := m["extra"]; ok {
		t.Error("extra must be absent unless set: the observation template branches on its presence, so an empty map is not the same as none")
	}
	// A handed-off call reports pending with no measured outcome. The template
	// reads this to leave returncode out of the observation entirely, because
	// "exit 0" for a job that is still running is a claim about work that has
	// not happened.
	pend := Result{Pending: true, View: "handed off"}.AsMap()
	if pend["pending"] != true || pend["output"] != "handed off" {
		t.Errorf("pending AsMap = %#v", pend)
	}
	withExtra := Result{Extra: map[string]any{"exception_type": "TimeoutExpired"}}.AsMap()
	if withExtra["extra"] == nil {
		t.Error("extra dropped")
	}
}
