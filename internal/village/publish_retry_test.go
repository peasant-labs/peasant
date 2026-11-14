package village_test

import (
	"context"
	_ "embed"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/publish_retry/cases.yaml
var publishRetryCases []byte

//go:embed testdata/publish_retry/manifest.yaml
var publishRetryManifest []byte

type publishRetryCase struct {
	Name         string   `yaml:"name"`
	Status       int      `yaml:"status"`
	Reset        bool     `yaml:"reset"`
	RetryAfter   string   `yaml:"retryAfter"`
	Failures     int64    `yaml:"failures"`
	WantAttempts int64    `yaml:"wantAttempts"`
	WantSleeps   []string `yaml:"wantSleeps"`
	WantStatus   int      `yaml:"wantStatus"`
	WantError    bool     `yaml:"wantError"`
}

var publishRetryNow = time.Date(2026, time.November, 14, 8, 0, 0, 0, time.UTC)

func loadPublishRetryFixtures(t *testing.T) []publishRetryCase {
	t.Helper()
	var doc struct {
		Cases []publishRetryCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(publishRetryCases, &doc); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(publishRetryManifest, "publish retry")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(doc.Cases))
	for _, tc := range doc.Cases {
		names = append(names, tc.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "publish retry"); err != nil {
		t.Fatal(err)
	}
	return doc.Cases
}

func publishRetryRequest(t *testing.T) schema.AuthoritativePublishRequest {
	t.Helper()
	request, err := schema.DecodeAuthoritativePublishRequest([]byte(`{"model":{"harness":"claude-code","model":"claude-opus-4-6"},"contentHash":"a7ffc6f8bf1ed76651c14756a061d662f580ff4de43b49fa82d80a4b80f8434a","visibilityIntent":"private"}`))
	if err != nil {
		t.Fatalf("fixture request: %v", err)
	}
	return request
}

// recordingRetryPolicy returns the default policy with a sleep that records
// each requested wait instead of waiting.
func recordingRetryPolicy(sleeps *[]time.Duration) village.RetryPolicy {
	policy := village.DefaultRetryPolicy()
	policy.Sleep = func(_ context.Context, d time.Duration) error {
		*sleeps = append(*sleeps, d)
		return nil
	}
	policy.Now = func() time.Time { return publishRetryNow }
	return policy
}

// resetConnection drops the connection without writing a response, after the
// request body has been read.
func resetConnection(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	_ = conn.Close()
}

func TestPublishAuthoritativeRetryFixtures(t *testing.T) {
	for _, tc := range loadPublishRetryFixtures(t) {
		t.Run(tc.Name, func(t *testing.T) {
			request := publishRetryRequest(t)
			receipt, err := testutil.AuthoritativePublishReceiptFromRequest(request, true)
			if err != nil {
				t.Fatal(err)
			}
			var attempts, payloads atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := attempts.Add(1)
				parts := parseMultipartBody(t, r)
				if string(parts["transcript_file"]) == `{"type":"say"}` {
					payloads.Add(1)
				}
				if attempt <= tc.Failures {
					if tc.Reset {
						resetConnection(t, w)
						return
					}
					if tc.RetryAfter != "" {
						w.Header().Set("Retry-After", tc.RetryAfter)
					}
					w.WriteHeader(tc.Status)
					_, _ = io.WriteString(w, `{"error":"transient"}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write(receipt)
			}))
			defer server.Close()

			var sleeps []time.Duration
			client := village.NewVillageClient(server.URL, testAPIKey, server.Client())
			client.SetRetryPolicy(recordingRetryPolicy(&sleeps))
			got, status, err := client.PublishAuthoritative(context.Background(), request, strings.NewReader(`{"type":"say"}`), "session.json")

			if (err != nil) != tc.WantError {
				t.Fatalf("err=%v wantError=%v", err, tc.WantError)
			}
			if attempts.Load() != tc.WantAttempts {
				t.Fatalf("attempts=%d want %d", attempts.Load(), tc.WantAttempts)
			}
			if payloads.Load() != tc.WantAttempts {
				t.Fatalf("attempts carrying the full transcript=%d want %d", payloads.Load(), tc.WantAttempts)
			}
			wantSleeps := make([]time.Duration, 0, len(tc.WantSleeps))
			for _, raw := range tc.WantSleeps {
				d, parseErr := time.ParseDuration(raw)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				wantSleeps = append(wantSleeps, d)
			}
			if len(sleeps) != len(wantSleeps) {
				t.Fatalf("sleeps=%v want %v", sleeps, wantSleeps)
			}
			for i := range sleeps {
				if sleeps[i] != wantSleeps[i] {
					t.Fatalf("sleeps=%v want %v", sleeps, wantSleeps)
				}
			}
			if tc.WantError {
				if status != tc.WantStatus {
					t.Fatalf("status=%d want %d", status, tc.WantStatus)
				}
				return
			}
			if status != http.StatusCreated || got.ContentHash != request.ContentHash {
				t.Fatalf("status=%d receipt=%+v, want the success receipt", status, got)
			}
		})
	}
}

// TestPublishAuthoritativeRetryReturnsReplayReceipt covers a publish the
// village applied before the connection dropped: the retry is a replay, and
// the client returns the village's replay receipt and status.
func TestPublishAuthoritativeRetryReturnsReplayReceipt(t *testing.T) {
	request := publishRetryRequest(t)
	replay, err := testutil.AuthoritativePublishReceiptFromRequest(request, false)
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if attempts.Add(1) == 1 {
			resetConnection(t, w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(replay)
	}))
	defer server.Close()

	var observed int
	var sleeps []time.Duration
	client := village.NewVillageClient(server.URL, testAPIKey, server.Client())
	client.SetRetryPolicy(recordingRetryPolicy(&sleeps))
	client.SetRequestObserver(func() { observed++ })
	receipt, status, err := client.PublishAuthoritative(context.Background(), request, strings.NewReader(`{}`), "session.json")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if status != http.StatusOK || receipt.Created || receipt.ContentHash != request.ContentHash {
		t.Fatalf("status=%d created=%v hash=%s, want the replay receipt", status, receipt.Created, receipt.ContentHash)
	}
	if attempts.Load() != 2 || observed != 2 || len(sleeps) != 1 {
		t.Fatalf("server attempts=%d observed=%d sleeps=%v, want 2 attempts and one backoff", attempts.Load(), observed, sleeps)
	}
}

// TestPublishAuthoritativeStopsRetryingWhenContextEnds proves a cancelled
// caller is not retried.
func TestPublishAuthoritativeStopsRetryingWhenContextEnds(t *testing.T) {
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	policy := village.DefaultRetryPolicy()
	policy.Sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	client := village.NewVillageClient(server.URL, testAPIKey, server.Client())
	client.SetRetryPolicy(policy)
	_, status, err := client.PublishAuthoritative(ctx, publishRetryRequest(t), strings.NewReader(`{}`), "session.json")
	if err == nil || status != http.StatusServiceUnavailable || attempts.Load() != 1 {
		t.Fatalf("err=%v status=%d attempts=%d, want one attempt then stop", err, status, attempts.Load())
	}
}
