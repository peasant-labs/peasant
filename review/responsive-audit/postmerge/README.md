# Post-merge verification

PR796 merged as `710ad57192a5a9ba9d65a29c7b18013ead4748d2`. The clean local develop worktree was fast-forwarded from canonical. Its tree `f91d7a523e669cfdce0366ba825d9a98e68f01f1` exactly equals reviewed head `f4f6c7095d1be5eb4693e0dbcc2713c91527693b`.

A fresh external normal `GOTOOLCHAIN=go1.25.8 make build` passed, including frozen dependency installation and provider provenance guard. Binary SHA256: `370e5c46a5fb2387499cda5fa268c2b7fb8477cff942376d3c00c32a9f99ba7f`; version `peasant v0.13.6-dev.4+710ad5719`. After checking the old daemon PIDs, cwd and arguments, both disposable previews were replaced with detached processes using this exact merged binary, preserving original data/config/state. Six marked CSS/JS assets match each live server byte-for-byte (12 checks). All three marked layout CSS assets also match the reviewed build; numeric/avatar measurements therefore need no repeat.

All20 focused post-merge screenshots were inspected at1440/390 in both themes. Actual Changes disclosure → injected503 → canonical retry → real200hunk passes; long errors stay within their boundary, and no annotation editing is offered without host persistence callbacks. Real transcript custom-label forms still open; no save/write was submitted. Prose/diagram permalinks leave inert ancestors at zero scroll and reader/footer gap at zero. Mermaid text is at least16px and inside SVG bounds; keyboard panning works on phone. Normal role/active borders are retained. No new finding. The complete514-image pre-merge audit remains valid because the source tree is identical.

No source, schema, API, migration, package publication or installer changes were made after merge. This evidence supplements the original inspected audit and does not replace it.
