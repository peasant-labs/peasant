# Peasant Release Runbook

This runbook is the operator's guide for cutting Peasant releases and for the
external package-publication rollout. It documents the release ceremony, the
secrets/permissions inventory, the publication checklist, and the
deferred work ladder.

For the maintainer-facing system reference — workflow ownership, release data flow,
package consumers, and why the Nix hash gate sits before tag publication — see
[Release Architecture](release-architecture.md).

> **Status:** the release pipeline is configured and locally validated. The first
> public-root tag uses the exact `v0.1.0` initial-final bootstrap. Downstream AUR and
> Homebrew publication stays disabled until the publication checklist is complete
> (`skip_upload: true`).

---

## 1. The release model

One canonical **GitHub Release** per tag is the single producer; every package
ecosystem (deb, rpm, AUR, Homebrew cask, nix) is a downstream consumer of the same
frozen artifacts.

```
release PR (title: "release(vX.Y.Z[-rcN]): …")
   │  merged by a maintainer (approval assertion deferred while only one maintainer is active)
   ▼
release-pr.yml  ──▶ runs make nix-vendor-hash
   │                 commits "chore: update nix vendor hash" if flake.nix changed
   ▼
annotated tag vX.Y.Z[-rcN] on the hash-current commit (App token)
   │
   ▼ (tag push triggers)
release.yml     ──▶ guard → nix vendorHash freshness gate → full-stack e2e
   │                                                     │
   │                                                     ├──▶ release e2e (installed packages)
   │                                                     ▼
   │                                                  goreleaser → smoke
   │                 builds 4 static targets, archives, checksums,
   │                 .deb/.rpm, and (after separate publisher enablement) AUR + cask
   ▼
GitHub Release (prerelease for -rcN; full release for final)
```

The gating logic lives in the canonical, shared **`github.com/peasant-labs/schema/cmd/release-guard`**
tool (shared with the schema repo — peasant no longer carries its own copy), which keeps it
testable instead of buried in workflow bash:

- **`github.com/peasant-labs/schema/cmd/release-guard`** — the shared CLI that owns the
  typed `ReleaseKind` grammar (`KindRC` / `KindFinal` / `KindInvalid`), the `Version`
  newtype, and the `ParseReleaseTitle` / `ParseTag` / guard functions. The workflows invoke
  it via `go run github.com/peasant-labs/schema/cmd/release-guard …`
  (`… parse-title`, `… parse-tag`).
- **`.github/release-guard.policy.yml`** — peasant's per-repo policy file. It declares this
  repo's release-pipeline publication gates (the `e2e` / `release-e2e` reusable-workflow
  gates and the goreleaser `release` job graph) so the shared `release-guard check-workflow`
  can validate `.github/workflows/release.yml` without any peasant-specific job-shape
  knowledge baked into the tool.

Implementing workflows: `.github/workflows/release-pr.yml` (tag minting),
`.github/workflows/release.yml` + `.goreleaser.yml` (build + publish),
`.github/workflows/release-validate.yml` (per-distro install validation on release-PRs
and on the rc path).

### Publication gates before Goreleaser

`release.yml` treats the Goreleaser `release` job as the publisher. It must stay behind
every source-level publication gate:

- `guard` rejects tags whose push actor is not `peasant-labs-releaser[bot]`
  (actor ID `291504229`) and malformed/non-Peasant tags. It does not require a prior
  rc for final tags.
- `nix-vendor-hash` proves the tag points at a source tree with a current `flake.nix`
  vendor hash.
- `e2e` calls the reusable `.github/workflows/e2e.yml` full-stack harness on every
  release tag, not only rc tags. That harness builds Peasant and the village server
  from source, provisions Postgres + MinIO with podman, drives ingest and
  `peasant village push`, verifies the skip-gate/retraction path, and asserts the
  village server-side secret scan.
- `release-e2e` calls the reusable `.github/workflows/release-e2e.yml`
  installed-binary/per-distro gate on every release tag, not only rc tags. It builds
  release snapshot artifacts and installs them across the supported distro paths
  before Goreleaser can publish the real release.

### Release-PR title grammar (frozen contract)

```
release(vX.Y.Z): <summary>        → KindFinal
release(vX.Y.Z-rcN): <summary>    → KindRC
```

Anything else is rejected: `released(...)`, `release(0.1.0): …` (missing `v`),
and hyphenated garbage. The grammar is implemented **once** in the
shared `github.com/peasant-labs/schema/cmd/release-guard` tool and consumed by both
workflows via `go run`.

