package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"

	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// autoPublishHandler serves the auto-publish rule routes of the settings page:
// save a rule, remove it, and install its hooks in one recorded repository.
// Saving or removing a rule changes no hook; installing is one explicit call
// per repository, and only in a repository Peasant has recorded sessions in.
type autoPublishHandler struct {
	store *store.Store
	// config is the configuration the server applies, including every
	// setting saved through the settings routes since it started.
	config *liveConfig
	// configHome is the XDG config root this server runs under; the rules
	// file lives in its config directory.
	configHome string
	// binding is bound into every hook the server installs, so the hook reads
	// the same rules, configuration, and store as the server.
	binding githooks.Binding
	// mu serializes this server's rule changes and installs.
	mu sync.Mutex
}

// Error codes of the auto-publish routes.
const (
	autoPublishInvalidCode     = "auto_publish_rule_invalid"
	autoPublishNotFoundCode    = "auto_publish_rule_not_found"
	autoPublishNotCoveredCode  = "auto_publish_repository_not_covered"
	autoPublishUnavailableCode = "auto_publish_unavailable"
)

// errNoRule reports that no rule has the identifier.
var errNoRule = errors.New("no auto-publish rule has this identifier")

// errAutoPublishStoreUnavailable reports that the server runs without the
// session store that names the recorded repositories.
var errAutoPublishStoreUnavailable = errors.New("this server runs without its session store, which names the repositories Peasant recorded")

func (h *autoPublishHandler) rulesPath() string {
	return autopublish.Path(defaults.ResolveConfigDirPathWith(h.configHome))
}

func (h *autoPublishHandler) hooks() autopublish.Hooks {
	return autopublish.Hooks{Lifecycle: githooks.New(githooks.NewExecGit()), Binding: h.binding}
}

// recorded lists the repositories Peasant has recorded sessions in.
func (h *autoPublishHandler) recorded(r *http.Request) ([]autopublish.Repository, error) {
	return autopublish.Recorded(r.Context(), h.store, &ingest.ExecGitResolver{})
}

// listRules reads every saved rule as the save route answers it: the recorded
// repositories it covers, with their hooks as they are. A rule that cannot be
// read fails the list rather than drop out of it, because a rule decides who
// can read a transcript.
func (h *autoPublishHandler) listRules(r *http.Request) ([]schema.AutoPublishRule, error) {
	rules, err := autopublish.Load(h.rulesPath())
	if err != nil {
		return nil, err
	}
	views := make([]schema.AutoPublishRule, 0, len(rules))
	if len(rules) == 0 {
		return views, nil
	}
	if h.store == nil {
		return nil, errAutoPublishStoreUnavailable
	}
	recorded, err := h.recorded(r)
	if err != nil {
		return nil, fmt.Errorf("list the recorded repositories: %w", err)
	}
	hooks := h.hooks()
	for _, rule := range rules {
		view, err := hooks.View(r.Context(), rule, recorded)
		if err != nil {
			return nil, fmt.Errorf("read the hooks of auto-publish rule %s: %w", rule.ID, err)
		}
		views = append(views, view)
	}
	return views, nil
}

// ready answers 503 when the server runs without its session store, which
// holds the recorded repositories.
func (h *autoPublishHandler) ready(w http.ResponseWriter) bool {
	if h.store != nil {
		return true
	}
	writeAPIError(w, http.StatusServiceUnavailable,
		"The auto-publish rules could not be changed because this server runs without its session store, which names the repositories Peasant recorded. Nothing was changed. Start Peasant with its normal store, then retry.",
		autoPublishUnavailableCode)
	return false
}

// --------------------------------------------------------------------------
// PUT /api/v1/settings/auto-publish/{id}
// --------------------------------------------------------------------------

// handleSaveRule creates or replaces one rule. It installs nothing: the answer
// lists the recorded repositories the rule covers and each one's hooks.
func (h *autoPublishHandler) handleSaveRule(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	if !h.ready(w) {
		return
	}
	id := r.PathValue("id")
	var request schema.AutoPublishRuleRequest
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, ruleBodyLimit), &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, "The auto-publish rule "+id+" was not saved because the body is not a rule: "+err.Error()+". Nothing was changed. Send kind, match, events, and collectives, then retry.", autoPublishInvalidCode)
		return
	}
	rule := autopublish.RuleFromRequest(id, request)
	err := request.Validate()
	if err == nil {
		err = rule.Validate()
	}
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "The auto-publish rule was not saved: "+err.Error()+". Nothing was changed.", autoPublishInvalidCode)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	err = autopublish.Update(h.rulesPath(), func(rules []autopublish.Rule) ([]autopublish.Rule, error) {
		if i := slices.IndexFunc(rules, func(existing autopublish.Rule) bool { return existing.ID == id }); i >= 0 {
			rules[i] = rule
			return rules, nil
		}
		return append(rules, rule), nil
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "The auto-publish rule was not saved: "+err.Error()+".", "")
		return
	}
	recorded, err := h.recorded(r)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "The auto-publish rule "+id+" was saved, but its repositories could not be listed: "+err.Error()+". Reload the settings page.", "")
		return
	}
	view, err := h.hooks().View(r.Context(), rule, recorded)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "The auto-publish rule "+id+" was saved, but its hooks could not be read: "+err.Error()+". Reload the settings page.", "")
		return
	}
	writeContract(w, &view)
}

