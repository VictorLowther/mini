package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMerge(t *testing.T) {
	cases := []struct {
		name string
		ms   []map[string]any
		want map[string]any
	}{
		{"no args", nil, map[string]any{}},
		{"single", []map[string]any{{"a": 1, "b": 2}}, map[string]any{"a": 1, "b": 2}},
		{"simple override", []map[string]any{{"a": 1, "b": 2}, {"b": 3, "c": 4}}, map[string]any{"a": 1, "b": 3, "c": 4}},
		{
			"nested merge",
			[]map[string]any{{"a": map[string]any{"x": 1, "y": 2}, "b": 3}, {"a": map[string]any{"y": 3, "z": 4}, "c": 5}},
			map[string]any{"a": map[string]any{"x": 1, "y": 3, "z": 4}, "b": 3, "c": 5},
		},
		{
			"deeply nested",
			[]map[string]any{{"a": map[string]any{"b": map[string]any{"c": 1, "d": 2}}}, {"a": map[string]any{"b": map[string]any{"d": 3, "e": 4}}}},
			map[string]any{"a": map[string]any{"b": map[string]any{"c": 1, "d": 3, "e": 4}}},
		},
		{"dict overridden by scalar", []map[string]any{{"a": map[string]any{"x": 1}}, {"a": "string"}}, map[string]any{"a": "string"}},
		{"scalar overridden by dict", []map[string]any{{"a": "string"}, {"a": map[string]any{"x": 1}}}, map[string]any{"a": map[string]any{"x": 1}}},
		{
			"lists replace, never merge",
			[]map[string]any{{"list": []any{1, 2, 3}}, {"list": []any{4, 5}}},
			map[string]any{"list": []any{4, 5}},
		},
		{
			"nil maps skipped",
			[]map[string]any{nil, {"a": 1}, nil, {"b": 2}},
			map[string]any{"a": 1, "b": 2},
		},
		{
			"empty nested dicts",
			[]map[string]any{{"a": map[string]any{}}, {"a": map[string]any{"x": 1}}},
			map[string]any{"a": map[string]any{"x": 1}},
		},
		{
			"unset values dropped",
			[]map[string]any{{"a": 1, "b": Unset}, {"a": Unset, "c": 4}},
			map[string]any{"a": 1, "c": 4},
		},
		{
			"nested unset only becomes empty map",
			[]map[string]any{{"run": map[string]any{"task": Unset}}},
			map[string]any{"run": map[string]any{}},
		},
		{
			"deeply nested unset",
			[]map[string]any{{"a": map[string]any{"b": map[string]any{"c": Unset}}}},
			map[string]any{"a": map[string]any{"b": map[string]any{}}},
		},
		{
			"mixed unset",
			[]map[string]any{{"run": map[string]any{"task": Unset, "other": "value"}}},
			map[string]any{"run": map[string]any{"other": "value"}},
		},
		{
			"null value survives as nil",
			[]map[string]any{{"none": nil}, {"none": Unset}},
			map[string]any{"none": nil},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Merge(c.ms...); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Merge() = %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestMergeDoesNotMutateInputs(t *testing.T) {
	d1 := map[string]any{"a": map[string]any{"x": 1}}
	d2 := map[string]any{"a": map[string]any{"y": 2}}
	Merge(d1, d2)
	if !reflect.DeepEqual(d1, map[string]any{"a": map[string]any{"x": 1}}) {
		t.Errorf("first input mutated: %#v", d1)
	}
	if !reflect.DeepEqual(d2, map[string]any{"a": map[string]any{"y": 2}}) {
		t.Errorf("second input mutated: %#v", d2)
	}
}

func TestKeyOverride(t *testing.T) {
	cases := []struct {
		spec string
		want map[string]any
	}{
		{"key=value", map[string]any{"key": "value"}},
		{`key="value"`, map[string]any{"key": "value"}},
		{"agent.mode=yolo", map[string]any{"agent": map[string]any{"mode": "yolo"}}},
		{"a.b.c=value", map[string]any{"a": map[string]any{"b": map[string]any{"c": "value"}}}},
		{"count=42", map[string]any{"count": float64(42)}},
		{"value=-10", map[string]any{"value": float64(-10)}},
		{"temperature=0.7", map[string]any{"temperature": 0.7}},
		{"enabled=true", map[string]any{"enabled": true}},
		{"enabled=false", map[string]any{"enabled": false}},
		{"value=null", map[string]any{"value": nil}},
		{`tags=["a","b"]`, map[string]any{"tags": []any{"a", "b"}}},
		{`config={"k":"v"}`, map[string]any{"config": map[string]any{"k": "v"}}},
		{`key=""`, map[string]any{"key": ""}},
		{`message="hello world"`, map[string]any{"message": "hello world"}},
		{"value=0", map[string]any{"value": float64(0)}},
		{"model.model_name=anthropic/claude-sonnet-4-5", map[string]any{"model": map[string]any{"model_name": "anthropic/claude-sonnet-4-5"}}},
		{"a=b=c", map[string]any{"a": "b=c"}},
		{"raw=not json [", map[string]any{"raw": "not json ["}},
	}
	for _, c := range cases {
		t.Run(c.spec, func(t *testing.T) {
			got, err := KeyOverride(c.spec)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("KeyOverride(%q) = %#v, want %#v", c.spec, got, c.want)
			}
		})
	}
}

func TestKeyOverrideEmptyKey(t *testing.T) {
	for _, spec := range []string{"=value", "model..name=value", "model.=value"} {
		_, err := KeyOverride(spec)
		if err == nil || !strings.Contains(err.Error(), "empty config key") {
			t.Errorf("KeyOverride(%q) error = %v, want \"empty config key\"", spec, err)
		}
	}
}

func TestResolveEmbeddedByName(t *testing.T) {
	data, name, err := Resolve("mini")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(name) != "mini.yaml" {
		t.Errorf("resolved to %q", name)
	}
	if !strings.Contains(string(data), "system_template") {
		t.Error("embedded mini.yaml did not contain expected keys")
	}
}

func TestResolveSearchOrder(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MINI_CONFIG_DIR", "")
	os.WriteFile(filepath.Join(dir, "thing.yaml"), []byte("from: cwd\n"), 0o644)
	m, err := FromSpec("thing")
	if err != nil {
		t.Fatal(err)
	}
	if m["from"] != "cwd" {
		t.Errorf("cwd candidate should win, got %#v", m)
	}

	os.Remove(filepath.Join(dir, "thing.yaml"))
	other := filepath.Join(dir, "cfgdir")
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, "thing.yaml"), []byte("from: configdir\n"), 0o644)
	t.Setenv("MINI_CONFIG_DIR", other)
	m, err = FromSpec("thing")
	if err != nil {
		t.Fatal(err)
	}
	if m["from"] != "configdir" {
		t.Errorf("MINI_CONFIG_DIR candidate should win, got %#v", m)
	}
}