### Tag namespaces

- `vX.Y.Z` and `vX.Y.Z-rcN` → Peasant releases (annotated, immutable).
- Schema module tags are created in the standalone public schema repository and cannot
  trigger Peasant's `release.yml`.

---

## 2. Secrets & permissions inventory

| Secret / setting | Where | Purpose | Set when |
|------------------|-------|---------|----------|
| `PEASANT_RELEASER_APP_ID` / `PEASANT_RELEASER_APP_PRIVATE_KEY` for GitHub App ID `3988034` (`peasant-labs-releaser`, `Contents: write`) | Repo secrets for release/write workflows | Push release-PR hash-fix commits and annotated tags, update the standalone Nix vendor hash, and mint the Homebrew tap token | Before the first real tag |
| `GITHUB_TOKEN` (default) | Per-workflow, automatic | Maintainer permission lookup (`gh api repos/{repo}/collaborators/{user}/permission`); read collaborators (needs push, which the workflow token has) | Already present |
| `AUR_KEY` | Repo secret | Unencrypted ed25519 **private** key for `ssh://aur@aur.archlinux.org/peasant-bin.git`; goreleaser `aurs` writes it to a temp file and sets `GIT_SSH_COMMAND` | When AUR publication is separately approved |
| `v*` tag ruleset | Repository settings | Rejects tag creation, update, deletion, and non-fast-forward changes from every actor except GitHub App ID `3988034` (`peasant-labs-releaser`) | Before the first real tag |

**Manual, one-time, UI-only step:** reconfirm that the
`peasant-labs-releaser` installation has `Contents: write` reach for both
`peasant-labs/peasant` and `peasant-labs/homebrew-tap`. E2E checks out the public
Village repository and public Go modules with read-only `GITHUB_TOKEN` access and no PAT.

The workflows use Blacksmith's amd64 and arm64 runner labels declared in
`.github/actionlint.yaml`. Before enabling Actions on a newly promoted repository,
grant the Blacksmith GitHub App access to it and verify that a smoke workflow can
acquire each declared label. Jobs remaining queued indicate missing runner access,
not a test failure; do not cut a release until all required labels schedule.

### Maintainer gating

Authorization is GitHub collaborator **permission** (`admin` or `maintain`), not a
checked-in `MAINTAINERS` file:

- On release-PR open/edit: the PR **author** must be `admin`/`maintain`, and the title
  must parse.
- On merge: a pre-existing tag is a **hard fail**. The at-least-one-APPROVED-review
  assertion remains **deferred**: with a single active maintainer,
  GitHub's no-self-approval rule makes it unsatisfiable. `release-guard check-approval` remains
  implemented + tested, but is not part of the current release workflow.

Branch protection on `develop` and the App-only `v*` tag ruleset are defense-in-depth
configured separately in GitHub. `release.yml` also checks the App's stable login and
actor ID before processing a tag. If the App identity changes, update the ruleset,
workflow assertion, fixture, and this runbook together.

---

## 3. Cutting the initial final or a release candidate

### Initial `v0.1.0` bootstrap record

The public-root repository began without inherited product tags, so release PR #18
used the exact `--initial-final v0.1.0` exception instead of manufacturing an rc for
already validated private history. The guard proved the complete `v*` namespace was
empty and no `v0.1.0` Release existed before the releaser App created the annotated
tag. Final releases no longer use this bootstrap-only path. The `skip_upload: true`
settings kept AUR and Homebrew untouched by this final.

### `v0.1.0` startup recovery record

The App-created `v0.1.0` tag points to commit
`807a1b68c8ec1952db6c289f383f42cbb0701db9`, but its first Release run
(`30946834984`) ended with `startup_failure` before GitHub created any jobs. The
cause was missing caller permissions for reusable workflows. PR #20 fixed that
graph for future tags, but GitHub reruns retain the workflow stored at the
original tag and therefore cannot consume the fix.

