# Installing Peasant on WSL (Windows Subsystem for Linux)

Peasant runs under **WSL2** with no Windows-specific build: WSL2 is a real Linux
kernel, so the static Linux binaries run unmodified. Install exactly as you would on
native Linux, then read the WSL-specific caveats below.

## Install

WSL2 distros are Linux, so follow the matching guide for your distro:

- **Ubuntu/Debian-WSL** (the default) → [ubuntu.md](ubuntu.md) (`.deb` or tarball)
- **Fedora-WSL** → `sudo dnf install ./peasant-<version>.x86_64.rpm`
- **Arch-WSL** → [arch.md](arch.md)
- Any distro → the raw `peasant_<version>_linux_<arch>.tar.gz` tarball

Pick the architecture matching `uname -m` (`x86_64` → amd64, `aarch64` → arm64).

## WSL-specific caveats

### 1. Opening the browser (`peasant web start`, `peasant login`)

Peasant opens your browser via the `$BROWSER` → `xdg-open` → `wslview` chain. Stock
WSL distros have **no `xdg-open`** (no desktop stack). Install `wslu` so the chain can
fall back to `wslview`, which opens your **Windows** browser:

```bash
sudo apt install wslu        # Ubuntu/Debian-WSL; dnf/pacman on other distros
```

If no launcher is found, Peasant now prints an **actionable error** and the URL to
open manually — it no longer fails silently. You can always copy the printed URL into
your Windows browser by hand.

### 2. Reaching the dashboard from Windows

`peasant web start` binds the dashboard port and accepts loopback connections. Under
WSL2's default **NAT** networking, Windows auto-forwards `localhost:<port>` into the
distro, and under **mirrored** networking (Windows 11 22H2+) localhost is shared
outright. Either way:

```
open http://localhost:8690 in your Windows browser
```

If it doesn't connect, check the **Hyper-V firewall** (mirrored mode) as the first
troubleshooting step.

### 3. Keep agent data inside the WSL filesystem

Peasant ingests the agent transcripts of the **WSL-side** tools (`~/.claude/projects`,
etc.). If you run Claude Code / OpenCode on the **Windows** side, those transcripts
live under `/mnt/c/Users/<you>/.claude`. Peasant can read them with
`--source-path /mnt/c/Users/<you>/.claude`, but the `/mnt/c` 9p mount is **slow** and
has coarser modification-time semantics, so keep data WSL-side when you can.

### 4. Restarting after `wsl --shutdown`

`wsl --shutdown` kills the detached `web start` server **without** removing its PID
file. Before starting again on the same port, run `peasant web stop` (it cleans up the
stale PID file), then `peasant web start`.

## Notes

- All state stays inside the WSL distro (`~/.config|.local/share|.local/state/peasant`),
  reachable from Windows only via `\\wsl$\<distro>\…`.
- No systemd required: `web start` daemonizes via fork + PID file and `web stop` falls
  back to SIGTERM, so it works on both systemd and `systemd=false` distros.
