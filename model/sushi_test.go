package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VictorLowther/mini/config"
	"github.com/VictorLowther/mini/env"
)

// resultFile writes output to a temp result file and returns the Result the
// loop would have built for it: OutputPath/OutputBytes from the file and the
// view from the one place that computes it. Mutators adjust the execution
// facts (returncode, cwd, exception) the way Execute leaves them.
func resultFile(t *testing.T, output string, mutators ...func(*env.Result)) env.Result {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.log")
	if err := os.WriteFile(path, []byte(output), 0o600); err != nil {
		t.Fatal(err)
	}
	res := env.Result{OutputPath: path, OutputBytes: int64(len(output))}
	view, offset, elided, err := ViewOf(path)
	if err != nil {
		t.Fatal(err)
	}
	res.View, res.ViewOffset, res.ElidedLines, res.Truncated = view, offset, elided, offset > 0
	for _, fn := range mutators {
		fn(&res)
	}
	return res
}

func templates(t *testing.T) (obs, errTmpl string) {
	t.Helper()
	m, err := config.FromSpec("mini")
	if err != nil {
		t.Fatal(err)
	}
	model := config.Sub(m, "model")
	return config.String(model, "observation_template", ""), config.String(model, "format_error_template", "")
}

func fixture(name string) []byte {
	b, err := os.ReadFile(filepath.Join("..", "testdata", "sushi", name))
	if err != nil {
		panic(err)
	}
	return b
}

