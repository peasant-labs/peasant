# Installing Peasant with Nix

Peasant builds with `buildGoModule` and `CGO_ENABLED=0`, producing a fully static,
no-libc binary that runs unchanged on NixOS and non-NixOS hosts alike.

## Run without installing

```bash
nix run github:peasant-labs/peasant -- version
nix run github:peasant-labs/peasant -- ingest
```

## Install into a profile

```bash
nix profile install github:peasant-labs/peasant#peasant
peasant version
```

## Use in a flake

```nix
{
  inputs.peasant.url = "github:peasant-labs/peasant";

  outputs = { self, nixpkgs, peasant, ... }: {
    # e.g. add to a NixOS/home-manager package set:
    # environment.systemPackages = [ peasant.packages.${system}.peasant ];
  };
}
```

The package is exposed as `packages.<system>.peasant` (also the flake's `default`).

## Build locally

```bash
git clone https://github.com/peasant-labs/peasant.git
cd peasant
nix build .#peasant
./result/bin/peasant version
```

## Reinstalling or upgrading

`nix run` does not install a persistent binary; each invocation resolves the requested flake
reference. For a profile installation, use `nix profile upgrade peasant` for an existing profile
entry, or run `nix profile install github:peasant-labs/peasant#peasant` when the entry is not
present. Nix replaces the profile entry atomically and does not remove Peasant's configuration,
database, ingested data, or state.

For a flake or local build, update the input or source and run `nix build` again. Refresh the
profile or result link you use; rebuilding does not run kickstart or remove Peasant data.

## Verify and start guided setup

After `peasant version` succeeds for a profile or local installation, run the guided setup wizard:

```bash
peasant version
peasant kickstart
```

For a later setup run, read [kickstart rerun and reset behavior](../KICKSTART.md#reset-and-standalone-boundaries).

## Notes

- **`vendorHash` drift:** The flake's `vendorHash` is a fixed-output hash over the
  vendored Go module graph, so it changes only on `go.mod`/`go.sum` edits (including
  re-pinning the `github.com/peasant-labs/schema` module — a normal tagged dependency
  with no `replace`). A first-party edit to the schema module's sources does NOT move
  it. After editing dependency or package inputs, run `make nix-vendor-hash`; the
  release automation runs the same target before tagging. See the
  [release architecture](../release-architecture.md) for the full gate model.
- **Web dashboard in source builds:** the Nix build stubs the embedded `web/out`
  assets (the dashboard frontend depends on a pnpm/Next.js build that cannot run in the Nix
  sandbox yet). The CLI, ingest pipeline, analytics, and APIs all work; the embedded
  dashboard is a stub in Nix-built binaries. A real frontend derivation is a
  [deferred](../release-runbook.md#deferred-ladder) follow-up.
- **Version output:** `peasant version` reports a `git`-rev-suffixed version between
  tagged releases; tagged builds inject the exact version via ldflags.
- **nixpkgs:** upstreaming to `nixpkgs` (`nix profile install nixpkgs#peasant`) is a
  [deferred](../release-runbook.md#deferred-ladder) post-release task — it
  requires a public repo and a tagged release. The licensing requirement is already
  met: Peasant is Apache-2.0, which nixpkgs treats as free, so the package would be
  installable without `allowUnfree`.
