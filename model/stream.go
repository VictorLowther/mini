package model

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// responseBytesPerContextToken is A-11's ceiling, expressed per context token:
// 8 whole contexts' worth of response bytes, at the 4 bytes per token the token
// estimator already uses. The heuristic is the owner's, and the point is that it
// sits far above any legitimate completion - a reply is bounded by max_tokens,
// which is itself bounded by the context - while still being finite. At the
// default 262144-token window that is 8 MiB per call.
const responseBytesPerContextToken = 8 * 4

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// maxLineBytes bounds one SSE line. sushi sends each tool call complete in a
// single delta, so a long command is one long line; the 64 KiB scanner default
// would truncate it.
const maxLineBytes = 10 << 20

// stream is an accumulated chat-completion reply.
type stream struct {
	content      strings.Builder
	reasoning    strings.Builder
	toolCalls    []map[string]any
	toolCallByIX map[int]int
	finishReason string
	usage        map[string]any
	meta         map[string]any
	response     map[string]any
}

// readStream consumes the SSE body. Every chunk shape sushi emits is handled
// by one path: `: keepalive` comments and blank lines are skipped, content and
// reasoning_content accumulate, tool_calls deltas are merged by index (sushi
// sends each call whole, a fragmenting server still works), the chunk with an
// empty choices array carries usage, and `data: [DONE]` ends the stream.
func readStream(body io.Reader, h *Handler, maxBytes int) (*stream, error) {
	sc := bufio.NewScanner(body)
	buf := make([]byte, 0, 64<<10)
	// Bound the buffer by the same ceiling that bounds the response, so a
	// hostile server cannot make us allocate maxLineBytes per concurrent request
	// before a single byte has been counted.
	lineCap := maxLineBytes
	if maxBytes > 0 && maxBytes < lineCap {
		lineCap = maxBytes
	}
	sc.Buffer(buf, lineCap)
	st := &stream{meta: map[string]any{}, toolCallByIX: map[int]int{}}
	sawChunk := false
	read := 0
	chunksParsed := 0
	for sc.Scan() {
		line := sc.Bytes()
		// A-11: count every byte off the wire, before any of the skips below,
		// because the unbounded case is precisely the one that never becomes a
		// chunk - a server (or a proxy, or a port pointed at the wrong service)
		// that streams `: keepalive` comments forever. Without this the only
		// ceilings were per-line and the scanner's own, so a response of any
		// size grew st.content, the tool-call table, every Handler callback and
		// the live GUI card until the process ran out of room, and the request
		// held a model-gate slot while it did.
		read += len(line) + 1
		if maxBytes > 0 && read > maxBytes {
			e := &APIError{
				Message: fmt.Sprintf(
					"%s sent %s of streamed response, past the %s ceiling mini allows one call "+
						"(8x the %d-token context this client believes the model has). "+
						"The stream was abandoned; nothing was retried.",
					"SSE stream", humanBytes(read), humanBytes(maxBytes), maxBytes/responseBytesPerContextToken),
				Type:      "response_too_large",
				Retryable: false,
			}
			// Whether any answer arrived is the difference between a server that
			// rambles and one that is not a model server at all - a wrong port or a
			// proxy is the usual cause. "Too big" alone would send the reader to the
			// wrong setting, so say which of the two this looked like.
			if chunksParsed == 0 {
				e.Message += "; no completion chunk had been received, so this may not be an OpenAI-compatible SSE endpoint"
			}
			return nil, e
		}
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		v, ok := strings.CutPrefix(string(line), "data:")
		if !ok {
			continue
		}
		payload := strings.TrimRight(strings.TrimPrefix(v, " "), "\r")
		if payload == "" {
			continue // an empty data field is a no-op event
		}
		if payload == "[DONE]" {
			sawChunk = true
			break
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return nil, &APIError{Message: "malformed SSE chunk: " + err.Error(), Retryable: true}
		}
		sawChunk = true
		chunksParsed++
		if errObj, ok := chunk["error"]; ok && errObj != nil {
			return nil, errorFromChunk(errObj)
		}
		for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "timings"} {
			if v, ok := chunk[k]; ok {
				st.meta[k] = v
			}
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			st.usage = u
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			st.finishReason = fr
			if fr == "error" {
				// Mid-stream failure: sushi reports an error field and marks
				// the choice errored, in the same frame.
				if errObj, ok := chunk["error"]; ok && errObj != nil {
					return nil, errorFromChunk(errObj)
				}
				return nil, &APIError{Message: "the stream ended with finish_reason=error", Type: st.finishReason, Retryable: true}
			}
		}
		delta, _ := choice["delta"].(map[string]any)
		if c, _ := delta["content"].(string); c != "" {
			st.content.WriteString(c)
			h.content(c)
		}
		if r, _ := delta["reasoning_content"].(string); r != "" {
			st.reasoning.WriteString(r)
			h.reasoning(r)
		}
		for _, raw := range asSlice(delta["tool_calls"]) {
			tc, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			st.mergeToolCall(tc)
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, &APIError{Message: "SSE line exceeded 10 MiB", Retryable: false}
		}
		return nil, &APIError{Message: "reading the model stream: " + err.Error(), Retryable: true}
	}
	if !sawChunk {
		return nil, &APIError{Message: "the model stream ended without any chunks", Retryable: true}
	}
	if st.finishReason == "error" {
		return nil, &APIError{Message: "the stream ended with finish_reason=error", Retryable: true}
	}
	st.response = st.buildResponse()
	return st, nil
}

