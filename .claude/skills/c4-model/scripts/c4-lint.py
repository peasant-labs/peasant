#!/usr/bin/env python3
"""Lint ASCII C4 diagrams in Markdown files.

A diagram is a fenced code block whose info string is exactly ``c4``. The notation is defined in
``references/ascii-notation.md``. Each rule has one code:

  TITLE          line 1 is not "<Type> diagram: <scope>"
  KEY            no "Key:" section, or an empty one
  NO_ELEMENTS    no well-formed [Type] tag in the block
  TAG            a [ ... ] tag is not in the closed set, or lacks its technology
  LEVEL_MIX      an element type that this diagram type does not allow
  ARROW_LABEL    an arrow with no intent label
  ARROW_TECH     a Container, Component, or Deployment arrow label with no (technology)
  STEP_NUMBER    a Dynamic diagram arrow label that does not start with "N. "
  BIDIRECTIONAL  a two-headed arrow

Usage:
  c4-lint.py FILE.md [FILE.md ...]   lint; exit 0 when clean, 1 on findings, 2 on usage error
  c4-lint.py --self-test             run the fixture cases in testdata/lint-cases.md

Standard library only.
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

DIAGRAM_TYPES = (
    "System Landscape",
    "System Context",
    "Container",
    "Component",
    "Code",
    "Dynamic",
    "Deployment",
)
TITLE_RE = re.compile(
    r"^(?P<type>" + "|".join(re.escape(t) for t in DIAGRAM_TYPES) + r") diagram: (?P<scope>\S.*)$"
)

KINDS_PLAIN = ("Person", "Software System")
KINDS_WITH_ARG = ("Container", "Component", "Code", "Deployment Node", "Infrastructure Node", "Group")
KIND_ARG_RE = re.compile(r"^(?P<kind>" + "|".join(re.escape(k) for k in KINDS_WITH_ARG) + r")(?P<rest>.*)$")

# Element kinds each diagram type may show. Group is allowed on every diagram but Code.
ALLOWED = {
    "System Landscape": {"Person", "Software System", "Group"},
    "System Context": {"Person", "Software System", "Group"},
    "Container": {"Person", "Software System", "Container", "Group"},
    "Component": {"Person", "Software System", "Container", "Component", "Group"},
    "Code": {"Component", "Code"},
    "Dynamic": {"Person", "Software System", "Container", "Component", "Group"},
    "Deployment": {"Deployment Node", "Infrastructure Node", "Software System", "Container", "Group"},
}
ARROW_TECH_TYPES = {"Container", "Component", "Deployment"}

CODES = (
    "TITLE",
    "KEY",
    "NO_ELEMENTS",
    "TAG",
    "LEVEL_MIX",
    "ARROW_LABEL",
    "ARROW_TECH",
    "STEP_NUMBER",
    "BIDIRECTIONAL",
)

# Fixture cases that must exist. Deleting one from the fixture fails the self-test.
REQUIRED_CASES = (
    "clean-container",
    "clean-context-with-elbow",
    "missing-title",
    "missing-key",
    "no-elements",
    "bad-tag",
    "level-mix",
    "unlabelled-arrows",
    "arrow-without-technology",
    "dynamic-unnumbered",
    "two-headed-arrow",
    "clean-label-with-angle-brackets",
    "clean-dynamic-vertical-steps",
    "single-dash-two-headed-arrow",
)

FENCE_RE = re.compile(r"^ {0,3}(?P<fence>`{3,}|~{3,})\s*(?P<info>\S*)\s*$")
TAG_RE = re.compile(r"\[([^\[\]]*)\]")
BIDI_RE = re.compile(r"<-+[^<>+|]*-*>|<=+[^<>+|]*=*>")
RIGHT_RE = re.compile(r"-{2,}(.*?)-{2,}>")
LEFT_RE = re.compile(r"<-{2,}(.*?)-{2,}(?!>)")
ELBOW_LABEL_RE = re.compile(r"-{2,}\s*([A-Za-z0-9][^-]*?)\s*-{2,}")
TECH_RE = re.compile(r"\([^()]*[A-Za-z][^()]*\)")
STEP_RE = re.compile(r"^\d+\.\s")
LETTER_RE = re.compile(r"[A-Za-z]")
CASE_RE = re.compile(r"^<!--\s*case:\s*(?P<name>[A-Za-z0-9_-]+)\s*\|\s*expect:\s*(?P<codes>[A-Za-z_ ,]+?)\s*-->\s*$")


class Finding:
    __slots__ = ("line", "code", "message")

    def __init__(self, line: int, code: str, message: str) -> None:
        self.line = line
        self.code = code
        self.message = message


def parse_tag(body: str) -> tuple[str | None, str | None]:
    """Return (kind, error). kind is None when the tag is not in the closed set."""
    body = body.strip()
    if body == "Person":
        return "Person", None
    if body in ("Software System", "Software System, external"):
        return "Software System", None
    m = KIND_ARG_RE.match(body)
    if m:
        kind = m.group("kind")
        rest = m.group("rest")
        if re.match(r"^:\s*\S", rest):
            return kind, None
        return kind, f"[{body}] needs text after the colon, for example [{kind}: Go]"
    return None, f"[{body}] is not a C4 type tag"


def clean_label(raw: str) -> str:
    return raw.strip(" +|=-").strip()


def check_arrow_label(label: str, dtype: str | None, line_no: int, out: list[Finding]) -> None:
    if not LETTER_RE.search(label):
        out.append(Finding(line_no, "ARROW_LABEL", "arrow has no intent label"))
        return
    if dtype in ARROW_TECH_TYPES and not TECH_RE.search(label):
        out.append(Finding(line_no, "ARROW_TECH", f"label '{label}' has no (technology)"))
    if dtype == "Dynamic" and not STEP_RE.match(label):
        out.append(Finding(line_no, "STEP_NUMBER", f"label '{label}' does not start with a step number"))


def vertical_labels(lines: list[str], i: int, c: int, head: str) -> list[str]:
    """Collect labels along the shaft of a vertical arrowhead at (i, c)."""
    step = -1 if head == "v" else 1
    labels: list[str] = []
    j = i + step
    while 0 <= j < len(lines):
        row = lines[j]
        ch = row[c] if c < len(row) else " "
        if ch == "|":
            segment = row[c + 1 :].split("|", 1)[0]
            m = re.match(r"^\s{1,3}(\S.*)$", segment)
            if m and LETTER_RE.search(m.group(1)):
                labels.append(m.group(1).strip())
            j += step
            continue
        if ch == "+":
            m = ELBOW_LABEL_RE.search(row)
            if m and LETTER_RE.search(m.group(1)):
                labels.append(m.group(1).strip())
            break
        break
    if step == -1:
        labels.reverse()  # collected bottom-up; return in reading order
    return labels


def lint_block(lines: list[str], first_line_no: int) -> list[Finding]:
    """Lint one diagram. first_line_no is the file line number of lines[0]."""
    out: list[Finding] = []
    ln = lambda i: first_line_no + i  # noqa: E731

    body_start = next((i for i, l in enumerate(lines) if l.strip()), None)
    dtype: str | None = None
    if body_start is None:
        out.append(Finding(first_line_no, "TITLE", "block is empty"))
        return out
    m = TITLE_RE.match(lines[body_start].strip())
    if m:
        dtype = m.group("type")
    else:
        out.append(
            Finding(ln(body_start), "TITLE", "line 1 must be '<Type> diagram: <scope>' with Type in " + ", ".join(DIAGRAM_TYPES))
        )

    key_idx = next((i for i, l in enumerate(lines) if l.strip().startswith("Key:")), None)
    if key_idx is None:
        out.append(Finding(ln(len(lines) - 1), "KEY", "no 'Key:' section"))
        body_end = len(lines)
    else:
        after = lines[key_idx].strip()[len("Key:") :].strip()
        rest = [l for l in lines[key_idx + 1 :] if l.strip()]
        if not after and not rest:
            out.append(Finding(ln(key_idx), "KEY", "'Key:' has no text"))
        body_end = key_idx

    body = lines[:body_end]
    element_count = 0
    for i, line in enumerate(body):
        if i == body_start:
            continue
        for tm in TAG_RE.finditer(line):
            kind, err = parse_tag(tm.group(1))
            if err:
                out.append(Finding(ln(i), "TAG", err))
            if kind and not err:
                element_count += 1
            if kind and dtype and kind not in ALLOWED[dtype]:
                out.append(Finding(ln(i), "LEVEL_MIX", f"[{kind}] is not allowed on a {dtype} diagram"))
    if element_count == 0:
        out.append(Finding(ln(body_start), "NO_ELEMENTS", "no [Type] tag found; every element needs one"))

    for i, line in enumerate(body):
        if i == body_start:
            continue
        for bm in BIDI_RE.finditer(line):
            out.append(Finding(ln(i), "BIDIRECTIONAL", f"'{bm.group(0)}' has two heads; draw two arrows"))
        for rm in RIGHT_RE.finditer(line):
            check_arrow_label(clean_label(rm.group(1)), dtype, ln(i), out)
        for lm in LEFT_RE.finditer(line):
            check_arrow_label(clean_label(lm.group(1)), dtype, ln(i), out)
        for c, ch in enumerate(line):
            if ch not in "v^":
                continue
            if c > 0 and line[c - 1] != " ":
                continue
            if c + 1 < len(line) and line[c + 1] != " ":
                continue
            labels = vertical_labels(body, i, c, ch)
            if not labels:
                out.append(Finding(ln(i), "ARROW_LABEL", f"vertical arrow at column {c + 1} has no label on its shaft"))
                continue
            check_arrow_label(" ".join(labels), dtype, ln(i), out)

    out.sort(key=lambda f: (f.line, f.code))
    return out


def extract_blocks(text: str) -> list[tuple[int, list[str]]]:
    """Return (first_line_no, lines) for every fenced block with info string c4."""
    blocks: list[tuple[int, list[str]]] = []
    lines = text.split("\n")
    i = 0
    while i < len(lines):
        fm = FENCE_RE.match(lines[i])
        if fm and fm.group("info") == "c4":
            fence = fm.group("fence")
            start = i + 1
            j = start
            while j < len(lines):
                cm = FENCE_RE.match(lines[j])
                if cm and cm.group("fence")[0] == fence[0] and len(cm.group("fence")) >= len(fence) and cm.group("info") == "":
                    break
                j += 1
            blocks.append((start + 1, lines[start:j]))
            i = j + 1
            continue
        i += 1
    return blocks


def lint_file(path: Path) -> list[Finding]:
    text = path.read_text(encoding="utf-8")
    findings: list[Finding] = []
    for first_line_no, block in extract_blocks(text):
        findings.extend(lint_block(block, first_line_no))
    return findings


def self_test() -> int:
    fixture = Path(__file__).resolve().parent / "testdata" / "lint-cases.md"
    text = fixture.read_text(encoding="utf-8")
    lines = text.split("\n")
    cases: list[tuple[str, set[str], int, list[str]]] = []
    pending: tuple[str, set[str]] | None = None
    blocks = {first: block for first, block in extract_blocks(text)}
    for i, line in enumerate(lines):
        cm = CASE_RE.match(line.strip())
        if cm:
            codes = {c for c in re.split(r"[ ,]+", cm.group("codes").strip()) if c}
            if codes == {"none"}:
                codes = set()
            pending = (cm.group("name"), codes)
            continue
        fm = FENCE_RE.match(line)
        if fm and fm.group("info") == "c4":
            first = i + 2
            if pending is None:
                print(f"FAIL fixture line {i + 1}: c4 block with no preceding <!-- case: ... | expect: ... --> line")
                return 1
            cases.append((pending[0], pending[1], first, blocks[first]))
            pending = None

    failures = 0
    seen_codes: set[str] = set()
    names = {name for name, _, _, _ in cases}
    for name, expected, first, block in cases:
        unknown = expected - set(CODES)
        if unknown:
            print(f"FAIL {name}: fixture expects unknown code(s) {sorted(unknown)}")
            failures += 1
            continue
        got = {f.code for f in lint_block(block, first)}
        seen_codes |= expected
        if got == expected:
            print(f"PASS {name}: {sorted(got) or 'clean'}")
        else:
            failures += 1
            print(f"FAIL {name}: expected {sorted(expected) or 'clean'}, got {sorted(got) or 'clean'}")
            for f in lint_block(block, first):
                print(f"       line {f.line}: {f.code} {f.message}")

    missing_cases = [n for n in REQUIRED_CASES if n not in names]
    if missing_cases:
        failures += 1
        print(f"FAIL manifest: required case(s) missing from the fixture: {missing_cases}")
    uncovered = [c for c in CODES if c not in seen_codes]
    if uncovered:
        failures += 1
        print(f"FAIL coverage: no fixture case goes red for {uncovered}")
    if not any(not expected for _, expected, _, _ in cases):
        failures += 1
        print("FAIL coverage: no clean case proves the lint can stay green")

    print(f"{len(cases)} cases, {failures} failure(s)")
    return 1 if failures else 0


def main(argv: list[str]) -> int:
    if len(argv) < 2:
        print(__doc__.strip())
        return 2
    if argv[1] == "--self-test":
        return self_test()
    total = 0
    for arg in argv[1:]:
        path = Path(arg)
        if not path.is_file():
            print(f"{arg}: not a file")
            return 2
        findings = lint_file(path)
        for f in findings:
            print(f"{path}:{f.line}: {f.code} {f.message}")
        total += len(findings)
    if total == 0:
        print("c4-lint: clean")
    return 1 if total else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
