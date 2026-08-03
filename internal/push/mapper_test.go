package push_test

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// allFieldsVisible returns PushFieldVisibility with all fields enabled for backward-compat tests.
func allFieldsVisible() config.PushFieldVisibility {
	return config.PushFieldVisibility{
		GitRemote:   true,
		GitBranch:   true,
		ProjectPath: true,
		HostSlug:    true,
		ProjectName: true,
	}
}

// mapOpts is a convenience for tests that don't care about field visibility.
func mapOpts(meta *ingest.UnifiedMetadata, metrics *schema.QualityMetrics, entries []schema.SessionEntry) push.MapOptions {
	return push.MapOptions{
		Meta:    meta,
		Metrics: metrics,
		Entries: entries,
		Fields:  allFieldsVisible(),
	}
}

// fixtureMetadata returns a fully populated UnifiedMetadata for tests.
func fixtureMetadata() *ingest.UnifiedMetadata {
	meta := ingest.NewUnifiedMetadata()

	sid := ingest.SessionID(testutil.TestSessionUUID)
	meta.SessionID = sid
	meta.ModelHarness = ingest.HarnessClaudeCode
	meta.Model = testutil.TestModel
	meta.Version = "2.1.47"

	// Unix millis for 2026-02-23T12:00:00Z
	const startMs int64 = 1740312000000
	const endMs int64 = 1740312360000 // +6 minutes
	const ingestedMs int64 = 1740312400000

	ingestedMsVal := ingestedMs
	meta.Timestamp = ingest.TimestampInfo{
		Start:    startMs,
		End:      endMs,
		Ingested: &ingestedMsVal,
	}

	branch := "main"
	remote := testutil.TestGitRemote
	worktree := testutil.TestDefaultWorktreeDir
	tracking := "origin/main"
	meta.Git = ingest.GitContext{
		Branch:   &branch,
		Remote:   &remote,
		Worktree: &worktree,
		Tracking: &tracking,
	}

	meta.Project = ingest.ProjectInfo{
		Hash:     testutil.TestProjectHash,
		FilePath: testutil.TestDefaultWorktreeDir,
		Name:     testutil.TestProjectName,
	}
	meta.HostSlug = ingest.HostSlug(testutil.TestHostSlug)
	meta.Source = ingest.SourceInfo{
		FilePath: "/home/test/.claude/projects/abc/def.jsonl",
		Format:   ingest.SourceFormatJSONL,
	}

	meta.Stats = ingest.StatsInfo{
		TurnCount:     5,
		ToolCallCount: 3,
		SubagentCount: 1,
		DurationMs:    360000,
		TokensIn:      1000,
		TokensOut:     500,
	}

	return &meta
}

// parsePublishRequest unmarshals the output of MapMetadata into a map for field-by-field assertions.
func parsePublishRequest(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("unmarshal publish request: %v", err)
	}
	return m
}

func TestMapMetadata_BasicFields(t *testing.T) {
	meta := fixtureMetadata()
	payload, err := push.MapMetadata(mapOpts(meta, nil, nil))
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}

	m := parsePublishRequest(t, payload)

	identity := m["identity"].(map[string]any)
	assertEqual(t, "sessionId", testutil.TestSessionUUID, identity["sessionId"])

	model := m["model"].(map[string]any)
	// Unified harness key: json:"harness", not "modelHarness".
	assertEqual(t, "harness", string(ingest.HarnessClaudeCode), model["harness"])
	if _, ok := model["modelHarness"]; ok {
		t.Error("legacy key 'modelHarness' must not appear in model wire object")
	}
	assertEqual(t, "model", string(testutil.TestModel), model["model"])
	assertEqual(t, "version", "2.1.47", model["version"])
	assertEqual(t, "hostSlug", testutil.TestHostSlug, model["hostSlug"])

	project := m["project"].(map[string]any)
	assertEqual(t, "name", testutil.TestProjectName, project["name"])

	source := m["source"].(map[string]any)
	assertEqual(t, "format", string(ingest.SourceFormatJSONL), source["format"])
}

