package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// priorAliasActivation installs one complete generation with the supplied
// aliases as the read authority. It is the real activation route, so the alias
// rows the prior reader merges are exactly the ones production writes.
func priorAliasActivation(t *testing.T, s *Store, sid schema.SessionID, genID string, aliases []indexformat.NativeAlias) {
	t.Helper()
	v2, blobs := buildTestGeneration(t, sid, genID, "authority text", "authority input", "authority output")
	v2.Generation.Aliases = aliases
	outcome, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation: v2, Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 1,
		ContentCapture: recoveryCapture(t, v2),
	})
	if err != nil {
		t.Fatalf("activate authority %s: %v", genID, err)
	}
	if outcome.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("authority %s disposition = %v, want ActivationCommittedNow", genID, outcome.Disposition)
	}
}

// priorAliasAttempt records one complete preview attempt with the supplied
// aliases as the latest attempt over the session's current certificate. The
// stored pair identity is recorded first so the eligibility clause that
// requires a verified pair identity holds.
func priorAliasAttempt(t *testing.T, s *Store, sid schema.SessionID, genID, identity string, aliases []indexformat.NativeAlias) {
	t.Helper()
	setStoredArtifactHash(t, s, sid, identity)
	v2, blobs := buildTestGeneration(t, sid, genID, "attempt text", "attempt input", "attempt output")
	v2.Generation.Aliases = aliases
	adapter := 2
	v2.Generation.Metadata.AdapterVersion = &adapter
	outcome, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation: v2, Blobs: blobs, IndexerVersion: 18, IndexedAtMs: 2,
		ContentCapture: sourceOmittedPreviewWrite(),
	})
	if err != nil {
		t.Fatalf("activate latest attempt %s: %v", genID, err)
	}
	if outcome.Disposition != ingest.ActivationLatestAttemptPreview {
		t.Fatalf("latest attempt %s disposition = %v, want ActivationLatestAttemptPreview", genID, outcome.Disposition)
	}
}

// TestLatestAttemptPriorMergesAttemptAliasesUnderAuthority proves the prior
// merge rule on the real prior reader: a key the attempt introduced is added
// under the authority's identity, a key the two share keeps the ref they agree
// on, the evidence authority stays the authority's alone, and a key the attempt
// introduced survives into a later complete generation with its ref unchanged.
func TestLatestAttemptPriorMergesAttemptAliasesUnderAuthority(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("51515151-6161-4171-8272-737374747575")
	seedGenerationSession(t, s, string(sid))
	ctx := context.Background()

	priorAliasActivation(t, s, sid, "gen_prior_authority", []indexformat.NativeAlias{
		{NativeKey: "block:shared", Ref: schema.SourceEntryRef("e_u1")},
	})
	priorAliasAttempt(t, s, sid, "gen_prior_attempt", hexIdentity('5'), []indexformat.NativeAlias{
		{NativeKey: "block:shared", Ref: schema.SourceEntryRef("e_u1")},
		{NativeKey: "block:introduced", Ref: schema.SourceEntryRef("e_result1")},
	})

	prior, err := s.ReadGenerationPrior(ctx, sid)
	if err != nil {
		t.Fatalf("ReadGenerationPrior: %v", err)
	}
	if prior == nil {
		t.Fatal("ReadGenerationPrior returned no prior with an active authority and a latest attempt")
	}
	if prior.GenerationID != "gen_prior_authority" {
		t.Fatalf("prior generation = %q, want the active authority", prior.GenerationID)
	}
	if !prior.HasCompleteGeneration || prior.Metadata == nil {
		t.Fatalf("prior evidence authority = (complete=%t, metadata=%t), want the authority's own", prior.HasCompleteGeneration, prior.Metadata != nil)
	}
	if got := prior.Aliases.Entries["shared"]; got != "e_u1" {
		t.Fatalf("shared alias ref = %q, want the agreeing ref e_u1", got)
	}
	if got := prior.Aliases.Entries["introduced"]; got != "e_result1" {
		t.Fatalf("introduced alias ref = %q, want the attempt's own e_result1", got)
	}

	// The candidate built from this prior persists the introduced key on the
	// next complete generation, so the ref stays source-bound across it.
	priorAliasActivation(t, s, sid, "gen_prior_complete", []indexformat.NativeAlias{
		{NativeKey: "block:shared", Ref: schema.SourceEntryRef("e_u1")},
		{NativeKey: "block:introduced", Ref: schema.SourceEntryRef("e_result1")},
	})
	after, err := s.ReadGenerationPrior(ctx, sid)
	if err != nil {
		t.Fatalf("ReadGenerationPrior after completion: %v", err)
	}
	if after == nil || after.GenerationID != "gen_prior_complete" {
		t.Fatalf("prior generation after completion = %+v, want the complete generation", after)
	}
	if got := after.Aliases.Entries["introduced"]; got != "e_result1" {
		t.Fatalf("introduced alias ref after completion = %q, want the stable e_result1", got)
	}
}

