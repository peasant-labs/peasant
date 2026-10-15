package testutil

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
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
// and its collectives the way Village does:
//   - content lands private, and an update keeps the transcript's audience;
//   - every share, approval, rejection, and withdrawal is an event in the
//     share history of one (transcript, collective) pair, and the pair's
//     current row follows its latest event (pending, approved, or rejected);
//   - a curated collective holds a share for its owner's approval, and a
//     share with a collective the caller is not a member of is skipped
//     without an error;
//   - a share that finds a live (pending or approved) row answers 409;
//   - the transcript read lists every current row in "shares" and only the
//     approved rows, with their status, in "enriched_shares";
//   - taking a transcript back records a withdrawal and removes the row.
//
// It answers the publish, the schema version, the collective reads and share
// changes, the transcript read, the share history, the waiting pull
// requests, and the annotation push. A transcript's identifier is its
// session's, so every session publishes to its own transcript.
type CollectiveVillage struct {
	server      *httptest.Server
	t           testing.TB
	collectives map[schema.VillageUUID]VillageCollective
	order       []schema.VillageUUID

	mu sync.Mutex
	// transcripts holds each transcript's current share rows by collective.
	transcripts map[schema.TranscriptID]map[schema.VillageUUID]schema.VillageShareStatus
	// events holds each (transcript, collective) pair's share history.
	events               map[schema.TranscriptID]map[schema.VillageUUID][]schema.VillageShareEvent
	public               map[schema.TranscriptID]bool
	shareStall           time.Duration
	publishes            []schema.AuthoritativePublishRequest
	ownerUpdates         int
	ownerVisibilities    []schema.TranscriptUpdateVisibility
	failPublish          bool
	failShare            map[schema.VillageUUID]bool
	conflictShare        map[schema.VillageUUID]bool
	failShareRead        bool
	failTranscriptRead   bool
	transcriptReads      int
	requiresNewerPeasant bool
	collectivesStatus    int
	repositoriesStatus   int
	promptRequests       []schema.VillagePromptRequest
	// pullBlobs holds each pullable transcript's served blob bytes, keyed by
	// transcript. pullAnnotations holds its authored annotations, and pullInfos
	// holds the metadata view the listing and the single-transcript GET serve.
	// A transcript with pull data is treated as owned and therefore pullable
	// (unless public, which the pull surface excludes); a filtered listing
	// further requires an APPROVED share into a listed member collective.
	pullBlobs       map[schema.TranscriptID][]byte
	pullAnnotations map[schema.TranscriptID][]schema.PullAnnotation
	pullInfos       map[schema.TranscriptID]schema.PullTranscriptInfo
	// pullListStatus, when non-zero, makes the pull listing answer with that
	// status instead of the list. skipGateStatus does the same for the
	// skip-gate. failPullContent makes the content GET of each listed
	// transcript answer 500. 0 / empty restores the happy answers.
	pullListStatus  int
	skipGateStatus  int
	failPullContent map[schema.TranscriptID]bool
	// pullListCalls counts pull-listing reads. skipGateCalls counts skip-gate
	// questions. contentCalls counts content GETs per transcript, so a test can
	// prove a pruned transcript was never downloaded.
	pullListCalls int
	skipGateCalls int
	contentCalls  map[schema.TranscriptID]int
}

// NewCollectiveVillage starts a CollectiveVillage that knows the collectives,
// in order. It closes when the test ends.
func NewCollectiveVillage(t testing.TB, collectives ...VillageCollective) *CollectiveVillage {
	t.Helper()
	v := &CollectiveVillage{
		t:               t,
		collectives:     map[schema.VillageUUID]VillageCollective{},
		transcripts:     map[schema.TranscriptID]map[schema.VillageUUID]schema.VillageShareStatus{},
		events:          map[schema.TranscriptID]map[schema.VillageUUID][]schema.VillageShareEvent{},
		public:          map[schema.TranscriptID]bool{},
		failShare:       map[schema.VillageUUID]bool{},
		conflictShare:   map[schema.VillageUUID]bool{},
		pullBlobs:       map[schema.TranscriptID][]byte{},
		pullAnnotations: map[schema.TranscriptID][]schema.PullAnnotation{},
		pullInfos:       map[schema.TranscriptID]schema.PullTranscriptInfo{},
		failPullContent: map[schema.TranscriptID]bool{},
		contentCalls:    map[schema.TranscriptID]int{},
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

// FailPublish makes every publish answer 500, so Village keeps the content
// and readers it had.
func (v *CollectiveVillage) FailPublish(fail bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.failPublish = fail
}

// ConflictShare makes sharing any transcript with the collective answer 409
// without recording anything, as Village does when another request shared it
// at the same moment.
func (v *CollectiveVillage) ConflictShare(id schema.VillageUUID) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.conflictShare[id] = true
}

// FailShareRead makes the share history read answer 500.
func (v *CollectiveVillage) FailShareRead(fail bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.failShareRead = fail
}

// FailTranscriptRead makes the owned transcript read fail closed.
func (v *CollectiveVillage) FailTranscriptRead(fail bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.failTranscriptRead = fail
}

// TranscriptReads is the number of visibility and share-list reads.
func (v *CollectiveVillage) TranscriptReads() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.transcriptReads
}

