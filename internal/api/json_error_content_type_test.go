package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/json-error-headers.yaml
var jsonErrorHeadersYAML []byte

// Errors use the existing contract envelope and statuses, including failures in
// the new session evidence reads; a quote or newline must not break that JSON.
func TestJSONErrorBodiesAreLabelledJSON(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name    string `yaml:"name"`
			Path    string `yaml:"path"`
			Message string `yaml:"message"`
			Status  int    `yaml:"status"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(jsonErrorHeadersYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	srv := NewServer(ServerConfig{Port: 0})
	if err := srv.Listen(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("temporary API server did not stop")
		}
	})
	client := http.Client{Timeout: 5 * time.Second}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			var response *http.Response
			if tc.Path != "" {
				var err error
				response, err = client.Get("http://" + srv.Addr().String() + tc.Path)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				recorder := httptest.NewRecorder()
				writeJSONError(recorder, tc.Status, tc.Message)
				response = recorder.Result()
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.Status {
				t.Fatalf("status = %d, want %d: %s", response.StatusCode, tc.Status, body)
			}
			if !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
				t.Fatalf("JSON body labelled %q", response.Header.Get("Content-Type"))
			}
			var envelope errorEnvelope
			if err := json.Unmarshal(body, &envelope); err != nil || envelope.Error == "" {
				t.Fatalf("invalid error envelope %q: %v", body, err)
			}
			if tc.Message != "" && envelope.Error != tc.Message {
				t.Fatalf("message = %q, want %q", envelope.Error, tc.Message)
			}
		})
	}
}