func TestResolveMissing(t *testing.T) {
	_, _, err := Resolve("definitely-not-a-config")
	if err == nil || !strings.Contains(err.Error(), "could not find config file") {
		t.Fatalf("err = %v", err)
	}
}

func TestFromSpecPrefersKeyValue(t *testing.T) {
	m, err := FromSpec("agent.step_limit=5")
	if err != nil {
		t.Fatal(err)
	}
	if Int(Sub(m, "agent"), "step_limit", 0) != 5 {
		t.Errorf("got %#v", m)
	}
}

func TestFromSpecYAMLTypes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte("number: 42\nfloat: 3.\nflag: true\ntext: a — b\nlist: [1, 2]\n"), 0o644)
	m, err := FromSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	if Int(m, "number", 0) != 42 || Float(m, "float", 0) != 3 || !Bool(m, "flag", false) {
		t.Errorf("bad scalars: %#v", m)
	}
	if String(m, "text", "") != "a — b" {
		t.Errorf("utf8 not preserved: %q", m["text"])
	}
	if len(StringSlice(m, "list")) != 2 {
		t.Errorf("list: %#v", m["list"])
	}
}

func TestMiniYAMLLoadsAsAgentModelEnvConfigs(t *testing.T) {
	m, err := FromSpec("mini")
	if err != nil {
		t.Fatal(err)
	}
	agent := Sub(m, "agent")
	if String(agent, "system_template", "") != "You are a helpful assistant that can interact with a computer.\n" {
		t.Errorf("system_template = %q", agent["system_template"])
	}
	if Float(agent, "cost_limit", 0) != 3.0 {
		t.Errorf("cost_limit = %v", agent["cost_limit"])
	}
	if String(agent, "mode", "") != "confirm" {
		t.Errorf("mode = %v", agent["mode"])
	}
	if String(Sub(m, "model"), "observation_template", "") == "" {
		t.Error("missing observation_template")
	}
	if StringSlice(Sub(m, "environment"), "env") != nil {
		if _, ok := Sub(Sub(m, "environment"), "env")["PAGER"]; !ok {
			t.Error("environment.env.PAGER missing")
		}
	}
}

func TestDotenvRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := SetEnvValue(path, "MINI_MODEL_NAME", "sushi/qwen"); err != nil {
		t.Fatal(err)
	}
	if err := SetEnvValue(path, "API_KEY", "sk-1'2\"3"); err != nil {
		t.Fatal(err)
	}
	if err := SetEnvValue(path, "OTHER", "keep"); err != nil {
		t.Fatal(err)
	}
	if err := SetEnvValue(path, "MINI_MODEL_NAME", "sushi/mimo"); err != nil {
		t.Fatal(err)
	}
	got, err := LoadEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"MINI_MODEL_NAME": "sushi/mimo", "API_KEY": `sk-1'2"3`, "OTHER": "keep"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadEnv = %#v, want %#v", got, want)
	}

	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 || lines[0] != "MINI_MODEL_NAME='sushi/mimo'" {
		t.Errorf("unexpected file layout: %q", string(data))
	}

	if err := DeleteEnvValue(path, "OTHER"); err != nil {
		t.Fatal(err)
	}
	got, _ = LoadEnv(path)
	if _, ok := got["OTHER"]; ok {
		t.Errorf("unset did not remove key: %#v", got)
	}
}

func TestLoadEnvMissingFile(t *testing.T) {
	got, err := LoadEnv(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestAccessorsCoerce(t *testing.T) {
	m := map[string]any{
		"i":        7,
		"f":        2.9,
		"s":        "12",
		"t":        true,
		"nil":      nil,
		"nested":   map[string]any{"deep": "yes"},
		"jsonlike": mustJSON(`{"a": 3}`),
	}
	if Int(m, "i", -1) != 7 || Int(m, "f", -1) != 2 || Int(m, "s", -1) != 12 {
		t.Error("Int coercion failed")
	}
	if Float(m, "i", 0) != 7 || Float(m, "nested.deep", 0) != 0 || String(m, "nested.deep", "") != "yes" {
		t.Error("dotted lookup failed")
	}
	if String(m, "i", "") != "7" || String(m, "t", "") != "true" {
		t.Error("String coercion failed")
	}
	if Bool(m, "missing", true) != true || Bool(m, "nil", true) != true || Bool(m, "i", false) != true {
		t.Error("Bool coercion failed")
	}
	if String(m, "jsonlike", "") != "" {
		t.Error("maps must not stringify")
	}
}

func mustJSON(s string) any {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		panic(err)
	}
	return v
}
