# Mounted responsive audit

Source: `f4f6c7095d1be5eb4693e0dbcc2713c91527693b`; Fairtrade: `0.0.57`.
Normal Go 1.25.8 production binary SHA256: `548b38d69d0be1b2bb5d46ed8d6625328d622ee287dd3a8d9ddfb2acd3d8f044`.

All 514 images were inspected in light/dark at desktop 1440×1000 and phone 390×844. The route/state inventory contains 486 captures: 104 routes, 174 conditional states, 148 publishing fixtures, 60 nested states. Sixteen focused captures cover actual Changes GET/error/retry/hunk, scoped permalink scrolling and Mermaid bounds. Twelve additional captures cover 9→10 publication counts and long-handle avatar alignment. JSON records separate browser fixture responses from actual local reads. No case failures, browser exceptions, or root horizontal overflow were observed.

`8691` serves a disposable synthetic stored transcript and real local reads. `8698` uses the application's existing mock stores for populated Changes/map data. Browser-only fixture responses cover conditional states without changing either store. The actual Share chooser → redact → publish navigation was exercised. No publication, login, settings, hooks, or annotation writes were submitted. Read-only Changes has no orphan annotation controls; existing turn label/custom-label controls remain available.

Actual Changes disclosure issued GET, displayed an injected 503, retried through the canonical control, then loaded a real backend 200 hunk. Long errors fit 390px in both themes. Permalink targets 1/222 leave inert ancestors at zero scroll and the reader/footer gap at zero; Mermaid renders after View Diagram and labels fit. Numeric glyph widths are equal; icon/label and avatar/handle centers align; the 9→10 change preserves action/bar height. Normal turn role icons and active borders remain while phase rows have no direct generic turn rule.

Validation: all 1,669 frontend tests, TypeScript, 14 contract mutations, 11 transcript-position mutations, frozen install, and normal production build pass. Five YAML-backed mounted tests cover diff loading/cache/retry/stale completion/unmount-before-read. No schema, API, database, or migration changes.

The screenshot manifest records every file's SHA256. Provenance records source/tree, package, binary, and six byte-identical served-asset checks. These are review artifacts, kept separate from the source branch. Website hero and Village profile changes are owned by their respective repositories and are not certified here.
