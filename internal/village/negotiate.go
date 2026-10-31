package village

import (
	"context"
	"fmt"
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
		"peasant push aborted: this CLI's push-contract v%s is older than peasant online's minimum accepted v%s\n"+
			"  what: peasant online (our managed upstream) no longer accepts pushes from this CLI version\n"+
			"  why:  the accepted window starts at v%s and this CLI is below it\n"+
			"  fix:  upgrade the peasant CLI to >= v%s, then re-run `peasant push`",
		cli, min, min, min)
}

// DowngradeEmitWarning is the ONE-LINE, end-user-actionable notice printed when
// the CLI is AHEAD of the village's current contract (NegotiationAheadOfCurrent)
// and downgrade-emit succeeds. It never instructs upgrading the village.
func DowngradeEmitWarning(cli, current schema.PushContractVersion) string {
	return fmt.Sprintf(
		"notice: this CLI's push-contract v%s is ahead of peasant online's current v%s; "+
			"emitting v%s for compatibility (a peasant online update is rolling out — no action needed)\n",
		cli, current, current)
}

// CannotDowngradeError is the actionable error returned when the CLI is ahead of
// the village's current contract AND cannot serialize that older shape (a MAJOR
// version gap). It instructs PINNING the CLI — never modifying the village.
func CannotDowngradeError(cli, current schema.PushContractVersion) error {
	return fmt.Errorf(
		"peasant push aborted: this CLI's push-contract v%s cannot be downgraded to peasant online's current v%s\n"+
			"  what: the version gap is a breaking (major) change this CLI cannot serialize backward\n"+
			"  why:  emitting v%s would drop or reshape fields the older peasant online server requires\n"+
			"  fix:  pin the peasant CLI to a release whose push-contract is <= v%s until the peasant online update finishes",
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

// NegotiatePullWindow returns the pull envelope the village serves when this
// CLI reads it, from the window the village advertises: its floor (min), its
// default (current) and, when the envelope is requestable, its newest (max).
//
// A village that advertises max serves the envelope a pull read asks for in the
// schema.PullContractHeader request header: the highest it supports at or below
// the requested version and at or above its floor. This CLI asks for
// cliCurrent, so the served envelope is cliCurrent, or max when max is older.
// A patch bump within a minor is additive, so a served version in cliCurrent's
// minor is read as cliCurrent. The village refuses a request below its floor
// (406), so a floor above the served envelope is refused here first.
//
// A village that advertises no max predates the header and serves its current
// envelope to every caller: this CLI accepts it only when that version lies
// inside the CLI's window [cliMin, cliCurrent]. A newer current version is
// refused even when the village's window reaches back into the CLI's: on a 0.x
// contract a minor bump is not additive (pull contract 0.3.0 removed the
// account strings 0.2.0 carried), so this CLI cannot read an envelope newer
// than its own.
//
// ok is false when no envelope this CLI reads is served; the caller handles an
// unadvertised window (min and current both empty) before calling. The
// returned envelope is always a readable envelope's canonical version (0.2.0 or
// 0.3.0), never a patch of one.
func NegotiatePullWindow(cliMin, cliCurrent, min, current, max schema.PushContractVersion) (schema.PushContractVersion, bool) {
	served := current
	if max != "" {
		served = cliCurrent
		if compareSemver(max, cliCurrent) < 0 {
			served = max
		}
	}
	if served == "" || compareSemver(served, cliMin) < 0 {
		return "", false
	}
	if compareSemver(served, cliCurrent) > 0 && !sameMinor(served, cliCurrent) {
		return "", false
	}
	if min != "" && compareSemver(min, served) > 0 {
		return "", false
	}
	return readablePullEnvelope(served)
}

// sameMinor reports whether two versions share their major and minor, so they
// differ at most by an additive patch.
func sameMinor(a, b schema.PushContractVersion) bool {
	pa, pb := semverParts(a), semverParts(b)
	return pa[0] == pb[0] && pa[1] == pb[1]
}

// CanDowngrade reports whether this CLI can serialize the older `target`
// contract. A MAJOR-version gap is a breaking change and is NOT downgradable;
// same-major older targets are (additions are backward-droppable).
func CanDowngrade(cli, target schema.PushContractVersion) bool {
	return semverParts(cli)[0] == semverParts(target)[0]
}

// compareSemver returns -1, 0, or 1 for a<b, a==b, a>b over MAJOR.MINOR.PATCH.
// Package-internal: the public window helpers (ClassifyContract/CanDowngrade,
// consumed by internal/push) build on it; there is no external consumer.
// Each component is compared NUMERICALLY (so 0.10.0 > 0.9.0) without
// converting it to an integer, so a run of digits too long for an int (which a
// version header may carry) still compares by value instead of overflowing.
// Any pre-release or build suffix on the patch component is ignored.
func compareSemver(a, b schema.PushContractVersion) int {
	pa, pb := semverParts(a), semverParts(b)
	for i := 0; i < 3; i++ {
		if c := compareDigits(pa[i], pb[i]); c != 0 {
			return c
		}
	}
	return 0
}

// compareDigits orders two runs of ASCII digits with no leading zeros by
// value: the longer run is larger, and runs of equal length order as strings.
func compareDigits(a, b string) int {
	switch {
	case len(a) != len(b):
		if len(a) < len(b) {
			return -1
		}
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// semverParts splits "MAJOR.MINOR.PATCH" into its three leading digit runs,
// each with leading zeros removed so equal values compare equal. Missing
// components and components with no leading digit are "" (zero);
// non-numeric/suffix content is truncated at the first non-digit so
// "1.2.3-rc1" parses as {1,2,3}.
func semverParts(v schema.PushContractVersion) [3]string {
	var out [3]string
	parts := strings.SplitN(v.String(), ".", 3)
	for i := 0; i < len(parts) && i < 3; i++ {
		out[i] = leadingDigits(parts[i])
	}
	return out
}

// leadingDigits returns the leading run of ASCII digits in s without its
// leading zeros ("" for zero or no digits).
func leadingDigits(s string) string {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	return strings.TrimLeft(s[:end], "0")
}
