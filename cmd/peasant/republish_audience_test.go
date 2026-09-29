package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// The corpus sits beside the push pipeline, which is what decides; only this
// package can reach all three doors, so it is read by repository path.
const (
	republishAudienceFixturePath  = "internal/push/testdata/republish-audience.yaml"
	republishAudienceManifestPath = "internal/push/testdata/republish-audience.manifest.yaml"
)

// audienceDoor is how a publication reaches the pipeline.
type audienceDoor string

const (
	// audienceDoorWeb is the Share wizard's request to the running local server.
	audienceDoorWeb audienceDoor = "web"
	// audienceDoorHook is the upload command a managed Git hook runs.
	audienceDoorHook audienceDoor = "hook"
	// audienceDoorCLI is `peasant village push`, with the case's flags.
	audienceDoorCLI audienceDoor = "cli"
)

var allAudienceDoors = []audienceDoor{audienceDoorWeb, audienceDoorHook, audienceDoorCLI}

// audienceLicense is a license the corpus names, or none. The explicit token
// keeps a forgotten key from reading as "no license".
type audienceLicense string

const audienceLicenseNone audienceLicense = "none"

func (l audienceLicense) valid() bool {
	return l == audienceLicenseNone || schema.License(l).IsValid()
}

func (l audienceLicense) license() schema.License {
	if l == audienceLicenseNone {
		return ""
	}
	return schema.License(l)
}

// audienceChange is what changes locally between the two publications.
type audienceChange string

const (
	// audienceChangeNone leaves the session exactly as it was published.
	audienceChangeNone audienceChange = "none"
	// audienceChangeContent changes the transcript content.
	audienceChangeContent audienceChange = "content"
	// audienceChangeAssociation adds a commit association: the publication
	// changes and its content does not.
	audienceChangeAssociation audienceChange = "association"
)

var allAudienceChanges = []audienceChange{audienceChangeNone, audienceChangeContent, audienceChangeAssociation}

type audienceRun struct {
	Door  audienceDoor `yaml:"door"`
	Flags []string     `yaml:"flags"`
	// FailVisibilityUpdates has the Village refuse this run's owner updates.
	FailVisibilityUpdates bool `yaml:"failVisibilityUpdates"`
}

type audienceExpectation struct {
	Failed            *bool               `yaml:"failed"`
	Published         *bool               `yaml:"published"`
	License           audienceLicense     `yaml:"license"`
	VisibilityUpdates []schema.Visibility `yaml:"visibilityUpdates"`
	Visibility        schema.Visibility   `yaml:"visibility"`
}

type audienceCase struct {
	Name       string `yaml:"name"`
	Configured struct {
		Visibility schema.Visibility `yaml:"visibility"`
		License    audienceLicense   `yaml:"license"`
	} `yaml:"configured"`
	First        audienceRun         `yaml:"first"`
	ExpectFirst  audienceExpectation `yaml:"expectFirst"`
	OwnerShares  *bool               `yaml:"ownerShares"`
	Change       audienceChange      `yaml:"change"`
	Update       audienceRun         `yaml:"update"`
	ExpectUpdate audienceExpectation `yaml:"expectUpdate"`
}

