# PR 791 mounted production evidence

Source: `3e39a150cc969756e0f247a586cb7ae7343b98bd`; exact Fairtrade `0.0.55` with generated pnpm lockfile.

Binary: normal Go 1.25.8 embedded-web build from external worktree `/private/tmp/peasant-ui-final-check`; `peasant v0.13.6-dev.4+3e39a150c`.
SHA-256: `c0e767775797826523fc410ceea975f0a4d66598ee5a415fd56039afa2eba159`.
The listener on port 8691 uses only disposable synthetic data/config/state under `/tmp/peasant-ui-preview`; it serves the actual embedded build, with no development proxy or response mocking.

All 16 screenshots were visually inspected: home, redaction settings, long transcript start and the later implementation-phase boundary, light/dark, 1440x1000 and 390x844. Persistent product chrome and mounted body are present; text, role icons and controls remain readable, without page-level horizontal overflow. The selected turn remains marked.

`measurements.json` records the 12 full-shell captures, theme/viewport, zero page errors, width, numeric fonts and mounted counts. `phase-measurements.json` records four focused mounted passes over the same long transcript: 84 turns, each phase parent is `txn-turnrow` with zero direct `txn-turn-rule` children, each phase owns one 1px bottom border, ordinary rules remain 1px, role icons remain 24px with 1px borders, and the active turn retains its inset highlight. Focused screenshots scroll the real transcript to its second phase without hiding or restyling any elements.

This synthetic long transcript contains no system rows. System-row ownership is verified by Fairtrade PR 217's canonical mixed-system fixture: four mounted separator tests pass for both themes and desktop/phone. Its output is retained in `fairtrade-55-mounted-final.log`; consumer screenshots do not claim to cover absent system rows.

Validation: 124 frontend files / 1,664 tests passed; TypeScript passed; all 14 contract-boundary production mutants killed. GitHub required checks for PR 791 are green, including Linux make check, Windows, built-binary journey, real frontend build and lockfile synchronization. No source changes were made for captures.

The research document is about installation and canonical Kickstart, explicitly distinguishes existing terminal setup from the proposed native installer/window, and includes the compact bootstrap Mermaid diagram. No installer or native GUI is implemented by this PR.