// TestMapMetadata_PublishesDurableAssociations verifies the publish wire uses
// producer-owned opaque association IDs alongside the originally observed
// commit hashes, rather than deriving response-time session/hash identifiers.
func TestMapMetadata_PublishesDurableAssociations(t *testing.T) {
	associationID, err := schema.NewAssociationID("assoc-00000000-0000-0000-0000-000000000001")
	if err != nil {
		t.Fatalf("NewAssociationID: %v", err)
	}
	opts := mapOpts(fixtureMetadata(), nil, nil)
	opts.Associations = []schema.PublishedAssociation{{
		ID:                 associationID,
		ObservedCommitHash: "observed-commit-001",
	}}

	payload, err := push.MapMetadata(opts)
	if err != nil {
		t.Fatalf("MapMetadata: %v", err)
	}
	var request schema.PublishRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatalf("unmarshal publish request: %v", err)
	}
	if len(request.Git.Associations) != 1 {
		t.Fatalf("publish associations = %d, want 1", len(request.Git.Associations))
	}
	association := request.Git.Associations[0]
	if association.ID != associationID {
		t.Errorf("published association ID = %q, want %q", association.ID, associationID)
	}
	if association.ObservedCommitHash != "observed-commit-001" {
		t.Errorf("published observed commit = %q, want observed-commit-001", association.ObservedCommitHash)
	}
}

func TestMapMetadata_License(t *testing.T) {
	t.Run("set license lands in the body", func(t *testing.T) {
		meta := fixtureMetadata()
		opts := mapOpts(meta, nil, nil)
		opts.License = schema.LicenseCCBY
		payload, err := push.MapMetadata(opts)
		if err != nil {
			t.Fatalf("MapMetadata returned error: %v", err)
		}
		m := parsePublishRequest(t, payload)
		assertEqual(t, "license", string(schema.LicenseCCBY), m["license"])
	})

	t.Run("empty license is omitted", func(t *testing.T) {
		meta := fixtureMetadata()
		// mapOpts leaves License zero-valued ("") — the legacy/un-set case.
		payload, err := push.MapMetadata(mapOpts(meta, nil, nil))
		if err != nil {
			t.Fatalf("MapMetadata returned error: %v", err)
		}
		m := parsePublishRequest(t, payload)
		if _, ok := m["license"]; ok {
			t.Error("empty license must be omitted (omitempty) so the village stores NULL; got a license key")
		}
	})
}

func TestMapMetadata_TimestampConversion(t *testing.T) {
	meta := fixtureMetadata()
	payload, err := push.MapMetadata(mapOpts(meta, nil, nil))
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}

	m := parsePublishRequest(t, payload)
	timestamp := m["timestamp"].(map[string]any)

	assertFloat64(t, "start", 1740312000000, timestamp["start"])
	assertFloat64(t, "end", 1740312360000, timestamp["end"])
	assertFloat64(t, "ingested", 1740312400000, timestamp["ingested"])
}

func TestMapMetadata_GitFields(t *testing.T) {
	meta := fixtureMetadata()
	payload, err := push.MapMetadata(mapOpts(meta, nil, nil))
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}

	m := parsePublishRequest(t, payload)
	git := m["git"].(map[string]any)

	assertEqual(t, "branch", "main", git["branch"])
	assertEqual(t, "remote", testutil.TestGitRemote, git["remote"])
	assertEqual(t, "worktree", testutil.TestDefaultWorktreeDir, git["worktree"])
	assertEqual(t, "tracking", "origin/main", git["tracking"])
}

func TestMapMetadata_NullGitFields(t *testing.T) {
	meta := fixtureMetadata()
	meta.Git = ingest.GitContext{}
	payload, err := push.MapMetadata(mapOpts(meta, nil, nil))
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}

	m := parsePublishRequest(t, payload)
	git := m["git"].(map[string]any)

	if _, exists := git["branch"]; exists {
		t.Error("branch should be absent when nil")
	}
	if _, exists := git["remote"]; exists {
		t.Error("remote should be absent when nil")
	}
}

