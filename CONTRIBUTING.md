# Contributing

[README](README.md) · [Configuration reference](docs/configuration.md) · [Protocol conversion](docs/protocol-conversion.md)

Submit issues and pull requests to [dillonzq/cpa-minimax](https://github.com/dillonzq/cpa-minimax). For vulnerabilities, follow the [security policy](SECURITY.md).

## Development environment

Use Go 1.26.10 or later, Git, CGO, and a C compiler targeting the CPA host’s OS and architecture. The supported Go version is declared in [go.mod](go.mod).

Download dependencies before running the offline suite:

```sh
go mod download all
```

Tests use mock upstreams and disposable local credential files. Do not use live MiniMax credentials or external APIs in automated tests. Local loopback networking must be available.

## Build a local plugin

Use the `debug` build tag for local shared-library builds. CI handles production release builds.

```sh
# Linux amd64
go build -tags debug -buildmode=c-shared -o plugins/linux/amd64/cpa-minimax.so .
```

Use the output path appropriate for the target host:

| Host | Example output path |
|---|---|
| Linux arm64 | `plugins/linux/arm64/cpa-minimax.so` |
| macOS arm64 | `plugins/darwin/arm64/cpa-minimax.dylib` |
| macOS amd64 | `plugins/darwin/amd64/cpa-minimax.dylib` |
| Windows amd64 | `plugins/windows/amd64/cpa-minimax.dll` |
| Windows arm64 | `plugins/windows/arm64/cpa-minimax.dll` |
| FreeBSD amd64 | `plugins/freebsd/amd64/cpa-minimax.so` |

Changing the output path does not cross-compile the library: the Go target and C toolchain must also match. Stop CPA before replacing a loaded library, then restart it with the [minimal configuration](README.md#configuration).

Local builds report `0.0.0-dev` by default. To set a development version, add `-ldflags "-X github.com/dillonzq/cpa-minimax/internal/plugin.pluginVersion=<version>"` to the build command.

## Validate changes

Run from the repository root:

```sh
go test -tags debug ./...
go test -tags debug -race ./...
go test ./.github/scripts/...
go vet -tags debug ./... ./.github/scripts/...
```

The race detector requires a supported platform and CGO toolchain. [CI](.github/workflows/build.yml) also runs `go test ./...` to check the production build configuration.

On Linux and macOS with CGO, the debug suite builds a temporary C-shared library and loads it through the pinned CLIProxyAPI SDK’s real plugin host. [tests/native_host_test.go](tests/native_host_test.go) adds [host tests](tests/testdata/pluginhost/native_host_abi_test.go) through a Go overlay in a temporary SDK module. It reads SDK sources from the module cache, copies only the module manifests, and disables dependency downloads. On other platforms, verify a local debug shared-library build as well.

## Pull requests

Keep each PR focused on one behavior or fix. Explain the problem, the resulting behavior, and the validation performed. Include relevant compatibility limits or known gaps.

- Format Go changes with `gofmt`.
- Add meaningful tests for changed behavior. Protocol changes need both streaming and non-streaming coverage.
- Keep automated tests offline and remove secrets from reports, examples, and fixtures.
- Update both READMEs for user-visible changes. Configuration changes also require both [English](docs/configuration.md) and [Chinese](docs/configuration.zh-CN.md) references; protocol changes require both conversion guides.
- Preserve the distinction between YAML key `type` and credential JSON `key_type`, and between YAML `base-url` and JSON `base_url`.

The [pull request template](.github/pull_request_template.md) lists the validation to report. For documentation-only changes, validate affected links and examples and state which runtime checks are unnecessary.

## Release notes and release checks

[RELEASE_NOTES.md](RELEASE_NOTES.md) records user-facing features and fixes. Omit maintenance-only changes such as documentation cleanup, test additions, and workflow adjustments. Use `# vX.Y.Z` headings, newest first; the release tag must match the first heading.

Before a release:

1. Confirm the version section describes the shipped behavior and requirements.
2. Run the validation commands above and check CI’s production configuration tests.
3. Check that the [release matrix](.github/workflows/build.yml) matches the README’s download table.
4. Preview the release body for the intended tag:

   ```sh
   go run ./.github/scripts/release-notes \
     -notes RELEASE_NOTES.md -tag v0.1.0 -out /tmp/cpa-minimax-release-notes.md
   ```

   Use a temporary output path appropriate for your platform and replace the example tag with the release version.

A stable version tag triggers the workflow’s platform builds. CI packages each shared library with `LICENSE`, generates SHA256 checksums, and prepares a **draft** GitHub release. Review the assets and notes before publishing. CI extracts only the matching version section and refuses to replace assets on an already published release.
