# PR #489 — Before / after evidence

feat(layout): capture the Grok Build session layout

- Before: base tip `883c4b3995ae9f0212b6f306fa7e6c4eaafa6207` of `cursor/harness-layout-capture-35a4` (PR #488) — `peasant layout list` is empty and `peasant layout capture grok-build` fails with `unknown layout tool "grok-build" (known: [])`.
- After: PR head rebased to `ece8197b465c184e9dd940ffb81a5c3abfced9b6` — `layout list` shows Grok Build with all declared artifacts; `layout capture grok-build` on the synthetic fixture finds 3 sessions and shapes their artifacts without leaking transcript text.
- Media: `before.png`, `after.png`, `before-after.mp4` (16.5s terminal recording rendered from the real command outputs in `before.txt` / `after.txt`).
- Logs: `grok-tests.log` (5 TestGrokBuild tests + full harnesslayout package pass), `cmd-test.log` (cmd/peasant layout tests + go vet pass).
- All fixture data is synthetic (the PR's own testdata). No real transcripts, tokens, or personal paths.
