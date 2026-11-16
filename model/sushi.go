package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/VictorLowther/mini/config"
	"github.com/VictorLowther/mini/env"
)

// DefaultBaseURL is where sushi listens on this machine (the user's sushi runs
// on 1234, not the 12345 default).
const DefaultBaseURL = "http://127.0.0.1:1234"

// Pricing turns sushi's token counts into the `cost` figure the agent's cost
// limit compares against. sushi itself never reports money.
type Pricing struct {
	InputPerMtok  float64
	OutputPerMtok float64
}

// Sushi is the OpenAI-compatible chat-completions client for a local sushi
// server. Loop owns one instance.
type Sushi struct {
	Model             string
	BaseURL           string
	MaxTokens         int
	Temperature       *float64
	ReasoningEffort   string
	ParallelToolCalls *bool
	Pricing           *Pricing
	// RetryMax overrides MINI_MODEL_RETRY_STOP_AFTER_ATTEMPT (default 10).
	RetryMax int
	// OnRetry, when set, is told about each retry so a UI can show progress
	// instead of appearing to hang.
	OnRetry func(attempt, maxAttempts int, err error, wait time.Duration)
	// Gate bounds this client's in-flight requests. Nil means DefaultGate, and
	// that is the point: the bound is opt-out-free, so a subagent that builds
	// its own Sushi cannot quietly step around the box's one real limit.
	Gate *Gate
	// OnGateWait, when set, is told about a request that had to queue for a
	// slot, so a front end can say so. A stalled window with no explanation is
	// indistinguishable from a crash, which is the same reason OnRetry exists and
	// reaches the transcript (cmd/mini/main.go:346-348).
	OnGateWait func(GateWait)
	// Tools is this session's advertised set. Zero means TopLevelTools, so a
	// client that never thought about kinds behaves as it always has; host sets
	// it from the session's kind, which makes one value drive the request array,
	// the parser's acceptance, and the prompt listing (§5.6).
	Tools       ToolSet
	HTTPClient  *http.Client
	ObsTemplate string
	ErrTemplate string

	// InfoTimeout bounds the startup GET /v1/models (default 3s). The client
	// itself has no Timeout because streams stall legitimately; a model-list
	// read is a question with a known-good default answer, so it can.
	InfoTimeout time.Duration
	// ContextWindow is the window the session runs against, resolved once at
	// startup by DiscoverContextWindow; ContextWindowSource names which of the
	// three it came from (ContextFromServer/Config/Default). Zero until
	// discovery has run - the loop reads its own copy off agent.Config.
	ContextWindow       int
	ContextWindowSource string

	// backoff converts a zero-based attempt index into a wait. Exponential
	// with min 4s and cap 60s by default; replaceable so tests stay fast.
	backoff func(attempt int) time.Duration
}

// NewSushi builds a model client from a `model:` config section.
func NewSushi(cfg map[string]any) *Sushi {
	base := config.FirstEnv("MINI_BASE_URL")
	if base == "" {
		base = DefaultBaseURL
	}
	s := &Sushi{
		Model:           config.String(cfg, "model_name", config.FirstEnv("MINI_MODEL_NAME")),
		BaseURL:         config.String(cfg, "base_url", base),
		MaxTokens:       config.Int(cfg, "max_tokens", 0),
		ReasoningEffort: config.String(cfg, "reasoning_effort", config.FirstEnv("MINI_THINKING")),
		RetryMax:        config.Int(cfg, "retry_stop_after_attempt", 0),
		ObsTemplate:     config.String(cfg, "observation_template", ""),
		ErrTemplate:     config.String(cfg, "format_error_template", "{{.error}}"),
	}
	if _, ok := cfg["temperature"]; ok {
		f := config.Float(cfg, "temperature", 0)
		s.Temperature = &f
	}
	if p := config.Sub(cfg, "pricing"); p != nil {
		s.Pricing = &Pricing{
			InputPerMtok:  config.Float(p, "input_per_mtok", 0),
			OutputPerMtok: config.Float(p, "output_per_mtok", 0),
		}
	}
	if _, ok := cfg["parallel_tool_calls"]; ok {
		b := config.Bool(cfg, "parallel_tool_calls", false)
		s.ParallelToolCalls = &b
	}
	s.BaseURL = strings.TrimRight(s.BaseURL, "/")
	return s
}

