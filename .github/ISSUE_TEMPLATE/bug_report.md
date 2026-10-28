---
name: Bug Report
about: Report a reproducible problem with the plugin
title: "[BUG] "
labels: bug
assignees: ''
---

Do not include API keys, CPA client or management tokens, or personal data. Report vulnerabilities privately as described in [SECURITY.md](https://github.com/dillonzq/cpa-minimax/blob/main/SECURITY.md).

### Problem

Describe the expected behavior and what happened instead.

### Reproduction

- Client endpoint or format: Chat Completions / Messages / Responses / Gemini
- Model ID, including any thinking suffix:
- Streaming: yes / no
- Credential type and region: subscription / api; global / cn
- Shared or per-key `base-url` override, if used (remove sensitive host details):
- Minimal sanitized plugin configuration and request:
- Exact HTTP status, MiniMax error code, and relevant sanitized log output:

### Environment

- CLIProxyAPI version:
- Plugin version or commit:
- CPA host OS and architecture (inside the container, if applicable):
- Installation: release ZIP / source build
- Go version and C compiler (source builds only):

### Checks performed

<!-- For example: confirmed key region, checked CPA startup logs, tested /v1/models and a request. -->
