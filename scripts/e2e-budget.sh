#!/usr/bin/env bash
# Bound the runner and ordinary descendants; Podman services have separate limits.
set -euo pipefail

fail() {
    echo "e2e budget: $*; no test was started. Fix the budget/user systemd session, or use E2E_MEMORY_MODE=external only under an outer runner/container hard limit (docs/e2e.md)." >&2
    exit 1
}

export PEASANT_INGEST_ARENA_BYTES=${PEASANT_INGEST_ARENA_BYTES:-67108864}
export GOMEMLIMIT=${GOMEMLIMIT:-2GiB}
export GOMAXPROCS=${GOMAXPROCS:-2}
# GOFLAGS is inherited by the harness's Peasant and Village builds too.
if [[ ${1:-} != --inside ]]; then
    export GOFLAGS="${GOFLAGS:-} -p=${E2E_BUILD_PARALLELISM:-2}"
fi
export E2E_MEMORY_MAX=${E2E_MEMORY_MAX:-8589934592}
export E2E_MEMORY_HIGH=${E2E_MEMORY_HIGH:-6442450944}
for value in "$E2E_MEMORY_MAX" "$E2E_MEMORY_HIGH"; do
    [[ $value =~ ^[1-9][0-9]*$ ]] || fail "MemoryMax/MemoryHigh must be positive byte counts"
done
(( E2E_MEMORY_HIGH <= E2E_MEMORY_MAX )) || fail "MemoryHigh exceeds MemoryMax"

if [[ ${1:-} == --inside ]]; then
    shift
    group=$(sed -n 's/^0:://p' /proc/self/cgroup)
    cg="/sys/fs/cgroup$group"
    [[ -n $group && -r $cg/memory.max ]] || fail "cannot read cgroup v2 memory controller inside scope"
    [[ $(cat "$cg/memory.max") == "$E2E_MEMORY_MAX" &&
       $(cat "$cg/memory.high") == "$E2E_MEMORY_HIGH" &&
       $(cat "$cg/memory.swap.max") == 0 ]] || fail "scope memory limit read-back mismatch"
    echo "e2e budget verified: MemoryMax=$(cat "$cg/memory.max") MemoryHigh=$(cat "$cg/memory.high") MemorySwapMax=$(cat "$cg/memory.swap.max")"
    trap 'echo "e2e cgroup memory.peak=$(cat "$cg/memory.peak") bytes"; cat "$cg/memory.events"' EXIT
    "$@"
    exit $?
fi

(( $# > 0 )) || fail "missing command"
case ${E2E_MEMORY_MODE:-systemd} in
    systemd)
        [[ $(uname -s) == Linux ]] || fail "local hard budget requires Linux cgroup v2"
        command -v systemd-run >/dev/null || fail "systemd-run is unavailable"
        command -v flock >/dev/null || fail "flock is unavailable"
        [[ -d ${XDG_RUNTIME_DIR:-} ]] || fail "XDG_RUNTIME_DIR is unavailable for the same-user runner lock"
        exec 9>"$XDG_RUNTIME_DIR/peasant-e2e-budget.lock"
        flock -n 9 || fail "another bounded E2E invocation is running for this user"
        systemctl --user show-environment >/dev/null || fail "cannot contact systemd user manager"
        # A scope retains the caller's environment (including Nix PATH) and cwd.
        systemd-run --user --scope --quiet --unit="peasant-e2e-$$" \
            -p "MemoryMax=$E2E_MEMORY_MAX" -p "MemoryHigh=$E2E_MEMORY_HIGH" \
            -p MemorySwapMax=0 -- bash "$0" --inside "$@"
        ;;
    external)
        echo "e2e budget: external hard limit required; GOMEMLIMIT=$GOMEMLIMIT is only a Go soft target" >&2
        exec "$@"
        ;;
    *) fail "unknown E2E_MEMORY_MODE=${E2E_MEMORY_MODE}" ;;
esac
