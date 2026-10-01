#!/usr/bin/env python3
"""Verify that the audited local SQLite copy has only documented changes."""

import hashlib
import json
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
if set(modified) != {"sqlite.go", "go.mod", "go.sum"}:
    raise SystemExit("unexpected SQLite modified-file set")
added = {
    "UPSTREAM.json", "PEASANT.md", "connection-lifetime.patch",
    "connection_lifetime_test.go", "testdata/connection-lifetime.yaml",
}
actual = {str(path.relative_to(root)) for path in root.rglob("*") if path.is_file()}
expected = set(manifest["files"]) | added
if actual != expected:
    raise SystemExit(f"SQLite file set changed: missing={expected - actual}, extra={actual - expected}")
for name, upstream_digest in manifest["files"].items():
    expected_digest = modified.get(name, upstream_digest)
    actual_digest = hashlib.sha256((root / name).read_bytes()).hexdigest()
    if actual_digest != expected_digest:
        raise SystemExit(f"undocumented SQLite source change: {name}")
print("SQLite origin and documented source hashes match")
