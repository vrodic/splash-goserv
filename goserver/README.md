# Go server

A Go port of Splash's HTTP server (`python -m server.server`). It takes the
same command line, writes the same serve record, and drives the same native
engine (`build/splash serve-native`) over the same binary protocol, holding
a fraction of the Python server's memory.

```bash
make build/splash                 # the engine, as before
make go-server                    # build/splash-server
SPLASH_SERVER=go ./splash serve --model ... # the launcher runs the Go server
```

Without `SPLASH_SERVER=go` the launcher runs the Python server, as before.

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

## Not yet ported

These answer with a 400 or 501 that says the Go server does not implement
them:

- Grammar-constrained output: `response_format` JSON schemas,
  `tool_choice: required`/named tools and `strict` tools, which need
  llguidance. `parallel_tool_calls: false` keeps the first call, and
  `tool_choice: none` reads any call as text, without a grammar.
- Images and PDFs. The server reports `vision: false`.
- `/v1/responses`, `/v1/judgments` and `/v1/systemone`.
- Prompt-prefix token reuse (`tokenization.PromptTokenizer`). Prompts are
  encoded whole.

## Parity checks

Run these from the repository root with `.venv` and the model's files:

```bash
cd goserver && go build -o ../build/tokcheck ./cmd/tokcheck && go build -o ../build/tplcheck ./cmd/tplcheck && cd ..
.venv/bin/python goserver/parity/tokenizer_parity.py MODEL_DIR   # vs tokenizers
PYTHONPATH=. .venv/bin/python goserver/parity/template_parity.py MODEL_DIR
```

At the time of the port: 4,979 strings with 0 token mismatches, and 8,050
template renders with 0 mismatches.

One known difference: transformers 5 rebuilds Qwen2Tokenizer's
pre-tokenizer with the older Qwen2 regex, which lacks `\p{M}`. The Python
server therefore splits combining marks differently from the model's own
`tokenizer.json`. The Go server follows `tokenizer.json`.
