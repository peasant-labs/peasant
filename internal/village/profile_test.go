package village_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/perf"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/profile_transport/cases.yaml
var transportProfileCases []byte

//go:embed testdata/profile_transport/manifest.yaml
var transportProfileManifest []byte

type transportProfileCase struct {
	Name                  string `yaml:"name"`
	Status                int    `yaml:"status"`
	ResponseBody          string `yaml:"responseBody"`
	ResponseError         bool   `yaml:"responseError"`
	TransportError        bool   `yaml:"transportError"`
	Publish               bool   `yaml:"publish"`
	ReadRequestBytes      int64  `yaml:"readRequestBytes"`
	Disabled              bool   `yaml:"disabled"`
	InvalidRequest        bool   `yaml:"invalidRequest"`
	WantError             bool   `yaml:"wantError"`
	Retryable             bool   `yaml:"retryable"`
	RetryDecision         bool   `yaml:"retryDecision"`
	ExpectedResponses     int64  `yaml:"expectedResponses"`
	ExpectedResponseBytes int64  `yaml:"expectedResponseBytes"`
}

type profileRoundTrip func(*http.Request) (*http.Response, error)

var _ http.RoundTripper = profileRoundTrip(nil)

func (f profileRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failingProfileReader struct{}

var _ io.Reader = failingProfileReader{}

func (failingProfileReader) Read([]byte) (int, error) {
	return 0, errors.New("PRIVATE_TRANSCRIPT_SENTINEL")
}

func loadTransportProfileFixtures(t *testing.T) []transportProfileCase {
	t.Helper()
	var doc struct {
		Cases []transportProfileCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(transportProfileCases))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("fixture must contain exactly one document: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(transportProfileManifest, "transport profiles")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(doc.Cases))
	for _, tc := range doc.Cases {
		names = append(names, tc.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "transport profiles"); err != nil {
		t.Fatal(err)
	}
	return doc.Cases
}

func TestClientProfileTransportFixtures(t *testing.T) {
	for _, tc := range loadTransportProfileFixtures(t) {
		t.Run(tc.Name, func(t *testing.T) {
			var attempts, consumed int64
			var trace bytes.Buffer
			collector := perf.NewCollectorWithOptions(nil, perf.NewJSONLTraceSink(&trace), perf.Options{Enabled: true})
			ctx := context.Background()
			if !tc.Disabled {
				ctx = perf.ContextWithRecorder(ctx, collector)
			}
			transport := profileRoundTrip(func(req *http.Request) (*http.Response, error) {
				attempts++
				if req.Body != nil {
					defer req.Body.Close()
					var err error
					if tc.ReadRequestBytes > 0 {
						consumed, err = io.CopyN(io.Discard, req.Body, tc.ReadRequestBytes)
					} else {
						consumed, err = io.Copy(io.Discard, req.Body)
					}
					if err != nil {
						t.Error(err)
					}
				}
				if tc.TransportError {
					return nil, errors.New("PRIVATE_TRANSCRIPT_SENTINEL")
				}
				var body io.Reader = strings.NewReader(tc.ResponseBody)
				if tc.ResponseError {
					body = io.MultiReader(body, failingProfileReader{})
				}
				return &http.Response{StatusCode: tc.Status, Body: io.NopCloser(body), Header: make(http.Header)}, nil
			})
			client := village.NewVillageClient("https://village.example", "test", &http.Client{Transport: transport})
			var err error
			if tc.InvalidRequest {
				_, _, err = client.PublishAuthoritative(ctx, schema.AuthoritativePublishRequest{}, strings.NewReader("PRIVATE_TRANSCRIPT_SENTINEL"), "content.json")
			} else if tc.Publish {
				_, _, err = client.Publish(ctx, []byte(`{}`), strings.NewReader("PRIVATE_TRANSCRIPT_SENTINEL"), "content.json")
			} else {
				_, _, err = client.GetSchemaVersion(ctx)
			}
			if (err != nil) != tc.WantError {
				t.Fatalf("error=%v wantError=%v", err, tc.WantError)
			}
			wantAttempts := int64(1)
			if tc.InvalidRequest {
				wantAttempts = 0
			}
			if attempts != wantAttempts {
				t.Fatalf("actual requests=%d want=%d; retry policy must not change", attempts, wantAttempts)
			}
			if tc.Disabled {
				if len(collector.Events()) != 0 || trace.Len() != 0 {
					t.Fatal("disabled client emitted profile events")
				}
				return
			}
			counts := map[perf.CounterName]int64{}
			var retryable, retryDecision bool
			for _, event := range collector.Events() {
				if event.Kind == perf.EventKindCounter {
					counts[event.CounterName] += event.CounterValue
				}
				if event.SafeError != nil {
					retryable = retryable || event.SafeError.Retryable
				}
				if event.Stage == perf.StagePushRetry {
					retryDecision = true
					if event.Outcome != perf.OutcomeSkipped {
						t.Error("retry decision must be skipped without a retry loop")
					}
				}
			}
			if counts[perf.CounterPushHTTPRequests] != attempts || counts[perf.CounterPushHTTPResponses] != tc.ExpectedResponses || counts[perf.CounterPushHTTPRetries] != 0 {
				t.Fatalf("request counters=%v", counts)
			}
			if counts[perf.CounterPushPayloadBytes] != consumed || counts[perf.CounterPushResponseBytes] != tc.ExpectedResponseBytes {
				t.Fatalf("byte counters=%v consumed=%d", counts, consumed)
			}
			if retryable != tc.Retryable || retryDecision != tc.RetryDecision {
				t.Fatalf("retryable=%v decision=%v", retryable, retryDecision)
			}
			if strings.Contains(trace.String(), "PRIVATE_TRANSCRIPT_SENTINEL") || strings.Contains(trace.String(), "https://village.example") {
				t.Fatal("transport profile leaked body or URL")
			}
		})
	}
}
