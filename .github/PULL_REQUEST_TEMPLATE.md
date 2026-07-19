<!-- Thanks for contributing to mamori! Please fill this out so reviewers have context. -->

## What & why

<!-- What does this change do, and why is it needed? Link any related issue. -->

Closes #

## Type of change

- [ ] Bug fix (non-breaking)
- [ ] New feature (non-breaking)
- [ ] Breaking change
- [ ] New provider module
- [ ] Middleware
- [ ] Docs / examples
- [ ] CI / release / tooling

## Checklist

- [ ] `go test ./...` passes for every affected module (`make test`)
- [ ] `go vet ./...` and `golangci-lint run` are clean (`make lint`)
- [ ] New/changed behavior is covered by tests
- [ ] Public API changes have doc comments
- [ ] Secret values are never logged or rendered unredacted

## For new or changed providers

- [ ] The provider passes the `providertest` conformance kit (`providertest.Run`)
- [ ] `Resolve` returns an error satisfying `errors.Is(err, mamori.ErrNotFound)` for missing values
- [ ] `Value.Version` is set from a native revision (or `mamori.VersionHash`) and changes when the value changes
- [ ] Secret-bearing schemes set `Value.Sensitive = true`
- [ ] `Watch` is implemented only if the backend has native change notification
- [ ] README documents schemes, ref grammar, auth, and what is verified vs needs a live backend

## Notes for reviewers

<!-- Anything else worth calling out. -->
