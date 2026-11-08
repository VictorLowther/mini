// Package config ports mini-swe-agent's configuration layering: recursive
// merge with an UNSET sentinel, config-spec resolution (file, embedded name,
// or key=value override) and the global .env file.
package config

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed configs
var embedded embed.FS

// Unset is the sentinel for "this option was not given on the command line".
// Merge skips it, which is what lets CLI flags merge on top of config files
// without clobbering them with zero values.
var Unset any = new(struct{})

// Merge merges maps left to right: later values win, nested maps are merged
// recursively, lists are replaced, Unset values are skipped and nil maps are
// ignored. Inputs are never mutated.
func Merge(ms ...map[string]any) map[string]any {
	result := map[string]any{}
	for _, m := range ms {
		if m == nil {
			continue
		}
		for k, v := range m {
			if v == Unset {
				continue
			}
			if vm, ok := v.(map[string]any); ok {
				// Merge even when there is no counterpart, to deep-copy and
				// strip nested Unset values.
				if em, ok := result[k].(map[string]any); ok {
					result[k] = Merge(em, vm)
				} else {
					result[k] = Merge(vm)
				}
				continue
			}
			result[k] = v
		}
	}
	return result
}

// Resolve finds the YAML file for a config spec (a path, a bare name, or a
// name relative to MINI_CONFIG_DIR) and returns its contents plus the
// resolved name. Candidates are tried in order: the literal path, MINI_CONFIG_DIR,
// then the configs/ subdirectory of the working directory or of the binary's
// embedded copy (config/configs).
func Resolve(spec string) (data []byte, name string, err error) {
	spec = forceYAMLSuffix(spec)
	candidates := []string{
		spec,
		filepath.Join(os.Getenv("MINI_CONFIG_DIR"), spec),
		filepath.Join("configs", spec),
	}
	for _, c := range candidates {
		if b, err := os.ReadFile(c); err == nil {
			return b, c, nil
		}
		// Absolute specs make the embedded candidates nonsensical; io/fs
		// rejects them anyway, so they simply miss.
		if b, err := embedded.ReadFile(filepath.ToSlash(c)); err == nil {
			return b, c, nil
		}
	}
	return nil, "", fmt.Errorf("could not find config file for %q (tried: %v)", spec, candidates)
}

// forceYAMLSuffix replaces the extension of the final path element with
// .yaml, matching pathlib's with_suffix.
func forceYAMLSuffix(spec string) string {
	dir, base := filepath.Split(spec)
	ext := filepath.Ext(base)
	if ext == ".yaml" {
		return spec
	}
	return dir + strings.TrimSuffix(base, ext) + ".yaml"
}

// KeyOverride interprets a "a.b.c=value" spec as a nested map. The value is
// JSON-decoded when possible ("42", "true", `["a","b"]`, `{"k":"v"}`), and
// kept as a raw string otherwise.
func KeyOverride(spec string) (map[string]any, error) {
	key, raw, ok := strings.Cut(spec, "=")
	if !ok {
		return nil, fmt.Errorf("invalid config spec %q: missing '='", spec)
	}
	var value any = raw
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		value = raw
	}
	keys := strings.Split(key, ".")
	for _, k := range keys {
		if k == "" {
			return nil, fmt.Errorf("invalid config spec %q: empty config key", spec)
		}
	}
	result := map[string]any{}
	current := result
	for _, k := range keys[:len(keys)-1] {
		next := map[string]any{}
		current[k] = next
		current = next
	}
	current[keys[len(keys)-1]] = value
	return result, nil
}

// FromSpec turns a single -c argument into a config map: key=value overrides
// and YAML files (on disk or embedded), which is what mini-swe-agent calls
// get_config_from_spec.
func FromSpec(spec string) (map[string]any, error) {
	if strings.Contains(spec, "=") {
		return KeyOverride(spec)
	}
	data, _, err := Resolve(spec)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing config %q: %w", spec, err)
	}
	return m, nil
}
