package env

import (
	"context"
	"testing"
	"time"
)

func TestExitCodesFromExternalCommands(t *testing.T) {
	l := &Local{Timeout: 10 * time.Second}
	for _, c := range []struct {
		cmd  string
		want int
	}{
		{"cat /definitely/missing", 1},
		{"false", 1},
		{"grep -c zzzz /etc/services", 1},
		{"cat /definitely/missing | grep x", 1},
		{"ls /definitely/missing >/dev/null", 2},
		{"cat /definitely/missing >&2", 1},
	} {
		res, err := l.Execute(context.Background(), c.cmd, "")
		if err != nil {
			t.Fatal(err)
		}
		if res.ReturnCode != c.want {
			t.Errorf("%q rc = %d, want %d (out=%q)", c.cmd, res.ReturnCode, c.want, res.Output)
		}
	}
}