// RequireNewerPeasant makes the double advertise a push contract window that
// starts above this build's, so a push stops before it sends anything.
func (v *CollectiveVillage) RequireNewerPeasant(required bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.requiresNewerPeasant = required
}

// DeleteTranscript removes a transcript and its shares, as its owner can on
// Village, so every later read or share change of it answers 404.
func (v *CollectiveVillage) DeleteTranscript(transcript schema.TranscriptID) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.transcripts, transcript)
	delete(v.events, transcript)
	delete(v.pullBlobs, transcript)
	delete(v.pullAnnotations, transcript)
	delete(v.pullInfos, transcript)
}

// AnswerRepositories makes every collective's linked repositories read
// answer with the status instead of the list; 0 restores the lists.
func (v *CollectiveVillage) AnswerRepositories(status int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.repositoriesStatus = status
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

// SetPullTranscript stores a pullable transcript's served blob and its authored
// annotations, deriving the metadata view the pull surface serves (the content
// hash over the blob bytes, the annotation count, and the group visibility a
// shared transcript carries). A transcript with pull data is treated as owned
// and therefore pullable unless public; a filtered pull listing further
// requires an APPROVED share into a listed member collective (see Share).
func (v *CollectiveVillage) SetPullTranscript(transcript schema.TranscriptID, blob []byte, annotations []schema.PullAnnotation) {
	v.mu.Lock()
	defer v.mu.Unlock()
	cp := append([]byte(nil), blob...)
	anns := append([]schema.PullAnnotation(nil), annotations...)
	v.pullBlobs[transcript] = cp
	v.pullAnnotations[transcript] = anns
	hash := schema.ComputeTranscriptHash(cp)
	v.pullInfos[transcript] = schema.PullTranscriptInfo{
		TranscriptID:    transcript,
		OwnerUserID:     TestOwnerUserID,
		OwnerUsername:   TestOwnerUsername,
		Title:           "pull title for " + transcript.String(),
		Harness:         schema.HarnessClaudeCode,
		Visibility:      schema.VisibilityGroup,
		ContentHash:     hash,
		ContractVersion: defaults.PublishSchemaVersion,
		PublishedAt:     villageDoubleTime.UnixMilli(),
		UpdatedAt:       villageDoubleTime.UnixMilli(),
		AnnotationCount: len(anns),
	}
}

// AnswerPullList makes the pull listing answer with the status instead of the
// list; 0 restores the list.
func (v *CollectiveVillage) AnswerPullList(status int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.pullListStatus = status
}

// AnswerSkipGate makes the pull skip-gate answer with the status instead of the
// currency answers; 0 restores the answers. A non-2xx status exercises the
// bulk driver's skip-gate fallback: every transcript is then pulled
// individually.
func (v *CollectiveVillage) AnswerSkipGate(status int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.skipGateStatus = status
}

// FailPullContent makes the content GET of the transcript answer 500, so a bulk
// pull records one failure and continues with the others. False restores the
// blob.
func (v *CollectiveVillage) FailPullContent(transcript schema.TranscriptID, fail bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if fail {
		v.failPullContent[transcript] = true
		return
	}
	delete(v.failPullContent, transcript)
}

// PullListCalls is the number of pull-listing reads received.
func (v *CollectiveVillage) PullListCalls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.pullListCalls
}

// SkipGateCalls is the number of skip-gate questions received.
func (v *CollectiveVillage) SkipGateCalls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.skipGateCalls
}

