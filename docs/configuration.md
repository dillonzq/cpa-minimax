# Configuration reference

[简体中文](configuration.zh-CN.md) · [README](../README.md#configuration)

All plugin settings belong under `plugins.configs.cpa-minimax` in CPA’s `config.yaml`. Both `plugins.enabled` and `plugins.configs.cpa-minimax.enabled` must be `true`. Examples below use canonical names; accepted aliases are listed at the end.

## Full configuration example

```yaml
plugins:
  enabled: true
  configs:
    cpa-minimax:
      enabled: true
      region: global                 # default region for keys: global | cn

      # Optional shared host override; omit to use each key's region default
      # base-url: https://proxy.example.com

      api-keys:
        - value: "${MINIMAX_PLAN_KEY}"
          name: "Team plan"
          type: subscription         # subscription | api; default: sk-cp- prefix → subscription
          region: cn
          # base-url: https://api.minimaxi.com  # optional per-key override
        - value: "${MINIMAX_PLAN_KEY_2}"
          type: subscription
        - value: "${MINIMAX_API_KEY}"
          name: "Pay-as-you-go"
          type: api
          priority: 0                # default: subscription 1, api 0; higher is preferred

      model-prefix:
        enabled: true                # false publishes bare model IDs
        value: minimax

      catalog:
        discover: true               # list /v1/models in addition to built-in models
        refresh-interval: 15m        # minimum 1m
        stale-while-unavailable: true

      protocols:                     # endpoint paths for every model
        messages:
          endpoint: /anthropic/v1/messages

      models:                        # overrides and extra declarations
        - name: MiniMax-M3
          display-name: MiniMax M3
          max-tokens: 131072

      quota:
        usage-count: remaining       # remaining | used
        endpoints:                   # tried in order
          - /v1/token_plan/remains
          - /v1/api/openplatform/coding_plan/remains

      request-timeout: 5m
      max-response-bytes: 67108864
      allow-http: false
```

## Regions and upstream addresses

A key uses its own `region`, then the top-level `region`, which defaults to `global`. Its address is resolved in this order:

1. The key’s explicit `base-url`.
2. An explicitly configured shared `base-url`.
3. The key region’s default host: `https://api.minimax.io` for `global`, or `https://api.minimax.cn` for `cn`.

With no shared override, mixed-region keys each use their own site. An explicit shared address applies to every key without an individual override, even across regions. Execution, streaming, token counting, model discovery, and subscription quota queries use the same address rules.

```yaml
region: global
api-keys:
  - value: "${MINIMAX_API_KEY}"
    type: api
  - value: "${MINIMAX_PLAN_KEY}"
    type: subscription
    region: cn
    base-url: https://api.minimaxi.com
```

Here the international API key uses `https://api.minimax.io`, while the China subscription key uses its own legacy-host override. Place this fragment under `plugins.configs.cpa-minimax`.

Both shared and per-key URLs require HTTPS; `allow-http: true` permits HTTP for local tests. Userinfo, queries, and fragments are rejected. Trailing `/`, `/v1`, and `/anthropic` are normalized away before the plugin appends endpoint paths. Proxy path prefixes such as `https://proxy.example.com/minimax` are supported. Omit an optional URL to use defaults; an explicit empty string is rejected.

Only key `value` fields expand `${ENV_VAR}`; `base-url` and other fields do not. Variables must be visible to the CPA process.

## Configuration options

| Option | Type | Default | Description |
|---|---|---|---|
| `api-keys` | `[]object` | *(required)* | Keys with `value` and optional `name`, `type`, `region`, `base-url`, `priority`. `value` supports `${ENV_VAR}`. Duplicates and empty values are rejected. |
| `api-keys[].name` | `string` | generated label | Optional display name and initial credential filename. |
| `api-keys[].type` | `string` | by prefix | `subscription` or `api`; see [Credential types](../README.md#credential-types). |
| `api-keys[].region` | `string` | `region` | `global` or `cn`. |
| `api-keys[].priority` | `int` | `1` / `0` | CPA priority; higher is preferred. Defaults to 1 for subscription and 0 for API keys. |
| `region` | `string` | `global` | Default region for keys without one. |
| `base-url` | `string` | by `region` | Shared upstream host override; when omitted, each key uses its region’s default (`https://api.minimax.io` or `https://api.minimax.cn`). HTTPS (or HTTP with `allow-http`), without query, fragment, or userinfo; a trailing `/v1` or `/anthropic` is dropped. |
| `api-keys[].base-url` | `string` | `base-url` / key region | Optional host override for one key, taking precedence over the shared `base-url`. Same URL rules. |
| `model-prefix.enabled` | `bool` | `true` | Publish `<prefix>/<model>` when `true`, bare IDs when `false`. |
| `model-prefix.value` | `string` | `minimax` | Model prefix. |
| `catalog.discover` | `bool` | `true` | List `/v1/models` in addition to built-in models. |
| `catalog.refresh-interval` | `duration` | `15m` | Discovery interval, minimum `1m`. |
| `catalog.stale-while-unavailable` | `bool` | `true` | Keep the last good listing when discovery fails. |
| `protocols.<protocol>.endpoint` | `string` | protocol default | Endpoint path for every model. |
| `models` | `[]object` | `[]` | Metadata overrides and declarations by upstream `name`: `display-name`, `description`, `max-context-length`, `max-tokens`, `input-modalities`, `output-modalities`, `thinking` (`min`, `max`, `zero-allowed`, `dynamic-allowed`, `levels`), and `protocols`. |
| `quota.usage-count` | `string` | `remaining` | How to read `*_usage_count` from the remains endpoint. |
| `quota.endpoints` | `[]string` | see example | Remains endpoints tried in order. |
| `request-timeout` | `duration` | `5m` | Deadline for opening and consuming an upstream request. |
| `max-response-bytes` | `int64` | `67108864` | Maximum response size; a stream counts all of its chunks. |
| `allow-http` | `bool` | `false` | Permit `http://` hosts for local testing. |

Metadata priority is **user `models` > built-in table**. Declared models absent from the built-in set and discovery are published with `UserDefined: true`; execution sends the declared `name` upstream.

## Model and protocol overrides

`models` entries match the upstream `name`; user metadata overrides the built-in table. `max-context-length`, `max-tokens`, and `thinking` describe catalog metadata: they do not expand upstream capabilities or automatically truncate requests. Explicit models absent from both the built-in set and discovery are marked `UserDefined: true`; requests send their original `name`.

`model-prefix.enabled: false` publishes bare IDs. When enabled, the prefix comes from `model-prefix.value`, defaulting to `minimax`.

```yaml
protocols:
  messages:
    endpoint: /anthropic/v1/messages
models:
  - name: MiniMax-M3
    max-tokens: 131072
    protocols:
      responses:
        enabled: false
      messages:
        endpoint: /anthropic/v1/messages
```

Endpoint paths resolve from model override → global `protocols` → protocol default. Global `protocols` only sets endpoints; use `enabled` under `models[].protocols` to switch a route. A listed protocol is enabled by default, and all paths must start with `/`. Disabling every protocol removes the model from the routable catalog.

`models[].thinking` accepts `min`, `max`, `zero-allowed`, `dynamic-allowed`, and `levels`. Omitted fields retain built-in values; explicit `0`, `false`, and empty lists preserve their configured meaning.

## Credential files

On start and reconfiguration, the plugin creates one CPA credential file per configured key, named after the key's `name` or a default label such as `MiniMax M Plan`, `MiniMax API CN`, or `MiniMax M Plan 2`. Records carry `type: minimax`, `api_key`, `region`, `key_type`, and optional `base_url`, so credentials added directly through CPA management work without a configuration entry:

```json
{"type": "minimax", "label": "Team plan", "api_key": "sk-cp-...", "region": "cn", "key_type": "subscription"}
```

For a key that is also configured, the configuration's `region`, `type`, `base-url`, and `priority` win over the record’s fields, including when the configured field takes a default value. Removing a key from the configuration does not delete its file; delete it through CPA management.

YAML uses `base-url` and `type: subscription|api`; credential JSON uses `base_url` and `key_type`, while JSON `type` must be `minimax`. Existing files and CPA-managed metadata are not overwritten on reconfiguration. A `priority` set directly on the credential file takes precedence over the plugin’s priority attribute.

## Accepted aliases

Prefer the canonical values above. Credential types and regions ignore case and surrounding whitespace.

| Setting | Canonical value | Aliases |
|---|---|---|
| Key `type` | `subscription` | `plan`, `m-plan`, `token-plan`, `coding-plan` |
| Key `type` | `api` | `pay-as-you-go`, `payg` |
| `region` | `global` | `intl`, `international`, `io` |
| `region` | `cn` | `china`, `mainland` |
