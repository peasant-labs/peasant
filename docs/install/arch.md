# Installing Peasant on Arch Linux

Peasant is distributed to Arch via the AUR package **`peasant-bin`** — a prebuilt
package wrapping the official static release binary. (The `-bin` suffix is required
by AUR policy for packages that ship prebuilt artifacts.)

> **Availability:** `peasant-bin` is published to the AUR starting with the first
> public stable release (`v0.1.0`). Release candidates (`-rcN`) are intentionally
> **not** pushed to the AUR. Until the AUR package is live, use [Option B](#option-b--raw-binary-tarball).

## Option A — `peasant-bin` from the AUR

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
peasant --version
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

## Option B — raw binary tarball

Works today regardless of AUR availability:

```bash
VERSION=0.1.0
ARCH=amd64    # or arm64

curl -fLO "https://github.com/peasant-labs/peasant/releases/download/v${VERSION}/peasant_${VERSION}_linux_${ARCH}.tar.gz"
curl -fLO "https://github.com/peasant-labs/peasant/releases/download/v${VERSION}/checksums.txt"
sha256sum --ignore-missing -c checksums.txt

tar xzf "peasant_${VERSION}_linux_${ARCH}.tar.gz"
sudo install -m755 peasant /usr/local/bin/peasant
peasant --version
```

## Notes

- The package has **no dependencies** — the binary is fully static.
- A source `peasant` package (building from source rather than the prebuilt binary)
  is [deferred](../release-runbook.md#deferred-ladder): the embedded web dashboard
  needs a pnpm/Next.js build step that an AUR build host cannot perform cleanly
  today. Official `[extra]` inclusion is a long-term, maintainer-driven path.
