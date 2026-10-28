# 配置参考

[English](configuration.md) · [返回 README](../README.zh-CN.md#配置)

插件配置位于 CPA `config.yaml` 的 `plugins.configs.cpa-minimax` 下，`plugins.enabled` 和 `plugins.configs.cpa-minimax.enabled` 都必须为 `true`。示例使用规范名称，兼容别名见文末。

## 完整配置示例

```yaml
plugins:
  enabled: true
  configs:
    cpa-minimax:
      enabled: true
      region: global                 # key 的默认地区：global | cn

      # 可选全局主机覆盖；省略时使用各 key 所属地区的默认地址
      # base-url: https://proxy.example.com

      api-keys:
        - value: "${MINIMAX_PLAN_KEY}"
          name: "团队套餐"
          region: cn
          # base-url: https://api.minimaxi.com  # 可选，覆盖此 key 的地址
          type: subscription         # subscription | api；默认按 sk-cp- 前缀判断
        - value: "${MINIMAX_PLAN_KEY_2}"
          type: subscription
        - value: "${MINIMAX_API_KEY}"
          name: "按量付费"
          type: api
          priority: 0                # 默认：订阅 1，API 0；越大越优先

      model-prefix:
        enabled: true                # false 时发布不带前缀的模型 ID
        value: minimax

      catalog:
        discover: true               # 在内置模型之外调用 /v1/models
        refresh-interval: 15m        # 最小 1m
        stale-while-unavailable: true

      protocols:                     # 所有模型的端点路径
        messages:
          endpoint: /anthropic/v1/messages

      models:                        # 覆盖与额外声明
        - name: MiniMax-M3
          display-name: MiniMax M3
          max-tokens: 131072

      quota:
        usage-count: remaining       # remaining | used
        endpoints:                   # 依次尝试
          - /v1/token_plan/remains
          - /v1/api/openplatform/coding_plan/remains

      request-timeout: 5m
      max-response-bytes: 67108864
      allow-http: false
```

## 地区与上游地址

每个 key 的地区先取自身 `region`，缺省时取顶层 `region`，默认 `global`。地址按以下顺序解析：

1. 此 key 显式设置的 `base-url`。
2. 显式设置的全局 `base-url`。
3. 此 key 所属地区的默认地址：`global` 为 `https://api.minimax.io`，`cn` 为 `https://api.minimax.cn`。

全局地址省略时，混合地区的 key 分别使用自己的站点；显式设置全局地址后，所有没有单独覆盖的 key 都使用它，即使地区不同。请求执行、流式输出、token 计数、模型发现和订阅额度查询使用同一地址规则。

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

此处国际站 API key 使用 `https://api.minimax.io`，中国站订阅 key 使用它自己的旧站点地址覆盖。上述片段放在 `plugins.configs.cpa-minimax` 下。

全局及单 key 地址都要求 HTTPS；本地测试时可用 `allow-http: true` 允许 HTTP。地址不能含用户信息、查询参数或片段；末尾 `/`、`/v1`、`/anthropic` 会按规范化规则去除，端点路径由插件追加。可保留代理路径前缀，如 `https://proxy.example.com/minimax`。省略可选地址即可使用默认值；显式空字符串会报错。

只有 key 的 `value` 支持 `${ENV_VAR}` 展开；`base-url` 等其他字段不展开环境变量。变量必须对运行 CPA 的进程可见。

## 配置项

| 配置项 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `api-keys` | `[]object` | *（必填）* | 包含 `value` 及可选 `name`、`type`、`region`、`base-url`、`priority`。`value` 支持 `${ENV_VAR}`；重复值和空值被拒绝。 |
| `api-keys[].name` | `string` | 自动生成 | 可选显示名称及初次生成的凭证文件名。 |
| `api-keys[].type` | `string` | 按前缀 | `subscription` 或 `api`；见[凭证类型](../README.zh-CN.md#凭证类型)。 |
| `api-keys[].region` | `string` | `region` | `global` 或 `cn`。 |
| `api-keys[].priority` | `int` | `1` / `0` | CPA 优先级，越大越优先。订阅默认 1，API 默认 0。 |
| `region` | `string` | `global` | 未指定地区的 key 的默认地区。 |
| `base-url` | `string` | 由 `region` 决定 | 全局主机覆盖；省略时各 key 使用所属地区的默认地址（`https://api.minimax.io` 或 `https://api.minimax.cn`）。需 HTTPS（`allow-http` 时可用 HTTP），不能含查询、片段或用户信息；末尾 `/v1` 或 `/anthropic` 会被去除。 |
| `api-keys[].base-url` | `string` | `base-url` / key 的地区 | 单 key 可选地址覆盖，优先于全局 `base-url`，URL 规则相同。 |
| `model-prefix.enabled` | `bool` | `true` | `true` 发布 `<prefix>/<model>`，`false` 发布裸 ID。 |
| `model-prefix.value` | `string` | `minimax` | 模型前缀。 |
| `catalog.discover` | `bool` | `true` | 在内置模型之外调用 `/v1/models`。 |
| `catalog.refresh-interval` | `duration` | `15m` | 发现间隔，最小 `1m`。 |
| `catalog.stale-while-unavailable` | `bool` | `true` | 发现失败时保留上次成功的列表。 |
| `protocols.<protocol>.endpoint` | `string` | 协议默认值 | 所有模型的端点路径。 |
| `models` | `[]object` | `[]` | 按上游 `name` 覆盖元数据或声明模型：`display-name`、`description`、`max-context-length`、`max-tokens`、`input-modalities`、`output-modalities`、`thinking`（`min`、`max`、`zero-allowed`、`dynamic-allowed`、`levels`）及 `protocols`。 |
| `quota.usage-count` | `string` | `remaining` | 如何解释 remains 接口的 `*_usage_count`。 |
| `quota.endpoints` | `[]string` | 见示例 | 依次尝试的 remains 端点。 |
| `request-timeout` | `duration` | `5m` | 打开并读取上游请求的截止时间。 |
| `max-response-bytes` | `int64` | `67108864` | 响应的最大字节数；流式响应累计所有分块。 |
| `allow-http` | `bool` | `false` | 本地测试时允许 `http://` 主机。 |

元数据优先级为**用户 `models` > 内置表**。不在内置集合和发现结果中的声明模型以 `UserDefined: true` 发布；执行时向上游发送声明的 `name`。

## 模型与协议覆盖

`models` 按上游模型 `name` 匹配，元数据优先级为用户配置高于内置表。`max-context-length`、`max-tokens` 和 `thinking` 用于目录元数据，不会扩大上游实际能力，也不会自动截断请求。未出现在内置表或发现结果中的显式模型标记为 `UserDefined: true`，请求发送原始 `name`。

`model-prefix.enabled: false` 发布不带前缀的 ID；启用时使用 `model-prefix.value`，默认为 `minimax`。

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

端点路径优先级为模型级覆盖 → 全局 `protocols` → 协议默认路径。全局 `protocols` 只能设置端点；在 `models[].protocols` 中使用 `enabled` 开关协议。列出的协议默认为开启，所有路径必须以 `/` 开头。禁用一个模型的全部协议会将它从可路由目录中移除。

`models[].thinking` 支持 `min`、`max`、`zero-allowed`、`dynamic-allowed`、`levels`。省略字段保留内置值，显式 `0`、`false` 或空列表保留其配置含义。

## 凭证文件

启动和重新配置时，插件为每个配置的 key 创建一个 CPA 凭证文件，文件名取自 key 的 `name`，或 `MiniMax M Plan`、`MiniMax API CN`、`MiniMax M Plan 2` 等默认标签。记录包含 `type: minimax`、`api_key`、`region`、`key_type` 及可选的 `base_url`，因此直接通过 CPA 管理接口添加的凭证无需配置条目也能使用：

```json
{"type": "minimax", "label": "团队套餐", "api_key": "sk-cp-...", "region": "cn", "key_type": "subscription"}
```

对于同时出现在配置中的 key，配置里的 `region`、`type`、`base-url`、`priority` 优先于文件内对应字段，包括配置采用默认值的情况。从配置中删除 key 不会删除其文件，请通过 CPA 管理接口删除。

YAML 使用 `base-url` 和 `type: subscription|api`；凭证 JSON 使用 `base_url` 和 `key_type`，JSON 的 `type` 必须为 `minimax`。重新配置不会覆盖现有文件及 CPA 管理的元数据；直接在凭证文件中设置的 `priority` 优先于插件提供的默认属性。

## 兼容别名

推荐使用上文的规范值。类型及地区在解析时忽略大小写和首尾空白。

| 配置 | 规范值 | 兼容别名 |
|---|---|---|
| key 的 `type` | `subscription` | `plan`、`m-plan`、`token-plan`、`coding-plan` |
| key 的 `type` | `api` | `pay-as-you-go`、`payg` |
| `region` | `global` | `intl`、`international`、`io` |
| `region` | `cn` | `china`、`mainland` |
