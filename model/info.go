package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Context-window discovery. The window a session runs against decides when
// compaction fires (agent/compaction.go) and what the footer's ctx% is
// measured against, so getting it from the server rather than from a number
// baked into the shipped config is what keeps those two honest when the same
// config meets a different server (or the same server serving a different
// extent, YaRN-extended or not).
//
// The resolution chain, in order:
//
//  1. what the server reports for this model (GET /v1/models, the OpenAI
//     standard model list, which is the only place a chat-completions server
//     can describe itself without a non-standard verb);
//  2. the local config's agent.context_window, which stays the operator's
//     override for a server that will not say;
//  3. DefaultContextWindow.

// DefaultContextWindow is the last resort: the window assumed when neither the
// server nor the local config names one. It is a size, not a disable switch -
// automatic compaction is switched off with compact_enabled (the loop only
// treats a ContextWindow of 0 as "unknown" when a Config is built directly,
// bypassing discovery). The number has to leave room under its own trigger for
// the compaction to land: at the shipped ratio and keep/summary budgets a
// window under ~70k would produce a rebuilt context bigger than the threshold
// that triggered it, so this is deliberately a modern local model's native
// extent (see docs/compaction-research.md: the models mini targets have
// 262 144 native context).
const DefaultContextWindow = 262144

// Where the effective window came from, recorded so the banner, the transcript
// header and the trajectory can say which of the three it was.
const (
	ContextFromServer  = "server"
	ContextFromConfig  = "config"
	ContextFromDefault = "default"
)

// A model list is a small document and an optional question, so both its
// patience and its response size are bounded hard: neither can hold a startup
// open or make a client read a server's whole database.
const (
	defaultInfoTimeout  = 3 * time.Second
	infoResponseMaxByte = 4 << 20
)

// contextLengthKeys are the field names an OpenAI-compatible server may carry
// its window in. /v1/models standardizes only id/object/created/owned_by, so
// every server that says more invents its own spelling; these are the ones in
// the wild (sushi and vLLM-style servers use context_length and max_model_len,
// llama.cpp's meta block uses n_ctx, OpenRouter context_window, and
// OpenAI's own model objects nest the limit under "limit"). Ordered most
// specific first, and the top level is read before the nested metadata: a
// server that runs an extended extent publishes that as the top-level number
// while its meta block keeps the model's native one.
var contextLengthKeys = []string{
	"context_length",
	"context_window",
	"max_context_length",
	"max_model_len",
	"default_context_window",
	"model_max_tokens",
	"n_ctx",
	"context_size",
}

// contextLengthContainers are the nested objects searched after the top level.
var contextLengthContainers = []string{"meta", "metadata", "model_card"}

// ModelInfo is one entry of the server's model list, reduced to what this
// lookup needs.
type ModelInfo struct {
	ID string
	// ContextWindow is what the server says the model's window is, 0 when it
	// says nothing usable.
	ContextWindow int
	// Loaded reports the entry the server marks as resident/ready, which is
	// how a local server telling several models apart names the one a request
	// would actually hit.
	Loaded bool
}

// infoTimeout bounds the startup lookup: a server that is slow to answer the
// model list must not hold the window open before the first turn.
func (s *Sushi) lookupTimeout() time.Duration {
	if s.InfoTimeout > 0 {
		return s.InfoTimeout
	}
	return defaultInfoTimeout
}

// ServerContextWindow asks this client's server for the window of the model it
// queries. It returns 0 with a nil error when the server answered but said
// nothing usable (a plain OpenAI list, an ambiguous set of ids): only a
// failure to ask at all comes back as an error. Neither case is fatal to a
// run - see DiscoverContextWindow.
func (s *Sushi) ServerContextWindow(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, s.lookupTimeout())
	defer cancel()
	entries, err := s.listModels(ctx)
	if err != nil {
		return 0, err
	}
	return s.matchModel(entries), nil
}

// DiscoverContextWindow resolves the window the session runs against (the
// chain documented at the top of this file), remembers it on the client for
// Serialize and for the front ends, and returns it with its source. Call it
// once, before the loop and the UI start reading either copy of the number.
func (s *Sushi) DiscoverContextWindow(ctx context.Context, configured int) (int, string) {
	server, err := s.ServerContextWindow(ctx)
	if err != nil {
		// A server that cannot be reached for its model list is still the
		// server the run talks to; the query path reports that, not here.
		// Saying nothing means "fall through the chain", not "fail the run".
		slog.Debug("model info lookup failed; falling back", "err", err)
		server = 0
	}
	// Every branch yields a positive window: the answer is never "no window",
	// which is what a ContextWindow of 0 means to the loop (no automatic
	// compaction at all) and which only a Config built by hand should carry.
	window, source := server, ContextFromServer
	switch {
	case window > 0:
	case configured > 0:
		window, source = configured, ContextFromConfig
	default:
		window, source = DefaultContextWindow, ContextFromDefault
	}
	s.ContextWindow, s.ContextWindowSource = window, source
	slog.Info("context window", "tokens", window, "source", source,
		"model", s.Model, "configured", configured)
	return window, source
}

