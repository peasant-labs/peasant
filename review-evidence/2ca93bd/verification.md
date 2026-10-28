# Peasant adoption handoff: ready for independent review

Task: https://github.com/peasant-labs/peasant/01a17f3a-1e15-786c-a22c-1fc6270cd0f6

## Current disposition after authorized setup repair

The previous blocker is resolved by the handoff's preferred **source-free native preparation plus existing boot overrides**. Both actual mounted gates now pass unchanged. No additional source commit was necessary: head remains clean `2ca93bd91ade6f8baa87a9cf7bcc29a79c5909ea`, exactly the original three-file dependency/release change. All historical failures below remain recorded, not erased or retrospectively relabelled green.

Preparation: generated a native Strike JSONL and sidecar from the existing canonical `internal/mock/testdata/strike_mounted_web.yaml` identity/content, then ran the actual `peasant harvest --detect-commits=false --json` into isolated HOME/XDG under `<temporary>/peasant-770-native-setup`. The canonical valid Strike ID now has a real stored location beside the in-memory provider's matching session. Used the supported `PEASANT_SESSION` and `SMOKE_SESSION` overrides to that ID for the generic transcript target. The fixed Strike journey, project/Map/Review coordinates, assertions, diagnostics and all source are unchanged. No legacy invalid-ID route was made valid, no API exception or response interception was introduced, and no schema fixture was renamed. Existing long-session/deep-link-turn40/input fixture coordinates are untouched.

Current results (same frozen head):

| Command | Result | Duration / log |
| --- | --- | --- |
| fresh `make build`, Node26.3.0 / pnpm11.24.0 | PASS | 24s; `peasant-770-native-build.log` |
| `PEASANT_SESSION=20260729T010203.123456789Z-AAAAAAAAAAAAAAAAAAAAAAAAAA node scripts/visual/boot-peasant.mjs` against seeded mock server8690 | PASS, all3 arms, no diagnostics | 7s; `peasant-770-native-boot.log` |
| seeded isolated HOME/XDG plus `SMOKE_SKIP_BUILD=1 SMOKE_SESSION=20260729T010203.123456789Z-AAAAAAAAAAAAAAAAAAAAAAAAAA node scripts/visual/full-app-smoke.mjs` | PASS, all12 generic surfaces in both themes plus10 fixed Strike interaction captures | 21s; `peasant-770-native-full-smoke.log` |
| fresh real-store native mounted screenshots/interactions, no mock provider | PASS,16 frames, no HTTP/client errors | 7s; `peasant-770-native-capture-final.log`, `peasant-770-native-captures/provenance.json` |
| actual system-row hide/restore | PASS, both themes | `peasant-770-native-system.json` |

Reproduction uses the retained synthetic setup root. Before starting either server, export `HOME=$root/home XDG_DATA_HOME=$root/data XDG_CONFIG_HOME=$root/config XDG_STATE_HOME=$root/state`, where `root=<temporary>/peasant-770-native-setup`. Native source bytes/sidecars and config remain there for independent review. To reconstruct the database from retained source, run `PEASANT_INGEST_ARENA_BYTES=67108864 ./peasant harvest --detect-commits=false --json` under that environment. Boot with the exact existing task mock flags, then apply the `PEASANT_SESSION` override above. Full-app smoke boots its own8699 server with the same environment and its existing full mock flags. Do not run database mutations concurrently with capture. No source/framework/test changes were needed, so a new YAML test/regression or empty commit would add no protection; the prior failed gates are the RED observation and the real prepared-store reruns are GREEN.

Representative changed-interaction proof additionally uses two synthetic native transcripts prepared and harvested in this same disposable store: a six-prompt Strike action trace derived from the existing `strike_protocol.jsonl` event shapes, and a synthetic Claude trace using the committed E2E record shapes with one system notice. These are local data, not a new shipped fixture/harness. Server ran **without mock flags** for this proof, so the real store/entries/detail path supplied the transcript and its extras. Actual project hash was obtained from `/api/v1/projects/summary`, not inferred from a label.

