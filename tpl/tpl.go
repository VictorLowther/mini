// Package tpl renders mini-swe-agent's prompt templates. The upstream prompts
// are written in Jinja; they have been mechanically translated to
// text/template (see configs/mini.yaml) and this package provides the filters
// the translation relies on.
package tpl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"unicode/utf16"
	"unicode/utf8"
)

// FuncMap holds the Jinja filter equivalents. text/template has no arithmetic
// and no string slicing by character, hence add/sub/mul/div, head/tail/charlen.
// Builtins (len, slice, and, or, not, eq, lt, printf) are deliberately not
// redeclared.
var FuncMap = template.FuncMap{
	"tojson":  toJSON,
	"charlen": func(s string) int { return utf8.RuneCountInString(s) },
	"head":    head,
	"tail":    tail,
	"int":     toInt,
	"add":     func(a, b any) any { return arith("+", a, b) },
	"sub":     func(a, b any) any { return arith("-", a, b) },
	"mul":     func(a, b any) any { return arith("*", a, b) },
	"div":     func(a, b any) any { return arith("/", a, b) },
}

// Render executes source under name against ctx. Missing keys yield the zero
// value rather than an error: every context here is built by this program, and
// aborting an observation render on an absent key would kill a run for cosmetics.
//
// The trailing newline is dropped from the source first: Jinja compiles with
// keep_trailing_newline=False, so `Template("x\n")` renders "x" and the
// prompts the Python agent sends never end in a newline.
func Render(name, source string, ctx map[string]any) (string, error) {
	return render(name, source, ctx, "missingkey=zero")
}

// RenderStrict is Render with `missingkey=error`: a reference to a key the
// context does not carry is an error instead of an empty string.
//
// It exists for the two templates whose context is fixed and whose silence is
// expensive - the system prompt and the instance prompt. Upstream renders every
// prompt with Jinja's StrictUndefined (`agents/default.py`), so a misspelled or
// renamed variable raises there and is invisible here: `{{ .tool_lst }}` ships a
// system prompt with no tool listing, the model quietly stops knowing what it can
// call, and every trajectory afterwards reads like a model-quality problem
// (A-7). Config is user-authored, so "every context here is built by this
// program" is not a promise the config layer can keep.
//
// The observation and format-error templates deliberately keep Render. Their
// context is per-result - a job observation has no `.returncode`, a plain one has
// no `.job_id` - so a missing key there is normal rather than a typo, and making
// it fatal would kill runs over template shape.
func RenderStrict(name, source string, ctx map[string]any) (string, error) {
	return render(name, source, ctx, "missingkey=error")
}

func render(name, source string, ctx map[string]any, missingKey string) (string, error) {
	t, err := template.New(name).Option(missingKey).Funcs(FuncMap).Parse(strings.TrimSuffix(source, "\n"))
	if err != nil {
		return "", fmt.Errorf("parsing template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("rendering template %s: %w", name, err)
	}
	return buf.String(), nil
}

// toJSON matches Jinja's tojson: JSON with <, >, & and ' escaped as \uXXXX and
// every non-ASCII character escaped, so the output is pure ASCII like Python's
// ensure_ascii=True. (Jinja also sorts map keys and puts a space after
// separators; the shipped templates only pass strings here.)
func toJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.Grow(len(b))
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == '\'':
			sb.WriteString(`\u0027`)
			i++
		case c < 0x80:
			sb.WriteByte(c)
			i++
		default:
			r, n := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && n == 1 {
				sb.WriteString(`\ufffd`)
			} else if r <= 0xFFFF {
				fmt.Fprintf(&sb, `\u%04x`, r)
			} else {
				lo, hi := utf16.EncodeRune(r)
				fmt.Fprintf(&sb, `\u%04x\u%04x`, lo, hi)
			}
			i += n
		}
	}
	return sb.String(), nil
}

// byteOffsetAfterRunes returns the byte index just past the n-th character.
func byteOffsetAfterRunes(s string, n int) int {
	count := 0
	for i := range s {
		if count == n {
			return i
		}
		count++
	}
	return len(s)
}

// head returns the first n characters of s (Jinja's s[:n]).
func head(s string, n int) string {
	if n < 0 {
		n = 0
	}
	return s[:byteOffsetAfterRunes(s, n)]
}

// tail returns the last n characters of s (Jinja's s[-n:]).
func tail(s string, n int) string {
	if n < 0 {
		return ""
	}
	return s[byteOffsetAfterRunes(s, max(0, utf8.RuneCountInString(s)-n)):]
}

// scalar coerces to (numeric value, isInteger) the way Jinja's filters do;
// unparseable input is 0, like Jinja's default(...) on undefined arithmetic.
func scalar(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int8, int16, int32, int64:
		return reflectFloat(t), true
	case uint, uint8, uint16, uint32, uint64:
		return reflectFloat(t), true
	case float32:
		return float64(t), false
	case float64:
		return t, false
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return float64(i), true
		}
		f, _ := t.Float64()
		return f, false
	case string:
		s := strings.TrimSpace(t)
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return float64(i), true
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, true
		}
		return f, false
	case nil:
		return 0, true
	}
	return 0, true
}

// toInt truncates toward zero, matching Jinja's int filter.
func toInt(v any) int64 {
	f, _ := scalar(v)
	return int64(f)
}

// arith evaluates a Jinja arithmetic operator. Division is always
// float division (Jinja's /), the rest stay integral when both operands are.
func arith(op string, a, b any) any {
	fa, ia := scalar(a)
	fb, ib := scalar(b)
	isInt := ia && ib
	switch op {
	case "+":
		if isInt {
			return int64(fa) + int64(fb)
		}
		return fa + fb
	case "-":
		if isInt {
			return int64(fa) - int64(fb)
		}
		return fa - fb
	case "*":
		if isInt {
			return int64(fa) * int64(fb)
		}
		return fa * fb
	}
	if fb == 0 {
		return 0
	}
	return fa / fb
}

func reflectFloat(v any) float64 {
	switch t := v.(type) {
	case int8:
		return float64(t)
	case int16:
		return float64(t)
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case uint:
		return float64(t)
	case uint8:
		return float64(t)
	case uint16:
		return float64(t)
	case uint32:
		return float64(t)
	case uint64:
		return float64(t)
	}
	return 0
}