// ContextWindowNote is the one-line statement of the resolved window every
// front end prints ("1048576 tok (server)"), so a user can see at a glance
// whether mini believes the server or their config. It reads "window unknown"
// before discovery has run.
func (s *Sushi) ContextWindowNote() string {
	if s.ContextWindow <= 0 {
		return "window unknown"
	}
	source := s.ContextWindowSource
	if source == "" {
		source = "unknown source"
	}
	return fmt.Sprintf("%d tok (%s)", s.ContextWindow, source)
}

// listModels performs GET {base}/v1/models and decodes the entries. No retry
// loop: this is an optional startup question, and the backoff schedule belongs
// to the calls a run cannot do without.
func (s *Sushi) listModels(ctx context.Context) ([]ModelInfo, error) {
	url := s.BaseURL + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := s.client().Do(req)
	if err != nil {
		return nil, &APIError{Message: fmt.Sprintf("could not read %s: %v", url, err), Retryable: ctx.Err() == nil}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, readError(res)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, infoResponseMaxByte))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", url, err)
	}
	// The standard envelope is {"object":"list","data":[...]}. A bare array is
	// off-spec but common enough in hand-rolled servers that it is worth
	// taking rather than losing the window over it.
	var doc struct {
		Data   []map[string]any `json:"data"`
		Models []map[string]any `json:"models"`
	}
	// A body that decodes to nothing usable is an empty answer, not a failure:
	// {"object":"list","data":[]} is a server saying it has no models to
	// describe, and the chain falls back exactly as if it had said nothing.
	// Only a body that is not a model list at all - an HTML error page from a
	// proxy in front of the server - is a failed question.
	if err := json.Unmarshal(body, &doc); err == nil {
		if len(doc.Data) > 0 {
			return modelInfos(doc.Data), nil
		}
		// Servers that predate the envelope sometimes answer {"models":[...]}.
		return modelInfos(doc.Models), nil
	}
	var bare []map[string]any
	if err := json.Unmarshal(body, &bare); err == nil {
		return modelInfos(bare), nil
	}
	return nil, fmt.Errorf("decoding %s: no model list in the response", url)
}

func modelInfos(entries []map[string]any) []ModelInfo {
	out := make([]ModelInfo, 0, len(entries))
	for _, e := range entries {
		id, _ := e["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		out = append(out, ModelInfo{
			ID:            id,
			ContextWindow: contextLengthOf(e),
			Loaded:        entryLoaded(e),
		})
	}
	return out
}

// contextLengthOf reads the window out of one model entry, whatever spelling
// its server chose. Numbers that cannot be a context size (zero, negative, or
// past a billion tokens) are treated as absent: a garbage field must not make
// the compaction trigger unreachable.
func contextLengthOf(entry map[string]any) int {
	if n := firstContextLength(entry, contextLengthKeys); n > 0 {
		return n
	}
	for _, key := range contextLengthContainers {
		if sub, ok := entry[key].(map[string]any); ok {
			if n := firstContextLength(sub, contextLengthKeys); n > 0 {
				return n
			}
		}
	}
	// OpenAI's model objects carry {"limit":{"context":128000,...}}.
	if lim, ok := entry["limit"].(map[string]any); ok {
		if n := firstContextLength(lim, []string{"context", "context_window", "max_context_window"}); n > 0 {
			return n
		}
	}
	return 0
}

// firstContextLength reads the first key that carries a usable size. Numbers
// and digit strings both count - a server that stringifies the field still
// means it - and anything that cannot be a context size (zero, negative, past
// a billion tokens, "unknown") counts as absent, because a garbage field must
// not make the compaction trigger unreachable.
func firstContextLength(m map[string]any, keys []string) int {
	for _, k := range keys {
		n := num(m[k])
		if s, ok := m[k].(string); ok {
			n, _ = strconv.ParseFloat(strings.TrimSpace(s), 64)
		}
		if n > 0 && n < 1e9 {
			return int(n)
		}
	}
	return 0
}

