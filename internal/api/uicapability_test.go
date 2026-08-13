package api

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	schema "github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

const (
	expectedUICapabilityEndpointRows = 2
	expectedUICapabilityProducerRows = 3
)

//go:embed testdata/ui_capabilities.yaml
var uiCapabilitiesYAML []byte

// uiCapabilityFixtures is the complete UI-capabilities advertisement corpus:
// endpoint cases drive the real HTTP handler; producer cases drive the response
// constructor's canonicalization and fail-loud rejection at the seam a running
// server never reaches.
type uiCapabilityFixtures struct {
	DeclaredEndpointRows int                    `yaml:"declared_endpoint_rows"`
	DeclaredProducerRows int                    `yaml:"declared_producer_rows"`
	EndpointCases        []uiCapabilityEndpoint `yaml:"endpoint_cases"`
	ProducerCases        []uiCapabilityProducer `yaml:"producer_cases"`
}

// uiCapabilityEndpoint is one server-config -> advertised-tokens expectation
// exercised through the mounted HTTP endpoint.
type uiCapabilityEndpoint struct {
	Name           string   `yaml:"name"`
	Experimental   bool     `yaml:"experimental"`
	ExpectedTokens []string `yaml:"expected_tokens"`
}

// uiCapabilityProducer is one raw-token-input -> canonical-output (or actionable
// error) expectation exercised through newUICapabilitiesResponse directly.
type uiCapabilityProducer struct {
	Name           string   `yaml:"name"`
	InputTokens    []string `yaml:"input_tokens"`
	ExpectError    bool     `yaml:"expect_error"`
	ErrorContains  string   `yaml:"error_contains"`
	ExpectedTokens []string `yaml:"expected_tokens"`
}

// decodeUICapabilityFixtures strictly decodes the advertisement corpus: unknown
// fields and trailing documents are rejected. It returns an error (rather than
// failing a test) so the strict guards themselves can be exercised.
func decodeUICapabilityFixtures(data []byte) (uiCapabilityFixtures, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var fixtures uiCapabilityFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		return uiCapabilityFixtures{}, fmt.Errorf("decode ui-capabilities fixtures with known fields: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return uiCapabilityFixtures{}, fmt.Errorf("decode ui-capabilities fixtures: expected exactly one YAML document, got trailing content: %w", err)
	}
	return fixtures, nil
}

// loadUICapabilityFixtures strictly decodes and validates the advertisement
// corpus: unknown fields and trailing documents are rejected, declared row
// counts must match the actual rows, and case names must be unique per family.
func loadUICapabilityFixtures(t *testing.T) uiCapabilityFixtures {
	t.Helper()

	fixtures, err := decodeUICapabilityFixtures(uiCapabilitiesYAML)
	if err != nil {
		t.Fatalf("%v", err)
	}

	if fixtures.DeclaredEndpointRows != expectedUICapabilityEndpointRows || len(fixtures.EndpointCases) != expectedUICapabilityEndpointRows {
		t.Fatalf(
			"validate ui-capabilities endpoint row guard: declared=%d, actual=%d, required=%d",
			fixtures.DeclaredEndpointRows, len(fixtures.EndpointCases), expectedUICapabilityEndpointRows,
		)
	}
	if fixtures.DeclaredProducerRows != expectedUICapabilityProducerRows || len(fixtures.ProducerCases) != expectedUICapabilityProducerRows {
		t.Fatalf(
			"validate ui-capabilities producer row guard: declared=%d, actual=%d, required=%d",
			fixtures.DeclaredProducerRows, len(fixtures.ProducerCases), expectedUICapabilityProducerRows,
		)
	}

	endpointNames := make(map[string]struct{}, len(fixtures.EndpointCases))
	for _, c := range fixtures.EndpointCases {
		if strings.TrimSpace(c.Name) == "" {
			t.Fatal("validate ui-capabilities fixtures: endpoint case name is empty")
		}
		if _, dup := endpointNames[c.Name]; dup {
			t.Fatalf("validate ui-capabilities fixtures: duplicate endpoint case %q", c.Name)
		}
		endpointNames[c.Name] = struct{}{}
	}
	producerNames := make(map[string]struct{}, len(fixtures.ProducerCases))
	for _, c := range fixtures.ProducerCases {
		if strings.TrimSpace(c.Name) == "" {
			t.Fatal("validate ui-capabilities fixtures: producer case name is empty")
		}
		if _, dup := producerNames[c.Name]; dup {
			t.Fatalf("validate ui-capabilities fixtures: duplicate producer case %q", c.Name)
		}
		producerNames[c.Name] = struct{}{}
	}

	return fixtures
}