The16 native frames show whole shell/body in both themes at1440×1000 and390×844, default lists open/output closed, an opened Bash input/output, native per-turn Label open/cancel, and the real publish consent/login barrier open/cancel. No final publish or external login was performed. Six distinct command rows and six groups are asserted exactly; desktop compact on/off is asserted; diffs/files/annotations tab exits remain usable. Separate non-reduced-motion desktop evidence passed click selection,25-key bursts past both ends and reversal, keyboard reading-width persistence640px, zen round-trip, resting hover transition `background 0.12s`, and tabular counts (dark; light rapid-burst repeat not claimed). Both actual system-row hide and restore pass by named article, not only checkbox state. Captures used reduced motion and synthesized touch phone viewport, not physical touch hardware.

Current native screenshots/provenance: `<evidence>/peasant-770-native-captures/{dark,light}/{desktop,phone}-{reading,output,label,publish}.png`. Inspected all16 native frames, all22 fresh full-smoke frames and3 boot frames through five contact sheets. Fresh full-smoke Home now has a real seeded representative row, not the prior skeleton. Provenance JSON ties the exact process executable, rebuilt binary SHA256, served chunk bytes and release markers to the clean source SHA. The earlier inspected canonical demo15359 evidence remains the matching released-tree reference; that checkout/server was not changed. Native app fonts16px/24px, token backgrounds, zero radius and no sideways page overflow match the released demo's computed values. Different data/host shells are not asserted as pixel-identical.

Limits: native synthetic parser traces do not expose a separate Think output, so its boxed style remains demonstrated by the matching released demo rather than claimed as a new native-consumer observation. Saved-filter backward compatibility and full successful external publish are covered by existing tests/upstream evidence, not newly driven here. Pointer-interruption/continuous-motion frame proof and true overflowing-rail proof remain the user's explicit B4/B5 upstream deferrals; no new matrix was added. Phone filters are absent under the incumbent responsive policy, so compact is proved on desktop only. Two transient manual-probe failures were incorrect selectors (Label is an existing popover, not role-dialog; system is a named article, not `.txn-turn.system`), and one probe used label `fortuna` instead of the server's actual project hash. These were corrected in probes only; no source finding or gate suppression. Final capture recipe also correctly bounds compact checks to desktop; the first phone attempt timed out looking for a control this responsive view does not render. Final16-frame run is green with no diagnostics.

All earlier race/web/E2E/snapshot/distro checks below remain applicable to the identical source tree; no compiled source changed during repair, so they were not repeated. Final-head hosted release CI and fresh three-axis review remain supervisor-owned. The legacy **unprepared default mock route still returns400**, and overriding alone without native storage still returns404; this change does not pretend to fix those general developer defaults. It supplies the explicitly authorized supported synthetic setup for honest mounted acceptance instead.

## Frozen source

- Worktree: `<workspace>/peasant/peasant-770--release--fairtrade-transcript-adoption`
- Branch: `peasant-770--release--fairtrade-transcript-adoption`
- Local commit: `2ca93bd91ade6f8baa87a9cf7bcc29a79c5909ea`
- Base: `d461022c858ef800782490318e6b59b3558b75e4`; PR759 was already merged into this clean, freshly fetched default. The required normal default build passed before branch creation (26s).
- Version candidate: v0.13.3; absent from canonical tags/releases and open release PRs at preflight. Recheck before opening the one combined release PR.
- Files: `CHANGELOG.md`, `web/package.json`, generated `web/pnpm-lock.yaml` only. Private web version unchanged. No backend, API, schema, fixtures, harness, or UI source edits.
- Exact dependency: published `@peasant-labs/fairtrade` 0.0.48; SRI `sha512-z3NjuhSETH89pTBwe7RrFAPTdFT0vz5bXefdSQt9f8yaAhJMj/RWD9s1oro93sm2Xy1b6bW4A7tsYw0GLvfMzg==`. Matches supervisor publication receipt. No local links.
- Commit created through `git agent-commit`; nothing pushed, tagged, opened, merged, or closed.

## Completed checks