func loadRepublishAudienceFixture(t *testing.T) []audienceCase {
	t.Helper()
	root := testutil.ModuleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, republishAudienceFixturePath))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Cases []audienceCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(data, &document); err != nil {
		t.Fatalf("%s: %v", republishAudienceFixturePath, err)
	}
	manifestData, err := os.ReadFile(filepath.Join(root, republishAudienceManifestPath))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(manifestData, "republish audience")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(document.Cases))
	var keptByDoor []audienceDoor
	for _, c := range document.Cases {
		names = append(names, c.Name)
		for _, run := range []audienceRun{c.First, c.Update} {
			if !slices.Contains(allAudienceDoors, run.Door) {
				t.Fatalf("%s case %q names door %q; use web, hook, or cli", republishAudienceFixturePath, c.Name, run.Door)
			}
			if run.Door != audienceDoorCLI && len(run.Flags) > 0 {
				t.Fatalf("%s case %q gives the %s door flags; only the cli door takes them", republishAudienceFixturePath, c.Name, run.Door)
			}
		}
		if !c.Configured.Visibility.IsValid() || !c.Configured.License.valid() || c.OwnerShares == nil || !slices.Contains(allAudienceChanges, c.Change) {
			t.Fatalf("%s case %q needs a valid configured visibility and license (or none), an explicit ownerShares, and a change of none, content, or association", republishAudienceFixturePath, c.Name)
		}
		for label, expect := range map[string]audienceExpectation{"expectFirst": c.ExpectFirst, "expectUpdate": c.ExpectUpdate} {
			if expect.Failed == nil || expect.Published == nil || !expect.License.valid() || expect.VisibilityUpdates == nil || !expect.Visibility.IsValid() {
				t.Fatalf("%s case %q %s must state failed, published, license (or none), visibilityUpdates (or []), and visibility; a missing value would assert nothing", republishAudienceFixturePath, c.Name, label)
			}
			if !*expect.Published && (expect.License != audienceLicenseNone || len(expect.VisibilityUpdates) > 0) {
				t.Fatalf("%s case %q %s expects a license or an owner update from a run that publishes nothing", republishAudienceFixturePath, c.Name, label)
			}
		}
		if *c.OwnerShares && !*c.ExpectUpdate.Failed && c.ExpectUpdate.Visibility == schema.VisibilityGroup && len(c.ExpectUpdate.VisibilityUpdates) == 0 {
			keptByDoor = append(keptByDoor, c.Update.Door)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "republish audience"); err != nil {
		t.Fatal(err)
	}
	testutil.RequireClosedSetCoverage(t, "republish audience", "an update door that keeps a collective-only transcript collective-only", allAudienceDoors, keptByDoor)
	return document.Cases
}

// TestRepublishAudienceThroughEveryDoor publishes a session, lets its owner
// share the transcript with collectives on the Village, and publishes the
// session again, each time through a real door: the Share request to the
// running local server, the hook's upload command, or the CLI.
//
// An update used to move the transcript to whatever the door asked for: public
// from the Share wizard, which sends public on every push, and the configured
// default from a hook or the CLI. Either way the collectives lost access.
func TestRepublishAudienceThroughEveryDoor(t *testing.T) {
	t.Parallel()
	for _, c := range loadRepublishAudienceFixture(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			runRepublishAudienceCase(t, c)
		})
	}
}

const audienceSessionID = "abab1212-abab-4bab-8bab-abababababab"

func runRepublishAudienceCase(t *testing.T, c audienceCase) {
	dir := t.TempDir()
	village := newAudienceVillage(t)
	writeTestCredentialsFor(t, dir, village.server.URL)
	seedUploadableSession(t, dir, audienceSessionID)
	seedEntryCarrying(t, dir, audienceSessionID, "the first draft of this session")
	license := ""
	if c.Configured.License != audienceLicenseNone {
		license = "  license: " + string(c.Configured.License) + "\n"
	}
	cfgPath := writeCfg(t, dir, "audience.yaml", fmt.Sprintf("version: 1\noutput:\n  basePath: %s\npush:\n  method: all\n  visibility: %s\n%s",
		filepath.Join(dir, "peasant-sync"), c.Configured.Visibility, license))
	doors := startAudienceDoors(t, dir, cfgPath)

	village.refuseVisibilityUpdates(c.First.FailVisibilityUpdates)
	failed, said := doors.run(t, c.First)
	village.expect(t, "first publication", c.ExpectFirst, failed, said, doors)

	if *c.OwnerShares {
		village.shareWithCollectives()
	}
	switch c.Change {
	case audienceChangeContent:
		seedEntryCarrying(t, dir, audienceSessionID, "the second draft of this session")
	case audienceChangeAssociation:
		sessionID, err := ingest.NewSessionID(audienceSessionID)
		if err != nil {
			t.Fatal(err)
		}
		if err := doors.db.UpsertSessionCommits(t.Context(), sessionID, []ingest.CommitInfo{{Hash: strings.Repeat("c", 40), Message: "a commit the session made"}}); err != nil {
			t.Fatal(err)
		}
	}
	village.refuseVisibilityUpdates(c.Update.FailVisibilityUpdates)
	failed, said = doors.run(t, c.Update)
	village.expect(t, "update", c.ExpectUpdate, failed, said, doors)
}

