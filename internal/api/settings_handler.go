package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

// maxSettingUpdateBytes bounds a PATCH /api/v1/settings body. A setting value
// is at most a list of custom redaction patterns.
const maxSettingUpdateBytes = 1 << 20

// refusalKeyUnnamed is the key a refusal names when the request body names no
// readable key.
const refusalKeyUnnamed = "(none)"

// liveConfig is the configuration the running server applies: the one it
// started with, plus every setting saved through PATCH /api/v1/settings since.
// A saved setting replaces the whole snapshot, so a request that read one
// snapshot never sees half of a change.
type liveConfig struct {
	current atomic.Pointer[config.Config]
}

func newLiveConfig(cfg *config.Config) *liveConfig {
	live := &liveConfig{}
	live.current.Store(cfg)
	return live
}

// load returns the current snapshot. Callers read it and never change it.
func (l *liveConfig) load() *config.Config {
	if l == nil {
		return nil
	}
	return l.current.Load()
}

// apply copies spec's field from saved into a new snapshot. The server keeps
// what it started with for every other key, so a setting changed in
// config.yaml by hand still waits for a restart, as it always has.
func (l *liveConfig) apply(spec settingSpec, saved *config.Config) {
	if l == nil {
		return
	}
	current := l.current.Load()
	if current == nil {
		return
	}
	next := *current
	reflect.ValueOf(&next).Elem().FieldByIndex(spec.index).Set(reflect.ValueOf(saved).Elem().FieldByIndex(spec.index))
	l.current.Store(&next)
}

// settingsHandler serves GET and PATCH /api/v1/settings over one
// configuration file. config.yaml is the only store: `peasant config` and
// every command read the same file.
type settingsHandler struct {
	// path is the configuration file `peasant web start` loaded.
	path string
	// live receives each saved setting so the dashboard applies it at once.
	live *liveConfig
	// git detects the default user email when no configuration file exists,
	// the way every command's config.Load does.
	git ingest.GitResolver
	// rules lists the saved auto-publish rules the GET answer carries.
	rules autoPublishRules
	// mu serializes updates, so two updates cannot both start from the same
	// file and lose one of the changes.
	mu sync.Mutex
}

// handleGetSettings answers GET /api/v1/settings: every key of config.Config
// with its value in the file, the value that applies, and its metadata.
func (h *settingsHandler) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	if h.path == "" {
		writeAPIError(w, http.StatusServiceUnavailable, "The settings could not be read because this server was started without a configuration file in internal/api.handleGetSettings. Nothing was read. Start the dashboard with `peasant web start`, then retry.", "settings_unavailable")
		return
	}
	catalog, err := settingCatalog()
	if err != nil {
		slog.Error("settings: build the key catalog", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "The settings could not be listed because the settings catalog is invalid in internal/api.handleGetSettings: "+err.Error()+". Nothing was read. Report this defect.", "settings_catalog_invalid")
		return
	}
	_, document, cfg, err := h.readConfig(r.Context())
	if err != nil {
		slog.Warn("settings: read the configuration file", "path", h.path, "error", err)
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("The settings could not be read from %s in internal/api.handleGetSettings: %v. Nothing was changed. Fix or restore the configuration file, then retry.", h.path, err), "settings_unreadable")
		return
	}
	if h.rules == nil {
		writeAPIError(w, http.StatusInternalServerError, "The settings could not be listed because this server was built without the auto-publish rules in internal/api.handleGetSettings. Nothing was read. Report this defect.", "settings_unavailable")
		return
	}
	rules, err := h.rules.listRules(r.Context())
	if err != nil {
		slog.Warn("settings: read the auto-publish rules", "error", err)
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("The settings could not be listed because the auto-publish rules could not be read in internal/api.handleGetSettings: %v. Nothing was changed. Fix or remove the rules file, then retry.", err), "settings_auto_publish_unreadable")
		return
	}
	response := schema.LocalSettingsResponse{
		Settings:    make([]schema.LocalSetting, 0, len(catalog)),
		AutoPublish: rules,
	}
	for _, spec := range catalog {
		row, err := spec.row(document, cfg)
		if err != nil {
			slog.Error("settings: render a setting", "key", spec.key, "error", err)
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("The setting %s could not be read in internal/api.handleGetSettings: %v. Nothing was changed. Fix the key in %s, then retry.", spec.key, err, h.path), "settings_unreadable")
			return
		}
		response.Settings = append(response.Settings, row)
	}
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	_ = json.NewEncoder(w).Encode(response)
}

