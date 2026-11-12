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
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/filelock"
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

// Error codes of the settings read.
const (
	settingsUnavailableCode           = "settings_unavailable"
	settingsUnreadableCode            = "settings_unreadable"
	settingsAutoPublishUnreadableCode = "settings_auto_publish_unreadable"
	settingsValueNotAppliedCode       = "settings_value_not_applied"
)

// liveConfig is the configuration the running server applies. It starts as the
// configuration the server loaded. After each setting saved through PATCH
// /api/v1/settings it is the saved file, except the read-only keys, which keep
// what the server started with because they change another way or the server
// binds them when it starts. A save replaces the whole snapshot, so a reader
// never sees half of a change.
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

// apply makes the saved file the snapshot, keeping the current value of every
// read-only key in catalog.
func (l *liveConfig) apply(catalog []settingSpec, saved *config.Config) {
	l.applyOwning(catalog, saved, nil)
}

// applyOwning is apply for a writer that owns some read-only keys: owned reports
// the keys the saved file may change in the snapshot although the settings
// routes may not. A nil owned owns none.
func (l *liveConfig) applyOwning(catalog []settingSpec, saved *config.Config, owned func(key string) bool) {
	if l == nil {
		return
	}
	current := l.current.Load()
	if current == nil {
		return
	}
	next := *saved
	for _, spec := range catalog {
		if spec.readOnly != "" && (owned == nil || !owned(spec.key)) {
			reflect.ValueOf(&next).Elem().FieldByIndex(spec.index).Set(reflect.ValueOf(current).Elem().FieldByIndex(spec.index))
		}
	}
	l.current.Store(&next)
}

// settingsHandler serves GET and PATCH /api/v1/settings over one
// configuration file. config.yaml is the only store: `peasant config` and
// every command read the same file.
type settingsHandler struct {
	// catalog is every key the routes serve, built when the server starts.
	catalog []settingSpec
	// path is the configuration file `peasant web start` loaded.
	path string
	// live receives the file after every good read and save, so the dashboard
	// applies what the settings page shows.
	live *liveConfig
	// git detects the default user email when no configuration file exists,
	// the way every command's config.Load does.
	git ingest.GitResolver
	// rules reads the saved auto-publish rules the GET answer carries.
	rules *autoPublishHandler
	// mu serializes reads and updates of the file and the live configuration,
	// so two updates cannot both start from the same file and lose one of the
	// changes, and a read never applies a file an update is replacing.
	mu sync.Mutex
	// lockWait bounds how long an update waits for the configuration file's
	// lock, which the setup commit and `peasant config` also take. Zero waits
	// defaultSettingsLockWait.
	lockWait time.Duration
	// afterRead, when set, runs between an update's read of the file and its
	// swap. A test replaces the file there, as a writer that ignores the lock
	// would. It is nil in production.
	afterRead func()
}

// defaultSettingsLockWait is how long an update waits for another writer of
// config.yaml before it is refused. A request should not hang for minutes.
const defaultSettingsLockWait = 5 * time.Second

func (h *settingsHandler) lockWaitOrDefault() time.Duration {
	if h.lockWait > 0 {
		return h.lockWait
	}
	return defaultSettingsLockWait
}

