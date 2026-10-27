# Stored-library browsing review

Source commit: `713b6ee3236758fc842659c27dbe382506e9f7a9` (Peasant #759).
Published dependencies: Go/npm schema 0.35.0, Fairtrade 0.0.46.

The real built binary served the mounted application. The root Playwright journey
and the filtered transcript publication runner passed their served-build provenance,
computed-style, theme, navigation and state assertions. Each image was personally
opened and inspected before publication. Only synthetic fixture data is shown.

- `root/{dark,light}/{desktop,phone}/{home-projects,map-projects,review-projects}.png`
  contains the full application chrome and both recorded projects, without obsolete
  hidden-by-selection notices.
- `publish/{dark,light}/{desktop,mobile}/unpublished-outside-ingestion-selection.png`
  shows an ordinary not-published transcript with an explicit publish action.

Captures ran with a temporary HOME and all XDG roots, GOWORK=off, and dedicated
sandbox ports. No live store or user server was used. Browser fixtures establish
presentation and interaction; real-store HTTP tests establish browsing eligibility.

Keep this screenshot branch available while the PR and its embedded images exist.