func (s *Sushi) retryMax() int {
	if s.RetryMax > 0 {
		return s.RetryMax
	}
	if v, err := strconv.Atoi(os.Getenv("MINI_MODEL_RETRY_STOP_AFTER_ATTEMPT")); err == nil && v > 0 {
		return v
	}
	return 10
}

func (s *Sushi) wait(attempt int) time.Duration {
	if s.backoff != nil {
		return s.backoff(attempt)
	}
	// tenacity's wait_exponential(multiplier=1, min=4, max=60): 4, 8, 16, 32,
	// 60, 60…
	d := 4 * time.Second << min(attempt, 4)
	return min(d, 60*time.Second)
}

// APIError is a failure reported by (or while talking to) the server.
type APIError struct {
	Status    int
	Type      string
	Code      string
	Message   string
	Retryable bool
}

func (e *APIError) Error() string {
	if e.Status == 0 {
		return e.Message
	}
	return fmt.Sprintf("http %d: %s (%s)", e.Status, e.Message, e.Type)
}

// abortOn mirrors litellm's abort_exceptions: these are configuration or
// capacity mistakes that retrying cannot fix.
func abortOn(status int, errType string) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	case http.StatusBadRequest:
		return errType == "invalid_request_error"
	}
	return false
}

// Query runs one model call, streaming, and returns the assistant message
// with its parsed actions in extra. Control-flow conditions come back as
// *Interrupt; transient failures are retried (rule 9) and then returned.
func (s *Sushi) Query(ctx context.Context, msgs []Message, h *Handler) (Message, error) {
	payload, err := s.payloadFor(s.requestBody(msgs))
	if err != nil {
		return nil, err
	}
	stream, elapsed, err := s.retryStream(ctx, payload, h, s.retryMax())
	if err != nil {
		return nil, err
	}
	cost := s.cost(stream.usage)
	GLOBAL_STATS.Add(cost, stream.totalTokens())
	msg := stream.assistant()
	extra := Extra(msg)
	extra["response"] = stream.response
	extra["cost"] = cost
	extra["timestamp"] = float64(time.Now().UnixMilli()) / 1000
	// Generation time for the successful attempt: the UIs' per-message header
	// (retry waits are deliberately excluded - they are announced separately).
	extra["elapsed_seconds"] = elapsed.Seconds()
	if u, ok := stream.response["usage"]; ok {
		extra["model_stats"] = u
	}
	actions, err := parseToolCallActions(stream.toolCalls, s.ErrTemplate, s.tools(), map[string]any{
		"finish_reason": stream.finishReason,
	})
	if err != nil {
		// Rule 8: the FormatError message must carry the cost and the raw
		// response, because the call was billed before parsing failed.
		if ie, ok := err.(*Interrupt); ok && len(ie.Messages) > 0 {
			e := Extra(ie.Messages[0])
			e["cost"] = cost
			e["response"] = stream.response
		}
		return nil, err
	}
	extra["actions"] = actions
	return msg, nil
}

// Summarize is the compaction side-query: the SAME projected context the
// next real turn will send, plus one directive user turn, so the request's
// entire prefix hits the server's warmed prompt cache - at mini's scale that
// is the difference between a compaction costing a full-session prefill and
// costing the directive's few dozen tokens. The reply must be prose: a tool
// call or an output-limited cut is a failure, never a half-summary.
//
// The retry budget is deliberately smaller than Query's: a summarize call
// that keeps failing is compaction's problem to report, not the run's to
// stall on.
func (s *Sushi) Summarize(ctx context.Context, msgs []Message, directive string, maxTokens int) (string, error) {
	all := make([]Message, 0, len(msgs)+1)
	all = append(all, msgs...)
	all = append(all, Message{"role": "user", "content": directive})
	body := s.requestBody(all)
	if maxTokens > 0 {
		body["max_tokens"] = maxTokens
	}
	payload, err := s.payloadFor(body)
	if err != nil {
		return "", err
	}
	stream, _, err := s.retryStream(ctx, payload, nil, min(s.retryMax(), 3))
	if err != nil {
		return "", err
	}
	if len(stream.toolCalls) > 0 {
		return "", &APIError{Message: "the summary request answered with a tool call instead of prose", Retryable: false}
	}
	if stream.finishReason == "length" {
		// A cut-off summary is a trap: it reads complete and is not. qwen-code
		// learned this the hard way; mini drops the whole attempt instead.
		return "", &APIError{Message: "the summary hit its output token limit before finishing", Retryable: false}
	}
	text := strings.TrimSpace(stream.content.String())
	if text == "" {
		return "", &APIError{Message: "the summary request returned no text", Retryable: false}
	}
	return text, nil
}

