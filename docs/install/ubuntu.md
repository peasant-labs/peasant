# Installing Peasant on Ubuntu / Debian

Peasant ships a statically linked, dependency-free binary (`CGO_ENABLED=0`), so it
runs on any Ubuntu/Debian release with **no extra packages** to install.

Each release publishes, on its [GitHub Release page](https://github.com/peasant-labs/peasant/releases):

- `.deb` packages — `peasant_<version>_linux_amd64.deb`, `peasant_<version>_linux_arm64.deb`
- `.tar.gz` archives — `peasant_<version>_linux_amd64.tar.gz`, `peasant_<version>_linux_arm64.tar.gz`
- `checksums.txt` — SHA-256 of every artifact

Pick the architecture matching `dpkg --print-architecture` (`amd64` or `arm64`).

## Option A — `.deb` package (recommended)

The `.deb` gives you a real package entry: clean removal and proper version-ordered
upgrades via the dpkg database.

```bash
VERSION=0.1.0                       # the release you want, without the leading "v"
ARCH=$(dpkg --print-architecture)   # amd64 or arm64

# Download the package and the checksums
curl -fLO "https://github.com/peasant-labs/peasant/releases/download/v${VERSION}/peasant_${VERSION}_linux_${ARCH}.deb"
curl -fLO "https://github.com/peasant-labs/peasant/releases/download/v${VERSION}/checksums.txt"

# Verify integrity (optional but recommended)
sha256sum --ignore-missing -c checksums.txt

# Install (apt resolves the local file; works on apt >= 1.1, i.e. every supported release)
sudo apt install "./peasant_${VERSION}_linux_${ARCH}.deb"

peasant version    # should include ${VERSION}
```

`dpkg -i ./peasant_*.deb` works identically if you prefer.

**Upgrading:** download the newer `.deb` and `sudo apt install ./…` it — dpkg's
version comparison upgrades in place. Release-candidate packages (`0.1.0~rc1`) sort
*older* than their final (`0.1.0`), so an rc upgrades cleanly to the final.

**Removing:** `sudo apt remove peasant`.

> **No `apt upgrade` channel.** There is no hosted apt repository yet, so Peasant
> will not appear in `apt update`/`apt upgrade`. You re-download the `.deb` per
> release. A hosted, GPG-signed repo is a [deferred fast-follow](../release-runbook.md#deferred-ladder)
> that will be built only on demand. This matches how comparable Go CLIs (k9s, dive)
> distribute.

## Option B — raw binary tarball

No package database entry; you manage the binary yourself.

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

## Notes

- The binary needs nothing from the host — no glibc-dev, no Node, no git. It runs
  unchanged inside minimal containers (`ubuntu:22.04`, `ubuntu:24.04`, distroless).
- The `.deb` declares **no dependencies** (it is a static binary) and installs the
  single executable to `/usr/bin/peasant`. Configuration and data live under your
  XDG directories (`~/.config/peasant/`, `~/.local/share/peasant/`), never in `/etc`.
- Running under WSL? See [wsl.md](wsl.md) for the browser-launch and localhost notes.