// audienceDoors holds what the three doors need: the configuration and data
// directories the CLI and hook are bound to, and the local server the Share
// wizard talks to, sharing that same database.
type audienceDoors struct {
	dir, cfgPath string
	shareURL     string
	db           *store.Store
}

func startAudienceDoors(t *testing.T, dir, cfgPath string) *audienceDoors {
	t.Helper()
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := api.NewServer(api.ServerConfig{Port: 0, Store: db, Config: cfg, ConfigHome: dir, DataHome: dir, StateHome: dir})
	if err := server.Listen(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		_ = db.Close()
	})
	return &audienceDoors{dir: dir, cfgPath: cfgPath, shareURL: "http://" + server.Addr().String(), db: db}
}

// run publishes through one door and reports whether the door reported a
// failure, with what it said. The corpus says whether a failure is expected.
func (d *audienceDoors) run(t *testing.T, run audienceRun) (failed bool, said string) {
	t.Helper()
	switch run.Door {
	case audienceDoorWeb:
		// The body web/src/lib/share/push.ts sends. Bounded, because the gate
		// runs with no test timeout and a hung loopback request would hang it.
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		body := fmt.Sprintf(`{"sessionIds":[%q],"redactionLevel":"standard","visibility":"public"}`, audienceSessionID)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.shareURL+defaults.RouteSyncPush.String(), strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		var result struct {
			Errors int `json:"errors"`
		}
		if err == nil {
			err = json.Unmarshal(raw, &result)
		}
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("Share push: status=%d body=%s err=%v", response.StatusCode, raw, err)
		}
		return result.Errors > 0, string(raw)
	case audienceDoorHook:
		argv := githooks.RepositoryArgv("", githooks.Binding{ConfigPath: d.cfgPath, ConfigDir: d.dir, DataDir: d.dir, StateDir: d.dir, Timeout: time.Minute})
		return d.execute(argv[1:])
	default:
		args := []string{"--config", d.cfgPath, "--config-dir", d.dir, "--data-dir", d.dir, "--state-dir", d.dir, "village", "push", "--non-interactive"}
		return d.execute(append(args, run.Flags...))
	}
}

func (d *audienceDoors) execute(args []string) (failed bool, said string) {
	root := buildRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(args)
	err := root.Execute()
	return err != nil, fmt.Sprintf("peasant %s: err=%v\n%s", strings.Join(args, " "), err, &output)
}

// audienceVillage is a Village that keeps one transcript's audience the way the
// real one does: content lands private, a republish keeps the visibility the
// transcript had before it, a publish that sends no license keeps the license,
// and an identical operation changes nothing.
type audienceVillage struct {
	server *httptest.Server

	mu          sync.Mutex
	held        bool
	visibility  schema.Visibility
	license     *schema.License
	fingerprint schema.PublishRequestFingerprint
	published   []schema.License    // the license each publish request carried
	updates     []schema.Visibility // every owner visibility update received
	refuse      bool                // answer owner visibility updates with 503
	observedAt  struct{ published, updates int }
}