func (s *Sushi) payloadFor(body map[string]any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}
	return payload, nil
}

// retryStream is the one POST-and-accumulate loop behind both Query and
// Summarize: transient failures back off and retry up to maxAttempts,
// litellm-style aborts and the caller's context cancellation end it, and a
// nil Handler means "stream but show nobody". The duration returned is the
// successful attempt's own generation time, retry waits excluded.
func (s *Sushi) retryStream(ctx context.Context, payload []byte, h *Handler, maxAttempts int) (*stream, time.Duration, error) {
	for attempt := 0; ; attempt++ {
		started := time.Now()
		stream, err := s.attempt(ctx, payload, h)
		var interrupt *Interrupt
		if errors.As(err, &interrupt) || err == nil {
			return stream, time.Since(started), err
		}
		if ctx.Err() != nil || attempt+1 >= maxAttempts {
			return nil, 0, err
		}
		var api *APIError
		if errors.As(err, &api) && !api.Retryable {
			return nil, 0, err // litellm's abort_exceptions: retrying cannot fix this
		}
		wait := s.wait(attempt)
		slog.Warn("model query failed, retrying", "attempt", attempt+1, "max_attempts", maxAttempts, "err", err, "wait", wait.String())
		if s.OnRetry != nil {
			s.OnRetry(attempt+1, maxAttempts, err, wait)
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			return nil, 0, ctx.Err()
		}
	}
}

func (s *Sushi) requestBody(msgs []Message) map[string]any {
	prepared := make([]Message, 0, len(msgs))
	for _, m := range ProjectContext(msgs) {
		// ProjectContext has already dropped exit rows (transcript
		// bookkeeping, no server accepts role "exit") and turned any
		// compaction rows into their synthesized summaries.
		c := make(Message, len(m))
		for k, v := range m {
			if k != "extra" {
				c[k] = v
			}
		}
		prepared = append(prepared, c)
	}
	body := map[string]any{
		"model":          s.Model,
		"messages":       prepared,
		"tools":          s.tools().schemas(),
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if s.MaxTokens > 0 {
		body["max_tokens"] = s.MaxTokens
	}
	if s.Temperature != nil {
		body["temperature"] = *s.Temperature
	}
	if s.ReasoningEffort != "" {
		body["reasoning_effort"] = s.ReasoningEffort
	}
	if s.ParallelToolCalls != nil {
		body["parallel_tool_calls"] = *s.ParallelToolCalls
	}
	return body
}

// tools is the advertised set, defaulting to the top-level one.
func (s *Sushi) tools() ToolSet {
	if len(s.Tools) > 0 {
		return s.Tools
	}
	return TopLevelTools()
}

// gate is the bound this client obeys. Defaulting to the package-level Gate is
// what makes the limit unskippable rather than a courtesy.
func (s *Sushi) gate() *Gate {
	if s.Gate != nil {
		return s.Gate
	}
	return DefaultGate
}

// attempt makes one HTTP request and reads its whole streamed response before
// returning, which is why the slot is taken here and nowhere else: this function
// is the request *and* the response, so holding across it is exactly the
// "entire request/response, streaming included" the knob promises
// (docs/parallel-sessions.md §5.7). Holding one level up, in retryStream, would
// instead hold a slot through the exponential backoff - minutes of sleeping that
// have nothing to do with the GPU and everything to do with starving the
// sessions queued behind this one.
func (s *Sushi) attempt(ctx context.Context, payload []byte, h *Handler) (*stream, error) {
	g := s.gate()
	asked := time.Now()
	release, err := g.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if waited := time.Since(asked); waited > time.Millisecond && s.OnGateWait != nil {
		busy, waiting, limit := g.Stats()
		s.OnGateWait(GateWait{Waited: waited, Busy: busy, Waiting: waiting, Limit: limit})
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	// Loopback needs no auth: sushi exempts it from --api-key.
	res, err := s.client().Do(req)
	if err != nil {
		return nil, &APIError{Message: fmt.Sprintf("could not reach %s: %v", s.BaseURL, err), Retryable: ctx.Err() == nil}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, readError(res)
	}
	return readStream(res.Body, h)
}

func (s *Sushi) client() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	// No Timeout: sushi can hold a stream silent for minutes while it queues
	// and prefills, so cancellation is context-only.
	return &http.Client{}
}

func readError(res *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	e := &APIError{Status: res.StatusCode, Message: strings.TrimSpace(string(b)), Retryable: !abortOn(res.StatusCode, "")}
	var doc map[string]any
	if json.Unmarshal(b, &doc) == nil {
		if errObj, ok := doc["error"]; ok && errObj != nil {
			e.Message, e.Type, e.Code = errorFields(errObj)
			e.Retryable = !abortOn(res.StatusCode, e.Type)
		}
	}
	return e
}

// errorFields pulls message/type/code out of an error object, whatever shape
// the server put the code in (sushi uses a string, OpenAI an integer).
func errorFields(v any) (message, errType, code string) {
	m, _ := v.(map[string]any)
	message, _ = m["message"].(string)
	errType, _ = m["type"].(string)
	switch c := m["code"].(type) {
	case string:
		code = c
	case float64:
		code = strconv.FormatInt(int64(c), 10)
	case nil:
	default:
		code = fmt.Sprint(c)
	}
	if message == "" {
		b, _ := json.Marshal(v)
		message = "model error: " + string(b)
	}
	return message, errType, code
}

func (s *Sushi) cost(usage map[string]any) float64 {
	if s.Pricing == nil || usage == nil {
		return 0
	}
	cached := 0.0
	if d, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		cached = num(d["cached_tokens"])
	}
	in := num(usage["prompt_tokens"]) - cached
	out := num(usage["completion_tokens"])
	return (in*s.Pricing.InputPerMtok + out*s.Pricing.OutputPerMtok) / 1e6
}

