package tpl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/VictorLowther/mini/config"
)

// TestGolden diffs the Go renderer against output produced by the real Jinja
// engine from the Python sources, so the template translation in
// config/configs/*.yaml is byte-exact rather than approximately right.
//
// Goldens come from testdata/gen_goldens.py; the test skips when they are
// absent (a checkout without the Python reference next to it).
func TestGolden(t *testing.T) {
	goldens, err := filepath.Glob(filepath.Join("..", "testdata", "templates", "*.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if len(goldens) == 0 {
		t.Skip("no goldens: run uv run --with jinja2 --with pyyaml python3 testdata/gen_goldens.py --write-contexts")
	}
	sources := map[string]map[string]any{}
	for _, g := range goldens {
		name := strings.TrimSuffix(filepath.Base(g), ".golden")
		parts := strings.Split(name, "__")
		if len(parts) != 3 {
			t.Fatalf("golden %q must be named <spec>__<template_key>__<case>", name)
		}
		spec, key := parts[0], parts[1]
		if _, ok := sources[spec]; !ok {
			m, err := config.FromSpec(spec)
			if err != nil {
				t.Fatalf("%s: %v", spec, err)
			}
			sources[spec] = m
		}
		src := templateSource(sources[spec], key)
		if src == "" {
			t.Fatalf("%s.yaml has no %s template", spec, key)
		}
		ctxFile := filepath.Join("..", "testdata", "contexts", name+".json")
		raw, err := os.ReadFile(ctxFile)
		if err != nil {
			t.Fatal(err)
		}
		ctx := map[string]any{}
		if err := json.Unmarshal(raw, &ctx); err != nil {
			t.Fatalf("%s: %v", ctxFile, err)
		}
		want, err := os.ReadFile(g)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Render(name, src, ctx)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != string(want) {
			t.Errorf("%s: rendered output differs from the Jinja golden\n%s", name, describeDiff(got, string(want)))
		}
	}
}

func templateSource(m map[string]any, key string) string {
	for _, section := range []string{"agent", "model", "environment"} {
		if sub := config.Sub(m, section); sub != nil {
			if s, ok := sub[key].(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// describeDiff points at the first differing character.
func describeDiff(got, want string) string {
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	lo := max(0, i-80)
	return "first difference at byte " + strconv.Itoa(i) +
		"\n  go: ..." + quote(got[lo:min(len(got), i+80)]) +
		"\n jinja: ..." + quote(want[lo:min(len(want), i+80)])
}

func quote(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "\n", `\n`), "\t", `\t`) }
