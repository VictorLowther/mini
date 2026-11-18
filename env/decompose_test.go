package env

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestStepsFlattensOnlySequencing(t *testing.T) {
	cases := []struct {
		cmd  string
		want []string // nil means: must be refused
	}{
		// plain
		{cmd: "ls -la", want: []string{"ls -la"}},
		{cmd: "echo hi", want: []string{"echo hi"}},
		// sequencing connectors flatten, so every part can be checked
		{cmd: "echo a; echo b", want: []string{"echo a", "echo b"}},
		{cmd: "echo a\necho b", want: []string{"echo a", "echo b"}},
		{cmd: "echo a && echo b", want: []string{"echo a", "echo b"}},
		{cmd: "echo a || echo b", want: []string{"echo a", "echo b"}},
		{cmd: "git status | grep foo", want: []string{"git status", "grep foo"}},
		{cmd: "echo a; echo b; echo c", want: []string{"echo a", "echo b", "echo c"}},
		// A-1 proper: the injection splits into two steps, the second one
		// unmatched, so a gate requiring all-steps-approved refuses it.
		{cmd: "echo hi; touch /tmp/wlpwned", want: []string{"echo hi", "touch /tmp/wlpwned"}},
		{cmd: "git status && rm -rf /", want: []string{"git status", "rm -rf /"}},

		// quoting: one literal argument, not two commands. This is the case a
		// text separator-blacklist gets wrong and the tree cannot.
		{cmd: "echo 'a; b'", want: []string{"echo 'a; b'"}},
		{cmd: `echo "a && b"`, want: []string{`echo "a && b"`}},
		{cmd: "echo 'rm -rf /'", want: []string{"echo 'rm -rf /'"}},

		// anything with dynamic text is refused, not interpreted
		{cmd: "echo $(whoami)"},
		{cmd: "echo `date`"},
		{cmd: "echo $HOME"},
		{cmd: "echo ${PATH}"},
		{cmd: "echo $((1+1))"},
		{cmd: "echo <(date)"},
		{cmd: `echo "a $(b) c"`},
		{cmd: `echo $'a\nb'`},

		// structure the text does not show
		{cmd: "echo hi > /tmp/x"},
		{cmd: "echo hi 2>&1"},
		{cmd: "cat <<EOF\nhi\nEOF"},
		{cmd: "echo hi >> /tmp/x"},
		{cmd: "(cd /; rm -rf x)"},
		{cmd: "{ echo a; }"},
		{cmd: "for i in 1; do echo $i; done"},
		{cmd: "while true; do echo a; done"},
		{cmd: "if true; then echo a; fi"},
		{cmd: "case x in y) echo a;; esac"},
		{cmd: "f() { echo a; }"},
		{cmd: "echo a &"},
		{cmd: "! echo a"},
		{cmd: "FOO=1 echo a"},
		{cmd: "echo a |& cat"},

		// NOT refused, by construction rather than by oversight: brace expansion
		// is not a node type - the parser leaves `{a,b}` inside a Lit and the
		// expand layer fans it out at runtime. There is nothing structural to
		// check, and a textual hunt for braces is the exact approach this design
		// rejects. Same as globbing, which has always been literal text: an
		// approved command can act on more arguments than its text shows. See
		// TestWordExpansionHappensAfterApproval for the consequence.
		{cmd: "echo {a,b}", want: []string{"echo {a,b}"}},
		{cmd: "rm -rf /tmp/*", want: []string{"rm -rf /tmp/*"}},
		{cmd: "[[ -f x ]]"},
		{cmd: "time echo a"},
		{cmd: "declare -x FOO=1"},
		{cmd: "((1+1))"},

		// degenerate and unparseable inputs refuse too
		{cmd: ""},
		{cmd: "# just a comment"},
		{cmd: "echo a;; echo b"},
		{cmd: "echo (unbalanced"},
	}

	for _, tc := range cases {
		prog, err := Decompose(tc.cmd)
		if err != nil {
			if tc.want != nil {
				t.Errorf("%q: unexpected parse error: %v", tc.cmd, err)
			}
			continue
		}
		steps, err := prog.Steps()
		if tc.want == nil {
			if err == nil {
				t.Errorf("%q: wanted a refusal, got %d step(s)", tc.cmd, len(steps))
			} else if !errors.Is(err, ErrNotPlain) {
				t.Errorf("%q: refusal must satisfy errors.Is(ErrNotPlain), got %v", tc.cmd, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: wanted %v, refused: %v", tc.cmd, tc.want, err)
			continue
		}
		got := make([]string, len(steps))
		for i, s := range steps {
			got[i] = s.Text
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%q: steps = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

// The gate's whole claim is that it inspects what the interpreter runs, so the
// two must agree about what is even parseable.
func TestDecomposeAgreesWithLocalOnParseValidity(t *testing.T) {
	for _, cmd := range []string{"echo ok", "echo (broken", "echo a;; echo b", "ls -la"} {
		_, derr := Decompose(cmd)
		res, rerr := (&Local{Timeout: 5 * time.Second}).Execute(t.Context(), cmd, t.TempDir(), io.Discard)
		if rerr != nil {
			t.Fatal(rerr)
		}
		parsedByLocal := res.Extra["exception_type"] != "ParserError"
		parsedByGate := derr == nil
		if parsedByLocal != parsedByGate {
			t.Errorf("%q: Local parsed=%v, Decompose parsed=%v - they must agree", cmd, parsedByLocal, parsedByGate)
		}
	}
}

func TestLitSeparatesEmptyLiteralFromNonLiteral(t *testing.T) {
	// Word.Lit() upstream cannot tell these apart; the gate must.
	for cmd, want := range map[string]string{
		`echo ""`:    "",
		`echo ''`:    "",
		`echo a`:     "a",
		`echo 'a b'`: "a b",
	} {
		prog, err := Decompose(cmd)
		if err != nil {
			t.Fatal(err)
		}
		steps, err := prog.Steps()
		if err != nil {
			t.Fatalf("%q: %v", cmd, err)
		}
		got, ok := Lit(steps[0].Args[1])
		if !ok || got != want {
			t.Errorf("%q: Lit = %q,%v want %q,true", cmd, got, ok, want)
		}
	}
}

// The whitelist approves a command; it does not approve the set of files that
// command then expands to. Both glob and brace expansion happen in the interpreter,
// after parsing, so no structural check can see the fan-out - `echo {a,b}` is one
// word in the tree and two arguments at runtime. Recorded as a test because it is a
// limit of the design, and a comment in a config file is how it should be met: a
// whitelist entry is an authorization to run that program with that shape of
// argument, not an enumeration of paths.
func TestWordExpansionHappensAfterApproval(t *testing.T) {
	prog, err := Decompose("echo {a,b}")
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prog.Steps()
	if err != nil {
		t.Fatalf("brace expansion is not a distinct node, so it cannot be refused: %v", err)
	}
	if got, _ := Lit(steps[0].Args[1]); got != "{a,b}" {
		t.Fatalf("the tree carries the text %q, not the expanded words", got)
	}
}