// --------------------------------------------------------------------------
// DELETE /api/v1/settings/auto-publish/{id}
// --------------------------------------------------------------------------

// handleDeleteRule removes a binding. Rule-installed hooks remain on disk,
// but require an active rule before sending anything; a retained binding can
// keep them active, while a separately installed terminal hook keeps its own
// publication consent.
func (h *autoPublishHandler) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	if !h.ready(w) {
		return
	}
	id := r.PathValue("id")
	h.mu.Lock()
	defer h.mu.Unlock()
	var removed autopublish.Rule
	recorded, err := h.recorded(r)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "The rule was not removed because its recorded repositories could not be read: "+err.Error()+". Retry.", "")
		return
	}
	err = autopublish.Update(h.rulesPath(), func(rules []autopublish.Rule) ([]autopublish.Rule, error) {
		i := slices.IndexFunc(rules, func(existing autopublish.Rule) bool { return existing.ID == id })
		if i < 0 {
			return nil, errNoRule
		}
		removed = rules[i]
		return slices.Delete(rules, i, i+1), nil
	})
	if errors.Is(err, errNoRule) {
		writeAPIError(w, http.StatusNotFound, "No auto-publish rule has the identifier "+id+". Nothing was changed.", autoPublishNotFoundCode)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "The auto-publish rule "+id+" was not removed: "+err.Error()+".", "")
		return
	}
	repositories, err := h.hooks().States(r.Context(), removed, recorded)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "The auto-publish rule "+id+" was removed, but its hooks could not be read: "+err.Error()+". Reload the settings page.", "")
		return
	}
	writeContract(w, &schema.AutoPublishRemovalResponse{ID: id, Repositories: repositories})
}

// --------------------------------------------------------------------------
// POST /api/v1/settings/auto-publish/{id}/install
// --------------------------------------------------------------------------

// handleInstall installs one rule's hooks in one recorded repository the rule
// covers. A path that is not such a repository installs nothing.
func (h *autoPublishHandler) handleInstall(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	if !h.ready(w) {
		return
	}
	id := r.PathValue("id")
	var request schema.AutoPublishInstallRequest
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, ruleBodyLimit), &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, "No hook was installed for rule "+id+" because the body does not name a repository: "+err.Error()+". Send the path of one recorded repository the rule covers, then retry.", autoPublishInvalidCode)
		return
	}
	if err := request.Validate(); err != nil {
		writeAPIError(w, http.StatusBadRequest, "No hook was installed for rule "+id+": "+err.Error()+".", autoPublishInvalidCode)
		return
	}
	if cfg := h.config.load(); cfg != nil && !config.RedactionLevelSupported(cfg.Redaction.Level) {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("No hook was installed for rule %s because redaction.level is %q, which this version cannot apply, so every upload the hook runs would be refused. Set redaction.level to %s, then retry.", id, cfg.Redaction.Level, config.RecommendedRedactionLevel), autoPublishInvalidCode)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rules, err := autopublish.Load(h.rulesPath())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "No hook was installed: "+err.Error()+".", "")
		return
	}
	i := slices.IndexFunc(rules, func(rule autopublish.Rule) bool { return rule.ID == id })
	if i < 0 {
		writeAPIError(w, http.StatusNotFound, "No auto-publish rule has the identifier "+id+". No hook was installed.", autoPublishNotFoundCode)
		return
	}
	recorded, err := h.recorded(r)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "No hook was installed because the recorded repositories could not be listed: "+err.Error()+". Retry.", "")
		return
	}
	repository, _, err := h.hooks().Install(r.Context(), rules[i], request.Path, recorded)
	if errors.Is(err, autopublish.ErrNotCovered) {
		writeAPIError(w, http.StatusBadRequest, "No hook was installed: "+err.Error()+". Pick a repository from the rule's list.", autoPublishNotCoveredCode)
		return
	}
	if err != nil && repository.Path == "" {
		writeAPIError(w, http.StatusBadRequest, "No hook was installed: "+err.Error()+".", autoPublishInvalidCode)
		return
	}
	writeContract(w, &repository)
}

// ruleBodyLimit bounds a rule or install body: each is a few short fields.
const ruleBodyLimit = 1 << 20
