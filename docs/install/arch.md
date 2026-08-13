# Installing Peasant on Arch Linux

Peasant `v0.1.0` is distributed to Arch through the official static Linux release
tarball. The planned AUR package **`peasant-bin`** remains disabled until a separately
approved later final release. (The `-bin` suffix is required by AUR policy for
packages that ship prebuilt artifacts.)

> **Availability:** `peasant-bin` is not published for `v0.1.0`. Release candidates
> (`-rcN`) will never be pushed to the AUR. Use the current tarball path below until
> AUR publication is explicitly enabled for a later final release.

## Current path: raw binary tarball

```bash
VERSION=0.1.0
ARCH=amd64    # or arm64

curl -fLO "https://github.com/peasant-labs/peasant/releases/download/v${VERSION}/peasant_${VERSION}_linux_${ARCH}.tar.gz"
curl -fLO "https://github.com/peasant-labs/peasant/releases/download/v${VERSION}/checksums.txt"
sha256sum --ignore-missing -c checksums.txt

tar xzf "peasant_${VERSION}_linux_${ARCH}.tar.gz"
sudo install -m755 peasant /usr/local/bin/peasant
peasant version
```

## Deferred path: `peasant-bin` from the AUR

The following commands apply only after `peasant-bin` appears in the AUR.

With an AUR helper (`yay`, `paru`, …):

```bash
yay -S peasant-bin
# or
paru -S peasant-bin
```

Manually with `makepkg`:

```bash
git clone https://aur.archlinux.org/peasant-bin.git
cd peasant-bin
makepkg -si        # downloads the release tarball, verifies sha256, installs
peasant version
```

`peasant-bin` `provides`/`conflicts` the name `peasant`, so it will swap cleanly for
a future source or official package without manual intervention.

- Installs the binary to `/usr/bin/peasant`.
- Installs the license to `/usr/share/licenses/peasant-bin/LICENSE`.
- Supported architecture: `x86_64` (and `aarch64` where the release publishes an
  arm64 tarball).

**Updating:** your AUR helper picks up new versions automatically; with manual
`makepkg`, `git pull` then `makepkg -si` again.

**Removing:** `sudo pacman -Rns peasant-bin`.

## Reinstalling or upgrading

For the current raw-tarball channel, download the newer `tar.gz` archive, verify it, extract it, and
repeat `sudo install -m755 peasant /usr/local/bin/peasant`. Reinstalling or upgrading replaces
the binary without removing Peasant's configuration, database, ingested data, or state.

If the AUR package becomes available, update it with `yay -Syu peasant-bin` or `paru -Syu
peasant-bin`. A manual AUR reinstall or upgrade repeats `git pull` and `makepkg -si`; the package
manager replaces the binary and keeps Peasant's configuration and data.

## Verify and start guided setup

After `peasant version` succeeds, run the guided setup wizard:

```bash
peasant version
peasant kickstart
```

For a later setup run, read [kickstart rerun and reset behavior](../KICKSTART.md#reset-and-standalone-boundaries).

## Notes

- The package has **no dependencies** — the binary is fully static.
- A source `peasant` package (building from source rather than the prebuilt binary)
  is [deferred](../release-runbook.md#7-deferred-ladder): the embedded web dashboard
  needs a pnpm/Next.js build step that an AUR build host cannot perform cleanly
  today. Official `[extra]` inclusion is a long-term, maintainer-driven path.