// entryLoaded reports whether the server says this model is resident. Local
// servers tell it either as a boolean or as a state string; an entry saying
// neither is not-loaded rather than unknown-loaded, so the last tier of
// matchModel can only fire on a server that actually distinguishes its models.
func entryLoaded(entry map[string]any) bool {
	if v, ok := entry["loaded"].(bool); ok {
		return v
	}
	state, _ := entry["state"].(string)
	switch strings.ToLower(state) {
	case "ready", "loaded", "running", "active":
		return true
	}
	return false
}

// matchModel picks the window of the list entry describing the model this
// client queries. Servers answer with their own ids: a local server names the
// checkpoint ("beamster/Qwen3.8-Flash-Next-Sushi-4bpw") while the config holds
// the alias the user typed ("beamster/qwen"), so an exact-id match alone would
// miss the commonest case this lookup exists for. The name tiers run from most
// to least confident and narrow the field as they go; the first one naming
// exactly one entry wins.
//
// Anything that stays ambiguous returns 0, and so does a list in which no
// entry carries a window: no server opinion beats a wrong one, because the
// fallback is the local config the operator wrote on purpose.
func (s *Sushi) matchModel(entries []ModelInfo) int {
	// An entry may carry the configured name exactly and say nothing about its
	// window. Take that for what it is - silence about this model - instead of
	// going looking for a window under some other id: a server holding both
	// "gpt-4o" and "gpt-4o-mini" would otherwise talk the mini's extent into
	// the bigger model's compaction trigger.
	want := strings.ToLower(strings.TrimSpace(s.Model))
	if want == "" {
		// Nothing to match a name against: guessing from a model list would be
		// guessing at an unnamed model.
		return 0
	}
	named := filterModelInfos(entries, func(m ModelInfo) bool {
		return m.ID == s.Model || namesEqual(m, want)
	})
	if len(named) > 0 {
		withWindow := filterModelInfos(named, func(m ModelInfo) bool { return m.ContextWindow > 0 })
		if len(withWindow) == 1 {
			return withWindow[0].ContextWindow
		}
		return 0
	}

	candidates := filterModelInfos(entries, func(m ModelInfo) bool { return m.ContextWindow > 0 })
	if len(candidates) == 0 {
		return 0
	}
	aliased := false
	for _, tier := range []func(ModelInfo) bool{
		func(m ModelInfo) bool { return onePrefixesOther(m, want) },
		func(m ModelInfo) bool { return oneContainsOther(m, want) },
	} {
		matched := filterModelInfos(candidates, tier)
		switch {
		case len(matched) == 1:
			return matched[0].ContextWindow
		case len(matched) > 1:
			candidates, aliased = matched, true
		}
	}
	if aliased {
		// Several entries share the name; only the server itself can say which
		// one a request would hit.
		return soleLoadedContext(candidates)
	}
	// The list never named this model at all. A local server holding exactly
	// one model - or exactly one resident model, which is what a slot-bound
	// server reports - is still answerable; anything else is a guess.
	if len(candidates) == 1 {
		return candidates[0].ContextWindow
	}
	return soleLoadedContext(candidates)
}

// soleLoadedContext is the last tier: the window of the one resident model, or
// 0 when the server does not single one out.
func soleLoadedContext(candidates []ModelInfo) int {
	loaded := filterModelInfos(candidates, func(m ModelInfo) bool { return m.Loaded })
	if len(loaded) == 1 {
		return loaded[0].ContextWindow
	}
	return 0
}

func filterModelInfos(in []ModelInfo, keep func(ModelInfo) bool) []ModelInfo {
	out := make([]ModelInfo, 0, len(in))
	for _, m := range in {
		if keep(m) {
			out = append(out, m)
		}
	}
	return out
}

// nameForms is the pair of strings a model id is compared on: the whole
// (folded) id and its last path segment, which is what makes "qwen" and
// "sushi/qwen" describe the same model.
func nameForms(m ModelInfo) (full, tail string) {
	full = strings.ToLower(strings.TrimSpace(m.ID))
	return full, segment(full)
}

func segment(lower string) string {
	return lower[strings.LastIndexByte(lower, '/')+1:]
}

func namesEqual(m ModelInfo, want string) bool {
	full, tail := nameForms(m)
	return full == want || tail == want || tail == segment(want)
}

func onePrefixesOther(m ModelInfo, want string) bool {
	full, tail := nameForms(m)
	wt := segment(want)
	return strings.HasPrefix(full, want) || strings.HasPrefix(want, full) ||
		strings.HasPrefix(tail, wt) || strings.HasPrefix(wt, tail)
}

func oneContainsOther(m ModelInfo, want string) bool {
	full, tail := nameForms(m)
	wt := segment(want)
	return strings.Contains(full, want) || strings.Contains(want, full) ||
		strings.Contains(tail, wt) || strings.Contains(wt, tail)
}
