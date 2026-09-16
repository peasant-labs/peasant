package push

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/perf"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// --- Version negotiation ---
//
// Compatibility is SERVER-LED: the village is OUR managed upstream and end-users
// CANNOT upgrade it. Before uploading, the CLI preflights GET
// /api/v1/schema/version and compares its own push-contract version
// (defaults.PublishSchemaVersion) against the village-advertised accept window
// [MinPushContractVersion, PushContractVersion]. All messages are end-user
// actionable and NEVER instruct "upgrade the village".
//
// The semver/window helpers and SchemaVersionQuerier interface live in
// internal/village so the pull pipeline reuses the same logic without a pull →
// push import inversion. This file keeps the push-pipeline negotiate() method
// that orchestrates them against the live transport.

// Transport is the complete Village transport required by NewPipeline.
// Publication stays a separate narrow interface, while composing the version
// query here makes negotiation constructor-enforced rather than dependent on a
// runtime type assertion.
type Transport interface {
	Publisher
	village.SchemaVersionQuerier
}

// negotiate preflights the village schema-version endpoint and returns the
// push-contract version the pipeline should EMIT plus the receiver's freshly
// advertised capability support, or an actionable error that aborts the push.
//
// The advertisement is fetched ONCE per real upload run, immediately before the
// upload phase, and is never cached: a scan performed earlier is a local
// requirement statement, not a receiver answer, so support changed since the
// scan is detected here.
//
// Fail-open applies to the CONTRACT VERSION only: if the preflight is
// unavailable (transport error / nil body), the push proceeds at the CLI
// version — the village's server-side validation still rejects bad
// harness/model. Capability support is NOT failed open: the returned support
// carries Known=false, so any payload that requires an optional capability is
// refused before upload rather than assumed compatible.
func (p *Pipeline) negotiate(ctx context.Context) (emit schema.PushContractVersion, support RemoteCapabilitySupport, negotiateErr error) {
	rec := perf.RecorderFromContext(ctx)
	span := rec.StartChildSpan(perf.StagePushNegotiate, perf.ParentSpanFromContext(ctx), nil)
	ctx = perf.ContextWithParentSpan(ctx, span.ID())
	defer func() {
		outcome := perf.OutcomeOK
		if negotiateErr != nil {
			outcome = perf.OutcomeFailed
			rec.Error(perf.StagePushNegotiate, fmt.Errorf("contract negotiation failed; check CLI compatibility before retrying"), nil)
		}
		span.End(outcome, nil)
	}()
	cli := defaults.PublishSchemaVersion

	resp, _, err := p.transport.GetSchemaVersion(ctx)
	if err != nil {
		span.End(perf.OutcomeSkipped, nil) // Existing fail-open fallback, not a successful handshake.
		// Fail-open, so this is a note about a degraded preflight and not a
		// failure. --quiet promises errors and one final result line, and a hook
		// runs with it: an unreachable village would otherwise print this into
		// every commit on top of the warning the hook already emits.
		if !p.runCfg.Quiet {
			fmt.Fprintf(p.stderr,
				"notice: schema-version preflight unavailable (%v); emitting CLI contract v%s\n", err, cli)
		}
		return cli, RemoteCapabilitySupport{Known: false, Unavailable: err}, nil
	}
	if resp == nil {
		span.End(perf.OutcomeSkipped, nil)
		return cli, RemoteCapabilitySupport{Known: false}, nil
	}

	min := resp.MinPushContractVersion
	current := resp.PushContractVersion
	support = RemoteCapabilitySupport{Known: true, Capabilities: resp.ContentCapabilities}

	switch village.ClassifyContract(cli, min, current) {
	case village.NegotiationOlderThanMin:
		return "", RemoteCapabilitySupport{}, village.UpgradeCLIError(cli, min)
	case village.NegotiationAheadOfCurrent:
		if !village.CanDowngrade(cli, current) {
			return "", RemoteCapabilitySupport{}, village.CannotDowngradeError(cli, current)
		}
		fmt.Fprint(p.stderr, village.DowngradeEmitWarning(cli, current))
		return current, support, nil
	default: // within or unadvertised
		return cli, support, nil
	}
}
