# Session Compaction Research

A survey of how the agentic harnesses checked out beside mini (`~/src/llm_stuff/`)
manage conversation-context growth, distilled into approaches with advantages,
disadvantages, and implementation difficulty, ending with a recommendation mapped
onto mini's architecture (SQLite transcript store, GUI-only cards, local sushi
server, long autonomous bash turns).

Sources: `crush` (Go), `zero` (Go), `oh-my-pi`/omp (Rust+TS), `qwen-code` (TS),
`pi` via `little-coder` (TS + extensions), `mini-swe-agent` (Python).
All findings read from the working copies on 2026-11-14.

---

## 1. The design space

Every harness resolves the same six questions. The axes:

1. **Trigger** — reserve-based (`window − reserve`) vs percentage-of-window vs
   error-driven (react to an actual overflow rejection) vs idle/scheduled.
   And *where in the loop* it is checked: at user-prompt boundaries only, or at
   every tool-loop turn (mid-run). Little-coder's most valuable finding is that
   the check point matters as much as the formula: an autonomous agent that runs
   dozens of tool calls without returning to the user-prompt boundary will sail
   past any threshold that is only evaluated between prompts.
2. **Mechanism** — LLM summary vs structural/no-LLM reduction (prune stale tool
   output, dedupe identical reads, elide media, truncate observations) vs a
   ladder of both. For bash-heavy agents, tool *observations* are ~90% of
   tokens; the no-LLM stages often defer or eliminate the paid summary entirely.
3. **What survives** — full-history summary (no verbatim tail) vs split-point
   with a verbatim recent tail, plus structured state (plans, todo lists, file
   ledgers) preserved independently of the prose summary.
4. **Boundary integrity** — the dangling tool-call problem at the cut point.
   Two schools: (a) *never split a call/result pair by construction* (cut only
   at points where the kept side stays contiguous to the leaf); (b) *repair at
   send time* (synthesize results for unanswered calls). mini already has the
   machinery for (b) — pre-allocated placeholder slots filled positionally.
5. **Persistence** — compaction as a *view directive* over an immutable full
   history (crush's `summary_message_id` pointer; pi/omp's append-only JSONL
   compaction entries) vs a mutation of a live message list. Every harness that
   persists it does the former. This composes perfectly with mini's design: the
   store already holds everything; only `requestBody` projection changes.
6. **Prompt/KV-cache economics** — compaction rewrites the tail region and must
   leave the system+tools prefix intact for cache reuse; the summarizing request
   itself can be shaped to *read* the just-warmed prefix (qwen-code's
   cache-sharing side-query; crush sends the summary call with the same session
   affinity headers and system prefix). On local hardware, where every prefill
   costs wall-clock seconds on the same GPU as everything else, this axis is
   more important than dollar cost.

---

## 2. Per-harness profiles

### 2.1 mini-swe-agent — the "do nothing" baseline

No compaction at all. History is append-only and sent whole every step; context
is bounded only by per-observation template truncation (head+tail elision above
10 000 chars, with an `<elided_chars>` marker and a warning telling the model to
use `head`/`tail`/`sed` next time) and by hard abort limits (cost/steps/wall
clock). `ContextWindowExceededError` is in `abort_exceptions` — the run dies
rather than compressing. Notably, the *untruncated* output survives in
`extra.raw_output`, decoupling what-the-model-sees from what-is-stored.

- **Advantages**: zero complexity, fully inspectable trajectory, nothing to get
  subtly wrong (no boundary rules, no repair paths). The observation-template
  truncation alone prevents the single worst token sink.
- **Disadvantages**: session length is capped by the window, full stop; the run
  aborts on overflow, so long tasks are impossible on small windows.
- **Difficulty**: trivial (~0 LOC of compaction; ~20 lines of template
  truncation). mini-swe-agent is where mini started; mini has already outgrown
  it (SQLite sessions, resume, GUI).

**Lesson for mini**: head/tail observation capping at record time is cheap
insurance and orthogonal to any compaction scheme chosen later.

### 2.2 crush — rolling single-anchor summary (Go)

