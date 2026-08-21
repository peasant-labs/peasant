package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/mounted_selection_safety.yaml
var mountedSelectionSafetyYAML []byte

const (
	mountedSelectionCaseCount    = 8
	mountedSelectionSessionCount = 14
	mountedSelectionHostSlug     = "github.com-example-mounted-selection"
)

type mountedSelectionKind string

const (
	mountedSelectionClonePath       mountedSelectionKind = "clone-path"
	mountedSelectionExplicitSession mountedSelectionKind = "explicit-session"
	mountedSelectionRemote          mountedSelectionKind = "remote"
	mountedSelectionName            mountedSelectionKind = "name"
)

var allMountedSelectionKinds = []mountedSelectionKind{
	mountedSelectionClonePath,
	mountedSelectionExplicitSession,
	mountedSelectionRemote,
	mountedSelectionName,
}

type mountedSelectionRole string

const (
	mountedRoleSelectedClone          mountedSelectionRole = "selected-clone"
	mountedRoleSameRemoteSibling      mountedSelectionRole = "same-remote-sibling"
	mountedRoleExplicitSession        mountedSelectionRole = "explicit-session"
	mountedRoleExplicitSessionSibling mountedSelectionRole = "explicit-session-sibling"
	mountedRoleForceSelectedClone     mountedSelectionRole = "force-selected-clone"
	mountedRoleForceSameRemoteSibling mountedSelectionRole = "force-same-remote-sibling"
	mountedRoleAmbiguousRemoteCloneA  mountedSelectionRole = "ambiguous-remote-clone-a"
	mountedRoleAmbiguousRemoteCloneB  mountedSelectionRole = "ambiguous-remote-clone-b"
	mountedRoleUniqueRemote           mountedSelectionRole = "unique-remote"
	mountedRoleUniqueName             mountedSelectionRole = "unique-name"
	mountedRoleExactBranchDenied      mountedSelectionRole = "exact-branch-denied"
	mountedRoleSiblingBranchAllowed   mountedSelectionRole = "sibling-branch-allowed"
	mountedRoleProjectSessionDenied   mountedSelectionRole = "project-session-denied"
	mountedRoleProjectSessionAllowed  mountedSelectionRole = "project-session-allowed"
)

var allMountedSelectionRoles = []mountedSelectionRole{
	mountedRoleSelectedClone,
	mountedRoleSameRemoteSibling,
	mountedRoleExplicitSession,
	mountedRoleExplicitSessionSibling,
	mountedRoleForceSelectedClone,
	mountedRoleForceSameRemoteSibling,
	mountedRoleAmbiguousRemoteCloneA,
	mountedRoleAmbiguousRemoteCloneB,
	mountedRoleUniqueRemote,
	mountedRoleUniqueName,
	mountedRoleExactBranchDenied,
	mountedRoleSiblingBranchAllowed,
	mountedRoleProjectSessionDenied,
	mountedRoleProjectSessionAllowed,
}

var requiredMountedExclusionCases = []string{
	"auto new branches keeps one exact branch denied",
	"project admission keeps one exact session denied",
}

type mountedSelectionSafetyDocument struct {
	DeclaredCases    int                          `yaml:"declared_cases"`
	DeclaredSessions int                          `yaml:"declared_sessions"`
	Cases            []mountedSelectionSafetyCase `yaml:"cases"`
}

type mountedSelectionSafetyCase struct {
	Name              string                   `yaml:"name"`
	SelectionKind     mountedSelectionKind     `yaml:"selection_kind"`
	SelectedClone     string                   `yaml:"selected_clone"`
	SelectedSession   string                   `yaml:"selected_session"`
	Force             bool                     `yaml:"force"`
	AutoNewBranches   bool                     `yaml:"auto_new_branches"`
	BranchExclusions  []mountedBranchExclusion `yaml:"branch_exclusions"`
	SessionExclusions []string                 `yaml:"session_exclusions"`
	Rows              []mountedSelectionRow    `yaml:"rows"`
}

type mountedBranchExclusion struct {
	Clone    string   `yaml:"clone"`
	Branches []string `yaml:"branches"`
}

