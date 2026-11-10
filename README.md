# Home adoption screenshots (peasant#857)

Durable both-theme evidence for the shared-table Home adoption.

- `sourceHead`: the implementation branch head these captures were taken from
  (`peasant-857--feat-home-adoption--shared-table`).
- `reviewedHead`: the head the review wave inspected before the follow-up fixes.
- `home/`: the mounted Home journey (`home-pages-visual.mjs`), both themes at
  desktop and phone.
- `root-page/`: the mounted root-page journey (`root-page-visual.mjs`), both
  themes at desktop and phone, every filter and family state.
- `web-test-transcript.log`: the full `pnpm --dir web test` transcript for
  `sourceHead` (136 files, 1761 tests, exit 0). `make check` is Go-only and
  peasant CI runs no frontend tests, so this transcript is the web gate record.
- `provenance.json`: the source head, pins, binary and chunk hashes, commands,
  and the capture inventory.

Generated PNGs are evidence only; they are not application assets.
