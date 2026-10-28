# cpa-minimax

<p align="center">
  <img src="assets/logo.png" alt="MiniMax" width="200" height="200">
</p>

<p align="center">
  <a href="README.md">English</a> · <strong>简体中文</strong>
</p>

<p align="center">
  <a href="#安装">安装</a> ·
  <a href="#配置">配置</a> ·
  <a href="docs/configuration.zh-CN.md">配置参考</a> ·
  <a href="#额度">额度</a> ·
  <a href="docs/protocol-conversion.zh-CN.md">协议说明</a> ·
  <a href="https://github.com/dillonzq/cpa-minimax/releases">版本发布</a>
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/github/license/dillonzq/cpa-minimax" alt="License: MIT"></a>
  <a href="https://help.router-for.me/plugin/development"><img src="https://img.shields.io/badge/CLIProxyAPI-8.0.0%2B-555" alt="CLIProxyAPI 8.0.0+"></a>
  <a href="go.mod"><img src="https://img.shields.io/badge/Go-1.26.10%2B-555" alt="源码构建需 Go 1.26.10+"></a>
</p>

[CLIProxyAPI](https://help.router-for.me/plugin/development)（CPA）的原生 Go 插件，通过同一个 provider 接入 MiniMax 订阅 key 和按量付费 key。

- 支持 Chat Completions、Messages、Responses、Gemini 客户端及流式输出，协议转换使用 [apix](https://github.com/dillonzq/apix/tree/v0.1.0)。
- 国际站和中国大陆站凭证共用一个池，支持全局及单 key 上游地址配置。
- 通过 CPA 优先级先使用订阅额度，按错误冷却凭证，并接入 CPA 原生额度查询。
- 内置 MiniMax-M3.1-Flash 和 MiniMax-M3，支持模型发现、思考控制及 Claude 格式 token 计数。

**插件 ID：** `cpa-minimax` · **Provider 及默认模型前缀：** `minimax`

## 要求

- **CLIProxyAPI**：`v8.0.0+`
- **仅源码构建**：Go 1.26.10+、启用 CGO，以及面向 CPA 宿主平台的 C 编译器。预编译发布库无需 Go 工具链或编译器。

## 安装

从 [GitHub Releases](https://github.com/dillonzq/cpa-minimax/releases) 下载对应版本的 ZIP 和 `checksums.txt`。请选择 **CPA 宿主的操作系统和架构**，容器部署以容器内环境为准：

| 系统 | 架构 | ZIP 后缀 | 库文件 |
|---|---|---|---|
| Linux | amd64 / arm64 | `linux_amd64.zip` / `linux_arm64.zip` | `cpa-minimax.so` |
| macOS | amd64 / arm64 | `darwin_amd64.zip` / `darwin_arm64.zip` | `cpa-minimax.dylib` |
| Windows | amd64 / arm64 | `windows_amd64.zip` / `windows_arm64.zip` | `cpa-minimax.dll` |
| FreeBSD | amd64 | `freebsd_amd64.zip` | `cpa-minimax.so` |

1. 校验下载：Linux 用 `sha256sum <archive>`，macOS 用 `shasum -a 256 <archive>`，PowerShell 用 `Get-FileHash <archive> -Algorithm SHA256`，并与 `checksums.txt` 比对。
2. 停止 CPA，备份 `config.yaml`、`auth-dir` 凭证目录和现有插件库。
3. 将库文件放入 `<cliproxyapi_root>/plugins/<os>/<arch>/`，例如 `plugins/linux/amd64/`。
4. 启用 `plugins.enabled` 和 `plugins.configs.cpa-minimax.enabled` 并添加 key（见[配置](#配置)）。
5. 重启 CPA，检查日志，用 CPA 客户端密钥验证 `GET /v1/models`，再发起一次真实请求。

## 配置

```yaml
plugins:
  enabled: true
  configs:
    cpa-minimax:
      enabled: true
      api-keys:
        - value: "${MINIMAX_API_KEY}"
```

启动 CPA 前，在其进程环境中设置 `MINIMAX_API_KEY`。中国大陆站 key 还需在本插件配置的 `enabled: true` 下添加 `region: cn`。请求 `/v1/models` 和推理接口时使用 CPA 客户端密钥；MiniMax key 只放在插件配置中。

默认地区为 `global`，两个插件开关都必须开启。用 `type: subscription` 或 `type: api` 显式声明计费类型，详见[凭证与轮换](#凭证与轮换)。混合地区、单 key `base-url`、全部配置项及凭证文件说明见[配置参考](docs/configuration.zh-CN.md)。

### 验证安装

重启 CPA 后，使用它面向客户端的访问地址和 **CPA 客户端密钥**。以下 shell 示例假设已设置 `CPA_BASE_URL` 和 `CPA_CLIENT_KEY`，地址末尾不带 `/`。

```sh
curl --fail-with-body "${CPA_BASE_URL}/v1/models" \
  -H "Authorization: Bearer ${CPA_CLIENT_KEY}"

curl --fail-with-body "${CPA_BASE_URL}/v1/chat/completions" \
  -H "Authorization: Bearer ${CPA_CLIENT_KEY}" \
  -H "Content-Type: application/json" \
  -d '{"model":"minimax/MiniMax-M3","messages":[{"role":"user","content":"Hello"}],"max_tokens":128}'
```

默认前缀下，模型列表应包含 `minimax/MiniMax-M3` 和 `minimax/MiniMax-M3.1-Flash`。列出模型只能检查注册情况；第二次调用还会验证所选 MiniMax 凭证及上游连通性。

## 凭证与轮换

### 凭证类型

| 类型 | `type` 取值 | 计费方式 | 默认优先级 |
|---|---|---|:-:|
| M Plan 订阅 key | `subscription` | 订阅额度 | `1` |
| 按量付费 API key | `api` | 账户余额 | `0` |

显式 `type` 优先；未设置时，`sk-cp-` 开头的 key 视为 `subscription`，其余视为 `api`。这只是本地前缀推断，不会调用 API 核验账户套餐；如果前缀与实际类型不符，请显式设置 `type`。M Plan 原称 Coding Plan / Token Plan。

### 地区

| 地区 | `region` 取值 | 默认主机 |
|---|---|---|
| 国际站 | `global`（默认） | `https://api.minimax.io` |
| 中国大陆站 | `cn` | `https://api.minimax.cn` |

key 必须使用所属站点，顶层 `region` 是未指定地区的 key 的默认值。上游地址优先级为 **单 key `base-url` → 显式全局 `base-url` → key 所属地区的默认地址**，见[地址配置示例](docs/configuration.zh-CN.md#地区与上游地址)。

### 轮换与冷却

CPA 在最高 `priority` 的可用凭证中轮换。默认订阅 key 为 `1`，API key 为 `0`；没有更高优先级的订阅凭证可用时，API key 才会参与调度。若要两类 key 一起轮换，为它们设置相同优先级。

插件负责分类 MiniMax 错误，CPA 负责重试和冷却。通过 CPA 管理接口设置的文件级 `priority` 优先于插件值；配置中的优先级变更在 CPA 重新解析凭证后生效，例如重启后。

配置中的 key 会生成 CPA 凭证文件。从 YAML 删除 key **不会**删除对应文件，请通过 CPA 管理接口删除。命名、字段优先级及手动添加方法见[凭证文件](docs/configuration.zh-CN.md#凭证文件)。

## 模型

| 模型 | 上下文 | 最大输出 | 思考档位 | 可关闭思考 |
|---|--:|--:|---|:-:|
| `MiniMax-M3.1-Flash` | 1,000,000 | 524,288 | low、medium、high、xhigh、max | ✗ |
| `MiniMax-M3` | 1,000,000 | 524,288 | none、low、medium、high、xhigh、max | ✓ |

以上为插件内置目录中的限额，不代表每个 key 都有访问权限。无需模型发现即可发布，配置禁用时除外。默认模型 ID 为 `minimax/<model>`，可通过 `model-prefix` 更改或移除前缀。

模型发现使用配置中的第一个 `api` key；没有 API key 时使用第一个 key，并访问该 key 解析后的上游地址。发现结果只补充可路由的 `MiniMax-M*` 模型，跳过无关的语音、视频和图像模型。发现失败时保留内置及显式声明的模型；是否继续保留上次发现结果由 `catalog.stale-while-unavailable` 决定。

M2.5 和 M2.7 默认不发布，仅在发现结果包含它们或通过 `models` 显式声明时出现。使用旧 ID 前请查看 MiniMax 的[模型生命周期公告](https://platform.minimax.io/docs/faq/models)。

### 思考行为

- **Chat `reasoning_split`** 默认为 `true`，思考内容进入 `reasoning_content`，而不是 `content` 中的 `<think>` 标签。M3.1 Flash 始终为 `true`。
- **M3.1 Flash** 始终思考。effort `none` 改为 `low`（CPA 对不能关闭思考的模型也这样处理），Messages 的 `thinking` 对象改为 `{"type":"adaptive"}`。
- **M3（Messages）** 未请求时不思考。effort `none` 发送 `thinking: {"type":"disabled"}`；其他 effort 开启自适应思考。
- **模型名后缀**如 `minimax/MiniMax-M3(high)` 或 `(none)` 替换请求体中的推理控制，路由时剥离后缀。

## 协议选择

插件为 `MiniMax-M*` 模型分配以下默认上游路由，实际可用性由 MiniMax 决定：

| 协议 | 端点 |
|---|---|
| Messages（Anthropic 兼容，MiniMax 推荐） | `/anthropic/v1/messages` |
| Chat Completions（OpenAI 兼容） | `/v1/chat/completions` |
| Responses | `/v1/responses` |

每个请求按客户端格式的顺序使用模型支持的第一个协议：

| 客户端格式 | 顺序 |
|---|---|
| Chat Completions | Chat Completions → Responses → Messages |
| Responses | Responses → Chat Completions → Messages |
| Messages | Messages → Responses → Chat Completions |
| Gemini | Messages → Responses → Chat Completions |

客户端自身协议无需转换。MiniMax 没有 Gemini 端点，Gemini 请求转为 MiniMax 推荐的 Messages（支持 thinking 块和交错思考）。所有协议都使用 `Authorization: Bearer` 鉴权。

通过 `models[].protocols` 开关单个模型的路由，通过 `protocols` 全局覆盖端点路径。示例见[模型与协议覆盖](docs/configuration.zh-CN.md#模型与协议覆盖)，字段映射和转换损失见[协议转换说明](docs/protocol-conversion.zh-CN.md)。

## 额度

先用 `GET /v0/management/quota/providers` 确认支持情况，再以 `{"auth_index":"<所选索引>"}` 调用 `POST /v0/management/quota/fetch`。两者都需要 CPA 管理密钥。插件只查询所选凭证。

**订阅 key** 在按上述优先级解析的主机上调用 `GET /v1/token_plan/remains`，必要时尝试旧接口 `/v1/api/openplatform/coding_plan/remains`。每个 `model_remains` 条目成为一个分组（例如 `MiniMax-M*`），包含：

| 桶 | 来源 | 重置时间 |
|---|---|---|
| `rolling` | `current_interval_total_count` / `current_interval_usage_count` | `end_time` |
| `weekly` | `current_weekly_total_count` / `current_weekly_usage_count` | `weekly_end_time` |

总量为 `0` 的窗口表示不限，予以省略。插件默认将 `*_usage_count` 解释为**剩余**次数；若你的账户返回的是已用次数，设置 `quota.usage-count: used`。无效读数返回错误，不会编造 0。

**按量 key** 只报告计划 `Pay-as-you-go`，不含用量桶。插件不查询账户余额；余额耗尽时通过错误 1008 使该 key 冷却（402）。请在 MiniMax 控制台查看余额。

鉴权失败会直接停止查询，不再尝试后续端点。不支持重置额度。

## 故障排查

先检查 CPA 日志，以及所选凭证的类型、地区和最终地址。插件优先使用 `base_resp.status_code` 或错误消息中的 MiniMax 错误码分类；即使 HTTP 成功，非零 `base_resp` 也会视为失败。以下为固定版本 CPA v8.0.0 宿主的默认行为。

| MiniMax 错误 | 含义 | 状态 | CPA 行为 |
|---|---|:-:|---|
| 1004、2049 | key 无效或地区错误 | 401 | 冷却 30 分钟 |
| 1008 | 按量余额不足 | 402 | 冷却 30 分钟 |
| 2057、2061、2068 | key 或套餐不能使用该端点或模型 | 403 | 冷却 30 分钟 |
| 2053、2056、2067 | 当前窗口的套餐用量或加油包耗尽 | 429 | 额度退避 |
| 1002、1041、1045、2045–2047、2062 | 限流，包括高峰期套餐限流 | 429 | 额度退避 |
| 1000、1001、1024、1033、1039、2064、2066、HTTP 529 | 临时错误或过载 | 503 | 短暂冷却后重试 |
| 2013 | 参数错误 | 400 | 不重试 |
| 2022、2069 | 模型不存在或已下线 | 404 | 不重试 |

| 现象 | 检查项 |
|---|---|
| 插件或模型未出现 | 库文件系统/架构是否匹配、两个 `enabled` 开关、启动日志及 `model-prefix`。 |
| `invalid_config` | 至少一个非空且不重复的 key；变量是否对 CPA 进程可见；未启用 `allow-http` 时必须使用 HTTPS。 |
| 1004 或持续 401 | key 是否有效、所属站点是否正确、全局或单 key 地址覆盖是否指向正确站点。 |
| 只有内置模型 | 检查发现日志及 `/v1/models` 权限；发现优先使用 API key。 |
| API key 没有额度桶 | 正常行为，按量 key 只显示计划。 |
| YAML 删除 key 后仍显示 | 通过 CPA 管理接口删除保留的凭证文件。 |

可复现问题请附脱敏配置、请求、版本和日志，提交 [issue](https://github.com/dillonzq/cpa-minimax/issues/new/choose)；漏洞请[私下报告](SECURITY.md)。

## 限制

- CPA 插件错误 envelope 不携带 retry-after，因此 429 使用 CPA 的额度退避，而非窗口的重置时间。
- 协议转换可能省略字段或拒绝无法表示的内容；目录中存在模型不代表上游一定允许访问。

## 开发

源码构建需要 Go 1.26.10+、CGO 和目标平台的 C 编译器。本地共享库使用 `debug` 标签构建：

```sh
go mod download all
go build -tags debug -buildmode=c-shared -o plugins/linux/amd64/cpa-minimax.so .
```

其他平台、测试命令、离线宿主 ABI 测试和发布检查见[贡献指南](CONTRIBUTING.md)。

## 文档

- [配置参考](docs/configuration.zh-CN.md)：完整示例、默认值、上游地址、模型覆盖及凭证文件。
- [协议转换说明](docs/protocol-conversion.zh-CN.md)：字段保留、推理回放、工具及流式限制。
- [贡献与发布检查](CONTRIBUTING.md) · [安全政策](SECURITY.md) · [发布记录](RELEASE_NOTES.md)。

## 许可证与商标

插件代码采用 [MIT 许可证](LICENSE)。MiniMax 官方 Logo 用于标识对接的上游服务，名称及图标不属于该许可证的授权范围。本项目由社区独立维护，与 MiniMax 无隶属关系，亦未获其背书。官方来源、商标说明及插件商店图标地址见[资源说明](assets/README.md)。