// num reads a JSON number. Decoded bodies only ever hold float64, but usage
// maps are also built in Go (and ints are what a caller would naturally
// write), and a missed case here silently drops the reported-token anchor
// that ContextTokens prefers over its own estimate - so accept them all.
func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

// FormatObservations renders each executed action into the tool message the
// model sees next.
func (s *Sushi) FormatObservations(msg Message, results []env.Result, tvars map[string]any) []Message {
	return formatObservations(ActionsOf(msg), results, s.ObsTemplate, tvars)
}

// TemplateVars contributes the model section to the prompt context.
func (s *Sushi) TemplateVars() map[string]any { return s.configDump() }

func (s *Sushi) configDump() map[string]any {
	m := map[string]any{
		"model_name":            s.Model,
		"base_url":              s.BaseURL,
		"max_tokens":            s.MaxTokens,
		"reasoning_effort":      s.ReasoningEffort,
		"observation_template":  s.ObsTemplate,
		"format_error_template": s.ErrTemplate,
		"model_kwargs":          map[string]any{},
	}
	if s.Temperature != nil {
		m["temperature"] = *s.Temperature
	}
	if s.ParallelToolCalls != nil {
		m["parallel_tool_calls"] = *s.ParallelToolCalls
	}
	if s.Pricing != nil {
		m["pricing"] = map[string]any{
			"input_per_mtok":  s.Pricing.InputPerMtok,
			"output_per_mtok": s.Pricing.OutputPerMtok,
		}
	}
	// What the window turned out to be, and who said so: a trajectory whose
	// compaction timing is being second-guessed three weeks later needs to say
	// whether the number came from the server, the config, or the built-in
	// default.
	if s.ContextWindow > 0 {
		m["context_window"] = s.ContextWindow
		m["context_window_source"] = s.ContextWindowSource
	}
	return m
}

// Serialize is the model half of the trajectory file.
func (s *Sushi) Serialize() map[string]any {
	return map[string]any{
		"info": map[string]any{
			"config": map[string]any{
				"model":      s.configDump(),
				"model_type": "mini/model.Sushi",
			},
		},
	}
}
