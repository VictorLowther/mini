package agent

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

// openerHooks implements CallOpener to record where the loop told it a call's
// output would go. OnSubmit ends the run so the scripted turn order stays short.
type openerHooks struct {
	mu     sync.Mutex
	opened []model.Action
	paths  []string
	exists []bool
}

func (h *openerHooks) BeforeExecute(context.Context, []model.Action, map[string]any) error {
	return nil
}
func (h *openerHooks) OnSubmit(context.Context, model.Message) error   { return nil }
func (h *openerHooks) NextHumanCommand(context.Context) (string, bool) { return "", false }
func (h *openerHooks) OnMessage(model.Message)                         {}
func (h *openerHooks) OnInterrupt(context.Context) error               { return nil }

func (h *openerHooks) OnCallOpened(action model.Action, path string) {
	// The promise is about this instant: the file must exist *while the command is
	// about to run*, because a front end that is told the path has nothing to read
	// if the path names something that appears later or is cleaned up before it is
	// ever looked at.
	_, err := os.Stat(path)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.opened = append(h.opened, action)
	h.paths = append(h.paths, path)
	h.exists = append(h.exists, err == nil)
}

func (h *openerHooks) seen() ([]model.Action, []string, []bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]model.Action(nil), h.opened...), append([]string(nil), h.paths...),
		append([]bool(nil), h.exists...)
}

// TestLoopNamesTheResultFileBeforeExecuting is the seam a live card needs: the only
// moment a foreground call's output path can be learned is before the command runs,
// and a loop that announces it while it has no file to point at hands the front end
// nothing to tail. (Without a Recorder the loop's file is scratch space that goes
// away when the run ends, which is why the assertion is about the instant of
// announcement.)
func TestLoopNamesTheResultFileBeforeExecuting(t *testing.T) {
	dir := t.TempDir()
	l := newLoop(t, &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("counting", "printf one\\n; sleep 0.4; printf two\\n"),
		done("counted"),
	}}, &env.Local{Timeout: 10 * time.Second, CWD: dir})
	h := &openerHooks{}
	l.Hooks = h

	if _, err := l.Run(context.Background(), "print two lines"); err != nil {
		t.Fatal(err)
	}
	actions, paths, existed := h.seen()
	if len(actions) != 1 || len(paths) != 1 {
		t.Fatalf("%d calls announced with %d paths, want one and one", len(actions), len(paths))
	}
	if actions[0].Command != "printf one\\n; sleep 0.4; printf two\\n" {
		t.Errorf("announced command = %q", actions[0].Command)
	}
	if paths[0] == "" {
		t.Fatal("the call was announced with no result path")
	}
	if !existed[0] {
		t.Errorf("the path announced at %q did not exist when it was handed over", paths[0])
	}
}
