# Current UX: structure, layout choices, idioms, design decisions

Reviewed: `ui/` - then ~5,535 non-test LOC, now **5,935 across 16 files**
(`find ui -name '*.go' ! -name '*_test.go' | xargs wc -l`, measured after §10;
the bubble rewrite and `mockup.go` account for the growth) - against
`docs/parallel-sessions.md`
(3,001 ln then; it carries §8.13 now), `jobs-research.md`, `whitelist-gate-hardening.md`,
`adversarial-review-2026-11.md`, and ~4,000 lines of tests. Fyne
`v2.8.3-0.20261115175655` (a **dev pin**, not a release - see §5.6 and §7).

Line numbers refer to `ui/fyneview.go` unless another file is named. This is a
survey of what exists and why, written before a round of UX changes: no
recommendations, and §6 lists the ideas the design has already rejected.

**Two of those changes have since landed - read §10 and §11 first.** They record
what was rewritten in place (the bubble paints an outline, not a tint; and which
side a card sits on is who spoke, not what the wrapper is named), what is now
historical, and how far the `fyneview.go` line citations have drifted.

---

## 1. Structure - four owners, two front ends, one seam each

The code splits **by ownership, not convenience** (`shell.go:19-30`):

| Layer | File | Owns |
|---|---|---|
| `shell` | `shell.go` (644) | The window: app, `DocTabs` strip, menu bar, the one 1-second tick, sticky state, quit. "Only the shell may quit the app, and only the selected session may move the window's title or take the keyboard." |
| `GUI` | `fyneui.go` (738) | **One session == one tab.** Transcript state, pending question, ask queue, streaming buffers, run ctx. |
| `guiView` | `fyneview.go` (2,106) | Every widget of one session. "Never `fyne.Do` from inside a view method" (`:59-62`). |
| `block` | `blocks.go` (245) | The transcript model both front ends render from - "the string shapes are defined here so no two surfaces can drift." |

**The interface** (`ui.go:68-80`) is five methods: `Reasoning`, `Content`,
`Message`, `Ask`, `Interactive`. Progress for in-flight work arrives through two
*optional* interfaces asserted at the call site - `Issuer`
(`IssueCalls`/`SettleCalls`) and `callObserver` (`CallStarted`) - deliberately
**not** on `UI`, because "forcing an empty method on it and on every test fake to
satisfy an interface nobody calls it through is the dead-code shape this package
has been deleting elsewhere" (`ui.go:90-93`). Same reasoning for
`OnCallOpened` (`interactive.go:267-272`). This is a house idiom: *capability
arrives as an optional seam, and its absence is rendered honestly rather than
faked.*

**Everything crossing into the widget tree crosses one seam:** `GUI.post` ->
`shell.post` (a `syncs.Func` from `fyne.io/fyne/v2/internal`, defaulting to
`fyne.Do`). Eight distinct goroutine classes exist (canvas, per-session run,
model streaming, ticker, Go timer, opener, closer, spawn) and the rule is
uniform - *do non-widget work on the calling goroutine, post only the widgets.*
`Post` is installed at construction, not assigned afterwards, because "building
the window already schedules a deferred recompute, and that callback reads the
field - which made the assignment itself a data race" (`fyneui.go:70-72`). Two
mutexes cover the shared session/window state: `GUI.mu` (never held across a
`post` or widget call) and `shell.mu`, which by documented rule guards **only**
`thinking` and `quitting` - `sessions`/`tabs` are toolkit-goroutine-confined
(`shell.go:43-44`). `askQueue` and `LogTarget` keep their own small locks
(`askqueue.go:33`, `logtarget.go:21`) - `sessions`/`tabs` "belong to the toolkit goroutine like the widget
tree they mirror" (`shell.go:40-44`).

**Prompts are serialized per session** by `askQueue` (`askqueue.go`) - a ticket
queue rather than a mutex, "for one reason: a waiter must be cancellable", with
strict FIFO "which a mutex does not promise and a human watching prompts
reasonably expects."

---

## 2. Layout choices

### 2.1 The window

One window per process (`a.NewWindow("mini")`, `shell.go:102`); sessions are
tabs, never extra windows. `ShowAndRun` is never used - `window.Show()` then
`app.Run()` (`shell.go:204-222`), so a startup error can show a window
(`GUI.ShowError`). Root content is *only* the `container.NewDocTabs()` with
`TabLocationTop`. **No icon, no custom theme, no DPI/scale code anywhere.** Size
is restored from the store and applied *before* Show so the window "opens at the
size it closed at instead of cascading one resize in front of them"
(`shell.go:133`); saved size is `Canvas().Size()`, "the honest measurement"
(`:327-329`). Both `SetCloseIntercept` and `SetOnClosed` are bound to `quit`,
because glfw runs the intercept *instead of* closing (`shell.go:135-139`).

### 2.2 The session column

```
Border{ bottom: VBox[ status Label, chatInput ] }{ center }
center = Custom(centerLayout){ Scroll(transcript), Card(panelScroll) }
```

`centerLayout` is the load-bearing one (`:601-634`): it is a hand-rolled vertical
split because Fyne's own split containers have a `MinSize` that would grow the
window. The question card keeps its natural height; the transcript gets the rest.
The governing constraint, stated at `:599-603`: **"the glfw driver resizes the
window up to the content tree's MinSize (fitContent), so nothing inside it may
report a minimum that follows the transcript's length."** The entire layout is
shaped by that one fact.

### 2.3 The virtualized transcript

