# Structured Logging Template (for caller review)

Extracted from the research plan for standalone review. Do **not** feed to Grok until Nicholas explicitly approves.

## 4. Structured logging template (REQUIRED)

Copy this template into your working notes and into the final deliverable file:

`/workspace/peasant-labs/2026-09-21 Village Upload Pre-Optimization Benchmarking/research/FINDINGS-LOG.md`

Use **one Finding block per discovery**. Do not merge unrelated facts. Timestamps in America/Vancouver (PT).

```text
================================================================================
FINDINGS LOG — Village upload “bigger issue” (dry-run store memory)
================================================================================
Session: <grok session id or start time PT>
Agent: Grok Build TUI
Repo: /workspace/peasant-labs/polyrepo/peasant/develop
Related bench: /workspace/peasant-labs/bench-runs/007-2gb
Status: IN_PROGRESS | ROOT_CAUSE_CONFIRMED | ROOT_CAUSE_REFUTED | BLOCKED
================================================================================

### Finding <NNN>
- timestamp_pt: YYYY-MM-DD HH:MM:SS PT
- category: code_path | measurement | hypothesis | refutation | blast_radius | repro | issue_candidate | other
- severity: info | low | medium | high | critical
- confidence: low | medium | high
- summary: <one sentence>
- details: |
    <multi-line evidence, quotes, numbers>
- evidence_paths:
    - <absolute path or file:line>
- symbols:
    - <package.Func or CLI flag>
- metrics:
    - db_bytes: <int or n/a>
    - peak_rss_kib: <int or n/a>
    - exit_code: <int or n/a>
    - wall_ms: <int or n/a>
- links_to_findings: [<NNN>, ...]
- issue_flag: false | true
- issue_labels: []  # e.g. ["bug","performance","memory","village-push","dry-run"]
- notes: |
    <optional>

--------------------------------------------------------------------------------

### Root Cause Statement (fill when ready)
- statement: |
    <precise causal statement>
- primary_symbol: <Func>
- primary_path: <file:lines>
- mechanism: |
    <how memory is multiplied>
- why_cleanup_was_insufficient: |
    <why 10.5 GiB MemAvailable still failed for ~4.29 GiB DB>
- disproved_alternatives:
    - <e.g. “redaction is the memory killer at open time” — disproved because …>

--------------------------------------------------------------------------------

### Draft GitHub Issue
- title: <50–80 chars>
- body: |
    ## Summary
    ...
    ## Evidence
    ...
    ## Suspected root cause
    ...
    ## Reproduction
    ...
    ## Acceptance criteria
    ...
- suggested_labels: []
- suggested_assignees: []
- blocked_by: []
- related_docs:
    - /workspace/peasant-labs/2026-09-21 Village Upload Pre-Optimization Benchmarking/README.md

================================================================================
END OF TEMPLATE
================================================================================
```

**Flagging rule:** set `issue_flag: true` on any Finding that alone would justify filing (confirmed double-copy, confirmed OOM linkage, confirmed dry-run-only vs all opens, etc.). The Root Cause Statement should reference those Finding IDs.

---