type mountedSelectionRow struct {
	Role                mountedSelectionRole `yaml:"role"`
	SessionID           string               `yaml:"session_id"`
	Clone               string               `yaml:"clone"`
	GitRemote           string               `yaml:"git_remote"`
	ProjectName         string               `yaml:"project_name"`
	Branch              string               `yaml:"branch"`
	Pushed              bool                 `yaml:"pushed"`
	ExpectedPush        bool                 `yaml:"expected_push"`
	ExpectedPruneDelete bool                 `yaml:"expected_prune_delete"`
}

func decodeMountedSelectionSafety(source []byte) (mountedSelectionSafetyDocument, error) {
	var document mountedSelectionSafetyDocument
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode mounted selection safety fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return document, fmt.Errorf("mounted selection safety fixture must contain exactly one YAML document: %v", err)
	}
	if document.DeclaredCases != mountedSelectionCaseCount || len(document.Cases) != mountedSelectionCaseCount {
		return document, fmt.Errorf("mounted selection safety fixture case count mismatch: declared=%d actual=%d required=%d", document.DeclaredCases, len(document.Cases), mountedSelectionCaseCount)
	}

	allowedKinds := make(map[mountedSelectionKind]bool, len(allMountedSelectionKinds))
	for _, kind := range allMountedSelectionKinds {
		allowedKinds[kind] = true
	}
	allowedRoles := make(map[mountedSelectionRole]bool, len(allMountedSelectionRoles))
	for _, role := range allMountedSelectionRoles {
		allowedRoles[role] = true
	}
	seenCases := make(map[string]bool, len(document.Cases))
	seenSessions := make(map[string]bool, document.DeclaredSessions)
	seenKinds := make(map[mountedSelectionKind]bool, len(allMountedSelectionKinds))
	seenRoles := make(map[mountedSelectionRole]bool, len(allMountedSelectionRoles))
	forceCases := 0
	rowCount := 0
	for caseIndex, fixture := range document.Cases {
		if fixture.Name == "" || seenCases[fixture.Name] {
			return document, fmt.Errorf("mounted selection safety fixture cases[%d] has an empty or duplicate name %q", caseIndex, fixture.Name)
		}
		seenCases[fixture.Name] = true
		if !allowedKinds[fixture.SelectionKind] {
			return document, fmt.Errorf("mounted selection safety fixture case %q has unknown selection kind %q", fixture.Name, fixture.SelectionKind)
		}
		seenKinds[fixture.SelectionKind] = true
		if len(fixture.Rows) == 0 {
			return document, fmt.Errorf("mounted selection safety fixture case %q has no session rows", fixture.Name)
		}
		if fixture.Force {
			forceCases++
		}
		selectedCloneFound := false
		selectedSessionFound := false
		caseClones := make(map[string]bool, len(fixture.Rows))
		caseSessions := make(map[string]bool, len(fixture.Rows))
		for rowIndex, row := range fixture.Rows {
			rowCount++
			if !allowedRoles[row.Role] || seenRoles[row.Role] {
				return document, fmt.Errorf("mounted selection safety fixture case %q rows[%d] has unknown or duplicate role %q", fixture.Name, rowIndex, row.Role)
			}
			seenRoles[row.Role] = true
			if row.SessionID == "" || seenSessions[row.SessionID] {
				return document, fmt.Errorf("mounted selection safety fixture case %q rows[%d] has an empty or duplicate session ID %q", fixture.Name, rowIndex, row.SessionID)
			}
			seenSessions[row.SessionID] = true
			caseSessions[row.SessionID] = true
			caseClones[row.Clone] = true
			if _, err := ingest.NewSessionID(row.SessionID); err != nil {
				return document, fmt.Errorf("mounted selection safety fixture case %q rows[%d] has invalid session ID %q: %w", fixture.Name, rowIndex, row.SessionID, err)
			}
			if row.Clone == "" || row.ProjectName == "" || row.Branch == "" {
				return document, fmt.Errorf("mounted selection safety fixture case %q rows[%d] must define clone, project_name, and branch", fixture.Name, rowIndex)
			}
			if row.ExpectedPush == row.ExpectedPruneDelete {
				return document, fmt.Errorf("mounted selection safety fixture case %q rows[%d] must be retained by exactly one side of the selection boundary", fixture.Name, rowIndex)
			}
			selectedCloneFound = selectedCloneFound || row.Clone == fixture.SelectedClone
			selectedSessionFound = selectedSessionFound || row.SessionID == fixture.SelectedSession
			if fixture.Force && !row.Pushed {
				return document, fmt.Errorf("mounted selection safety fixture force case %q rows[%d] must start already pushed so --force changes eligibility", fixture.Name, rowIndex)
			}
		}
		for exclusionIndex, exclusion := range fixture.BranchExclusions {
			if !caseClones[exclusion.Clone] || len(exclusion.Branches) == 0 {
				return document, fmt.Errorf("mounted selection safety fixture case %q branch_exclusions[%d] must name a row clone and at least one branch", fixture.Name, exclusionIndex)
			}
		}
		for exclusionIndex, sessionID := range fixture.SessionExclusions {
			if !caseSessions[sessionID] {
				return document, fmt.Errorf("mounted selection safety fixture case %q session_exclusions[%d] names unavailable session %q", fixture.Name, exclusionIndex, sessionID)
			}
		}
		switch fixture.SelectionKind {
		case mountedSelectionClonePath:
			if fixture.SelectedClone == "" || !selectedCloneFound || fixture.SelectedSession != "" {
				return document, fmt.Errorf("mounted selection safety fixture clone-path case %q must identify one clone and no explicit session", fixture.Name)
			}
		case mountedSelectionExplicitSession:
			if fixture.SelectedSession == "" || !selectedSessionFound || fixture.SelectedClone != "" {
				return document, fmt.Errorf("mounted selection safety fixture explicit-session case %q must identify one listed session and no clone", fixture.Name)
			}
		case mountedSelectionRemote:
			for _, row := range fixture.Rows {
				if row.GitRemote == "" || row.GitRemote != fixture.Rows[0].GitRemote {
					return document, fmt.Errorf("mounted selection safety fixture remote case %q must give every row the same non-empty remote", fixture.Name)
				}
			}
		case mountedSelectionName:
			if len(fixture.Rows) != 1 || fixture.Rows[0].GitRemote != "" {
				return document, fmt.Errorf("mounted selection safety fixture name case %q must have one non-Git row", fixture.Name)
			}
		}
	}
	if rowCount != mountedSelectionSessionCount || document.DeclaredSessions != mountedSelectionSessionCount {
		return document, fmt.Errorf("mounted selection safety fixture session count mismatch: declared=%d actual=%d required=%d", document.DeclaredSessions, rowCount, mountedSelectionSessionCount)
	}
	if forceCases != 1 {
		return document, fmt.Errorf("mounted selection safety fixture must contain exactly one already-pushed --force case, got %d", forceCases)
	}
	for _, kind := range allMountedSelectionKinds {
		if !seenKinds[kind] {
			return document, fmt.Errorf("mounted selection safety fixture does not cover selection kind %q", kind)
		}
	}
	for _, role := range allMountedSelectionRoles {
		if !seenRoles[role] {
			return document, fmt.Errorf("mounted selection safety fixture does not cover role %q", role)
		}
	}
	for _, required := range requiredMountedExclusionCases {
		if !seenCases[required] {
			return document, fmt.Errorf("mounted selection safety fixture is missing required exact-exclusion case %q", required)
		}
	}
	return document, nil
}