PR #21 added a reviewed, expiring recovery path for this exact incident. Its first
dispatch, run [`30952716604`](https://github.com/peasant-labs/peasant/actions/runs/30952716604),
failed closed in the GitHub-evidence step because the repository `GITHUB_TOKEN`
redacted `bypass_actors` from the ruleset response. Nix, publication, and smoke were
all empty and skipped, and no Release was created.

PR #22 bound the redacted response to the admin-verified ruleset node and exact
creation/update timestamps, required `current_user_can_bypass: never`, and proved
the failed attempt never reached mutable work. Replacement run
[`30954397604`](https://github.com/peasant-labs/peasant/actions/runs/30954397604)
executed at recovery commit `73ea8eeb40904e439df9d939569c17959c8edca5`, using
exact-tag E2E run `30948998607` and Release E2E run `30948998662`. Preflight, Nix
vendor-hash freshness, publication, and native amd64/arm64 smoke all passed on the
first attempt.

The resulting full [v0.1.0 Release](https://github.com/peasant-labs/peasant/releases/tag/v0.1.0)
contains four archives, two `.deb` packages, two `.rpm` packages, and
`checksums.txt`. All eight artifact checksums verify; the checksum manifest's
SHA-256 digest is `aad6b68481d61691b0f30a92c80301895210da049185d87878cad37f89d94dbd`.
The annotated tag object remains `b1f8fe4b9a40ac32c1d7e1a8748cb11575595c4f`
and still peels to `807a1b68c8ec1952db6c289f383f42cbb0701db9`. AUR and
Homebrew remained disabled. The one-time workflow and verifier were removed after
success, so this incident record is not an executable redispatch procedure.

### Later release candidates

1. Ensure `develop` is green and contains everything the rc should include.
2. Open a PR into `develop` titled exactly:
   `release(vX.Y.Z-rc1): <summary>` (bump `rcN` for subsequent candidates).
   - `release-pr.yml` (open/edit trigger) validates the title grammar and that **you**
     are an `admin`/`maintain` collaborator. Fix the title or authorship if it fails.
   - `release-validate.yml` (path-filtered to packaging-relevant files — `**.go`,
     `.goreleaser.yml`, flake/`go.mod`/`go.sum`/web manifests, `Makefile`; a real
     release PR always matches) runs the per-distro install matrix against a goreleaser
     **`--snapshot`** build (synthetic version): deb 2×2 (ubuntu 22.04/24.04 ×
      amd64/arm64), rpm (fedora `dnf` + leap `zypper modifyrepo --disable --all`
      followed by `zypper --no-refresh --allow-unsigned-rpm`), Arch
     `makepkg` (x86_64), `brew style`, the **rc-only** macOS cask install, and
     `nix build .#peasant`.
3. **Merge** (the approval assertion is deferred during the single-maintainer
   period - §2).
   - `release-pr.yml` (merge trigger) checks out the merge
     commit, runs `make nix-vendor-hash`, and mints the annotated tag using the App
     token. If `flake.nix` changes, it first commits
     `chore: update nix vendor hash` to `develop` and tags that hash-fix commit. It
     **hard-fails if the tag already exists**.
4. The tag push triggers `release.yml`:
   - **guard** job: verifies the tag push actor is `peasant-labs-releaser[bot]`
     (actor ID `291504229`); for an rc, it then proceeds after parsing the tag.
   - **nix-vendor-hash** job: re-runs `make nix-vendor-hash` and fails if `flake.nix`
      would change. This catches any release-ceremony tag not created from a
      hash-current commit.
   - **full-stack e2e** job: calls `.github/workflows/e2e.yml` and must produce a
     positive `--- PASS: TestSkipGateE2E` line. A skipped harness or `[no tests to
     run]` blocks publication because no product e2e coverage was proven.
   - **release e2e** job: calls `.github/workflows/release-e2e.yml` and must produce
     a positive `--- PASS: TestReleasePerDistro` line. This proves installed package
     artifacts across the per-distro release paths before publication.
   - **goreleaser** job (Blacksmith amd64, `CGO_ENABLED=0`): builds the 4 static
     targets, archives, `checksums.txt`, `.deb`/`.rpm`. Marks the GitHub Release as a
     **prerelease**. With `skip_upload: true`/`auto`, the AUR and tap are **untouched**.
   - **smoke** job (native amd64 + arm64): asserts the binary is static (`ldd`) and
     `peasant version` output contains the tag (substring check `grep -qF "${TAG#v}"`).
5. Verify the prerelease on the Releases page: 4 archives + 2 `.deb` + 2 `.rpm` +
   `checksums.txt`, and **nothing** pushed to AUR/tap.

---

## 4. Promoting to a final release

This section describes finals after the exact initial `v0.1.0` bootstrap.

1. Open a PR into `develop` titled `release(vX.Y.Z): <summary>`. Same validation as an
   rc, plus `release-validate.yml` skips the macOS cask install (rc-only).
2. Merge (approval assertion deferred during the single-maintainer period - §2).
   `release-pr.yml` updates the Nix vendor hash if
   needed and mints the annotated final tag on the hash-current commit.
3. `release.yml` runs; the **guard** job verifies the release App actor and tag grammar,
   followed by the tag-time **nix-vendor-hash** freshness gate and the full-stack
   e2e and installed-package release e2e publication gates. On success goreleaser
   publishes a **full** (non-prerelease) Release. AUR and Homebrew remain untouched
   while their explicit `skip_upload: true` safety settings remain in force. The smoke
   job re-checks static linkage + `peasant version`.
4. Verify the full Release and complete artifact set. Verify AUR, the tap, and nixpkgs
   only after their separate publication checklist items are approved and enabled.

---

## 5. Release guard rules

The `release.yml` **guard** job verifies that the tag push came from the release App
and that the tag parses as a Peasant rc or final release. Final tags can publish full
releases without a prior rc. RC tags still publish prereleases and still run the
rc-only packaging validation matrix.

### Nix vendorHash freshness gate

`release.yml` also runs `make nix-vendor-hash` before Goreleaser. If that command
would edit `flake.nix`, the release stops before publishing and reports that the tag
was created from a stale source tree. Do not repair a published tag in place: update
`develop` with the current vendor hash and cut a new release tag.

The companion `nix-vendor-hash.yml` workflow keeps `develop` warm after dependency
or package-input changes land. It runs on pushes to `develop` touching Go module
files, web package manifests/locks, flake files, the `Makefile`, the
hash update script, or the workflow itself. Release tagging still performs its own
pre-tag update because a tag must point at immutable, hash-current source.

---

## 6. Publication checklist

Run these, in order, when enabling external package publication.

- [x] **FINAL LICENSE DECISION — settled: Apache-2.0.** `LICENSE` is the verbatim
      Apache License 2.0 text, with the appendix boilerplate filled in as
      `Copyright 2026 Peasant Labs`. It is **byte-identical** to the `LICENSE` shipped
      by every sibling repository (`schema`, `village`, `fairtrade-design-system`,
      `transcript-browser`) — automated license detection matches on byte-identity, so
      keep them in lockstep and change them together.
      Every packaging metadata reference declares the matching stock SPDX id: nfpm
      `license: "Apache-2.0"`, `aurs.license` (→ PKGBUILD `license=()`), the cask
      `license`, and flake `meta.license = licenses.asl20` (free, so nixpkgs
      default-installable). If the license is ever revisited, all four move together.
      Verify agreement with:

      ```bash
      grep -n 'license:' .goreleaser.yml     # expect three "Apache-2.0" lines
      nix eval --json .#packages.x86_64-linux.peasant.meta.license   # expect spdxId Apache-2.0, free true
      ```

      Peasant does not ship a `NOTICE` file. Apache-2.0 §4(d) applies to downstream
      redistribution of a notice only when the work includes one.
- [x] **MAINTAINER CONTACT.** Package metadata publishes a project role address
      (deb `Maintainer:` and AUR PKGBUILD `# Maintainer:` lines):
      - nfpm `maintainer:` — **single-valued** (deb permits one `Maintainer:`), so it
        carries the role address alone.
      - `aurs.maintainers:` — a **list**; AUR emits one `# Maintainer:` per entry
        and uses the same project role address, obfuscated per AUR convention.

      The role address must remain reachable for users and downstream packagers.
- [x] **RELEASE IDENTITIES.** Keep these three roles distinct:
      - Tag provenance is the GitHub event actor `peasant-labs-releaser[bot]`
        (actor ID `291504229`), enforced by the App-only tag ruleset and
        `release.yml`.
      - Automated Git commits use
        `peasant-release-bot <noreply@peasantlabs.org>`. This exact author appears
        in both `release-pr.yml` commit/tag sites, `nix-vendor-hash.yml`, and the
        AUR and Homebrew `commit_author` blocks in `.goreleaser.yml`. If one of
        these five sites changes, update all five.
      - Package maintainer metadata uses the project contact
        `Peasant Labs <admin@peasantlabs.org>` (obfuscated where AUR requires it).
        Neither bot identity is a maintainer contact.
- [x] Make the repository **public**.
- [ ] Confirm the GitHub App has `Contents: write` on `peasant-labs/peasant` and
      `peasant-labs/homebrew-tap` (§2).
- [x] Configure branch protection on `develop` as defense-in-depth. Do not add the
      self-approval gate while the single-maintainer constraint remains.
- [x] Configure the `v*` tag ruleset so only GitHub App ID `3988034`
      (`peasant-labs-releaser`) can bypass tag creation/update/deletion protection.
      Administrators must not bypass it.
- [ ] **AUR:** create the `aur.archlinux.org` account, register a **dedicated** ed25519
      public key, and add `AUR_KEY` (the private key) as a repo secret. The first
      `goreleaser` push to `ssh://aur@aur.archlinux.org/peasant-bin.git` **creates**
      the package (no web form). The first AUR push must use a separately approved
      later final release; `v0.1.0` keeps uploads disabled.
- [ ] **Homebrew tap:** create the `peasant-labs/homebrew-tap` repository and grant
      the App `Contents: write` on it. `release.yml` mints the short-lived tap token
      at release time; there is no long-lived `TAP_GITHUB_TOKEN` secret.
- [ ] **Flip `skip_upload`:** change `aurs` and `homebrew_casks` `skip_upload` from
      `true` to **`auto`** in `.goreleaser.yml`. (`auto` additionally keeps prereleases
      off the package repos forever — rc tags never touch AUR/tap.)
- [ ] **nixpkgs:** open the `pkgs/by-name/pe/peasant/package.nix` PR (requires public
      repo + finalized license + a tagged release). Add yourself to the maintainer
      list; use `versionCheckHook` + `nix-update-script`.
- [ ] **Live verification (after external publishers are enabled):**
      - `go install github.com/peasant-labs/peasant/cmd/peasant@v0.1.0` compiles and
        `peasant version` reports `v0.1.0` (the `ReadBuildInfo` fallback).
      - From a clean machine: `brew tap peasant-labs/tap && brew install --cask peasant`.
      - From a clean machine: `yay -S peasant-bin` (or `makepkg -si`).
      - Real-URL `makepkg`/`dnf`/`zypper` install from the published artifacts.

---

## 7. Deferred ladder

The following are optional release-hardening improvements. RPM artifacts and WSL
documentation are part of the current release contract rather than this list.

1. **macOS native install** — Developer ID signing + notarization (quill-from-Linux is
   feasible, ~1–2 days once the Apple account + $99/yr entity exist) and a notarized
   `.pkg`. Raw browser downloads currently require the manual `xattr` step in the
   macOS install guide; the cask hook takes over only after Homebrew publication.
2. **SBOM** generation for release artifacts.
3. **Build provenance / artifact attestations**.
4. **cosign** signing of artifacts.
5. **Hosted distro repos** — COPR (Fedora, binary-repackage variant, on demand) /
   OBS (skip). The rpm **artifacts** and WSL **docs** already land; only the hosted
   repos are deferred.
6. **Tree-sitter removal + effectiveness corpus** — gates moving Maximum
   redaction off cgo entirely. Until then, `maximum` redaction requires a cgo build
   (`redact.MaximumAvailable`); a `CGO_ENABLED=0` binary hard-errors actionably.
7. **Hosted apt repo / PPA** — only path to `apt upgrade`; permanent GPG + metadata
   ops. Demand-triggered; prefer aptly → object storage over SaaS. PPA is structurally
   impossible (no-network builders vs the pnpm web embed).
8. **homebrew-core** formula — 6–12+ months after stable launch (notability + source-build rule).

---

## Appendix — frozen artifact contract

Consumed by AUR `source_*`, the cask `url`, the install docs, and any future
curl-install script. **Do not change without updating every consumer.**

- Archives: `peasant_{version}_{linux|darwin}_{amd64|arm64}.tar.gz`
- Checksums: `checksums.txt` (SHA-256 of every artifact)
- Debian: `peasant_{version}_linux_{amd64|arm64}.deb` (rc → `{x.y.z}~rcN`)
- RPM: `peasant_{version}_linux_{amd64|arm64}.rpm` (nfpm uses goreleaser's
  `file_name_template`, not the conventional `name-version-release.arch` form)
- Binary name inside every archive/package: `peasant`, installed to `/usr/bin/peasant`
  (packages) — version injected via
  `-X github.com/peasant-labs/peasant/internal/defaults.version={version}`.
