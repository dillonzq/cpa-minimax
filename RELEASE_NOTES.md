# v0.1.0

First release of **cpa-minimax**, a native CLIProxyAPI plugin that exposes
MiniMax as a single provider (`minimax`). Requires **CLIProxyAPI v8.0.0 or
later**.

## Features

- One provider for both MiniMax credential types: M Plan subscription keys
  (formerly Coding Plan / Token Plan) and pay-as-you-go API keys. Subscription
  keys default to a higher CPA priority, so plan usage is spent first and API
  keys take over while every plan key is cooling down.
- International (`api.minimax.io`) and China mainland (`api.minimax.cn`)
  sites, chosen per key with a configurable default region, a shared
  `base-url`, and per-key address overrides.
- Chat Completions, Messages, Responses, and Gemini clients through
  [apix v0.1.0](https://github.com/dillonzq/apix/tree/v0.1.0); upstream uses
  MiniMax's Anthropic-compatible, OpenAI-compatible, and Responses endpoints.
- Built-in MiniMax-M3.1-Flash and MiniMax-M3 with documented limits and thinking
  levels, optional `/v1/models` discovery, and per-model overrides.
- MiniMax thinking rules: `reasoning_split` on Chat, always-on thinking for
  M3.1 Flash, and adaptive or disabled thinking for M3.
- MiniMax error codes drive CPA cooldowns: auth failures (1004/2049) → 401,
  balance (1008) → 402, plan usage (2053/2056/2067) and rate limits → 429,
  overloads → 503.
- Native CPA quotas: subscription keys report their 5-hour and weekly windows
  per model group; pay-as-you-go keys report their plan.
- `executor.count_tokens` for Claude-format requests through
  `/anthropic/v1/messages/count_tokens`.

## Installation

Download `cpa-minimax_0.1.0_<os>_<arch>.zip` for the **CPA host's operating
system and architecture** and `checksums.txt` from this release. Verify the ZIP's
SHA256, stop CPA, place the library in `<cliproxyapi_root>/plugins/<os>/<arch>/`,
configure `plugins.configs.cpa-minimax`, and restart CPA. See the
[English](https://github.com/dillonzq/cpa-minimax#install) or
[Chinese](https://github.com/dillonzq/cpa-minimax/blob/main/README.zh-CN.md#安装) installation
guide and the [configuration reference](https://github.com/dillonzq/cpa-minimax/blob/main/docs/configuration.md).