func loadMountedSelectionSafety(t *testing.T) mountedSelectionSafetyDocument {
	t.Helper()
	document, err := decodeMountedSelectionSafety(mountedSelectionSafetyYAML)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func TestMountedSelectionSafetyFixtureRejectsSemanticMutation(t *testing.T) {
	mutated := bytes.Replace(mountedSelectionSafetyYAML, []byte("role: unique-name"), []byte("role: unknown-name-role"), 1)
	if _, err := decodeMountedSelectionSafety(mutated); err == nil {
		t.Fatal("a count-preserving selection-role mutation unexpectedly validated")
	}
}

type mountedSelectionWorld struct {
	Dir         string
	ConfigPath  string
	DBPath      string
	OutputBase  string
	ClonePaths  map[string]string
	MetadataDir map[string]string
}

func seedMountedSelectionWorld(t *testing.T, fixture mountedSelectionSafetyCase) mountedSelectionWorld {
	t.Helper()
	dir := t.TempDir()
	writeTestCredentials(t, dir)
	world := mountedSelectionWorld{
		Dir:         dir,
		ConfigPath:  filepath.Join(dir, "selection.yaml"),
		DBPath:      string(defaults.ResolveDBFilePathWith(dir)),
		OutputBase:  filepath.Join(string(defaults.ResolveDataDirPathWith(dir)), "peasant-sync"),
		ClonePaths:  make(map[string]string, len(fixture.Rows)),
		MetadataDir: make(map[string]string, len(fixture.Rows)),
	}
	for _, row := range fixture.Rows {
		clonePath := filepath.Join(dir, "clones", row.Clone)
		if err := os.MkdirAll(clonePath, 0o755); err != nil {
			t.Fatalf("create physical clone %q: %v", row.Clone, err)
		}
		world.ClonePaths[row.Clone] = clonePath
	}

	cfg := config.BaseConfig()
	cfg.Output.BasePath = world.OutputBase
	cfg.Push.Method = config.PushMethodAll
	cfg.Push.Visibility = config.VisibilityPrivate
	harnessSelection := config.SelectionHarnessConfig{}
	first := fixture.Rows[0]
	switch fixture.SelectionKind {
	case mountedSelectionClonePath:
		harnessSelection.Projects = []config.ProjectSelection{{
			GitRemote:  first.GitRemote,
			ClonePaths: []string{world.ClonePaths[fixture.SelectedClone]},
		}}
	case mountedSelectionExplicitSession:
		harnessSelection.Sessions = []string{fixture.SelectedSession}
	case mountedSelectionRemote:
		harnessSelection.Projects = []config.ProjectSelection{{GitRemote: first.GitRemote}}
	case mountedSelectionName:
		harnessSelection.Projects = []config.ProjectSelection{{Name: filepath.Base(world.ClonePaths[first.Clone])}}
	default:
		t.Fatalf("unsupported mounted selection kind %q", fixture.SelectionKind)
	}
	for _, exclusion := range fixture.BranchExclusions {
		harnessSelection.Exclusions.Branches = append(harnessSelection.Exclusions.Branches, config.BranchExclusion{
			ClonePath: world.ClonePaths[exclusion.Clone],
			Branches:  append([]string(nil), exclusion.Branches...),
		})
	}
	harnessSelection.Exclusions.Sessions = append([]string(nil), fixture.SessionExclusions...)
	cfg.Selection = config.SelectionConfig{
		Mode:                  config.SelectionModeSelected,
		AutoIngestNewBranches: fixture.AutoNewBranches,
		Harnesses: map[string]config.SelectionHarnessConfig{
			defaults.HarnessClaudeCode.String(): harnessSelection,
		},
	}
	if err := config.SaveAtomic(world.ConfigPath, cfg); err != nil {
		t.Fatalf("save mounted selection config: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(world.DBPath), defaults.PrivateDirPerm); err != nil {
		t.Fatalf("create mounted selection data directory: %v", err)
	}
	db, err := store.Open(world.DBPath)
	if err != nil {
		t.Fatalf("open mounted selection store: %v", err)
	}
	entries := make([]ingest.StoreEntry, 0, len(fixture.Rows))
	for index, row := range fixture.Rows {
		entry := makeCmdStoreEntry(
			t,
			row.SessionID,
			mountedSelectionHostSlug,
			row.GitRemote,
			row.Branch,
			1_705_276_800_000+int64(index)*60_000,
			world.ClonePaths[row.Clone],
		)
		entry.Metadata.Project.Name = row.ProjectName
		if row.GitRemote == "" {
			entry.Metadata.Git.Remote = nil
		}
		entries = append(entries, entry)
	}
	if err := db.InsertSessions(t.Context(), entries); err != nil {
		db.Close()
		t.Fatalf("insert mounted selection sessions: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close mounted selection store after seeding: %v", err)
	}

	for _, entry := range entries {
		metadataPath := ingest.SessionMetadataPath(
			world.OutputBase,
			entry.Metadata.HostSlug.String(),
			entry.Metadata.SessionID.String(),
			"",
		)
		if err := os.MkdirAll(filepath.Dir(metadataPath), defaults.PrivateDirPerm); err != nil {
			t.Fatalf("create mounted push metadata directory: %v", err)
		}
		encoded, err := json.Marshal(entry.Metadata)
		if err != nil {
			t.Fatalf("encode mounted push metadata: %v", err)
		}
		if err := os.WriteFile(metadataPath, encoded, defaults.PrivateFilePerm); err != nil {
			t.Fatalf("write mounted push metadata: %v", err)
		}
		world.MetadataDir[entry.Metadata.SessionID.String()] = filepath.Dir(metadataPath)
	}

	pushedIDs := make([]ingest.SessionID, 0, len(fixture.Rows))
	for _, row := range fixture.Rows {
		if row.Pushed {
			pushedIDs = append(pushedIDs, ingest.SessionID(row.SessionID))
		}
	}
	if len(pushedIDs) > 0 {
		seedPublicationCursorsForTest(t, world.DBPath, pushedIDs, 1_800_000_000_000)
	}
	return world
}

func mountedViewerIDs(t *testing.T, world mountedSelectionWorld) map[string]bool {
	t.Helper()
	cfg, err := loadConfig(world.ConfigPath)
	if err != nil {
		t.Fatalf("load mounted viewer config: %v", err)
	}
	policy, err := sessionvisibility.New(cfg.Selection)
	if err != nil {
		t.Fatalf("build mounted viewer policy: %v", err)
	}
	db, err := store.Open(world.DBPath)
	if err != nil {
		t.Fatalf("open mounted viewer store: %v", err)
	}
	defer db.Close()
	provider := api.NewStoreDataProvider(db, policy)
	sessions, err := provider.Sessions(t.Context())
	if err != nil {
		t.Fatalf("list mounted viewer sessions: %v", err)
	}
	visible := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		visible[session.ID.String()] = true
	}
	return visible
}

func mountedExpectedIDs(fixture mountedSelectionSafetyCase, include func(mountedSelectionRow) bool) map[string]bool {
	ids := make(map[string]bool)
	for _, row := range fixture.Rows {
		if include(row) {
			ids[row.SessionID] = true
		}
	}
	return ids
}

func assertMountedIDSet(t *testing.T, label string, got, want map[string]bool) {
	t.Helper()
	if setsEqual(got, want) {
		return
	}
	keys := func(values map[string]bool) []string {
		result := make([]string, 0, len(values))
		for value := range values {
			result = append(result, value)
		}
		sort.Strings(result)
		return result
	}
	t.Fatalf("%s IDs = %v, want %v", label, keys(got), keys(want))
}

func assertMountedPushCandidatesProven(
	t *testing.T,
	db *store.Store,
	fixture mountedSelectionSafetyCase,
	world mountedSelectionWorld,
) {
	t.Helper()
	rows, err := db.AllPushableSessions(t.Context())
	if err != nil {
		t.Fatalf("read complete mounted push cohort: %v", err)
	}
	inputs := make([]selectionCandidateInput, len(rows))
	for index, row := range rows {
		branch := ""
		if row.GitBranch != nil {
			branch = *row.GitBranch
		}
		inputs[index] = selectionCandidateInput{
			Harness:     ingest.Harness(row.ModelHarness),
			GitRemote:   row.GitRemote,
			ProjectName: row.ProjectName,
			ProjectHash: row.ProjectHash,
			ProjectPath: row.ProjectPath,
			Branch:      branch,
			SessionID:   ingest.SessionID(row.SessionID),
		}
	}
	candidates, err := prepareSelectionCandidates(t.Context(), inputs, ingest.NewPhysicalPathResolver())
	if err != nil {
		t.Fatalf("prepare mounted push candidates: %v", err)
	}
	wantPath := make(map[string]string, len(fixture.Rows))
	for _, row := range fixture.Rows {
		wantPath[row.SessionID] = world.ClonePaths[row.Clone]
	}
	if len(candidates) != len(wantPath) {
		t.Fatalf("prepared mounted push candidates = %d, want %d", len(candidates), len(wantPath))
	}
	for _, candidate := range candidates {
		if candidate.RemoteMultiplicity == ingest.DiscoveryIdentityUnproven || candidate.NameMultiplicity == ingest.DiscoveryIdentityUnproven {
			t.Fatalf("session %s reached the matcher with zero/unproven multiplicity: remote=%d name=%d", candidate.SessionID, candidate.RemoteMultiplicity, candidate.NameMultiplicity)
		}
		if got, want := candidate.ClonePath.String(), wantPath[candidate.SessionID.String()]; got != want {
			t.Fatalf("session %s prepared physical clone = %q, want %q", candidate.SessionID, got, want)
		}
	}
}

func mountedChooserIDs(t *testing.T, fixture mountedSelectionSafetyCase, world mountedSelectionWorld) map[string]bool {
	t.Helper()
	cfg, err := loadConfig(world.ConfigPath)
	if err != nil {
		t.Fatalf("load mounted chooser config: %v", err)
	}
	db, err := store.Open(world.DBPath)
	if err != nil {
		t.Fatalf("open mounted chooser store: %v", err)
	}
	defer db.Close()
	assertMountedPushCandidatesProven(t, db, fixture, world)
	selection, err := preparePushSelection(t.Context(), db, cfg.SelectionMatcher(), ingest.NewPhysicalPathResolver())
	if err != nil {
		t.Fatalf("prepare mounted chooser selection: %v", err)
	}
	wizardSessions, err := buildPushWizardSessions(
		t.Context(),
		db,
		&ingest.OSFileSystem{},
		cfg.Output.BasePath,
		push.PushCandidateQuery{Force: fixture.Force, Method: cfg.Push.Method, Sources: cfg.Push.Sources},
		selection,
	)
	if err != nil {
		t.Fatalf("build mounted push chooser: %v", err)
	}
	displayed := make(map[string]bool, len(wizardSessions))
	for _, session := range wizardSessions {
		displayed[session.Row.SessionID] = true
	}
	want := mountedExpectedIDs(fixture, func(row mountedSelectionRow) bool { return row.ExpectedPush })
	assertMountedIDSet(t, "mounted chooser display", displayed, want)
	selected := make(map[string]bool)
	for _, sessionID := range push.NewPushWizard(theme.New(theme.ModeDark), wizardSessions).SelectedSessionIDs() {
		selected[sessionID] = true
	}
	return selected
}

func mountedPipelineIDs(t *testing.T, fixture mountedSelectionSafetyCase, world mountedSelectionWorld) map[string]bool {
	t.Helper()
	args := []string{"--dry-run", "--json", "--config=" + world.ConfigPath}
	if fixture.Force {
		args = append(args, "--force")
	}
	output, err := executePushCmd(t, world.Dir, args)
	if err != nil {
		t.Fatalf("run mounted push pipeline: %v\n%s", err, output)
	}
	jsonStart := bytes.IndexByte([]byte(output), '{')
	if jsonStart < 0 {
		t.Fatalf("mounted push pipeline returned no JSON object: %q", output)
	}
	var result jsonPushResult
	if err := json.Unmarshal([]byte(output)[jsonStart:], &result); err != nil {
		t.Fatalf("decode mounted push pipeline result: %v\n%s", err, output)
	}
	if result.Errors != 0 {
		t.Fatalf("mounted push pipeline reported %d preflight errors: %+v", result.Errors, result.Sessions)
	}
	wantStatus := make(map[string]string, len(fixture.Rows))
	for _, row := range fixture.Rows {
		if !row.ExpectedPush {
			continue
		}
		status := push.PushStatusNew.String()
		if row.Pushed {
			status = push.PushStatusUpdated.String()
		}
		wantStatus[row.SessionID] = status
	}
	ids := make(map[string]bool, len(result.Sessions))
	for _, session := range result.Sessions {
		ids[session.SessionID] = true
		if session.Status != wantStatus[session.SessionID] || session.Error != "" {
			t.Errorf("mounted push pipeline session %s = status %q error %q, want status %q and no error", session.SessionID, session.Status, session.Error, wantStatus[session.SessionID])
		}
	}
	return ids
}

func TestMountedSelectionPushChooserPipelineAndPruneKeepTheCloneBoundary(t *testing.T) {
	fixtures := loadMountedSelectionSafety(t)
	for _, fixture := range fixtures.Cases {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			world := seedMountedSelectionWorld(t, fixture)
			wantPushed := mountedExpectedIDs(fixture, func(row mountedSelectionRow) bool { return row.ExpectedPush })
			if len(fixture.BranchExclusions) > 0 || len(fixture.SessionExclusions) > 0 {
				viewerIDs := mountedViewerIDs(t, world)
				assertMountedIDSet(t, "mounted viewer", viewerIDs, wantPushed)
			}

			chooserIDs := mountedChooserIDs(t, fixture, world)
			assertMountedIDSet(t, "mounted chooser selected", chooserIDs, wantPushed)

			pipelineIDs := mountedPipelineIDs(t, fixture, world)
			assertMountedIDSet(t, "mounted push pipeline", pipelineIDs, wantPushed)

			beforePrune, err := store.Open(world.DBPath)
			if err != nil {
				t.Fatalf("open mounted selection store before manual prune: %v", err)
			}
			allBeforePrune, queryErr := beforePrune.QueryPrunableSessions(t.Context(), ingest.PruneFilter{All: true})
			beforePrune.Close()
			if queryErr != nil {
				t.Fatalf("query mounted selection store before manual prune: %v", queryErr)
			}
			if len(allBeforePrune) != len(fixture.Rows) {
				t.Fatalf("non-destructive viewer/push paths left %d stored sessions, want all %d before manual prune", len(allBeforePrune), len(fixture.Rows))
			}

			pruneOutput, _, err := executePruneCmdWithConfig(
				t,
				world.Dir,
				world.ConfigPath,
				[]string{"--config=" + world.ConfigPath, "--unselected", "--confirm", "--json"},
			)
			if err != nil {
				t.Fatalf("mounted prune --unselected: %v\n%s", err, pruneOutput)
			}
			var pruneResult struct {
				Deleted int      `json:"deleted"`
				Errors  []string `json:"errors"`
			}
			if err := json.Unmarshal([]byte(pruneOutput), &pruneResult); err != nil {
				t.Fatalf("decode mounted prune result: %v\n%s", err, pruneOutput)
			}
			wantDeleted := mountedExpectedIDs(fixture, func(row mountedSelectionRow) bool { return row.ExpectedPruneDelete })
			if pruneResult.Deleted != len(wantDeleted) || len(pruneResult.Errors) != 0 {
				t.Fatalf("mounted prune result = deleted %d errors %v, want deleted %d and no errors", pruneResult.Deleted, pruneResult.Errors, len(wantDeleted))
			}

			verify, err := store.Open(world.DBPath)
			if err != nil {
				t.Fatalf("reopen mounted selection store after prune: %v", err)
			}
			remainingRows, err := verify.QueryPrunableSessions(context.Background(), ingest.PruneFilter{All: true})
			verify.Close()
			if err != nil {
				t.Fatalf("query mounted selection store after prune: %v", err)
			}
			remaining := make(map[string]bool, len(remainingRows))
			for _, row := range remainingRows {
				remaining[row.SessionID.String()] = true
			}
			wantRemaining := mountedExpectedIDs(fixture, func(row mountedSelectionRow) bool { return !row.ExpectedPruneDelete })
			assertMountedIDSet(t, "sessions retained after manual prune", remaining, wantRemaining)

			for _, row := range fixture.Rows {
				_, statErr := os.Stat(world.MetadataDir[row.SessionID])
				if row.ExpectedPruneDelete && !os.IsNotExist(statErr) {
					t.Errorf("unselected session %s transcript directory still exists after manual prune: %v", row.SessionID, statErr)
				}
				if !row.ExpectedPruneDelete && statErr != nil {
					t.Errorf("selected session %s transcript directory was removed by manual prune: %v", row.SessionID, statErr)
				}
			}
		})
	}
}