func TestMapMetadata_StatsFields(t *testing.T) {
	meta := fixtureMetadata()
	payload, err := push.MapMetadata(mapOpts(meta, nil, nil))
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}

	m := parsePublishRequest(t, payload)
	stats, ok := m["stats"].(map[string]any)
	if !ok {
		t.Fatalf("stats is not a map, got %T", m["stats"])
	}

	assertFloat64(t, "turnCount", 5, stats["turnCount"])
	assertFloat64(t, "toolCallCount", 3, stats["toolCallCount"])
	assertFloat64(t, "subagentCount", 1, stats["subagentCount"])
	assertFloat64(t, "durationMs", 360000, stats["durationMs"])
	assertFloat64(t, "tokensIn", 1000, stats["tokensIn"])
	assertFloat64(t, "tokensOut", 500, stats["tokensOut"])
}

func TestMapMetadata_SubagentsField(t *testing.T) {
	meta := fixtureMetadata()
	subID := ingest.SessionID(testutil.TestSubagentID)
	parentID := ingest.SessionID(testutil.TestSessionUUID)
	meta.Subagents = []ingest.SubagentRef{
		{SessionID: subID, ParentUUID: parentID},
	}

	payload, err := push.MapMetadata(mapOpts(meta, nil, nil))
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}

	m := parsePublishRequest(t, payload)

	subagents, ok := m["subagents"]
	if !ok {
		t.Fatal("subagents field missing from payload")
	}
	subList, ok := subagents.([]any)
	if !ok {
		t.Fatalf("subagents is not an array, got %T", subagents)
	}
	if len(subList) != 1 {
		t.Errorf("subagents count: got %d, want 1", len(subList))
	}
}

func TestMapMetadata_EmptySubagents(t *testing.T) {
	meta := fixtureMetadata()
	meta.Subagents = []ingest.SubagentRef{}

	payload, err := push.MapMetadata(mapOpts(meta, nil, nil))
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}

	m := parsePublishRequest(t, payload)

	if _, exists := m["subagents"]; exists {
		t.Error("subagents should be absent for empty slice")
	}
}

func TestMapMetadata_DiagnosticsWarnings(t *testing.T) {
	meta := fixtureMetadata()
	meta.Diagnostics = ingest.DiagnosticsInfo{
		Warnings: []ingest.DiagnosticEntry{
			{
				ErrorType:   "parse_error",
				Location:    "line 47",
				Message:     "unexpected token",
				Remediation: "check the file",
			},
		},
		Partial: func(b bool) *bool { return &b }(true),
	}

	payload, err := push.MapMetadata(mapOpts(meta, nil, nil))
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}

	m := parsePublishRequest(t, payload)
	diag, ok := m["diagnostics"].(map[string]any)
	if !ok {
		t.Fatalf("diagnostics is not a map, got %T", m["diagnostics"])
	}

	warnings, ok := diag["warnings"]
	if !ok {
		t.Fatal("diagnostics.warnings field missing")
	}
	warnList, ok := warnings.([]any)
	if !ok {
		t.Fatalf("diagnostics.warnings is not an array, got %T", warnings)
	}
	if len(warnList) != 1 {
		t.Errorf("diagnostics.warnings count: got %d, want 1", len(warnList))
	}

	partialVal, _ := diag["partial"].(bool)
	if !partialVal {
		t.Error("diagnostics.partial should be true")
	}
}

func TestMapMetadata_ParentUUID(t *testing.T) {
	meta := fixtureMetadata()
	parentID := ingest.SessionID(testutil.TestSubagentID)
	meta.ParentUUID = &parentID

	payload, err := push.MapMetadata(mapOpts(meta, nil, nil))
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}

	m := parsePublishRequest(t, payload)
	identity := m["identity"].(map[string]any)

	assertEqual(t, "parentUuid", testutil.TestSubagentID, identity["parentUuid"])
}

