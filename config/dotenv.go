package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadEnv parses a dotenv file of `KEY='value'` lines (the format python-dotenv's
// set_key writes) and returns it in file order. Missing files yield an empty map.
func LoadEnv(path string) (map[string]string, error) {
	values := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return values, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = unquote(strings.TrimSpace(raw))
	}
	return values, sc.Err()
}

func unquote(v string) string {
	if len(v) >= 2 {
		if q := v[0]; (q == '\'' || q == '"') && v[len(v)-1] == q {
			inner := v[1 : len(v)-1]
			if q == '\'' {
				return inner
			}
			return strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\"`, `"`, `\\`, `\`).Replace(inner)
		}
	}
	return v
}

// SetEnvValue writes KEY='value' to the dotenv file at path, replacing an
// existing entry in place and keeping all other lines, like dotenv.set_key.
func SetEnvValue(path, key, value string) error {
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	// python-dotenv writes KEY='value'; a value containing a quote is written
	// double-quoted instead so the file stays parseable.
	entry := fmt.Sprintf("%s='%s'", key, value)
	if strings.Contains(value, "'") {
		entry = fmt.Sprintf("%s=\"%s\"", key, strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`))
	}
	replaced := false
	for i, l := range lines {
		if k, _, ok := strings.Cut(strings.TrimSpace(l), "="); ok && strings.TrimSpace(k) == key {
			lines[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		lines = append(lines, entry)
	}
	return writeLines(path, lines)
}

// DeleteEnvValue removes a key from the dotenv file at path.
func DeleteEnvValue(path, key string) error {
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	kept := lines[:0]
	for _, l := range lines {
		if k, _, ok := strings.Cut(strings.TrimSpace(l), "="); ok && strings.TrimSpace(k) == key {
			continue
		}
		kept = append(kept, l)
	}
	return writeLines(path, kept)
}

func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	return lines, nil
}

func writeLines(path string, lines []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	return os.WriteFile(path, []byte(content), 0o600)
}
