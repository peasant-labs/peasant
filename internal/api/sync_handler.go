package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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

	// Ingest state (protected by mu).
	mu             sync.Mutex
	ingestStatus   string // "idle", "running", "done", "error"
	ingestProgress *ingest.ProgressState
	ingestResult   *ingest.PipelineResult
	ingestError    error
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
	ID          string `json:"id"`
	Harness     string `json:"harness"`
	ProjectName string `json:"projectName"`
	ProjectHash string `json:"projectHash"`
	HostSlug    string `json:"hostSlug"`
	StartTime   string `json:"startTime"`
	DurationMs  int64  `json:"durationMs"`
	TotalTokens int    `json:"totalTokens"`
	TurnCount   int    `json:"turnCount"`
	Model       string `json:"model"`
	SyncStatus  string `json:"syncStatus"`
}

func (h *syncHandler) handleSyncSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())

	if h.store == nil {
		http.Error(w, `{"error":"store not available"}`, http.StatusServiceUnavailable)
		return
	}

	ctx := r.Context()

	// Get all pushable sessions (with pushed_at info).
	sessions, err := h.store.AllPushableSessions(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"query sessions: %s"}`, err), http.StatusInternalServerError)
		return
	}

	// Get held-back sessions (missing metrics).
	heldMap := make(map[string]bool)
	held, heldErr := h.store.SessionsWithoutMetrics(ctx)
	if heldErr == nil {
		for _, session := range held {
			heldMap[session.SessionID] = true
		}
	}

	// Resolve output base path for metadata file checks.
	outputBase := ""
	if h.config != nil {
		resolved, err := ingest.NewResolvedPath(h.config.Output.BasePath)
		if err == nil {
			outputBase = string(resolved)
		}
	}

	// Map to response with sync status.
	result := make([]syncSessionResponse, 0, len(sessions))
	for _, s := range sessions {
		status := computeSyncStatus(s, heldMap, outputBase)
		result = append(result, syncSessionResponse{
			ID:          s.SessionID,
			Harness:     s.ModelHarness,
			ProjectName: s.ProjectName,
			ProjectHash: s.ProjectHash,
			HostSlug:    s.HostSlug,
			StartTime:   time.UnixMilli(s.StartMs).UTC().Format(time.RFC3339),
			DurationMs:  s.DurationMs,
			TotalTokens: s.TokensTotal,
			TurnCount:   s.TurnCount,
			Model:       s.ModelID,
			SyncStatus:  status,
		})
	}

	data, _ := json.Marshal(map[string]any{"sessions": result})
	w.Write(data)
}

// computeSyncStatus determines the sync status for a session row.
// Sessions without metadata files on disk are marked "held" since push would fail.
func computeSyncStatus(s ingest.PushSessionRow, heldMap map[string]bool, outputBase string) string {
	if heldMap[s.SessionID] {
		return "held"
	}
	// Check that the metadata file actually exists on disk.
	if outputBase != "" {
		metaPath := filepath.Join(outputBase, s.HostSlug, s.SessionID, s.SessionID+"--metadata.json")
		if _, err := os.Stat(metaPath); err != nil {
			return "held"
		}
	}
	if s.PushedAt == nil {
		return "new"
	}
	if s.IngestedMs > *s.PushedAt {
		return "updated"
	}
	return "synced"
}

// --------------------------------------------------------------------------
// GET /api/v1/sync/auth — check village credentials
// --------------------------------------------------------------------------

func (h *syncHandler) handleSyncAuth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())

	creds, err := auth.LoadCredentials()
	if err != nil || creds == nil || !creds.IsValid() {
		json.NewEncoder(w).Encode(map[string]any{
			"authenticated": false,
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]any{
		"authenticated":     true,
		"username":          creds.Username,
		"villageUrl":        creds.VillageURL,
		"villageConfigured": creds.VillageURL != "",
	})
}

// --------------------------------------------------------------------------
// GET /api/v1/sync/redactions?session_id=X&level=standard
// --------------------------------------------------------------------------

// maxItemsPerRuleGroup is the maximum number of items returned per rule group
// in the grouped redaction response.
const maxItemsPerRuleGroup = 50

// redactionItem is a single redaction match with context information.
type redactionItem struct {
	Category            redact.CategoryString `json:"category"`
	RuleID              string                `json:"ruleId"`
	RuleDisplayName     string                `json:"ruleDisplayName"`
	OriginalText        string                `json:"originalText"`
	RedactedReplacement string                `json:"redactedReplacement"`
	Description         string                `json:"description"`
	LineNumber          int                   `json:"lineNumber"`
	ContextBefore       []string              `json:"contextBefore"`
	ContextAfter        []string              `json:"contextAfter"`
}

// ruleGroup holds deduplicated items for a single redaction rule, capped at
// maxItemsPerRuleGroup entries.
type ruleGroup struct {
	RuleID      string          `json:"ruleId"`
	DisplayName string          `json:"displayName"`
	Count       int             `json:"count"`
	Items       []redactionItem `json:"items"`
}

// categoryGroup holds all rule groups for a single redaction category.
type categoryGroup struct {
	Category   redact.CategoryString `json:"category"`
	TotalCount int                   `json:"totalCount"`
	Rules      []ruleGroup           `json:"rules"`
}

// groupedRedactionResponse is the response shape for the grouped redaction endpoint.
// The server pre-groups matches by category then by rule so the frontend can
// render an accordion without any client-side aggregation.
type groupedRedactionResponse struct {
	Total      int             `json:"total"`
	Categories []categoryGroup `json:"categories"`
}

// redactionResponse is kept for backward compatibility but the handler now
// returns groupedRedactionResponse.
type redactionResponse = groupedRedactionResponse

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

	// Read transcript content.
	content, err := h.readTranscriptContent(r.Context(), sessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusNotFound)
		return
	}

	// Create redactor at the requested level.
	xdg := redact.XDGPaths{
		DataHome:   string(defaults.ResolveDataDirPath()),
		ConfigHome: string(defaults.ResolveConfigDirPath()),
		StateHome:  string(defaults.ResolveStateDirPath()),
	}
	redactor, err := redact.NewRedactor(redactLevel, userPatterns, xdg)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create redactor: "+err.Error())
		return
	}

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

	for _, m := range matches {
		key := dedupKey{rule: m.Rule, text: m.MatchedText}
		if !seen[key] {
			seen[key] = true
			deduped = append(deduped, m)
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
	ruleItems := make(map[ruleKey][]redactionItem)
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
			ruleItems[rk] = append(ruleItems[rk], redactionItem{
				Category:            cat,
				RuleID:              m.Rule,
				RuleDisplayName:     ruleDisplayName(m.Rule),
				OriginalText:        m.MatchedText,
				RedactedReplacement: replacement,
				Description:         ruleDescription(m.Rule),
				LineNumber:          lineNum,
				ContextBefore:       extractContext(lines, lineNum-1, -2),
				ContextAfter:        extractContext(lines, lineNum-1, 2),
			})
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
	categories := make([]categoryGroup, 0, len(sortedCats))
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

		rules := make([]ruleGroup, 0, len(ruleIDs))
		for _, ruleID := range ruleIDs {
			rk := ruleKey{category: cat, rule: ruleID}
			items := ruleItems[rk]
			if items == nil {
				items = []redactionItem{}
			}
			rules = append(rules, ruleGroup{
				RuleID:      ruleID,
				DisplayName: ruleDisplayName(ruleID),
				Count:       ruleCounts[rk],
				Items:       items,
			})
		}

		categories = append(categories, categoryGroup{
			Category:   cat,
			TotalCount: catCounts[cat],
			Rules:      rules,
		})
	}

	resp := groupedRedactionResponse{
		Total:      len(matches), // total raw matches (before dedup)
		Categories: categories,
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

// readTranscriptContent reads the transcript file for a session.
func (h *syncHandler) readTranscriptContent(ctx context.Context, sessionIDStr string) (string, error) {
	sid, sidErr := ingest.NewSessionID(sessionIDStr)
	if sidErr != nil {
		return "", fmt.Errorf("invalid session ID")
	}

	hostSlug, parentID, err := h.store.LookupSessionLocation(ctx, sid)
	if err != nil || hostSlug == "" {
		return "", fmt.Errorf("session not found")
	}

	dataDir := string(defaults.Data.DataDirPath)
	var sessionDir string
	if parentID != "" {
		sessionDir = filepath.Join(dataDir, "peasant-sync", hostSlug, parentID, "subagents", sessionIDStr)
	} else {
		sessionDir = filepath.Join(dataDir, "peasant-sync", hostSlug, sessionIDStr)
	}

	// Try JSONL first, then JSON.
	for _, ext := range []string{string(ingest.SourceFormatJSONL), string(ingest.SourceFormatJSON)} {
		candidate := filepath.Join(sessionDir, sessionIDStr+defaults.TranscriptPrefix+ext)
		data, err := os.ReadFile(candidate)
		if err == nil {
			return string(data), nil
		}
	}

	return "", fmt.Errorf("transcript file not found")
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
// POST /api/v1/sync/push — execute push pipeline
// --------------------------------------------------------------------------

type pushRequest struct {
	SessionIDs     []string `json:"sessionIds"`
	RedactionLevel string   `json:"redactionLevel"`
	Visibility     string   `json:"visibility"`
}

type pushSessionResult struct {
	SessionID string `json:"sessionId"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	Title     string `json:"title,omitempty"`
}

