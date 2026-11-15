package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// infoServer answers GET /v1/models with a canned body (a string is sent raw,
// anything else is marshalled) and counts the requests it was asked for, so a
// test can see both what the lookup read and that it asked exactly once.
func infoServer(t *testing.T, modelName string, reply any) (*Sushi, *httptest.Server, *paths) {
	t.Helper()
	return infoStatusServer(t, modelName, http.StatusOK, reply)
}

// infoStatusServer is infoServer with the status the model list comes back
// with, for the ways a server can decline to answer.
func infoStatusServer(t *testing.T, modelName string, status int, reply any) (*Sushi, *httptest.Server, *paths) {
	t.Helper()
	var mu sync.Mutex
	seen := &paths{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen.paths = append(seen.paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if raw, ok := reply.(string); ok {
			_, _ = w.Write([]byte(raw))
			return
		}
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	s := newSushi(t, srv.URL)
	s.Model = modelName
	return s, srv, seen
}

type paths struct{ paths []string }

func (p *paths) count(want string) int {
	n := 0
	for _, got := range p.paths {
		if got == want {
			n++
		}
	}
	return n
}

func list(entries ...map[string]any) map[string]any {
	return map[string]any{"object": "list", "data": entries}
}

// TestServerContextWindowFieldSpellings covers the field names in the wild.
// /v1/models standardizes none of them, so a server that says more than
// id/object/created/owned_by invents its own spelling.
func TestServerContextWindowFieldSpellings(t *testing.T) {
	for _, tc := range []struct {
		name string
		body any
		want int
	}{
		{"sushi", list(map[string]any{"id": "m", "context_length": 1048576,
			"max_model_len": 1048576, "meta": map[string]any{"context_length": 1048576}}), 1048576},
		{"vllm max_model_len", list(map[string]any{"id": "m", "max_model_len": 32768}), 32768},
		{"openrouter context_length", list(map[string]any{"id": "m", "context_length": 131072}), 131072},
		{"context_window", list(map[string]any{"id": "m", "context_window": 65536}), 65536},
		{"llama.cpp meta n_ctx", list(map[string]any{"id": "m", "meta": map[string]any{"n_ctx": 4096}}), 4096},
		{"openai limit context", list(map[string]any{"id": "m", "limit": map[string]any{"context": 128000}}), 128000},
		{"bare array", []map[string]any{{"id": "m", "context_length": 8192}}, 8192},
		// The extent the server runs wins over the checkpoint's native one.
		{"top level before meta", list(map[string]any{"id": "m", "context_length": 1048576,
			"meta": map[string]any{"n_ctx": 262144}}), 1048576},
		// Unusable numbers are absent, not the compaction trigger.
		{"standard openai entry says nothing", list(map[string]any{
			"id": "m", "object": "model", "created": 1, "owned_by": "openai"}), 0},
		{"zero", list(map[string]any{"id": "m", "context_length": 0}), 0},
		{"negative", list(map[string]any{"id": "m", "context_length": -1}), 0},
		{"absurd", list(map[string]any{"id": "m", "context_length": 1e12}), 0},
		{"stringly typed", list(map[string]any{"id": "m", "context_length": "262144"}), 262144},
		{"non-numeric size", list(map[string]any{"id": "m", "context_length": "unknown"}), 0},
		{"models envelope", map[string]any{"models": []any{map[string]any{"id": "m", "context_length": 65536}}}, 65536},
		{"empty list", list(), 0},
		{"no ids", list(map[string]any{"context_length": 4096}), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := infoServer(t, "m", tc.body)
			got, err := s.ServerContextWindow(context.Background())
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Errorf("window = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestServerContextWindowNameMatching: the config holds the name the user
// typed, the server answers with ids of its own, and the one case that matters
// most - a local server aliasing a long checkpoint id - must not fall through.
func TestServerContextWindowNameMatching(t *testing.T) {
	const sushiAlias = `{"object":"list","data":[
	  {"id":"beamster/Qwen3.8-Flash-Next-Sushi-4bpw","object":"model","owned_by":"sushi",
	   "loaded":true,"state":"ready","context_length":1048576,
	   "meta":{"context_length":1048576,"architecture":"qwen4_exp"}},
	  {"id":"beamster/DeepSeek-V4.1-Flash-1017-Sushi-STQ-1.75bpw","object":"model","owned_by":"sushi",
	   "loaded":false,"state":"unloaded","context_length":524288}]}`

	for _, tc := range []struct {
		name, model, body string
		want              int
	}{
		{"exact id", "m", `{"data":[{"id":"m","context_length":4096}]}`, 4096},
		{"typed alias prefixes the checkpoint id", "beamster/qwen", sushiAlias, 1048576},
		{"bare segment alias", "qwen", sushiAlias, 1048576},
		{"case and provider folded", "BEAMSTER/Qwen3.8-Flash-Next-Sushi-4bpw", sushiAlias, 1048576},
		{"exact id beats residency", "beamster/DeepSeek-V4.1-Flash-1017-Sushi-STQ-1.75bpw", sushiAlias, 524288},
		{"unknown name: the one resident model", "some/alias", sushiAlias, 1048576},
		// Refusing to guess is the point: the caller falls back to config.
		{"unknown and none resident", "some/alias",
			`{"data":[{"id":"a/x","context_length":111},{"id":"b/y","context_length":222}]}`, 0},
		{"unknown and both resident", "some/alias",
			`{"data":[{"id":"a/x","loaded":true,"context_length":111},{"id":"b/y","loaded":true,"context_length":222}]}`, 0},
		{"a nameless model does not steal another's window", "gpt-4o",
			`{"data":[{"id":"gpt-4o","object":"model"},{"id":"gpt-4o-mini","context_length":128000}]}`, 0},
		{"substring ties stay ambiguous", "qwen",
			`{"data":[{"id":"qwen-7b","context_length":32768},{"id":"qwen-72b","context_length":131072}]}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := infoServer(t, tc.model, tc.body)
			got, err := s.ServerContextWindow(context.Background())
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Errorf("window = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestServerContextWindowFailures: every way a server can decline is an error
// to the caller - and asked exactly once, because the model list is an
// optional startup question while the backoff schedule belongs to the calls a
// run cannot do without.
func TestServerContextWindowFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   any
	}{
		{"404", http.StatusNotFound, `{"error":{"message":"nope","type":"invalid_request_error"}}`},
		{"401", http.StatusUnauthorized, `{"error":{"message":"keys required"}}`},
		{"500", http.StatusInternalServerError, `{"error":{"message":"boom","type":"server_error"}}`},
		{"a proxy's html", http.StatusOK, "<html><body>502</body></html>"},
		{"truncated json", http.StatusOK, `{"object":"list","data":[{"id":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, seen := infoStatusServer(t, "m", tc.status, tc.body)
			got, err := s.ServerContextWindow(context.Background())
			if err == nil {
				t.Fatalf("window = %d, want an error", got)
			}
			if n := seen.count("GET /v1/models"); n != 1 {
				t.Errorf("%d requests to /v1/models, want 1", n)
			}
		})
	}
}

// TestServerContextWindowEmptyListIsSilence: a server that answers with no
// models is not a failed question, and a run must not care which happened.
func TestServerContextWindowEmptyListIsSilence(t *testing.T) {
	for _, body := range []any{list(), `{"object":"list"}`, []map[string]any{}} {
		s, _, _ := infoServer(t, "m", body)
		got, err := s.ServerContextWindow(context.Background())
		if err != nil || got != 0 {
			t.Errorf("window = %d, err = %v, want 0 and no error", got, err)
		}
	}
}

func TestServerContextWindowAsksExactlyOnce(t *testing.T) {
	s, _, seen := infoServer(t, "m", list(map[string]any{"id": "m", "context_length": 4096}))
	got, err := s.ServerContextWindow(context.Background())
	if err != nil || got != 4096 {
		t.Fatalf("window = %d, err = %v", got, err)
	}
	if n := seen.count("GET /v1/models"); n != 1 {
		t.Errorf("%d requests to /v1/models, want 1", n)
	}
	if n := seen.count("POST /v1/chat/completions"); n != 0 {
		t.Errorf("the lookup reached the completion endpoint %d times", n)
	}
}

func TestServerContextWindowHonoursTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release // never answered while the lookup is still waiting
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	s := newSushi(t, srv.URL)
	s.InfoTimeout = 20 * time.Millisecond
	started := time.Now()
	if _, err := s.ServerContextWindow(context.Background()); err == nil {
		t.Fatal("a server that never answers must not read as silence")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("the lookup waited %s; InfoTimeout is %s", elapsed, s.InfoTimeout)
	}
}

// TestServerContextWindowWithoutAModelName: guessing at an unnamed model is
// worse than saying nothing, however short the list is.
func TestServerContextWindowWithoutAModelName(t *testing.T) {
	s, _, _ := infoServer(t, "  ", list(map[string]any{"id": "m", "context_length": 4096}))
	got, err := s.ServerContextWindow(context.Background())
	if err != nil || got != 0 {
		t.Errorf("window = %d, err = %v; want silence", got, err)
	}
}

// TestServerContextWindowRespectsTheCaller: a caller whose context is already
// done gets an error, not a hung startup.
func TestServerContextWindowRespectsTheCaller(t *testing.T) {
	s, _, _ := infoServer(t, "m", list(map[string]any{"id": "m", "context_length": 4096}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := s.ServerContextWindow(ctx); err == nil || got != 0 {
		t.Errorf("window = %d, err = %v; want an error from the cancelled context", got, err)
	}
}

// TestDiscoverContextWindow is the whole chain: the server first, then the
// local config, then the built-in default.
func TestDiscoverContextWindow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       any
		configured int
		want       int
		wantSource string
	}{
		{"server beats config", list(map[string]any{"id": "qwen3.6-35b", "context_length": 1048576}), 262144, 1048576, ContextFromServer},
		{"server beats a missing config", list(map[string]any{"id": "qwen3.6-35b", "context_length": 1048576}), 0, 1048576, ContextFromServer},
		{"silent server falls back to config", list(map[string]any{"id": "qwen3.6-35b"}), 262144, 262144, ContextFromConfig},
		{"silent server and no config take the default", list(map[string]any{"id": "qwen3.6-35b"}), 0, DefaultContextWindow, ContextFromDefault},
		{"unparsable server falls back to config", "{not json", 32768, 32768, ContextFromConfig},
		// One model in the list, under an id of the server's own choosing
		// (llama.cpp and friends answer "default"): it is the model the
		// request would hit, so its window is the server's answer.
		{"a lone model under another id", list(map[string]any{"id": "other", "context_length": 999}), 0, 999, ContextFromServer},
		{"nothing to read and none configured", list(map[string]any{"id": "other"}), 0, DefaultContextWindow, ContextFromDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := infoServer(t, "qwen3.6-35b", tc.body)
			got, source := s.DiscoverContextWindow(context.Background(), tc.configured)
			if got != tc.want {
				t.Errorf("window = %d, want %d", got, tc.want)
			}
			if source != tc.wantSource {
				t.Errorf("source = %q, want %q", source, tc.wantSource)
			}
			// The client keeps the answer for the front ends and the
			// trajectory, and never lets a zero out of the chain.
			if s.ContextWindow != tc.want || s.ContextWindowSource != tc.wantSource {
				t.Errorf("client recorded %d/%q, want %d/%q",
					s.ContextWindow, s.ContextWindowSource, tc.want, tc.wantSource)
			}
			if note := s.ContextWindowNote(); !strings.Contains(note, fmt.Sprint(tc.want)) ||
				!strings.Contains(note, tc.wantSource) {
				t.Errorf("note = %q", note)
			}
			dumped := s.Serialize()["info"].(map[string]any)["config"].(map[string]any)["model"].(map[string]any)
			if dumped["context_window"] != tc.want || dumped["context_window_source"] != tc.wantSource {
				t.Errorf("trajectory model section = %v", dumped)
			}
		})
	}
}

// TestDiscoverContextWindowSurvivesADownServer: the startup question is
// optional, so a run against a server that is not listening still starts with
// the configured window.
func TestDiscoverContextWindowSurvivesADownServer(t *testing.T) {
	s := newSushi(t, "http://127.0.0.1:1")
	s.InfoTimeout = 100 * time.Millisecond
	got, source := s.DiscoverContextWindow(context.Background(), 42000)
	if got != 42000 || source != ContextFromConfig {
		t.Errorf("window = %d/%q, want 42000/config", got, source)
	}
}

func TestContextWindowNoteBeforeDiscovery(t *testing.T) {
	s := newSushi(t, "http://127.0.0.1:1")
	if got := s.ContextWindowNote(); got != "window unknown" {
		t.Errorf("note = %q, want the pre-discovery wording", got)
	}
	if got := s.configDump()["context_window"]; got != nil {
		t.Errorf("the trajectory invented a window: %v", got)
	}
}