func newAudienceVillage(t *testing.T) *audienceVillage {
	t.Helper()
	v := &audienceVillage{}
	v.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/transcripts/publish"):
			v.publish(t, w, r)
		case r.Method == http.MethodPatch:
			v.update(t, w, r)
		case strings.Contains(r.URL.Path, "/annotations/manifest"):
			_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
		case strings.Contains(r.URL.Path, "/annotations"):
			_ = json.NewEncoder(w).Encode(schema.AnnotationPushResponse{})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(schema.SchemaVersionResponse{MinPushContractVersion: "0.1.0", PushContractVersion: defaults.PublishSchemaVersion, ContentCapabilities: []schema.ContentCapability{schema.ContentCapabilityObservedModelV1}})
		default:
			t.Errorf("unexpected Village request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(v.server.Close)
	return v
}

func (v *audienceVillage) publish(t *testing.T, w http.ResponseWriter, r *http.Request) {
	captured := &capturedPublish{parts: map[string]string{}}
	captured.record(t, r)
	request, err := schema.DecodeAuthoritativePublishRequest([]byte(captured.snapshot()["metadata"]))
	if err != nil {
		t.Errorf("decode publish request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	created := !v.held
	raw, err := testutil.AuthoritativePublishReceiptFromRequest(request, created)
	var receipt schema.AuthoritativePublishResponse
	if err == nil {
		err = json.Unmarshal(raw, &receipt)
	}
	if err != nil {
		t.Errorf("build receipt: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	v.published = append(v.published, request.License)
	if created {
		v.held, v.visibility = true, schema.VisibilityPrivate
	}
	// A new operation replaces the content. The transcript keeps its
	// visibility, and its license unless the request names one. The accepted
	// operation sent again changes nothing.
	if created || receipt.RequestOperationFingerprint != v.fingerprint {
		if request.License != "" {
			license := request.License
			v.license = &license
		}
		v.fingerprint = receipt.RequestOperationFingerprint
	}
	receipt.Visibility = v.visibility
	receipt.Applied.NormalizedValues.Visibility = v.visibility
	receipt.Applied.License = v.license
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(receipt)
}

func (v *audienceVillage) update(t *testing.T, w http.ResponseWriter, r *http.Request) {
	var request schema.OwnerTranscriptUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Visibility == nil {
		t.Errorf("owner update without a visibility: %v", err)
		http.Error(w, "bad owner update", http.StatusBadRequest)
		return
	}
	id, err := schema.NewTranscriptID(testutil.TestSessionUUID)
	if err != nil {
		t.Error(err)
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.updates = append(v.updates, schema.Visibility(*request.Visibility))
	if v.refuse {
		http.Error(w, `{"error":"owner update unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	v.visibility = schema.Visibility(*request.Visibility)
	_ = json.NewEncoder(w).Encode(schema.OwnerTranscriptUpdateResponse{TranscriptID: id, TranscriptURL: "https://village.example/transcripts/" + id.String(), Visibility: *request.Visibility, Tags: []string{}, UpdatedAt: 2})
}

// refuseVisibilityUpdates makes the Village answer owner visibility updates
// with a transient failure until it is called again with false.
func (v *audienceVillage) refuseVisibilityUpdates(refuse bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.refuse = refuse
}

// shareWithCollectives is the owner sharing the transcript with collectives on
// the Village, which leaves it readable by their members only.
func (v *audienceVillage) shareWithCollectives() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.visibility = schema.VisibilityGroup
}

// expect compares what the Village observed since the previous run with the
// corpus, and checks that the local receipt records the audience the Village
// reported.
func (v *audienceVillage) expect(t *testing.T, label string, want audienceExpectation, failed bool, said string, doors *audienceDoors) {
	t.Helper()
	if failed != *want.Failed {
		t.Errorf("%s: the door reported failed=%v; want %v\n%s", label, failed, *want.Failed, said)
	}
	v.mu.Lock()
	published := v.published[v.observedAt.published:]
	updates := v.updates[v.observedAt.updates:]
	visibility := v.visibility
	v.observedAt.published, v.observedAt.updates = len(v.published), len(v.updates)
	v.mu.Unlock()

	if got := len(published) > 0; got != *want.Published {
		t.Fatalf("%s: the Village received %d publish request(s); want published=%v", label, len(published), *want.Published)
	}
	if *want.Published && (len(published) != 1 || published[0] != want.License.license()) {
		t.Errorf("%s: publish requests carried licenses %q; want exactly one carrying %q", label, published, want.License.license())
	}
	if !slices.Equal(updates, want.VisibilityUpdates) {
		t.Errorf("%s: the Village received owner visibility updates %v; want %v", label, updates, want.VisibilityUpdates)
	}
	if visibility != want.Visibility {
		t.Errorf("%s: the transcript is %s on the Village; want %s", label, visibility, want.Visibility)
	}
	if !*want.Published || failed {
		return
	}
	input, err := doors.db.LoadPublicationInput(t.Context(), ingest.SessionID(audienceSessionID))
	if err != nil {
		t.Fatal(err)
	}
	hash, err := schema.NewProjectHash(string(input.ReceiptProjectHash))
	if err != nil {
		t.Fatal(err)
	}
	record, err := doors.db.Publication(t.Context(), v.server.URL, "user-00001", hash, audienceSessionID)
	if err != nil || record == nil {
		t.Fatalf("%s: no local receipt: %v", label, err)
	}
	if record.Receipt.Visibility != visibility {
		t.Errorf("%s: the local receipt records %s while the Village holds %s", label, record.Receipt.Visibility, visibility)
	}
}
