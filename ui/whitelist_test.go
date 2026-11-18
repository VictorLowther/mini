package ui

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/env"
)

func mustWhitelist(t *testing.T, patterns ...string) *Interactive {
	t.Helper()
	re, err := compileWhitelist(patterns)
	if err != nil {
		t.Fatal(err)
	}
	return &Interactive{Cfg: newConfig("confirm"), whitelist: re}
}

// A-1, as a table. The old gate approved every `want=false` row that began with a
// whitelisted word, because the pattern was anchored only at the start and the
// rest of the command was never examined.
func TestWhitelistApprovesOnlyWhatTheHumanAuthorized(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		cmd      string
		want     bool
	}{
		{"exact command", []string{"echo"}, "echo", true},
		{"single authorized command with args", []string{"echo .*"}, "echo hi", true},
		{"full match replaces prefix", []string{"ls"}, "ls -la", false},
		{"full match, written out", []string{"ls -la"}, "ls -la", true},

		// the injection family: an authorized prefix carrying unauthorized shell
		{"semicolon append", []string{"echo"}, "echo hi; touch /tmp/wlpwned", false},
		{"and-list append", []string{"git status"}, "git status && rm -rf /", false},
		{"or-list append", []string{"git status"}, "git status || rm -rf /", false},
		{"newline append", []string{"echo"}, "echo hi\ntouch /tmp/wlpwned", false},
		{"pipeline append", []string{"echo"}, "echo hi | sh", false},
		{"background append", []string{"echo"}, "echo hi & rm -rf /", false},
		{"command substitution", []string{"echo .*"}, "echo $(touch /tmp/wlpwned)", false},
		{"backtick substitution", []string{"echo .*"}, "echo `touch /tmp/wlpwned`", false},
		{"param expansion", []string{"echo .*"}, "echo $HOME", false},
		{"process substitution", []string{"cat .*"}, "cat <(rm -rf /)", false},
		{"redirection", []string{"echo .*"}, "echo hi > /tmp/wlpwned", false},
		{"subshell", []string{"echo .*"}, "(echo hi)", false},
		{"brace group", []string{"echo .*"}, "{ echo hi; }", false},
		{"inline assignment", []string{"echo .*"}, "FOO=1 echo hi", false},
		{"function definition", []string{"echo .*"}, "f() { echo hi; }", false},
		{"for loop", []string{"echo .*"}, "for i in a; do echo $i; done", false},
		{"heredoc", []string{"cat"}, "cat <<EOF\nhi\nEOF", false},
		{"unparseable", []string{"echo .*"}, "echo (unbalanced", false},
		{"empty command", []string{"echo .*"}, "", false},
		{"comment only", []string{"echo .*"}, "# nothing", false},

		// every stage of a chain must be authorized on its own
		{"both stages authorized", []string{"echo a", "echo b"}, "echo a; echo b", true},
		{"second stage not authorized", []string{"echo a"}, "echo a; echo b", false},
		{"pipeline both authorized", []string{"git status", "grep foo"}, "git status | grep foo", true},
		{"pipeline one side unauthorized", []string{"git status"}, "git status | grep secret", false},

		// quoting is what the tree sees, not what a text scan sees
		{"semicolon inside single quotes", []string{"echo .*"}, "echo 'a; b'", true},
		{"dangerous text inside quotes", []string{"echo .*"}, `echo "rm -rf /"`, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := mustWhitelist(t, tc.patterns...)
			got := whitelistChecks(tc.cmd, h.whitelist)
			if got.Allowed != tc.want {
				t.Errorf("whitelistChecks(%q) allowed=%v, want %v (reason: %s)", tc.cmd, got.Allowed, tc.want, got.Reason)
			}
			if !got.Allowed && got.Reason == "" {
				t.Errorf("a refusal must carry a reason the human can act on: %q", tc.cmd)
			}
		})
	}
}

// An empty whitelist approves nothing, whatever the command.
func TestEmptyWhitelistNeverApproves(t *testing.T) {
	if v := whitelistChecks("echo hi", nil); v.Allowed {
		t.Fatal("no patterns must not mean no restrictions")
	}
}

// A-2's second layer: an unrecognised mode asks instead of silently running
// everything. The parse-boundary check in host.Open is the first layer.
func TestUnknownModeFailsClosed(t *testing.T) {
	for _, mode := range []string{"Confirm", "CONFIRM", "confirm ", "banana", "", "confirmation"} {
		h := mustWhitelist(t, "echo .*")
		h.Cfg.Mode = mode
		ask, why := h.shouldAskConfirmation("echo hi; touch /tmp/wlpwned")
		if !ask {
			t.Errorf("mode %q skipped confirmation", mode)
		}
		if why == "" {
			t.Errorf("mode %q asked without saying why", mode)
		}
	}
	// and the two modes that genuinely have no asker still do not nag
	for _, mode := range []string{"yolo", "human"} {
		h := mustWhitelist(t)
		h.Cfg.Mode = mode
		if ask, _ := h.shouldAskConfirmation("rm -rf /"); ask {
			t.Errorf("mode %q must not prompt", mode)
		}
	}
}

