# `iplane up` — the one-liner

The chapter's flagship verb. Provisions a GPU pod, runs the engine
image on it, drops you into a chat REPL, and tears everything down on
exit. All in one command.

## TL;DR

```bash
export RUNPOD_API_KEY=rpa_...
iplane up --model Qwen/Qwen2.5-1.5B-Instruct
```

Three to eight minutes later (first run pulls the multi-GB vLLM
image), the prompt drops:

```
  Chat with the model. Empty line OR Ctrl-C exits.

> What is the capital of France?

  Paris.

  (1.234s · 8 prompt + 1 completion tokens)

> Explain transformers in one sentence.

  Transformers are deep-learning architectures that process sequences
  in parallel using attention mechanisms.

  (3.412s · 9 prompt + 18 completion tokens)

>
  (empty prompt -- exiting)
Terminated deployment iplane-up-20260525t190000
```

That's it. The GPU is gone, you're not billed for anything past the
exit moment.

## With telemetry (recommended)

The engine ships traces to whatever OTLP endpoint you point at:

```bash
# Hosted (Grafana Cloud Free, Honeycomb, etc.)
export IPLANE_OTEL_ENDPOINT=https://otlp-gateway-prod-XXX.grafana.net/otlp
export IPLANE_OTEL_HEADERS='Authorization=Basic <token>'

# Or local stack via cloudflared tunnel
COMPOSE_PROFILES=tunnel make up
export IPLANE_OTEL_ENDPOINT=$(iplane telemetry url)

iplane up --model Qwen/Qwen2.5-1.5B-Instruct
```

Every chat turn produces a span. Open Grafana → Explore → Tempo to
see them. See `docs/telemetry.md` for the full recipe.

If you don't want telemetry at all:

```bash
iplane up --model Qwen/Qwen2.5-1.5B-Instruct --no-telemetry
```

This silences the "no IPLANE_OTEL_ENDPOINT" warning and skips the
OTel env propagation entirely.

## Flags

| Flag | Default | Notes |
|------|---------|-------|
| `--model` | **required** | HF model id, e.g. `Qwen/Qwen2.5-1.5B-Instruct` |
| `--provider` | `runpod` | Only `runpod` is deployable in v0.1 |
| `--class` | `small` | `small` / `medium` / `large` / `xlarge` |
| `--image` | `vllm/vllm-openai:v0.7.0` | Engine image |
| `--region` | unset | Provider region hint |
| `--otel-endpoint` | `$IPLANE_OTEL_ENDPOINT` | OTLP sink URL |
| `--otel-headers KEY=VAL` | `$IPLANE_OTEL_HEADERS` | OTLP request headers |
| `--no-telemetry` | `false` | Skip OTel env, silence missing-endpoint warning |
| `--id` | `iplane-up-<timestamp>` | Deployment id |
| `--timeout` | `15m` | Cold-start ceiling |
| `--no-chat` | `false` | Skip the REPL; print endpoint + block on Ctrl-C |
| `--debug-shell` | `false` | Opt into publicIp + sshd (`iplane instance ssh` works) |
| `--max-tokens` | `256` | REPL completion cap |
| `--temperature` | `0.7` | REPL sampling temperature |

## What runs underneath

`iplane up` is pure orchestration — it doesn't introduce new primitives:

1. **`CreateDeployment{auto-provision, wait=true}`** — the same call
   `iplane deployment deploy` makes. Image-as-pod by default, no
   publicIp unless `--debug-shell`.
2. **`WatchDeployment` (streaming)** — the same stream the 03 demo
   uses for the progress messages during cold-start.
3. **`POST /v1/chat/completions`** — direct HTTP to the engine's
   proxy URL. iplane is not in the data path.
4. **`DestroyDeployment`** — fires on any exit (REPL exit, Ctrl-C,
   provision failure). Idempotent on the Service side.

You can do all of this manually with `iplane deployment deploy` +
`iplane deployment query` + `iplane deployment destroy`. `up` is the
"I want one command for the chapter's demo flow" version.

## When NOT to use `up`

- **You want the pod to outlive the shell.** `up` terminates on exit;
  there's no `--detach`. Use `iplane deployment deploy` directly +
  `iplane deployment query` against it.
- **You want multiple deployments at once.** v0.1 `up` is single-
  instance. Run `iplane deployment deploy` per model with different
  ids.
- **You're scripting non-interactively.** The chat REPL needs a TTY.
  Use `--no-chat` to get just the endpoint, or use the underlying
  verbs.

## Bigger / smaller models

```bash
# Small (24 GB GPU): 1.5B-Instruct, ~60s warm cold-start
iplane up --model Qwen/Qwen2.5-1.5B-Instruct

# Medium (48 GB GPU): 7B-Instruct
iplane up --model Qwen/Qwen2.5-7B-Instruct --class medium

# Large (80 GB GPU): 14B-Instruct
iplane up --model Qwen/Qwen2.5-14B-Instruct --class large
```

The cold-start estimates in `examples/03/main.go`'s model table apply
roughly: image pull (10–15 GB) + HF download (3–30 GB) + model load
to VRAM (10–60s). First run on any host is slow; subsequent runs hit
the image cache.

## Architectural caveats

- **Single instance, single model**. Multi-replica / multi-model `up`
  is v0.2+ (chapter 7's queue + scheduler).
- **No router**. The engine endpoint is the proxy URL directly. If you
  need request balancing or model routing, that's a v0.2 concern.
- **No auth on the endpoint**. The proxy URL is unauthenticated by
  default. Don't print it in public talks; rotate the deployment id
  after you do.
- **`iplane up` is in-process only**. No `--service-url` flag. If you
  want to forward to a long-running `iplane serve`, use
  `iplane deployment deploy --service-url <url>`.
