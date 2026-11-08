package env

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Uname is platform.uname()._asdict(): the keys system, node, release, version,
// machine and processor. The instance template branches on {{system}}, so
// "Darwin" must read exactly that, not Go's "darwin".
//
// One `uname` call per flag, resolved lazily and cached: a single multi-flag
// call cannot be parsed back reliably because -v ("Darwin Kernel Version 27.0.0:
// …") contains spaces and uname space-joins its flags onto one line.
func Uname() map[string]string {
	unameOnce.Do(func() { unameCache = fetchUname() })
	return unameCache
}

var (
	unameOnce  sync.Once
	unameCache map[string]string
)

// unameFields maps the platform.uname() attribute to the uname(1) flag that
// reports it.
var unameFields = []struct{ key, flag string }{
	{"system", "-s"},
	{"node", "-n"},
	{"release", "-r"},
	{"version", "-v"},
	{"machine", "-m"},
	{"processor", "-p"},
}

func fetchUname() map[string]string {
	vars := make(map[string]string, len(unameFields))
	for _, f := range unameFields {
		vars[f.key] = trimBlank(unameFlag(f.flag))
	}
	// No uname binary (Windows): best effort from the Go runtime, matching the
	// capitalised sysname Python reports.
	if vars["system"] == "" {
		vars["system"] = titleGo(runtime.GOOS)
		vars["machine"] = runtime.GOARCH
		vars["node"], _ = os.Hostname()
	}
	// CPython treats "unknown" as blank and falls back to the machine name.
	if vars["processor"] == "" || vars["processor"] == "unknown" {
		vars["processor"] = vars["machine"]
	}
	return vars
}

func unameFlag(flag string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "uname", flag).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func trimBlank(s string) string { return strings.TrimSpace(s) }

func titleGo(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