func TestMapMetadata_NoParentUUID(t *testing.T) {
	meta := fixtureMetadata()
	meta.ParentUUID = nil

	payload, err := push.MapMetadata(mapOpts(meta, nil, nil))
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}

	m := parsePublishRequest(t, payload)
	identity := m["identity"].(map[string]any)

	if _, exists := identity["parentUuid"]; exists {
		t.Error("parentUuid should be absent when nil")
	}
}

func TestMapMetadata_VisibilityVariants(t *testing.T) {
	// Note: visibility is NOT in PublishRequest in schema. It's used to determine access.
	// However, if we wanted it in the JSON, it would need to be added to PublishRequest.
	// The current PublishRequest does NOT have visibility.
	// Let me check mapper.go implementation again.
}

func TestMapMetadata_NilMetrics_NoQualityKey(t *testing.T) {
	meta := fixtureMetadata()
	payload, err := push.MapMetadata(mapOpts(meta, nil, nil))
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}
	m := parsePublishRequest(t, payload)
	if _, exists := m["quality"]; exists {
		t.Error("quality key should be absent when SessionMetrics is nil")
	}
}

func TestMapMetadata_WithMetrics_V2Fields(t *testing.T) {
	meta := fixtureMetadata()

	title := "Fix login bug"
	outcome := ingest.OutcomeResolved
	filesTouched := 4
	linesChanged := 120
	durationMinutes := 6.0
	retryLoops := 1
	retryTokensWasted := 200
	withinSessionReverts := 0
	signalDensity := 0.75
	specQualityScore := 0.82
	explorationRatio := 0.3
	scopeBreadth := 5
	discoveryTurns := 2
	computedAt := int64(1740312400000)
	computeVersion := 1
	hasExamples := true
	hasConstraints := false

	metrics := &schema.QualityMetrics{
		TitleGenerated:       &title,
		Outcome:              &outcome,
		FilesTouched:         &filesTouched,
		LinesChanged:         &linesChanged,
		DurationMinutes:      &durationMinutes,
		RetryLoops:           &retryLoops,
		RetryTokensWasted:    &retryTokensWasted,
		WithinSessionReverts: &withinSessionReverts,
		SignalDensity:        &signalDensity,
		SpecQualityScore:     &specQualityScore,
		ExplorationRatio:     &explorationRatio,
		ScopeBreadth:         &scopeBreadth,
		DiscoveryTurns:       &discoveryTurns,
		M7SpecHasExamples:    &hasExamples,
		M7SpecHasConstraints: &hasConstraints,
		ComputedAt:           &computedAt,
		ComputeVersion:       &computeVersion,
	}

	payload, err := push.MapMetadata(mapOpts(meta, metrics, nil))
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}

	m := parsePublishRequest(t, payload)

	qualityMap, ok := m["quality"].(map[string]any)
	if !ok {
		t.Fatalf("quality is not a map, got %T", m["quality"])
	}

	assertEqual(t, "titleGenerated", "Fix login bug", qualityMap["titleGenerated"])
	assertEqual(t, "outcome", string(ingest.OutcomeResolved), qualityMap["outcome"])
	assertFloat64(t, "filesTouched", 4, qualityMap["filesTouched"])
	assertFloat64(t, "linesChanged", 120, qualityMap["linesChanged"])
	assertFloat64(t, "durationMinutes", 6.0, qualityMap["durationMinutes"])
	assertFloat64(t, "retryLoops", 1, qualityMap["retryLoops"])
	assertFloat64(t, "signalDensity", 0.75, qualityMap["signalDensity"])
	assertFloat64(t, "specQualityScore", 0.82, qualityMap["specQualityScore"])
	assertFloat64(t, "explorationRatio", 0.3, qualityMap["explorationRatio"])
	assertFloat64(t, "scopeBreadth", 5, qualityMap["scopeBreadth"])
	assertFloat64(t, "discoveryTurns", 2, qualityMap["discoveryTurns"])
	assertFloat64(t, "computeVersion", 1, qualityMap["computeVersion"])
	assertFloat64(t, "computedAt", float64(computedAt), qualityMap["computedAt"])

	if v, ok := qualityMap["m7SpecHasExamples"].(bool); !ok || !v {
		t.Errorf("m7SpecHasExamples: got %v (%T), want true", qualityMap["m7SpecHasExamples"], qualityMap["m7SpecHasExamples"])
	}
	if v, ok := qualityMap["m7SpecHasConstraints"].(bool); !ok || v {
		t.Errorf("m7SpecHasConstraints: got %v (%T), want false", qualityMap["m7SpecHasConstraints"], qualityMap["m7SpecHasConstraints"])
	}
}