// ContentCalls is the number of content GETs received for the transcript, so a
// test can prove a skip-gate-pruned transcript was never downloaded.
func (v *CollectiveVillage) ContentCalls(transcript schema.TranscriptID) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.contentCalls[transcript]
}

// Share records a share event of the transcript in the collective, as its
// owner or the collective could have made on Village: pending, approved, or
// rejected.
func (v *CollectiveVillage) Share(transcript schema.TranscriptID, id schema.VillageUUID, status schema.VillageShareStatus) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.recordLocked(transcript, id, status)
}

// recordLocked appends one share event and moves the pair's current row to
// it, the way Village derives transcript_shares from its latest attempt.
func (v *CollectiveVillage) recordLocked(transcript schema.TranscriptID, id schema.VillageUUID, status schema.VillageShareStatus) {
	if v.events[transcript] == nil {
		v.events[transcript] = map[schema.VillageUUID][]schema.VillageShareEvent{}
	}
	history := v.events[transcript][id]
	v.events[transcript][id] = append(history, schema.VillageShareEvent{EventNum: int32(len(history) + 1), Status: status, RecordedAt: villageDoubleTime})
	if v.transcripts[transcript] == nil {
		v.transcripts[transcript] = map[schema.VillageUUID]schema.VillageShareStatus{}
	}
	switch status {
	case schema.VillageShareStatusPending, schema.VillageShareStatusApproved, schema.VillageShareStatusRejected:
		v.transcripts[transcript][id] = status
	default:
		delete(v.transcripts[transcript], id)
	}
}

// StallShare makes every share answer only after d, as a Village that
// accepted the connection and stopped answering would.
func (v *CollectiveVillage) StallShare(d time.Duration) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.shareStall = d
}

// SetPublic makes the transcript public on the double, or private again, as
// its owner could on Village.
func (v *CollectiveVillage) SetPublic(transcript schema.TranscriptID, public bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.public[transcript] = public
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

// OwnerVisibilities returns visibility changes actually requested by the CLI.
// This double deliberately refuses owner updates rather than applying them.
func (v *CollectiveVillage) OwnerVisibilities() []schema.TranscriptUpdateVisibility {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]schema.TranscriptUpdateVisibility(nil), v.ownerVisibilities...)
}

// Public reports the actual audience state retained by the Village double,
// including owner visibility updates after content publication.
func (v *CollectiveVillage) Public(transcript schema.TranscriptID) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.public[transcript]
}

// Audience returns the collectives that can read the transcript or will once
// their owner approves it: its approved and pending share rows.
func (v *CollectiveVillage) Audience(transcript schema.TranscriptID) map[schema.VillageUUID]schema.VillageShareStatus {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := map[schema.VillageUUID]schema.VillageShareStatus{}
	for id, status := range v.transcripts[transcript] {
		if status == schema.VillageShareStatusApproved || status == schema.VillageShareStatusPending {
			out[id] = status
		}
	}
	return out
}

