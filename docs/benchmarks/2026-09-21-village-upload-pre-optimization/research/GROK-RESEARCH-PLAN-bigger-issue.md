# Grok Research Assignment — Root Cause of the “Bigger Issue”

**Issued:** 2026-09-21 (PT)  
**Issuer:** Peasant Lead (Nicholas Tam’s peasant-labs contribution lead)  
**Tooling:** Interactive Grok Build TUI (`grok`), auto-approve tools, work in `/workspace/peasant-labs`  
**Priority:** Investigate and prove the root cause; do not implement a fix yet unless needed to confirm the cause.  
**Deliverables (required):**
1. A completed structured log using the template below (one Finding block per discrete discovery).
2. A root-cause write-up that names the exact code path(s), why memory scales with DB size, and why free-RAM cleanup did not make `007-2gb` dry-run succeed.
3. A draft GitHub-issue body (Title / Summary / Evidence / Suspected root cause / Reproduction / Acceptance criteria) ready to file later.
4. Pointers to concrete files + symbols (package/function/line ranges) in the peasant polyrepo.

---

## 0. What “the bigger issue” is (problem statement)

During Village upload **pre-optimization benchmarking** on the shared Grok Bot box (15 GiB RAM, **0 swap**), scaled synthetic corpora were harvested and then run through:

```text
peasant … village push --dry-run --non-interactive --yes --timing --verbose \
  --profile-output …/push-profile.json \
  --profile-trace  …/push-profile.jsonl
```

Observations:

- Sets **001–005** (up through ~500 MiB corpus / multi-GiB peak RSS on 005) could complete dry-run profiling. **Redaction** dominated CPU once corpora left toy size.
- Set **006-1gb** hit harvest OOM and/or push open failures.
- Set **007-2gb** (~2.02 GiB corpus → harvested `peasant.db` ≈ **4.29 GiB**):
  - **Primary push:** exit **137** (kernel OOM-kill); peak RSS ~**10.79 GiB**; dmesg anon-rss ~**11.38 GiB**; **no** profile written.
  - **Retry** (`--concurrency 1`, `GOMEMLIMIT=12GiB`): exit **3**; soft fail opening analytics store with sqlite **deserialize / memory allocation failure**; still **no** profile.
  - **Post-cleanup retry** (linkedin-guest Chrome killed; 18 stale Fork-4 tabs closed; `MemAvailable` ≈ **10.5 GiB**, well above the 005 peak of ~4.25 GiB): **again** exit **3**, wall ~20 s, peak RSS ~**9.99 GiB**, same sqlite deserialize failure, **no** profile.

**Caller framing:** Freeing memory / closing Chrome was **not** the real fix path. There is a **bigger issue**: dry-run / read-only store open appears to **deserialize the entire SQLite DB into process memory and then make a second full copy** (`store.OpenReadOnly` / related path). That architectural behavior makes multi-GiB DBs fundamentally unsafe on a 15 GiB host **before** redaction, payload build, or any Village network I/O.

**Non-goals for this research:**
- Do not start a live Village server or perform real HTTP publish.
- Do not re-run the full 1–2 GiB harvest unless required for a minimal repro (prefer reusing existing `bench-runs/007-2gb` state).
- Do not “optimize redaction” yet — redaction is a separate CPU bottleneck on successful mid-size runs; this ticket is about **memory at dry-run open**.
- Do not invent timings; only cite measured artifacts or newly measured numbers.

**Important policy already documented:** every run today was **`--dry-run` only** — **zero** HTTP requests to Village.

---

## 1. Research objectives (verbose)

### Objective A — Confirm the failing API surface
Prove which CLI entrypoint and Go call chain runs when `village push --dry-run` starts on an already-harvested DB:
- CLI flags → command handler → village push service → analytics / session store open.
- Distinguish **harvest write path** vs **push dry-run read path**.
- Identify whether dry-run intentionally chooses a deserialize-based open (e.g. for immutability / snapshot isolation) vs accidentally sharing a code path meant for small DBs.

