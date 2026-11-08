# Go server

A Go port of Splash's HTTP server (`python -m server.server`). It takes the
same command line, writes the same serve record, and drives the same native
engine (`build/splash serve-native`) over the same binary protocol, holding
a fraction of the Python server's memory.

```bash
make build/splash      # the engine
make go-server         # build/splash-go
```

## splash-go: the splash command in Go

`build/splash-go` is the Go port of `./splash` (`install/launcher.py` and
`install/clients.py`). It takes the same commands and options, and runs the
Go server in its own process:

```bash
./build/splash-go serve --model Jundot/Qwen3.8-27B-oQ4e-mtp \
  --model-dir ~/.omlx/models/Jundot/Qwen3.8-27B-oQ4e-mtp \
  --draft-model ~/.omlx/models/incoai/Qwen3.8-27B-DFlash2 \
  --max-cache-disk 0 --offline --port 8000
./build/splash-go claude      # or opencode, codex, hermes, pi; --port PORT; agent args after --
```

`serve` takes the port's lock and writes its serve record as `./splash`
does, so the desktop app and a second `serve` see it the same way. It finds
the model's prepared directory by the selection link the Python installer
names (`install/models/<OWNER>/<REPO>` or `.selections/<hash>`). Where that
directory still verifies (each linked file's size and times as `model.json`
records them), it serves it with no Python at all:
- **Local files** (`--model-dir`): with `--draft-model`, or with `--offline`.
- **Hub models:** with `--offline`.

Otherwise, for a model not prepared yet, files that changed, or a Hub model
online that the installer checks for updates, it runs the Python installer
once (`install/models.py` or `install/local.py`, which need `.venv`), then
serves in Go. `./splash` and `install/` are unchanged.

The agent commands write the same configuration and environment as Python's
`clients.py`. Each agent's argv, environment and written files (OpenCode's
inline config, Codex's `-c` settings, Hermes's profile `config.yaml`, Pi's
`models.json`) were compared against `./splash`'s and match. Codex needs the
Responses API, which the Go server does not serve yet.

## Running the server alone

`splash-go server` is the server alone, without `serve`'s port check, locks,
serve record or model lookup: `python -m server.server`'s command line, for
debugging. It needs a model directory Splash has prepared: a selection link in
`install/models/.selections/` or an assembly in `install/models/.resolved/`.
Preparing a new model is still the Python installer's job, done once (any
`./splash serve` run prepares one).

```bash
ls -l install/models/.selections/     # each link names a prepared model
./build/splash-go server install/models/.selections/<link> \
  --model Jundot/Qwen3.8-27B-oQ4e-mtp --max-cache-disk 0 --port 8000
```

It takes `splash serve`'s server options (`--host`, `--api-key`,
`--max-context`, `--max-memory`, `--idle-release`, `--queue-size`, …) as
`python -m server.server` does. `--binary` defaults to the `splash` engine
beside it, and `--serve-record` is only for the desktop app.

Constrained output loads llguidance from the Python environment's wheel
library (`.venv/lib/python3*/site-packages/llguidance/_lib.abi3.so`) with
`dlopen`. That reads a file and runs no Python. `SPLASH_LLGUIDANCE=/path/to/_lib.abi3.so`
points elsewhere.

Agent clients then connect to it as to `splash serve`, such as Claude Code
with `ANTHROPIC_BASE_URL=http://127.0.0.1:8000`, or any OpenAI client with
`http://127.0.0.1:8000/v1`.

## Memory

