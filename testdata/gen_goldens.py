#!/usr/bin/env python3
"""Render the shipped Jinja templates into testdata/templates/*.golden.

The Go port translates mini-swe-agent's Jinja prompts to text/template; this
script is the proof that the translation is byte-exact. It renders the
original Jinja sources (read straight from the Python checkout) over the fixed
contexts in testdata/contexts/*.json and writes one .golden file per pair,
which tpl's TestGolden diffs the Go renderer against.

    uv run --with jinja2 --with pyyaml python3 testdata/gen_goldens.py
    ... -- --write-contexts    # also (re)create the context files
"""

import json
import os
import sys
from pathlib import Path

import jinja2
import yaml

ROOT = Path(__file__).resolve().parent
SRC = Path(os.environ.get("MINISWEA_SRC", ROOT.parent.parent / "mini-swe-agent/src/minisweagent/config"))
CTX = ROOT / "contexts"
OUT = ROOT / "templates"

# Long enough to cross the 10000-character elision threshold, with multi-byte
# characters in the head, the elided middle and the tail: Jinja slices by
# character, so a byte-based Go port would visibly differ here.
ALPHABET = "üñïcodé 0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ .,;:!\"'()\\"
LONG = "".join(ALPHABET[i % len(ALPHABET)] for i in range(12000))
SHORT = "hello"
EXC = 'An error occurred while executing the command: Command \'x\' returned non-zero exit status 1 "quoted".'
ERR_NO_TOOLCALLS = "No tool calls found in the response. Every response MUST include at least one tool call."
ERR_PARSE = 'Error parsing tool call arguments: Expecting property name enclosed in double quotes: line 1 column 2 (char 1).'


def contexts():
    """(spec, key, case) -> the render context, shared by both engines."""
    obs = lambda out, rc, exc: {"output": {"output": out, "returncode": rc, "exception_info": exc}}
    err = lambda error, actions, has, finish: {
        "error": error,
        "actions": actions,
        "has_tool_calls": has,
        "finish_reason": finish,
    }
    inst = lambda system: {
        "task": "Fix the flaky test in tests/test_time.py\nit sometimes fails:\n\tassert 3 == 4",
        "system": system,
        "release": "25.0.0",
        "version": "Darwin Kernel Version 25.0.0: Mon Aug 11 00:00:00 PDT 2025; root:xnu-12345.6.7~8/RELEASE_ARM64_T8132",
        "machine": "arm64",
        "node": "workstation.local",
        "processor": "arm",
    }
    out = {
        ("mini", "observation_template", "short"): obs(SHORT, 0, ""),
        ("mini", "observation_template", "failure"): obs("", -1, EXC),
        ("mini", "observation_template", "long"): obs(LONG, 0, ""),
        ("mini", "observation_template", "long_exception"): obs(LONG, 0, EXC),
        ("mini", "observation_template", "unicode_short"): obs("café ☕ <b> & 'q' \"d\"", 0, ""),
        ("mini", "format_error_template", "truncation_after_tool_calls_choice"): err(ERR_NO_TOOLCALLS, [], False, "tool_calls"),
        ("mini", "format_error_template", "truncation_length"): err(ERR_NO_TOOLCALLS, [], True, "length"),
        ("mini", "format_error_template", "malformed_call"): err(ERR_PARSE, [], True, "stop"),
        ("mini", "format_error_template", "no_reason"): err(ERR_PARSE, [], False, ""),
        ("mini", "instance_template", "darwin"): inst("Darwin"),
        ("mini", "instance_template", "linux"): inst("Linux"),
        ("mini", "system_template", "plain"): inst("Darwin"),
    }
    return out


def jinja_source(spec: str, key: str) -> str:
    data = yaml.safe_load((SRC / f"{spec}.yaml").read_text(encoding="utf-8"))
    for section in ("agent", "model", "environment"):
        value = data.get(section, {})
        if isinstance(value, dict) and key in value:
            return value[key]
    raise SystemExit(f"{spec}.yaml has no {key}")


def main() -> int:
    write_contexts = "--write-contexts" in sys.argv
    CTX.mkdir(parents=True, exist_ok=True)
    OUT.mkdir(parents=True, exist_ok=True)
    if not SRC.is_dir():
        raise SystemExit(f"mini-swe-agent config dir not found at {SRC} (set MINISWEA_SRC)")
    for (spec, key, case), ctx in sorted(contexts().items()):
        name = f"{spec}__{key}__{case}"
        if write_contexts:
            (CTX / f"{name}.json").write_text(json.dumps(ctx, indent=2) + "\n", encoding="utf-8")
        ctx_file = CTX / f"{name}.json"
        if not ctx_file.is_file():
            raise SystemExit(f"missing context {ctx_file}; run with --write-contexts")
        rendered = jinja2.Template(
            jinja_source(spec, key), undefined=jinja2.StrictUndefined
        ).render(**json.loads(ctx_file.read_text(encoding="utf-8")))
        (OUT / f"{name}.golden").write_text(rendered, encoding="utf-8")
        print(f"wrote {name}.golden ({len(rendered)} chars)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
