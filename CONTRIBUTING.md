# Contributing to Peasant

Thank you for helping improve Peasant.

## Before you start

For a bug or feature, search the issue tracker before opening a new issue. For a substantial or
breaking change, open an issue first so the intended user behavior and cross-repository impact can
be discussed before implementation.

Do not post transcripts, tokens, credentials, personal paths, or other sensitive data. Follow
[`SECURITY.md`](SECURITY.md) for vulnerabilities.

## Development

1. Fork the repository and create a focused branch from the repository's default development branch.
2. Enter the development environment with `nix develop` when available.
3. Make a focused change with tests and documentation for user-visible behavior.
4. Run `make check`. It runs the race detector by default (`RACE=1`, i.e.
   `go test -race`). To save CI minutes, the detector runs in CI only on release
   pull requests, not on feature pull requests or merges, so **catching data
   races is a local developer responsibility**: run `make check` (or at least
   `go test -race ./...`) locally before you push. Use `make e2e` when changing
   the Village integration; see [`TESTING.md`](TESTING.md) for its additional
   prerequisites.
5. Open a pull request explaining the user impact, implementation, and exact validation performed.

Generated files should be changed through their generators. Run `make docs-cli` after changing the
CLI command tree. Wire-contract changes must first land in the public schema module and receive a
tag before this repository updates its dependency.

Repository-specific architecture, type-safety, fixture, and migration rules are in
[`AGENTS.md`](AGENTS.md).

## Review

Reviewers may ask for changes to correctness, tests, security, compatibility, or maintainability.
Keep pull requests small enough to review and avoid unrelated cleanup. By contributing, you agree
that your contribution is licensed under this repository's license.

All participation is governed by [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).
