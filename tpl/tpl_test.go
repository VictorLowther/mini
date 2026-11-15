package tpl

import (
	"strings"
	"testing"

	"github.com/VictorLowther/mini/config"
)

func TestToJSON(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"hi", `"hi"`},
		{"a\nb\"c", `"a\nb\"c"`},
		{`it's`, `"it\u0027s"`},
		{"<b>&", `"\u003cb\u003e\u0026"`},
		{"café", `"caf\u00e9"`},
		{"😀", `"\ud83d\ude00"`},
		{42, `42`},
		{nil, `null`},
	}
	for _, c := range cases {
		got, err := toJSON(c.in)
		if err != nil || got != c.want {
			t.Errorf("toJSON(%#v) = %s, %v, want %s", c.in, got, err, c.want)
		}
	}
}

func TestHeadTailCharlen(t *testing.T) {
	cases := []struct{ src, want string }{
		{`{{ head .s 3 }}`, "hel"},
		{`{{ head .s 99 }}`, "hello"},
		{`{{ tail .s 2 }}`, "lo"},
		{`{{ tail .s 99 }}`, "hello"},
		{`{{ charlen .s }}`, "5"},
		{`{{ head .u 2 }}`, "hé"},
		{`{{ tail .u 2 }}`, "lo"},
		{`{{ charlen .u }}`, "5"},
	}
	ctx := map[string]any{"s": "hello", "u": "héllo"}
	for _, c := range cases {
		got, err := Render("t", c.src, ctx)
		if err != nil || got != c.want {
			t.Errorf("Render(%s) = %q (%v), want %q", c.src, got, err, c.want)
		}
	}
	long := strings.Repeat("x", 12000)
	if got, _ := Render("t", `{{ tail .s 5000 }}`, map[string]any{"s": long}); len(got) != 5000 {
		t.Errorf("tail len = %d", len(got))
	}
}

func TestArithAndInt(t *testing.T) {
	cases := []struct{ src, want string }{
		{`{{ sub (charlen .s) 10000 }}`, `-9995`},
		{`{{ add 1 2 }}`, `3`},
		{`{{ mul 2.5 4 }}`, `10`},
		{`{{ div 7 2 }}`, `3.5`},
		{`{{ int 2.9 }}{{ int -2.9 }}`, `2-2`},
		{`{{ int "42" }}`, `42`},
		{`{{ int "not a number" }}`, `0`},
	}
	ctx := map[string]any{"s": "hello"}
	for _, c := range cases {
		got, err := Render("t", c.src, ctx)
		if err != nil || got != c.want {
			t.Errorf("Render(%s) = %q (%v), want %q", c.src, got, err, c.want)
		}
	}
}

func TestRenderWhitespaceAndMissingKey(t *testing.T) {
	got, err := Render("t", "{{- if eq .system \"Darwin\" -}}\non mac\n{{- end -}}", map[string]any{"system": "Darwin"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "on mac" {
		t.Errorf("got %q", got)
	}
	// missingkey=zero: an absent key must not abort the render.
	got, err = Render("t", "[{{ if .nope }}y{{else}}n{{end}}]", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "[n]" {
		t.Errorf("got %q", got)
	}
	if _, err := Render("bad", "{{", nil); err == nil || !strings.Contains(err.Error(), "template bad") {
		t.Errorf("parse error should name the template: %v", err)
	}
}

// TestShippedTemplatesRender guards the embedded config against template
// syntax errors and missing render keys.
func TestShippedTemplatesRender(t *testing.T) {
	ctx := map[string]any{
		"task": "T", "system": "Darwin", "release": "R", "version": "V", "machine": "M",
		"output":         map[string]any{"output": "x", "returncode": 0, "exception_info": ""},
		"error":          "e",
		"actions":        []any{},
		"has_tool_calls": false,
		"finish_reason":  "stop",
		// The observation window. The shipped prompts quote these numbers, so
		// they must resolve here; agent.TemplateVars supplies the real values
		// from model.MaxViewLines/MaxViewChars, and tpl cannot import model
		// (model imports tpl), hence the literals. agent's
		// TestShippedPromptsStateTheWindow keeps them honest.
		"view_lines": 100, "view_chars": 10000,
	}
	for _, spec := range []string{"mini"} {
		m, err := config.FromSpec(spec)
		if err != nil {
			t.Fatalf("%s: %v", spec, err)
		}
		for section, keys := range map[string][]string{
			"agent": {"system_template", "instance_template"},
			"model": {"observation_template", "format_error_template"},
		} {
			sub := config.Sub(m, section)
			for _, key := range keys {
				src, _ := sub[key].(string)
				if src == "" {
					t.Fatalf("%s.%s.%s is empty", spec, section, key)
				}
				out, err := Render(spec+"/"+key, src, ctx)
				if err != nil {
					t.Errorf("%s %s: %v", spec, key, err)
					continue
				}
				if strings.Contains(out, "<no value>") {
					t.Errorf("%s %s rendered a missing key: %s", spec, key, out)
				}
			}
		}
	}
}
