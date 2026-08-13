package village_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_capabilities.yaml
var contentCapabilityFixtureYAML []byte

//go:embed testdata/content_capabilities.manifest.yaml
var contentCapabilityManifestYAML []byte

type contentCapabilityCase struct {
	Name                  string                    `yaml:"name"`
	Response              contentCapabilityResponse `yaml:"response"`
	SupportsObservedModel bool                      `yaml:"supportsObservedModel"`
}

type contentCapabilityResponse struct {
	AnnotationSchemaVersion string                     `yaml:"annotationSchemaVersion"`
	PushContractVersion     schema.PushContractVersion `yaml:"pushContractVersion"`
	MinPushContractVersion  schema.PushContractVersion `yaml:"minPushContractVersion"`
	ContentCapabilities     []schema.ContentCapability `yaml:"contentCapabilities"`
}

func (r contentCapabilityResponse) wire() schema.SchemaVersionResponse {
	return schema.SchemaVersionResponse{
		AnnotationSchemaVersion: r.AnnotationSchemaVersion,
		PushContractVersion:     r.PushContractVersion,
		MinPushContractVersion:  r.MinPushContractVersion,
		ContentCapabilities:     r.ContentCapabilities,
	}
}

type contentCapabilityFixture struct {
	Cases []contentCapabilityCase `yaml:"cases"`
}

func loadContentCapabilityFixture(t *testing.T) contentCapabilityFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(contentCapabilityFixtureYAML))
	decoder.KnownFields(true)
	var fixture contentCapabilityFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode content capability fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("content capability fixture must contain exactly one document: %v", err)
	}
	manifest, err := testutil.DecodeSemanticManifest(contentCapabilityManifestYAML, "content capability")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for index, fixtureCase := range fixture.Cases {
		names[index] = fixtureCase.Name
		if fixtureCase.Name == "" || fixtureCase.Response.PushContractVersion == "" || fixtureCase.Response.MinPushContractVersion == "" {
			t.Fatalf("content capability fixture case %q is incomplete", fixtureCase.Name)
		}
	}
	if err := testutil.ValidateSemanticNames(manifest, names, "content capability"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestVillageClient_ContentCapabilityAdvertisement(t *testing.T) {
	for _, fixtureCase := range loadContentCapabilityFixture(t).Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			advertisementErr := schema.ValidateContentCapabilityAdvertisements(fixtureCase.Response.ContentCapabilities)
			if fixtureCase.Name == "exact_observed_model_capability" && advertisementErr != nil {
				t.Fatalf("canonical server advertisement rejected: %v", advertisementErr)
			}
			if fixtureCase.Name == "duplicate_known_token_is_tolerated_by_reader" && advertisementErr == nil {
				t.Fatal("duplicate server advertisement unexpectedly passed producer validation")
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/schema/version" {
					t.Fatalf("request path=%q", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(fixtureCase.Response.wire()); err != nil {
					t.Fatalf("encode response: %v", err)
				}
			}))
			defer server.Close()

			client := village.NewVillageClient(server.URL, "test-key", server.Client())
			response, _, err := client.GetSchemaVersion(context.Background())
			if err != nil {
				t.Fatalf("GetSchemaVersion: %v", err)
			}
			if got := village.SupportsObservedModel(response.ContentCapabilities); got != fixtureCase.SupportsObservedModel {
				t.Fatalf("SupportsObservedModel=%t, want %t; capabilities=%+v", got, fixtureCase.SupportsObservedModel, response.ContentCapabilities)
			}
		})
	}
}
