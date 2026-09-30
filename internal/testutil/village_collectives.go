package testutil

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// VillageCollective is one collective a CollectiveVillage knows.
type VillageCollective struct {
	ID         schema.VillageUUID
	Name       string
	Acceptance schema.VillageGroupAcceptanceMode
	// Member says the signed-in user belongs to it. Village skips a share with
	// a collective the user is not a member of, without an error.
	Member bool
	// LinkedGithubOrg is the GitHub organization it links, if any.
	LinkedGithubOrg *string
	// Repositories are the "owner/name" GitHub repositories it links.
	Repositories []string
}

// CollectiveVillage is a Village double that keeps each published transcript
// and its collectives the way Village does: content lands private, a curated
// collective holds a share for its owner's approval, a live share answers 409,
// and taking a transcript back removes the collective's access. It answers the
// publish, the schema version, the collective reads and share changes, the
// transcript read, the waiting pull requests, and the annotation push. A
// transcript's identifier is its session's, so every session publishes to its
// own transcript.
type CollectiveVillage struct {
	server      *httptest.Server
	t           testing.TB
	collectives map[schema.VillageUUID]VillageCollective
	order       []schema.VillageUUID

	mu                   sync.Mutex
	transcripts          map[schema.TranscriptID]map[schema.VillageUUID]schema.VillageShareStatus
	publishes            []schema.AuthoritativePublishRequest
	ownerUpdates         int
	failShare            map[schema.VillageUUID]bool
	failShareRead        bool
	requiresNewerPeasant bool
	collectivesStatus    int
	promptRequests       []schema.VillagePromptRequest
}

// NewCollectiveVillage starts a CollectiveVillage that knows the collectives,
// in order. It closes when the test ends.
func NewCollectiveVillage(t testing.TB, collectives ...VillageCollective) *CollectiveVillage {
	t.Helper()
	v := &CollectiveVillage{
		t:           t,
		collectives: map[schema.VillageUUID]VillageCollective{},
		transcripts: map[schema.TranscriptID]map[schema.VillageUUID]schema.VillageShareStatus{},
		failShare:   map[schema.VillageUUID]bool{},
	}
	for _, collective := range collectives {
		v.collectives[collective.ID] = collective
		v.order = append(v.order, collective.ID)
	}
	v.server = httptest.NewServer(http.HandlerFunc(v.serve))
	t.Cleanup(v.server.Close)
	return v
}

// URL is the double's base URL.
func (v *CollectiveVillage) URL() string { return v.server.URL }

// Close stops the double, so every later request fails to connect.
func (v *CollectiveVillage) Close() { v.server.Close() }

// FailShare makes sharing any transcript with the collective answer 500.
func (v *CollectiveVillage) FailShare(id schema.VillageUUID) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.failShare[id] = true
}

// FailShareRead makes the transcript read answer 500.
func (v *CollectiveVillage) FailShareRead(fail bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.failShareRead = fail
}

// RequireNewerPeasant makes the double advertise a push contract window that
// starts above this build's, so a push stops before it sends anything.
func (v *CollectiveVillage) RequireNewerPeasant(required bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.requiresNewerPeasant = required
}

// AnswerCollectives makes the collectives list answer with the status instead
// of the list; 0 restores the list.
func (v *CollectiveVillage) AnswerCollectives(status int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.collectivesStatus = status
}

// SetPromptRequests sets the caller's waiting pull requests.
func (v *CollectiveVillage) SetPromptRequests(requests ...schema.VillagePromptRequest) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.promptRequests = requests
}

// Share records a live share of the transcript with the collective, as its
// owner could have made on Village.
func (v *CollectiveVillage) Share(transcript schema.TranscriptID, id schema.VillageUUID, status schema.VillageShareStatus) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.transcripts[transcript] == nil {
		v.transcripts[transcript] = map[schema.VillageUUID]schema.VillageShareStatus{}
	}
	v.transcripts[transcript][id] = status
}

// Publishes returns every publish request received, in order.
func (v *CollectiveVillage) Publishes() []schema.AuthoritativePublishRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]schema.AuthoritativePublishRequest(nil), v.publishes...)
}

// OwnerUpdates is how many owner updates were received.
func (v *CollectiveVillage) OwnerUpdates() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.ownerUpdates
}

// Audience returns the transcript's current shares with their status.
func (v *CollectiveVillage) Audience(transcript schema.TranscriptID) map[schema.VillageUUID]schema.VillageShareStatus {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := map[schema.VillageUUID]schema.VillageShareStatus{}
	for id, status := range v.transcripts[transcript] {
		out[id] = status
	}
	return out
}

