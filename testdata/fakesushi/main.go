// Command fakesushi replays recorded SSE bodies so the agent can be exercised
// end to end without a model. It is under testdata/, so `go build ./...` never
// sees it.
//
//	go run ./testdata/fakesushi :8009
//
// It answers GET /health and /v1/models like sushi - including the
// context_length fields mini reads at startup to decide its compaction trigger
// - and serves one recorded stream per POST /v1/chat/completions in sequence,
// looping back to the last body once the script runs out (so an unexpected
// extra turn is visible in the trajectory rather than hanging).
//
// Flags:
//
//	-delay 250ms   pause between SSE frames, to prove the client tolerates stalls
//	-scripts a.sse,b.sse   override the script (default: every testdata/sushi/*.sse in name order)
//	-context-length 131072   what /v1/models reports; 0 leaves the field out,
//	                          which is a plain OpenAI list and makes mini fall
//	                          back to its config for the window
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func main() {
	addr := flag.String("addr", "", "listen address (positional arg also accepted)")
	scripts := flag.String("scripts", "", "comma-separated .sse files")
	delay := flag.Duration("delay", 0, "pause between frames")
	keepalive := flag.Bool("keepalive", false, "send a ': keepalive' comment before the first frame")
	contextLength := flag.Int("context-length", 131072, "what /v1/models reports as context_length; 0 omits the field")
	flag.Parse()

	listen := *addr
	if listen == "" {
		listen = flag.Arg(0)
	}
	if listen == "" {
		listen = ":8009"
	}

	files := splitCSV(*scripts)
	if len(files) == 0 {
		// Default: the recorded turns in the order the plan's fixture list
		// names them: a listing, then the submission.
		dir := "testdata/sushi"
		if _, err := os.Stat(dir); err != nil {
			dir = "../../testdata/sushi"
		}
		for _, name := range []string{"toolcall.sse", "submit.sse"} {
			if p := filepath.Join(dir, name); fileExists(p) {
				files = append(files, p)
			}
		}
	}
	var bodies [][]byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			log.Fatalf("fakesushi: %v", err)
		}
		bodies = append(bodies, b)
	}
	if len(bodies) == 0 {
		log.Fatal("fakesushi: no scripts to replay")
	}

	var mu sync.Mutex
	next := 0
	seen := []string{}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []any{
			// Several models, only one resident: enough to prove a client
			// picks the right entry instead of the first one. Both ids in
			// use are here - "fakesushi" and the "test-model" the README's
			// invocation passes to -m.
			modelEntry("fakesushi", true, *contextLength),
			modelEntry("test-model", true, *contextLength),
			modelEntry("fakesushi-unloaded", false, *contextLength),
		}})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":{"message":"bad json","type":"invalid_request_error"}}`, http.StatusBadRequest)
			return
		}
		if n, ok := body["n"].(float64); ok && n > 1 {
			http.Error(w, `{"error":{"message":"n>1 is not supported","type":"invalid_request_error","code":400}}`, http.StatusBadRequest)
			return
		}
		mu.Lock()
		i := next
		if i >= len(bodies) {
			i = len(bodies) - 1
		}
		next++
		seen = append(seen, fmt.Sprintf("%d messages, stream=%v", countMessages(body["messages"]), body["stream"]))
		script := bodies[i]
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if *keepalive {
			_, _ = w.Write([]byte(": keepalive\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		for _, frame := range splitFrames(string(script)) {
			if _, err := w.Write([]byte(frame)); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			if *delay > 0 {
				select {
				case <-time.After(*delay):
				case <-r.Context().Done():
					return
				}
			}
		}
	})
	mux.HandleFunc("/debug/requests", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = fmt.Fprintf(w, "%d requests\n%s\n", len(seen), strings.Join(seen, "\n"))
	})

	log.Printf("fakesushi listening on %s (replaying %d scripts, delay %s)", listen, len(bodies), *delay)
	if err := http.ListenAndServe(listen, mux); err != nil {
		log.Fatal(err)
	}
}

func countMessages(v any) int {
	if list, ok := v.([]any); ok {
		return len(list)
	}
	return 0
}

// splitFrames keeps each `data:` line with its blank terminator, so the pause
// between frames is visible to a watching client.
func splitFrames(s string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(s, "\n") {
		cur.WriteString(line)
		cur.WriteString("\n")
		if line == "" {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// modelEntry is one /v1/models item in sushi's shape: the window both at the
// top level and inside the meta block, plus the residency flags a client needs
// to tell several served models apart. A zero contextLength omits the fields,
// which is what a plain OpenAI-compatible server sends.
func modelEntry(id string, loaded bool, contextLength int) map[string]any {
	state := "unloaded"
	if loaded {
		state = "ready"
	}
	entry := map[string]any{
		"id":       id,
		"object":   "model",
		"owned_by": "testdata",
		"loaded":   loaded,
		"state":    state,
	}
	if contextLength > 0 {
		entry["context_length"] = contextLength
		entry["max_model_len"] = contextLength
		entry["meta"] = map[string]any{"context_length": contextLength}
	}
	return entry
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