// mergeToolCall folds one delta into the call with the same index, so servers
// that fragment arguments across chunks still produce a complete call.
func (st *stream) mergeToolCall(tc map[string]any) {
	index := 0
	if f, ok := tc["index"].(float64); ok {
		index = int(f)
	}
	slot, seen := st.toolCallByIX[index]
	if !seen {
		slot = len(st.toolCalls)
		st.toolCallByIX[index] = slot
		st.toolCalls = append(st.toolCalls, map[string]any{"index": float64(index), "type": "function"})
	}
	existing := st.toolCalls[slot]
	for _, k := range []string{"id", "type"} {
		if v, ok := tc[k]; ok && v != nil {
			existing[k] = v
		}
	}
	fn, _ := tc["function"].(map[string]any)
	if len(fn) == 0 {
		return
	}
	target, _ := existing["function"].(map[string]any)
	if target == nil {
		target = map[string]any{}
		existing["function"] = target
	}
	if name, _ := fn["name"].(string); name != "" {
		target["name"] = name
	}
	if args, _ := fn["arguments"].(string); args != "" {
		prev, _ := target["arguments"].(string)
		target["arguments"] = prev + args
	}
}

// assistant builds the OpenAI-shaped reply. Nil-valued keys are omitted so the
// message can be sent straight back in the history.
func (st *stream) assistant() Message {
	msg := Message{"role": "assistant"}
	if st.content.Len() > 0 {
		msg["content"] = st.content.String()
	} else {
		msg["content"] = ""
	}
	if st.reasoning.Len() > 0 {
		msg["reasoning_content"] = st.reasoning.String()
	}
	if len(st.toolCalls) > 0 {
		msg["tool_calls"] = st.toolCalls
	}
	return msg
}

// buildResponse reconstructs the chat-completion object the stream carried, so
// extra.response looks like litellm's model_dump() in the trajectory.
func (st *stream) buildResponse() map[string]any {
	resp := map[string]any{"model": st.Model()}
	for k, v := range st.meta {
		resp[k] = v
	}
	resp["choices"] = []any{map[string]any{
		"index":         0,
		"message":       st.assistant(),
		"finish_reason": nullable(st.finishReason),
	}}
	if st.usage != nil {
		resp["usage"] = st.usage
	}
	return resp
}

func (st *stream) Model() string {
	m, _ := st.meta["model"].(string)
	return m
}

func (st *stream) totalTokens() int {
	if st.usage == nil {
		return 0
	}
	return int(num(st.usage["total_tokens"]))
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func asSlice(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	}
	return nil
}

func errorFromChunk(v any) error {
	e := &APIError{Retryable: true}
	e.Message, e.Type, e.Code = errorFields(v)
	if code, err := strconv.Atoi(e.Code); err == nil {
		e.Status = code
		e.Retryable = !abortOn(code, e.Type)
	}
	return e
}