// handleGetSettings answers GET /api/v1/settings: every key of config.Config
// with its value in the file, the value that applies, and its metadata, and
// every saved auto-publish rule.
func (h *settingsHandler) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	if h.path == "" {
		writeAPIError(w, http.StatusServiceUnavailable, "The settings could not be read because this server was started without a configuration file in internal/api.handleGetSettings. Nothing was read. Start the dashboard with `peasant web start`, then retry.", settingsUnavailableCode)
		return
	}
	h.mu.Lock()
	_, document, cfg, err := h.readConfig(r.Context())
	if err == nil {
		// The dashboard applies what the page is about to show, including a
		// change made outside the page, such as with `peasant config`.
		h.live.apply(h.catalog, cfg)
		if applied := h.live.load(); applied != nil {
			// Start-only keys still use the running snapshot. Their Value
			// comes from document, while Effective describes this server.
			cfg = applied
		}
	}
	h.mu.Unlock()
	if err != nil {
		slog.Warn("settings: read the configuration file", "path", h.path, "error", err)
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("The settings could not be read from %s in internal/api.handleGetSettings: %v. Nothing was changed. Fix or restore the configuration file, then retry.", h.path, err), settingsUnreadableCode)
		return
	}
	rules, err := h.rules.listRules(r)
	if errors.Is(err, errAutoPublishStoreUnavailable) {
		writeAPIError(w, http.StatusServiceUnavailable, "The settings could not be listed because the auto-publish rules need the session store, which this server runs without, in internal/api.handleGetSettings. Nothing was changed. "+autoPublishStoreRemedy, autoPublishUnavailableCode)
		return
	}
	if err != nil {
		slog.Warn("settings: read the auto-publish rules", "error", err)
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("The settings could not be listed because the auto-publish rules could not be read in internal/api.handleGetSettings, and nothing was changed: %v.", err), settingsAutoPublishUnreadableCode)
		return
	}
	response := schema.LocalSettingsResponse{
		Settings:    make([]schema.LocalSetting, 0, len(h.catalog)),
		AutoPublish: rules,
	}
	for _, spec := range h.catalog {
		row, err := spec.row(document, cfg)
		var notApplied *settingNotAppliedError
		if errors.As(err, &notApplied) {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("The settings could not be listed: %v, then retry. Nothing was changed.", err), settingsValueNotAppliedCode)
			return
		}
		if err != nil {
			slog.Error("settings: render a setting", "key", spec.key, "error", err)
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("The setting %s could not be read in internal/api.handleGetSettings: %v. Nothing was changed. Fix the key in %s, then retry.", spec.key, err, h.path), settingsUnreadableCode)
			return
		}
		response.Settings = append(response.Settings, row)
	}
	writeContract(w, &response)
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
	spec, ok := lookupSetting(h.catalog, request.Key)
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
	// The file lock is the one every writer of config.yaml takes: this route,
	// the setup commit and `peasant config`. Holding it from the read to the swap
	// means an update starts from the file as the last writer left it and cannot
	// put back a setting that writer just saved.
	file, err := config.OpenConfigFile(h.path, time.Now().Add(h.lockWaitOrDefault()))
	if err != nil {
		slog.Warn("settings: take the configuration lock before an update", "path", h.path, "key", request.Key, "error", err)
		// The contract declares 503 for an update another writer holds up, and 500
		// for one that cannot write the file at all.
		status := http.StatusInternalServerError
		reason := fmt.Sprintf("%s could not be saved: %v; no file was changed; fix the directory's ownership or permissions, then retry.", request.Key, err)
		if filelock.IsDeadline(err) {
			status = http.StatusServiceUnavailable
			reason = fmt.Sprintf("%s could not be saved because another change holds the configuration file %s: %v. Nothing was changed. Wait for it to finish, then retry.", request.Key, h.path, err)
		}
		writeSettingRefusal(w, status, request.Key, reason)
		return
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			slog.Warn("settings: release the configuration lock after an update", "path", h.path, "key", request.Key, "error", closeErr)
		}
	}()
	data, document, _, err := file.Read(r.Context())
	if err != nil {
		slog.Warn("settings: read the configuration file before an update", "path", h.path, "key", request.Key, "error", err)
		writeSettingRefusal(w, http.StatusInternalServerError, request.Key, fmt.Sprintf("The configuration file %s could not be read: %v. Nothing was changed. Fix or restore the file, then retry.", h.path, err))
		return
	}
	for attempt := 0; ; attempt++ {
		update, refusal := h.prepareUpdate(r.Context(), spec, request.Key, replacement, data, document)
		if refusal != nil {
			writeSettingRefusal(w, refusal.status, request.Key, refusal.reason)
			return
		}
		if h.afterRead != nil {
			h.afterRead()
		}
		swapErr := file.Swap(data, update.bytes)
		if swapErr == nil {
			h.live.apply(h.catalog, update.saved)
			w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
			// The row passed the contract check before the save.
			_ = json.NewEncoder(w).Encode(update.row)
			return
		}
		var changed *config.ConfigChangedError
		if !errors.As(swapErr, &changed) {
			slog.Warn("settings: write the configuration file", "path", h.path, "key", request.Key, "error", swapErr)
			// The save error says what happened to the file, so the reason repeats
			// it rather than claim nothing changed.
			writeSettingRefusal(w, http.StatusInternalServerError, request.Key, fmt.Sprintf("%s could not be saved: %v.", request.Key, swapErr))
			return
		}
		if attempt > 0 {
			writeSettingRefusal(w, http.StatusInternalServerError, request.Key, fmt.Sprintf("%s could not be saved because %s changed again while the update was recomputed against it. Nothing was changed. Stop the program that keeps rewriting it, then retry.", request.Key, h.path))
			return
		}
		// A writer that does not take the lock replaced the file after it was
		// read. Compute the update once more against what is there now.
		data = changed.Fresh
		if document, _, err = config.ParseDocument(data); err != nil {
			slog.Warn("settings: read the configuration file after it changed", "path", h.path, "key", request.Key, "error", err)
			writeSettingRefusal(w, http.StatusInternalServerError, request.Key, fmt.Sprintf("The configuration file %s could not be read: %v. Nothing was changed. Fix or restore the file, then retry.", h.path, err))
			return
		}
	}
}

