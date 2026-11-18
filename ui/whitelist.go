package ui

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/VictorLowther/mini/env"
)

// The whitelist, as a walk over a parsed command.
//
// It is a separate function from the gate on purpose. The gate is a policy
// question with a human attached to it - ask, or do not ask, and say why. The
// walk below is a fact about a shell program: does this command consist solely of
// simple commands the user authorized? Keeping them apart is what lets the next
// policy (a blacklist, a sanitiser, a rewrite) reuse the parse without inheriting
// the prompt.

// whitelistRe compiles each entry as a whole-command match.
//
// Two changes from the original port, both security-relevant. The pattern is
// wrapped in a group so an entry's own alternation stays inside it, and it is
// anchored at BOTH ends. The old `^(?:p)` matched a prefix, which is A-1: it let
// `git status` approve `git status; rm -rf /`, because everything after the
// authorized prefix was simply never looked at.
//
// This is a deliberate divergence from upstream, whose re.match is start-anchored
// the same way. The reasoning, and the compatibility story, is in
// docs/whitelist-gate-hardening.md; do not "restore fidelity" here without
// reading it.
func compileWhitelist(patterns []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, p := range patterns {
		re, err := regexp.Compile("^(?:" + p + ")\\s*$")
		if err != nil {
			return nil, fmt.Errorf("whitelist pattern %q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// whitelistVerdict is the walk's answer, with the reason kept because a human who
// is interrupted without being told why will disable the gate rather than fix the
// pattern.
type whitelistVerdict struct {
	Allowed bool
	Reason  string
}

// whitelistChecks reports whether a command may skip confirmation.
//
// The rule is fail-closed in every direction: it must parse, the parse must
// reduce to a list of simple commands with no dynamic words, and *every* command
// in that list must match some pattern in full. A pipeline or `;` chain is
// therefore only free if each stage was authorized on its own - which is the
// friendlier reading of the user's intent than refusing every separator, and the
// safe reading of A-1.
//
// An empty whitelist approves nothing, as before.
func whitelistChecks(command string, patterns []*regexp.Regexp) whitelistVerdict {
	if len(patterns) == 0 {
		return whitelistVerdict{Reason: "no whitelist is configured"}
	}
	prog, err := env.Decompose(command)
	if err != nil {
		return whitelistVerdict{Reason: "the command did not parse as bash"}
	}
	steps, err := prog.Steps()
	if err != nil {
		if !errors.Is(err, env.ErrNotPlain) {
			return whitelistVerdict{Reason: "the command could not be inspected"}
		}
		return whitelistVerdict{Reason: err.Error()}
	}
	for _, s := range steps {
		if !matchesAny(s.Text, patterns) {
			if len(steps) == 1 {
				return whitelistVerdict{Reason: fmt.Sprintf("%q is not on the whitelist", s.Text)}
			}
			return whitelistVerdict{Reason: fmt.Sprintf("%q is not on the whitelist, and every command run must be", s.Text)}
		}
	}
	return whitelistVerdict{Allowed: true}
}

func matchesAny(text string, patterns []*regexp.Regexp) bool {
	for _, re := range patterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}
