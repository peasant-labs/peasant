# Independent replay preservation review

## Lower three frozen layers

Result: no blocker, important or minor findings. Review is source/distribution preservation only; no runtime tests were rerun.

| PR | Frozen head | Original own commits | Retained original commits | Added mechanical commit |
|---|---|---:|---:|---:|
| #521 | `9756a18c458ff949e0da0ed98b290816101632da` | 19 | 19 | 1 |
| #530 | `146ca207b9f4a9a37289adca187e2d9b32061f03` | 18 | 18 | 1 |
| #532 | `8455ae2699482656896f660b26c76131730448ce` | 11 | 11 | 0 |

Range-diff accounts for every original own commit. The 521 replay differences are import context only, followed by its new generated vendor hash; 530 retains its consent pin and adds the ordinary-package import/hash adaptation; 532 preserves all eleven patches exactly. Tests, fixture YAML, named deletion manifests, production decisions and assertion bodies are unchanged. All Go endpoint differences in original own files normalize exactly after replacing the upstream SQLite import literal with the canonical ordinary package and sorting import declarations. The three imported publication/API fixture tests retain their entire executable bodies.

Endpoint byte comparison preserves 49/56 own files at 521, 53/62 at 530, and all 24 at 532. Remaining differences are documented imports, go.mod/go.sum, notices and the generated Nix hash. The canonical foundation scripts, workflows, Makefile and flake.lock are unchanged on all three layers. The flake devTools and build configuration are retained; only vendorHash changes to match the actual schema graph. The ordinary driver remains part of the main module, with no relative replacement or nested-module dependency.

Inspected each final dependency proof and independently matched the frozen go.mod/go.sum SHA-256 and actual flake hash. The isolated dependency executor archives the staged source, checks module edges, exports a fresh vendor tree and checks its complete fingerprint against the real prewarm inputs; temporary owned source/vendor trees are cleaned. Actual updater logs match 0.25's `sha256-vgdWH1yejzxjl2lpd3+oqadzyqyEGF5Yybu9+vFrqqk=` and 0.26's `sha256-uB0GW0BAcO/2nW1yLR0mxnC1Zq5/NI3msrA4atT7W7E=`. This is distribution evidence, not a runtime pass. Root owns actual Linux store/race gating.

Detailed file/range accounting: `peasant-lower-three-replay-axis-b-accounting.json`. Upper layers and top 539's original 863e fixture correction remain to be confirmed when their frozen heads arrive.

## Four frozen interface layers

Result: no blocker, important or minor findings at these exact heads.

| PR | Frozen head | Original own commits preserved exactly | Own changed files byte-identical |
|---|---|---:|---:|
| #523 | `0ca70961d964302aab2b6d457a916c9e460ab9ec` | 21 | 117 |
| #537 | `234ab46013e24ea33e2a2916120a529ed81c972b` | 10 | 22 |
| #538 | `cc4d238399a8ef923c7503bc395993a7e2768a3f` | 5 | 27 |
| #536 | `b9c09c2f367f96334dc18e8e4f2a4e8ec5cddc58` | 16 | 59 |

Full tracked web trees are byte-identical to their original reviewed heads, including fixture and capture harness files. All original own patches are exact range-diff matches. Each source/dependency proof matches its final go.mod/go.sum and generated flake vendorHash, with the schema 0.26 graph. Foundation generator configuration, workflows, scripts, Makefile and flake.lock remain unchanged. No tests or source mutations were performed by this reviewer. Top 539 remains pending its final frozen head and the explicit original 863e fixture-correction check.

## Final layer and whole-stack conclusion

**All eight frozen layers: no blocker, important or minor findings.**

Top 539: `1708ee11cf356d1ea8991828be625c545f0356ef`. Thirteen original own commits retained, including original local 863e86d, with one additional generated graph/hash commit. Its pin replay differs only by leaving the foundation vendor hash in place until the new graph is validated and its actual hash is committed. Of 77 original own changed files, 73 are byte-identical; remaining files are go.mod/go.sum, generated notices and vendorHash. Full tracked web source, fixtures and capture harness are byte-identical to original 863e.

The full nine-file 863e correction is byte-for-byte preserved: three real config/settings/navigation fixture test files plus all six ANSI golden outputs. This comparison uses local original 863e, not its older remote e3fa head. Actual settings API assertions, physical fixture paths, bounded section navigation and real terminal expectations remain intact.

Schema 0.27 dependency proof matches frozen go.mod/go.sum and actual `sha256-Ff1h18VZ2pNuPTDE3zjWNsoYbwUh/JZW0pKEatiCpqk=` updater output. Across all eight heads, the ordinary audited driver subtree is byte-identical to merged foundation3217f21. Flake build/devTools configuration is identical after normalizing only vendorHash; canonical generator scripts, Makefile, workflows and flake.lock are unchanged. Every original113 own commits is accounted for. Three extra mechanical adaptation/hash commits were added at521/530/539; none of the original fixture obligations were deleted or substituted.

Root owns actual Linux/Pi/race/module-install/build gates and fresh binary/served provenance. Existing screenshots remain tied to their original captured source; byte-identical web trees allow preserving those images only with explicit original-versus-current provenance. This review does not relabel old captures or claim a fresh runtime pass. No source edits, tests, rebases or publication occurred in this independent review.

Detailed final accounting: `peasant-top-replay-axis-b-accounting.json`, with actual `peasant539-axis-b-actual-range-diff.txt`; lower/four-upper accounting listed above.
