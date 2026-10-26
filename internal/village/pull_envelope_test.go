package village_test

import (
	"context"
	_ "embed"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/pull_envelopes.yaml
var pullEnvelopeFixtureYAML []byte

type pullEnvelopeFixtures struct {
	RequiredNames []string                  `yaml:"requiredNames"`
	Negotiate     []pullNegotiateFixture    `yaml:"negotiate"`
	Read          []pullEnvelopeReadFixture `yaml:"read"`
}

type pullNegotiateFixture struct {
	Name         string                     `yaml:"name"`
	Min          schema.PushContractVersion `yaml:"min"`
	Current      schema.PushContractVersion `yaml:"current"`
	Max          schema.PushContractVersion `yaml:"max"`
	WantEnvelope schema.PushContractVersion `yaml:"wantEnvelope"`
	WantError    string                     `yaml:"wantError"`
}

type pullEnvelopeReadFixture struct {
	Name             string                     `yaml:"name"`
	Envelope         schema.PushContractVersion `yaml:"envelope"`
	Served           string                     `yaml:"served"`
	Status           int                        `yaml:"status"`
	Surface          string                     `yaml:"surface"`
	Body             string                     `yaml:"body"`
	Want             []pullPersonExpect         `yaml:"want"`
	WantError        string                     `yaml:"wantError"`
	WantIncompatible bool                       `yaml:"wantIncompatible"`
}

type pullPersonExpect struct {
	Kind     schema.VillagePublicOwnerKind `yaml:"kind"`
	ID       schema.VillageUUID            `yaml:"id"`
	Username string                        `yaml:"username"`
}

func loadPullEnvelopeFixtures(t *testing.T) pullEnvelopeFixtures {
	t.Helper()
	var f pullEnvelopeFixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(pullEnvelopeFixtureYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&f); err != nil {
		t.Fatalf("decode pull envelope fixtures: %v", err)
	}
	present := map[string]bool{}
	for _, c := range f.Negotiate {
		present[c.Name] = true
	}
	for _, c := range f.Read {
		present[c.Name] = true
	}
	for _, name := range f.RequiredNames {
		if !present[name] {
			t.Fatalf("pull envelope fixture %q is required but missing", name)
		}
	}
	return f
}

// The CLI reads pull contracts 0.2.0 and 0.3.0, so it negotiates against a
// village by window overlap and records the newest shared envelope.
func TestNegotiatePull_WindowOverlapFixtures(t *testing.T) {
	if defaults.MinPullContractVersion != "0.2.0" || defaults.PullContractVersion != "0.3.0" {
		t.Fatalf("the fixtures describe the pull window [0.2.0, 0.3.0]; got [%s, %s]", defaults.MinPullContractVersion, defaults.PullContractVersion)
	}
	for _, c := range loadPullEnvelopeFixtures(t).Negotiate {
		t.Run(c.Name, func(t *testing.T) {
			body := `{"annotationSchemaVersion":"16","supportedTargetKinds":["session"],"supportedTypeIds":["t1"],` +
				`"pushContractVersion":"0.1.0","minPushContractVersion":"0.1.0"`
			if c.Min != "" || c.Current != "" {
				body += `,"minPullContractVersion":"` + c.Min.String() + `","pullContractVersion":"` + c.Current.String() + `"`
			}
			if c.Max != "" {
				body += `,"pullContractMaxVersion":"` + c.Max.String() + `"`
			}
			srv := negotiateTestServer(t, body+"}")
			envelope, err := village.NewVillageClient(srv.URL, testAPIKey, nil).NegotiatePull(context.Background())
			switch c.WantError {
			case "":
				if err != nil {
					t.Fatalf("NegotiatePull: %v", err)
				}
				if envelope != c.WantEnvelope {
					t.Fatalf("negotiated envelope = %q, want %q", envelope, c.WantEnvelope)
				}
			case "incompatible":
				if !errors.Is(err, village.ErrPullContractIncompatible) || !strings.Contains(err.Error(), "incompatible") {
					t.Fatalf("want an incompatible-window error; got envelope %q, err %v", envelope, err)
				}
			case "too-old":
				if !errors.Is(err, village.ErrPullContractIncompatible) || !strings.Contains(err.Error(), "does not advertise a pull contract") {
					t.Fatalf("want a village-too-old error; got envelope %q, err %v", envelope, err)
				}
			default:
				t.Fatalf("unknown wantError %q", c.WantError)
			}
		})
	}
}

// Every pull GET asks for the newest pull contract this CLI reads and reads a
// person in the envelope the response names, falling back to the negotiated
// envelope, then 0.2.0, when the response names none; anything that is not a
// profile reads as anonymous and keeps no identity. A 406 or an envelope this
// CLI does not read is the incompatible-contract error.
func TestPullGETs_ReadPersonFixtures(t *testing.T) {
	id := mustTranscriptID(t)
	for _, c := range loadPullEnvelopeFixtures(t).Read {
		t.Run(c.Name, func(t *testing.T) {
			route := map[string]string{
				"metadata":    "/api/v1/pull/transcripts/" + id.String(),
				"annotations": "/api/v1/pull/transcripts/" + id.String() + "/annotations",
				"list":        "/api/v1/pull/transcripts",
				"content":     "/api/v1/pull/transcripts/" + id.String() + "/content",
			}[c.Surface]
			if route == "" {
				t.Fatalf("unknown surface %q", c.Surface)
			}
			var requested []string
			srv := pullTestServer(t, route, func(w http.ResponseWriter, r *http.Request) {
				requested = r.Header.Values(schema.PullContractHeader)
				w.Header().Set("Content-Type", "application/json")
				if c.Served != "" {
					w.Header().Set(schema.PullContractHeader, c.Served)
				}
				if c.Status != 0 {
					w.WriteHeader(c.Status)
				}
				_, _ = io.WriteString(w, strings.ReplaceAll(c.Body, "TRANSCRIPT", id.String()))
			})
			client := village.NewVillageClient(srv.URL, testAPIKey, nil)
			client.SetPullEnvelope(c.Envelope)

			var people []schema.VillagePublicOwner
			var err error
			switch c.Surface {
			case "metadata":
				var info *schema.PullTranscriptInfo
				if info, err = client.GetPullTranscript(context.Background(), id); err == nil {
					people = append(people, info.Owner)
				}
			case "annotations":
				var annotations []schema.PullAnnotation
				if annotations, err = client.GetPullTranscriptAnnotations(context.Background(), id); err == nil {
					for _, a := range annotations {
						people = append(people, a.Author)
					}
				}
			case "list":
				var list *schema.PullListResponse
				if list, err = client.ListPullableTranscripts(context.Background(), village.PullListOptions{Page: 1, Limit: 50}); err == nil {
					for _, row := range list.Transcripts {
						people = append(people, row.Owner)
					}
				}
			case "content":
				var body io.ReadCloser
				if body, _, err = client.GetPullTranscriptContent(context.Background(), id, ""); err == nil {
					_, err = io.ReadAll(body)
					_ = body.Close()
				}
			}
			if len(requested) != 1 || requested[0] != schema.PublicOwnerPullContractVersion.String() {
				t.Errorf("%s read sent %s = %q, want exactly [%q]", c.Surface, schema.PullContractHeader, requested, schema.PublicOwnerPullContractVersion)
			}
			if c.WantIncompatible && !errors.Is(err, village.ErrPullContractIncompatible) {
				t.Fatalf("want the incompatible-contract error; got %v", err)
			}
			if c.WantError != "" {
				if err == nil || !strings.Contains(err.Error(), c.WantError) {
					t.Fatalf("want an error containing %q; got %v", c.WantError, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("read %s: %v", c.Surface, err)
			}
			if len(people) != len(c.Want) {
				t.Fatalf("read %d people, want %d", len(people), len(c.Want))
			}
			for i, want := range c.Want {
				got := people[i]
				if got.Kind != want.Kind || got.ID != want.ID || got.Username != want.Username || (want.Kind == schema.VillagePublicOwnerAnonymous && (got.DisplayName != nil || got.AvatarURL != nil)) {
					t.Errorf("person %d = %+v, want %+v", i, got, want)
				}
			}
		})
	}
}

// A stored metadata snapshot written before the upgrade reads back as the same
// profile, so an existing pull stays valid.
func TestDecodePullTranscriptInfo_LegacySnapshot(t *testing.T) {
	info, err := village.DecodePullTranscriptInfo([]byte(`{"transcriptId":"` + mustTranscriptID(t).String() + `","ownerUserId":"0a000000-0000-4000-8000-000000000001","ownerUsername":"alice","visibility":"group","publishedAt":0,"updatedAt":0,"annotationCount":0}`))
	if err != nil {
		t.Fatalf("DecodePullTranscriptInfo: %v", err)
	}
	if !info.Owner.IsAccount("0a000000-0000-4000-8000-000000000001") || info.Owner.Username != "alice" {
		t.Fatalf("legacy snapshot owner = %+v", info.Owner)
	}
}
