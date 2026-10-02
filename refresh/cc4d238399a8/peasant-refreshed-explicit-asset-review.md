# Explicit exported asset review

Root inspected the per-file byte differences against the hash-addressed original exports. This is a comparison of real normal builds, not a filename/hash exemption. No images were recaptured.

| PR | Concrete differences |
|---|---|
| #523 | HTML: the recorded build ID and exact referenced runtime/common/session filenames. Common chunk re-export wrapper ID8206 becomes5825 and moves to its sorted module position; session entry ID5825 becomes8206 and its bootstrap reference changes with it. All function bodies are unchanged. Webpack initialized-chunk map ordering changes, with the same numeric entries and zero values. |
| #537 | HTML: the recorded Next build ID only. The two build-manifest filenames move under that new ID with identical file bytes. Runtime JavaScript and all CSS are identical. |
| #538 | HTML: the recorded Next build ID and exact webpack runtime filename. Runtime initialized-chunk map key order changes, with the same numeric entries and zero values. The two manifest bodies are identical; all other JavaScript and CSS bytes are unchanged. |
| #536 | HTML: the recorded Next build ID only. The two build-manifest filenames move under that new ID with identical file bytes. Runtime JavaScript and all CSS are identical. |
| #539 | HTML: the recorded Next build ID and exact webpack runtime filename. Runtime initialized-chunk map key order changes, with the same numeric entries and zero values. The two manifest bodies are identical; all other JavaScript and CSS bytes are unchanged. |

The JSON delta contains 73 concrete changed/renamed file records, including unchanged manifest bytes under new paths. The current binary HTTP proof checks every JS/CSS route reference against the newly exported bytes. The complete tracked frontend tree, fixtures, locks, build configuration and capture harness are byte-identical at each original/current pair.

Original screenshots retain their original source, binary and inspection identity. Changed backend lifetime, consent and schema behavior is covered separately by source and runtime gates; frontend equivalence does not prove backend behavior.
