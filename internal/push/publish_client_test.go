package push

import (
	_ "embed"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/publish_client.yaml
var publishClientYAML []byte

//go:embed testdata/publish_client.manifest.yaml
var publishClientManifestYAML []byte

type publishClientCase struct {
	Name        string `yaml:"name"`
	AppVersion  string `yaml:"appVersion"`
	SentVersion string `yaml:"sentVersion"`
}

type publishClientFingerprintCase struct {
	Name        string   `yaml:"name"`
	AppVersions []string `yaml:"appVersions"`
}

type publishClientFixture struct {
	Name             string                         `yaml:"name"`
	Cases            []publishClientCase            `yaml:"cases"`
	FingerprintCases []publishClientFingerprintCase `yaml:"fingerprintCases"`
}

func loadPublishClientFixture(t *testing.T) publishClientFixture {
	t.Helper()
	var fixture publishClientFixture
	if err := testutil.DecodeFixtureYAML(publishClientYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(publishClientManifestYAML, "publish client")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	for _, c := range fixture.FingerprintCases {
		if len(c.AppVersions) < 2 {
			t.Fatalf("fingerprint case %q needs at least two versions", c.Name)
		}
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "publish client"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// publishClientInputs builds one valid metadata document and transcript through
// the production mapper, reusing the metadata-limit recipe.
func publishClientInputs(t *testing.T) (metadata, content []byte) {
	t.Helper()
	recipe := loadPublicationMetadataLimits(t)
	metadata, err := mapMetadataWithPolicy(metadataLimitOptions(t, recipe, 1), defaults.PushMetadataDocumentCapBytes)
	if err != nil {
		t.Fatal(err)
	}
	return metadata, []byte(recipe.Recipe.Transcript)
}

func TestAuthoritativeRequestClient(t *testing.T) {
	t.Parallel()
	fixture := loadPublishClientFixture(t)
	metadata, content := publishClientInputs(t)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			request, err := buildAuthoritativeRequestWithPolicy(metadata, content, defaults.PushMetadataDocumentCapBytes, defaults.AppVersion(c.AppVersion))
			if err != nil {
				t.Fatal(err)
			}
			wire, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]json.RawMessage
			if err := json.Unmarshal(wire, &document); err != nil {
				t.Fatal(err)
			}
			if c.SentVersion == "" {
				if request.Client != nil {
					t.Fatalf("client = %+v, want omitted", *request.Client)
				}
				if _, ok := document["client"]; ok {
					t.Fatalf("wire carries client %s, want no client key", document["client"])
				}
				return
			}
			want := schema.AuthoritativeClientInfo{Name: fixture.Name, Version: c.SentVersion}
			if request.Client == nil || *request.Client != want {
				t.Fatalf("client = %+v, want %+v", request.Client, want)
			}
			if err := request.Client.Validate(); err != nil {
				t.Fatalf("sent client fails the schema validator: %v", err)
			}
			decoded, err := schema.DecodeAuthoritativePublishMetadataRaw(wire)
			if err != nil {
				t.Fatalf("strict schema decoder refused the final request: %v", err)
			}
			if !reflect.DeepEqual(decoded, request) {
				t.Fatal("strict schema decoder changed the final request")
			}
			if err := schema.ValidatePublicationRequest(decoded); err != nil {
				t.Fatalf("schema refused the final request: %v", err)
			}
		})
	}
}

func TestAuthoritativeRequestClientOutsideFingerprint(t *testing.T) {
	t.Parallel()
	fixture := loadPublishClientFixture(t)
	metadata, content := publishClientInputs(t)
	for _, c := range fixture.FingerprintCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			var (
				firstOperation   schema.CanonicalPublishOperation
				firstFingerprint schema.PublishRequestFingerprint
				clients          = map[string]bool{}
			)
			for i, version := range c.AppVersions {
				request, err := buildAuthoritativeRequestWithPolicy(metadata, content, defaults.PushMetadataDocumentCapBytes, defaults.AppVersion(version))
				if err != nil {
					t.Fatal(err)
				}
				client := "omitted"
				if request.Client != nil {
					client = request.Client.Version
				}
				clients[client] = true
				operation, err := schema.CanonicalizePublishRequest(request)
				if err != nil {
					t.Fatal(err)
				}
				fingerprint, err := schema.FingerprintPublishOperation(operation)
				if err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					firstOperation, firstFingerprint = operation, fingerprint
					continue
				}
				if fingerprint != firstFingerprint {
					t.Fatalf("version %q fingerprint = %s, want %s", version, fingerprint, firstFingerprint)
				}
				if !reflect.DeepEqual(operation, firstOperation) {
					t.Fatalf("version %q changed the canonical operation", version)
				}
			}
			if len(clients) != len(c.AppVersions) {
				t.Fatalf("the versions built only %d distinct clients, want %d", len(clients), len(c.AppVersions))
			}
		})
	}
}