### Objective B — Prove the double-materialization hypothesis
Locate and quote the implementation of `store.OpenReadOnly` (or today’s equivalent name) and any helpers that:
1. Read the on-disk SQLite file (or dump) into a byte buffer / memory DB.
2. Call sqlite deserialize (or go-sqlite3 / modernc / crawshaw / etc. equivalent).
3. Duplicate the buffer or reopen a second in-memory copy.
Measure or reason about peak RSS ≈ `k * sizeof(DB)` with `k ≥ 2` plus Go heap overhead.

### Objective C — Tie failure modes to the same root cause
Explain as one causal chain:
- OOM-kill (137) on primary 007 attempt,
- soft exit 3 with `sqlite: deserialize … memory allocation failure` on constrained retries,
- why ~10.5 GiB `MemAvailable` still fails for a ~4.29 GiB DB,
- why mid-size sets succeeded (DB small enough that 2× fit).

### Objective D — Scope blast radius
Which other commands use the same open path? (`village push` without dry-run, local query, redact preview, doctor, sync, etc.)  
Would a live push hit the same wall before network? Or only dry-run?

### Objective E — Produce an issue-ready artifact
Using the logging template, emit findings that can be flagged as a single engineering issue with clear reproduction and acceptance criteria (e.g. “dry-run open of an N-GiB DB must not require ≥2N GiB RSS”).

---

## 2. Known evidence (start here — verify, do not discard)

### Paths
- Package / reports:  
  `/workspace/peasant-labs/2026-09-21 Village Upload Pre-Optimization Benchmarking/`
  - `README.md` (process, no-network note, initial 007 failure, post-cleanup retry)
  - `CONSOLIDATED-REPORT.md`
  - `Individual Sub-Agent Reports/007-2gb.md`
- Run sandbox: `/workspace/peasant-labs/bench-runs/007-2gb/`
  - `data-home/peasant/peasant.db` (~4.29 GiB) — confirm exact filename (`peasant.db` vs `peasant.db` variants)
  - `profiles/push.meta`, `push.stderr`, `rss-peak.txt`, archives of prior attempts
- Code: `/workspace/peasant-labs/polyrepo/peasant/develop/` (binary `bin/peasant`, version `profile-box`)
- Design notes: `/workspace/peasant-labs/VILLAGE-UPLOAD-DESIGN.md`, `FOUNDATIONAL-KNOWLEDGE.md`

### Measured failure signatures (cite exactly when logging)
- Exit 137 / dmesg OOM on peasant with anon-rss ~11.38 GiB class.
- Exit 3 stderr class:  
  `sqlite: deserialize to "main": memory allocation failure`  
  (wording may vary slightly: `deserialize to "main"` / `deserialize to "main"` — copy **exact** current stderr from artifacts).
- Post-cleanup retry (2026-09-21 ~22:40 local): exit 3, wall ~20140 ms, peak RSS ~10476284 KiB, no profile files.

### Hypothesized root cause (to confirm or refute)
`store.OpenReadOnly` (name TBD by code search) deserializes the entire DB into memory then makes a second full copy, so dry-run open alone demands on the order of **2 × DB size** heap before any useful push work.

---

## 3. Method — required investigation steps (do in order)

### Step 1 — Inventory symbols
Search the peasant tree for:
- `OpenReadOnly`, `OpenReadonly`, `OpenReadOnly`, `Deserialize`, `deserialize`, `sqlite3_deserialize`, `Serialize`, `Backup`, `Memory`, `dry-run`, `DryRun`, `analytics store`, `inspect … for dry-run`.
Record every hit with file:line in a Finding.

### Step 2 — Trace CLI → open
From `village push` command definition, walk to the first store open under `--dry-run`.  
Draw a short call chain (function names only).

### Step 3 — Read the open implementation end-to-end
For the winning function(s):
- What bytes are read from disk?
- Is there an explicit `make([]byte, size)` or `io.ReadAll` of the whole file?
- Is sqlite deserialize invoked? Which driver?
- Is there a second clone/copy/Open of the memory DB?
- Are there comments explaining why?

### Step 4 — Correlate with runtime evidence
Compare code expectations to:
- `bench-runs/007-2gb` DB file size on disk,
- peak RSS from push attempts,
- whether failure happens **before** profile stages appear (expected if open fails first).