All logs are under `<evidence>/` with the `peasant-770-` prefix. Final source tree was identical throughout checks and is clean; build/captures identify the committed head. No pending gates.

| Command | Result | Time / evidence |
| --- | --- | --- |
| `pnpm install --lockfile-only` | PASS, only Fairtrade resolution changed | initial regeneration 3s including install |
| `pnpm install --frozen-lockfile && pnpm typecheck && pnpm test` | PASS, 124 files / 1648 tests | 33s; install/typecheck/test logs |
| `make check RACE=1` | PASS, exactly-once screen passed | 771s; check.log; expected committed non-blocking budget warning, combined test wall 12m15.486s |
| `make build` | PASS | 29s; build.log, real Next export and embedded CLI provenance gate |
| `make e2e-schema-parity VILLAGE_REPO=<temporary>/peasant-770-village-e2e` | PASS | exact workflow-pinned Village `2f98f74a3498fb6d01e37e396dfea565c67cb2f3`, Schema v0.35.0 |
| `make e2e E2E_TEST_FLAGS=-v` with pinned Village checkout | INCOMPLETE, exit0 but service tests skipped | 14s; e2e.log; isolated HOME prevented peer Git VCS status; distro test skipped missing snapshot |
| documented prebuilt Village binaries (`go build -race` for server and setup-demo) | PASS | 5s; no source edits |
| bounded `make e2e` retry of the eight skipped service/demo test names with `VILLAGE_BIN` and `SETUP_DEMO_BIN` | PASS, no skips | 293s; e2e-retry2.log; required skip-gate, pull, refresh, Pi, convergence, hook and identity paths passed; hard memory scope/no swap retained |
| `goreleaser release --snapshot --clean` | PASS, five targets and package/checksum artifacts | 35s; snapshot.log; no publication |
| bounded `make e2e E2E_TEST_FLAGS='-v -run ^TestReleasePerDistro'` with same prebuilt peers | PASS, all five distro subtests | 108s; release-e2e.log; Ubuntu22.04/24.04, Fedora, openSUSE, Arch |
| `node scripts/visual/boot-peasant.mjs` | FAIL | 2s; boot.log; mock session commits/PR requests400 |
| `SMOKE_SKIP_BUILD=1 node scripts/visual/full-app-smoke.mjs` | FAIL | 18s; full-smoke.log; generic transcript both themes400, Strike mounted flow both themes404 on extras |
| `PEASANT_ORIGIN=http://localhost:8690 TRANSCRIPT_INPUT_OUTPUT=… node scripts/visual/transcript-input-gate.mjs` | PASS, all eight fixture cases | 7s; input.log; native wheel, trackpad equivalent, PageDown, End, Home, focused control wheel, deep link, short transcript |
| `git diff --check`, staged inspection, internal-taxonomy grep | PASS | no shipped internal task strings |

The successful pinned-tool web and build gates used Node26.3.0 with pnpm11.24.0, exposed by `<temporary>/peasant-adoption-tools/pnpm`. The initially used `corepack` shim resolved Node24 despite the shell Node26; frozen install/typecheck/tests and the final build were rerun with the explicit Node26 wrapper. No global tool installation or hooks changed. The first focused E2E retry failed shell parsing before any tests (unquoted regex in Make recipe); retry2 quotes it correctly. Successful tests were not repeated.

## Blocker: existing mock transport diagnostics

The mandated route mounts a real transcript through the binary and WebSocket:
`http://localhost:8690/projects/fortuna/sess-c3d4e5f6-a7b8-9012-cdef-123456789012/`.
But GET `/api/v1/sessions/sess-c3d4e5f6-a7b8-9012-cdef-123456789012/commits` and `/pull-requests` both return400 `invalid session ID`. The host's `useSessionExtras` makes these display-only requests beside the transcript. Strict API session-ID validation rejects the synthetic prefixed mock ID. Full-app smoke also reports404 for the Strike synthetic session's extras.

