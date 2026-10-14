package ingest

import (
	"context"
	_ "embed"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/native_prior_authority_bridge.yaml
var nativePriorAuthorityBridgeYAML []byte

// bridgeAuthoritySpec is the certificate every authority-present case serves.
type bridgeAuthoritySpec struct {
	Status           string `yaml:"status"`
	SourceAuthority  string `yaml:"sourceAuthority"`
	TranscriptOrigin string `yaml:"transcriptOrigin"`
	CaptureFormat    string `yaml:"captureFormat"`
	FailureCode      string `yaml:"failureCode"`
}

func (s bridgeAuthoritySpec) authority(t *testing.T) *StoredCaptureAuthority {
	t.Helper()
	status, err := NewContentCaptureStatus(s.Status)
	if err != nil {
		t.Fatalf("bridge fixture authority status: %v", err)
	}
	source, err := NewContentSourceAuthority(s.SourceAuthority)
	if err != nil {
		t.Fatalf("bridge fixture authority source: %v", err)
	}
	format, err := NewContentCaptureFormat(s.CaptureFormat)
	if err != nil {
		t.Fatalf("bridge fixture authority format: %v", err)
	}
	code, err := NewContentCaptureFailureCode(s.FailureCode)
	if err != nil {
		t.Fatalf("bridge fixture authority failure code: %v", err)
	}
	return &StoredCaptureAuthority{Status: status, SourceAuthority: source, TranscriptOrigin: bridgeOrigin(t, s.TranscriptOrigin), CaptureFormat: format, FailureCode: code}
}

func bridgeOrigin(t *testing.T, name string) TranscriptOrigin {
	t.Helper()
	switch name {
	case "file":
		return TranscriptOriginFile
	case "opencode-legacy-sqlite":
		return TranscriptOriginOpenCodeLegacySQLite
	case "opencode-current-sqlite":
		return TranscriptOriginOpenCodeCurrentSQLite
	default:
		t.Fatalf("bridge fixture names unknown transcript origin %q", name)
		return TranscriptOriginFile
	}
}

// bridgePriorCase is one named prior-loader scenario. The loader is a pure
// composition over the active-generation prior reader and the stored-capture
// authority reader, so each case configures those two seams and the run mode.
type bridgePriorCase struct {
	Name                 string `yaml:"name"`
	Prior                string `yaml:"prior"`
	Authority            string `yaml:"authority"`
	StoreKind            string `yaml:"storeKind"`
	Force                bool   `yaml:"force"`
	Reindex              bool   `yaml:"reindex"`
	AuthorityError       bool   `yaml:"authorityError"`
	WantPrior            string `yaml:"wantPrior"`
	WantCaptureAuthority bool   `yaml:"wantCaptureAuthority"`
	WantAuthorityReads   int    `yaml:"wantAuthorityReads"`
	WantError            bool   `yaml:"wantError"`
}

type bridgePriorDocument struct {
	RequiredNames []string            `yaml:"requiredNames"`
	SessionID     string              `yaml:"sessionId"`
	Authority     bridgeAuthoritySpec `yaml:"authority"`
	Cases         []bridgePriorCase   `yaml:"cases"`
}

func loadBridgePriorDocument(t *testing.T) bridgePriorDocument {
	t.Helper()
	decoder := yaml.NewDecoder(strings.NewReader(string(nativePriorAuthorityBridgeYAML)))
	decoder.KnownFields(true)
	var document bridgePriorDocument
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode native_prior_authority_bridge.yaml: %v", err)
	}
	if len(document.RequiredNames) == 0 {
		t.Fatal("native_prior_authority_bridge.yaml declares no required names")
	}
	seen := make(map[string]bool, len(document.Cases))
	names := make([]string, 0, len(document.Cases))
	for _, c := range document.Cases {
		if strings.TrimSpace(c.Name) == "" || seen[c.Name] {
			t.Fatalf("native prior authority bridge case name %q is empty or repeated", c.Name)
		}
		seen[c.Name] = true
		switch c.Prior {
		case "active-complete", "none":
		default:
			t.Fatalf("case %q names an unknown prior %q", c.Name, c.Prior)
		}
		switch c.Authority {
		case "present", "absent":
		default:
			t.Fatalf("case %q names an unknown authority %q", c.Name, c.Authority)
		}
		switch c.StoreKind {
		case "", "both", "prior-only":
		default:
			t.Fatalf("case %q names an unknown store kind %q", c.Name, c.StoreKind)
		}
		if c.WantError {
			if c.WantPrior != "" {
				t.Fatalf("case %q expects an error and also names a wanted prior %q", c.Name, c.WantPrior)
			}
		} else {
			switch c.WantPrior {
			case "active", "bridge", "empty":
			default:
				t.Fatalf("case %q names an unknown wanted prior %q", c.Name, c.WantPrior)
			}
		}
		names = append(names, c.Name)
	}
	required := make(map[string]bool, len(document.RequiredNames))
	for _, name := range document.RequiredNames {
		required[name] = true
	}
	for _, name := range names {
		if !required[name] {
			t.Fatalf("case %q is not declared in requiredNames; add it to the deletion guard", name)
		}
	}
	for _, name := range document.RequiredNames {
		if !seen[name] {
			t.Fatalf("requiredNames declares %q, which no case carries", name)
		}
	}
	return document
}

