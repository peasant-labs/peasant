package village

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/peasant-labs/schema"
)

// --- Version-negotiation helpers ---
//
// Compatibility is SERVER-LED: the village is OUR managed upstream and end-users
// CANNOT upgrade it. Before uploading (push) or pulling, the CLI preflights GET
// /api/v1/schema/version and compares its own contract version against the
// village-advertised accept window [Min, Current]. All messages are end-user
// actionable and NEVER instruct "upgrade the village".
//
// These helpers live here so the pull pipeline can reuse the same semver/window
// logic without a pull → push import inversion. The push pipeline's negotiate()
// method and the pull pipeline both call them from their respective packages.
//
// The push-acceptance floor (MinPushContractVersion) is distinct from the
// village's display migrate-on-read floor — a village may still render stored
// blobs older than the version it will accept on a fresh push.

// SchemaVersionQuerier is the subset of the village client both the push
// preflight and the pull preflight need. The production *VillageClient satisfies
// it; tests inject a double. Push composes it into its constructor-required
// transport, while pull consumes this narrow contract directly.
type SchemaVersionQuerier interface {
	GetSchemaVersion(ctx context.Context) (*schema.SchemaVersionResponse, int, error)
}

// ContractNegotiation is the typed outcome of comparing the CLI's contract
// version against the village-advertised [Min, Current] accept window.
type ContractNegotiation int

const (
	// NegotiationWithin: CLI ∈ [Min, Current] — emit at the CLI version.
	NegotiationWithin ContractNegotiation = iota
	// NegotiationOlderThanMin: CLI < Min — hard abort; the CLI must be upgraded.
	NegotiationOlderThanMin
	// NegotiationAheadOfCurrent: CLI > Current — downgrade-emit to Current and
	// warn (hard abort only if the older shape is not serializable).
	NegotiationAheadOfCurrent
	// NegotiationUnadvertised: the village advertised no window (an older,
	// pre-contract village) — proceed at the CLI version (back-compat / fail-open).
	NegotiationUnadvertised
)

// String renders the outcome for logs/diagnostics.
func (c ContractNegotiation) String() string {
	switch c {
	case NegotiationWithin:
		return "within"
	case NegotiationOlderThanMin:
		return "older-than-min"
	case NegotiationAheadOfCurrent:
		return "ahead-of-current"
	case NegotiationUnadvertised:
		return "unadvertised"
	default:
		return "unknown"
	}
}

// UpgradeCLIError is the actionable error returned when the CLI is OLDER than the
// village's minimum accepted contract (NegotiationOlderThanMin). It instructs
// upgrading the CLI — never the village.
func UpgradeCLIError(cli, min schema.PushContractVersion) error {
	return fmt.Errorf(
		"peasant push aborted: this CLI's push-contract v%s is older than the village's minimum accepted v%s\n"+
			"  what: the village (our managed upstream) no longer accepts pushes from this CLI version\n"+
			"  why:  the accepted window starts at v%s and this CLI is below it\n"+
			"  fix:  upgrade the peasant CLI to >= v%s, then re-run `peasant push`",
		cli, min, min, min)
}

// DowngradeEmitWarning is the ONE-LINE, end-user-actionable notice printed when
// the CLI is AHEAD of the village's current contract (NegotiationAheadOfCurrent)
// and downgrade-emit succeeds. It never instructs upgrading the village.
func DowngradeEmitWarning(cli, current schema.PushContractVersion) string {
	return fmt.Sprintf(
		"notice: this CLI's push-contract v%s is ahead of the village's current v%s; "+
			"emitting v%s for compatibility (a village update is rolling out — no action needed)\n",
		cli, current, current)
}

// CannotDowngradeError is the actionable error returned when the CLI is ahead of
// the village's current contract AND cannot serialize that older shape (a MAJOR
// version gap). It instructs PINNING the CLI — never modifying the village.
func CannotDowngradeError(cli, current schema.PushContractVersion) error {
	return fmt.Errorf(
		"peasant push aborted: this CLI's push-contract v%s cannot be downgraded to the village's current v%s\n"+
			"  what: the version gap is a breaking (major) change this CLI cannot serialize backward\n"+
			"  why:  emitting v%s would drop or reshape fields the older village requires\n"+
			"  fix:  pin the peasant CLI to a release whose push-contract is <= v%s until the village update finishes",
		cli, current, current, current)
}

// ClassifyContract compares the CLI contract version against the village's
// advertised [min, current] accept window and returns the typed outcome. An
// empty min AND current means the village advertised no window (pre-contract
// village) — NegotiationUnadvertised, proceed at the CLI version.
func ClassifyContract(cli, min, current schema.PushContractVersion) ContractNegotiation {
	if min == "" && current == "" {
		return NegotiationUnadvertised
	}
	if current != "" && compareSemver(cli, current) > 0 {
		return NegotiationAheadOfCurrent
	}
	if min != "" && compareSemver(cli, min) < 0 {
		return NegotiationOlderThanMin
	}
	return NegotiationWithin
}

// CanDowngrade reports whether this CLI can serialize the older `target`
// contract. A MAJOR-version gap is a breaking change and is NOT downgradable;
// same-major older targets are (additions are backward-droppable).
func CanDowngrade(cli, target schema.PushContractVersion) bool {
	return parseSemver(cli)[0] == parseSemver(target)[0]
}

// compareSemver returns -1, 0, or 1 for a<b, a==b, a>b over MAJOR.MINOR.PATCH.
// Package-internal: the public window helpers (ClassifyContract/CanDowngrade,
// consumed by internal/push) build on it; there is no external consumer.
// Each component is compared NUMERICALLY (so 0.10.0 > 0.9.0). Any pre-release or
// build suffix on the patch component is ignored.
func compareSemver(a, b schema.PushContractVersion) int {
	pa, pb := parseSemver(a), parseSemver(b)
	for i := 0; i < 3; i++ {
		switch {
		case pa[i] < pb[i]:
			return -1
		case pa[i] > pb[i]:
			return 1
		}
	}
	return 0
}

// parseSemver splits "MAJOR.MINOR.PATCH" into a numeric triple. Missing
// components default to 0; non-numeric/suffix content is truncated at the first
// non-digit so "1.2.3-rc1" parses as {1,2,3}.
func parseSemver(v schema.PushContractVersion) [3]int {
	var out [3]int
	parts := strings.SplitN(v.String(), ".", 3)
	for i := 0; i < len(parts) && i < 3; i++ {
		out[i] = leadingInt(parts[i])
	}
	return out
}

// leadingInt parses the leading run of ASCII digits in s (0 if none).
func leadingInt(s string) int {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, _ := strconv.Atoi(s[:end])
	return n
}
