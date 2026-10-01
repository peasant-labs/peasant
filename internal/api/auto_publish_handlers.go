package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	store  *store.Store
	config *config.Config
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

// handleDeleteRule removes one rule. It changes no hook: a hook keeps
// publishing until the user removes it, and the answer reports the hooks of
// the repositories the rule covered as they are.
func (h *autoPublishHandler) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	if !h.ready(w) {
		return
	}
	id := r.PathValue("id")
	h.mu.Lock()
	defer h.mu.Unlock()
	var removed autopublish.Rule
	err := autopublish.Update(h.rulesPath(), func(rules []autopublish.Rule) ([]autopublish.Rule, error) {
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
	recorded, err := h.recorded(r)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "The auto-publish rule "+id+" was removed, but its repositories could not be listed: "+err.Error()+". Reload the settings page.", "")
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
	if h.config != nil && !config.RedactionLevelSupported(h.config.Redaction.Level) {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("No hook was installed for rule %s because redaction.level is %q, which this version cannot apply, so every upload the hook runs would be refused. Set redaction.level to %s, then retry.", id, h.config.Redaction.Level, config.RecommendedRedactionLevel), autoPublishInvalidCode)
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

// decodeStrict decodes a JSON body into a contract type, refusing an unknown
// field, a missing body, and anything after the one value.
func decodeStrict(body io.Reader, into any) error {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("the body holds more than one JSON value")
	}
	return nil
}

// ruleBodyLimit bounds a rule or install body: each is a few short fields.
const ruleBodyLimit = 1 << 20

// writeContract answers 200 with a contract value, after checking it the way
// a client does. A value that breaks the contract is not sent.
func writeContract(w http.ResponseWriter, value contractValue) {
	if err := value.Validate(); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "The answer breaks the Local API contract, so it is not sent: "+err.Error()+". Retry; if it repeats, report it.", "")
		return
	}
	_ = json.NewEncoder(w).Encode(value)
}

// contractValue is a contract type that checks its own invariants.
type contractValue interface{ Validate() error }