Fyne "walks the whole object tree every repaint frame ... at frame rate no matter
what changed - so the live tree must stay proportional to the viewport, not to
the session" (`:117-120`). Rows are materialized from `blockH` cumulative heights
with a **one-viewport margin above and below** (`recomputeWindow`, `:410-455`;
the margin is why `winLast` can be `len-1` with the tail pinned). Two transparent
rectangles stand in for off-screen history; `topPad`/`botPad` are the only
permanent objects.

- `virtualLayout.Layout` positions rows from `winRows` and **writes each row's
  measured `MinSize().Height` back into `blockH`** (`:232-234`) - estimates only
  steer until the first real measurement.
- Rows enter and leave one at a time at the edges; survivors keep their widgets,
  textures and rasterized glyphs (`setWindow`, `:456-501`).
- Card width = `viewport - scrollbarGutter()` where the gutter is
  `ScrollBarSize()+Padding()` (`:316`) - because the transcript bar and a card's
  own bar would otherwise stack in one column and "grabbing the transcript bar is
  a coin flip on which scrollbar answers."
- `atBottom` has **no negative tolerance** (`:802`): overscroll reads as a genuine
  hold release, and the tolerance is one line, not a pixel.

### 2.4 The card (`area`) and the bubble

Every transcript row is now an `area` - a ruled change: *"all of the cards should
be real containers instead of being just rich text widgets with background
colors"* (§8.8). Composition is
`Stack[ bubble, VBox[ Border{trailing: actions}(header), body ] ]`
(`areaCard`, `:1419-1448`).

The visual reference is still explicit - "speech bubbles in the style of
apptrix.org/components/widget/chat ... a rounded body with a tail flaring to a
point on the sender's side" (`:1200-1233`) - but **the kind is now the stroke**,
since §10. The bubble is one `canvas.ArbitraryPolygon` (`:1302`) carrying
`FillColor`, `StrokeColor` and `StrokeWidth`: the primitive strokes its own path
in this Fyne pin, in both painters, so there is no second inset shape faking a
border. The interior is the plain theme surface for every kind (`bubbleSurface`,
`:1254`); the hue lives entirely in the frame (`bubbleOutline`, `:1261`), at full
intensity and `padding/2` thick with a 1 pt floor (`bubbleStrokeWidth`, `:1274`).
The five hues are the ones the tinted design used (`bubbleAccent`, `:1234`) and
everything without a hue of its own takes `bubbleNeutral` (`:1247`). The reason
they are fixed hues and not theme names has not changed: "ColorNamePrimary (user)
and ColorNameHyperlink (LLM output) are both blue in the default theme."

What the fills were, and why they went: `bubbleFill` blended each accent onto the
background at `0x33` (the five labelled kinds), `0x2a` (`LLM output`) or `0x18`
(unknown) and `blendOnto` composited it; both are deleted, one client each and
the client gone. The visible edge was fill-vs-background contrast, which asked
the reader to decode a surface hue *through* the text sitting on it, produced
near-identical tints side by side, and left every unlabelled kind in the same
9.4% wash (§7). Opaque survived the redesign as a property of both halves rather
than disappearing with the tint: of the interior, so the scroll's stand-in
rectangles do not show through the body, and of the stroke, because a zero-alpha
hue paints an invisible frame while every field reads correct - which is exactly
what the old comment about low-alpha fills rendering "effectively invisible" was
guarding, and what `A: 0xff` on every accent literal now guards.

Sides and labels come from one table, `messageKindLabel` (`:1165-1190`, first
match wins): `transcript>0` -> **tool call**; `assistant` -> **LLM output**;
`user` -> **user** right-justified when the human spoke it, **harness** left when
the harness wrote the sentence. That side split is the README's rule "so the two
never read alike." Who spoke is **carried**, not inferred - `block.typed` from
`extra["user_typed"]` - because the previous rule was
"`subtype` in {"", `UserSteering`} -> user, everything else -> harness", which
made every human-typed kind that had not been enumerated into a harness card. See
§11. Headers use the *body's own font* (`TextStyle{}`), never a
label widget, so header width tracks body width exactly, and the *plain theme
foreground* for every kind (`areaColor`, `:1413`) - a decision since §10, with
`TestGUIAreaHeadersAndAlignment` asserting it, because a function that takes a
kind and ignores it is indistinguishable from unfinished work without one.

### 2.5 The numbers (every knob in one place)

| Knob | Value | Site |
|---|---|---|
| **Master unit** | `theme.SizeNamePadding` (=4 by default) - everything spatial is a multiple | |
| corner radius | x1 | `:1317` |
| tail drop / flare base | x1.5 / x2 | `:1271, :1275` |
| card insets | top p, bottom 2.5p, left p, right p | `:1436` |
| **outline width** (since §10) | `padding/2` (=2 pt at defaults), floored at 1 pt | `:1274` |
| **bubble interior** (since §10) | theme background, uncoloured, opaque | `:1254` |
| tip softening | `min(radius/4, 1)` - one of two hard pixels (the other is the outline floor) | `:1319` |
| mono card growth cap | `min(content, 10 lines)`, measured from glyph `"M"` | `:1509-1519` |
| code fold threshold | 6 lines open, >=7 accordion `[lang . N lines]` | `markdown.go:24-28` |
| fence split | >=3 markers, indent <4 opens | `markdown.go:78-81` |
| live repaint gap | 80 ms leading-edge, clamped >=0 | `:88` (field), `:182` (set) |
| height estimate | `lines x TextSize x 1.5 + cards x 4p + 2p` | `:321-331` |
| pre-show stand-in viewport | 640 px | `:428` |
| arming window (quit, tab x, End) | 10 s | `shell.go:376`, `:368` |
| tick | 1 s, one timer for all sessions | `shell.go:240` |
| exit grace | 2 s | `shell.go:227` |
| window size guard | [200, 65536] both sides; +/-0.5 pt compare | `prefs.go:66-67`, `:102`, `:127-133` |
| settings call bound | 2 s "so a locked DB costs a slow keystroke, not a hung window" | `session/settings.go:30-32` |
| default window | 960x720 | `prefs.go:59` |
| panel action elision | 4 shown then `+N more commands`; 6 lines per action | `blocks.go:205-234` |