func (v *CollectiveVillage) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/api/v1/schema/version":
		window := schema.SchemaVersionResponse{
			MinPushContractVersion: "0.1.0",
			PushContractVersion:    defaults.PublishSchemaVersion,
			MinPullContractVersion: defaults.MinPullContractVersion,
			PullContractVersion:    defaults.PullContractVersion,
			ContentCapabilities:    schema.AllContentCapabilities,
		}
		v.mu.Lock()
		if v.requiresNewerPeasant {
			window.MinPushContractVersion, window.PushContractVersion = "99.0.0", "99.0.0"
			window.MinPullContractVersion, window.PullContractVersion = "99.0.0", "99.0.0"
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
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/users/me/collectives/") && strings.HasSuffix(path, "/events"):
		v.readEvents(w, strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/users/me/collectives/"), "/events"))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/groups/") && strings.HasSuffix(path, "/repositories"):
		v.listRepositories(w, schema.VillageUUID(strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/groups/"), "/repositories")))
	case r.Method == http.MethodGet && path == "/api/v1/annotations/manifest":
		_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
	case r.Method == http.MethodPost && path == "/api/v1/annotations":
		_ = json.NewEncoder(w).Encode(schema.AnnotationPushResponse{})
	case r.Method == http.MethodGet && path == "/api/v1/pull/transcripts":
		v.listPullable(w, r)
	case r.Method == http.MethodPost && path == "/api/v1/pull/transcripts/skip-gate":
		v.skipGate(w, r)
	case strings.HasPrefix(path, "/api/v1/pull/transcripts/"):
		v.pullTranscript(w, r, strings.TrimPrefix(path, "/api/v1/pull/transcripts/"))
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
		var request schema.OwnerTranscriptUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			v.t.Errorf("decode owner update: %v", err)
		}
		v.mu.Lock()
		v.ownerUpdates++
		if request.Visibility != nil {
			v.ownerVisibilities = append(v.ownerVisibilities, *request.Visibility)
		}
		v.mu.Unlock()
		http.Error(w, `{"error":"this double does not apply owner updates"}`, http.StatusInternalServerError)
	case r.Method == http.MethodPost && share == "share":
		v.share(w, r, transcript)
	case r.Method == http.MethodDelete && strings.HasPrefix(share, "share/"):
		// Village withdraws a live share and answers the same whether or not
		// one was live.
		id := schema.VillageUUID(strings.TrimPrefix(share, "share/"))
		v.mu.Lock()
		if _, exists := v.transcripts[transcript]; !exists {
			v.mu.Unlock()
			http.Error(w, `{"error":"Transcript not found"}`, http.StatusNotFound)
			return
		}
		if status, live := v.transcripts[transcript][id]; live && (status == schema.VillageShareStatusPending || status == schema.VillageShareStatusApproved) {
			v.recordLocked(transcript, id, schema.VillageShareStatusRetracted)
		}
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
	if v.failPublish {
		http.Error(w, `{"error":"Failed to store the transcript"}`, http.StatusInternalServerError)
		return
	}
	shares, exists := v.transcripts[transcript]
	if !exists {
		v.transcripts[transcript] = map[schema.VillageUUID]schema.VillageShareStatus{}
	}
	receipt.TranscriptID = transcript
	receipt.TranscriptURL = "https://village.example/transcripts/" + transcript.String()
	receipt.Created = !exists
	// Each publish is later than the one before, as Village's clock is, so
	// "the transcript published last" is observable.
	receipt.UpdatedAt += int64(len(v.publishes))
	// Content lands private; a transcript shared with a collective keeps that
	// audience when it is updated.
	switch {
	case v.public[transcript]:
		receipt.Visibility = schema.VisibilityPublic
		receipt.Applied.NormalizedValues.Visibility = schema.VisibilityPublic
	case hasLiveShare(shares):
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
	v.mu.Lock()
	stall := v.shareStall
	v.mu.Unlock()
	if stall > 0 {
		// The body is read, so the server notices the client giving up.
		select {
		case <-time.After(stall):
		case <-r.Context().Done():
			return
		}
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
	if v.conflictShare[id] {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(schema.VillageErrorResponse{Error: "Transcript was submitted to this collective by another request at the same moment as this one. Nothing was submitted to that collective. Share it again."})
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
		v.recordLocked(transcript, id, status)
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
	v.transcriptReads++
	if v.failTranscriptRead {
		http.Error(w, `{"error":"Transcript read unavailable"}`, http.StatusInternalServerError)
		return
	}
	shares, exists := v.transcripts[transcript]
	if !exists {
		http.Error(w, `{"error":"Transcript not found"}`, http.StatusNotFound)
		return
	}
	rows := make([]schema.VillageTranscriptShare, 0, len(shares))
	approved := make([]schema.VillageEnrichedTranscriptShare, 0, len(shares))
	for id, status := range shares {
		rows = append(rows, schema.VillageTranscriptShare{GroupID: id, GroupName: v.collectives[id].Name, SharedAt: villageDoubleTime})
		if status == schema.VillageShareStatusApproved {
			approved = append(approved, schema.VillageEnrichedTranscriptShare{TranscriptID: transcript, GroupID: id, GroupName: v.collectives[id].Name, AcceptanceMode: v.collectives[id].Acceptance, Status: status, SharedAt: villageDoubleTime})
		}
	}
	// A fixed order that is not the collectives' name order, so a caller
	// that depends on the order it receives is caught.
	sort.Slice(rows, func(i, j int) bool { return rows[i].GroupID > rows[j].GroupID })
	sort.Slice(approved, func(i, j int) bool { return approved[i].GroupID > approved[j].GroupID })
	visibility := schema.VillageTranscriptVisibilityPrivate
	switch {
	case v.public[transcript]:
		visibility = schema.VillageTranscriptVisibilityPublic
	case hasLiveShare(shares):
		visibility = schema.VillageTranscriptVisibilityShared
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"transcript": map[string]any{"visibility": visibility}, "shares": rows, "enriched_shares": approved})
}

// readEvents answers the share history of one pair, named by the path
// "{groupId}/transcripts/{transcriptId}", oldest event first.
func (v *CollectiveVillage) readEvents(w http.ResponseWriter, pair string) {
	group, transcript, _ := strings.Cut(pair, "/transcripts/")
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.failShareRead {
		http.Error(w, `{"error":"Failed to load the share-event history"}`, http.StatusInternalServerError)
		return
	}
	if _, exists := v.transcripts[schema.TranscriptID(transcript)]; !exists {
		http.Error(w, `{"error":"Transcript not found"}`, http.StatusNotFound)
		return
	}
	history := append([]schema.VillageShareEvent{}, v.events[schema.TranscriptID(transcript)][schema.VillageUUID(group)]...)
	_ = json.NewEncoder(w).Encode(history)
}

func hasLiveShare(shares map[schema.VillageUUID]schema.VillageShareStatus) bool {
	for _, status := range shares {
		if status == schema.VillageShareStatusApproved || status == schema.VillageShareStatusPending {
			return true
		}
	}
	return false
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
	v.mu.Lock()
	status := v.repositoriesStatus
	v.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(schema.VillageErrorResponse{Error: http.StatusText(status)})
		return
	}
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

// pullableLocked reports whether the double holds pull data for the transcript
// and may serve it: public transcripts are excluded, exactly as the pull
// surface excludes them. A held, private transcript is treated as owned and
// therefore pullable; a filtered listing further requires an APPROVED share
// into a listed member collective. The caller must hold v.mu.
func (v *CollectiveVillage) pullableLocked(transcript schema.TranscriptID) bool {
	if _, ok := v.pullInfos[transcript]; !ok {
		return false
	}
	if v.public[transcript] {
		return false
	}
	return true
}

// matchesPullFilterLocked reports whether the transcript counts for a pull
// listing narrowed to filter. An empty filter lists everything pullable; a
// listed collective counts only when the caller is a member of it and the
// transcript holds an APPROVED share there. The caller must hold v.mu.
func (v *CollectiveVillage) matchesPullFilterLocked(transcript schema.TranscriptID, filter []schema.VillageUUID) bool {
	if !v.pullableLocked(transcript) {
		return false
	}
	if len(filter) == 0 {
		return true
	}
	shares := v.transcripts[transcript]
	for _, id := range filter {
		collective, known := v.collectives[id]
		if !known || !collective.Member {
			continue
		}
		if shares[id] == schema.VillageShareStatusApproved {
			return true
		}
	}
	return false
}

// listPullable answers GET /api/v1/pull/transcripts with the page, limit, and
// comma-separated collectives filter the village declares. The rows are the
// transcripts the double holds that the filter admits, in transcript-ID order,
// paged by (page-1)*limit. Total is the admitted count, so a caller pages to
// the end and de-duplicates client-side when a transcript matches several
// listed collectives.
func (v *CollectiveVillage) listPullable(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	status := v.pullListStatus
	v.pullListCalls++
	v.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(schema.VillageErrorResponse{Error: http.StatusText(status)})
		return
	}
	query := r.URL.Query()
	page, err := strconv.Atoi(query.Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	limit, err := strconv.Atoi(query.Get("limit"))
	if err != nil || limit < 1 {
		limit = 50
	}
	var filter []schema.VillageUUID
	if raw := strings.TrimSpace(query.Get("collectives")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				filter = append(filter, schema.VillageUUID(part))
			}
		}
	}
	v.mu.Lock()
	matched := make([]schema.PullTranscriptInfo, 0)
	for id, info := range v.pullInfos {
		if v.matchesPullFilterLocked(id, filter) {
			matched = append(matched, info)
		}
	}
	v.mu.Unlock()
	sort.Slice(matched, func(i, j int) bool { return matched[i].TranscriptID < matched[j].TranscriptID })
	total := len(matched)
	start := (page - 1) * limit
	var pageRows []schema.PullTranscriptInfo
	if start < total {
		end := start + limit
		if end > total {
			end = total
		}
		pageRows = matched[start:end]
	}
	if pageRows == nil {
		pageRows = []schema.PullTranscriptInfo{}
	}
	_ = json.NewEncoder(w).Encode(schema.PullListResponse{Transcripts: pageRows, Page: page, Limit: limit, Total: total})
}

// pullAnnotationHashesLocked returns the content-hash set the double holds for
// the transcript's annotations, using the pipeline's dedup-key rule: the
// annotation's own content hash when present, else a hash of its stable ID.
// The caller must hold v.mu.
func (v *CollectiveVillage) pullAnnotationHashesLocked(transcript schema.TranscriptID) []string {
	anns := v.pullAnnotations[transcript]
	out := make([]string, 0, len(anns))
	for i := range anns {
		a := &anns[i]
		if a.ContentHash != nil && *a.ContentHash != "" {
			out = append(out, *a.ContentHash)
			continue
		}
		out = append(out, schema.ComputeTranscriptHash([]byte(a.ID)))
	}
	sort.Strings(out)
	return out
}

// skipGate answers POST /api/v1/pull/transcripts/skip-gate. An id is current
// exactly when the caller's sent content hash equals the held served-blob hash
// and the sent annotation-hash set equals the held set. An id the caller may
// not pull — unknown to the double or public — is ABSENT from Results, withheld
// by omission rather than answered.
func (v *CollectiveVillage) skipGate(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	status := v.skipGateStatus
	v.skipGateCalls++
	v.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(schema.VillageErrorResponse{Error: http.StatusText(status)})
		return
	}
	var request schema.PullSkipGateRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		v.t.Errorf("decode skip-gate request: %v", err)
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	results := make([]schema.PullSkipGateResult, 0, len(request.Items))
	for _, item := range request.Items {
		info, ok := v.pullInfos[item.TranscriptID]
		if !ok || v.public[item.TranscriptID] {
			continue
		}
		contentCurrent := item.ContentHash == info.ContentHash
		held := v.pullAnnotationHashesLocked(item.TranscriptID)
		sent := append([]string(nil), item.AnnotationHashes...)
		sort.Strings(sent)
		annotationsCurrent := len(sent) == len(held)
		if annotationsCurrent {
			for i := range sent {
				if sent[i] != held[i] {
					annotationsCurrent = false
					break
				}
			}
		}
		results = append(results, schema.PullSkipGateResult{
			TranscriptID:       item.TranscriptID,
			ContentCurrent:     contentCurrent,
			AnnotationsCurrent: annotationsCurrent,
		})
	}
	_ = json.NewEncoder(w).Encode(schema.NewPullSkipGateResponse(results))
}