### Step 5 — Minimal confirmation (optional, careful)
If safe on this host: a tiny Go snippet or `peasant` debug log (if exists) that prints heap around open — **or** simply argue from code + existing RSS without another OOM. Prefer not to OOM-kill the shared box. If you must run, use the smallest DB that still exercises OpenReadOnly (e.g. copy of 003/004) to show RSS ≈ f(DB size).

### Step 6 — Document alternatives (design-level only)
List 3–5 plausible fixes **without implementing**: mmap/readonly file open, streaming query without deserialize, temp file snapshot, page-cache friendly sqlite open, size guard that fails fast with a clear error before trying 2× allocate, etc. Rank by safety for dry-run semantics.

### Step 7 — Issue draft
Fill the issue section of the logging template completely.

---

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

## 5. Output files Grok must write

1. `.../research/FINDINGS-LOG.md` — filled template.  
2. `.../research/ROOT-CAUSE.md` — narrative ≤ ~200 lines, no fluff, with call chain.  
3. `.../research/ISSUE-DRAFT.md` — copy of the Draft GitHub Issue section.

When finished, print a short stderr/stdout style summary:
`RESEARCH COMPLETE — root_cause=<confirmed|refuted|blocked> — findings=<N> — issue_draft=<path>`

---

## 6. Constraints & safety

- Shared box: avoid intentional multi-GiB OOMs; prefer code reading + existing artifacts.
- Do not delete `bench-runs/007-2gb/data-home`.
- Do not modify production configs outside the research folder except ephemeral debug if unavoidable (document it).
- Stay on dry-run / local analysis.

---

## 7. First actions (do now)

1. Create `FINDINGS-LOG.md` with header Status=IN_PROGRESS.  
2. `rg` / code search for OpenReadOnly + deserialize.  
3. Log Finding 001 with the exact open function and file:line.  
4. Continue until Root Cause Statement can be marked confirmed or clearly refuted.

Begin.


---

## 8. Caller amendments (2026-09-21 ~22:53 PT) — REQUIRED

Nicholas approved the plan with these additions:

### 8.1 Deliverables (expanded)
1. **Fill the structured logging template** (`FINDINGS-LOG.md`) — required but **extra / supporting**.
2. **Also produce your own standalone research report** (Grok’s narrative):  
   `research/GROK-REPORT-bigger-issue.md`  
   This is a first-class deliverable, not a paste of the template. Include: executive summary, call chain, root cause, evidence, harness-vs-real-Village analysis, recommendations, open questions.
3. Keep `ISSUE-DRAFT.md` / issue section for later filing.

### 8.2 Local Village workflow verification (NEW — critical)
Do **not** conclude that the failure is only an architectural dry-run issue without checking whether the **testing harness** (`village push --dry-run` / `store.OpenReadOnly` deserialize path) is the thing that forces full-DB materialization.

**Required experiment:** attempt the **proper end-to-end workflow against a local Village** (real HTTP to a Village process running on this box or in compose), not dry-run stubs:

1. Discover how this polyrepo runs Village locally (README / compose / `make` / nix / binary under `polyrepo/village/develop`). Document what you find.
2. If Village can be started: bring it up locally; configure peasant credentials/url to that local Village (isolated XDG under a new `bench-runs/007-local-village/` or similar — do not clobber 007-2gb dry-run artifacts).
3. Run the **non-dry-run** push path against local Village for a **small** control set first (e.g. 001 or 002), then — only if memory-safe — attempt a larger set or a reduced subset of 007.
4. Compare code paths: does live push open the store via normal `store.Open` (file-backed) vs `OpenReadOnly` deserialize?
5. Record in both the logging template **and** your report whether:
   - the OOM/deserialize failure is **dry-run/harness-specific**, or
   - live local Village push hits the **same** memory wall, or
   - live push fails for a **different** reason (auth, schema, transport, redaction, etc.).

If local Village **cannot** be started on this box (no docker/podman, build blockers, etc.), log that as a Finding with `issue_flag` as appropriate, state exactly what blocked it, and still complete the code-path comparison (dry-run vs live push open path) from source so the harness hypothesis is answered analytically.

**Safety:** Prefer small corpora for the first live local push. Do not OOM-kill the shared box with a blind 007 live push; escalate size only with MemAvailable headroom documented.

### 8.3 Explicit non-regression
Continue zero reliance on Nicholas’s Mac transcripts. Synthetic / bench-datasets only.