Header text carries **no width logic at all**: `cardHeaderText` (`:1452-1464`)
builds `[]string{label}` and appends each non-zero metric (`%d tok`, `%.1fs`,
`$%.2f`), joined with U+00B7. Truncation, wrapping and per-field elision do not
exist here, so a long compaction title or a five-part metric run can wrap the
header line - the only budget is the card's own width.

### 2.6 The live row and the question panel

The streaming turn is **a row of the same virtual scroll**, not a docked region:
"a separate docked region made the transcript's viewport height change as the
stream grew (the scrollbar jumps under the reader); sharing one scroll means one
region whose size only the window controls" (`:108-113`). Closed markdown blocks
are parsed once and kept; only the open tail re-renders per flush, with an
unterminated fence closed for the preview "so the reader sees code, not the
syntax of a block that is not finished yet" (`:1643-1644`).

The panel belongs **to questions only** (§8.11 killed the tool-call panel: it
"competed with the transcript for the center column, its MinSize could push the
window taller than the screen, and while it was up the tab strip could not be
reached"). Two shapes exist: `AskConfirm` -> title + monospace action list + dim
hint line; everything else -> **title only**. There are no buttons anywhere in
the panel - the answer channel is the chat input.

---

## 3. Interaction inventory

**Input rules.** Enter sends; **Shift+Enter** (and Option+Enter on macOS, and
Cmd/Ctrl+Enter via a separate mechanism) inserts a newline - "multi-line input is
a keystroke here, never a mode the user can get stuck in" (`:2099-2101`).
Up/Down are deliberately *not* history: there is **no submit history, no draft
save/restore, and `submit()` clears the entry unconditionally before dispatch**
(`:2102-2106`). The entry is never disabled or read-only; running state changes
only what submitted text *becomes*.

**Chords.** Esc (cancel - three outcomes, silent at idle because "leaving is
deliberate"); bare `y` in an empty entry confirms a pending `AskConfirm`
(lowercase only; uppercase falls through and becomes a rejection comment);
Control-Command-T on macOS / Ctrl+T elsewhere toggles thinking - plain Cmd-T is
*left free "for future tabs"* (`:35-43`). **Option/Cmd+Enter variants and the
`View > Show thinking` menu item have no tests.**

**Menu.** Run (Cancel turn, Compact now, Cmd-Q Quit) - Edit (Cmd-C Copy, see
§5.6) - Session (Option-arrows cycle, Cmd-. pause, Export, Cmd-W close) - View
(Show thinking) - Help (Commands). **There is no "New session" item or
accelerator** - the only path is the strip's `+`.

**Dispatch order** (`handleInput`, `:1806-1870`) differs by kind: with no question
pending, `/quit` -> empty -> `commandOrSteer` -> run-ended (text **dropped**) ->
queue as steering. With one pending, `AskTask` has **no slash handling** (so `/y`
becomes task text), `AskConfirm` runs slash then treats `""` and `/y` as confirm
and anything else as a rejection comment, `AskInterrup` resolves verbatim, and
the rest get the full treatment. The comment claims "Input is never dropped"
(`:1810-1812`) and step 4 contradicts it.

---

## 4. Idioms - the house style

1. **Comments are the design record, and they include the failures.**
   Post-mortems, not just intent: "the first cut used low-alpha fills ...
   rendered effectively invisible"; "That early-return is what left the live
   area's size stale between turns"; "a coincidence that has broken twice."
   Section citations to `docs/parallel-sessions.md` are scattered through the
   code and are the authoritative link between a rule and its origin. There is
   **not one `TODO`/`FIXME`/`HACK` in the entire render path** - deferred work is
   recorded in the docs instead.
2. **Absence over a wrong value.** `theme.ColorNameDisabled` for header colour
   rather than inventing `areaColor(kind)` (`:1344-1349`); no control rather than
   a disabled one (`fyneui.go:45-49`, `:344-348`); a preference nobody expressed
   is never written (`prefs.go:21-28`); a missing job row reads "no longer
   tracked," not "completed."
3. **The honesty rule, cited repeatedly.** "A button that cannot reach the
   registry is the lying affordance §8.6 says not to ship"; `esc` on a parked
   session must not say "cancelling this turn..." because that "would be a lie
   with a note attached"; the footer shows *no* count rather than one that lies;
   `ReturnCode` is never fabricated for a run still going.
4. **Educate the nagging.** The confirm prompt says *why* it asked: "someone who
   is nagged without being told what would stop the nagging turns the gate off
   instead of fixing the pattern - which is how a security control dies"
   (`slash.go:58-68`).
5. **Pure mapping vs view-owned wiring.** `blockCards` is a pure
   `block -> widgets` function; anything needing a capability (the End button) is
   attached in `newRow`, never after the row reaches the canvas - "the whole
   reason there is no refresh path to get wrong here."
6. **Repaint, don't refresh; rewrite, don't rebuild.** `repaint(o)` goes through
   `Driver().CanvasForObject(...).Refresh(o)` "without re-wrapping or re-celling a
   single widget." Materialized rows keep a handle on their text widget
   (`transcriptRow.body`) so a ticking job rewrites in place - eviction "made the
   transcript lurch once a second."
7. **Everything expensive is diff-guarded or coalesced:** paint-gap for
   streaming, one ticker for N sessions, `SetText`/`SetTitle`/tab-label only on
   change, `bubbleRenderer.Layout` early-returns on an unchanged size.
8. **One message = one card.** Both `note()` and `noteLocal()` append one block
   because a per-line split printed "eight bubbles for one help block" and spent
   the row budget.
9. **Stringly-typed kind coupling, no constants.** `"LLM output"` is spelled
   literally at `:1172, :1230, :1565, :1644`; the **outline** is keyed on the
   *display label*, not the block kind, so a rename degrades a card to the
   neutral frame with no compile error. Since §10 that failure is louder than it
   used to be: the label used to key a background tint that sat alongside a
   header naming the same kind, and now it keys the card's only colour.
10. **Small typographic conventions:** U+00B7 middle dot as separator
    everywhere, U+2026 for in-progress, a check mark for exit, warning and pause
    glyphs as tab markers, platform key glyphs in prose, `> ` as the print prompt.

**Test idiom.** The suite injects a headless app through
`GUIOptions.App: test.NewApp()` together with a drainable `Post` queue
(`fyneui_test.go:205`, `:216`; also `blocks_test.go:45`, `prefs_test.go:156`,
`askqueue_test.go:139`). **No driver is patched** - `test.Driver` and
`OverwriteGlfwApp` appear nowhere in the tree, and the README's "runs headless
against an injected test app" is accurate. Assertions land on **model state** (`committed`,
`winFirst`/`winLast`, `pinned`), **rendered text** (`strings.Contains`/`Count`)
and - since §10 - **painted pixels**: `test.NewWindow(o)` plus
`Canvas().Capture()` renders through the software painter with no driver patch,
which is how `TestGUIBubbleOutlinesPaint` knows a frame was *drawn* rather than
assigned. `test.NewCanvas()` cannot do this: it has no painter, so its
`Capture()` returns a blank image, and a test written against it passes while
proving nothing. Geometry is asserted only for scroll extents, card widths and
minima (now including the mockup column's, `TestGUIMockupLeavesScrollbarGutter`).
**No test asserts where a transcript row is *placed*** (row `Move`/`Resize`
geometry is never read back), and `paintGap` is
zeroed in tests so the stream throttle is untested. `MINI_DEBUG_LAYOUT=1` (`:28`)
is the only layout introspection.

---

## 5. Design decisions on the record

1. **`DocTabs`, chosen by reading vendored source, not docs** ("Which Fyne
   container: the check, and why it is DocTabs", `parallel-sessions.md:1226`).
   `AppTabs` has no `CreateTab`, no `CloseIntercept`, **no per-tab close icon at
   all, and no scroller whatsoever.** Four traps documented and designed around:
   the intercept *suppresses removal* (so all teardown funnels through
   `closeTab`); the jump menu is text-only upstream ("settling that now avoids
   designing a glyph system the toolkit cannot carry"); **neither container
   reorders**, so order is insertion order and any reorder needs an explicit
   `Select`; `CreateTab` is synchronous but opening a session is not, hence the
   **picker tab** (also chosen over `dialog.NewForm`, which drags in a
   virtual-filesystem dependency for one field).
2. **Tabs over a roster - by maintainer override** (decision 15). The survey
   recommended a roster/tree; that was rejected, and its virtue was *moved into
   the labels*: `[warn][pause]name . N job(s)`, repainted only when the string
   changes, because "the strip is the only place a session that is not on screen
   can say anything at all." No OS-level notification exists - the warning glyph
   is the whole mechanism.
3. **No second UI over the canvas** (§8.11). In-flight calls became transcript
   cards with a lifecycle (`created`/`running`/`finished`/`ended`), one card per
   call via `tcid` - because "a call that runs for two minutes shows *nothing*
   for two minutes" and "thinking" and "hung" look identical.
4. **Sticking to the working edge is intent, not geometry** (§8.9). `pinned`
   changes **only** on a scroll callback the human caused, and `following` guards
   our own `ScrollToBottom` - because glfw fires `OnScrolled` for programmatic
   scrolls, and a geometry-derived pin "un-pinned a transcript nobody had
   touched."
5. **Attribution is carried, never derived** (decision 5/13) - and §11 is what
   happens where that rule was not yet applied.
   `Question.Origin` -> `displayTitle` prefixes `subagent <name> . `, because
   "an unattributed queue is how a human approves `rm -rf build/` believing they
   pressed enter on their own command." Bubbling became *routing* with tabs;
   "attended-ness follows the front end, not the parent." Everything else a hook
   can be asked **deliberately does not** cross.
6. **Copy is a menu item, not a handler, and the focus trap is the model**
   (§8.2). Mouse-down in any selectable widget moves focus to an internal
   `*widget.focusSelectable` whose `TypedKey`/`TypedRune` are no-ops, and glfw
   dispatches a shortcut to the focused widget and *returns* - so only main-menu
   shortcuts fire first. Consequence accepted: the highlight vanishes on Cmd-C,
   "so Cmd-C *is* the 'copied' feedback; both halves cannot be had." Copy must
   use `app.Clipboard()`; `window.Clipboard()` is "a single-use stub in the test
   driver, where writing it is silently invisible."
7. **Mono bodies are one `RichText`, not `TextGrid`** - a cell grid is "a canvas
   text and a rectangle per *cell*, walked every frame," "measured at roughly a
   fifth of a core sitting idle."
8. **A ~10-line card cap, hard-coded**, so long output "scrolls inside the card
   instead of pushing the transcript (and the model's attention) around it."
9. **Sticky state goes in the DB's `settings` table, not the config**, "which is
   run configuration that scripts and `-c` overrides depend on and `--print`
   should not silently rewrite." **Placement is deliberately *not* remembered** -
   Fyne's `Window` has no position getter or move callback, `RequestPosition` may
   be ignored (Wayland), and a stale value can park a window beyond an unplugged
   monitor.
10. **Limits prompts are retired wholesale** (decision 14): a step/cost cap
    "became a second, invisible author of the report." `AskLimits` etc. are
    *retired, not deleted*, "because old transcripts carry them ... deleting
    those would erase history to remove a feature." The whole GUI dialog
    subsystem turned out to exist only for that prompt - "an abstraction with one
    client, whose client is gone, is not an abstraction."
11. **Leaving is armed everywhere** - Cmd-Q notes in **every** busy session, the
    tab close glyph likewise, and the End button arms to "End it?", because
    "ending a job that has been training for twenty minutes must not cost a brush
    past a button."
12. **`mode` fails closed**: only `"yolo"` and `"human"` skip the prompt, because
    the old inverted test made every misspelled mode a *silent* yolo (A-2).
13. **Failure becomes a transcript line.** `Host.Start`'s error is the window's
    only content; `RetryNotice` exists because "a stalled window with no
    explanation is indistinguishable from a crash"; unstreamed errors "must land
    in the transcript or they vanish."
14. **A kind is carried by its outline, not its background** (2026-11-18; §10 and
    `parallel-sessions.md` §8.13). Same five hues at full intensity, `padding/2`
    thick; interior is the plain theme surface, opaque so what paints under a row
    does not show through it. One `ArbitraryPolygon` rather than two stacked
    shapes, because the primitive strokes its own path in both painters - GL
    inward from the path, software centred but padded by `VectorPad`. Every hue
    literal states `A: 0xff`, because a zero-alpha accent is an invisible frame
    with every field correct, and the suite has now pixel-asserted that
    distinction. Headers stay foreground, and a test says that is chosen.

---

## 6. Explicitly rejected - do not re-propose without new evidence

Twenty-two items are on the record; the ones most likely to be re-suggested by a
UX pass:

- **A session roster/tree instead of tabs** - overridden by decision 15; the
  remaining gap ("one screen listing every session at a glance") is recorded as a
  deliberate trade, not an oversight.
- **An icon/glyph status system in the tab strip** - the toolkit cannot carry it.
- **A bottom panel for running calls** - deleted by maintainer report (§8.11).
- **A background-tasks `/tasks` dialog** - "the footer count is 90% of its value
  at 2% of its cost."
- **`job_send` as a visible feature** - "multiplies the states the GUI must show."
- **Any confirm prompt on `job_kill`, including `all: true`** - "makes the
  mitigation slower than the problem it mitigates."
- **Any limit/budget dialog, including rewording "Agent is ready"** - decision 14
  and §9.10 withdrew four drafted label changes.
- **Auto-killing or auto-waiting a child's leftover jobs** - visibility plus a
  human `End` control is the ruling (decision 10).
- **Remembering window placement** - API gap, §5.9.
- **A disabled or unwired placeholder control** - "the lying affordance §8.6
  refuses to ship."
- **Polling `Process.Children()` to open child tabs** - "it would work and is
  wrong: it makes the UI guess."
- **`timeout_s` clamping** - "a ceiling on a training run is not a guardrail; it
  is a bug that fires at 3 a.m."
- **Typed response metadata feeding the job card** - the card reads the row and
  the file itself, "which means the job card is a real component."
- **`dialog.NewForm` for the `+`** - replaced by the picker tab (dependency cost).
- **Tinted bubble fills as the kind marker** - retired by §10 in favour of
  outlines (§5.14). The hues stay and the *surface* colour does not come back
  without an answer to the two complaints that buried it: five pale tints that
  read alike, and text sitting on five different backgrounds.

---

## 7. Drift, staleness, and the places nothing will catch you

- **README vs code.** It used to say cards are "rounded, **bordered**" when no
  stroke object existed; since §10 the stroke is real, so that line reads true -
  and it now spells the outline out, so a future reader of the README is not
  misled in that direction again. It still cites `fyneui.go`/`fyneview.go` as the GUI while the window is
  built in `shell.go`. It calls `Print` the "`--print` front end"; it is also the
  *only* front end whenever stdout is not a TTY (`main.go:270-273`).
- **The "cannot drift" claim is narrower than it reads.** It holds for GUI
  live-vs-resume (both through one function, `GUI.blockFor`). **`Print` never
  calls `messageBlock`** - it re-implements the mapping (`print.go:147-198`),
  duplicates the assistant banner literal instead of `assistantHeader`, and never
  prints `sessionHeaderLine`. The test that sounds like it covers this asserts
  *trajectory JSON* equality, not transcript text.
- **Dead code a layout change will step on:** `areaColor(kind)` ignores its
  parameter and returns one constant - which §10 turned from "the API is a lie"
  into a documented decision with an assertion behind it (`areaColor` `:1413`,
  `TestGUIAreaHeadersAndAlignment`), and which is still the single place a
  per-kind header hue would go; `selectableRT` and `collapseSpaces` have no callers while the invariant
  is duplicated by hand in three places; `limitLabelSteps`/`limitLabelCost`
  survive the feature that needed them; the `Loop` doc comment (`interactive.go:17`)
  still promises a limits banner; four `area` fields are written and never read.
- **Live contradictions in comments:** `blockCards`' header says
  notes/errors/exit "stay plain rows" while the code builds `areaCard` for all
  three; the accent map's banner still claims "the five kinds are tellable at a
  glance" (`:1220-1234`) while
  eight literal labels (`tool call`, `LLM output`, `user`, `harness`, `thinking`, `exit`, `error`, `note`) plus raw `b.kind` and compaction `b.title` passthrough reach `bubbleOutline` and `note`/`exit`/`error` - the most
  common rows - all take the same `bubbleNeutral` frame. Compaction feeds
  free-form `b.title` in as the colour key, so it can never match. Before §10
  that was a 9.4% wash nobody noticed; now it is a deliberate-looking line, which
  is the honest way for the debt to show. Deferred, not resolved.
- **Behavioural oddities:** one API call stamps `stats` on **two** cards, so
  `1234 tok . 3.2s . $0.07` prints twice, while `costLabel` drops the `$`
  whenever a duration is present; a `system` block renders grey where equivalent
  harness feedback renders amber; a right-aligned user bubble's alignment code
  path depends on whether its markdown happened to contain a blank line;
  `err != nil` with a non-empty `exit_status` shows only "run finished"; the
  picker tab makes **every** Session/Run/Help menu item silently no-op; the
  thinking menu item never changes state.
- **Untested but visible:** row *placement* inside the `centerLayout` /
  `recomputeWindow` path (tests resize `rootC` and assert scroll extents, offsets
  and `winFirst`/`winLast`, but never a row's own position or width); `esc`; `guiScopeNote`; confirm-panel elision; Option/Cmd+Enter;
  transcript re-wrap on width change; the exact hue of every kind but `user`
  (one frame is pixel-asserted since §10, the other four are only compared with
  each other); the `codeFoldAfter` boundary;
  and `bubbleTailDrop`/`bubbleTailBase` (a rename is "completely uncovered"). The
  related trap that *is* real and *is* documented in the source: `newChatInput`
  hand-builds `&widget.Entry{MultiLine: true, Wrapping: fyne.TextWrapWord}`
  (`:1993`) rather than calling `widget.NewMultiLineEntry()`, and
  `c.ExtendBaseWidget(c)` (`:1996`) must stay the **first** claim - `ExtendBaseWidget`
  is first-write-wins, and reintroducing a `widget.New*Entry` constructor or an
  extra claim splits the wrapper's renderer from the inner entry's ("two
  renderers, two scrolls", war story at `:1979-1991`). `input_resize_test.go:38`
  is the test that pins the fixed behaviour.
- **Process:** the Fyne pin must be swapped for the 2.9.0 release when it ships
  ("until then, treat any Fyne-side regression as ours to bisect");
  `fyneview.go` + `blocks.go` have never run under `-race` with N live sessions
  streaming cards (`adversarial-review-2026-11.md:408`); and
  `TestGUITrajectoryMatchesPrintMode` has a documented ~1-in-3 deadlock that was
  deliberately left alone - "a blind change to how a slash command interacts with
  a pending prompt is exactly the sort of edit that turns a one-in-three hang
  into a one-in-one."

---

## 8. Where to touch things

| Change | Files | Caught by tests? |
|---|---|---|
| Spacing / vertical rhythm | theme padding only, `blockSpacer` (`:1609`), the x1.5 coefficient in `estimateBlockHeight` | no - estimates are never compared to measured |
| Colour / contrast | `bubbleAccent` (`:1234`), `bubbleOutline` (`:1261`), `bubbleSurface` (`:1254`), `bubbleStrokeWidth` (`:1274`) - `bubbleFill` and `blendOnto` are gone (§10) | outlines differ by kind; `user`'s hue is pixel-asserted, the other four only compared to each other |
| Bubble frame, tail, stroke | `bubble.points` (`:1312`), `newBubble` (`:1301`), `bubbleRenderer.Layout` (`:1347`) | the pixels, not the geometry - nothing reads the polygon's points back |
| Card chrome | `areaCard` (`:1419`), `area.CreateRenderer` (`:1400`) | partially (`TestGUICardBodiesAreSelectable`) |
| Header content/format | `cardHeaderText` (`1452-1464`) - no width logic, so anything here that grows competes with wrapping | exact strings: `TestGUIAreaHeadersAndAlignment` (`fyneui_test.go:1282`) |
| Kinds, labels, sides | `messageKindLabel` (`1165-1190`) + `blockCards` (`:1548-1593`) + `bubbleAccent` (`:1234`) + `costLabel` - **four string-coupled sites, no constants** | the sides, by table: `TestGUITranscriptSideFollowsAuthorship` |
| Per-kind controls | `area.setActions` (`:1396`) from `attachCardActions` (`:350`) - must be called before the row reaches the canvas | `TestGUIJobCardEndsItsOwnJob` |
| Input area | `newChatInput` (`:2044`) - mind the first-write-wins trap in §7 | `TestPaintedInputFollowsResize` |
| Panel | `setPanel` (`:1783`) + `showQuestion`'s switch on `Question.Kind` (`:1799`) + `Interactive`'s answer semantics. **The `questionPanel*Mode` constants this row used to cite do not exist anywhere in the tree** - another §9 survivor | `TestGUIConfirmPanelDrivesTheLoop` |
| Window/tab chrome | `shell.go` `newShell`/`menus`/`label` | `TestGUIModeInTitleTracksVisibleSession` |
| **Anything geometry** | `fyneview.go` | **nothing for placement** (row widths are pinned, in the app and - since §10 - in the mockup). Verify by hand: `MINI_DEBUG_LAYOUT=1`, or `go run ./cmd/bubblemock` |
| The mockup window | `mockup.go`, `cmd/bubblemock` | `TestGUIMockupLeavesScrollbarGutter` (the gutter, not the look) |
| Print parity | **by hand**, or adopt `messageBlock` in `print.go` and fix the two literal duplications | no equivalence test exists |

---

## Appendix: method

Five parallel deep reads (render primitives; live/status/panel/input;
`fyneui.go`+`shell.go`; tests + `print.go` + `blocks.go` + `prefs.go` +
`markdown.go`; the design docs) consolidated against a direct read of the
virtualization core (`fyneview.go:28-1104`), `interactive.go`, `slash.go`,
`askqueue.go`, `jobs.go`, `transcript.go`, `ui.go`, `logtarget.go`, and the
interface-relevant sections of `parallel-sessions.md`. The numeric claims in
section 2.5 and the `chatInput` note in section 7 were subsequently re-checked
line by line against the source; three rows that did not survive that check were
removed - see section 9.

---

## 9. Corrections after self-audit

An earlier draft of this file, and the report it came from, contained claims that
are not true. They were removed on 2026-11-18 and are recorded here so the rest of
the document can be weighed correctly.

**Invented, and removed:**

1. *"A prompt-injection attempt in reviewed material asked an assistant to reveal
   private instructions."* **There was no prompt injection.** Nothing in the repo
   contains such text (`grep -ri "reveal.*system prompt\|ignore previous" .` over
   the working tree returns nothing), and none of the five subagent report files
   mention it. Each subagent session received exactly **one** user turn - the
   audit brief - and no second instruction of any kind. The claim first appeared in
   one of my own turns and then propagated into the report and into this file.
2. Three rows of the section 2.5 table - a "confirm yank" threshold, an "input min
   height", and a "markdown degradation on `|`, `#`, `~~~`" rule. No such
   identifier or predicate exists in `ui/fyneview.go`; they were presented as quoted
   from the audits and were not.
3. A warning that adding a `chatInput` field after a given line would "silently
   never size" the painted scroll. The real, source-documented hazard is the
   `ExtendBaseWidget` first-write-wins claim (retained, corrected, in section 7).

**Corrected citations:** `paintGap` (`:88`/`:182`, not `:174`),
`estimateBlockHeight` (`:321-331`), the tick (`shell.go:240`), exit grace
(`shell.go:227`), the window-side guards (`prefs.go:66-67, :102, :127-133`), the
settings timeout (`session/settings.go:30-32`, not `prefs.go`), and the elided
confirm-panel string, which is `+N more commands` with no leading ellipsis.

**Why this matters for reading the rest.** The sections describing structure,
layout mechanism, idioms and design decisions were assembled from the five report
files and from direct reads of `fyneview.go`, `interactive.go`, `slash.go`,
`askqueue.go`, `jobs.go`, `transcript.go`, `ui.go` and `logtarget.go`, and the
passages quoted from `docs/parallel-sessions.md` were read from that file directly.
But the failures above show that specific numbers, identifiers and line citations
in this document can be wrong in a way that reads convincingly. **Treat any
individual line reference as unverified until you open the file** - the ones in
sections 2.5 and 7 have now been checked against source; the rest have not.

To reproduce the checks performed here:

```bash
# every turn a subagent received (one per session: the brief)
sqlite3 -readonly ~/.mini/sessions.db "select substr(session_id,-6), seq, role,
  length(content) from turns where session_id in
  (select id from sessions where parent_id='01a1efe4-5a18-7b0e-ad81-caa7d7b0d593')
  and role='user';"

# the agents' own reports, untruncated (deliveries are cut at ~10 KB)
sed -n '160,175p' ~/.mini/tcr/01a1efe4-5a18-7b0e-ad81-caa7d7b0d593/job-j3b11ebf8.log
```

**Second pass (same day), mechanical.** Every quoted rationale span (24) and every
backticked identifier and literal (161) in this file was then searched against the
working tree: all resolve to real text, so the quotations are sound. All 86 numeric
citations point at lines that exist; auditing what those lines actually say turned up
three further errors, now corrected above:

- *"`cardHeaderText` allocates width structurally - 70% / 60% / 50% reserves, with
  mid-string truncation."* **False.** `cardHeaderText` (`fyneview.go:1388-1400`) has
  no width logic and no truncation; it appends non-zero metrics and joins them.
- *"The GUI test suite monkey-patches the driver
  (`app.Driver().(*test.Driver).OverwriteGlfwApp`, `fyneui_test.go:1762`)."*
  **False.** No such call exists anywhere in the tree; tests inject
  `App: test.NewApp()` via `GUIOptions`, which is what the README says.
- *"Two mutexes only"* and *"no test asserts `Resize`/`Move`/heights on a real
  widget"* were both overstated: `askQueue` and `LogTarget` hold their own locks,
  and several tests do resize real objects (`scroll_test.go:21`,
  `input_resize_test.go:38`, `shell_test.go:821`) - what is genuinely unasserted is
  a transcript **row's** position and width.

Citation drift corrected in the same pass: the interface (`ui.go:68-80`, not
`:76-94`), the dead-code-shape quote (`ui.go:90-93`), `OnCallOpened`
(`interactive.go:267-272`), the virtualization comment (`:117-120`), the pre-show
viewport (`:428`), `atBottom` (`:802`), and `paintGap` (`:88`/`:182`). The
"eleven-plus labels" count is replaced by the eight literal labels plus the
`b.kind` / `b.title` passthrough, which is what the code actually produces.

---

## 10. Amendment, 2026-11-18: the bubbles became outlines

The survey described a design that has since changed once, in the direction it was
written to inform: tinted fills are out, coloured outlines are in. The decision
record is `docs/parallel-sessions.md` §8.13. This section is where **this** file
was edited to stay true, and what it means for reading the rest of it.

**Rewritten in place.** §2.4 - a bubble is an opaque theme surface inside a
stroked frame, and the hues are `bubbleAccent` at full intensity rather than
`blendOnto` at 16-20%. §2.5 - outline width and interior added, several citations
moved. §4 - the string-keyed coupling now describes the outline (and a rename
loses more than it used to), and the test-idiom paragraph records that the suite
can assert on painted pixels. §5.14 - the decision itself. §6 - tinted fills are
now on the rejected list, so they do not come back as a fresh idea. §7 - the
README is no longer wrong about a stroke, the grey fall-through is a shared
neutral frame, `areaColor` is a decision with a test rather than dead code, and
the untested list shrank by exactly one item. §8 - the colour, frame, geometry
and mockup rows.

**Load-bearing and unchanged.** §2.3's gutter. `scrollbarGutter()` (`:316`) is why
the mockup command had to be fixed: its first version stacked every card's right
edge - and every scrollable card's own bar - on top of the transcript scrollbar,
which is the mis-grab this file describes in §2.3 as "a coin flip on which
scrollbar answers." Production had a test for it (`TestGUICardsLeaveScrollbarGutter`)
and the tool did not, so the wrong thing rendered at full size and looked like the
design. `TestGUIMockupLeavesScrollbarGutter` closes that.

**Two things this file got right in advance.** §4's "the honesty rule" is why the
interior stayed opaque instead of going transparent, and why a stroke has a
floor: a colour that is assigned correctly and painted invisibly is the failure
mode this codebase has been bitten by repeatedly, and `A: 0xff` on every accent
literal plus the pixel test are the answer to it. §7's note that no test asserted
anything a painter does is now, in this one corner, out of date - see §4.

**Citation drift.** `ui/fyneview.go` grew from 2,123 to 2,163 lines with §10, and
to 2172 with §11. Every number in this file was re-derived a second time when §11
landed, so the citations below :1165 are exact and the ones §10 re-derived are
exact again. Citations
below `:1195` are unaffected and still exact; in the bubble and area region every
number in this file was re-read off the current tree (`bubbleAccent` `:1234`,
`bubbleSurface` `:1254`, `bubbleOutline` `:1261`, `bubbleStrokeWidth` `:1274`,
`bubble.points` `:1312`, `areaColor` `:1413`, `areaCard` `:1419-1448`,
`cardHeaderText` `:1452-1464`, `transcriptGrid` `:1509-1519`, plus the four
`"LLM output"` sites `:1172, :1230, :1565, :1644`); a citation elsewhere in the
file that lands after those is short by up to 40. The §9 rule is not withdrawn:
open the file before trusting a number.

---

## 11. Amendment, 2026-11-18: which side a card sits on is who spoke

Reported as a display bug - text the user typed appeared in a **harness** card -
and fixed as a design one, because the display was only the symptom.

**What was actually wrong.** `messageKindLabel` (`:1165-1190`) had the right
nouns and the wrong default. For `kind == "user"` it asked whether `subtype` was
one of the human-authored strings (`""`, `"UserSteering"`) and sent everything
else to the harness. So every `interrupt_type` that existed but had not been
enumerated read as harness feedback: the comment typed at the interruption prompt
(`"UserInterruption"`, from `"Interrupted by user: ..."`) and the next thing
typed at the ready line (`"UserNewTask"`). Mid-run steering was already correct -
it is echoed as `block{kind: "user"}` with no subtype, and its commit is skipped
(`fyneui.go:453`) - which is why the two paths looked like one bug to whoever hit
them: the same keystroke becomes an *answer to a prompt* the moment the run is
not mid-turn, and prompts are where the mislabelled types are written.

**The rule that was broken is §5.5's**: attribution was **derived** from a field
the harness writes for the model's benefit, instead of carried. Derived
attribution is wrong by default for every kind nobody has thought of yet, and
nothing fails when that happens.

**Now it is carried.** `ui/blocks.go` gained `block.typed`, read from
`extra["user_typed"]` (`userTypedKey`), and `ui/slash.go` gained `userWords`
beside `interruptUser` for the sites where the words came off the keyboard:
`Interactive.OnInterrupt`'s comment, and the answer at the ready line. The
`interrupt_type` the model and the store see is **unchanged** - only an extra key
is new - so sessions keep their shape. The subtype list still back-fills
transcripts written before the marker existed, and `"UserNewTask"` joined it on
the reasoning that its wrapper can only ever quote the human.

**On the "new task" framing**, which the maintainer called out as wrong for this
UI: a session is a continuing conversation whose turns build on each other, not a
run of discrete tasks, and the transcript should not imply otherwise. That is
why the ready-line text now reads as the human's own card. The sentence handed to
the model still says *"The user added a new task: ..."*, and so does the prompt
title (`newTaskTitle`, `slash.go:70`) - **that wording is a prompt decision, not
a display one**, and it is the next thing to revisit here.

**What stayed harness, on purpose:** rejections, format errors, job updates, a
parent's message to a child, and "Temporary interruption caught." - a
cancellation the human chose not to comment on. The test is who wrote the
sentence, not whether the human is mentioned in it; a harness report wearing the
user's blue bubble is the honesty rule (§4, idiom 3) and the lying affordance §8.6 refuses to ship,
wearing a colour and an alignment.

**Pinned** by `TestGUITranscriptSideFollowsAuthorship` (a table, including a
subtype invented after the test was written - precisely the case the old default
got wrong), `TestGUIAuthorshipTravelsInTheMessage` (the marker survives the
`model.Message` round trip), `TestGUIEscCancelsTheTurn` and
`TestGUINextTurnAtTheReadyLineIsTheHuman` end to end. All four were run against
the old condition and fail on it; the `esc` test's failure message reproduces the bug
report.

**One citation in this file was found false while doing it:** §2.3's `atBottom`
"`:790`" is `:802`. Corrected - it is the kind of drift §9 says to expect, and
the §8 table's `questionPanel*Mode` row (never re-verified by the §9 pass) names
constants that do not exist at all, so that row now says so instead of pointing
nowhere.
