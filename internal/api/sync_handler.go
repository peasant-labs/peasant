package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// syncHandler serves the web sync/push API endpoints.
type syncHandler struct {
	store  *store.Store
	config *config.Config
	// configHome, dataHome, and stateHome override the XDG roots this handler
	// resolves config, data, and state paths under. Empty keeps the process
	// environment as the default, so only tests inject explicit roots and the
	// CLI keeps its environment-derived defaults. Mirrors ServerConfig.
	configHome string
	dataHome   string
	stateHome  string
	// scopeIssuer mints the opaque member scope a rendered sync helper group
	// carries. The server owns the bounded scope cache; the sync route only
	// consumes the issuer seam so its members replay the sync predicate.
	scopeIssuer groupedScopeIssuer

	// Ingest state (protected by mu).
	mu             sync.Mutex
	ingestStatus   string // "idle", "running", "done", "error"
	ingestProgress *ingest.ProgressState
	ingestResult   *ingest.PipelineResult
	ingestError    error
}

// configDir resolves the config directory this handler reads and writes under,
// preferring the injected XDG_CONFIG_HOME override over the environment.
func (h *syncHandler) configDir() defaults.ConfigDirPath {
	return defaults.ResolveConfigDirPathWith(h.configHome)
}

// dataDir resolves the data directory this handler reads and writes under,
// preferring the injected XDG_DATA_HOME override over the environment.
func (h *syncHandler) dataDir() defaults.DataDirPath {
	return defaults.ResolveDataDirPathWith(h.dataHome)
}

// stateDir resolves the state directory this handler reads under, preferring the
// injected XDG_STATE_HOME override over the environment.
func (h *syncHandler) stateDir() defaults.StateDirPath {
	return defaults.ResolveStateDirPathWith(h.stateHome)
}

// dbPath resolves the analytics database path under dataDir.
func (h *syncHandler) dbPath() defaults.DBFilePath {
	return defaults.ResolveDBFilePathWith(h.dataHome)
}

// credentials loads the stored village credentials from configDir.
func (h *syncHandler) credentials() (*auth.Credentials, error) {
	return auth.LoadCredentialsFrom(h.configHome)
}

func syncUserPatterns(cfg *config.Config) ([]redact.UserPattern, error) {
	if cfg == nil {
		return nil, nil
	}
	patterns, err := config.CustomPatternsToUserPatterns(cfg.Redaction.CustomPatterns)
	if err != nil {
		return nil, fmt.Errorf("prepare configured custom redaction patterns: %w; no transcript was scanned or published; fix redaction.custom_patterns in config.yaml and retry", err)
	}
	return patterns, nil
}

// --------------------------------------------------------------------------
// GET /api/v1/sync/sessions — pushable sessions with sync status
// --------------------------------------------------------------------------

type syncSessionResponse struct {
	ID          string            `json:"id"`
	Harness     string            `json:"harness"`
	ProjectName string            `json:"projectName"`
	ProjectHash string            `json:"projectHash"`
	HostSlug    string            `json:"hostSlug"`
	StartTime   string            `json:"startTime"`
	DurationMs  int64             `json:"durationMs"`
	TotalTokens int               `json:"totalTokens"`
	TurnCount   int               `json:"turnCount"`
	Model       string            `json:"model"`
	SyncStatus  schema.SyncStatus `json:"syncStatus"`
	// HoldReason says why a held row waits. Only a held row carries one.
	HoldReason schema.SyncHoldReason `json:"holdReason,omitempty"`
	// PreviouslyPushed reports that this session was published before, whatever
	// its status now: a held row can have been published.
	PreviouslyPushed bool `json:"previouslyPushed"`
}

func (h *syncHandler) handleSyncSessions(w http.ResponseWriter, r *http.Request) {
	// view is opt-in, exactly as on the sessions route. Omission preserves the
	// exact legacy flat envelope; only the published grouped value selects the
	// grouped sync payload, and any other value is refused rather than served
	// as flat under a name the caller did not ask for.
	view := r.URL.Query().Get("view")
	if view != "" && view != groupedViewValue {
		writeAPIError(w, http.StatusBadRequest,
			fmt.Sprintf("Sync sessions could not be listed because query field \"view\" is %q in internal/api.handleSyncSessions. No sessions were returned, because an unpublished view value cannot be served safely. Omit view for the flat sync list or use view=%s, then retry.", view, groupedViewValue),
			"grouped_view_unknown")
		return
	}
	if view == groupedViewValue {
		h.serveGroupedSyncSessions(w, r)
		return
	}

	var reader syncSessionReader
	if h.store != nil {
		reader = h.store
	}
	serveSyncSessions(w, r, reader)
}

type syncSessionReader interface {
	ingest.PublicationMetadataReader
	AllPushableSessions(context.Context) ([]ingest.PushSessionRow, error)
	SessionsWithoutMetrics(context.Context) ([]ingest.HeldSession, error)
}

var _ syncSessionReader = (*store.Store)(nil)