// pullTranscript answers the single-transcript pull GETs the pipeline drives:
// GET /api/v1/pull/transcripts/{id}, /{id}/content, and /{id}/annotations. A
// transcript the caller may not pull answers 404, never 403, so existence never
// leaks.
func (v *CollectiveVillage) pullTranscript(w http.ResponseWriter, r *http.Request, rest string) {
	id, tail, _ := strings.Cut(rest, "/")
	transcript := schema.TranscriptID(id)
	v.mu.Lock()
	info, ok := v.pullInfos[transcript]
	blob := v.pullBlobs[transcript]
	anns := append([]schema.PullAnnotation(nil), v.pullAnnotations[transcript]...)
	failContent := v.failPullContent[transcript]
	pullable := ok && !v.public[transcript]
	if r.Method == http.MethodGet && tail == "content" {
		v.contentCalls[transcript]++
	}
	v.mu.Unlock()
	if r.Method != http.MethodGet {
		v.t.Errorf("unexpected Village pull request %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"error":"unexpected"}`, http.StatusNotFound)
		return
	}
	if !pullable {
		http.Error(w, `{"error":"Transcript not found"}`, http.StatusNotFound)
		return
	}
	switch tail {
	case "":
		_ = json.NewEncoder(w).Encode(info)
	case "content":
		if failContent {
			http.Error(w, `{"error":"content unavailable"}`, http.StatusInternalServerError)
			return
		}
		etag := `"` + info.ContentHash + `"`
		if match := r.Header.Get("If-None-Match"); match != "" && (match == etag || match == info.ContentHash || match == "W/"+etag) {
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write(blob)
	case "annotations":
		if anns == nil {
			anns = []schema.PullAnnotation{}
		}
		_ = json.NewEncoder(w).Encode(anns)
	default:
		v.t.Errorf("unexpected Village pull request %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"error":"unexpected"}`, http.StatusNotFound)
	}
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
