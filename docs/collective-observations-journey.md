# Collective observations built-binary journey

This focused journey exercises Peasant's real exported `/village` page, persistent
header, Local API, credential files, SQLite Store and CLI. Only the remote Village
dependency is a fixture. It does not intercept browser/local API traffic, use
`next dev`, substitute an unpublished design-system package, or replace the
existing Puppeteer probes.

## Prerequisites and commands

Use Linux with `/proc` and the repository Nix development shell. The shell supplies
Go, Node, pnpm, util-linux `flock`, `tar` and Chromium. Chromium is explicitly
selected; neither the runner nor CI downloads a Playwright browser at runtime.
The exact package versions and integrity are read from the manifest/lockfile.

```sh
nix develop
export CHROME_PATH="$(command -v chromium)"
export PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
pnpm --dir web install --frozen-lockfile
pnpm --dir web run test:journey:support
pnpm --dir web run test:journey:fixture
pnpm --dir web run test:journey:api
pnpm --dir web run journey:collective-observations
pnpm --dir web run test:journey:mutations
```

The journey runs the whole mounted catalog by default. A local investigation may
narrow it with `--grep`, the one argument the runner forwards to Playwright:

```sh
pnpm --dir web run journey:collective-observations --grep '<name>|<name>'
```

The build manifest records `requestedSelection` and `fullCatalogRequested` before
the suite runs, and adds `suiteResult` with `catalogComplete` only when it
finishes. A filtered or counterfactual run never claims a completed catalog.

The first test command needs no Next build and tests real disposable-child
lifetimes plus provenance refusal boundaries. The fixture command builds the small
Go seeder and validates remote payloads against the installed canonical schema.
It uses published module/package pins, not a source-prototype workspace.

The API command separately compiles the real CLI and seeder, starts that absolute
foreground binary with the normal production environment setting
`PEASANT_COLLECTIVE_FETCH_TIMEOUT=2s`, and exercises real HTTP/Store domain effects.
The remote owner-history fixture genuinely waits until the client cancels its
response. That local deadline must yield local 504 with completed catalog and
retained history. A distinct remote HTTP 504 must yield local 502. Invalid runtime
configuration must refuse startup before remote I/O or listener readiness.
The command asserts exact declared remote request paths/counts and complete
schema-validated local reports. It requires no browser and makes **no UI claim**:
it does not use `make build`, certify embedded web assets, or capture `/village`.
Its explicitly named API-only reports record source/binary provenance and whether
an external source-prototype Go workspace was selected. Normal published-schema
resolution is used unless an external development configuration explicitly says
otherwise; a source-prototype API run is never a published consumer proof.

The mounted command requires a committed, clean consumer tree and published
schema/Fairtrade packages that actually export the observation contract,
`VillageObservations`, and canonical village navigation. Missing exports,
integrity mismatch, a missing browser, a stub export, a failed build or an
unavailable service is an error, never a skipped test. An unreleased preparation
cannot pass these prerequisites. The dedicated workflow runs the same commands;
it is not expected to be green before the dependency releases and coherent
consumer adoption.

The bounded mutation command uses the SAME built binary, real local API/Store,
fixtures, user actions and runner. It selects one desktop project and three named
counterfactuals from YAML: discard a typed partial result, break the actual nav
route, and remove the page body. It changes only the app's compiled JS response
inside that negative-test browser context, never Local API/remote response data
or published DS code. Each original/mutated asset hash and intended observed
failure is recorded; missing anchors, unexpected errors and surviving mutants
fail. Its manifest says `counterfactual: true`; these are rejection evidence,
not trusted positive screenshots or a second unfiltered catalog.

## Artifact and production-path checks

The runner verifies the installed first-party package bytes against the real npm
tarballs, including registry/frozen-lock SHA-512 integrity. It rejects source
links and candidate packages rather than treating them as published proof.
It invokes `make build` once, then starts that absolute embedded binary with
`web start --foreground --no-browser` in an isolated XDG environment.
The mounted runner uses the same normal `PEASANT_COLLECTIVE_FETCH_TIMEOUT=2s`
process setting, so its real deadline scenario fits the unchanged 60-second test
budget. The setting changes no Local API request field and inserts no fake response.

Before Playwright, it records source commit/file hashes, lockfile/package
integrity, exported web assets and binary SHA-256. It compares actual served
`/village` HTML and referenced JS/CSS to the built export, requires the route/body
markers and verifies `/proc/<pid>/exe` against that binary. Restarts recheck the
same provenance. The mounted cases assert actual navigation/active state and body,
typed partial effects/dates, independent paging/conflicts, local outage versus
remote failure, settings exits, real logout/account reseeding, same-Store CLI
parity, optional pull-metadata warnings and computed styles/axe/keyboard access.

Each theme runs desktop and mobile. Screenshots, computed styles, build manifest,
machine results and failure traces are retained under the gitignored
`review-capture/collective-observations-<random>/` directory. Credentials and the
disposable Store do not live there. Inspect every screenshot before citing it.
File generation is not acceptance, and PR evidence still needs durable hosting.

## Lifetime and cost

- One worker, no retries, 60 seconds per test and 300 seconds per suite.
- The combined production/seeder build has a 600-second ceiling. App startup is
  bounded at 30 seconds. Commands and fixture exports have separate bounded
  output and execution limits.
- A persistent inode `flock` serializes the entire build/recovery/cleanup lifetime
  in this worktree. The gate file is never unlinked. An immutable random owner
  identity is atomically published before any workload child can spawn.
  Its private state is nested under that ignored ownership directory, not a
  changing Nix-shell TMPDIR; a later shell can therefore verify stale ownership.
- Every child has exactly one settlement waiter. Workload groups carry an
  inherited random ownership marker; cleanup signals only proven owned groups,
  escalates TERM to KILL, waits for absence and reaps direct children before
  removing private state and releasing ownership. A failed reap retains metadata
  and the keeper rather than claiming successful teardown.
- SIGKILL cannot execute `finally`. On the next acquisition, flock serializes
  stale-owner verification and marker-proven cleanup. Only the exact random state
  root in the validated immutable identity is removed. A losing contender cannot
  remove a live winner's ownership record. No fixed shared DB, live XDG directory,
  broad process kill or SQL bypass is used.

## Why this test level exists

Subject: the shipped navigation/body, partial HTTP-result rendering, category
traversal state and exact served artifact. Direct adapter/Store tests cannot
observe a dead header route, a missing exported body, wrong theme/computed fonts,
or a stale embedded binary. They remain the cheaper coverage for reconciliation,
transactions and source/account races; the browser does not duplicate them.

The fixture seeder uses production auth and Store methods and canonical payload
validators. Synthetic remote history includes other-device attempts. Seeding and
new diagnostic/receipt/terminal writes never use raw SQL. Account-switch tests
retain the same real Store; ordinary per-case fixture reset removes only owned
disposable state after the app has stopped.

Support tests mutate refusal conditions by serving a stale HTML/asset or removing
required package/body exports; real child tests cover forced termination,
failed spawn and concurrent stale recovery. Mounted body-removal, dead-route and
typed-partial-discard mutations must be run against the published coherent build
before claiming final production evidence. A long real service deadline must not
be replaced by a forged local response merely to meet the test budget.

This narrow harness can be simplified when another built-binary runner covers
these same production risks and ownership guarantees. It is not a new full UI
catalog or a general-purpose visual framework.