// TestUICapabilities_StrictDecoder proves the fixture loader rejects unknown
// fields and trailing documents, so the strict guards are not vacuous.
func TestUICapabilities_StrictDecoder(t *testing.T) {
	unknownField := append([]byte("unexpected_fixture_field: true\n"), uiCapabilitiesYAML...)
	if _, err := decodeUICapabilityFixtures(unknownField); err == nil || !strings.Contains(err.Error(), "field unexpected_fixture_field not found") {
		t.Fatalf("unknown field error = %v, want strict field rejection", err)
	}
	trailingDocument := append(append([]byte{}, uiCapabilitiesYAML...), []byte("\n---\nunexpected: document\n")...)
	if _, err := decodeUICapabilityFixtures(trailingDocument); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing document error = %v, want single-document rejection", err)
	}
}

// TestUICapabilities_Endpoint drives the mounted GET /api/v1/config/capabilities
// handler for each fixture server config and asserts the advertised token set
// plus the JSON content type and no-store cache header.
func TestUICapabilities_Endpoint(t *testing.T) {
	fixtures := loadUICapabilityFixtures(t)

	for _, tc := range fixtures.EndpointCases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			srv := NewServer(ServerConfig{Port: 0, Experimental: tc.Experimental})
			if err := srv.Listen(ctx); err != nil {
				t.Fatalf("Listen: %v", err)
			}
			baseURL := "http://" + srv.Addr().String()

			errCh := make(chan error, 1)
			go func() { errCh <- srv.Serve(ctx) }()

			resp, err := http.Get(baseURL + defaults.RouteConfigCapabilities.String())
			if err != nil {
				t.Fatalf("GET %s: %v", defaults.RouteConfigCapabilities, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
			if got := resp.Header.Get(defaults.HeaderContentType); got != defaults.ContentJSON.String() {
				t.Errorf("Content-Type = %q, want %q", got, defaults.ContentJSON.String())
			}
			if got := resp.Header.Get(defaults.HeaderCacheControl); got != defaults.CacheControlNoStore {
				t.Errorf("Cache-Control = %q, want %q", got, defaults.CacheControlNoStore)
			}

			body, _ := io.ReadAll(resp.Body)
			var result schema.UICapabilitiesResponse
			if err := json.Unmarshal(body, &result); err != nil {
				t.Fatalf("unmarshal response %q: %v", string(body), err)
			}
			if err := assertTokensEqual(tc.ExpectedTokens, result.UICapabilities); err != nil {
				t.Errorf("advertised tokens: %v (body: %s)", err, string(body))
			}

			cancel()
			if err := <-errCh; err != nil {
				t.Errorf("shutdown: %v", err)
			}
		})
	}
}

// TestUICapabilities_Producer drives newUICapabilitiesResponse directly to prove
// dedupe + lexicographic sort and the actionable rejection of out-of-inventory
// tokens.
func TestUICapabilities_Producer(t *testing.T) {
	fixtures := loadUICapabilityFixtures(t)

	for _, tc := range fixtures.ProducerCases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			input := make([]UICapability, len(tc.InputTokens))
			for i, raw := range tc.InputTokens {
				input[i] = UICapability(raw)
			}

			resp, err := newUICapabilitiesResponse(input)
			if tc.ExpectError {
				if err == nil {
					t.Fatalf("expected error, got nil (resp: %+v)", resp)
				}
				if !strings.Contains(err.Error(), tc.ErrorContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.ErrorContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err := assertTokensEqual(tc.ExpectedTokens, resp.UICapabilities); err != nil {
				t.Errorf("canonical tokens: %v", err)
			}
		})
	}
}

// assertTokensEqual compares an expected token list against an advertised list,
// treating an empty expectation and a nil/omitted advertisement as equal.
func assertTokensEqual(want, got []string) error {
	if len(want) != len(got) {
		return fmt.Errorf("length mismatch: want %v (%d), got %v (%d)", want, len(want), got, len(got))
	}
	for i := range want {
		if want[i] != got[i] {
			return fmt.Errorf("index %d: want %q, got %q (want=%v got=%v)", i, want[i], got[i], want, got)
		}
	}
	return nil
}