func (v *CollectiveVillage) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/api/v1/schema/version":
		window := schema.SchemaVersionResponse{MinPushContractVersion: "0.1.0", PushContractVersion: defaults.PublishSchemaVersion, ContentCapabilities: []schema.ContentCapability{schema.ContentCapabilityObservedModelV1}}
		v.mu.Lock()
		if v.requiresNewerPeasant {
			window.MinPushContractVersion, window.PushContractVersion = "99.0.0", "99.0.0"
		}
		v.mu.Unlock()
		_ = json.NewEncoder(w).Encode(window)
	case r.Method == http.MethodPost && path == "/api/v1/transcripts/publish":
		v.publish(w, r)
	case r.Method == http.MethodGet && path == "/api/v1/users/me/prompt-requests":
		v.mu.Lock()
		requests := append([]schema.VillagePromptRequest{}, v.promptRequests...)
		v.mu.Unlock()
		_ = json.NewEncoder(w).Encode(schema.VillagePromptRequestsResponse{Requests: requests})
	case r.Method == http.MethodGet && path == "/api/v1/groups":
		v.listCollectives(w)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/groups/") && strings.HasSuffix(path, "/repositories"):
		v.listRepositories(w, schema.VillageUUID(strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/groups/"), "/repositories")))
	case r.Method == http.MethodGet && path == "/api/v1/annotations/manifest":
		_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
	case r.Method == http.MethodPost && path == "/api/v1/annotations":
		_ = json.NewEncoder(w).Encode(schema.AnnotationPushResponse{})
	case strings.HasPrefix(path, "/api/v1/transcripts/"):
		v.transcript(w, r, strings.TrimPrefix(path, "/api/v1/transcripts/"))
	default:
		v.t.Errorf("unexpected Village request %s %s", r.Method, path)
		http.Error(w, `{"error":"unexpected"}`, http.StatusNotFound)
	}
}

func (v *CollectiveVillage) transcript(w http.ResponseWriter, r *http.Request, rest string) {
	id, share, _ := strings.Cut(rest, "/")
	transcript := schema.TranscriptID(id)
	switch {
	case r.Method == http.MethodGet && share == "":
		v.readShares(w, transcript)
	case r.Method == http.MethodPatch && share == "":
		v.mu.Lock()
		v.ownerUpdates++
		v.mu.Unlock()
		http.Error(w, `{"error":"this double does not apply owner updates"}`, http.StatusInternalServerError)
	case r.Method == http.MethodPost && share == "share":
		v.share(w, r, transcript)
	case r.Method == http.MethodDelete && strings.HasPrefix(share, "share/"):
		v.mu.Lock()
		delete(v.transcripts[transcript], schema.VillageUUID(strings.TrimPrefix(share, "share/")))
		v.mu.Unlock()
		_ = json.NewEncoder(w).Encode(schema.VillageStatusResponse{Status: "unshared"})
	default:
		v.t.Errorf("unexpected Village transcript request %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"error":"unexpected"}`, http.StatusNotFound)
	}
}

func (v *CollectiveVillage) publish(w http.ResponseWriter, r *http.Request) {
	request, err := schema.DecodeAuthoritativePublishRequest([]byte(multipartPart(r, "metadata")))
	if err != nil {
		v.t.Errorf("decode publish request: %v", err)
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	raw, err := AuthoritativePublishReceiptFromRequest(request, false)
	var receipt schema.AuthoritativePublishResponse
	if err == nil {
		err = json.Unmarshal(raw, &receipt)
	}
	if err != nil {
		v.t.Errorf("build receipt: %v", err)
		http.Error(w, `{"error":"receipt"}`, http.StatusInternalServerError)
		return
	}
	transcript := schema.TranscriptID(request.Identity.SessionID)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.publishes = append(v.publishes, request)
	shares, exists := v.transcripts[transcript]
	if !exists {
		v.transcripts[transcript] = map[schema.VillageUUID]schema.VillageShareStatus{}
	}
	receipt.TranscriptID = transcript
	receipt.TranscriptURL = "https://village.example/transcripts/" + transcript.String()
	receipt.Created = !exists
	// Content lands private; a transcript shared with a collective keeps that
	// audience when it is updated.
	if len(shares) > 0 {
		receipt.Visibility = schema.VisibilityGroup
		receipt.Applied.NormalizedValues.Visibility = schema.VisibilityGroup
	}
	if receipt.Created {
		w.WriteHeader(http.StatusCreated)
	}
	_ = json.NewEncoder(w).Encode(receipt)
}

func (v *CollectiveVillage) share(w http.ResponseWriter, r *http.Request, transcript schema.TranscriptID) {
	var request schema.VillageShareTranscriptRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.GroupIDs) != 1 {
		v.t.Errorf("share request %+v, %v: the client shares with one collective per request", request, err)
		http.Error(w, `{"error":"Invalid request body"}`, http.StatusBadRequest)
		return
	}
	id := request.GroupIDs[0]
	v.mu.Lock()
	defer v.mu.Unlock()
	shares, exists := v.transcripts[transcript]
	if !exists {
		http.Error(w, `{"error":"Transcript not found"}`, http.StatusNotFound)
		return
	}
	if v.failShare[id] {
		http.Error(w, `{"error":"Could not record the submission of this transcript"}`, http.StatusInternalServerError)
		return
	}
	if status, live := shares[id]; live && (status == schema.VillageShareStatusPending || status == schema.VillageShareStatusApproved) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(schema.VillageErrorResponse{Error: "This transcript is already submitted to 1 collective"})
		return
	}
	if collective, known := v.collectives[id]; known && collective.Member {
		status := schema.VillageShareStatusApproved
		if collective.Acceptance == schema.VillageGroupAcceptanceCurated {
			status = schema.VillageShareStatusPending
		}
		shares[id] = status
	}
	rows := make([]schema.VillageTranscriptShare, 0, len(shares))
	for shared := range shares {
		rows = append(rows, schema.VillageTranscriptShare{GroupID: shared, GroupName: v.collectives[shared].Name, SharedAt: villageDoubleTime})
	}
	_ = json.NewEncoder(w).Encode(rows)
}

