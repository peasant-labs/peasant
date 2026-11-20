# Peasant Release Runbook

## Current release procedure

PR titles carry intent, not versions: `fix(cli)[patch]: handle a missing file`
and `feat(web)[minor]: show progress` are releasing examples. The
`title-guard` checks the live title on PR events and merge groups. Content PRs
must not edit `CHANGELOG.md`, including its `[Unreleased]` block. The first
generated release section preserves that block's entries.

The release job plans from `develop`. With `RELEASE_BATCH_MODE=tag`, it keeps
one bot PR on `automation/release`, computes its version, and arms it only
when the merge queue permits. The release PR's merge-group build checks its
content and Nix vendor hash before running full-stack e2e and the packaging
validation matrix. `merge-gate` also requires schema parity against the
Village ref in the existing workflow-contract fixture. If a release gate
fails, the PR is ejected without landing or tagging; retry the release-batch
dispatch for a transient failure, or merge a normal fix and let the bot
rebuild the release PR.

After the release PR lands, `release-batch.yml` verifies its content again and
pushes the annotated tag on that commit. A tag triggers `release.yml`: the
actor and tag are checked, `release-intent verify-tag` proves that the tag is
reachable from `develop`, and the Nix, e2e, installed-package, and smoke gates
remain. The credential-free build runs GoReleaser with `--skip=publish`,
checks `dist/checksums.txt`, stages the release files in one flat
`dist/assets` directory with the notes beside it, and hands `release-dist` to
the confined GitHub Release publisher. The publisher refuses a tag that
already has a release or a leftover draft, creates the draft and uploads every
asset in one `gh release create --draft`, verifies the draft, then publishes
it. A rerun after a published, complete release succeeds without changing it.
Before a rerun, delete a leftover draft by its id with
`gh api -X DELETE repos/peasant-labs/peasant/releases/<id>`, never with
`gh release delete <tag>`; no new tag is needed. The publisher does not
publish to the Homebrew tap or AUR. `peasant-labs/homebrew-tap` publishes the
cask itself on a fifteen-minute schedule, and the final-tag cask smoke waits
up to 16 minutes for the tap to carry the tagged version before it installs.

The vendor-hash workflow computes a change without an App credential and, if
needed, opens or updates `chore(nix): update vendor hash` on
`automation/vendor-hash` with auto-merge. It does not push `develop`. The
ordinary PR hash check is a report; the release PR's group build makes it a
hard gate. No workflow repairs the hash after a tag.

For a forced release use the `cut-release` dispatch with `patch` or `minor`;
it creates an empty bot PR and the queue drives the normal release. Never
create, move, or delete tags by hand. A failed publication burns its tag; fix
forward. `make check-workflows` builds the infra tool at the SHA in
`title-guard.yml` and checks the full workflow directory.

### Verdict inheritance

A release PR is release-only when its tree equals its base commit's tree
except for `CHANGELOG.md`. The release PR's runs, and its merge group, then
inherit the `merge-gate` verdict that the exact base commit recorded in its
own merge-queue run instead of rerunning `make check`, the harvester guard,
the license notices, the web jobs, the mounted Home gate, the Windows job and
schema parity. Those jobs run in every ordinary merge group for a code change,
so the base verdict proves them, and none of them reads `CHANGELOG.md`.

