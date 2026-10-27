# Stored-library browsing review

Source commit: `85f4f64af24c1de05218bc69fa4259cd71b4ed8c` (Peasant #759).
Published dependencies: Go/npm schema 0.35.0, Fairtrade 0.0.47.

Fresh captures came from this exact clean-head built binary, not a renamed copy
of the preceding build. Both mounted runners passed served-build provenance,
computed-style, theme, navigation and state assertions in isolated HOME/XDG roots.
Only synthetic fixture data is shown; no live store or user server was used.

- All twelve root picker PNGs are SHA-256 byte-identical to the personally opened
  and inspected preceding `713b6ee3` captures. `image-identity.json` records both
  hashes per file. The fresh capture and identity proof preserve that visual review.
- The four publication PNGs changed with canonical Fairtrade 0.0.47 and were all
  freshly opened and inspected in dark/light and desktop/mobile. The ordinary
  not-published label and explicit publish action remain correct; upstream ledger
  fixes are retained.

`root/{dark,light}/{desktop,phone}/` contains Home, Map and Review project pickers;
`publish/{dark,light}/{desktop,mobile}/` contains the outside-ingestion-selection
transcript's ordinary publication bar. Each image retains full application chrome.
Browser fixtures prove presentation; real-store HTTP tests prove row eligibility.

Keep this screenshot branch available while the PR and embedded images exist.
