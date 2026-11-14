# testdata

Fixtures. Every test that reads this directory is offline and
read-only: `git status --porcelain testdata/` must stay clean after
`go test ./...`.

|Item|What it is|Regenerate with|
|---|---|---|
|`sushi/*.sse`|Hand-written OpenAI-style SSE scripts (one turn per file) with sushi's exact frame shapes: keepalive comments, `finish_reason` `length`/`error`, a complete tool call in one delta, a fragmented one, parallel calls, an `include_usage` chunk with `timings`.|edit by hand; `testdata/fakesushi` replays them byte-for-byte|
|`fakesushi/main.go`|Replays those streams over HTTP so the CLI and the model client can be exercised with no GPU. `package main` under `testdata/` is invisible to `./...`, so it never ships.|`go run ./testdata/fakesushi -addr :8009 -delay 150ms -keepalive`|

Try it against the real agent loop:

```bash
go run ./testdata/fakesushi -addr :8009 -delay 150ms &
/tmp/mini -p -y -m test-model --url http://127.0.0.1:8009 \
  -t "run 'echo hi', verify, then submit" --exit-immediately -o /tmp/out.traj.json
python3 -c "import json;d=json.load(open('/tmp/out.traj.json'));print(d['info']['exit_status'],repr(d['info']['submission']),len(d['messages']))"
# Submitted 'Everything verified; the task is complete.' 6
```
