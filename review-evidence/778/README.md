# Mounted local-app evidence

Exact PR source `0c1f46452b4ed71f9e2d03018a8946566ed418e8`, published Fairtrade `0.0.54`. A normal Go 1.25.8 `make build` from a clean external worktree produced the binary identified in `provenance.json`. The provider artifact guard passes against the compiled web output and embedded binary. The preview serves only isolated synthetic recordings and data.

All 16 mounted images were inspected: home, redaction, the long transcript and its rendered diagram in both themes at desktop/mobile sizes. There is no document overflow, browser exception or failed read. Turn icons retain their borders and compact 24px visual size. The header and reader agree on 84 authored turns with 28 tool calls; the synthetic demo also contains 13 diffs, 9 files and human annotations.

The final diagram check asserts transformed text and node bounds lie inside the SVG viewBox, uses the consuming app's resolved font, retains 16px labels, and verifies keyboard panning on a touch-sized viewport. A long diagram intentionally scrolls within its accessible region rather than shrinking its text to fit a phone. This replaces the provisional 0.0.53 evidence, whose font mismatch clipped two labels.