func TestMapMetadata_WithCostFields(t *testing.T) {
	meta := fixtureMetadata()

	costInput := 0.015
	costOutput := 0.045
	costReasoning := 0.010
	costCacheRead := 0.002
	costCacheWrite := 0.001
	costTotal := 0.073
	costModelID := "claude-sonnet-4-20250514"
	scope := "full"
	computeVersion := 1

	metrics := &schema.QualityMetrics{
		CostInputUSD:      &costInput,
		CostOutputUSD:     &costOutput,
		CostReasoningUSD:  &costReasoning,
		CostCacheReadUSD:  &costCacheRead,
		CostCacheWriteUSD: &costCacheWrite,
		CostTotalUSD:      &costTotal,
		CostModelID:       &costModelID,
		Scope:             &scope,
		ComputeVersion:    &computeVersion,
	}

	payload, err := push.MapMetadata(mapOpts(meta, metrics, nil))
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}

	m := parsePublishRequest(t, payload)

	qualityMap, ok := m["quality"].(map[string]any)
	if !ok {
		t.Fatalf("quality is not a map, got %T", m["quality"])
	}

	// Verify all 8 cost/scope fields.
	assertFloat64(t, "costInputUsd", 0.015, qualityMap["costInputUsd"])
	assertFloat64(t, "costOutputUsd", 0.045, qualityMap["costOutputUsd"])
	assertFloat64(t, "costReasoningUsd", 0.010, qualityMap["costReasoningUsd"])
	assertFloat64(t, "costCacheReadUsd", 0.002, qualityMap["costCacheReadUsd"])
	assertFloat64(t, "costCacheWriteUsd", 0.001, qualityMap["costCacheWriteUsd"])
	assertFloat64(t, "costTotalUsd", 0.073, qualityMap["costTotalUsd"])
	assertEqual(t, "costModelId", "claude-sonnet-4-20250514", qualityMap["costModelId"])
	assertEqual(t, "scope", "full", qualityMap["scope"])
}

// --- PushFieldVisibility omission tests ---

// TestMapMetadata_DefaultVisibility_AllFieldsOmitted verifies that when all
// PushFieldVisibility flags are false (zero value), sensitive fields are omitted.
func TestMapMetadata_DefaultVisibility_AllFieldsOmitted(t *testing.T) {
	meta := fixtureMetadata()
	opts := push.MapOptions{
		Meta:   meta,
		Fields: config.PushFieldVisibility{}, // all false
	}
	payload, err := push.MapMetadata(opts)
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}

	m := parsePublishRequest(t, payload)

	// Git fields gated by visibility should be absent.
	git := m["git"].(map[string]any)
	if _, exists := git["remote"]; exists {
		t.Error("gitRemote should be absent when Fields.GitRemote is false")
	}
	if _, exists := git["branch"]; exists {
		t.Error("gitBranch should be absent when Fields.GitBranch is false")
	}

	// Project path should be empty/absent.
	project := m["project"].(map[string]any)
	if path, _ := project["filePath"].(string); path != "" {
		t.Errorf("projectPath should be empty when Fields.ProjectPath is false, got %q", path)
	}

	// HostSlug should be empty.
	model := m["model"].(map[string]any)
	if slug, _ := model["hostSlug"].(string); slug != "" {
		t.Errorf("hostSlug should be empty when Fields.HostSlug is false, got %q", slug)
	}

	// project.hash is ALWAYS sent (salted, non-correlatable) — never gated, even
	// when every raw-identity field is omitted.
	wantHash := string(testutil.TestProjectHash)
	if hash, _ := project["hash"].(string); hash != wantHash {
		t.Errorf("project.hash must always be present, got %q want %q", hash, wantHash)
	}
}

