package ingest

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// bridgePriorStore is a dependency fake for the OpenCode prior loader: it
// answers the active-generation prior and the stored capture authority from
// configured values and records whether the authority was consulted. Only the
// two prior-reader methods are exercised, so the embedded interface stays nil.
type bridgePriorStore struct {
	MetricsStore

	prior          *NativeGenerationPrior
	priorErr       error
	authority      *StoredCaptureAuthority
	authorityErr   error
	authorityReads int
}

func (s *bridgePriorStore) ReadNativeGenerationPrior(context.Context, SessionID) (*NativeGenerationPrior, error) {
	return s.prior, s.priorErr
}

func (s *bridgePriorStore) ReadStoredCaptureAuthority(context.Context, SessionID) (*StoredCaptureAuthority, error) {
	s.authorityReads++
	return s.authority, s.authorityErr
}

// priorOnlyStore implements only the active-generation prior reader, proving a
// store without the authority reader bridges nothing and keeps the empty prior.
type priorOnlyStore struct {
	MetricsStore
	prior *NativeGenerationPrior
}

func (s *priorOnlyStore) ReadNativeGenerationPrior(context.Context, SessionID) (*NativeGenerationPrior, error) {
	return s.prior, nil
}

func bridgeTestSession() DiscoveredSession {
	return DiscoveredSession{SessionID: SessionID("ses_bridge_authority_test"), Harness: HarnessOpenCode}
}

// TestOpenCodePriorLoaderBridgesStoredCaptureAuthority pins the production prior
// acquisition: with no active generation it consults the stored capture
// certificate and preserves its origin and format without fabricating aliases,
// captured-prefix proof, or a complete-generation claim. An active generation
// wins outright, an operator-initiated rebuild suppresses the bridge, and a
// store without the authority reader keeps the existing empty prior.
func TestOpenCodePriorLoaderBridgesStoredCaptureAuthority(t *testing.T) {
	ctx := context.Background()
	session := bridgeTestSession()

	t.Run("active-generation-prior-wins", func(t *testing.T) {
		store := &bridgePriorStore{
			prior:     &NativeGenerationPrior{HasCompleteGeneration: true, Aliases: NewProjectionPriorState()},
			authority: &StoredCaptureAuthority{SourceAuthority: ContentSourceProviderSource, CaptureFormat: ContentCaptureFormatFull},
		}
		p := &Pipeline{metricsStore: store}
		prior, err := p.openCodePriorLoader()(ctx, session)
		if err != nil {
			t.Fatalf("openCodePriorLoader: %v", err)
		}
		if !prior.HasCompleteGeneration {
			t.Fatal("active complete prior was not preserved")
		}
		if prior.CaptureAuthority != nil {
			t.Fatalf("active generation still consulted the stored certificate: %+v", prior.CaptureAuthority)
		}
		if store.authorityReads != 0 {
			t.Fatalf("authority reader consulted %d times with an active generation, want 0", store.authorityReads)
		}
	})

	t.Run("no-generation-bridges-authority", func(t *testing.T) {
		store := &bridgePriorStore{
			authority: &StoredCaptureAuthority{SourceAuthority: ContentSourceProviderSource, CaptureFormat: ContentCaptureFormatFull},
		}
		p := &Pipeline{metricsStore: store}
		prior, err := p.openCodePriorLoader()(ctx, session)
		if err != nil {
			t.Fatalf("openCodePriorLoader: %v", err)
		}
		if prior.HasCompleteGeneration {
			t.Fatal("the bridge claimed a complete generation that does not exist")
		}
		if prior.CaptureAuthority == nil {
			t.Fatal("the stored capture authority was not bridged")
		}
		if prior.CaptureAuthority.SourceAuthority != ContentSourceProviderSource || prior.CaptureAuthority.CaptureFormat != ContentCaptureFormatFull {
			t.Fatalf("bridged authority = %+v, want the certificate's own origin and format", prior.CaptureAuthority)
		}
		if len(prior.Aliases.Entries) != 0 || len(prior.Aliases.Submissions) != 0 {
			t.Fatalf("the bridge fabricated aliases: %+v", prior.Aliases)
		}
		if prior.HasCapturedPrefix || len(prior.CapturedPrefix) != 0 {
			t.Fatalf("the bridge fabricated captured-prefix proof: has=%v rows=%d", prior.HasCapturedPrefix, len(prior.CapturedPrefix))
		}
	})

	t.Run("no-generation-no-authority-keeps-empty-prior", func(t *testing.T) {
		store := &bridgePriorStore{}
		p := &Pipeline{metricsStore: store}
		prior, err := p.openCodePriorLoader()(ctx, session)
		if err != nil {
			t.Fatalf("openCodePriorLoader: %v", err)
		}
		if prior.HasCompleteGeneration || prior.CaptureAuthority != nil {
			t.Fatalf("first discovery claimed authority: %+v", prior)
		}
		if store.authorityReads != 1 {
			t.Fatalf("authority reader consulted %d times, want 1", store.authorityReads)
		}
	})

	t.Run("explicit-rebuild-suppresses-bridge", func(t *testing.T) {
		for _, config := range []PipelineConfig{{Force: true}, {Reindex: true}} {
			store := &bridgePriorStore{
				authority: &StoredCaptureAuthority{SourceAuthority: ContentSourceProviderSource, CaptureFormat: ContentCaptureFormatFull},
			}
			p := &Pipeline{metricsStore: store, config: config}
			prior, err := p.openCodePriorLoader()(ctx, session)
			if err != nil {
				t.Fatalf("openCodePriorLoader: %v", err)
			}
			if prior.CaptureAuthority != nil {
				t.Fatalf("operator-initiated rebuild still bridged the certificate: %+v", prior.CaptureAuthority)
			}
			if store.authorityReads != 0 {
				t.Fatalf("authority reader consulted %d times for an explicit rebuild, want 0", store.authorityReads)
			}
		}
	})

	t.Run("store-without-authority-reader-bridges-nothing", func(t *testing.T) {
		store := &priorOnlyStore{}
		p := &Pipeline{metricsStore: store}
		prior, err := p.openCodePriorLoader()(ctx, session)
		if err != nil {
			t.Fatalf("openCodePriorLoader: %v", err)
		}
		if prior.CaptureAuthority != nil || prior.HasCompleteGeneration {
			t.Fatalf("a store without the authority reader bridged something: %+v", prior)
		}
	})

	t.Run("authority-reader-error-is-actionable", func(t *testing.T) {
		store := &bridgePriorStore{authorityErr: errors.New("injected authority read failure")}
		p := &Pipeline{metricsStore: store}
		_, err := p.openCodePriorLoader()(ctx, session)
		if err == nil {
			t.Fatal("an authority read failure produced no error")
		}
		if !strings.Contains(err.Error(), session.SessionID.String()) {
			t.Fatalf("the refusal does not name the session: %v", err)
		}
	})
}
