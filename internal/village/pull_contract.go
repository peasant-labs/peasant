package village

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// --- Requesting the pull envelope ---
//
// Every pull read (list, metadata, annotations, content) names the newest pull
// contract this CLI reads in the schema.PullContractHeader request header.
// Peasant online serves the highest envelope it supports at or below that and
// reports the version it served in the same response header, so each response
// is decoded by what it says it is. A village that predates the header ignores
// it, sends none back, and serves the envelope NegotiatePull settled from its
// advertised window.

// wellFormedPullContract matches the MAJOR.MINOR.PATCH digits the header
// carries. Anything else is read as an absent header, as the village does.
var wellFormedPullContract = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// readablePullEnvelopes are the pull envelopes this CLI decodes, newest first.
// A patch bump within a minor is additive, so any 0.3.z reads as 0.3.0 and any
// 0.2.z as 0.2.0.
var readablePullEnvelopes = []schema.PushContractVersion{
	schema.PublicOwnerPullContractVersion,
	schema.LegacyPullContractVersion,
}

// readablePullEnvelope maps a pull contract version onto the envelope this CLI
// decodes it as: the readable envelope with the same major and minor. ok is
// false for a version in no readable minor.
func readablePullEnvelope(version schema.PushContractVersion) (schema.PushContractVersion, bool) {
	for _, envelope := range readablePullEnvelopes {
		if sameMinor(version, envelope) {
			return envelope, true
		}
	}
	return "", false
}

// newPullGet builds an authed pull read that requests the newest pull envelope
// this CLI reads.
func (c *VillageClient) newPullGet(ctx context.Context, url string) (*http.Request, error) {
	req, err := c.newAuthedGet(ctx, url)
	if err != nil {
		return nil, err
	}
	req.Header.Set(schema.PullContractHeader, defaults.PullContractVersion.String())
	return req, nil
}

// servedPullEnvelope reads the envelope a pull response says it carries. A
// missing or malformed header means a village that predates the requestable
// envelope: it served what NegotiatePull settled, or pull contract 0.2.0 when
// nothing was negotiated. A served version in no minor this CLI reads is an
// incompatible-contract error.
func (c *VillageClient) servedPullEnvelope(resp *http.Response, path string) (schema.PushContractVersion, error) {
	raw := strings.TrimSpace(resp.Header.Get(schema.PullContractHeader))
	if !wellFormedPullContract.MatchString(raw) {
		if negotiated := c.negotiatedPullEnvelope(); negotiated != "" {
			return negotiated, nil
		}
		return schema.LegacyPullContractVersion, nil
	}
	envelope, ok := readablePullEnvelope(schema.PushContractVersion(raw))
	if !ok {
		return "", fmt.Errorf(
			"peasant pull aborted: peasant online served pull contract %s, which this CLI does not read\n"+
				"  what: the response from %s carries %s: %s\n"+
				"  why:  this CLI reads pull contracts %s and %s (any patch of either), and asked for %s\n"+
				"  where: pull response envelope\n"+
				"  fix:  upgrade the peasant CLI to a release that reads pull contract %s, then re-run the pull\n"+
				"  (%w)",
			raw, path, schema.PullContractHeader, raw,
			defaults.MinPullContractVersion, defaults.PullContractVersion, defaults.PullContractVersion,
			raw, ErrPullContractIncompatible)
	}
	return envelope, nil
}

// pullContractRefusedError is the actionable error for a 406: peasant online
// no longer serves the pull contract this CLI asked for.
func pullContractRefusedError(op, path string, body []byte) error {
	reason := strings.TrimSpace(string(body))
	var payload schema.VillageErrorResponse
	if err := json.Unmarshal(body, &payload); err == nil && strings.TrimSpace(payload.Error) != "" {
		reason = strings.TrimSpace(payload.Error)
	}
	if reason == "" {
		reason = "peasant online sent no reason"
	}
	return fmt.Errorf(
		"%s: peasant online returned 406 Not Acceptable\n"+
			"  what: peasant online no longer serves pull contract %s, the newest this CLI reads (%s header on %s)\n"+
			"  why:  %s\n"+
			"  where: pull request envelope\n"+
			"  fix:  upgrade the peasant CLI to the latest release, then re-run the pull\n"+
			"  (%w)",
		op, defaults.PullContractVersion, schema.PullContractHeader, path, reason, ErrPullContractIncompatible)
}