Confirmed the SAME400 responses with the separately built, unchanged default-branch binary pinned to Fairtrade0.0.47, running in its own synthetic directories on8691. Evidence: `<temporary>/peasant-770-baseline/diagnostics.json`. This is not introduced by the package update. Neither response interception nor diagnostic exclusions nor API changes were made.

Supervisor decision needed: use a valid native synthetic recorded session for clean transcript production proof, or explicitly authorize a narrowly scoped existing mock fixture repair. The generic full-app gate still needs a valid resolution, not simply a new screenshot. Do not claim either failed mounted gate passed. No broader source change was attempted.

## Mounted evidence and provenance

- Captured on exact built `./peasant`, PID939143 at capture time; `/proc/939143/exe` resolved to the isolated worktree binary. Binary SHA256 and every referenced app JS/CSS byte comparison are in `peasant-770-captures/provenance.json`.
- `assertServedBuild` verified current checkout `web/out`, binary freshness, served chunk set and release markers `txn-thinking-box`, `txn-sticky-disclosure`, `txn-rail-title` before capturing.
- Demo: existing15359 Vite preview, untouched; source tree `1d6bab0b0d53e2f7db7fdd4525995d92b920c508` equals release merge5a2f9f3 tree. Served demo asset hashes match that checkout's `dist`.
- Chromium Playwright, desktop1440×1000, emulated touch phone390×844, reduced motion. Actual root AND transcript themes asserted, no forced DOM theme. Real first tool disclosure clicked from closed to open. Demo additionally opens Think. Primary captures retain whole viewport chrome/body, not cropped components. Different synthetic data means visual comparison, not a same-data pixel-parity claim.
- Computed app body Atkinson proportional16px/24px; mono chrome; radius0; theme ground dark `rgb(11,11,10)` and light `rgb(251,250,247)`; no page horizontal overflow. Demo matches these design values. Host's collapsed filters is deliberate existing `filtersOpen=false`, not a divergence to fix.
- Inspected all16 primary frames,8 input frames,22 full-smoke frames through the seven generated contact sheets. Primary states have nonblank shell/body and responsive reading; failed full-smoke dashboard captures contain a loading skeleton and are NOT representative-body completion proof. Mock tool output is sparse. No new visual polish source changes.
- Standing checklist read entirely. Proven here: action lists initially open, outputs initially closed, actual tool disclosure; real scrolling/deep link/outline; both rendered themes, full shell, responsive widths, shared fonts/tokens. Not fully re-proven on Peasant's sparse mock: Think/Bash input, actual system-row hide/restore, compact-mode toggle, rapid selection bursts, width interruption/no snapping, interrupted motion, retained panels/share exits. Unit tests protect host publish/redaction paths; that is not a claim of full current mounted checklist coverage. Upstream accepted release proof remains upstream, not substituted as consumer proof.
- Impeccable context/harden/audit guidance loaded; incumbent Fairtrade authority preserved. Narrow audit passes observed theming/type/radius/reflow but remains incomplete on interaction/accessibility and error-path coverage because of the blocker. Detector run on the changed manifest is recorded in `peasant-770-impeccable.json`; no UI source changed. Do not claim a full WCAG or motion audit.

Primary screenshot set:
`<evidence>/peasant-770-captures/{demo,app}/{dark,light}/{desktop,phone}-{reading,output}.png`

## Historical supervisor next actions (superseded by current disposition above)

1. Resolve/authorize the mounted-proof blocker without weakening diagnostics or broadening the release silently.
2. Finish currently unproven consumer checklist items, then fresh review (max3 rounds) on exact frozen head or authorized repair head.
3. Publish durable inspected screenshots, open ONE combined pin+release PR into develop. Draft body: `peasant-770-pr-body.md`.
4. Apply/reapply mandatory final-head `full-gate`; all required release PR jobs must pass. Hosted native Windows/macOS, ARM package matrix, brew/Nix validation and the label ceremony are NOT claimed by this local Linux run; supervisor owns those gates and release automation.
5. After merge verify automated tag/release/checksums/smokes and sync/build default. No manual tags.

Worker remains available for review fixes. Source frozen; no task closed.