The release group still runs `release-content`, `vendor-hash`, `check-arm64`,
`cgo0`, `e2e` and `release-validate`. These run only in release groups, so an
ordinary base group never ran them and its verdict has nothing for them to
inherit. Some of them also check the exact tree being tagged: the release
content against the live `develop`, the vendor hash, and the packages built
from that tree. `check-arm64` (about 18m41s) stays on the critical path, so a
release group's wall time is roughly unchanged; the saving is runner time,
estimated at about 27 runner-minutes per release group from the job timings
of the v0.21.0 release merge-group run (run 44405289479, release PR #1092).

Each `merge-gate` run records where its verdict came from in its last step:
`verdict-source: full` when `make check` ran, `verdict-source: inherited` when
it inherited, and `verdict-source: unaffected` when the path filter found no
code change. All three are inheritable. An unaffected base, such as a
docs-only group, was itself built on a base proven the same way, so every
chain ends at a run where `make check` passed.

A missing, pending, failed or ambiguous base verdict fails the `changes` job;
it never falls back to the suite. A merge group waits up to 20 minutes for a
pending base verdict, and a release PR run up to 2 minutes. A base verdict
still pending after the wait fails the job on purpose (fail closed);
rerunning the job after the base's queue run finishes clears it.

Bases from before the marker steps carry no source. Such a base is accepted
only when its own `make check` ran (`legacy-full`), so a pre-marker docs-only
base fails closed as `base-verdict-failed`. Rerunning that base's merge-queue
run does not help, because it reruns the old workflow without markers. The
recovery is a new landing on `develop`, after which the release bot refreshes
the release PR onto the new base. Until then the release PR's own required
`merge-gate` is red, so it cannot be queued. Open release PR #1105
("release[patch]: v0.21.1") is the current instance: its base is #1107, a
`CHANGELOG.md`-only change whose group skipped `make check`.

The `legacy-full` acceptance is a bootstrap rule with an exit condition: it
can be removed once no pre-marker commit can be a release PR's base, that is,
once every release PR's base is a commit landed after verdict inheritance was
adopted.

### One-time cutover

Before merging adoption, set `squash_merge_commit_title=PR_TITLE`. Enable
`allow_auto_merge`, create an exact-`develop` Merge queue ruleset with the
`merge-gate` and `title-guard` required contexts, and compare its read-back
with `release-intent check-workflows --print-required`. Retitle queued PRs
only after removing them from the queue, and drop changelog edits from content
PRs. Run release-batch in plan-only mode first and inspect its output and
absence of writes; enable `RELEASE_BATCH_MODE=tag` only after that group has
been checked. The release App needs contents and pull-request write access;
its client ID is `PEASANT_RELEASER_CLIENT_ID` and its private key remains
`PEASANT_RELEASER_APP_PRIVATE_KEY`.

## Reference and publication records

The sections below are the reference for the release model, credentials,
tag-time gates, reruns, and package publication. The `v0.1.0` records in §3
and the checklist in §7 are dated history of the first rollout, not
instructions. The immutable artifact names in the appendix remain the
distribution contract.

For the maintainer-facing system reference — workflow ownership, release data flow,
package consumers, and why the Nix hash gate sits before tag publication — see
[Release Architecture](release-architecture.md).

> **Status:** the first public-root tag used the exact `v0.1.0` initial-final
> bootstrap. GoReleaser uploads neither the Homebrew cask nor the AUR package
> (`skip_upload: "true"` for both); the tap publishes the cask itself, and AUR
> stays disabled until its publication checklist is complete.

---

## 1. The release model

One canonical **GitHub Release** per tag is the single producer; every package
ecosystem (deb, rpm, AUR, Homebrew cask, nix) is a downstream consumer of the same
frozen artifacts.

```
content PR (title: "type(scope)[level]: subject")
   │  title-guard (release-intent check-title) on PR events and in the merge group
   ▼
merge queue ──▶ develop
   │
   ▼ (push to develop, or the 30-minute schedule)
release-batch.yml ──▶ release-intent plan computes vX.Y.Z from the landed titles
   │                   opens/refreshes "release[level]: vX.Y.Z" on automation/release
   ▼
release PR through the merge queue
   │  release group: release-content, vendor-hash, check-arm64, cgo0, e2e, release-validate
   ▼
release-batch.yml ──▶ annotated tag vX.Y.Z on the landed release commit (App token)
   │
   ▼ (tag push triggers)
release.yml       ──▶ guard + verify-tag → nix vendorHash freshness gate
   │                  → full-stack e2e + release e2e (installed packages)
   │                  → build (goreleaser --skip=publish) → publish → smokes
   ▼
GitHub Release
```

Two tools carry the gating logic, which keeps it testable instead of buried in
workflow bash:

- **`release-intent`**, built in each workflow from the pinned
  `peasant-labs/infra` commit, owns the title grammar (`check-title` in
  `title-guard.yml`), the version plan and the release PR (`release-batch.yml`),
  `verify-tag` in `release.yml`, and the workflow contract (`check-workflows`,
  also run by `make check-workflows`). It reads `.github/release-intent.yaml`
  (release config) and `.github/release-queue.policy.yml` (queue policy).
- **`github.com/peasant-labs/schema/cmd/release-guard`** keeps only
  `parse-tag`, which `release.yml`'s `guard` job runs to classify the tag as a
  final or an rc.

Implementing workflows: `.github/workflows/title-guard.yml` (titles and release
content), `.github/workflows/release-batch.yml` (release PR and tag),
`.github/workflows/cut-release.yml` (forced release),
`.github/workflows/release.yml` + `.goreleaser.yml` (build + publish),
`.github/workflows/release-validate.yml` (per-distro install validation in the
release merge group and on the rc path).

### Publication gates before Goreleaser

`release.yml` builds with Goreleaser in `build` and publishes in `publish`. Both
stay behind every source-level publication gate:

- `guard` rejects tags whose push actor is not `peasant-labs-releaser[bot]`
  (actor ID `291504229`) and malformed/non-Peasant tags. It does not require a prior
  rc for final tags.
- `verify-tag` runs `release-intent verify-tag` to prove the tag is reachable
  from `develop`.
- `nix-vendor-hash` proves the tag points at a source tree with a current `flake.nix`
  vendor hash.
- `e2e` calls the reusable `.github/workflows/e2e.yml` full-stack harness on every
  release tag, not only rc tags. That harness builds Peasant and the village server
  from source, provisions Postgres + RustFS with podman, drives ingest and
  `peasant village push`, verifies the skip-gate/retraction path, and asserts the
  village server-side secret scan.
- `release-e2e` calls the reusable `.github/workflows/release-e2e.yml`
  installed-binary/per-distro gate on every release tag, not only rc tags. It builds
  release snapshot artifacts and installs them across the supported distro paths
  before Goreleaser can publish the real release.

### PR titles

Every PR title is `type(scope)[level]: subject`, judged by `release-intent
check-title` ([current procedure](#current-release-procedure),
[`CONTRIBUTING.md`](../CONTRIBUTING.md#release-notes-and-pr-titles)). No
author writes a version: the only titles that carry one are the bot's
`release[level]: vX.Y.Z`, and a forced cut is `release[level]: cut a release`.

### Tag namespaces

- `vX.Y.Z` → Peasant releases (annotated, immutable), computed by the planner.
- `vX.Y.Z-rcN` → still parsed by `release.yml` as a prerelease, with the rc-only
  packaging checks, but the planner computes final versions only and no current
  workflow creates an rc tag.
- Schema module tags are created in the standalone public schema repository and cannot
  trigger Peasant's `release.yml`.

---

## 2. Secrets & permissions inventory

| Secret / setting | Where | Purpose | Set when |
|------------------|-------|---------|----------|
| `PEASANT_RELEASER_CLIENT_ID` (variable) / `PEASANT_RELEASER_APP_PRIVATE_KEY` (secret) for GitHub App ID `3988034` (`peasant-labs-releaser`, contents and pull-request write) | Used only by the write jobs of `release-batch.yml`, `cut-release.yml`, and `nix-vendor-hash.yml` | Push the `automation/*` bot branches, open their PRs with auto-merge, and push the annotated release tag | Before the first real tag |
| `RELEASE_BATCH_MODE` / `RELEASE_BATCH_FLOOR` | Repo variables | `tag` turns on the release PR and tag writes (unset: plan only); the floor drops an unparseable landed subject and everything before it | `RELEASE_BATCH_MODE` at cutover; the floor only for recovery |
| `GITHUB_TOKEN` (default) | Per-workflow, automatic | Read-only checkout, PR, and queue reads; `contents: write` only in `release.yml`'s `publish` job, which creates the GitHub Release | Already present |
| `AUR_KEY` | Repo secret | Unencrypted ed25519 **private** key for `ssh://aur@aur.archlinux.org/peasant-bin.git`; goreleaser `aurs` writes it to a temp file and sets `GIT_SSH_COMMAND` | When AUR publication is separately approved |
| `v*` tag ruleset | Repository settings | Rejects tag creation, update, deletion, and non-fast-forward changes from every actor except GitHub App ID `3988034` (`peasant-labs-releaser`) | Before the first real tag |

**Manual, one-time, UI-only step:** reconfirm that the
`peasant-labs-releaser` installation has contents and pull-request write reach for
`peasant-labs/peasant`; peasant holds no tap credential. E2E checks out the public
Village repository and public Go modules with read-only `GITHUB_TOKEN` access and no PAT.

The workflows use Blacksmith's amd64 and arm64 runner labels declared in
`.github/actionlint.yaml`. Before enabling Actions on a newly promoted repository,
grant the Blacksmith GitHub App access to it and verify that a smoke workflow can
acquire each declared label. Jobs remaining queued indicate missing runner access,
not a test failure; do not cut a release until all required labels schedule.

A self-hosted MicroVM pool (`self-hosted` / `linux` / `x64` / `microvm`) serves
architecture-neutral jobs through the shared `determine-runner` workflow, with
each caller's Blacksmith fallback when the pool is unavailable. Routed container
workloads use Podman inside the guest, not the host runtime. Two x86_64 jobs
remain pinned to amd64 Blacksmith runners:

- the **full-stack podman e2e driver** (`e2e.yml`), preserving its existing
  hosted runner and full-stack coverage;
- the **goreleaser snapshot** (`release-validate.yml`), preserving its existing
  hosted runner and clean workspace.

### Release authority

No workflow checks a PR author's collaborator permission. Authority comes from
the release mechanism itself: only the releaser App writes the release PR and
the tag, the version comes from the planner rather than a person, and only the
`develop` merge queue lands the release PR (see
[Verdict inheritance](#verdict-inheritance) for what its group re-proves). A
required independent approval stays **deferred**: with a single active
maintainer, GitHub's no-self-approval rule makes it unsatisfiable, and no
approval check is implemented today.

Branch protection on `develop` and the App-only `v*` tag ruleset are defense-in-depth
configured separately in GitHub. `release.yml` also checks the App's stable login and
actor ID before processing a tag. If the App identity changes, update the ruleset,
workflow assertion, fixture, and this runbook together.

---

## 3. Bootstrap records

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

---

## 4. Cutting a release

Nobody opens a release PR or picks a version; the
[current procedure](#current-release-procedure) above is the whole ceremony.
In order:

1. Releasing PRs land on `develop` through the merge queue.
2. `release-batch.yml` keeps the standing `release[level]: vX.Y.Z` PR on
   `automation/release` current and arms it in the queue. To release with
   nothing pending, dispatch `cut-release.yml` with `patch` or `minor`.
3. The release PR's merge group runs the release-only jobs listed under
   [Verdict inheritance](#verdict-inheritance). A failure ejects it before it
   lands or is tagged.
4. Once it lands, `release-batch.yml` pushes the annotated tag on that commit,
   and the tag push triggers `release.yml`:
   - **guard**: verifies the tag push actor is `peasant-labs-releaser[bot]`
     (actor ID `291504229`) and classifies the tag with `release-guard parse-tag`.
   - **verify-tag**: `release-intent verify-tag` proves `develop` reaches the tag.
   - **nix-vendor-hash**: re-runs `make nix-vendor-hash` and fails if `flake.nix`
     would change.
   - **full-stack e2e**: calls `.github/workflows/e2e.yml` and must produce a
     positive `--- PASS: TestSkipGateE2E` line. A skipped harness or `[no tests to
     run]` blocks publication because no product e2e coverage was proven.
   - **release e2e**: calls `.github/workflows/release-e2e.yml` and must produce
     a positive `--- PASS: TestReleasePerDistro` line. This proves installed package
     artifacts across the per-distro release paths before publication.
   - **build** (`CGO_ENABLED=0`, no write token): builds the dashboard and runs
     `goreleaser release --skip=publish` for the 5 static targets, archives,
     `checksums.txt`, and `.deb`/`.rpm`; takes the release notes from the tag's
     `CHANGELOG.md` section; and hands the staged assets to `publish`.
   - **publish**: creates the draft with every asset, verifies it, and publishes
     it (the [current procedure](#current-release-procedure) covers reruns and
     leftover drafts).
   - **smoke** (native amd64 + arm64): asserts the binary is static (`ldd`) and
     `peasant version` output contains the tag (substring check `grep -qF "${TAG#v}"`).
   - **windows-smoke** (`windows-latest`, amd64): downloads both published Windows
     assets, verifies each against `checksums.txt`, unpacks the zip and asserts it
     carries `peasant.exe`, `LICENSE`, `README.md` and `THIRD_PARTY_NOTICES`, asserts
     both binaries report the tag, then starts the dashboard, probes it over HTTP and
     stops it. There is no `ldd` on Windows, so a runner carrying no Go toolchain and no
     MinGW runtime stands in for the static-linkage check; that is weaker, which is why
     this job starts the server rather than only reading `version`.
   - **macos-cask-smoke** (final tags): waits for the tap to carry the tagged
     version, then installs the cask and asserts its version.
5. Verify the Release on the Releases page: 4 `.tar.gz` + 1 Windows `.zip` +
   1 bare Windows `.exe` + 2 `.deb` + 2 `.rpm` + `checksums.txt` (11 files,
   `checksums.txt` included). Verify AUR and nixpkgs only after their separate
   publication checklist items are approved and enabled.

---

## 5. Re-running a failed release

`release.yml` runs once per tag. When a job fails, re-run **only the failed jobs**;
do not re-run the whole workflow.

The tag's `CHANGELOG.md` section is the release body, extracted by
`scripts/release-header` in `build`. A missing or duplicate section fails the
release PR's `release-content` check and, again, `build` on the tagged checkout.

- The GitHub UI action *Re-run failed jobs*, or `gh run rerun <run-id> --failed`,
  re-runs the failed jobs and their downstream dependents. Successful upstream
  and unrelated jobs are retained — a failed smoke re-runs only the smoke, and
  `guard`, `nix-vendor-hash`, `full-stack e2e`, `release e2e` are not re-run.
- A gate that failed re-runs together with its dependents, so `build`,
  `publish`, and the smokes run once the gate passes. That is the normal case:
  publication had been skipped, not executed, when the gate failed.
- `build` and `publish` are safe to re-run: `publish` refuses a tag that already
  has a release or a leftover draft, and succeeds without changes when the
  release is already published and complete (see the
  [current procedure](#current-release-procedure) for deleting a leftover
  draft). `.goreleaser.yml`'s `release.mode` and `replace_existing_artifacts`
  no longer act, because `build` runs with `--skip=publish`. No job pushes to
  the tap.
- `smoke` / `macos-cask-smoke` only download and install the published artifacts;
  they are safe to re-run alone, in any order.
- If `e2e` or `release-e2e` failed, its re-run also re-runs `build`, `publish`,
  and the smokes, because publication depends on those gates. That dependency is
  intentional: a gate failure must block publication.
- A re-run consumes the workflow stored at the tag, not the current `develop`.
  A fix to `release.yml` itself therefore does not reach an existing tag's
  re-run. The `v0.1.0` startup record in §3 documents the only recovery exercise
  of that shape; its one-time workflow was removed after it succeeded, so a
  workflow-level failure at tag time needs a new, reviewed recovery path.

After any re-run, repeat the §4 verification: the full artifact set,
`checksums.txt`, and a fresh smoke on both architectures.

---

## 6. Release guard rules

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
hash update script, or the workflow itself, and opens or updates the
`automation/vendor-hash` PR rather than pushing `develop`. The release PR's merge
group runs `vendor-hash` as a hard gate because a tag must point at immutable,
hash-current source.

---

## 7. Publication checklist

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
      - Automated Git commits and the release tag use the name
        `peasant-release-bot`: `release-batch.yml`, `cut-release.yml`, and
        `nix-vendor-hash.yml` set `bots@peasant-labs.invalid`, and the disabled
        AUR and Homebrew `commit_author` blocks in `.goreleaser.yml` keep
        `noreply@peasantlabs.org`. Change the three workflow sites together.
      - Package maintainer metadata uses the project contact
        `Peasant Labs <admin@peasantlabs.org>` (obfuscated where AUR requires it).
        Neither bot identity is a maintainer contact.
- [x] Make the repository **public**.
- [x] Confirm the GitHub App has `Contents: write` on `peasant-labs/peasant` and
      `peasant-labs/homebrew-tap` (§2). Satisfied by the org-wide App installation
      (`repository_selection: all`).
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
- [x] **Homebrew tap:** the `peasant-labs/homebrew-tap` repository exists and the App
      has `Contents: write` on it (org-wide install). `release.yml` mints the
      short-lived tap token at release time; there is no long-lived `TAP_GITHUB_TOKEN`
      secret.
- [x] **Flip `homebrew_casks` `skip_upload`:** set to **`auto`** in `.goreleaser.yml`
      (publishes the cask on final tags; `auto` keeps prereleases off the tap forever —
      rc tags never touch the tap).
- [ ] **Flip `aurs` `skip_upload`:** change from `true` to **`auto`** once AUR
      publishing is approved and `AUR_KEY` is set. It stays `true` until then.
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

## 8. Deferred ladder

The following are optional release-hardening improvements. RPM artifacts and WSL
documentation are part of the current release contract rather than this list.

1. **macOS native install** — Developer ID signing + notarization (quill-from-Linux is
   feasible, ~1–2 days once the Apple account + $99/yr entity exist) and a notarized
   `.pkg`. Raw browser downloads currently require the manual `xattr` step in the
   macOS install guide; cask installs clear it automatically via the post-install hook.
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
- Windows archive: `peasant_{version}_windows_amd64.zip` (amd64 only; same payload
  as the tar.gz archives, license notices included)
- Windows executable: `peasant_{version}_windows_amd64.exe` — the bare binary, for
  a direct download and for `peasant upgrade`, which replaces the running binary
  and so wants an executable rather than an archive. Carries no accompanying
  files, which is why the `.zip` stays the documented install path.
- Checksums: `checksums.txt` (SHA-256 of every artifact)
- Debian: `peasant_{version}_linux_{amd64|arm64}.deb` (rc → `{x.y.z}~rcN`)
- RPM: `peasant_{version}_linux_{amd64|arm64}.rpm` (nfpm uses goreleaser's
  `file_name_template`, not the conventional `name-version-release.arch` form)
- Binary name inside every archive/package: `peasant`, installed to `/usr/bin/peasant`
  (packages) — version injected via
  `-X github.com/peasant-labs/peasant/internal/defaults.version={version}`.
