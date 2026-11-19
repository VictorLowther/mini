# How other harnesses put control events on the wire

Surveyed 2026-11-18, to settle one question raised by a display bug: does an agent
harness need to *narrate* turn structure - "The user added a new task: X",
"Interrupted by user: X", "Commands not executed..." - in the messages it sends the
model, or is that carried by `role` and the tool-call protocol?

Subjects: `crush` (Go), `zero` (Go), `mini-swe-agent` (Python, mini's ancestor),
`little-coder` (TS, a pi extension pack), `oh-my-pi` (TS/Rust), `qwen-code` (TS),
and `sushi` (Zig) as the chat server mini talks to. All paths below are in
`~/src/llm_stuff/<repo>`.

**Method, and its limits.** Six subagents, one per repo, each with the same
seven-event brief; then this session verified the claims that the decisions rest
on by opening the cited lines itself. Verified here personally: crush
`settle.go:16-22`, `content.go:172-176`, `agent.go:1352`; qwen-code
`core/coreToolScheduler.ts:1271-1300`, `client.ts:258`; little-coder
`scripts/patch-pi.mjs:37-60`, `quality-monitor/index.ts:41`; oh-my-pi
`ai/src/types.ts:111,1243`, `session/messages.ts:762-766,824-832`; zero
`internal/providers/` (`IsError`: **0 hits**), `agent/loop.go:925-928`;
sushi `chat.zig:82-89,1439-1451`, `server.zig:3300-3303`,
`tool_calls.zig:2859-2861`; mini-swe-agent `litellm_model.py:76-77`,
`interactive.py:131-138`. Everything else is subagent-reported with a file:line
that this session did **not** re-open - the `docs/ux-audit-2026-11.md` §9 lesson
applies: check a citation before building on it.

## Verdict

**Turn framing is not narrated by any harness surveyed except mini and its
ancestor.** The literal `The user added a new task:` exists only in
`mini-swe-agent/src/minisweagent/agents/interactive.py:159` and its Go port;
searched-and-not-found in the other five. Two corrections to the naive version of
that conclusion, though, both of which matter for what mini should do:

1. **The server enforces the role *vocabulary*, not the turn graph.** sushi
   canonicalizes `developer→system` and `function→tool` (`src/chat.zig:82-89`) and
   returns `400 invalid_request_error` for an unknown role - but *only on
   `/v1/chat/completions`* (`src/request_parse.zig:453-455` →
   `src/server.zig:2290-2293`). `/v1/messages` does no role check at all and drops
   unknown roles silently; `/v1/responses` passes them to the template, where an
   error causes a **silent** ChatML fallback (`chat.zig:992-999`). There is **no
   tool-pairing check** (`tool_call_id` is read only when `role == "tool"`,
   `server.zig:3300-3303`) and **no alternation requirement** anywhere - so an
   unanswered tool call is the harness's problem. That is *why* every harness
   synthesizes a tool result for a call that never ran: not because the API
   narrates it, but because the API will not fix it. pi goes furthest and pads a
   literal `Continue.` user message when Anthropic demands alternation
   (`packages/ai/src/providers/anthropic.ts:6310,6337`).
2. **Structure that does not cross the wire cannot replace prose.** zero's
   `Message.IsError` is read by **no** provider (`grep -rn IsError
   internal/providers/` → 0 hits); its denials reach the model as the text
   `"Error: ... was not executed."`, and the structured `Meta[policy_refusal]` /
   `DenialCategory` fields exist so the *harness* never has to re-parse that prose
   (`loop.go:2252-2286`, "STRUCTURED ONLY... Nothing below reads Output"). So the
   consensus is not "say nothing in words"; it is **which slot, and in whose
   voice**.

## What each event becomes

| Event | crush | zero | oh-my-pi | qwen-code | little-coder | mini-swe-agent |
|---|---|---|---|---|---|---|
| Turn interrupted | tool `is_error` + `StopTurn`; UI-only text | **nothing in-array** (partial turn not appended) | assistant `stopReason:"aborted"`; rescue note → developer | nothing for bare Esc; tool for cancelled calls | nothing - *deletes* pi's abort marker | **user, narrated** |
| Call that never ran | `settle.go:16-22`, one per cause | tool `IsError` | toolResult + `details.__synthetic`, 4 distinct causes | `functionResponse.error` | toolResult `isError` + `reason` | tool (`rc -1`) **and** user prose |
| Steering | user, verbatim | no mid-run path | user, verbatim + per-request `<system-notice>` | user, verbatim + `Steer` tag | user, `deliverAs:"steer"` | does not exist |
| Permission denied | tool: `"User denied permission"` + `StopTurn` | tool + `Meta[policy_refusal]` | toolResult `isError` | `response.error` | toolResult `isError` | **user prose** |
| Malformed call | tool error | **user** directive | developer | `response.error` | user steer directive | **user prose** |
| Continue after rest / resume | no wrapper; post-truncation restates *session state* | digest: `"Previous session context:"`, `"Current user request:"` | user slot + **`attribution:"agent"`** | `<summary>` + synthetic model ack | followUp: `"Your context was automatically compacted mid-task..."` | **user, narrated as a new task** |

## The rule the six converge on

**You may narrate what happened to a call - including that a human refused it -
inside that call's result. You may not fabricate a human turn.**

Corollaries visible in every codebase:

- **User-slot prose is for directives addressed to the model, never reports about
  the human.** zero's nudges ("Your previous response had no visible output...",
  "You stopped without calling a tool, but the task is not finished"), pi's
  "You were saying this but I interrupted you:", qwen's "Do not respond with
  text." - all second person, all about the model's next action. Even sushi
  injects `tool_choice` into the last user turn as a directive
  (`chat.zig:1439-1451`, `tool_calls.zig:2859-2861`).