// bridgePriorStore is a dependency fake for the OpenCode prior loader: it
// answers the active-generation prior and the stored capture authority from
// configured values and records whether the authority was consulted. Only the
// two prior-reader methods are exercised, so the embedded interface stays nil.
type bridgePriorStore struct {
	MetricsStore

	prior          *NativeGenerationPrior
	authority      *StoredCaptureAuthority
	authorityErr   error
	authorityReads int
}

func (s *bridgePriorStore) ReadNativeGenerationPrior(context.Context, SessionID) (*NativeGenerationPrior, error) {
	return s.prior, nil
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

// TestOpenCodePriorLoaderBridgesStoredCaptureAuthority drives every named prior
// loader scenario through the production loader: with no active generation it
// consults the stored certificate and preserves its state without fabricating
// aliases, captured-prefix proof, or a complete-generation claim. An active
// generation wins outright, an operator-initiated rebuild suppresses the
// bridge, and a store without the authority reader keeps the existing empty
// prior.
func TestOpenCodePriorLoaderBridgesStoredCaptureAuthority(t *testing.T) {
	document := loadBridgePriorDocument(t)
	session := DiscoveredSession{SessionID: SessionID(document.SessionID), Harness: HarnessOpenCode}
	ctx := context.Background()
	for _, tc := range document.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			authority := document.Authority.authority(t)
			var store MetricsStore
			var reads *int
			switch tc.StoreKind {
			case "prior-only":
				store = &priorOnlyStore{prior: activePriorFor(tc)}
			default:
				fake := &bridgePriorStore{prior: activePriorFor(tc)}
				if tc.Authority == "present" {
					fake.authority = authority
				}
				if tc.AuthorityError {
					fake.authorityErr = errors.New("injected authority read failure")
				}
				reads = &fake.authorityReads
				store = fake
			}
			p := &Pipeline{metricsStore: store, config: PipelineConfig{Force: tc.Force, Reindex: tc.Reindex}}
			prior, err := p.openCodePriorLoader()(ctx, session)
			if tc.WantError {
				if err == nil {
					t.Fatal("an authority read failure produced no error")
				}
				if !strings.Contains(err.Error(), document.SessionID) {
					t.Fatalf("the refusal does not name the session: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("openCodePriorLoader: %v", err)
			}
			assertBridgePrior(t, tc, prior, authority)
			if reads != nil && *reads != tc.WantAuthorityReads {
				t.Fatalf("authority reader consulted %d times, want %d", *reads, tc.WantAuthorityReads)
			}
		})
	}
}

func activePriorFor(tc bridgePriorCase) *NativeGenerationPrior {
	if tc.Prior != "active-complete" {
		return nil
	}
	return &NativeGenerationPrior{HasCompleteGeneration: true, Aliases: NewProjectionPriorState()}
}

func assertBridgePrior(t *testing.T, tc bridgePriorCase, prior OpenCodeProvenancePrior, authority *StoredCaptureAuthority) {
	t.Helper()
	switch tc.WantPrior {
	case "active":
		if !prior.HasCompleteGeneration {
			t.Fatal("active complete prior was not preserved")
		}
	case "bridge":
		if prior.HasCompleteGeneration {
			t.Fatal("the bridge claimed a complete generation that does not exist")
		}
		if prior.CaptureAuthority == nil {
			t.Fatal("the stored capture authority was not bridged")
		}
		if *prior.CaptureAuthority != *authority {
			t.Fatalf("bridged authority = %+v, want the certificate's own state %+v", *prior.CaptureAuthority, *authority)
		}
	case "empty":
		if prior.HasCompleteGeneration {
			t.Fatal("the empty prior claimed a complete generation")
		}
	}
	if got := prior.CaptureAuthority != nil; got != tc.WantCaptureAuthority {
		t.Fatalf("capture authority present = %v, want %v", got, tc.WantCaptureAuthority)
	}
	if len(prior.Aliases.Entries) != 0 || len(prior.Aliases.Submissions) != 0 {
		t.Fatalf("the loader fabricated aliases: %+v", prior.Aliases)
	}
	if prior.HasCapturedPrefix || len(prior.CapturedPrefix) != 0 {
		t.Fatalf("the loader fabricated captured-prefix proof: has=%v rows=%d", prior.HasCapturedPrefix, len(prior.CapturedPrefix))
	}
}
