package village

import (
	"context"
	_ "embed"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/observation_reads.yaml
var observationReadYAML []byte

type observationReadFixture struct {
	Name              string                                   `yaml:"name"`
	Method            string                                   `yaml:"method"`
	Path              string                                   `yaml:"path"`
	Body              string                                   `yaml:"body"`
	Status            int                                      `yaml:"status"`
	PaddingBytes      int                                      `yaml:"paddingBytes"`
	Invalid           bool                                     `yaml:"invalid"`
	WantUser          *observationUserExpectation              `yaml:"wantUser"`
	WantAssociations  []observationAssociationExpectation      `yaml:"wantAssociations"`
	WantSubmissions   []observationSubmissionExpectation       `yaml:"wantSubmissions"`
	WantMemberships   []schema.VillageUserGroup                `yaml:"wantMemberships"`
	WantCollective    *schema.VillageGroupDetailRecord         `yaml:"wantCollective"`
	WantEvents        []schema.VillageShareEvent               `yaml:"wantEvents"`
	WantOrganizations []schema.VillageMetadataUserOrganization `yaml:"wantOrganizations"`
}

type observationUserExpectation struct {
	ID       schema.VillageUUID `yaml:"id"`
	Username string             `yaml:"username"`
}

type observationAssociationExpectation struct {
	ID              schema.VillageUUID `yaml:"id"`
	Name            string             `yaml:"name"`
	Description     *string            `yaml:"description"`
	LinkedGithubOrg *string            `yaml:"linkedGithubOrg"`
	SharedAt        time.Time          `yaml:"sharedAt"`
}

type observationSubmissionExpectation struct {
	CollectiveID schema.VillageUUID        `yaml:"collectiveId"`
	TranscriptID schema.TranscriptID       `yaml:"transcriptId"`
	Title        *string                   `yaml:"title"`
	EventNum     int32                     `yaml:"eventNum"`
	Status       schema.VillageShareStatus `yaml:"status"`
	RecordedAt   time.Time                 `yaml:"recordedAt"`
}

func loadObservationReadFixtures(t *testing.T) []observationReadFixture {
	t.Helper()
	var data struct {
		Cases []observationReadFixture `yaml:"cases"`
	}
	if err := yaml.Unmarshal(observationReadYAML, &data); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	statuses := map[schema.VillageShareStatus]bool{}
	for _, fixture := range data.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatal("missing/duplicate observation fixture name")
		}
		names[fixture.Name] = true
		if !fixture.Invalid {
			switch fixture.Method {
			case "user":
				if fixture.WantUser == nil {
					t.Fatalf("missing returned user expectation: %s", fixture.Name)
				}
			case "associations":
				if fixture.WantAssociations == nil {
					t.Fatalf("missing complete association expectation: %s", fixture.Name)
				}
			case "submissions":
				if fixture.WantSubmissions == nil {
					t.Fatalf("missing complete submission expectation: %s", fixture.Name)
				}
				for _, row := range fixture.WantSubmissions {
					statuses[row.Status] = true
				}
			case "memberships":
				if fixture.WantMemberships == nil {
					t.Fatalf("missing complete membership expectation: %s", fixture.Name)
				}
			case "collective":
				if fixture.WantCollective == nil {
					t.Fatalf("missing collective projection expectation: %s", fixture.Name)
				}
			case "events":
				if fixture.WantEvents == nil {
					t.Fatalf("missing complete attempt expectation: %s", fixture.Name)
				}
			case "organizations":
				if fixture.WantOrganizations == nil {
					t.Fatalf("missing organization input expectation: %s", fixture.Name)
				}
			default:
				t.Fatalf("unknown successful source method: %s", fixture.Method)
			}
		}
	}
	wantStatuses := map[schema.VillageShareStatus]bool{}
	for _, status := range schema.AllVillageShareStatuses {
		wantStatuses[status] = true
	}
	if !reflect.DeepEqual(statuses, wantStatuses) {
		t.Fatalf("supported submission status membership = %v, want exact schema set %v", statuses, wantStatuses)
	}
	const required = "verified-stable-user-uuid username-without-stable-id-refused foreign-viewer-dedicated-associations-only known-empty-complete-associations missing-associations-not-empty null-associations-not-empty repeated-association-refused known-association-nondisclosure-preserves-status available-owned-pair-discovery approved-owned-pair-discovery rejected-owned-pair-discovery retracted-owned-pair-discovery revoked-owned-pair-discovery cross-collective-pair-refused unknown-submission-status-refused null-discovery-not-complete complete-known-empty-discovery truncated-valid-json-prefix trailing-second-json-document-refused"
	for _, name := range strings.Fields(required) {
		if !names[name] {
			t.Fatalf("required fixture %s missing", name)
		}
	}
	const requiredSources = "complete-membership-input empty-membership-input null-membership-not-complete repeated-membership-refused unknown-membership-policy-refused permitted-detail-projection detail-wrong-identity-refused detail-unavailable-not-empty complete-own-attempts complete-legacy-rejected-unknown-decision-history retracted-unknown-decision-refused revoked-unknown-decision-refused empty-own-attempts null-attempts-not-complete repeated-attempt-ordinal-refused malformed-attempt-actor-refused history-unavailable-not-empty complete-account-organizations empty-account-organizations null-organizations-not-complete repeated-organization-login-refused"
	for _, name := range strings.Fields(requiredSources) {
		if !names[name] {
			t.Fatalf("required source fixture %s missing", name)
		}
	}
	return data.Cases
}

