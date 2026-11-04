# Statically linked components of the regex engine

`github.com/wasilibs/go-re2` ships a WebAssembly build of RE2. The blob is a
statically linked artifact: it contains RE2, cre2 and Abseil, so a binary that
embeds the blob redistributes all three. Their licences are permissive and
require their notices to travel with the binary.

The pinned versions come from the module's own build recipe, not from a guess:

| Component | Version | Source |
|---|---|---|
| RE2 | tag `2025-11-05` | `buildtools/wasm/version.txt` in the pinned go-re2 module |
| Abseil | tag `20240722.0` | `buildtools/wasm/Dockerfile` in the pinned go-re2 module |
| cre2 | vendored copy | `internal/cre2/NOTICE.txt` in the pinned go-re2 module |

`RE2-2025-11-05-LICENSE` and `ABSEIL-20240722.0-LICENSE` are the upstream texts
at those tags — the version is part of each filename — committed here because the
go-re2 module does not carry them and the notices generator must stay offline and
byte-stable. cre2's notice is read from the module cache instead, so it follows
the pinned module.

`scripts/gen-third-party-notices.sh` reproduces all three in
`THIRD_PARTY_NOTICES`. When the go-re2 version changes, re-read its
`buildtools/wasm/` recipe and refresh these files if either tag moved.