Internal name: "summarize". Triggered by a `StopWhen` check **after each agent
step**: compact when remaining context ≤ 20k tokens (windows > 200k) or ≤ 20%
(smaller windows); unknown window size → auto disabled (protects local models —
directly relevant to mini). The main model re-summarizes *everything from the
previous summary onward, including that summary* (`agent/agent.go`
`summarize()`, template in `agent/templates/summary.md` — sections: Current
State, Files & Changes, Technical Context, Strategy, Exact Next Steps;
"err on the side of too much detail"). The summary is stored as an assistant
message and the session's `summary_message_id` pointer is advanced; rebuild is
`SELECT ... >= summary` **with `msgs[0].Role = User`** — the assistant-stored
summary is replayed as a user message, a two-line hack that sidesteps leading-
message role rules. Original history is never deleted. Dangling tool calls are
made impossible *generally*, not at the cut point: `preparePrompt` +
`settleTurn` guarantee every call ID on the wire has exactly one result
(stored, deduped, or a synthesized "interrupted, you may retry" error),
including crash-repair on resume. Todo list is injected into the summarization
prompt. On failure: anchor not swapped, nothing dropped, turn ends with error.
PreCompact/PostCompact hooks can veto compaction. ~450 LOC, low coupling —
it reuses the normal turn pipeline rather than a parallel compaction path.

- **Advantages**: simplest coherent persistent design; zero read-path
  complexity (one pointer); rolling re-summarization keeps exactly one summary
  in context (no stack of layered summaries); generic settle/repair machinery
  doubles as resume-crash-repair.
- **Disadvantages**: no verbatim tail at all — the model loses *recent*
  exchanges' precision at every hop; the summary is re-summarized every compaction
  (quadratic-ish re-summarization of the summary itself, with cumulative loss);
  no zero-LLM fallback — if the summarizer fails, nothing shrinks; thresholds
  not configurable.
- **Difficulty**: **low**. The hardest part (pair settlement) is machinery mini
  has already independently built (placeholders + positional fill + exit-row
  filtering). A crush-style design fits mini's store almost exactly as-is.

### 2.3 pi / little-coder — split-point tail preservation on a session tree (TS)