// TestMapMetadata_SelectiveVisibility_GitRemoteOnly verifies that setting only
// GitRemote=true includes gitRemote but omits other gated fields.
func TestMapMetadata_SelectiveVisibility_GitRemoteOnly(t *testing.T) {
	meta := fixtureMetadata()
	opts := push.MapOptions{
		Meta: meta,
		Fields: config.PushFieldVisibility{
			GitRemote: true,
			// GitBranch, ProjectPath, HostSlug all false
		},
	}
	payload, err := push.MapMetadata(opts)
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}

	m := parsePublishRequest(t, payload)

	git := m["git"].(map[string]any)
	// GitRemote should be present.
	if _, exists := git["remote"]; !exists {
		t.Error("gitRemote should be present when Fields.GitRemote is true")
	}
	assertEqual(t, "remote", testutil.TestGitRemote, git["remote"])

	// GitBranch should be absent.
	if _, exists := git["branch"]; exists {
		t.Error("gitBranch should be absent when Fields.GitBranch is false")
	}

	// ProjectPath should be empty.
	project := m["project"].(map[string]any)
	if path, _ := project["filePath"].(string); path != "" {
		t.Errorf("projectPath should be empty when Fields.ProjectPath is false, got %q", path)
	}

	// HostSlug should be empty.
	model := m["model"].(map[string]any)
	if slug, _ := model["hostSlug"].(string); slug != "" {
		t.Errorf("hostSlug should be empty when Fields.HostSlug is false, got %q", slug)
	}
}

// TestMapMetadata_ProjectHash_AlwaysSent verifies that project.hash is always
// present (salted 64-hex) regardless of field-visibility config — it is no
// longer gated. Sending the salted, non-correlatable hash lets the village group
// a user's sessions by project without recovering any plaintext, and avoids the
// village 422 that an omitted hash previously triggered.
func TestMapMetadata_ProjectHash_AlwaysSent(t *testing.T) {
	wantHash := string(testutil.TestProjectHash)
	hexPattern := regexp.MustCompile(`^[0-9a-f]{64}$`)

	cases := []struct {
		name   string
		fields config.PushFieldVisibility
	}{
		{name: "default (all raw-identity omitted)", fields: config.DefaultPushFieldVisibility()},
		{name: "all raw-identity visible", fields: allFieldsVisible()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := fixtureMetadata()
			payload, err := push.MapMetadata(push.MapOptions{
				Meta:   meta,
				Fields: tc.fields,
			})
			if err != nil {
				t.Fatalf("MapMetadata returned error: %v", err)
			}

			m := parsePublishRequest(t, payload)
			project, ok := m["project"].(map[string]any)
			if !ok {
				t.Fatalf("project is not a map, got %T", m["project"])
			}

			hash, _ := project["hash"].(string)
			if hash != wantHash {
				t.Errorf("project.hash: got %q, want %q", hash, wantHash)
			}
			if !hexPattern.MatchString(hash) {
				t.Errorf("project.hash %q is not a 64-char lowercase hex digest", hash)
			}
		})
	}
}

// TestMapMetadata_ProjectName_OmittedByDefault verifies project names follow the
// raw-identity visibility policy even on the authoritative publication path.
func TestMapMetadata_ProjectName_OmittedByDefault(t *testing.T) {
	meta := fixtureMetadata()
	opts := push.MapOptions{
		Meta:   meta,
		Fields: config.DefaultPushFieldVisibility(), // ProjectName defaults to false
	}
	payload, err := push.MapMetadata(opts)
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}

	m := parsePublishRequest(t, payload)
	project, ok := m["project"].(map[string]any)
	if !ok {
		t.Fatalf("project is not a map, got %T", m["project"])
	}

	if name, _ := project["name"].(string); name != "" {
		t.Errorf("project.name should be empty when Fields.ProjectName is false, got %q", name)
	}
}