type pushResponse struct {
	New      int                 `json:"new"`
	Updated  int                 `json:"updated"`
	Skipped  int                 `json:"skipped"`
	Errors   int                 `json:"errors"`
	Sessions []pushSessionResult `json:"sessions"`
}

func (h *syncHandler) handleSyncPush(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())

	if h.config == nil {
		http.Error(w, `{"error":"store or config not available"}`, http.StatusServiceUnavailable)
		return
	}

	// Parse request body.
	var req pushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"invalid request body: %s"}`, err), http.StatusBadRequest)
		return
	}

	if len(req.SessionIDs) == 0 {
		http.Error(w, `{"error":"no session IDs provided"}`, http.StatusBadRequest)
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
		http.Error(w, `{"error":"store or config not available"}`, http.StatusServiceUnavailable)
		return
	}

	// Load credentials.
	creds, err := auth.LoadCredentials()
	if err != nil || creds == nil || !creds.IsValid() {
		http.Error(w, `{"error":"not authenticated — run 'peasant village login' first"}`, http.StatusUnauthorized)
		return
	}
	if creds.VillageURL == "" {
		http.Error(w, `{"error":"village URL not configured — run 'peasant village login' to set your village endpoint"}`, http.StatusBadRequest)
		return
	}

	// Create redactor.
	xdg := redact.XDGPaths{
		DataHome:   string(defaults.ResolveDataDirPath()),
		ConfigHome: string(defaults.ResolveConfigDirPath()),
		StateHome:  string(defaults.ResolveStateDirPath()),
	}
	redactor, err := redact.NewRedactor(level, userPatterns, xdg)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create redactor: "+err.Error())
		return
	}

	// Create village client.
	client := village.NewVillageClient(creds.VillageURL, creds.APIKey, nil)

	// Resolve visibility.
	visibility := schema.Visibility("private")
	if req.Visibility != "" {
		visibility = schema.Visibility(req.Visibility)
	}

	// Resolve ~ in output base path (same as CLI cmd_push.go).
	resolvedOutput, resolveErr := ingest.NewResolvedPath(h.config.Output.BasePath)
	if resolveErr != nil {
		http.Error(w, fmt.Sprintf(`{"error":"resolve output path: %s"}`, resolveErr), http.StatusInternalServerError)
		return
	}
	pushCfg := *h.config
	pushCfg.Output.BasePath = string(resolvedOutput)

	// Build and run the push pipeline.
	pipeline, err := push.NewPipeline(
		h.store,
		client,
		creds,
		&pushCfg,
		&ingest.OSFileSystem{},
		push.PipelineConfig{
			FilterSessionIDs: req.SessionIDs,
			Visibility:       visibility,
		},
		redactor,
		io.Discard, // stderr warnings not needed for web response
	)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create push pipeline: "+err.Error())
		return
	}

	result, err := pipeline.Run(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"push failed: %s"}`, err), http.StatusInternalServerError)
		return
	}

	// Also push annotations (best-effort).
	_, _ = push.PushAnnotations(r.Context(), client, h.store, false)

	// Build response.
	resp := pushResponse{
		New:      result.New,
		Updated:  result.Updated,
		Skipped:  result.Skipped,
		Errors:   result.Errors,
		Sessions: make([]pushSessionResult, 0, len(result.Sessions)),
	}
	for _, s := range result.Sessions {
		psr := pushSessionResult{
			SessionID: s.SessionID,
			Status:    s.Status.String(),
			Title:     s.Title,
		}
		if s.Error != nil {
			psr.Error = s.Error.Error()
		}
		resp.Sessions = append(resp.Sessions, psr)
	}

	data, _ := json.Marshal(resp)
	w.Write(data)
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
	creds, err := auth.LoadCredentials()
	if err == nil && creds != nil && creds.IsValid() && creds.VillageURL != "" {
		json.NewEncoder(w).Encode(map[string]string{"status": "already_authenticated"})
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
	_ = auth.ClearCredentials()

	// Launch the OAuth flow in a background goroutine. auth.Login opens the
	// browser and waits for the callback, so we return immediately.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if _, loginErr := auth.Login(ctx, villageURL, false); loginErr != nil {
			slog.Error("village login failed", "err", loginErr)
		}
	}()

	json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
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
		outputPath = string(defaults.ResolveDataDirPath())
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
	dataDir := string(defaults.ResolveDataDirPath())
	if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
		h.setIngestError(fmt.Errorf("create data directory: %w", err))
		return
	}
	db, err := store.Open(string(defaults.ResolveDBFilePath()))
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
		ingest.WithIndexers(map[defaults.Harness]ingest.TranscriptIndexer{
			defaults.HarnessClaudeCode: ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true)),
			defaults.HarnessOpenCode:   ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullDepth(true)),
		}),
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

	if cfg.Sources.ClaudeCode.Enabled {
		var paths []ingest.ResolvedPath
		for _, p := range cfg.Sources.ClaudeCode.Paths {
			rp, err := ingest.NewResolvedPath(p)
			if err == nil {
				paths = append(paths, rp)
			}
		}
		sources[defaults.HarnessClaudeCode] = ingest.SourceConfig{Paths: paths, Enabled: true}
	}

	if cfg.Sources.OpenCode.Enabled {
		var paths []ingest.ResolvedPath
		for _, p := range cfg.Sources.OpenCode.Paths {
			rp, err := ingest.NewResolvedPath(p)
			if err == nil {
				paths = append(paths, rp)
			}
		}
		sources[defaults.HarnessOpenCode] = ingest.SourceConfig{Paths: paths, Enabled: true}
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
