package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// lookup walks a dotted key path through nested map[string]any values.
func lookup(m map[string]any, key string) (any, bool) {
	cur := any(m)
	for _, part := range strings.Split(key, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func raw(m map[string]any, key string) (any, bool) {
	v, ok := lookup(m, key)
	if !ok || v == nil || v == Unset {
		return nil, false
	}
	return v, true
}

// String reads a string value; other scalars are stringified the way pydantic
// would coerce them.
func String(m map[string]any, key, def string) string {
	v, ok := raw(m, key)
	if !ok {
		return def
	}
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case bool:
		return strconv.FormatBool(t)
	}
	return def
}

// number coerces YAML/JSON scalars (yaml.v3 gives int or float64, JSON decoding
// only ever float64) and numeric strings to float64.
func number(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

// Int reads an integer value, truncating toward zero like Python's int().
func Int(m map[string]any, key string, def int) int {
	v, ok := raw(m, key)
	if !ok {
		return def
	}
	if f, ok := number(v); ok {
		return int(f)
	}
	return def
}

// Float reads a float value.
func Float(m map[string]any, key string, def float64) float64 {
	v, ok := raw(m, key)
	if !ok {
		return def
	}
	if f, ok := number(v); ok {
		return f
	}
	return def
}

// Bool reads a bool value, accepting the usual YAML/JSON spellings.
func Bool(m map[string]any, key string, def bool) bool {
	v, ok := raw(m, key)
	if !ok {
		return def
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		if b, err := strconv.ParseBool(strings.TrimSpace(t)); err == nil {
			return b
		}
	}
	if f, ok := number(v); ok {
		return f != 0
	}
	return def
}

// StringSlice reads a list of strings; a bare string becomes a one-element slice.
func StringSlice(m map[string]any, key string) []string {
	v, ok := raw(m, key)
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []string:
		return t
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	return nil
}

// Sub returns a nested map (e.g. the "agent" section) or nil.
func Sub(m map[string]any, key string) map[string]any {
	if v, ok := lookup(m, key); ok {
		if mm, ok := v.(map[string]any); ok {
			return mm
		}
	}
	return nil
}
