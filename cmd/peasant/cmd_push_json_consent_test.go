package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/push_json_public_consent.yaml
var pushJSONPublicConsentYAML []byte

//go:embed testdata/push_json_public_consent_manifest.yaml
var pushJSONPublicConsentManifestYAML []byte

type pushJSONPublicConsentCase struct {
	Name               string   `yaml:"name"`
	ConfigVisibility   string   `yaml:"configVisibility"`
	Args               []string `yaml:"args"`
	Stdin              string   `yaml:"stdin"`
	WantExit           int      `yaml:"wantExit"`
	WantRefusal        bool     `yaml:"wantRefusal"`
	WantRequests       bool     `yaml:"wantRequests"`
	WantPublishAttempt bool     `yaml:"wantPublishAttempt"`
}

func loadPushJSONPublicConsentCases(t *testing.T) []pushJSONPublicConsentCase {
	t.Helper()
	var fixtures struct {
		Cases []pushJSONPublicConsentCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(pushJSONPublicConsentYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode push --json public consent fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(pushJSONPublicConsentManifestYAML, "push --json public consent")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixtures.Cases))
	for _, fixture := range fixtures.Cases {
		names = append(names, fixture.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "push --json public consent"); err != nil {
		t.Fatal(err)
	}
	return fixtures.Cases
}

// TestPushCmd_JSONIsNeverPublicConsent runs the production push with --json over
// one publishable session and proves --json never stands in for consent: a
// public push without --yes is refused before any peasant online request, with
// exit 3 and the refusal inside the JSON document; --yes still publishes; a
// private push is unchanged; and stdout is one JSON document in every case.
func TestPushCmd_JSONIsNeverPublicConsent(t *testing.T) {
	t.Parallel()
	for _, tc := range loadPushJSONPublicConsentCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			var requests, publishes atomic.Int64
			captured := &capturedPublish{parts: map[string]string{}}
			village := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/auth/me" {
					_ = json.NewEncoder(w).Encode(schema.VillageUser{ID: testutil.VillageFixtureAccountID})
					return
				}
				if id, isTranscript := strings.CutPrefix(r.URL.Path, "/api/v1/transcripts/"); r.Method == http.MethodPatch && isTranscript {
					// The owner update a public publish sends after the
					// upload: answer it as peasant online does, so the
					// consented run succeeds.
					var update schema.OwnerTranscriptUpdateRequest
					if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
						t.Errorf("decode owner update: %v", err)
					}
					var visibility schema.TranscriptUpdateVisibility
					if update.Visibility != nil {
						visibility = *update.Visibility
					}
					_ = json.NewEncoder(w).Encode(schema.OwnerTranscriptUpdateResponse{
						TranscriptID: schema.TranscriptID(id), TranscriptURL: "https://village.example/transcripts/" + id,
						Tags: []string{}, Visibility: visibility, UpdatedAt: 1,
					})
					return
				}
				if !strings.Contains(r.URL.Path, "/transcripts/publish") {
					_, _ = w.Write([]byte(`{}`))
					return
				}
				publishes.Add(1)
				captured.record(t, r)
				receipt, err := testutil.AuthoritativePublishReceipt([]byte(captured.snapshot()["metadata"]), true)
				if err != nil {
					t.Errorf("build authoritative receipt: %v", err)
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write(receipt)
			}))
			t.Cleanup(village.Close)

			dir := t.TempDir()
			writeTestCredentialsFor(t, dir, village.URL)
			seedPushableSession(t, dir)
			cfgPath := writeCfg(t, dir, "consent.yaml",
				"version: 1\noutput:\n  basePath: "+dir+"/transcripts\npush:\n  method: all\n  visibility: "+tc.ConfigVisibility+"\n")

			root := newTestRoot()
			root.AddCommand(BuildPushCommand())
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetIn(strings.NewReader(tc.Stdin))
			args := append([]string{"--data-dir", dir, "--config-dir", dir, "push",
				"--json", "--config", cfgPath, "--state-dir", dir}, tc.Args...)
			root.SetArgs(args)
			err := root.Execute()

			exit := defaults.ExitOK
			if err != nil {
				exit = exitCodeFor(err)
			}
			if tc.WantExit != 0 && exit.Int() != tc.WantExit {
				t.Fatalf("exit = %d, want %d; err: %v\nstdout: %s\nstderr: %s", exit, tc.WantExit, err, stdout.String(), stderr.String())
			}

			// Every case: stdout is exactly one JSON document, and no prompt was
			// shown anywhere.
			var doc map[string]any
			decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
			if decodeErr := decoder.Decode(&doc); decodeErr != nil {
				t.Fatalf("stdout is not a JSON document: %v\nstdout: %s\nstderr: %s", decodeErr, stdout.String(), stderr.String())
			}
			if decoder.More() {
				t.Fatalf("stdout holds more than one JSON document: %s", stdout.String())
			}
			if strings.Contains(stdout.String()+stderr.String(), "Continue? [y/N]") {
				t.Fatalf("--json showed the interactive consent prompt\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
			}

			refusal, _ := doc["refusal"].(string)
			if tc.WantRefusal {
				if !strings.Contains(refusal, "--yes") {
					t.Errorf("the JSON document must carry a refusal naming --yes; got %q", refusal)
				}
				if err == nil {
					t.Error("a refused public push must fail")
				}
			} else if refusal != "" {
				t.Errorf("unexpected refusal %q", refusal)
			}

			if got := requests.Load() > 0; got != tc.WantRequests {
				t.Errorf("peasant online requests made = %v, want %v (count %d)", got, tc.WantRequests, requests.Load())
			}
			if got := publishes.Load() > 0; got != tc.WantPublishAttempt {
				t.Errorf("publication attempted = %v, want %v\nstdout: %s\nstderr: %s", got, tc.WantPublishAttempt, stdout.String(), stderr.String())
			}
			if tc.WantPublishAttempt && err != nil {
				t.Errorf("a consented or private push must succeed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
			}
		})
	}
}
