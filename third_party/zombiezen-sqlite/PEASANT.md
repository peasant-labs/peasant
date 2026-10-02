# SQLite connection lifetime patch

This directory is the complete Go module distribution of
`zombiezen.com/go/sqlite v1.4.2`, licensed under ISC. Its upstream origin is
`https://github.com/zombiezen/go-sqlite.git`, tag commit
`e5fb83745cf1640f27b86f6560ca32f50b2442e9`. `UPSTREAM.json` records the original
file hashes, modified Go files, and metadata renames; `connection-lifetime.patch` shows the
complete changes to upstream files. Go import literals are mechanically rewritten to the in-module package path.
The original go.mod, go.sum, go.work and go.work.sum bytes are retained as
UPSTREAM.*.txt, so they do not create an excluded nested module. All other
upstream bytes are unchanged.

This is an ordinary package in the Peasant module; no local replace or nested
module is used. A real Go module ZIP therefore carries this source and supports
the documented `go install ...@version` distribution. The original LICENSE travels
with the source and its text remains in the generated THIRD_PARTY_NOTICES.

## Failure and fix

Native `sqlite3_close` can free an address before Go removes the authorizer,
busy handler, and connection registrations for that address. A concurrent
`OpenConn` can reuse the address and install new registrations; the old Close
then removes the new connection's callbacks. Statement preparation can panic
in `authTrampoline`, and the busy callback can disappear.

Close now removes registrations only while it still owns the address in
`allConns.table`, under the same lock used by OpenConn registration. All three
registrations are removed together. A new connection registers before it can
install application callbacks; if it has not registered yet, it waits for this
lock. Callback removal still happens after native close, so callbacks remain
available during native close. Nil, duplicate, and native close error behavior
is unchanged. Authorization denial remains enforced; no nil-authorizer fallback
or permissive exception was introduced.

## Verification and test scope

`make sqlite-connection-test` runs the source audit and complete upstream Go
suite with the race detector. The normal main-module test plan also includes
these packages. `make module-install-check` uses the real Go module zipper and
runs `go install ...@version` through an isolated local file proxy. CI's CGO=0
job requires that installation check. No source directory build substitutes for
versioned installation.

The named fixture tests model the exact scheduling boundary after native close
and after a replacement OpenConn registers. They invoke the production cleanup
operation with the retired identity at a real live connection's address, then
prepare real SQL through its allow/deny authorizer and force real SQLite lock
contention through its busy handler. Successful actual Close must also release
its own registrations. This avoids allocator timing, retries, sleeps, and an
in-process panic preventing useful assertion diagnostics.

### Test promotion rationale

1. Subject: a retired connection cannot delete a newer owner's callbacks.
2. Necessity: ordinary open/close unit tests cannot select native address reuse
   and scheduling; an untouched-driver concurrent probe reproduced the panic.
3. Production path: the modeled boundary calls the same cleanup as Close;
   authorization and lock contention use actual SQLite operations.
4. Cost: six fixture cases, two local connections per contention case; no copied
   SQL engine, process, screenshot, or service.
5. Lifetime: test-owned temporary databases and cleanup/deferred connection Close.
6. Concurrency: each case owns its database; global maps use the driver's locks.
7. CI parity: the normal main-module plan includes the driver; the targeted Make
   command also runs from a clean checkout without history or local module links.
8. Evidence: actual authorization allow/deny and busy callbacks remain effective;
   actual Close removes registrations.
9. Mutation: removing the ownership check must fail preservation; removing all
   cleanup must fail the successful Close case.
10. Exit condition: remove the local fork once an audited upstream release includes
    equivalent ownership protection and passes these regression cases.

The separate publication-bundle SQLITE_BUSY report has not been attributed to
this callback race. This patch makes no claim to resolve that failure.

### Module installation test promotion rationale

1. Subject: a published Go module ZIP must include the audited driver and install
   the CLI with `go install ...@version`.
2. Necessity: direct package builds cannot detect nested-module exclusion or a
   local replace rejected by versioned installation; both blocked the first fix.
3. Production path: golang.org/x/mod/zip applies the real Go distribution rules;
   the real Go installer consumes that ZIP through a file proxy.
4. Cost: two named YAML obligations and two bounded installer processes, at most
   three minutes each. No build matrix, copied compiler, or fake archive.
5. Lifetime: one owned temporary directory contains source, proxy, module cache
   and binaries; its Go module cache is explicitly writable (`-modcacherw`),
   and deferred removal reports cleanup failure on success and error returns. Process
   timeouts kill the installer process group (including compiler children); SIGKILL may leave that OS temporary directory.
6. Concurrency: unique directories and module versions; the user's module cache
   is only read. Go's ordinary shared build cache handles its own locking.
7. CI parity: tracked source and explicit CGO=0, GOWORK=off, dependency checksums;
   no checkout history or prebuilt web dashboard. The real zipper omits symlinks.
8. Evidence: actual installation produces a nonempty CLI executable.
9. Mutation: a named ZIP omitting the driver must fail with its missing package,
   rather than succeed through a workspace or dependency fallback.
10. Exit condition: keep the focused installation gate while this distribution
    is supported; simplify only if versioned Go installation is retired.

## Historical harvester comparison

The harvester guard keeps one shared native-fixture builder across both exact
production snapshots and both original input corpora. For snapshots predating
this ordinary package, it supplies the candidate's non-test Go driver support
and LICENSE at the otherwise-absent package path. The builder returns database
paths; no Conn or Stmt crosses into either production reader. The older module's
existing modernc dependencies already satisfy this v1.4.2 support; its go.mod,
production imports, parser code and version registry are untouched.

An existing historical driver is never rewritten. Identical runtime support is
left intact; a different or additional runtime file fails closed with an explicit
compatibility error rather than silently changing the historical implementation.
Named filesystem fixtures cover absent support, existing preservation, refusal
and an original module-based builder. The real two-revision guard still compares
both corpora through the actual original production parsers and version checks.

## Scoped static analysis

Moving the driver into the main module exposes upstream's unchanged modernc
native-memory uintptr conversions to full `go vet`; dependencies were previously
outside that command's package targets. The unsafeptr analyzer cannot model those
native allocations as Go heap pointers. Every Peasant package and driver
subpackage still gets all analyzers. Only the native driver root package omits
unsafeptr; all its other analyzers run. The source audit additionally requires
its exact original unsafe.Pointer source lines (including multiplicity) from the
official v1.4.2 distribution and rejects any new conversions in added fork files.
No upstream pointer operation is rewritten or new operation exempted.
