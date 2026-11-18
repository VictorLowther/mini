package env

import (
	"errors"
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Program is one parsed command: the whole syntax tree, plus the source it was
// parsed from so any node can be named in the text the human wrote.
//
// The tree is returned rather than a flattened command list, and that is the
// load-bearing choice in this file:
//
//   - The tree is already what gets executed. Local parses once (local.go) and
//     runs the resulting *syntax.File via runner.Run, so a caller that inspects
//     this tree is inspecting the object the interpreter will act on. The gate
//     and the execution cannot disagree about what a command is, because there
//     is no second parse of a second string to disagree with.
//   - The features queued behind the whitelist need the tree and nothing less.
//     A blacklist has to see node kinds the whitelist ignores (a Redirect whose
//     word is a CmdSubst; a ProcCmd; a TestClause). Sanitisation and rewriting
//     have to *mutate* nodes and then have the mutation be what runs, which only
//     makes sense if the thing handed around is the tree. A []SimpleCommand
//     answers today's question and forces the next feature to build a parallel,
//     text-based API - and a second API that re-parses text is precisely the
//     drift this exists to remove.
//   - syntax already exposes Walk and Printer, so there is no new dependency and
//     no reinvented AST.
//
// File is a public field on purpose. Anything that wants the tree takes it
// directly; the helpers below are convenience over it, not a fence around it.
type Program struct {
	File *syntax.File

	src string
}

// Decompose parses a command into a Program.
//
// It uses the same parser configuration as Local, deliberately: if the two ever
// parse a command differently, the gate approves text the interpreter then
// reads another way. A parse error is returned unchanged - Local refuses the
// same input for the same reason, so "unparseable" is not a gate decision.
func Decompose(command string) (*Program, error) {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, err
	}
	return &Program{File: file, src: command}, nil
}

// Source is the exact text the program was parsed from.
func (p *Program) Source() string { return p.src }

// Text returns the source slice a node occupies. It reports false when the
// position is missing or out of range rather than guessing, because every
// caller's rule is that unknown means ask. Use this for matching: it is the text
// the human wrote, which is what existing whitelist patterns were written
// against; Args carries the shell-parsed form when you need that instead.
func (p *Program) Text(n syntax.Node) (string, bool) {
	if n == nil {
		return "", false
	}
	start, end := n.Pos().Offset(), n.End().Offset()
	if end <= start || int(end) > len(p.src) {
		return "", false
	}
	return strings.TrimSpace(p.src[start:end]), true
}

