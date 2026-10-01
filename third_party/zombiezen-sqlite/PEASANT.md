# SQLite connection lifetime patch

This directory is the complete Go module distribution of
`zombiezen.com/go/sqlite v1.4.2`, licensed under ISC. Its upstream origin is
`https://github.com/zombiezen/go-sqlite.git`, tag commit
`e5fb83745cf1640f27b86f6560ca32f50b2442e9`. `UPSTREAM.json` records the original
file hashes and the three modified files; `connection-lifetime.patch` shows the
complete changes to upstream files. All other upstream bytes are unchanged.

The main module uses a local `replace`; releases therefore ship this audited
source, without depending on an external fork. The original LICENSE travels
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

The upstream module's workspace references a CLI module excluded from the Go
module distribution. Tests explicitly use `GOWORK=off`. The only dependency
addition in this nested module is the YAML loader used by the regression test.

## Verification and test scope

`make sqlite-connection-test` runs the fork audit and the complete upstream Go
test suite with the race detector. `make check` requires that target, because
the main module's `go test ./...` does not traverse nested modules.

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
7. CI parity: the normal Make target runs the nested module explicitly, without
   checkout history, local module links, or undocumented environment variables.
8. Evidence: actual authorization allow/deny and busy callbacks remain effective;
   actual Close removes registrations.
9. Mutation: removing the ownership check must fail preservation; removing all
   cleanup must fail the successful Close case.
10. Exit condition: remove the local fork once an audited upstream release includes
    equivalent ownership protection and passes these regression cases.

The separate publication-bundle SQLITE_BUSY report has not been attributed to
this callback race. This patch makes no claim to resolve that failure.