// newServer replays the given fixture bodies in order, recording each request.
func newServer(t *testing.T, replies ...any) (*httptest.Server, *[]Request, *int32) {
	t.Helper()
	var n int32
	var requests []Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		requests = append(requests, req)
		i := int(atomic.AddInt32(&n, 1)) - 1
		if i >= len(replies) {
			i = len(replies) - 1
		}
		switch rep := replies[i].(type) {
		case int:
			w.WriteHeader(rep)
			w.Write([]byte(`{"error":{"message":"boom","type":"server_error"}}`))
		case errorBody:
			w.WriteHeader(rep.status)
			w.Write([]byte(rep.body))
		case string:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.Write(fixture(rep))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &requests, &n
}

// Request is the decoded chat-completions body.
type Request struct {
	Model             string         `json:"model"`
	Messages          []Message      `json:"messages"`
	Tools             []any          `json:"tools"`
	Stream            bool           `json:"stream"`
	StreamOptions     map[string]any `json:"stream_options"`
	MaxTokens         int            `json:"max_tokens"`
	Temperature       *float64       `json:"temperature"`
	ReasoningEffort   string         `json:"reasoning_effort"`
	ParallelToolCalls *bool          `json:"parallel_tool_calls"`
}

type errorBody struct {
	status int
	body   string
}

func newSushi(t *testing.T, url string) *Sushi {
	t.Helper()
	obs, errTmpl := templates(t)
	return &Sushi{
		Model:       "qwen3.6-35b",
		BaseURL:     url,
		ObsTemplate: obs,
		ErrTemplate: errTmpl,
		HTTPClient:  &http.Client{},
		backoff:     func(int) time.Duration { return time.Millisecond },
	}
}

func TestQueryHappyPath(t *testing.T) {
	srv, requests, _ := newServer(t, "toolcall.sse")
	s := newSushi(t, srv.URL)
	msg, err := s.Query(context.Background(), []Message{{"role": "user", "content": "hi"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg["role"] != "assistant" || msg["content"] != "Let me check the current directory." {
		t.Errorf("message = %#v", msg)
	}
	if msg["reasoning_content"] != "The user wants a listing. " {
		t.Errorf("reasoning dropped: %#v", msg["reasoning_content"])
	}
	acts := ActionsOf(msg)
	if len(acts) != 1 || acts[0].Command != "ls -la" || acts[0].ToolCallID != "call_abc" {
		t.Fatalf("actions = %#v", acts)
	}
	extra := Extra(msg)
	if extra["cost"] != 0.0 {
		t.Errorf("cost without pricing must be 0, got %v", extra["cost"])
	}
	resp, ok := extra["response"].(map[string]any)
	if !ok {
		t.Fatalf("extra.response missing: %#v", extra)
	}
	if resp["id"] != "chatcmpl-t1" || resp["model"] != "qwen3.6-35b" {
		t.Errorf("response meta = %#v", resp)
	}
	u := resp["usage"].(map[string]any)
	if u["total_tokens"].(float64) != 154 {
		t.Errorf("usage = %#v", u)
	}
	if stats, ok := extra["model_stats"]; !ok {
		t.Error("extra.model_stats missing for the UI")
	} else if stats.(map[string]any)["cached_tokens"] == nil {
	}
	if _, ok := resp["timings"]; !ok {
		t.Error("sushi's timings should survive into the trajectory")
	}

	// Request shape: the tool schema and streaming/usage flags.
	if len(*requests) != 1 {
		t.Fatalf("requests = %d", len(*requests))
	}
	req := (*requests)[0]
	if !req.Stream {
		t.Error("stream must be on so the GUI can show thinking")
	}
	if req.StreamOptions["include_usage"] != true {
		t.Error("stream_options.include_usage must be set")
	}
	tool := req.Tools[0].(map[string]any)
	if tool["function"].(map[string]any)["name"] != "bash" {
		t.Errorf("tools = %#v", req.Tools)
	}
	if req.MaxTokens != 0 || req.Temperature != nil || req.ReasoningEffort != "" || req.ParallelToolCalls != nil {
		t.Errorf("unset options must be omitted: %#v", req)
	}
	// extra must never reach the API.
	if _, ok := req.Messages[0]["extra"]; ok {
		t.Error("extra key leaked into the request")
	}
}

func TestQueryOptionsAreSent(t *testing.T) {
	srv, requests, _ := newServer(t, "toolcall.sse")
	s := newSushi(t, srv.URL)
	s.MaxTokens = 512
	s.ReasoningEffort = "high"
	s.Temperature = ptr(0.2)
	s.ParallelToolCalls = ptr(false)
	s.BaseURL = srv.URL
	if _, err := s.Query(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	req := (*requests)[0]
	if req.MaxTokens != 512 || req.ReasoningEffort != "high" || *req.Temperature != 0.2 || *req.ParallelToolCalls {
		t.Errorf("body = %#v", req)
	}
}

func TestReasoningContentRoundTrips(t *testing.T) {
	srv, requests, _ := newServer(t, "toolcall.sse", "toolcall.sse")
	s := newSushi(t, srv.URL)
	msg1, err := s.Query(context.Background(), []Message{{"role": "user", "content": "hi"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(context.Background(), []Message{{"role": "user", "content": "hi"}, msg1}, nil); err != nil {
		t.Fatal(err)
	}
	second := (*requests)[1].Messages
	if len(second) != 2 {
		t.Fatalf("history = %#v", second)
	}
	if second[1]["reasoning_content"] != "The user wants a listing. " {
		t.Errorf("sushi's chat template needs reasoning_content back in history, got %#v", second[1])
	}
	if second[1]["tool_calls"] == nil {
		t.Error("tool_calls must round-trip")
	}
}

func interruptOf(t *testing.T, err error) *Interrupt {
	t.Helper()
	var in *Interrupt
	if !errors.As(err, &in) {
		t.Fatalf("want *Interrupt, got %T: %v", err, err)
	}
	if in.Kind != KindFormatError {
		t.Fatalf("kind = %s", in.Kind)
	}
	if len(in.Messages) != 1 || in.Messages[0]["role"] != "user" {
		t.Fatalf("messages = %#v", in.Messages)
	}
	return in
}

func TestFormatErrors(t *testing.T) {
	obs, errTmpl := templates(t)
	_ = obs
	cases := []struct {
		fixture    string
		wantIn     string
		wantNotIn  string
		hasToolCfg bool
	}{
		{"bad_args.sse", "Error parsing tool call arguments:", "", true},
		{"unknown_tool.sse", "Unknown tool 'python'.", "Missing 'command'", true},
		{"missing_command.sse", "Missing 'command' argument in bash tool call.", "", true},
		{"length_limit.sse", "reached the output token limit (finish_reason=length)", "Tool call error:", false},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			srv, _, _ := newServer(t, c.fixture)
			s := newSushi(t, srv.URL)
			s.ErrTemplate = errTmpl
			_, err := s.Query(context.Background(), nil, nil)
			if err == nil {
				t.Fatal("expected a FormatError")
			}
			in := interruptOf(t, err)
			content, _ := in.Messages[0]["content"].(string)
			if !strings.Contains(content, c.wantIn) {
				t.Errorf("content = %q, want it to contain %q", content, c.wantIn)
			}
			if c.wantNotIn != "" && strings.Contains(content, c.wantNotIn) {
				t.Errorf("content = %q, must not contain %q", content, c.wantNotIn)
			}
			extra := Extra(in.Messages[0])
			if extra["interrupt_type"] != "FormatError" {
				t.Errorf("extra = %#v", extra)
			}
			// Rule 8: the billed call and the raw reply must survive parsing.
			if _, ok := extra["cost"]; !ok {
				t.Error("FormatError message must carry cost")
			}
			if _, ok := extra["response"]; !ok {
				t.Error("FormatError message must carry the raw response")
			}
			if _, err := json.Marshal(extra["response"]); err != nil {
				t.Errorf("response must be JSON-serializable: %v", err)
			}
		})
	}
}

// A plain-text turn that ends with finish_reason=stop is the submission
// itself, not an error: no actions to run, and the text comes back intact.
func TestPlainResponseIsTheSubmission(t *testing.T) {
	srv, _, _ := newServer(t, "no_tool_calls.sse")
	s := newSushi(t, srv.URL)
	msg, err := s.Query(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("a stop-terminated plain response must not error: %v", err)
	}
	if got, _ := msg["content"].(string); got != "I think we are done here." {
		t.Errorf("content = %q", got)
	}
	if acts := ActionsOf(msg); len(acts) != 0 {
		t.Errorf("actions = %v, want none", acts)
	}
}

// A resumed history carries exit rows - how earlier runs ended. They are
// transcript bookkeeping, not model context: role "exit" is not a valid
// chat-completions role, and the wire copy must drop it.
func TestRequestBodyDropsExitRows(t *testing.T) {
	srv, requests, _ := newServer(t, "toolcall.sse")
	s := newSushi(t, srv.URL)
	_, err := s.Query(context.Background(), []Message{
		{"role": "system", "content": "s"},
		{"role": "user", "content": "task"},
		{"role": "assistant", "content": "done"},
		{"role": "exit", "content": "done", "extra": map[string]any{"exit_status": "Submitted", "submission": "done"}},
		{"role": "user", "content": "The user added a new task: again"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs := (*requests)[0].Messages
	for _, m := range msgs {
		if m["role"] == "exit" {
			t.Fatalf("an exit row reached the wire: %v", msgs)
		}
	}
	if len(msgs) != 4 {
		t.Errorf("%d messages on the wire, want 4 (the four non-exit rows):\n%v", len(msgs), msgs)
	}
}

func TestExactErrorStrings(t *testing.T) {
	srv, _, _ := newServer(t, "no_finish_reason.sse")
	s := newSushi(t, srv.URL)
	s.ErrTemplate = "{{ .error }}"
	_, err := s.Query(context.Background(), nil, nil)
	in := interruptOf(t, err)
	want := "No tool calls found in the response. Every response must either include at least one tool call or be a final answer without one."
	if got := in.Messages[0]["content"]; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}

	srv2, _, _ := newServer(t, "unknown_tool.sse")
	s2 := newSushi(t, srv2.URL)
	s2.ErrTemplate = "{{ .error }}"
	_, err = s2.Query(context.Background(), nil, nil)
	if got := interruptOf(t, err).Messages[0]["content"]; got != "Unknown tool 'python'." {
		t.Errorf("content = %q", got)
	}
}

func TestKeepaliveComments(t *testing.T) {
	srv, _, _ := newServer(t, "keepalive.sse")
	s := newSushi(t, srv.URL)
	msg, err := s.Query(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if acts := ActionsOf(msg); len(acts) != 1 || acts[0].Command != "echo hi" {
		t.Errorf("actions = %#v", acts)
	}
}

func TestFragmentedToolCallArguments(t *testing.T) {
	srv, _, _ := newServer(t, "fragmented_toolcall.sse")
	s := newSushi(t, srv.URL)
	msg, err := s.Query(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	acts := ActionsOf(msg)
	if len(acts) != 1 || acts[0].Command != "echo frag" || acts[0].ToolCallID != "call_x" {
		t.Errorf("actions = %#v", acts)
	}
}

func TestParallelToolCalls(t *testing.T) {
	srv, _, _ := newServer(t, "parallel_toolcalls.sse")
	s := newSushi(t, srv.URL)
	msg, err := s.Query(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	acts := ActionsOf(msg)
	if len(acts) != 2 || acts[0].Command != "echo one" || acts[1].Command != "echo two" {
		t.Fatalf("actions = %#v", acts)
	}
	if acts[0].ToolCallID != "call_1" || acts[1].ToolCallID != "call_2" {
		t.Errorf("tool_call_ids = %#v", acts)
	}
}

func TestMidStreamErrorRetriesThenFails(t *testing.T) {
	srv, _, counter := newServer(t, "midstream_error.sse")
	s := newSushi(t, srv.URL)
	s.RetryMax = 3
	_, err := s.Query(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var api *APIError
	if !errors.As(err, &api) || !strings.Contains(api.Message, "model unloaded") {
		t.Fatalf("err = %v", err)
	}
	if got := atomic.LoadInt32(counter); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestRetryThenSucceed(t *testing.T) {
	srv, _, counter := newServer(t, 500, "toolcall.sse")
	s := newSushi(t, srv.URL)
	var waits []time.Duration
	s.backoff = func(attempt int) time.Duration {
		d := time.Duration(4<<attempt) * time.Second
		waits = append(waits, d)
		return 0
	}
	if _, err := s.Query(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(counter); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
	if len(waits) != 1 || waits[0] != 4*time.Second {
		t.Errorf("first backoff should be 4s, got %v", waits)
	}
}

func TestBackoffScheduleMatchesTenacity(t *testing.T) {
	s := &Sushi{}
	want := []time.Duration{4, 8, 16, 32, 60, 60, 60}
	for i, w := range want {
		if got := s.wait(i); got != w*time.Second {
			t.Errorf("wait(%d) = %v, want %v", i, got, w*time.Second)
		}
	}
}

func TestNoRetryOnAbortErrors(t *testing.T) {
	cases := map[string]errorBody{
		"context overflow": {400, `{"error":{"message":"This model's maximum context length is 4096 tokens. However, you requested 8192 tokens (12000 in the messages). Please reduce the length of the messages or the completion.","type":"invalid_request_error","param":null,"code":400}}`},
		"bad request":      {400, `{"error":{"message":"n>1 is not supported","type":"invalid_request_error"}}`},
		"auth":             {401, `{"error":{"message":"nope","type":"authentication_error"}}`},
		"not found":        {404, `{"error":{"message":"unknown model","type":"not_found_error"}}`},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _, counter := newServer(t, body)
			s := newSushi(t, srv.URL)
			_, err := s.Query(context.Background(), nil, nil)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := atomic.LoadInt32(counter); got != 1 {
				t.Errorf("attempts = %d, want 1 (must not retry)", got)
			}
			var api *APIError
			if !errors.As(err, &api) {
				t.Fatalf("err = %v", err)
			}
			if api.Retryable {
				t.Error("error should be marked non-retryable")
			}
		})
	}
}

func TestServerErrorsDoRetry(t *testing.T) {
	srv, _, counter := newServer(t, errorBody{503, `{"error":{"message":"model is loading","type":"server_error"}}`})
	s := newSushi(t, srv.URL)
	s.RetryMax = 4
	_, _ = s.Query(context.Background(), nil, nil)
	if got := atomic.LoadInt32(counter); got != 4 {
		t.Errorf("attempts = %d, want 4", got)
	}
}

func TestContextCancellationAbortsRetries(t *testing.T) {
	srv, _, _ := newServer(t, 500)
	s := newSushi(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Query(ctx, nil, nil); !errors.Is(err, context.Canceled) && err == nil {
		t.Fatalf("err = %v", err)
	}
}

func TestPricingCost(t *testing.T) {
	srv, _, _ := newServer(t, "toolcall.sse")
	s := newSushi(t, srv.URL)
	s.Pricing = &Pricing{InputPerMtok: 1.0, OutputPerMtok: 2.0}
	msg, err := s.Query(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// usage: 120 prompt (0 cached) + 34 completion => (120*1 + 34*2)/1e6
	if got := Extra(msg)["cost"]; got != (120+68)/1e6 {
		t.Errorf("cost = %v", got)
	}
}

func TestFormatObservations(t *testing.T) {
	obs, _ := templates(t)
	s := &Sushi{ObsTemplate: obs}
	msg := Message{"extra": map[string]any{"actions": []Action{
		{Command: "ls", ToolCallID: "c1"},
		{Command: "pwd", ToolCallID: "c2"},
	}}}
	results := []env.Result{resultFile(t, "file list\n", func(r *env.Result) { r.CWD = "/work" })}
	out := s.FormatObservations(msg, results, map[string]any{"task": "T"})
	if len(out) != 2 {
		t.Fatalf("got %d messages", len(out))
	}
	if out[0]["role"] != "tool" || out[0]["tool_call_id"] != "c1" {
		t.Errorf("msg0 = %#v", out[0])
	}
	first, _ := out[0]["content"].(string)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(first), &parsed); err != nil {
		t.Fatalf("observation is not valid JSON: %q (%v)", first, err)
	}
	if parsed["output"] != "file list\n" || parsed["returncode"].(float64) != 0 {
		t.Errorf("parsed = %#v", parsed)
	}
	// The second action never ran: padded observation.
	second, _ := out[1]["content"].(string)
	if !strings.Contains(second, `"returncode": -1`) {
		t.Errorf("padded observation = %q", second)
	}
	extra := Extra(out[0])
	// Tool text is never in the message any more: extra names the file that
	// holds it.
	if extra["raw_output"] != nil {
		t.Errorf("raw_output must not come back: %#v", extra)
	}
	if extra["exception_info"] != "" || extra["returncode"] != 0 {
		t.Errorf("extra = %#v", extra)
	}
	path, _ := extra["tcr_path"].(string)
	if b, err := os.ReadFile(path); err != nil || string(b) != "file list\n" {
		t.Errorf("tcr_path %q does not hold the output: %q, %v", path, b, err)
	}
	// The UI renders the transcript from these instead of parsing content back.
	if extra["command"] != "ls" {
		t.Errorf("extra.command = %v", extra["command"])
	}
	if extra["cwd"] != "/work" {
		t.Errorf("extra.cwd = %v", extra["cwd"])
	}
	if _, ok := extra["timestamp"].(float64); !ok {
		t.Errorf("timestamp must marshal like Python's time.time(): %#v", extra["timestamp"])
	}
}

func TestObservationElidesLongOutput(t *testing.T) {
	obs, _ := templates(t)
	s := &Sushi{ObsTemplate: obs}
	// 120 lines of 60 chars: the view is the last MaxViewLines lines, the
	// rest is elided and named by output_path.
	long := strings.Repeat(strings.Repeat("x", 60)+"\n", 120)
	msg := Message{"extra": map[string]any{"actions": []Action{{Command: "cat big", ToolCallID: "c1"}}}}
	out := s.FormatObservations(msg, []env.Result{resultFile(t, long)}, nil)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out[0]["content"].(string)), &parsed); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if parsed["elided_lines"].(float64) != 20 {
		t.Errorf("elided_lines = %v, want 20", parsed["elided_lines"])
	}
	if parsed["output_bytes"].(float64) != float64(len(long)) {
		t.Errorf("output_bytes = %v, want %d", parsed["output_bytes"], len(long))
	}
	if tail, _ := parsed["output_tail"].(string); tail != strings.Repeat(strings.Repeat("x", 60)+"\n", 100) {
		t.Errorf("output_tail is not the last 100 lines (len %d)", len(tail))
	}
	if parsed["warning"] == "" || parsed["output_path"] == "" {
		t.Errorf("parsed = %#v", parsed)
	}
	if _, ok := parsed["output"]; ok {
		t.Error("the full output must be replaced by the tail when eliding")
	}
}

func TestObservationCarriesResultExtra(t *testing.T) {
	obs, _ := templates(t)
	s := &Sushi{ObsTemplate: obs}
	msg := Message{"extra": map[string]any{"actions": []Action{{Command: "sleep 100", ToolCallID: "c1"}}}}
	res := resultFile(t, "partial", func(r *env.Result) {
		r.ReturnCode = -1
		r.ExceptionInfo = "timed out"
		r.Extra = map[string]any{"exception_type": "TimeoutExpired"}
	})
	out := s.FormatObservations(msg, []env.Result{res}, nil)
	extra := Extra(out[0])
	if extra["exception_type"] != "TimeoutExpired" {
		t.Errorf("result extra not merged into the message extra: %#v", extra)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out[0]["content"].(string)), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["exception_info"] != "timed out" {
		t.Errorf("parsed = %#v", parsed)
	}
}

func TestHumanActionGetsUserObservation(t *testing.T) {
	obs, _ := templates(t)
	s := &Sushi{ObsTemplate: obs}
	msg := Message{"extra": map[string]any{"actions": []Action{{Command: "ls"}}}}
	out := s.FormatObservations(msg, []env.Result{resultFile(t, "x\n")}, nil)
	if out[0]["role"] != "user" {
		t.Errorf("role = %v, want user for actions without a tool_call_id", out[0]["role"])
	}
	if _, ok := out[0]["tool_call_id"]; ok {
		t.Error("tool_call_id must be absent")
	}
}

func ptr[T any](v T) *T { return &v }

func TestStreamIgnoresForeignFrameLines(t *testing.T) {
	// "data:" with no payload and a short non-data line must be skipped, not
	// crash the reader (some proxies add event:/id: lines). The stream itself
	// is a plain-text stop turn: valid under the prompt contract, so it comes
	// back parsed rather than as an error.
	body := "data:\n\nid: 7\nevent: message\n: keepalive\n" +
		"data: " + `{"choices":[{"index":0,"delta":{"content":"ok"}}]}` + "\n\n" +
		"data: " + `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	s := newSushi(t, srv.URL)
	s.ErrTemplate = "{{.error}}"
	msg, err := s.Query(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got, _ := msg["content"].(string); got != "ok" {
		t.Errorf("content = %q, want ok", got)
	}
}
