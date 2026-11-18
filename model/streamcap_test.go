package model

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A-11: one call can stream at most 8x the byte-size of the context window.
// The shape that used to run the process out of room is a server that never
// answers - a proxy, the wrong port, a stalled model - because keepalive
// comments never become chunks, so nothing else bounded what we buffered.
func TestStreamIsCappedPerCallAndNotRetried(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		// Forever, until the client walks away.
		for i := 0; ; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			if _, err := fmt.Fprintf(w, ": keepalive %d %s\n\n", i, strings.Repeat("x", 512)); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer srv.Close()

	s := newSushi(t, srv.URL)
	s.RetryMax = 5 // must not matter: a refused ceiling is not retryable
	s.ContextWindow = 2
	if got := s.maxResponseBytes(); got != 2*responseBytesPerContextToken {
		t.Fatalf("ceiling = %d, want %d", got, 2*responseBytesPerContextToken)
	}

	_, err := s.Query(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("an endless keepalive stream was accepted")
	}
	var api *APIError
	if !errors.As(err, &api) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if api.Retryable {
		t.Error("the ceiling must not be retryable: retrying re-downloads the same flood")
	}
	if api.Type != "response_too_large" {
		t.Errorf("type = %q, want response_too_large", api.Type)
	}
	for _, want := range []string{"abandoned", "no completion chunk"} {
		if !strings.Contains(api.Message, want) {
			t.Errorf("message must say what happened (%q lacks %q)", api.Message, want)
		}
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("the server was asked %d times, want exactly 1", n)
	}
}

// The ceiling has to sit far enough above real traffic that it never fires on a
// legitimate completion.
func TestLegitimateStreamsPassTheCeiling(t *testing.T) {
	body := "data: " + `{"choices":[{"index":0,"delta":{"content":"hello"}}]}` + "\n\n" +
		"data: " + `{"choices":[{"index":0,"delta":{"reasoning_content":"thought"}}]}` + "\n\n" +
		": keepalive\n\n" +
		"data: " + `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	s := newSushi(t, srv.URL)
	s.ContextWindow = 4096 // 128 KiB ceiling for a ~150 byte reply
	msg, err := s.Query(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("a normal completion tripped the ceiling: %v", err)
	}
	if content, _ := msg["content"].(string); content != "hello" {
		t.Errorf("content = %q", content)
	}
}

// An unset window falls back to the default rather than to no ceiling, and the
// derivation is 8 whole contexts of bytes at 4 bytes per token.
func TestMaxResponseBytesDerivation(t *testing.T) {
	s := &Sushi{}
	if got, want := s.maxResponseBytes(), responseBytesPerContextToken*DefaultContextWindow; got != want {
		t.Errorf("unset window: %d, want %d (never unlimited)", got, want)
	}
	s.ContextWindow = -5
	if got, want := s.maxResponseBytes(), responseBytesPerContextToken*DefaultContextWindow; got != want {
		t.Errorf("negative window: %d, want %d", got, want)
	}
	s.ContextWindow = 262144
	if got, want := s.maxResponseBytes(), 8*4*262144; got != want {
		t.Errorf("default-sized window: %d, want %d", got, want)
	}
	if got := s.maxResponseBytes(); got != 8<<20 {
		t.Errorf("8x a 262144-token context should be 8 MiB, got %d", got)
	}
}