Measured with `Jundot/Qwen3.8-27B-oQ4e-mtp`, `--max-cache-disk 0`, on an
M4 Max. The engine is the same process in both runs (about 2.5 GB RSS, and
the model's wired Metal memory). The workload was 8 concurrent streamed
chats with prompts of about 15k tokens, plus 4 Messages requests with tools
(`parity/memory_load.py`).

| server process | Python | Go |
| --- | --- | --- |
| RSS, idle after start | 634 MB | 8–18 MB |
| peak RSS under load | 674 MB | 71 MB |
| peak footprint under load | 559 MB | 84 MB |
| footprint after a constrained request | (included above) | 208 MB |

llguidance's token trie for the 248k-token vocabulary takes about 120 MB,
so the Go server loads it on the first constrained request (about 0.4 s)
rather than at startup, as the Python server does.

## What it covers

- Engine protocol v12, process start/stop, relaunch after a failure with
  backoff, `/status`, `/metrics`, `/ready`, `/health`, `/v1/memory/release`.
- `/v1/chat/completions` (streamed and whole), `/v1/completions`,
  `/v1/messages` (streamed and whole, thinking, signatures, tool_use),
  both `count_tokens` routes, `/tokenize`, `/apply-template`, `/v1/models`,
  and the chat page.
- Chat templates: a Jinja interpreter (`jinja/`) with transformers' environment
  (trim_blocks, lstrip_blocks, loopcontrols, `generation`, `tojson`,
  `raise_exception`). The startup probe matches Python's: it detects and
  patches later system messages, and finds the generation prompt for each
  reasoning effort, the reasoning report and the thinking end.
- Tokenizer: byte-level BPE from `tokenizer.json` (`tokenizer/`), with
  incremental decoding.
- Reasoning splitting, tool-call parsing (Qwen XML and GLM layouts), stop
  sequences, thinking budgets, sampling defaults, cache salts. Cache salts
  are bit-identical to Python's, so `--persistent-cache` entries carry over.
  So do thinking signatures, which use Fernet under the shared key.
- Host, origin and API-key checks, and both error shapes.
- Constrained output, as in `constraints.py` and `tool_schema.py`:
  JSON-schema and `json_object` response formats, Anthropic output formats,
  `tool_choice` required, named and none, `parallel_tool_calls: false`, and
  `strict` tools. The grammars match Python's byte for byte
  (`parity/grammar_parity.py`). The server answers the engine's mask
  requests, draft rows included, and checks generated tokens against the
  grammar.

## llguidance

`llg/` drives llguidance through its C API (`llguidance.h`, v1.8.0 from
github.com/guidance-ai/llguidance, MIT). The library is the one the Python
environment already ships, loaded with `dlopen` at run time:
`.venv/lib/python3*/site-packages/llguidance/_lib.abi3.so`, `python/...` in a
packaged install, or `SPLASH_LLGUIDANCE`. That wheel is a CPython
extension, so `llg/pystubs.c` defines the 96 CPython symbols it imports.
They are never called, since only the `llg_*` C functions are used. Without
the library, constrained requests answer 400 `constraint_unsupported` and
everything else still works.

## Not yet ported

These answer with a 400 or 501 that says the Go server does not implement
them:

- Checking a structured answer against its schema after generation. The
  grammar already enforces the schema, except for keywords it leaves out,
  such as look-around patterns. Meta-validation of schemas checks only
  `type` names, which are what Python's `check_schema` usually reports.
- Images and PDFs. The server reports `vision: false`.
- `/v1/responses`, `/v1/judgments` and `/v1/systemone`.
- Prompt-prefix token reuse (`tokenization.PromptTokenizer`). Prompts are
  encoded whole.

## Parity checks

Run these from the repository root with `.venv` and the model's files:

```bash
cd goserver && for c in tokcheck tplcheck grammarcheck; do go build -o ../build/$c ./cmd/$c; done && cd ..
.venv/bin/python goserver/parity/tokenizer_parity.py MODEL_DIR   # vs tokenizers
PYTHONPATH=. .venv/bin/python goserver/parity/template_parity.py MODEL_DIR
PYTHONPATH=. .venv/bin/python goserver/parity/grammar_parity.py MODEL_DIR
```

At the time of the port: 4,979 strings with 0 token mismatches, 8,050
template renders with 0 mismatches, and 2,216 grammars with 0 mismatches.

The Go server handles two constraint cases explicitly (not yet checked
against the Python server):
- A speculative token batch can carry the grammar's last tokens and the EOS
  together, and llguidance's bulk consume refuses the EOS once the grammar
  has stopped. The Go server consumes the batch up to the EOS, then accepts
  the EOS after a finished grammar.
- A mask's draft rows are computed on a copy of the matcher, because a
  refused draft token leaves a matcher in an error state that rollback does
  not clear.

One known difference: transformers 5 rebuilds Qwen2Tokenizer's
pre-tokenizer with the older Qwen2 regex, which lacks `\p{M}`. The Python
server therefore splits combining marks differently from the model's own
`tokenizer.json`. The Go server follows `tokenizer.json`.
