# v0.1.2

Fix M Plan usage responses that provide percentages instead of request counts.
Requires **CLIProxyAPI v8.0.0 or later**.

## Fixes

- Read `current_interval_remaining_percent` and
  `current_weekly_remaining_percent` before falling back to count fields.
  Current plans can return zero for every count while still reporting valid
  remaining percentages; those responses previously failed with
  `quota response has no readings` and surfaced as HTTP 502 in CPA.
- Show upstream percentages and reset times without inventing request totals.
  Both 0% and 100% are valid readings. `quota.usage-count` continues to apply
  only to count-based responses and never inverts remaining percentages.
- Try the next configured quota endpoint when a response has no usable
  readings. Authentication failures still stop the lookup immediately.

## Upgrade

Download `cpa-minimax_0.1.2_<os>_<arch>.zip` for your CPA host and verify it
against `checksums.txt`. Stop CPA before replacing the plugin library, then
restart CPA. No configuration changes are required.

# v0.1.1

Fix subscription usage queries for China mainland and international accounts.
Requires **CLIProxyAPI v8.0.0 or later**.

## Fixes

- Query subscription usage on the official website hosts:
  `https://www.minimax.cn` for China mainland and `https://www.minimax.io`
  for international accounts. Previously, quota queries reused the inference
  API host, which could time out and surface as a 502 Bad Gateway in CPA.
- Preserve explicit shared and per-key `base-url` overrides for quota queries.
  Pay-as-you-go keys continue to report their plan without querying subscription
  usage or account balances.

## Upgrade

Download `cpa-minimax_0.1.1_<os>_<arch>.zip` for your CPA host and verify it
against `checksums.txt`. Stop CPA before replacing the plugin library, then
restart CPA. No configuration migration is required. An explicitly configured
`base-url` continues to override the default quota host.

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