pi (the substrate little-coder runs) is the canonical "claude-style" compaction.
Trigger: `contextTokens > window − reserveTokens` (default reserve 16 384),
checked **at run-end and pre-prompt only**. Mechanism: walk backwards from the
leaf accumulating ~`keepRecentTokens` (default 20 000) worth of messages; the
cut point must be a user/assistant message — **never a tool result** — so the
kept window is always contiguous and a call/result pair can never be split; the
summarized side is serialized to *plain text* (never replayed as messages), so
API-shape validation never happens near the boundary. Output: a structured
checkpoint with fixed sections (`Goal / Constraints & Preferences / Progress
(Done|In Progress|Blocked) / Key Decisions / Next Steps / Critical Context`),
plus a file-op ledger (`<read-files>`/`<modified-files>`) accumulated across
compaction generations. If the cut lands mid-turn, the turn's *prefix* is
summarized separately and merged ("turn-split" handling — the common case for
long autonomous runs). The summary replays as a user message wrapped in
"the conversation history before this point was compacted into the following
summary". Persistence: append-only JSONL **tree** — a compaction is just
another node carrying `firstKeptEntryId`; resume rebuilds the path leaf→root and
applies the latest compaction on it; forking before a compaction doesn't
inherit it. Full history always survives in the file. Iteration: previous
summary is fed into an UPDATE prompt ("PRESERVE all existing information…").
Overflow recovery: error classified from ~25 provider regexes (including
llama.cpp's phrasing — directly useful to sushi), strip error message, compact,
retry **once**. ~1 040 LOC core.

little-coder's extensions are the field report on running this with **small
local models**, which is mini's exact situation:

- **context-watchdog**: pi's boundary-only check "can blow straight past the
  window" during autonomous runs, so it hooks every `turn_start` and compacts
  at 80% of window mid-run. Two hard-won details: (1) it must not `await`
  compact mid-run (deadlock: compact aborts the agent, the agent awaits the
  handler); (2) the post-compaction resume message must *steer behavior* —
  "do NOT re-read files you have already read" — because post-compaction
  amnesia triggers re-exploration that re-inflates context into a doomed second
  compaction (#68). It measures post-compaction progress and *pauses* auto if
  the freed headroom was < 5 points.
- **local-estimate**: providers may lie about prompt size (Ollama reports
  `prompt_eval_count` *after* silently truncating), so it keeps a chars/4
  local estimate of exactly what would be sent and reconciles with
  `max(reported, estimate)` — biased toward compacting *early*, the safe
  direction.
- **evidence-compact**: compaction only rebuilds the message array, so
  out-of-band data kept in an extension store survives automatically; a 32-line
  bridge message after compaction just reminds the model its data still exists.
- **Pitfall found by measurement**: pi's defaults are *unsound for 32k windows*
  — `keepRecentTokens` 20 000 (61% of window) exceeds what the trigger leaves
  to keep. Reserve-based and tail-budget-based parameters must be checked
  against each other for small windows.

- **Advantages**: verbatim recent tail (no precision loss on current work);
  contiguous-keep rule makes dangling pairs structurally impossible with zero
  repair code; summarizer input is only the discarded middle, so the paid call
  is much smaller than full-history designs; compaction-as-graph-node gives
  fork/branch/resume semantics for free; the file-op ledger preserves
  reconstructible-but-expensive state cheaply.
- **Disadvantages**: prose summary + tail is the only memory — structured live
  state (plans) needs explicit carry machinery; layered summaries accumulate on
  repeated compaction (mitigated by UPDATE prompt but not eliminated); default
  parameters hostile to small windows; boundary-only triggering is useless for
  autonomous runs (hence the watchdog bolt-on).
- **Difficulty**: **low–medium**. The split-point + never-cut-at-tool-result
  rule is ~100 lines given mini's turn-order ledger; the structured summary
  prompt and update prompt are text.

### 2.4 zero — the three-tier cost ladder (Go)

The most systematically engineered non-exotic design surveyed. ~4.4k LOC, but
the pipeline composes in clearly separable stages:

`estimate → calibrate (EMA on real usage) → threshold+hysteresis gate →
free prune → project (semantic brief) → paid LLM summary → append preserved
state JSON → splice at assistant boundary`, plus a reactive error-driven path
reusing the same stages.

Key components:

- **Estimator**: no tokenizer; non-whitespace-bytes/4 (~15–20% better than
  len/4), images flat 1 000, +4 per message/tool-call overhead — then
  *calibrated by real provider usage through an EMA (α=0.3, per-sample clamp)*.
  Tool definitions count toward size because "they ride on every turn."
- **Gate**: 70% of window, with a **low-water-mark hysteresis** (no
  re-compaction until history grows past the post-compaction size) and a
  shrink check (if compaction didn't shrink, keep the original anyway and move
  the mark) — anti-thrash machinery.
- **prune.go (free stage)**: replace *stale* tool outputs (older than last N
  messages and a trailing 40 000-token protection window, ≥200-token bodies,
  only when ≥20 000 tokens are reclaimable) with
  `[pruned <tool> output (~N tokens) — re-run the tool if you need it again]`;
  byte-identical superseded `read_file` results dedupe to a placeholder — but
  *only under pressure*, because rewriting history burns the warm cache prefix
  for small savings. Pruning frequently avoids the summarizer call entirely.
- **Projection (summarizer input reduction)**: the elided middle is rebuilt as
  one bounded "brief" — user turns ≤256 informative words (stopword-skipping),
  assistant ≤200 words + ≤8 one-line tool-call summaries, tool results
  **dropped entirely** unless errors/ask_user answers/touched files, previous
  summary carried head+tail-clipped, whole brief ≤24 KiB.
- **Preserved state**: after the prose summary, append a **single-line JSON
  block** (one line so markdown/fences round-trip losslessly) with the active
  plan, task state (objective, constraints, changed files, unresolved failures,
  artifacts — mutable fields admitted only when a `PlanParity` check says the
  snapshot still agrees with the transcript), recent edits (≤20 paths,
  re-edit moves path to newest), loaded skills/tool schemas, project
  instructions. Designed to **merge idempotently across repeated
  compactions**.
- **Boundary**: keep system verbatim; split at `len − preserveLast(6)`, then
  walk **backward only** until an **assistant** message — because the summary
  injects as a *user* message, a user-/tool-led suffix would create
  consecutive-same-role turns strict providers reject. Pair integrity: whole
  call/result pairs fall into the middle together.
- **Summarizer model**: first-class separate-model support
  (`ZERO_COMPACTION_MODEL` / preferences → curated per-vendor cheap defaults
  → literal `"main"` to disable; sticky-broken fallback to main model).
  Motivation comment: at a 200k window one summarization is a ~100k-token
  input call at full price, "the single most expensive recurring event."
- **Overflow of the summarizer input itself**: recursively halve the slice,
  summarize halves, re-summarize the two partials into one.
- **Durable split**: in-loop compaction is **transient** (full raw history
  stays in the event log; a resumed run recomputes it); manual `/compact`
  *is* persisted as a `session_compaction` event that replay rehydrates as a
  view projection. There's also an unused `NativeCompaction` seam reserved for
  provider-side compaction.
- **Incremental compaction at the tool boundary** (adjacent system): per-tool
  output ceilings with full-output **spill files** and notices telling the
  model to `grep`/`read_file` the spill instead of re-running; command-aware
  reducers that strip success-only noise from `go test`/`pytest`/`cargo`
  output. This is observation hygiene done right: bounded, recoverable,
  zero-LLM.

- **Advantages**: cheapest-first ladder means most pressure is resolved with
  no paid call; hysteresis + shrink check make thrashing impossible; the
  structured-JSON-preserved-state block is the best surveyed answer to "prose
  summaries lose the state the next turn actually acts on"; cache-prefix
  discipline is explicit at every rewrite point; usage-calibrated estimation
  is uniquely rigorous.
- **Disadvantages**: heaviest of the "sane" designs (~4.4k LOC, ~2.8k test
  LOC); auto compaction is run-scoped/non-durable (resume recomputes from full
  history — arguably fine, arguably a bug); threshold documented
  inconsistently three ways; no oldest-drop last resort if the summarizer
  itself keeps failing.
- **Difficulty**: **medium–high** for the whole system; **low** to steal
  stages piecemeal (prune.go alone is 187 LOC and the highest value-per-line
  component in this survey for a bash agent).

### 2.5 qwen-code — full-history summary + disk restoration + microcompaction (TS)

Gemini-CLI lineage, deliberately rebased onto claude-code semantics. Threshold
ladder: `computeThresholds(window)` gives warn/auto/hard tiers around
`0.85 × window` with 20k/13k/3k-token buffers; the compared quantity is
API-reported prompt tokens + previous output + char/4 of the pending message
(fixes the classic "gate lags one turn" and "first send after resume sees 0"
under-counts). Three send-path hooks: cheap-gate before send at `auto`, forced
**pre-send rescue at `hard`**, and one-shot **reactive compress-and-retry** on
provider overflow/413. Manual `/compress` (LLM) and `/compress-fast`
(no-LLM). PreCompact/PostCompact hooks.

Mechanism: **no split point, no verbatim tail** — the entire curated history
goes into the side-query, which produces a `<state_snapshot>` with 9 fixed
sections (primary request, technical concepts, files & code, errors & fixes,
problem solving, **all user messages**, pending tasks, current work, next
step), preceded by a disposable `<analysis>` scratchpad that is *stripped
after generation* (CoT quality without paying its tokens on replay).
Continuity is bought back by **post-compact attachments**: recent
read/modified files **re-read from disk** (≤5 files, ≤5k tokens each,
workspace-boundary + symlink + binary guards), up to 3 recent images, the
plan-mode banner ("the summary may not reflect this constraint — honor plan
mode regardless"), and the background-tasks roster. The rebuilt history is
`user(summary + resume trailer) / model("Got it…") / user(attachments)`, with
the trailer generated in code, never by the model. Two summarizer request
shapes: a **cache-sharing** path that re-posts the *unslimmed* history with the
main system prompt+tools so the warm prefix cache hits, appending the directive
as a final user turn (strictly gated: provider-reported token anchor,
`prompt + 20k ≤ window`, well-formed output required), falling back to a cold
slimmed request. **Microcompaction** is a separate pure function that blanks
old tool outputs to `[Old tool result content cleared]` and stale media to
placeholders, triggered by *idle minutes* (default 60) or cumulative
compactable-tool chars (default 500k, clearing down to a threshold/2
**low-watermark** specifically to stop per-turn cache thrash), keeping the
recent N per kind. Notable depth: circuit breaker (3 consecutive failures
disable auto until a manual succeeds), inflation guard (if new history isn't
smaller, NOOP with a failure status), truncation guard (missing
`</state_snapshot>` → drop), every compaction persisted as a `chat_compression`
**checkpoint record** in the JSONL — resume replaces accumulated history with
the latest checkpoint snapshot.

- **Advantages**: single canonical state document (no stale layered summaries);
  disk-restoration of file context is *fresher* than verbatim tails for coding
  agents (files have changed since they were read); checkpoint-style
  persistence makes resume cheap and exact; microcompaction handles most
  pressure with no LLM; cache-sharing side-query is the most cache-exploitative
  summarization request surveyed; extensive failure taxonomy with explicit
  statuses and a breaker.
- **Disadvantages**: heaviest test surface (8.8k test LOC "signals how
  hard-won the edge cases were"); full-history side-query is the most expensive
  summarization shape when cache-sharing is unavailable; losing the verbatim
  tail makes the model more prone to redoing recent micro-work; enormous
  configuration surface.
- **Difficulty**: **high** for the whole system; the checkpoint-record
  persistence idea and the `<state_snapshot>` prompt are individually cheap.

### 2.6 oh-my-pi (snapcompact) — five-method fallback chain (Rust+TS)

The research-grade outlier: ~20k LOC of compaction-specific runtime.
Default `methodOrder = [remote, snapcompact, handoff, shake, soft]` —
provider-native compaction → **snapcompact** → LLM handoff document →
mechanical elision → classic summary — with capability-gated skipping, chosen
per situation (overflow recovery prefers the no-network methods).

**snapcompact** is the novel bit: the discarded history is *not* summarized —
it is serialized to compact text (`¶user:`/`¶think:`/`¶ai:`/`¶call:name(args)`
with tool results merged into their calls, capped), then **printed onto
model-sized PNG frames of pixel-font text that a vision model reads back**.
Foveated layout: one page of verbatim plain text at the oldest edge, one at
the newest, an imaged middle; over-budget, the middle itself goes HQ/dense-LQ/
HQ and drops the oldest dense slice. Frame shape and resolution are chosen
per *reading model lineage* to arbitrage each provider's vision billing
(Gemini's fixed per-image cost → maximize pixels per image; Anthropic's pixel
cap → 1932px; OpenAI area-proportional → resolution buys nothing). The
canonical artifact is the bounded *text*; frames are re-rendered per compaction
rather than carried forward. Tuned with ~30k LOC of shipped Python evals
(SQuAD recall under real billing rules). Triggers include threshold, mid-turn,
overflow, incomplete-output, and an idle mode that times itself **5s before
the Anthropic cache-warmed prefix would expire** so the summary request still
reads the cache — plus **async speculative compaction**: background-
summarization armed before the threshold is crossed and committed atomically
on crossing, discarded if the branch changes. Cut points never land on tool
results (same contiguity rule as pi, including split-turn prefix summaries);
tree rewinds *can* orphan calls, so the rebuild strips every `tool_use` whose
result is off the resolved path. Compaction entries are first-class JSONL tree
nodes; display transcript is fully decoupled from LLM context (an inline
"📷 compacted" divider; the chat never visually restarts). Progress guard:
every automatic pass must land ≤ 80% of threshold or it counts as no-progress
and eventually *blocks* auto-continuation instead of looping. Also handles
Claude's reasoning-extraction classifier hazard (archives for such readers
strip `¶think:` sections) and model-relative readability of compaction
boundaries.

- **Advantages**: lossy-but-unsummarized archive (nothing hallucinated into
  the record; the model reads its own history, not a paraphrase); no LLM
  call, no network — overflow-safe by construction; multiple fidelity layers
  (verbatim text edges + re-derivable source + "re-read files rather than
  guess" guide + `history://` retrieval + recoverable `artifact://` elisions);
  display/context decoupling is exactly right for a GUI app; the
  cache-timed/idle and speculative-trigger ideas are genuinely good.
- **Disadvantages**: requires a vision-capable model *and* image support on
  the wire; ~20k LOC; correctness depends on brittle per-provider billing
  tables; a small local model reading 80 dense PNG pages is unproven
  (omp's own evals targeted frontier models); enormous surface for one user's
  laptop agent.
- **Difficulty**: **very high** as designed. But its *concepts* port down:
  the divider-card UX, the progress guard ("must land under the recovery
  band or treat as failure"), speculative/background compaction, and shake-
  style recoverable elision are each independently implementable.

---

## 3. Cross-cutting components ranked by value-per-line for mini

Ordered by what a local, bash-only, GUI, single-user agent gets most out of,
cheapest first:

| Component | Seen in | LOC ballpark | Pays off for mini? |
|---|---|---|---|
| Observation head/tail capping at record time | mini-swe-agent | ~20 | **Already done** — `model/view.go` caps the model's view to a 100-line/10k-char contiguous tail; full output lives in the session's tcr result file, reachable via `tcr_path`/`tcr_offset`. |
| Free tool-output pruning under pressure (placeholder text, re-run hint) | zero `prune.go` | ~200 | **No** — retroactively rewriting cached history trades free prefix tokens for re-prefill plus rediscovery; only defensible when the window is the binding constraint, which a 262k–1M window is not. |
| Mid-run trigger at tool-loop boundaries + hysteresis | little-coder watchdog, zero | ~50 + ~30 | **Yes** — mini's runs are exactly the autonomous long-turns shape where boundary-only triggering fails. |
| Split-point cut that never separates a call from its result (cut only at assistant boundaries; kept tail contiguous to leaf) | pi, zero, omp | ~60 | **Yes** — mini already orders turns and can reuse the tail as-is; complements (not replaces) placeholder settlement. |
| Structured summary prompt + previous-summary UPDATE prompt | pi, qwen-code, crush | prompt text | **Yes** — this is the summarizer contract; pi's section skeleton is the best starting text. |
| Summary stored as transcript row, replayed as user role | crush, pi, qwen-code | ~30 | **Yes** — maps 1:1 onto mini's store + `requestBody` filter design; the assistant-stored/user-replayed trick is proven twice. |
| Cache-prefix discipline (never rewrite history mid-session; compaction rewrites only the dynamic tail) | zero, qwen-code, omp | policy, ~0 LOC | **Yes — and for mini it is the first principle, not a refinement.** sushi KV-cache reuse is load-bearing for mini's latency budget; this constraint vetoes most of the incremental-shrink catalog above. |
| Cache-*reading* summarization (side-query = same prefix + appended directive, so the ~1M-token prefill is a cache read, not a recompute) | qwen-code cache-sharing path, crush (same affinity/system prefix for the summarize call) | ~50 | **Yes — this is the decisive trick at mini's scale.** It makes the compaction event itself nearly prefill-free; only the post-compaction rebuild pays. |
| Separate cheap summarizer model with fallback | zero, qwen-code | ~120 | **No** — a second model means a second, cold prefix; at mini's scale the cache-reading trick (same model, same warmed prefix + appended directive) beats any prefill-cost saving a separate summarizer could offer. |
| File-op ledger / preserved-state block in summary | pi, zero | ~100–800 (zero's is elaborate) | Medium — mini has no plan/todo tools yet; a compact "files touched" ledger is the 10% of zero's block worth stealing early. |
| Disk/state restoration attachments post-compaction | qwen-code | ~900 | Low now — mini can `cat` any file it needs; re-embedding bytes mostly costs prefill. |
| Provider error-string overflow classification | pi (~25 regexes, incl. llama.cpp), zero (10) | ~80 | Yes if sushi's own overflow error phrasing is stable — one regex suffices for one server. |
| Microcompaction on idle/size with low-watermark | qwen-code | ~1 100 | **No** — same cache-destruction objection as retro-pruning. |
| Snapcompact-style image archive | omp | ~20 000 | No for mini (the mechanism is omp-shaped, and shake-style elision covers the niche) — but the old objection "small models may not decode dense pixel-font PNGs" is **dead**: user-reported experience has the local model reading omp's snapcompact archives fine. |
| Circuit breaker + inflation guard on the summary | qwen-code, omp progress-band | ~100 | Yes at v2 — prevents the post-compaction re-inflation loop little-coder hit as #68. |

---

## 4. Recommendation for mini

### The two deployment facts that decide it

1. **The window is not the scarce resource.** The model has 262 144 native
   context, usually YaRN-extended ×4 (~1 M). Multi-hour sessions are the norm
   the design should assume; the first compaction of a session may be hundreds
   of turns in.
2. **Prompt-cache reuse is the deciding factor.** Every token already in
   sushi's warmed prefix is free; every edit to history at position *k*
   re-prefills everything after *k* and destroys the state the model had.

Together these **veto the incremental-shrink school** (zero's prune ladder,
qwen-code's microcompaction, little-coder's 70–80% auto-compaction): all of
them fight window exhaustion, which is not mini's problem, and all of them
pay for it in cache rewrites and rediscovery, which *are* mini's problem.
The correct posture:

> **While a session lives, its context is append-only and never edited.
> Compaction is a rare, singular, deliberately cache-*exploiting* event.**

This is also a continuation of decisions already made this session: canonical
tool-call IDs at the store boundary with verbatim wire replay, positional
placeholder fills, and stable `requestBody` projection are exactly the
byte-stability discipline that keeps the sushi prefix warm turn after turn.

### What compaction looks like under those rules

**Trigger — near the edge, not early.** Estimate context as
`last reported usage.total_tokens + chars/4 of everything appended since` (pi's
technique; zero's estimator minus the calibration machinery, which only pays
off when compaction is frequent). Fire at a configurable fraction of the
*effective* window, defaulting high (~90%): with a 1M window this happens
maybe once per epic session. Also keep a reactive path that recognizes sushi's
own context-length rejection and triggers the same code once. Two user-facing
knobs worth exposing because they are judgment calls, not engineering calls:

* **effective window = native vs YaRN.** Compacting at 262k instead of 1M is a
  *model-quality* lever (beyond native extent, YaRN-degraded attention and
  quadratic-cost decode), not a capacity one. It should be configuration, not
  code.
* The whole scheme presumes sushi can *hold* the warmed prefix of a ~1M-token
  session (quantized-KV work pending). If cache capacity ever makes the prefix
  re-prefill anyway, the calculus flips back toward compacting early and
  keeping the re-prefillable prompt small — worth revisiting then, not now.

**The summarize call must READ the cache, not fight it.** Do not build a
second "compaction prompt." Send the *byte-identical live prompt* — same
system prefix, same message array — with exactly one appended user turn
holding the summarization directive (qwen-code's cache-sharing side-query;
crush routes its summarize call with the same session affinity for the same
reason). At mini's scale this is the difference between a compaction costing
a 1M-token cold prefill and costing a few hundred tokens of real compute.
Validity gates before accepting the result: no tool call in the response,
well-formed summary envelope, non-empty after stripping the scratch block.

**Rebuild — pi split point with a fat tail.**

* Keep the last **~20–50k tokens** of turns **verbatim** (omp ships
  `keepRecentTokens = 20 000` and users run local models comfortably up to
  ~50k; mini's default should land in that range, as config). The tail's job
  is recency of in-flight work only — durable state lives in the summary and
  the tcr ledger, so fatter tails buy rediscovery-prevention at the price of
  a bigger one-off rebuild prefill and a larger attention-degraded working
  set beyond the native window. (pi's defaults were *broken* on 32k models
  because `keepRecentTokens` exceeded the headroom its trigger left; the
  parameters must be checked against each other whatever the window size.)
* Cut only at a **completed assistant boundary** — a turn whose whole
  placeholder set is filled and whose tool results are on disk — so the kept
  tail is contiguous and replayable with zero repair (the pi/zero/omp
  consensus rule; mini's slot machinery makes legality checkable).
* Summarize the dropped middle with pi's structured-section prompt and its
  UPDATE-on-previous-summary prompt for repeat compactions (crush's rolling
  re-summarization — re-summarizing the old summary plus everything since —
  is exactly the quadratic re-read this design is trying to avoid; the
  *summary-to-summary* hop is cheap here only because the middle region is
  small, but the UPDATE prompt is still strictly better text).
* **The mini-specific gold in the summary**: a ledger of files touched and
  their **tcr result paths** for the dropped span. mini's raw outputs already
  survive on disk (the `View`/`tcr_path` split), so the post-compaction model
  can `sed -n` its own forgotten observations out of the result files instead
  of re-running commands. That is qwen-code's disk-restoration trick for free
  — no re-embedding bytes into the prompt, no cache cost until the model
  actually asks.

**Placement & persistence — crush-style, in one row.** The compaction is a
first-class transcript record (summary text, `first_kept_turn_seq`, tokens
before/after, model used). `requestBody` anchors at the latest compaction:
`[system + instance prompt] + [summary projected as a user message] +
[kept tail verbatim] + [everything appended after]`. Original history is
never mutated (already true of the store). Resume needs nothing new — it
rebuilds the same projection, byte-identical to the live prompt, so a resumed
session hits sushi's cache exactly where the killed one left off. This is the
natural landing in the in-flight transcript/context split: the compaction row
is a transcript record *about* the transcript, like exit rows — visible to
the GUI, invisible except as the anchor to the wire.

**GUI.** A compaction **card** in the scroll (omp's divider pattern): the
visual transcript never restarts; the card summarizes "session compacted,
N → M tokens" and expands to the full summary text. Manual "Compact now"
uses the identical path with `force=true`.

**Guards (all cheap, because the event is rare).** One-shot overflow retry
(pi); if a compaction does not land under the band, fail loudly rather than
loop (omp's no-progress → block-and-report); after a compaction, append a
short steering line to the summary's user wrapper — little-coder's "do NOT
re-read files you have already read" — and *mean it differently here*: also
"raw earlier outputs remain on disk at the listed tcr paths." Anti-thrash
hysteresis (zero's low-water mark) degenerates to a no-op when compactions
are this rare, but costs ~5 lines, so keep it.

### Build order

1. `requestBody` anchor: projection from the latest compaction row (schema:
   one table or one extra record type in the session store — no migration
   burden, matching the earlier no-sweep decision).
2. Cache-sharing summarize side-query + result validity gates + the
   structured/UPDATE prompts.
3. Split-point selection at completed-assistant boundaries with tail budget
   from config; tcr-path ledger extraction.
4. Threshold check at tool-loop boundaries (mid-run, little-coder's lesson:
   mini's autonomous runs never touch a user-prompt boundary) + reactive
   sushi-overflow path, one-shot.
5. Compaction card in the GUI + manual force path.
6. Guards: progress band, steering line, low-water mark.

Deliberately **not** built: prune stages, microcompaction, speculative
background compaction, separate summarizer model, image archives. The first
three rewrite or race the cache; a separate summarizer model has its own
cold prefix (defeating the cache-reading trick, which is the whole point on
local hardware); snapcompact is omp-shaped machinery whose niche mini's tcr
spill files already fill — though it is worth knowing that the local model
reads omp's frames fine, so the idea is not disqualified by its reader, only
by its ~20k-LOC cost. If the session
ever grows past two compactions, the middle-to-summarize is small and the
UPDATE hop is cheap — the rare-event design makes most of qwen-code/zero's
sophistication unnecessary rather than just deferred.
