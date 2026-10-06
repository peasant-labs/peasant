#!/usr/bin/env python3
"""Verify that the audited local SQLite copy has only documented changes."""

from collections import Counter
import hashlib
import json
import re
from pathlib import Path

root = Path(__file__).resolve().parents[1] / "third_party" / "zombiezen-sqlite"
manifest = json.loads((root / "UPSTREAM.json").read_text())
if (manifest["module"], manifest["version"], manifest["origin"], manifest["commit"]) != (
    "zombiezen.com/go/sqlite", "v1.4.2", "https://github.com/zombiezen/go-sqlite.git",
    "e5fb83745cf1640f27b86f6560ca32f50b2442e9",
):
    raise SystemExit("unexpected SQLite upstream origin")
if hashlib.sha256((root / "connection-lifetime.patch").read_bytes()).hexdigest() != manifest["patch_sha256"]:
    raise SystemExit("SQLite documented patch changed")
modified = manifest["modified_files"]
if "sqlite.go" not in modified or any(not name.endswith(".go") for name in modified):
    raise SystemExit("unexpected SQLite modified-file set")
renamed = manifest["renamed_files"]
if renamed != {name: "UPSTREAM." + name + ".txt" for name in ("go.mod", "go.sum", "go.work", "go.work.sum")}:
    raise SystemExit("unexpected SQLite metadata renames")
added = {
    "UPSTREAM.json", "PEASANT.md", "connection-lifetime.patch",
    "connection_lifetime_test.go", "testdata/connection-lifetime.yaml",
}
actual = {str(path.relative_to(root)) for path in root.rglob("*") if path.is_file()}
expected = {renamed.get(name, name) for name in manifest["files"]} | added
if actual != expected:
    raise SystemExit(f"SQLite file set changed: missing={expected - actual}, extra={actual - expected}")
for name, upstream_digest in manifest["files"].items():
    expected_digest = modified.get(name, upstream_digest)
    actual_digest = hashlib.sha256((root / renamed.get(name, name)).read_bytes()).hexdigest()
    if actual_digest != expected_digest:
        raise SystemExit(f"undocumented SQLite source change: {name}")
# The only vet exception is the native package's unsafeptr analyzer. Its
# original modernc address conversions must remain exactly the upstream set;
# no added source (including our regression test) can introduce another one.
actual_pointer_lines = {}
for path in root.rglob("*.go"):
    lines = [line.strip() for line in path.read_text().splitlines() if "unsafe.Pointer(" in line]
    if lines:
        actual_pointer_lines[str(path.relative_to(root))] = Counter(lines)
expected_pointer_lines = {name: Counter(lines) for name, lines in manifest["native_pointer_lines"].items()}
if actual_pointer_lines != expected_pointer_lines:
    raise SystemExit("SQLite native pointer expressions differ from audited upstream source")
print("SQLite origin and documented source hashes match")

# All Peasant-owned connection/statement types must use the same audited driver.
repo = root.parents[1]
for path in repo.rglob("*.go"):
    if ".git" in path.parts or "node_modules" in path.parts:
        continue
    if re.search(r'"zombiezen\.com/go/sqlite(?:/[^"\n]*)?"', path.read_text()):
        raise SystemExit(f"Peasant still imports the unaudited driver: {path.relative_to(repo)}")
print("All Peasant-owned SQLite imports use the audited package")
