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
	OnRetry     func(attempt, maxAttempts int, err error, wait time.Duration)
	HTTPClient  *http.Client
	ObsTemplate string
	ErrTemplate string

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
	body := s.requestBody(msgs)
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}
	for attempt := 0; ; attempt++ {
		msg, err := s.attempt(ctx, payload, h)
		var interrupt *Interrupt
		if errors.As(err, &interrupt) || err == nil {
			return msg, err
		}
		if ctx.Err() != nil || attempt+1 >= s.retryMax() {
			return nil, err
		}
		var api *APIError
		if errors.As(err, &api) && !api.Retryable {
			return nil, err // litellm's abort_exceptions: retrying cannot fix this
		}
		wait := s.wait(attempt)
		slog.Warn("model query failed, retrying", "attempt", attempt+1, "max_attempts", s.retryMax(), "err", err, "wait", wait.String())
		if s.OnRetry != nil {
			s.OnRetry(attempt+1, s.retryMax(), err, wait)
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}
}

func (s *Sushi) requestBody(msgs []Message) map[string]any {
	prepared := make([]Message, 0, len(msgs))
	for _, m := range msgs {
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
		"tools":          []any{BASH_TOOL},
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

func (s *Sushi) attempt(ctx context.Context, payload []byte, h *Handler) (Message, error) {
	started := time.Now()
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
	stream, err := readStream(res.Body, h)
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
	extra["elapsed_seconds"] = time.Since(started).Seconds()
	if u, ok := stream.response["usage"]; ok {
		extra["model_stats"] = u
	}
	actions, err := parseToolCallActions(stream.toolCalls, s.ErrTemplate, map[string]any{
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

func num(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
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