// Print renders a node back to shell source. It is the primitive a future
// rewriter needs: mutate the tree, print it, and run the printed form - or run
// the mutated tree, since Local runs the tree already.
func (p *Program) Print(n syntax.Node) (string, error) {
	var b strings.Builder
	if err := syntax.NewPrinter().Print(&b, n); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Walk visits every node of the tree, depth-first, in source order. fn returns
// false to skip that node's children. This is the seam a blacklist is built on.
func (p *Program) Walk(fn func(syntax.Node) bool) {
	if p.File != nil {
		syntax.Walk(p.File, fn)
	}
}

// HasNode reports whether any node in the tree satisfies pred - the shape a
// "refuse if the command contains X" rule takes, where X is a node kind rather
// than a substring (a CmdSubst inside single quotes is not a substitution, and
// only the tree knows the difference).
func (p *Program) HasNode(pred func(syntax.Node) bool) bool {
	found := false
	p.Walk(func(n syntax.Node) bool {
		if found {
			return false
		}
		if pred(n) {
			found = true
			return false
		}
		return true
	})
	return found
}

// Step is one leaf command: the simplest thing a whitelist can reason about.
type Step struct {
	// Text is the command exactly as written, which is what a whitelist pattern
	// matches. It never includes a trailing separator.
	Text string
	// Args is the shell-parsed word list: quotes removed, escapes resolved.
	// Program.Lit words only, so it is a real argv as the shell would build it
	// from these bytes - useful for a policy that cares about the first word.
	Args []*syntax.Word
	// Node is the call itself, for callers that need more than the text.
	Node *syntax.CallExpr
}

// ErrNotPlain is what Steps returns for a program it will not describe as a flat
// list of simple commands. It wraps a human-readable reason, because the
// confirmation prompt should say what broke the whitelist rather than just
// nagging - a gate nobody can learn is a gate that gets deleted.
var ErrNotPlain = errors.New("env: not a plain list of simple commands")

type notPlain struct{ why string }

func (e *notPlain) Error() string        { return e.why }
func (e *notPlain) Is(target error) bool { return target == ErrNotPlain }
func notPlainf(format string, a ...any) error {
	return &notPlain{why: fmt.Sprintf(format, a...)}
}

// Steps flattens the program into the leaf commands it runs, and refuses
// otherwise.
//
// It walks through the connectors that only sequence work - `;`, a newline, `&&`,
// `||`, and `|` - so `echo a; echo b` is two steps and a caller can require both
// to be authorized. That is what makes structural checking usable rather than
// useless: a rule that asks on every separator teaches the model to stop using
// the whitelist, and a rule that approves the whole chain because its first word
// was authorized is A-1.
//
// Everything else is refused rather than interpreted, including things that look
// innocuous in text: a subshell, a block, any loop, a function declaration, a
// negated or backgrounded statement, a redirection or heredoc, an assignment, a
// `|&` coprocess pipe, and any word that is not statically literal - which is
// where `$(`, backticks, `${…}`, `<(`, `((…))` and brace expansion all land. The
// test for "literal" is the tree's, so `'a; b'` passes as one literal argument
// while `a; b` is two commands: quoting is exactly where text matching and shell
// meaning diverge, and this is the one place that can tell them apart.
func (p *Program) Steps() ([]Step, error) {
	var steps []Step
	var walk func(*syntax.Stmt) error

	walk = func(s *syntax.Stmt) error {
		if s == nil || s.Cmd == nil {
			return notPlainf("an empty statement")
		}
		switch c := s.Cmd.(type) {
		case *syntax.BinaryCmd:
			switch c.Op {
			case syntax.AndStmt, syntax.OrStmt, syntax.Pipe:
			default:
				return notPlainf("the connector %q", c.Op.String())
			}
			if err := walk(c.X); err != nil {
				return err
			}
			return walk(c.Y)

		case *syntax.CallExpr:
			if s.Background {
				return notPlainf("a command run in the background")
			}
			if s.Negated {
				return notPlainf("a negated command")
			}
			if s.Coprocess {
				return notPlainf("a coprocess pipe")
			}
			if s.Disown {
				return notPlainf("a disowned command")
			}
			if len(s.Redirs) > 0 {
				return notPlainf("a redirection or heredoc")
			}
			if len(c.Assigns) > 0 {
				return notPlainf("an inline assignment")
			}
			if len(c.Args) == 0 {
				return notPlainf("a command with no words")
			}
			for _, w := range c.Args {
				if _, ok := Lit(w); !ok {
					text, _ := p.Text(w)
					return notPlainf("the word %q, which is not a static literal", text)
				}
			}
			text, ok := p.Text(c)
			if !ok {
				return notPlainf("a command whose text cannot be located")
			}
			steps = append(steps, Step{Text: text, Args: c.Args, Node: c})
			return nil

		default:
			return notPlainf("a %s", nodeKind(c))
		}
	}

	for _, s := range p.File.Stmts {
		if err := walk(s); err != nil {
			return nil, err
		}
	}
	if len(steps) == 0 {
		return nil, notPlainf("no command at all")
	}
	return steps, nil
}

// Lit returns a word's literal value, and whether it has one at all. A word is
// literal when every part is plain text or a quoted group of plain text, which
// means: no command substitution, no parameter expansion, no arithmetic, no
// process substitution, no brace expansion.
//
// This is not Word.Lit() from upstream: that returns "" both for a word that is
// not a literal and for the empty literal, which cannot be told apart. The
// difference is the whole of `echo ""`, so this walks the parts instead.
func Lit(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range w.Parts {
		switch v := part.(type) {
		case *syntax.Lit:
			b.WriteString(v.Value)
		case *syntax.SglQuoted:
			if v.Dollar {
				// $'…' resolves escapes, so its text is not its value.
				return "", false
			}
			b.WriteString(v.Value)
		case *syntax.DblQuoted:
			for _, inner := range v.Parts {
				l, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(l.Value)
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// nodeKind names a command node for the "why did this need approval" line.
func nodeKind(n any) string {
	switch n.(type) {
	case *syntax.Subshell:
		return "subshell"
	case *syntax.Block:
		return "brace group"
	case *syntax.IfClause:
		return "conditional"
	case *syntax.WhileClause:
		return "loop"
	case *syntax.ForClause:
		return "for loop"
	case *syntax.CaseClause:
		return "case statement"
	case *syntax.FuncDecl:
		return "function definition"
	case *syntax.TestClause:
		return "test clause"
	case *syntax.ArithmCmd:
		return "arithmetic command"
	case *syntax.DeclClause:
		return "declaration builtin"
	case *syntax.TimeClause:
		return "time clause"
	case *syntax.CoprocClause:
		return "coprocess"
	case *syntax.LetClause:
		return "let expression"
	}
	return strings.TrimPrefix(fmt.Sprintf("%T", n), "*syntax.")
}