// TestLatestAttemptPriorConflictRefusesActionableError proves a shared key the
// two generations map to different refs refuses instead of silently rebinding
// either generation's content, and the refusal names the key and both refs.
func TestLatestAttemptPriorConflictRefusesActionableError(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("62626262-7272-4272-8373-747475757676")
	seedGenerationSession(t, s, string(sid))
	ctx := context.Background()

	priorAliasActivation(t, s, sid, "gen_conflict_authority", []indexformat.NativeAlias{
		{NativeKey: "block:shared", Ref: schema.SourceEntryRef("e_u1")},
	})
	priorAliasAttempt(t, s, sid, "gen_conflict_attempt", hexIdentity('6'), []indexformat.NativeAlias{
		{NativeKey: "block:shared", Ref: schema.SourceEntryRef("e_call1")},
	})

	prior, err := s.ReadGenerationPrior(ctx, sid)
	if err == nil {
		t.Fatalf("ReadGenerationPrior returned %+v, want the conflict refusal", prior)
	}
	if !errors.Is(err, ingest.ErrPriorAliasConflict) {
		t.Fatalf("ReadGenerationPrior error = %v, want ErrPriorAliasConflict", err)
	}
	for _, want := range []string{"shared", "e_call1", "e_u1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("conflict refusal %q does not name %q", err, want)
		}
	}
}

// TestLatestAttemptPriorCertificateOnlyCarriesAttemptAliases proves the
// certificate-only state: with no active generation the prior is the latest
// attempt's aliases and no evidence document, so the stored capture certificate
// remains the authority the caller judges against.
func TestLatestAttemptPriorCertificateOnlyCarriesAttemptAliases(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("73737373-8383-4383-8484-757576767777")
	seedGenerationSession(t, s, string(sid))
	ctx := context.Background()

	priorAliasActivation(t, s, sid, "gen_certificate_authority", []indexformat.NativeAlias{
		{NativeKey: "block:authority", Ref: schema.SourceEntryRef("e_u1")},
	})
	priorAliasAttempt(t, s, sid, "gen_certificate_attempt", hexIdentity('7'), []indexformat.NativeAlias{
		{NativeKey: "block:attempt", Ref: schema.SourceEntryRef("e_result1")},
	})
	// Clear the active pointer: the certificate and the latest attempt stay,
	// which is the V1-certificate / file-backed authority state.
	execGenerationSQL(t, s, `UPDATE sessions SET active_generation_id = NULL WHERE session_id = '`+string(sid)+`';`)

	prior, err := s.ReadGenerationPrior(ctx, sid)
	if err != nil {
		t.Fatalf("ReadGenerationPrior: %v", err)
	}
	if prior == nil {
		t.Fatal("ReadGenerationPrior returned no prior for a certificate-only session with a latest attempt")
	}
	if prior.HasCompleteGeneration {
		t.Fatal("the certificate-only prior claimed a complete generation")
	}
	if prior.Metadata != nil || len(prior.PriorEvidence) != 0 {
		t.Fatalf("the certificate-only prior carried an evidence document (metadata=%t evidence=%d), want none", prior.Metadata != nil, len(prior.PriorEvidence))
	}
	if got := prior.Aliases.Entries["attempt"]; got != "e_result1" {
		t.Fatalf("attempt alias ref = %q, want the attempt's own e_result1", got)
	}
	if _, ok := prior.Aliases.Entries["authority"]; ok {
		t.Fatal("the certificate-only prior carried an alias from the cleared authority")
	}
}