func serveSyncSessions(w http.ResponseWriter, r *http.Request, db syncSessionReader) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())

	if db == nil {
		http.Error(w, `{"error":"store not available"}`, http.StatusServiceUnavailable)
		return
	}

	entries, err := loadSyncEntries(r.Context(), db)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"query sessions: %s"}`, err), http.StatusInternalServerError)
		return
	}

	result := make([]syncSessionResponse, 0, len(entries))
	for _, entry := range entries {
		s := entry.row
		result = append(result, syncSessionResponse{
			ID:               s.SessionID,
			Harness:          s.ModelHarness,
			ProjectName:      s.ProjectName,
			ProjectHash:      s.ProjectHash,
			HostSlug:         s.HostSlug,
			StartTime:        time.UnixMilli(s.StartMs).UTC().Format(time.RFC3339),
			DurationMs:       s.DurationMs,
			TotalTokens:      s.TokensTotal,
			TurnCount:        s.TurnCount,
			Model:            s.ModelID,
			SyncStatus:       entry.status,
			HoldReason:       entry.hold,
			PreviouslyPushed: s.PushedAt != nil,
		})
	}

	data, _ := json.Marshal(map[string]any{"sessions": result})
	w.Write(data)
}

// computeSyncStatus determines the sync status for a session row, and why a held
// row waits. A session whose metrics are not computed yet is held for its
// metrics; one without a coherent database capture is held for its publication
// metadata. Both wait for normal ingest.
func computeSyncStatus(s ingest.PushSessionRow, metricsMissing, metadataReady bool) (schema.SyncStatus, schema.SyncHoldReason) {
	if metricsMissing {
		return schema.SyncStatusHeld, schema.SyncHoldReasonMetricsMissing
	}
	if !metadataReady {
		return schema.SyncStatusHeld, schema.SyncHoldReasonMetadataMissing
	}
	if s.PushedAt == nil {
		return schema.SyncStatusNew, ""
	}
	if s.IngestedMs > *s.PushedAt {
		return schema.SyncStatusUpdated, ""
	}
	return schema.SyncStatusSynced, ""
}

// --------------------------------------------------------------------------
// GET /api/v1/sync/auth — check village credentials
// --------------------------------------------------------------------------

func (h *syncHandler) handleSyncAuth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())

	creds, err := h.credentials()
	if err != nil || creds == nil || !creds.IsValid() {
		_ = json.NewEncoder(w).Encode(schema.SyncAuthResponse{Authenticated: false})
		return
	}

	_ = json.NewEncoder(w).Encode(schema.SyncAuthResponse{
		Authenticated:     true,
		Username:          creds.Username,
		VillageURL:        creds.VillageURL,
		VillageConfigured: creds.VillageURL != "",
	})
}

// --------------------------------------------------------------------------
// POST /api/v1/sync/logout — end this computer's Village sign-in
// --------------------------------------------------------------------------

// handleSyncLogout removes the stored Village credential. It changes local
// state only: the key stays valid on Village until it is revoked there, which
// `peasant village logout` does. Logging out a computer that holds no
// credential is not an error.
func (h *syncHandler) handleSyncLogout(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())

	// A credential file that cannot be parsed still signs this computer in as
	// far as the user can tell, so it is removed like a valid one.
	creds, loadErr := h.credentials()
	held := creds != nil || loadErr != nil
	if err := auth.ClearCredentialsFrom(h.configHome); err != nil {
		writeAPIError(w, http.StatusInternalServerError,
			"Village sign-out could not remove the stored credential in internal/api.handleSyncLogout: "+err.Error()+". This computer is still signed in. Fix the permissions of the Peasant config directory, then sign out again.",
			"sync_logout_failed")
		return
	}
	status := schema.SyncLogoutAlreadyLoggedOut
	if held {
		status = schema.SyncLogoutLoggedOut
	}
	_ = json.NewEncoder(w).Encode(schema.SyncLogoutResponse{Status: status})
}

// --------------------------------------------------------------------------
// GET /api/v1/sync/redactions?session_id=X&level=standard
// --------------------------------------------------------------------------

// maxItemsPerRuleGroup is the maximum number of items returned per rule group
// in the grouped redaction response.
const maxItemsPerRuleGroup = 50

func (h *syncHandler) handleSyncRedactions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		http.Error(w, `{"error":"missing session_id parameter"}`, http.StatusBadRequest)
		return
	}

	// An omitted level is passed through UNFILLED. This door names no level of its
	// own, and neither does its sibling on the push path: what a missing setting
	// means is one question with one answer, and internal/config is where it is
	// answered. The two doors each used to answer it themselves, which is how the
	// same configuration could mean different protection depending on which one a
	// request arrived at.
	requestedLevel := redact.RedactionLevel(r.URL.Query().Get("level"))
	if requestedLevel != "" && !requestedLevel.IsValid() {
		writeJSONError(w, http.StatusBadRequest, invalidSyncRedactionsLevelMessage(requestedLevel))
		return
	}
	// The level arrives from the query string, so it is a REQUEST rather than a
	// stored setting: it is held to the offered set, not merely to the set this
	// version can apply. A preview built at a level no run of Peasant will ever
	// use would show the caller findings that describe nothing.
	if !config.RedactionLevelOffered(requestedLevel) {
		writeJSONError(w, http.StatusBadRequest, unofferedSyncRedactionsLevelMessage(requestedLevel))
		return
	}
	redactLevel := config.ResolveRedactionPolicy(requestedLevel).Effective
	userPatterns, err := syncUserPatterns(h.config)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if h.store == nil {
		http.Error(w, `{"error":"store not available"}`, http.StatusServiceUnavailable)
		return
	}

	// Create redactor at the requested level.
	xdg := redact.XDGPaths{
		DataHome:   string(h.dataDir()),
		ConfigHome: string(h.configDir()),
		StateHome:  string(h.stateDir()),
	}
	redactor, err := redact.NewRedactor(redactLevel, userPatterns, xdg)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create redactor: "+err.Error())
		return
	}
	document, err := h.readReviewDocument(r.Context(), sessionID, redactor)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	content := document.text

	// Detect matches.
	matches := redactor.Detect(content)

	// Build replacement lookup.
	replacements := buildReplacementLookup()

	// Build line index for line numbers + context extraction.
	lineStarts := buildLineIndex(content)
	lines := strings.Split(content, "\n")

	// Deduplicate matches by (rule, matched text) to avoid showing
	// hundreds of identical "/Users/alice" path matches.
	type dedupKey struct {
		rule string
		text string
	}
	seen := make(map[dedupKey]bool)
	var deduped []redact.Match
	// The item of a deduplicated match names the first turn that shows it. The
	// scanned text starts with the publish metadata, which repeats the turns'
	// text, so the first occurrence overall is usually not in a turn; a later
	// occurrence of the same text under the same rule is.
	type turnLocation struct {
		entryIndex int
		toolCallID string
	}
	firstTurn := make(map[dedupKey]turnLocation)

	for _, m := range matches {
		key := dedupKey{rule: m.Rule, text: m.MatchedText}
		if !seen[key] {
			seen[key] = true
			deduped = append(deduped, m)
		}
		if _, located := firstTurn[key]; !located {
			if entryIndex, toolCallID, inTurn := document.locate(m.Offset); inTurn {
				firstTurn[key] = turnLocation{entryIndex: entryIndex, toolCallID: toolCallID}
			}
		}
	}

	// Group by category → rule.
	// catOrder and ruleOrder track insertion order for stable output.
	type ruleKey struct {
		category redact.CategoryString
		rule     string
	}
	catCounts := make(map[redact.CategoryString]int)
	catOrder := make([]redact.CategoryString, 0)
	ruleItems := make(map[ruleKey][]schema.SyncRedactionItem)
	ruleCounts := make(map[ruleKey]int)
	ruleOrder := make(map[redact.CategoryString][]string) // category → ordered rule IDs

	for _, m := range deduped {
		if err := m.Category.Validate(); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		cat := m.Category.String()
		if catCounts[cat] == 0 {
			catOrder = append(catOrder, cat)
		}
		catCounts[cat]++

		rk := ruleKey{category: cat, rule: m.Rule}
		if ruleCounts[rk] == 0 {
			ruleOrder[cat] = append(ruleOrder[cat], m.Rule)
		}
		ruleCounts[rk]++

		// Only collect up to maxItemsPerRuleGroup items per rule.
		if len(ruleItems[rk]) < maxItemsPerRuleGroup {
			lineNum := offsetToLine(lineStarts, m.Offset)
			replacement, ok := replacements[m.Rule]
			if !ok {
				replacement = "<REDACTED>"
			}
			item := schema.SyncRedactionItem{
				Category:            string(cat),
				RuleID:              m.Rule,
				RuleDisplayName:     ruleDisplayName(m.Rule),
				OriginalText:        m.MatchedText,
				RedactedReplacement: replacement,
				Description:         ruleDescription(m.Rule),
				LineNumber:          lineNum,
				ContextBefore:       extractContext(lines, lineNum-1, -2),
				ContextAfter:        extractContext(lines, lineNum-1, 2),
			}
			if location, inTurn := firstTurn[dedupKey{rule: m.Rule, text: m.MatchedText}]; inTurn {
				entryIndex := location.entryIndex
				item.EntryIndex = &entryIndex
				item.ToolCallID = location.toolCallID
			}
			ruleItems[rk] = append(ruleItems[rk], item)
		}
	}

	// Sort categories by count descending.
	sortedCats := make([]redact.CategoryString, len(catOrder))
	copy(sortedCats, catOrder)
	for i := 0; i < len(sortedCats); i++ {
		for j := i + 1; j < len(sortedCats); j++ {
			if catCounts[sortedCats[j]] > catCounts[sortedCats[i]] {
				sortedCats[i], sortedCats[j] = sortedCats[j], sortedCats[i]
			}
		}
	}

	// Build the grouped response.
	categories := make([]schema.SyncRedactionCategoryGroup, 0, len(sortedCats))
	for _, cat := range sortedCats {
		// Sort rules within category by count descending.
		ruleIDs := make([]string, len(ruleOrder[cat]))
		copy(ruleIDs, ruleOrder[cat])
		for i := 0; i < len(ruleIDs); i++ {
			for j := i + 1; j < len(ruleIDs); j++ {
				rk_i := ruleKey{category: cat, rule: ruleIDs[i]}
				rk_j := ruleKey{category: cat, rule: ruleIDs[j]}
				if ruleCounts[rk_j] > ruleCounts[rk_i] {
					ruleIDs[i], ruleIDs[j] = ruleIDs[j], ruleIDs[i]
				}
			}
		}

		rules := make([]schema.SyncRedactionRuleGroup, 0, len(ruleIDs))
		for _, ruleID := range ruleIDs {
			rk := ruleKey{category: cat, rule: ruleID}
			items := ruleItems[rk]
			if items == nil {
				items = []schema.SyncRedactionItem{}
			}
			rules = append(rules, schema.SyncRedactionRuleGroup{
				RuleID:      ruleID,
				DisplayName: ruleDisplayName(ruleID),
				Count:       ruleCounts[rk],
				Items:       items,
			})
		}

		categories = append(categories, schema.SyncRedactionCategoryGroup{
			Category:   string(cat),
			TotalCount: catCounts[cat],
			Rules:      rules,
		})
	}

	resp := schema.SyncRedactionsResponse{
		Total:      len(matches), // total raw matches (before dedup)
		Categories: categories,
	}
	// The preview is what the consent step shows, so a preview that breaks
	// its own contract is refused rather than shown.
	if err := resp.Validate(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "redaction preview: "+err.Error())
		return
	}

	data, _ := json.Marshal(resp)
	w.Write(data)
}

// extractContext returns up to |count| lines before (negative) or after (positive) lineIdx.
// lineIdx is 0-based. Always returns a non-nil slice (empty, not null in JSON).
func extractContext(lines []string, lineIdx int, count int) []string {
	result := make([]string, 0)
	if count < 0 {
		start := lineIdx + count
		if start < 0 {
			start = 0
		}
		for i := start; i < lineIdx && i < len(lines); i++ {
			result = append(result, truncateLine(lines[i]))
		}
	} else {
		end := lineIdx + 1 + count
		if end > len(lines) {
			end = len(lines)
		}
		for i := lineIdx + 1; i < end; i++ {
			result = append(result, truncateLine(lines[i]))
		}
	}
	return result
}

// truncateLine truncates a line to 200 characters max for context display.
func truncateLine(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// readTranscriptContent assembles scan input from the same capture as publication.
func (h *syncHandler) readTranscriptContent(ctx context.Context, sessionIDStr string) (string, error) {
	return h.readReviewContent(ctx, sessionIDStr, nil)
}

func (h *syncHandler) readReviewContent(ctx context.Context, sessionIDStr string, redactor redact.JSONRedactor) (string, error) {
	document, err := h.readReviewDocument(ctx, sessionIDStr, redactor)
	return document.text, err
}

// reviewDocument is the text the consent scan runs over, with where each turn
// of the transcript lies in it.
type reviewDocument struct {
	text string
	// transcriptStart is where the transcript's review text starts in text,
	// after the metadata line.
	transcriptStart int
	spans           push.ReviewSpans
}

// locate names the turn, and the tool call when there is one, that holds the
// byte at offset in the document's text.
func (d reviewDocument) locate(offset int) (entryIndex int, toolCallID string, ok bool) {
	if offset < d.transcriptStart {
		return 0, "", false
	}
	return d.spans.Locate(offset - d.transcriptStart)
}

func (h *syncHandler) readReviewDocument(ctx context.Context, sessionIDStr string, redactor redact.JSONRedactor) (reviewDocument, error) {
	input, detail, err := push.LoadPublicationInput(ctx, h.store, sessionIDStr)
	if err != nil {
		return reviewDocument{}, err
	}
	if err := push.ValidatePublicationInput(input); err != nil {
		return reviewDocument{}, err
	}
	var fields config.PushFieldVisibility
	if h.config != nil {
		fields = h.config.Push.Fields
	}
	// Validate the same recursive redaction used by publication before reporting
	// a successful scan. A metadata key collision must remain a failed scan.
	redacted, err := push.RedactEntries(redactor, input.Entries)
	if err != nil {
		return reviewDocument{}, err
	}
	if _, err := push.BuildTranscriptContentValidated(&input.Metadata, redacted, defaults.PublishSchemaVersion, fields, input.SessionOrigin); err != nil {
		return reviewDocument{}, err
	}
	// The review scan must see the exact bytes the publish will carry, so it
	// builds the same envelope through the shared durable-first builder and
	// derives the same capability requirements the upload gate will enforce.
	content, err := push.BuildPublishTranscriptContent(detail, &input.Metadata, input.Entries, defaults.PublishSchemaVersion, fields, input.SessionOrigin)
	if err != nil {
		return reviewDocument{}, err
	}
	metadata, err := push.MapMetadata(push.MapOptions{Meta: &input.Metadata, Metrics: input.Quality, Entries: input.Entries, Associations: input.Associations, Fields: fields.Resolve()})
	if err != nil {
		return reviewDocument{}, err
	}
	if _, err := push.ScanPublication(content); err != nil {
		return reviewDocument{}, err
	}
	data, err := push.PublicationReviewText(content, redactor)
	if err != nil {
		return reviewDocument{}, err
	}
	spans, err := push.PublicationReviewSpans(content)
	if err != nil {
		return reviewDocument{}, err
	}
	return reviewDocument{text: string(metadata) + "\n" + data, transcriptStart: len(metadata) + 1, spans: spans}, nil
}

// buildReplacementLookup builds a map from rule ID to replacement string.
func buildReplacementLookup() map[string]string {
	m := make(map[string]string, len(redact.Rules))
	for _, rule := range redact.Rules {
		m[rule.ID] = displayReplacement(rule.Replacement)
	}
	return m
}

// displayReplacement strips regex back-references ($1, ${2}, etc.) from a
// replacement string. These are expanded by ReplaceAllString during actual
// redaction but are meaningless when displayed in the UI.
func displayReplacement(replacement string) string {
	return backrefPattern.ReplaceAllString(replacement, "")
}

var backrefPattern = regexp.MustCompile(`\$\{?\d+\}?`)

// buildLineIndex returns byte offsets of each line start.
func buildLineIndex(content string) []int {
	starts := []int{0}
	for i, b := range content {
		if b == '\n' && i+1 < len(content) {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// offsetToLine converts a byte offset to a 1-based line number.
func offsetToLine(lineStarts []int, offset int) int {
	lo, hi := 0, len(lineStarts)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		if lineStarts[mid] <= offset {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return lo // 1-based: lo is the count of starts <= offset
}

// ruleDisplayName maps a rule ID to a short human-readable label.
// Used as the heading in the accordion rule group.
func ruleDisplayName(ruleID string) string {
	names := map[string]string{
		// Secrets
		"anthropic_key":              "Anthropic API Key",
		"openai_key":                 "OpenAI API Key",
		"github_pat":                 "GitHub Personal Access Token",
		"aws_access_key":             "AWS Access Key ID",
		"aws_secret_key":             "AWS Secret Key",
		"stripe_key":                 "Stripe API Key",
		"twilio_key":                 "Twilio API Key",
		"sendgrid_key":               "SendGrid API Key",
		"slack_token":                "Slack Token",
		"jwt_token":                  "JWT Token",
		"private_key_block":          "Private Key Block",
		"generic_api_key":            "Generic API Key",
		"bearer_token":               "Bearer Token",
		"basic_auth":                 "Basic Auth Credentials",
		"artifactory_api_token":      "Artifactory API Token",
		"artifactory_password":       "Artifactory Password",
		"azure_storage_key":          "Azure Storage Key",
		"discord_bot_token":          "Discord Bot Token",
		"gitlab_pat":                 "GitLab Personal Access Token",
		"gitlab_runner_registration": "GitLab Runner Token",
		"gitlab_cicd":                "GitLab CI/CD Token",
		"gitlab_incoming_mail":       "GitLab Mail Token",
		"gitlab_trigger":             "GitLab Trigger Token",
		"gitlab_agent":               "GitLab Agent Token",
		"gitlab_oauth_secret":        "GitLab OAuth Secret",
		"mailchimp_api_key":          "Mailchimp API Key",
		"npm_auth_token":             "npm Auth Token",
		"pypi_api_token":             "PyPI API Token",
		"pypi_test_token":            "PyPI Test Token",
		"square_oauth_secret":        "Square OAuth Secret",
		"telegram_bot_token":         "Telegram Bot Token",
		// PII
		"email":       "Email Address",
		"phone_us":    "Phone Number",
		"ssn":         "Social Security Number",
		"credit_card": "Credit Card Number",
		"ip_address":  "IP Address",
		// Paths
		"unix_home_path":      "Unix Home Path",
		"windows_home_path":   "Windows Home Path",
		"claude_project_slug": "Claude Project Slug",
		"peasant_host_slug":   "Peasant Host Slug",
	}
	if name, ok := names[ruleID]; ok {
		return name
	}
	// Fallback: title-case the rule ID.
	parts := strings.Split(ruleID, "_")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

// ruleDescription maps a rule ID to a human-readable description.
func ruleDescription(ruleID string) string {
	descriptions := map[string]string{
		// Secrets
		"anthropic_key":              "Anthropic API key detected",
		"openai_key":                 "OpenAI API key detected",
		"github_pat":                 "GitHub personal access token detected",
		"aws_access_key":             "AWS access key ID detected",
		"aws_secret_key":             "AWS secret key detected",
		"stripe_key":                 "Stripe API key detected",
		"twilio_key":                 "Twilio API key detected",
		"sendgrid_key":               "SendGrid API key detected",
		"slack_token":                "Slack token detected",
		"jwt_token":                  "JWT token detected",
		"private_key_block":          "Private key block detected",
		"generic_api_key":            "Generic API key detected",
		"bearer_token":               "Bearer token detected",
		"basic_auth":                 "Basic auth credentials detected",
		"artifactory_api_token":      "Artifactory API token detected",
		"artifactory_password":       "Artifactory password detected",
		"azure_storage_key":          "Azure storage key detected",
		"discord_bot_token":          "Discord bot token detected",
		"gitlab_pat":                 "GitLab personal access token detected",
		"gitlab_runner_registration": "GitLab runner registration token detected",
		"gitlab_cicd":                "GitLab CI/CD token detected",
		"gitlab_incoming_mail":       "GitLab incoming mail token detected",
		"gitlab_trigger":             "GitLab trigger token detected",
		"gitlab_agent":               "GitLab agent token detected",
		"gitlab_oauth_secret":        "GitLab OAuth application secret detected",
		"mailchimp_api_key":          "Mailchimp API key detected",
		"npm_auth_token":             "npm auth token detected",
		"pypi_api_token":             "PyPI API token detected",
		"pypi_test_token":            "PyPI test token detected",
		"square_oauth_secret":        "Square OAuth secret detected",
		"telegram_bot_token":         "Telegram bot token detected",
		// PII
		"email":       "Email address detected",
		"phone_us":    "Phone number detected",
		"ssn":         "Social security number detected",
		"credit_card": "Credit card number detected",
		"ip_address":  "IP address detected",
		// Paths
		"unix_home_path":      "Unix home directory path detected",
		"windows_home_path":   "Windows home directory path detected",
		"claude_project_slug": "Claude project path slug with username detected",
		"peasant_host_slug":   "Peasant host slug with username detected",
	}
	if desc, ok := descriptions[ruleID]; ok {
		return desc
	}
	// Fallback: humanize the rule ID.
	return strings.ReplaceAll(ruleID, "_", " ") + " detected"
}

// --------------------------------------------------------------------------
// POST /api/v1/sync/push — publish sessions and change who can read them
// --------------------------------------------------------------------------

func (h *syncHandler) handleSyncPush(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())

	if h.config == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "store or config not available", "")
		return
	}

	// The request is typed and closed: an unknown field, such as a visibility or
	// a license a stale client still sends, is refused before anything runs.
	req, changes, err := decodeSyncPushRequest(r.Body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), syncPushInvalidRequestCode)
		return
	}

	// Resolve and validate the requested level before credential access. Invalid
	// request data should produce the same 400 response whether or not this
	// workstation is authenticated.
	// Unfilled when absent, exactly as on the preview door above: no surface here
	// names a redaction level, so there is no second answer to keep in step.
	requestedLevel := redact.RedactionLevel(req.RedactionLevel)
	if requestedLevel != "" && !requestedLevel.IsValid() {
		writeJSONError(
			w,
			http.StatusBadRequest,
			invalidSyncPushLevelMessage(requestedLevel),
		)
		return
	}
	// The level arrives from the request body, so like the preview query it is a
	// REQUEST and is held to the offered set.
	//
	// It also resolves through the SAME shared policy the CLI push uses. Each of
	// the two push surfaces used to carry its own protection floor, and they were
	// set to different levels, so the same session published under the same
	// configuration was protected differently depending on which one sent it.
	// There is now one resolution and no floor argument for a surface to get
	// wrong. An unset level reaches it unfilled and resolves to the one default.
	if !config.RedactionLevelOffered(requestedLevel) {
		writeJSONError(w, http.StatusBadRequest, unofferedSyncPushLevelMessage(requestedLevel))
		return
	}
	level := config.ResolveRedactionPolicy(requestedLevel).Effective
	userPatterns, err := syncUserPatterns(h.config)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if h.store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "store or config not available", "")
		return
	}

	// Load credentials.
	creds, err := h.credentials()
	if err != nil || creds == nil || !creds.IsValid() {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated — run 'peasant village login' first", "")
		return
	}
	if creds.VillageURL == "" {
		writeAPIError(w, http.StatusBadRequest, "village URL not configured — run 'peasant village login' to set your village endpoint", "")
		return
	}

	// Create redactor.
	xdg := redact.XDGPaths{
		DataHome:   string(h.dataDir()),
		ConfigHome: string(h.configDir()),
		StateHome:  string(h.stateDir()),
	}
	redactor, err := redact.NewRedactor(level, userPatterns, xdg)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create redactor: "+err.Error())
		return
	}

	client := village.NewVillageClient(creds.VillageURL, creds.APIKey, nil)

	// Resolve ~ in output base path (same as CLI cmd_push.go).
	resolvedOutput, resolveErr := ingest.NewResolvedPath(h.config.Output.BasePath)
	if resolveErr != nil {
		writeJSONError(w, http.StatusInternalServerError, "resolve output path: "+resolveErr.Error())
		return
	}
	pushCfg := *h.config
	pushCfg.Output.BasePath = string(resolvedOutput)

	// A publish from the local web is for collectives: a first publication
	// opens private with no license, and the collective steps decide who can
	// read it. An update keeps the audience and license it has.
	pipeline, err := push.NewSharePipeline(h.store, client, creds, &pushCfg, redactor, req.SessionIDs)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create push pipeline: "+err.Error())
		return
	}
	published, err := push.SharePublish{Pipeline: pipeline, Store: h.store, Village: client, Creds: creds}.Run(r.Context(), req.SessionIDs, changes)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "push failed: "+err.Error())
		return
	}

	// Also push annotations (best-effort), but only the ones that belong to a
	// session this request published. The user chose those sessions; an
	// annotation on any other session, or on no session at all, is outside what
	// they chose to share.
	_, _ = push.PushAnnotationsSelected(r.Context(), client, h.store, push.AnnotationSelection{}.WithinPublishedSessions(published.Pushed), false, push.DefaultConcurrency)

	_ = json.NewEncoder(w).Encode(published.Response)
}

// syncPushInvalidRequestCode marks a push request the typed contract refuses.
const syncPushInvalidRequestCode = "sync_push_invalid_request"

// syncPushRequestLimit bounds the push request body. A request names sessions
// and collectives by identifier, so a megabyte holds thousands of each.
const syncPushRequestLimit = 1 << 20

// nullCollectiveList says why the body is refused when it sends collectives,
// or one of its lists, as null. The contract declares them non-nullable, and
// the typed decode would read null as an omitted list, so it is checked on the
// raw body. It returns "" when the body sends no null list or is not an object.
func nullCollectiveList(raw []byte) string {
	var request struct {
		Collectives json.RawMessage `json:"collectives"`
	}
	if json.Unmarshal(raw, &request) != nil || request.Collectives == nil {
		return ""
	}
	if isJSONNull(request.Collectives) {
		return "collectives is null; omit it to keep each transcript's audience"
	}
	var lists struct {
		Add    json.RawMessage `json:"add"`
		Remove json.RawMessage `json:"remove"`
	}
	if json.Unmarshal(request.Collectives, &lists) != nil {
		return ""
	}
	if isJSONNull(lists.Add) {
		return "collectives.add is null; omit a list you do not change"
	}
	if isJSONNull(lists.Remove) {
		return "collectives.remove is null; omit a list you do not change"
	}
	return ""
}

func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// villageUUIDPattern is the canonical lowercase form Village emits for a
// collective, and the form the contract declares for collectives.add and
// collectives.remove.
var villageUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// decodeSyncPushRequest reads the typed push request strictly: one JSON object
// with only the declared fields, a valid session list, and collectives named by
// their Village identifier, each in one list. It runs before any read or send.
func decodeSyncPushRequest(body io.Reader) (schema.SyncPushRequest, push.CollectiveChanges, error) {
	var req schema.SyncPushRequest
	refuse := func(why string) (schema.SyncPushRequest, push.CollectiveChanges, error) {
		return schema.SyncPushRequest{}, push.CollectiveChanges{}, fmt.Errorf("what: the push request is not one this server accepts\nwhy: %s\nwhere: the JSON body of POST /api/v1/sync/push\nmeans: nothing was scanned, published, shared, or taken back\nfix: send {\"sessionIds\": [...], \"redactionLevel\"?: ..., \"collectives\"?: {\"add\"?: [...], \"remove\"?: [...]}} with no other field, then retry", why)
	}
	raw, err := io.ReadAll(io.LimitReader(body, syncPushRequestLimit+1))
	if err != nil {
		return refuse(fmt.Sprintf("the body could not be read: %v", err))
	}
	if len(raw) > syncPushRequestLimit {
		return refuse(fmt.Sprintf("the body is larger than %d bytes", syncPushRequestLimit))
	}
	if why := nullCollectiveList(raw); why != "" {
		return refuse(why)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		why := fmt.Sprintf("the body could not be read as the typed request: %v", err)
		if strings.Contains(err.Error(), "unknown field") {
			why += "; the request names only sessions, a redaction level, and collectives, and carries no visibility or license, because publishing from the local web is for collectives"
		}
		return refuse(why)
	}
	if decoder.More() {
		return refuse("the body holds more than one JSON value")
	}
	if err := req.Validate(); err != nil {
		return refuse(err.Error())
	}
	var changes push.CollectiveChanges
	if req.Collectives != nil {
		for _, list := range []struct {
			name string
			ids  []schema.VillageUUID
		}{{"collectives.add", req.Collectives.Add}, {"collectives.remove", req.Collectives.Remove}} {
			for _, id := range list.ids {
				if !villageUUIDPattern.MatchString(id.String()) {
					return refuse(fmt.Sprintf("%s names %q, which is not a Village collective identifier; name each collective by its lowercase Village UUID", list.name, id))
				}
			}
		}
		changes = push.CollectiveChanges{Add: req.Collectives.Add, Remove: req.Collectives.Remove}
	}
	return req, changes, nil
}

func invalidSyncRedactionsLevelMessage(level redact.RedactionLevel) string {
	return strings.Join([]string{
		fmt.Sprintf("what: redaction level %q is invalid", level),
		"why: " + config.OfferedRedactionLevelsSentence(),
		`where: query field "level" on GET /api/v1/sync/redactions`,
		"when: validating the request before transcript lookup, redactor construction, or scanning",
		"means: no scan was started and no transcript content was classified",
		"fix: set level to " + config.RedactionLevelChoicePhrase() + " and retry",
	}, "\n")
}

// unofferedSyncRedactionsLevelMessage answers a level that IS a known level but is
// not one a caller may ask for. It is a client error, not a server error: the
// request named something the product does not offer.
//
// The "why" line is chosen from the level's disposition rather than written once,
// because the two reasons are different facts and a single blended sentence would
// be false for one of them. A level this version cannot apply and a level it
// simply no longer offers are not the same refusal.
func unofferedSyncRedactionsLevelMessage(level redact.RedactionLevel) string {
	return strings.Join([]string{
		fmt.Sprintf("what: redaction level %q is not a level this version offers", level),
		unofferedLevelReason(level, "a preview at that level would not describe any run Peasant can perform"),
		`where: query field "level" on GET /api/v1/sync/redactions`,
		"when: validating the request before transcript lookup, redactor construction, or scanning",
		"means: no scan was started and no transcript content was classified",
		"fix: set level to " + config.RedactionLevelChoicePhrase() + " and retry",
	}, "\n")
}

// unofferedLevelReason is the "why:" line for a level a caller may not ask for.
//
// The reason itself comes from config.RedactionRefusalReason - the same call the
// CLI's typed refusals make - so the two request surfaces and the command line
// cannot answer the same question differently. This wrapper adds only the "why:"
// prefix the local API's message shape uses.
//
// It used to restate the reason here instead. Nothing compared the two, each was
// pinned by its own fixtures, and a maintainer improving one would have shipped a
// green suite in which the CLI and POST /api/v1/sync/push gave different accounts
// of why the same level was refused.
func unofferedLevelReason(level redact.RedactionLevel, consequence string) string {
	return "why: " + config.RedactionRefusalReason(level, consequence)
}

func invalidSyncPushLevelMessage(level redact.RedactionLevel) string {
	return strings.Join([]string{
		fmt.Sprintf("what: redaction level %q is invalid", level),
		"why: " + config.OfferedRedactionLevelsSentence(),
		`where: JSON field "redactionLevel" on POST /api/v1/sync/push`,
		"when: validating the request before credential access, redactor construction, or push setup",
		"means: no scan or push was started and no content left the machine",
		"fix: set redactionLevel to " + config.RedactionLevelChoicePhrase() + " and retry",
	}, "\n")
}

// unofferedSyncPushLevelMessage refuses a push naming a level a caller may not
// ask for, rather than publishing under a level other than the one requested.
func unofferedSyncPushLevelMessage(level redact.RedactionLevel) string {
	return strings.Join([]string{
		fmt.Sprintf("what: redaction level %q is not a level this version offers", level),
		unofferedLevelReason(level, "publishing at it would not apply the protection the request named"),
		`where: JSON field "redactionLevel" on POST /api/v1/sync/push`,
		"when: validating the request before credential access, redactor construction, or push setup",
		"means: no scan or push was started and no content left the machine",
		"fix: set redactionLevel to " + config.RedactionLevelChoicePhrase() + " and retry",
	}, "\n")
}

// --------------------------------------------------------------------------
// POST /api/v1/sync/login — start village OAuth login flow
// --------------------------------------------------------------------------

func (h *syncHandler) handleSyncLogin(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())

	// If already fully authenticated (valid creds + village URL), nothing to do.
	creds, err := h.credentials()
	if err == nil && creds != nil && creds.IsValid() && creds.VillageURL != "" {
		_ = json.NewEncoder(w).Encode(schema.SyncLoginResponse{Status: schema.SyncLoginAlreadyAuthenticated})
		return
	}

	// Resolve village URL using the same precedence as the CLI:
	// env var → config file → production default.
	villageURL := defaults.DefaultVillageURL.String()
	if h.config != nil && h.config.Village.URL != "" {
		villageURL = h.config.Village.URL
	}

	// Clear any stale credentials so auth.Login doesn't short-circuit with
	// "already logged in" when creds exist but are invalid/expired.
	_ = auth.ClearCredentialsFrom(h.configHome)

	// Launch the OAuth flow in a background goroutine. auth.Login opens the
	// browser and waits for the callback, so we return immediately.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if _, loginErr := auth.LoginFrom(ctx, villageURL, false, h.configHome, nil); loginErr != nil {
			slog.Error("village login failed", "err", loginErr)
		}
	}()

	_ = json.NewEncoder(w).Encode(schema.SyncLoginResponse{Status: schema.SyncLoginPending})
}

// --------------------------------------------------------------------------
// POST /api/v1/sync/ingest — run ingest pipeline
// --------------------------------------------------------------------------

func (h *syncHandler) handleSyncIngest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())

	if h.config == nil {
		http.Error(w, `{"error":"config not available"}`, http.StatusServiceUnavailable)
		return
	}

	h.mu.Lock()
	if h.ingestStatus == "running" {
		h.mu.Unlock()
		http.Error(w, `{"error":"ingest already running"}`, http.StatusConflict)
		return
	}

	progState := ingest.NewProgressState()
	h.ingestStatus = "running"
	h.ingestProgress = progState
	h.ingestResult = nil
	h.ingestError = nil
	h.mu.Unlock()

	go h.runIngestPipeline(progState)

	json.NewEncoder(w).Encode(map[string]string{"status": "running"})
}

// runIngestPipeline constructs and runs the full ingest pipeline, mirroring
// the pattern from buildFTUEIngestRunner in cmd/peasant/cmd_kickstart.go.
func (h *syncHandler) runIngestPipeline(progState *ingest.ProgressState) {
	cfg := h.config
	fs := &ingest.OSFileSystem{}
	git := &ingest.ExecGitResolver{}

	// Build source configs from the loaded config.
	sources := buildWebSourceConfigs(cfg)

	outputPath := cfg.Output.BasePath
	if outputPath == "" {
		outputPath = string(h.dataDir())
	}
	resolvedOutput, err := ingest.NewResolvedPath(outputPath)
	if err != nil {
		h.setIngestError(fmt.Errorf("resolve output path: %w", err))
		return
	}

	staleness := time.Duration(cfg.Output.StalenessThresholdSec) * time.Second
	if staleness == 0 {
		staleness = time.Duration(defaults.ConfigStalenessThresholdSec) * time.Second
	}

	pipelineCfg := ingest.PipelineConfig{
		Sources:            sources,
		OutputDir:          resolvedOutput,
		StalenessThreshold: staleness,
		Progress:           progState,
	}

	// Open DB and wire analytics stages.
	dataDir := string(h.dataDir())
	if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
		h.setIngestError(fmt.Errorf("create data directory: %w", err))
		return
	}
	db, err := store.Open(string(h.dbPath()))
	if err != nil {
		h.setIngestError(fmt.Errorf("open analytics store: %w", err))
		return
	}
	defer db.Close()

	pipelineOpts := []ingest.PipelineOption{
		ingest.WithStore(db),
		ingest.WithMetricsStore(db),
	}

	// Refuse an import under a level this version cannot apply, rather than
	// storing transcripts the user believes were anonymised at import time. No
	// supported level redacts while ingest writes: that is the deferred redaction
	// model, and redaction happens on the way out.
	if !config.RedactionLevelSupported(cfg.Redaction.Level) {
		h.setIngestError(&config.UnsupportedRedactionLevelError{
			Level:     cfg.Redaction.Level,
			Source:    "the loaded configuration",
			Operation: "the web ingest",
			Step:      "before any transcript was read or written",
			Impact:    "Nothing was imported and nothing already imported was changed.",
		})
		return
	}

	pipelineOpts = append(pipelineOpts,
		ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
		ingest.WithAnalyzer(metrics.NewEngineWithModels(db, db)),
		ingest.WithClassifier(metrics.NewClassifierAnnotator(db, db)),
		ingest.WithLogger(db),
		ingest.WithIndexLogger(db),
	)

	pipeline, err := ingest.NewPipeline(fs, git, ingest.DefaultAdapterRegistry, pipelineCfg, pipelineOpts...)
	if err != nil {
		h.setIngestError(fmt.Errorf("create pipeline: %w", err))
		return
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		h.setIngestError(fmt.Errorf("pipeline failed: %w", err))
		return
	}

	h.mu.Lock()
	h.ingestStatus = "done"
	h.ingestResult = result
	h.mu.Unlock()
}

// setIngestError records a pipeline error under the mutex.
func (h *syncHandler) setIngestError(err error) {
	h.mu.Lock()
	h.ingestStatus = "error"
	h.ingestError = err
	h.mu.Unlock()
}

// buildWebSourceConfigs builds source configs from the application config,
// mirroring cmd/peasant/cmd_harvest.go:buildSourceConfigs.
func buildWebSourceConfigs(cfg *config.Config) map[defaults.Harness]ingest.SourceConfig {
	sources := map[defaults.Harness]ingest.SourceConfig{}

	for harness := range ingest.DefaultAdapterRegistry {
		configured, ok := cfg.Sources.Provider(harness)
		if !ok || !configured.Enabled {
			continue
		}
		var paths []ingest.ResolvedPath
		for _, p := range configured.Paths {
			rp, err := ingest.NewResolvedPath(p)
			if err == nil {
				paths = append(paths, rp)
			}
		}
		sources[harness] = ingest.SourceConfig{Paths: paths, Enabled: true}
	}

	return sources
}

// --------------------------------------------------------------------------
// GET /api/v1/sync/ingest/status — poll ingest pipeline state
// --------------------------------------------------------------------------

func (h *syncHandler) handleSyncIngestStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())

	h.mu.Lock()
	status := h.ingestStatus
	prog := h.ingestProgress
	result := h.ingestResult
	ingestErr := h.ingestError
	h.mu.Unlock()

	if status == "" {
		status = "idle"
	}

	resp := map[string]any{"status": status}

	if prog != nil {
		snap := prog.Snapshot()
		progress := make(map[string]any, len(snap))
		for stage, sp := range snap {
			progress[stage.String()] = map[string]any{
				"total": sp.Total,
				"done":  sp.Done,
				"ended": sp.Ended,
			}
		}
		resp["progress"] = progress
	}

	if result != nil {
		resp["result"] = map[string]any{
			"new":       result.Summary.New,
			"updated":   result.Summary.Updated,
			"unchanged": result.Summary.Unchanged,
			"errors":    result.Summary.Errors,
			"indexed":   result.Summary.Indexed,
			"computed":  result.Summary.Computed,
			"duration":  result.Duration.String(),
		}
	}

	if ingestErr != nil {
		resp["error"] = ingestErr.Error()
	}

	data, _ := json.Marshal(resp)
	w.Write(data)
}
