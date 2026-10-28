Read [CONTRIBUTING.md](../CONTRIBUTING.md) before submitting.

## Summary

<!-- Describe the concrete problem and resulting behavior. Link the related issue if any. -->

## Validation

<!-- Report results or explain why a check does not apply (for example, a documentation-only change). -->

- [ ] `go test -tags debug ./...`
- [ ] `go test -tags debug -race ./...`
- [ ] `go test ./.github/scripts/...`
- [ ] `go vet -tags debug ./... ./.github/scripts/...`
- [ ] Changed behavior has meaningful tests; protocol changes cover streaming and non-streaming.
- [ ] Both READMEs and affected configuration/protocol references are consistent.
- [ ] `RELEASE_NOTES.md` covers user-facing changes; maintenance-only changes are omitted.

## Compatibility and limitations

<!-- Include configuration migration, host requirements, or known gaps when relevant. -->