func (v *CollectiveVillage) readShares(w http.ResponseWriter, transcript schema.TranscriptID) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.failShareRead {
		http.Error(w, `{"error":"Failed to read the transcript"}`, http.StatusInternalServerError)
		return
	}
	shares, exists := v.transcripts[transcript]
	if !exists {
		http.Error(w, `{"error":"Transcript not found"}`, http.StatusNotFound)
		return
	}
	rows := make([]schema.VillageEnrichedTranscriptShare, 0, len(shares))
	for id, status := range shares {
		rows = append(rows, schema.VillageEnrichedTranscriptShare{TranscriptID: transcript, GroupID: id, GroupName: v.collectives[id].Name, AcceptanceMode: v.collectives[id].Acceptance, Status: status, SharedAt: villageDoubleTime})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"enriched_shares": rows})
}

func (v *CollectiveVillage) listCollectives(w http.ResponseWriter) {
	v.mu.Lock()
	status := v.collectivesStatus
	v.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(schema.VillageErrorResponse{Error: http.StatusText(status)})
		return
	}
	groups := make([]schema.VillageUserGroup, 0, len(v.order))
	for _, id := range v.order {
		collective := v.collectives[id]
		if !collective.Member {
			continue
		}
		groups = append(groups, schema.VillageUserGroup{
			ID: collective.ID, Name: collective.Name, CreatedBy: "00000000-0000-4000-8000-000000000001",
			CreatedAt: villageDoubleTime, UpdatedAt: villageDoubleTime,
			AcceptanceMode: collective.Acceptance, DataAccess: schema.VillageGroupDataAccessMembersOnly,
			LinkedGithubOrg: collective.LinkedGithubOrg, TranscriptDeletionPolicy: schema.VillageTranscriptDeletionUserChoice,
			Role: schema.VillageGroupRoleMember, MemberSince: villageDoubleTime, MemberCount: 2,
		})
	}
	_ = json.NewEncoder(w).Encode(groups)
}

func (v *CollectiveVillage) listRepositories(w http.ResponseWriter, id schema.VillageUUID) {
	collective, known := v.collectives[id]
	if !known || !collective.Member {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(schema.VillageErrorResponse{Error: "Collective membership required"})
		return
	}
	repositories := make([]schema.VillageLinkedRepository, 0, len(collective.Repositories))
	for _, fullName := range collective.Repositories {
		owner, name, _ := strings.Cut(fullName, "/")
		repositories = append(repositories, schema.VillageLinkedRepository{ID: "00000000-0000-4000-8000-000000000002", GroupID: id, Owner: owner, Name: name, InstallationID: 1, LinkedBy: "00000000-0000-4000-8000-000000000001"})
	}
	_ = json.NewEncoder(w).Encode(schema.VillageLinkedRepositoriesResponse{Repositories: repositories})
}

// villageDoubleTime is the one time the double reports for its rows.
var villageDoubleTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func multipartPart(r *http.Request, name string) string {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return ""
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	for {
		part, err := reader.NextPart()
		if err != nil {
			return ""
		}
		if part.FormName() == name {
			data, _ := io.ReadAll(part)
			return string(data)
		}
	}
}