func TestObservationSourceCompleteReads(t *testing.T) {
	for _, fixture := range loadObservationReadFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != fixture.Path || r.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Errorf("unexpected source request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				status := fixture.Status
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, fixture.Body)
				if fixture.PaddingBytes != 0 {
					_, _ = io.WriteString(w, strings.Repeat(" ", fixture.PaddingBytes))
				}
			}))
			defer server.Close()
			client := NewVillageClient(server.URL, "fixture-key", server.Client())
			var err error
			switch fixture.Method {
			case "user":
				var user schema.VillageUser
				user, err = client.VerifyUser(context.Background())
				if err == nil && fixture.WantUser != nil {
					got := observationUserExpectation{ID: user.ID, Username: user.GithubUsername}
					if got != *fixture.WantUser {
						t.Fatalf("returned user = %#v, want %#v", got, *fixture.WantUser)
					}
				}
			case "associations":
				var rows []schema.VillageTranscriptCollective
				rows, err = client.ReadTranscriptCollectives(context.Background(), schema.TranscriptID("22222222-2222-4222-8222-222222222222"))
				if err == nil {
					if rows == nil {
						t.Fatal("successful complete associations must be non-nil, including an empty set")
					}
					got := make([]observationAssociationExpectation, 0, len(rows))
					for _, row := range rows {
						got = append(got, observationAssociationExpectation{ID: row.ID, Name: row.Name, Description: row.Description, LinkedGithubOrg: row.LinkedGithubOrg, SharedAt: row.SharedAt})
					}
					if !reflect.DeepEqual(got, fixture.WantAssociations) {
						t.Fatalf("complete returned associations = %#v, want %#v", got, fixture.WantAssociations)
					}
				}
			case "submissions":
				var rows []schema.VillageCollectiveSubmission
				rows, err = client.ReadOwnSubmissions(context.Background(), schema.VillageUUID("33333333-3333-4333-8333-333333333333"))
				if err == nil {
					if rows == nil {
						t.Fatal("successful complete submissions must be non-nil, including an empty set")
					}
					got := make([]observationSubmissionExpectation, 0, len(rows))
					for _, row := range rows {
						got = append(got, observationSubmissionExpectation{CollectiveID: row.GroupID, TranscriptID: row.TranscriptID, Title: row.Title, EventNum: row.EventNum, Status: row.Status, RecordedAt: row.RecordedAt})
					}
					if !reflect.DeepEqual(got, fixture.WantSubmissions) {
						t.Fatalf("complete returned submissions = %#v, want %#v", got, fixture.WantSubmissions)
					}
				}
			case "memberships":
				var got []schema.VillageUserGroup
				got, err = client.ReadObservationMemberships(context.Background())
				if err == nil && (got == nil || !reflect.DeepEqual(got, fixture.WantMemberships)) {
					t.Fatalf("returned membership set differs from complete expected evidence: %#v", got)
				}
			case "collective":
				var got schema.VillageGroupDetailRecord
				got, err = client.ReadObservationCollective(context.Background(), schema.VillageUUID("33333333-3333-4333-8333-333333333333"))
				if err == nil && (fixture.WantCollective == nil || !reflect.DeepEqual(got, *fixture.WantCollective)) {
					t.Fatalf("returned collective projection differs from expected evidence: %#v", got)
				}
			case "events":
				var got []schema.VillageShareEvent
				got, err = client.ReadOwnShareEvents(context.Background(), schema.TranscriptID("22222222-2222-4222-8222-222222222222"), schema.VillageUUID("33333333-3333-4333-8333-333333333333"))
				if err == nil && (got == nil || !reflect.DeepEqual(got, fixture.WantEvents)) {
					t.Fatalf("returned attempt set differs from complete expected evidence: %#v", got)
				}
			case "organizations":
				var got []schema.VillageMetadataUserOrganization
				got, err = client.ReadObservationOrganizations(context.Background())
				if err == nil && (got == nil || !reflect.DeepEqual(got, fixture.WantOrganizations)) {
					t.Fatalf("returned organization set differs from complete expected evidence: %#v", got)
				}
			default:
				t.Fatalf("unknown method %s", fixture.Method)
			}
			if (err != nil) != fixture.Invalid {
				t.Fatalf("invalid=%v err=%v", fixture.Invalid, err)
			}
			if fixture.Status != 0 {
				var status *StatusError
				if !errors.As(err, &status) || status.StatusCode != fixture.Status {
					t.Fatalf("lost nondisclosure status: %v", err)
				}
			}
			if requests.Load() != 1 {
				t.Fatal("source read unexpectedly probed another endpoint")
			}
		})
	}
}

func TestObservationReadDoesNotForwardCredentialsOnRedirect(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(true)
		_, _ = io.WriteString(w, `{"id":"11111111-1111-4111-8111-111111111111"}`)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/other-installation/api/v1/auth/me", http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	_, err := NewVillageClient(source.URL, "fixture-key", source.Client()).VerifyUser(context.Background())
	var status *StatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusTemporaryRedirect || leaked.Load() {
		t.Fatalf("redirect followed or status lost: leak=%v err=%v", leaked.Load(), err)
	}
}