// settingUpdate is an update computed against one reading of the file: the
// bytes to write, the configuration they parse to, and the row the answer
// carries.
type settingUpdate struct {
	bytes []byte
	saved *config.Config
	row   schema.LocalSetting
}

// settingRefusal is why an update was not computed and what status says so.
type settingRefusal struct {
	status int
	reason string
}

// prepareUpdate applies replacement at the setting's key in the document read
// from data, and checks the result the way a save will. A nil data is a file
// that does not exist yet: the update starts from the defaults every command
// applies, so that writing one key does not drop the others, such as the email
// git supplies.
func (h *settingsHandler) prepareUpdate(ctx context.Context, spec settingSpec, key string, replacement *yaml.Node, data []byte, document *yaml.Node) (settingUpdate, *settingRefusal) {
	if data == nil {
		base, err := yaml.Marshal(config.LoadDefaults(ctx, h.git))
		if err == nil {
			document = &yaml.Node{}
			err = yaml.Unmarshal(base, document)
		}
		if err != nil {
			return settingUpdate{}, &settingRefusal{http.StatusInternalServerError, fmt.Sprintf("The default configuration could not be prepared for %s: %v. Nothing was changed.", h.path, err)}
		}
	}
	if err := setSettingNode(document, strings.Split(spec.key, "."), replacement); err != nil {
		return settingUpdate{}, &settingRefusal{http.StatusInternalServerError, fmt.Sprintf("%s could not be changed in %s: %v. Nothing was changed. Edit the file by hand.", key, h.path, err)}
	}
	updated, err := yaml.Marshal(document)
	if err != nil {
		return settingUpdate{}, &settingRefusal{http.StatusInternalServerError, fmt.Sprintf("The configuration could not be written for %s: %v. Nothing was changed.", key, err)}
	}
	saved, err := config.Parse(updated)
	if err != nil {
		return settingUpdate{}, &settingRefusal{http.StatusBadRequest, fmt.Sprintf("This value would make the configuration invalid: %v. Nothing was changed.", err)}
	}
	row, err := spec.row(document, saved)
	if err == nil {
		err = row.Validate()
	}
	if err != nil {
		slog.Error("settings: render an updated setting", "key", spec.key, "error", err)
		return settingUpdate{}, &settingRefusal{http.StatusInternalServerError, fmt.Sprintf("%s could not be read back after the change: %v. Nothing was changed. Report this defect.", key, err)}
	}
	return settingUpdate{bytes: updated, saved: saved, row: row}, nil
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
	var request schema.LocalSettingUpdateRequest
	if err := decodeStrict(bytes.NewReader(data), &request); err != nil {
		return schema.LocalSettingUpdateRequest{Key: named.Key}, err
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