- **When harness text must sit in the user slot, say so structurally.** pi's
  `MessageAttribution = "user" | "agent"` (`ai/src/types.ts:111`) is orthogonal to
  role and consumed by providers (`agentAuthored = synthetic === true ||
  attribution === "agent"`); qwen wraps injections in `<system-reminder>` and has
  `isSystemReminderContent` to recognize them (`environmentContext.ts:235-237,
  886-931`); crush tags `<user_notes>`/`<host_context>` and computes
  `IsPrompt()` - "is this something the user typed" - from the parts
  (`content.go:172-176`); little-coder uses `customType` + `display:false`
  (`_shared/inject.ts:64`).
- **Narration about a human, when it exists at all, lives in the tool result.**
  pi: `"The user did not answer the approval prompt ... NEVER retry unless the
  user asks."` crush: `"the call was never run; retry the call if the result is
  still needed"`. That is the *only* place third-person user references appear.
- **Persist verbatim, derive at the boundary.** pi stores the human's text with
  zero narration and applies emphasis per request as a pure function of the
  message, memoised in a WeakMap because "a fresh wrapper each request would read
  as a never-sent turn", then strips the flag before the model sees it -
  documented "**Never rendered**" (`messages.ts:824-832,762-766`,
  `types.ts:1243`).
- **Mind the ordering.** zero: "a user message between tool_results breaks strict
  provider replay" (`loop.go:925-928`), so user-role content is appended after
  every tool result of the turn.

## Ancestors and accidents

`extra["interrupt_type"]` is **never model-facing** in either the ancestor or
mini: every litellm-based model strips it
(`{k: v for k, v in msg.items() if k != "extra"}`, `litellm_model.py:76-77`) and
mini does the same at `model/sushi.go:296-297`. It is trajectory metadata for
front ends - which is why the view must not have to *guess* authorship from it,
and what `block.typed` fixed.

Three of these strings were also load-bearing for a reason the ancestor no longer
has. `The user added a new task:` exists in the ancestor only at the *ready
prompt* (`interactive.py:159`); the ancestor has **no resume path** at all
(`run()` clears history, `default.py:91`). mini's second use of it, at
`agent/agent.go:1378`, is not parity - it is a new job for an inherited string,
in a code path the ancestor never had.

## What mini changes as a result

Recorded as `docs/parallel-sessions.md` §8.15, implemented in the commit that
follows this note:

1. **Ready-line text is sent verbatim.** The wrapper asserted a task boundary the
   UI has outgrown and carried no fact the role did not (decision 1).
2. **An interruption comment is the human's words, unprefixed.** The cancellation
   is already structural: mini pads every un-run action with
   `NotExecutedResult()` (`model/actions.go:198`) and `agent.go:1732-1734` appends
   those observations *before* the interrupt's messages, so the "your turn was
   cut" fact reaches the model in the tool slot regardless (decision 2).
3. **Rejection moves to the call's result.** Each refused action's observation now
   carries the reason and the don't-retry-unchanged instruction, via a new
   `model.Interrupt.Observations`; the human's comment, when there is one, stays a
   verbatim user message. mini was paying for the same fact twice - `rc -1` in the
   tool slot and a narrated user turn (decision 3).
4. **Resume sends the task verbatim; the restatement is dropped.** Decided in
   implementation, not in advance: any framing sentence in that message would be
   either a false claim of authorship (the bug just fixed) or harness prose in the
   human's slot, and the honest third channel - a system note - has no reliable
   position, because sushi folds every system message after index 0 into the head
   (`chat.zig:95-127`). crush and zero both restate *session state*; if the model
   turns out to need that for resumed runs, it belongs in the system template
   rendered for the resumed session, not in a user turn (decision 4).

**Not done, deliberately:** the human-facing prompt copy still says
`"Agent is ready. Type a new task."` (`ui/slash.go:70`) - prompt wording with its
own test and README surface, not a turn-structure question.

Worth noting how the decisions land: mini's `instance_template` is a bare
`{{.task}}` (`config/configs/mini.yaml:164-165`), so a **fresh** run already sends
the human's words verbatim with no framing. The narrated wrappers only ever applied
to the turns that came after - which makes them an inconsistency rather than a
convention, and decisions 1, 2 and 4 simply make every turn look like the first
one. Print mode is unchanged: it labels every user-role row `User:`
regardless of type, and remains the "cannot drift" gap
`docs/ux-audit-2026-11.md` §7 records.