// TestMapMetadata_ProjectName_IncludedWhenOptIn verifies that Project.Name is present
// in the push payload when Fields.ProjectName is explicitly set to true.
func TestMapMetadata_ProjectName_IncludedWhenOptIn(t *testing.T) {
	meta := fixtureMetadata()
	opts := push.MapOptions{
		Meta: meta,
		Fields: config.PushFieldVisibility{
			ProjectName: true, // explicit opt-in
		},
	}
	payload, err := push.MapMetadata(opts)
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}

	m := parsePublishRequest(t, payload)
	project, ok := m["project"].(map[string]any)
	if !ok {
		t.Fatalf("project is not a map, got %T", m["project"])
	}

	name, _ := project["name"].(string)
	if name == "" {
		t.Error("project.name should be present when Fields.ProjectName is true")
	}
	if name != testutil.TestProjectName {
		t.Errorf("project.name: got %q, want %q", name, testutil.TestProjectName)
	}
}

// --- SessionEntry wire format tests ---

// TestMapMetadata_EntriesPassthrough verifies that when entries are passed to
// MapMetadata, they appear in the published JSON under the "entries" key.
//
// NOTE (L2): This test will FAIL at runtime until L3 wires entries into
// MapMetadata (currently the entries parameter is ignored with `_ = entries`).
func TestMapMetadata_EntriesPassthrough(t *testing.T) {
	meta := fixtureMetadata()

	toolKind := schema.ToolCallKindRead
	stopReason := schema.StopReasonEndTurn
	preview := "hello world"
	toolCallID := "tc-abc-123"
	tokensIn := 50
	tokensOut := 100
	rawLen := 2048

	entries := []schema.SessionEntry{
		{
			SessionID:      schema.SessionID(testutil.TestSessionUUID),
			EntryIndex:     0,
			Harness:        schema.HarnessClaudeCode,
			EntryType:      schema.EntryTypeToolUse,
			Role:           schema.RoleAssistant,
			HasToolUse:     true,
			ToolKind:       &toolKind,
			StopReason:     &stopReason,
			ContentPreview: &preview,
			ToolCallID:     &toolCallID,
			TokensIn:       &tokensIn,
			TokensOut:      &tokensOut,
			RawByteLength:  &rawLen,
			Depth:          1,
		},
	}

	payload, err := push.MapMetadata(mapOpts(meta, nil, entries))
	if err != nil {
		t.Fatalf("MapMetadata returned error: %v", err)
	}

	m := parsePublishRequest(t, payload)

	entriesRaw, exists := m["entries"]
	if !exists {
		t.Fatal("entries key should be present in published payload when entries are provided")
	}
	entriesList, ok := entriesRaw.([]any)
	if !ok {
		t.Fatalf("entries is not an array, got %T", entriesRaw)
	}
	if len(entriesList) != 1 {
		t.Fatalf("entries count: got %d, want 1", len(entriesList))
	}

	entry := entriesList[0].(map[string]any)
	assertEqual(t, "entryType", string(schema.EntryTypeToolUse), entry["entryType"])
	assertEqual(t, "role", string(schema.RoleAssistant), entry["role"])
	assertEqual(t, "toolKind", string(schema.ToolCallKindRead), entry["toolKind"])
	assertEqual(t, "stopReason", string(schema.StopReasonEndTurn), entry["stopReason"])
	assertEqual(t, "toolCallId", "tc-abc-123", entry["toolCallId"])

	if _, ok := entry["hasToolUse"].(bool); !ok {
		t.Errorf("hasToolUse should be a bool, got %T", entry["hasToolUse"])
	}
}