// Config-level validation of the mode, which is the invariant upstream's pydantic
// Literal gave and the Go port dropped.
func TestValidateModeRejectsUnknownValues(t *testing.T) {
	for _, ok := range []string{"confirm", "human", "yolo"} {
		if err := agent.ValidateMode(ok); err != nil {
			t.Errorf("ValidateMode(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"Confirm", "banana", "", "confirm ", "auto"} {
		if err := agent.ValidateMode(bad); err == nil {
			t.Errorf("ValidateMode(%q) = nil, want an error", bad)
		}
	}
}

// The end-to-end version of A-1: with `echo` whitelisted, the model must not be
// able to touch a file by hanging it off the end of an authorized command.
func TestWhitelistInjectionIsBlockedEndToEnd(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "wlpwned")

	c := newConfig("confirm", "echo")
	c.ConfirmExit = false
	cmd := "echo hi; touch " + victim
	// stdin answers the confirmation with a rejection comment.
	l, h := pair(t, c, []any{cmd, submit("done")}, &env.Local{Timeout: 10 * time.Second}, "not that way\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(victim); err == nil {
		t.Fatal("the injected half of a whitelisted command executed")
	}
	if !containsConfirm(h) {
		t.Fatal("no confirmation was asked for a command carrying unauthorized shell")
	}
	if !containsString(h, "not on the whitelist") {
		t.Errorf("the prompt never said which command was unauthorized; output:\n%s", h.out.String())
	}
}

func containsConfirm(h *harness) bool {
	for _, k := range h.ui.kinds {
		if k == AskConfirm {
			return true
		}
	}
	return false
}

func containsString(h *harness, needle string) bool {
	return len(h.ui.kinds) > 0 && contains(h.out.String(), needle)
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// The reason must survive into the prompt text, or the walk's care is wasted.
func TestConfirmationTitleCarriesTheReason(t *testing.T) {
	plain := confirmationTitle(2, "")
	if !contains(plain, "Execute 2 action(s)?") {
		t.Errorf("title lost its original text: %q", plain)
	}
	withWhy := confirmationTitle(2, `"touch /tmp/x" is not on the whitelist`)
	if !contains(withWhy, "Why it is asking") || !contains(withWhy, "touch /tmp/x") {
		t.Errorf("title dropped the reason: %q", withWhy)
	}
}

// oldGateApproves is the pre-fix rule, kept here and nowhere else: a
// start-anchored regex matched against the raw command text, with the remainder
// never examined.
func oldGateApproves(patterns []string, command string) bool {
	for _, p := range patterns {
		if regexp.MustCompile("^(?:" + p + ")").MatchString(command) {
			return true
		}
	}
	return false
}

// The negative control for A-1. Every bypass row above is asserted to have been
// approved by the old rule, so the table is evidence rather than decoration: if a
// row ever stops failing here it was never a real bypass and proves nothing.
func TestOldGateApprovedEveryBypassRow(t *testing.T) {
	bypasses := []struct {
		patterns []string
		cmd      string
	}{
		{[]string{"echo"}, "echo hi; touch /tmp/wlpwned"},
		{[]string{"git status"}, "git status && rm -rf /"},
		{[]string{"git status"}, "git status || rm -rf /"},
		{[]string{"echo"}, "echo hi\ntouch /tmp/wlpwned"},
		{[]string{"echo"}, "echo hi | sh"},
		{[]string{"echo"}, "echo hi & rm -rf /"},
		{[]string{"echo .*"}, "echo $(touch /tmp/wlpwned)"},
		{[]string{"echo .*"}, "echo `touch /tmp/wlpwned`"},
		{[]string{"echo .*"}, "echo > /tmp/wlpwned"},
		{[]string{"ls"}, "ls -la"},
		{[]string{"echo a"}, "echo a; echo b"},
		{[]string{"git status"}, "git status | grep secret"},
	}
	for _, b := range bypasses {
		if !oldGateApproves(b.patterns, b.cmd) {
			t.Errorf("control is vacuous: the OLD gate did not even approve %q with %v", b.cmd, b.patterns)
		}
		if v := whitelistChecks(b.cmd, mustRe(t, b.patterns...)); v.Allowed {
			t.Errorf("regression: the new gate approves a known bypass: %q with %v", b.cmd, b.patterns)
		}
	}
}

func mustRe(t *testing.T, patterns ...string) []*regexp.Regexp {
	t.Helper()
	re, err := compileWhitelist(patterns)
	if err != nil {
		t.Fatal(err)
	}
	return re
}
