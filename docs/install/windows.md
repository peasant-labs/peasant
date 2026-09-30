# Installing Peasant on Windows

Peasant ships a single statically linked `peasant.exe` for **Windows amd64**. There is
no installer and nothing to compile: the binary carries the dashboard and its SQLite
engine, so it has no runtime dependency beyond Windows itself.

Two assets are published per release. The **zip archive** is the recommended path — it
carries the licence and notice files alongside the executable. The **bare `.exe`** is
offered for a direct download and is what `peasant upgrade` replaces in place.

Windows arm64 is not published. Running Peasant inside WSL remains supported and is
documented separately in [wsl.md](wsl.md); this guide covers the native Windows build.

## Install from the zip archive

Run these in PowerShell:

```powershell
$Version = '0.1.0'
$Zip     = "peasant_${Version}_windows_amd64.zip"
$Base    = "https://github.com/peasant-labs/peasant/releases/download/v$Version"

Invoke-WebRequest -Uri "$Base/$Zip"          -OutFile $Zip
Invoke-WebRequest -Uri "$Base/checksums.txt" -OutFile checksums.txt

# Verify the download: the printed hash must appear beside the archive name.
(Get-FileHash -Algorithm SHA256 $Zip).Hash.ToLower()
Select-String -Path checksums.txt -Pattern $Zip

Expand-Archive -Path $Zip -DestinationPath "$env:USERPROFILE\peasant" -Force
```

Then put the directory on your `PATH` for future sessions. This reads and rewrites the
**user** `Path` only, so machine-wide entries are left untouched:

```powershell
$UserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
[Environment]::SetEnvironmentVariable('Path', "$UserPath;$env:USERPROFILE\peasant", 'User')
```

Open a new PowerShell window so the change takes effect.

## Install the bare executable

If you would rather not unpack an archive, download the executable directly. It arrives
without the licence and notice files that the zip carries, so prefer the zip unless you
have a reason not to:

```powershell
$Version = '0.1.0'
$Base    = "https://github.com/peasant-labs/peasant/releases/download/v$Version"

Invoke-WebRequest -Uri "$Base/peasant_${Version}_windows_amd64.exe" -OutFile peasant.exe
Invoke-WebRequest -Uri "$Base/checksums.txt"                        -OutFile checksums.txt

(Get-FileHash -Algorithm SHA256 peasant.exe).Hash.ToLower()
Select-String -Path checksums.txt -Pattern "peasant_${Version}_windows_amd64.exe"
```

## Reinstalling or upgrading

Repeat the download, the checksum check, and the `Expand-Archive` step with the newer
version, extracting over the same directory. That replaces the `peasant.exe` you
installed and nothing else: it does not remove your config, your database, your ingested
data, or your state, and it does not run setup again. Installing from the `.zip` and
installing the bare executable are interchangeable — either replaces the other.

`peasant upgrade` can do this for you, replacing the running executable in place.
It downloads the published `peasant_<version>_windows_amd64.exe`, verifies it against
`checksums.txt`, and installs it over the executable you are running.

Windows will not let a running program be overwritten, so the upgrade renames the
current executable to `peasant.exe.old` and puts the new build in the path it
vacated. That leaves `peasant.exe.old` beside `peasant.exe` afterwards, because
Windows also refuses to delete it while the process that upgraded is still
running. It is the previous version and nothing needs it: delete it whenever you
like, and the next `peasant upgrade` removes it for you.

When you run setup again, read
[kickstart rerun and reset behavior](../KICKSTART.md#reset-and-standalone-boundaries).

## Verify and start guided setup

Once `peasant version` prints a version, run the guided setup wizard:

```powershell
peasant version
peasant kickstart
```

## Where Peasant keeps your data

Peasant follows the same layout it uses elsewhere, rooted at `%USERPROFILE%` rather than
a unix home:

| Contents | Location |
|----------|----------|
| Config | `%USERPROFILE%\.config\peasant\config.yaml` |
| Database and ingested transcripts | `%USERPROFILE%\.local\share\peasant` |
| Logs and the `web start` PID file | `%USERPROFILE%\.local\state\peasant` |

`--config-dir`, `--data-dir`, and `--state-dir` override the parent of each.

## Agent sessions Peasant reads

Claude Code on Windows writes its transcripts below
`%USERPROFILE%\.claude\projects`, which is the supported root and the one kickstart
offers by default. Each project directory encodes its real path — `C--Users-alice-work`
for `C:\Users\alice\work` — and Peasant decodes that back to the filesystem path so
project names read normally.

## Git for Windows

Peasant derives a session's branch, remote, and worktree by running `git`. Install
[Git for Windows](https://git-scm.com/download/win) and make sure `git` is on your
`PATH`, or those fields stay empty and sessions group only by directory.

## Notes

- The dashboard binds to loopback. `peasant web start` prints the URL to open, and
  `peasant web stop` ends it.
- Windows has no POSIX file modes, so Peasant does not attempt to restrict
  permissions on the files it writes the way it does on unix.