// handleUpdateSetting answers PATCH /api/v1/settings: it changes one editable
// key in the configuration file, or changes nothing and says why.
func (h *settingsHandler) handleUpdateSetting(w http.ResponseWriter, r *http.Request) {
	request, err := decodeSettingUpdate(http.MaxBytesReader(w, r.Body, maxSettingUpdateBytes))
	if err != nil {
		writeSettingRefusal(w, http.StatusBadRequest, request.Key, "The request is not a setting update: "+err.Error()+". Nothing was changed. Send one JSON object with the setting's dotted key and its new value, or null to use the default.")
		return
	}
	if h.path == "" {
		writeSettingRefusal(w, http.StatusInternalServerError, request.Key, "This server was started without a configuration file, so there is nothing to change. Nothing was changed. Start the dashboard with `peasant web start`, then retry.")
		return
	}
	catalog, err := settingCatalog()
	if err != nil {
		slog.Error("settings: build the key catalog", "error", err)
		writeSettingRefusal(w, http.StatusInternalServerError, request.Key, "The settings catalog is invalid: "+err.Error()+". Nothing was changed. Report this defect.")
		return
	}
	spec, ok := lookupSetting(catalog, request.Key)
	if !ok {
		writeSettingRefusal(w, http.StatusBadRequest, request.Key, fmt.Sprintf("%s is not a setting. Nothing was changed. Use a key that GET %s lists.", request.Key, defaults.RouteSettings))
		return
	}
	if spec.readOnly != "" {
		writeSettingRefusal(w, http.StatusBadRequest, request.Key, fmt.Sprintf("%s cannot be changed here (%s). Nothing was changed.", request.Key, spec.readOnly))
		return
	}
	if err := request.Value.ValidateFor(spec.kind, spec.options); err != nil {
		reason := fmt.Sprintf("%s refused the value: %v. Nothing was changed. Send a value of kind %s, or null to use the default.", request.Key, err, spec.kind)
		if spec.kind == schema.LocalSettingChoice {
			reason = fmt.Sprintf("%s refused the value: %v. Nothing was changed. Send one of %s, or null to use the default.", request.Key, err, quotedOptions(spec.options))
		}
		writeSettingRefusal(w, http.StatusBadRequest, request.Key, reason)
		return
	}
	var replacement *yaml.Node
	if !request.Value.IsUnset() {
		value, err := spec.decodeSettingValue(request.Value)
		if err != nil {
			writeSettingRefusal(w, http.StatusBadRequest, request.Key, fmt.Sprintf("%s cannot hold this value: %v. Nothing was changed. Send a value of the shape GET %s shows for it.", request.Key, err, defaults.RouteSettings))
			return
		}
		replacement = &yaml.Node{}
		if err := replacement.Encode(value); err != nil {
			writeSettingRefusal(w, http.StatusBadRequest, request.Key, fmt.Sprintf("%s cannot hold this value: %v. Nothing was changed.", request.Key, err))
			return
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	data, document, _, err := h.readConfig(r.Context())
	if err != nil {
		slog.Warn("settings: read the configuration file before an update", "path", h.path, "key", request.Key, "error", err)
		writeSettingRefusal(w, http.StatusInternalServerError, request.Key, fmt.Sprintf("The configuration file %s could not be read: %v. Nothing was changed. Fix or restore the file, then retry.", h.path, err))
		return
	}
	if data == nil {
		// No file yet: start from the defaults every command applies, so that
		// writing one key does not drop the others, such as the email git
		// supplies.
		if data, err = yaml.Marshal(config.LoadDefaults(r.Context(), h.git)); err == nil {
			err = yaml.Unmarshal(data, document)
		}
		if err != nil {
			writeSettingRefusal(w, http.StatusInternalServerError, request.Key, fmt.Sprintf("The default configuration could not be prepared for %s: %v. Nothing was changed.", h.path, err))
			return
		}
	}
	if err := setSettingNode(document, strings.Split(spec.key, "."), replacement); err != nil {
		writeSettingRefusal(w, http.StatusInternalServerError, request.Key, fmt.Sprintf("%s could not be changed in %s: %v. Nothing was changed. Edit the file by hand.", request.Key, h.path, err))
		return
	}
	updated, err := yaml.Marshal(document)
	if err != nil {
		writeSettingRefusal(w, http.StatusInternalServerError, request.Key, fmt.Sprintf("The configuration could not be written for %s: %v. Nothing was changed.", request.Key, err))
		return
	}
	saved, err := config.Parse(updated)
	if err != nil {
		writeSettingRefusal(w, http.StatusBadRequest, request.Key, fmt.Sprintf("This value would make the configuration invalid: %v. Nothing was changed.", err))
		return
	}
	row, err := spec.row(document, saved)
	if err != nil {
		slog.Error("settings: render an updated setting", "key", spec.key, "error", err)
		writeSettingRefusal(w, http.StatusInternalServerError, request.Key, fmt.Sprintf("%s could not be read back after the change: %v. Nothing was changed. Report this defect.", request.Key, err))
		return
	}
	if err := config.SaveAtomicYAML(h.path, updated); err != nil {
		slog.Warn("settings: write the configuration file", "path", h.path, "key", request.Key, "error", err)
		writeSettingRefusal(w, http.StatusInternalServerError, request.Key, err.Error())
		return
	}
	h.live.apply(spec, saved)
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	_ = json.NewEncoder(w).Encode(row)
}

// autoPublishRules lists every saved auto-publish rule with the recorded
// repositories it covers and each one's hooks.
type autoPublishRules interface {
	listRules(ctx context.Context) ([]schema.AutoPublishRule, error)
}

var _ autoPublishRules = (*autoPublishHandler)(nil)

// listRules reads the rules the auto-publish routes save, each as the save
// route answers it: the recorded repositories it covers, with their hooks as
// they are. A rule that cannot be read fails the list rather than drop out of
// it, because a rule decides who can read a transcript.
func (h *autoPublishHandler) listRules(ctx context.Context) ([]schema.AutoPublishRule, error) {
	rules, err := autopublish.Load(h.rulesPath())
	if err != nil {
		return nil, err
	}
	views := make([]schema.AutoPublishRule, 0, len(rules))
	if len(rules) == 0 {
		return views, nil
	}
	if h.store == nil {
		return nil, errors.New("this server runs without its session store, which names the repositories Peasant recorded")
	}
	recorded, err := autopublish.Recorded(ctx, h.store, &ingest.ExecGitResolver{})
	if err != nil {
		return nil, fmt.Errorf("list the recorded repositories: %w", err)
	}
	hooks := h.hooks()
	for _, rule := range rules {
		view, err := hooks.View(ctx, rule, recorded)
		if err != nil {
			return nil, fmt.Errorf("read the hooks of auto-publish rule %s: %w", rule.ID, err)
		}
		views = append(views, view)
	}
	return views, nil
}

// readConfig reads the configuration file: its bytes, its YAML document, and
// the configuration it applies. A missing file reads as no bytes, an empty
// document, and the defaults every command applies without one.
func (h *settingsHandler) readConfig(ctx context.Context) ([]byte, *yaml.Node, *config.Config, error) {
	data, err := os.ReadFile(h.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &yaml.Node{}, config.LoadDefaults(ctx, h.git), nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, nil, nil, err
	}
	document := &yaml.Node{}
	if err := yaml.Unmarshal(data, document); err != nil {
		return nil, nil, nil, err
	}
	return data, document, cfg, nil
}

// decodeSettingUpdate reads exactly one update request and refuses a field the
// contract does not declare. The returned request carries whatever key it
// could read, so a refusal can name it.
func decodeSettingUpdate(body io.Reader) (schema.LocalSettingUpdateRequest, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return schema.LocalSettingUpdateRequest{}, err
	}
	var named struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(data, &named)
	request := schema.LocalSettingUpdateRequest{Key: named.Key}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return schema.LocalSettingUpdateRequest{Key: named.Key}, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return request, errors.New("the body holds more than one JSON value")
	}
	return request, request.Validate()
}

// writeSettingRefusal answers a refused update with the key and the reason.
func writeSettingRefusal(w http.ResponseWriter, status int, key, reason string) {
	if strings.TrimSpace(key) == "" {
		key = refusalKeyUnnamed
	}
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(schema.LocalSettingRefusal{Key: key, Error: reason})
}

// quotedOptions renders a choice menu for a refusal.
func quotedOptions(options []string) string {
	quoted := make([]string, len(options))
	for i, option := range options {
		quoted[i] = fmt.Sprintf("%q", option)
	}
	return strings.Join(quoted, ", ")
}