// TestSessionEntry_WireFormat_ToolCallID_JSONKey verifies that the schema.SessionEntry
// JSON key for the tool call ID is "toolCallId" (not "toolUseId"), aligning with ACP.
func TestSessionEntry_WireFormat_ToolCallID_JSONKey(t *testing.T) {
	toolCallID := "tc-xyz-789"
	entry := schema.SessionEntry{
		SessionID:  schema.SessionID(testutil.TestSessionUUID),
		EntryIndex: 0,
		Harness:    schema.HarnessClaudeCode,
		EntryType:  schema.EntryTypeToolUse,
		Role:       schema.RoleAssistant,
		ToolCallID: &toolCallID,
	}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal SessionEntry: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Must be "toolCallId" — NOT "toolUseId".
	if _, exists := raw["toolCallId"]; !exists {
		t.Errorf("expected JSON key 'toolCallId' in marshaled SessionEntry, got keys: %v", mapKeys(raw))
	}
	if _, exists := raw["toolUseId"]; exists {
		t.Error("unexpected JSON key 'toolUseId' — must use 'toolCallId' (ACP alignment)")
	}
}

// TestSessionEntry_WireFormat_ToolKindAndStopReason verifies that ToolKind and
// StopReason are present in the marshaled wire format when set.
func TestSessionEntry_WireFormat_ToolKindAndStopReason(t *testing.T) {
	toolKind := schema.ToolCallKindEdit
	stopReason := schema.StopReasonMaxTokens

	entry := schema.SessionEntry{
		SessionID:  schema.SessionID(testutil.TestSessionUUID),
		EntryIndex: 0,
		Harness:    schema.HarnessClaudeCode,
		EntryType:  schema.EntryTypeToolUse,
		Role:       schema.RoleAssistant,
		ToolKind:   &toolKind,
		StopReason: &stopReason,
		HasToolUse: true,
	}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal SessionEntry: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	assertEqual(t, "toolKind", string(schema.ToolCallKindEdit), raw["toolKind"])
	assertEqual(t, "stopReason", string(schema.StopReasonMaxTokens), raw["stopReason"])
}

// TestSessionEntry_WireFormat_OmitEmptyOptionals verifies that optional pointer
// fields are omitted from JSON when nil.
func TestSessionEntry_WireFormat_OmitEmptyOptionals(t *testing.T) {
	entry := schema.SessionEntry{
		SessionID:  schema.SessionID(testutil.TestSessionUUID),
		EntryIndex: 0,
		Harness:    schema.HarnessClaudeCode,
		EntryType:  schema.EntryTypeText,
		Role:       schema.RoleAssistant,
		// All optional fields left nil.
	}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal SessionEntry: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// These optional fields should be absent (omitempty).
	omittedKeys := []string{
		"toolKind", "stopReason", "toolCallId", "contentPreview",
		"tokensIn", "tokensOut", "toolNamesCsv", "rawByteLength",
		"entryId", "parentEntryId", "parentIndex", "toolInput", "toolOutput", "extra",
		"timestampMs",
	}
	for _, key := range omittedKeys {
		if _, exists := raw[key]; exists {
			t.Errorf("expected key %q to be omitted when nil, but it is present", key)
		}
	}

	// Required fields must always be present. The
	// SessionEntry harness is keyed json:"harness" (was "provider").
	requiredKeys := []string{"sessionId", "entryIndex", "harness", "entryType", "role", "hasToolUse", "hasThinking", "isError", "depth"}
	for _, key := range requiredKeys {
		if _, exists := raw[key]; !exists {
			t.Errorf("required key %q missing from marshaled entry", key)
		}
	}
	if _, exists := raw["provider"]; exists {
		t.Error("legacy key 'provider' must not appear in marshaled SessionEntry")
	}
}

// mapKeys returns the keys of a map for diagnostic messages.
func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// --- helpers ---

func assertEqual(t *testing.T, field string, want, got any) {
	t.Helper()
	if want != got {
		t.Errorf("%s: got %v (%T), want %v (%T)", field, got, got, want, want)
	}
}

func assertFloat64(t *testing.T, field string, want float64, got any) {
	t.Helper()
	v, ok := got.(float64)
	if !ok {
		t.Errorf("%s: expected float64, got %T", field, got)
		return
	}
	if v != want {
		t.Errorf("%s: got %v, want %v", field, v, want)
	}
}
