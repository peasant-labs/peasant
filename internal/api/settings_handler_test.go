package api

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/settings-keys.yaml
var settingsKeysYAML []byte

//go:embed testdata/settings-updates.yaml
var settingsUpdatesYAML []byte

//go:embed testdata/settings-reads.yaml
var settingsReadsYAML []byte

// settingsDefaultEmail is the email the stub git resolver reports, which a
// write with no configuration file starts from.
const settingsDefaultEmail = "settings-default@example.test"

type settingsKeysFixture struct {
	RequiredNames []string            `yaml:"requiredNames"`
	Editable      []string            `yaml:"editable"`
	ReadOnly      []string            `yaml:"readOnly"`
	Choices       map[string][]string `yaml:"choices"`
	PeasantConfig []string            `yaml:"peasantConfig"`
}

// settingUpdateSetup is what stands at the configuration path before a request.
type settingUpdateSetup string

const (
	settingUpdateFile       settingUpdateSetup = ""
	settingUpdateMissing    settingUpdateSetup = "missing"
	settingUpdateDirectory  settingUpdateSetup = "directory"
	settingUpdateUnreadable settingUpdateSetup = "unreadable"
	// settingUpdateReadOnlyDirectory is the file in a directory the server
	// may not write to.
	settingUpdateReadOnlyDirectory settingUpdateSetup = "readonly-directory"
)

type settingUpdateCase struct {
	Name          string             `yaml:"name"`
	Setup         settingUpdateSetup `yaml:"setup"`
	File          string             `yaml:"file"`
	Body          string             `yaml:"body"`
	Status        int                `yaml:"status"`
	Key           string             `yaml:"key"`
	Value         string             `yaml:"value"`
	Effective     string             `yaml:"effective"`
	FileContains  []string           `yaml:"fileContains"`
	FileOmits     []string           `yaml:"fileOmits"`
	ErrorContains []string           `yaml:"errorContains"`
}

// TestSettingsServeEveryConfigKey reads testdata/settings-keys.yaml. Every
// path config.yaml can hold belongs to exactly one entry, and every entry owns
// one, so a new Config field fails until it has an entry. Every entry is
// classified editable or read-only. GET returns exactly those keys with the
// read-only and `peasant config` flags the fixture pins and the registry
// implies.
func TestSettingsServeEveryConfigKey(t *testing.T) {
	t.Parallel()
	var fixture settingsKeysFixture
	if err := testutil.DecodeFixtureYAML(settingsKeysYAML, &fixture); err != nil {
		t.Fatalf("testdata/settings-keys.yaml: %v", err)
	}

	owned := map[string]bool{}
	for path := range flattenConfigYAML(t, filledConfig()) {
		var owners []string
		for _, key := range fixture.RequiredNames {
			if path == key || strings.HasPrefix(path, key+".") {
				owners = append(owners, key)
			}
		}
		if len(owners) != 1 {
			t.Errorf("config.yaml path %q belongs to %v; every config.Config field with a yaml key needs exactly one entry in testdata/settings-keys.yaml requiredNames", path, owners)
			continue
		}
		owned[owners[0]] = true
	}
	if err := testutil.RequireFixtureNames("testdata/settings-keys.yaml", "config.Config field", fixture.RequiredNames, owned); err != nil {
		t.Fatal(err)
	}
	for _, key := range fixture.RequiredNames {
		if slices.Contains(fixture.Editable, key) == slices.Contains(fixture.ReadOnly, key) {
			t.Errorf("%s must be listed in exactly one of editable and readOnly", key)
		}
	}
	if len(fixture.Editable)+len(fixture.ReadOnly) != len(fixture.RequiredNames) {
		t.Errorf("editable and readOnly list %d keys, want exactly the %d required names", len(fixture.Editable)+len(fixture.ReadOnly), len(fixture.RequiredNames))
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(path, config.BaseConfig()); err != nil {
		t.Fatal(err)
	}
	response := getSettings(t, newTestSettingsHandler(t, path))
	served := map[string]bool{}
	readOnly := map[string]bool{}
	inPeasantConfig := map[string]bool{}
	for _, setting := range response.Settings {
		served[setting.Key] = true
		readOnly[setting.Key] = !setting.Editable
		inPeasantConfig[setting.Key] = setting.InPeasantConfig
	}
	if err := testutil.RequireFixtureNames("GET "+defaults.RouteSettings.String(), "setting", fixture.RequiredNames, served); err != nil {
		t.Fatal(err)
	}
	if len(served) != len(fixture.RequiredNames) {
		t.Errorf("GET returned %d keys, want exactly the %d required names", len(served), len(fixture.RequiredNames))
	}
	assertExactSettingKeys(t, "read-only keys", readOnly, fixture.ReadOnly)
	editable := map[string]bool{}
	for key, isReadOnly := range readOnly {
		editable[key] = !isReadOnly
	}
	assertExactSettingKeys(t, "editable keys", editable, fixture.Editable)
	for _, setting := range response.Settings {
		menu, isChoice := fixture.Choices[setting.Key]
		if isChoice != (setting.Kind == schema.LocalSettingChoice) || !slices.Equal(setting.Options, menu) {
			t.Errorf("%s is kind %s with options %q, want choice %v with options %q", setting.Key, setting.Kind, setting.Options, isChoice, menu)
		}
	}
	for key := range fixture.Choices {
		if !served[key] {
			t.Errorf("choices names %s, which GET does not list", key)
		}
	}
	assertExactSettingKeys(t, "keys `peasant config` edits", inPeasantConfig, fixture.PeasantConfig)
	assertExactSettingKeys(t, "keys the `peasant config` registry writes", registryWrittenKeys(t, fixture.RequiredNames), fixture.PeasantConfig)
}

// TestSettingsUpdateFixtures runs testdata/settings-updates.yaml through PATCH
// /api/v1/settings: a saved change is in config.yaml and reads back through
// GET; a refusal names the key and the reason and leaves the configuration
// path exactly as it was.
func TestSettingsUpdateFixtures(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string            `yaml:"requiredNames"`
		Cases         []settingUpdateCase `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(settingsUpdatesYAML, &fixture); err != nil {
		t.Fatalf("testdata/settings-updates.yaml: %v", err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if c.Status == 0 || c.Key == "" || c.Body == "" {
				t.Fatal("a case needs a body, a status, and the key the response names")
			}
			if (c.Status == http.StatusOK) == (len(c.ErrorContains) > 0) {
				t.Fatal("a 200 case names the saved row and a refusal names its reason, never both")
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			before := arrangeConfigPath(t, c.Setup, c.File, path)
			handler := newTestSettingsHandler(t, path)
			running := config.BaseConfig()
			handler.live = newLiveConfig(running)

			recorder := httptest.NewRecorder()
			handler.handleUpdateSetting(recorder, httptest.NewRequest(http.MethodPatch, defaults.RouteSettings.String(), strings.NewReader(c.Body)))
			if recorder.Code != c.Status {
				t.Fatalf("status = %d, want %d; body: %s", recorder.Code, c.Status, recorder.Body)
			}
			assertNoTemporaryConfig(t, dir)
			if c.Status != http.StatusOK {
				var refusal schema.LocalSettingRefusal
				if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil || refusal.Validate() != nil {
					t.Fatalf("refusal is not a LocalSettingRefusal: %v; body: %s", err, recorder.Body)
				}
				if refusal.Key != c.Key {
					t.Errorf("refusal names %q, want %q", refusal.Key, c.Key)
				}
				for _, text := range c.ErrorContains {
					if !strings.Contains(refusal.Error, text) {
						t.Errorf("refusal %q does not say %q", refusal.Error, text)
					}
				}
				if after := snapshotConfigPath(t, path); after != before {
					t.Errorf("a refused update changed the configuration path:\nbefore: %s\nafter:  %s", before, after)
				}
				if handler.live.load() != running {
					t.Error("a refused update changed the configuration the running server applies")
				}
				return
			}

			var saved schema.LocalSetting
			if err := json.Unmarshal(recorder.Body.Bytes(), &saved); err != nil || saved.Validate() != nil {
				t.Fatalf("response is not a valid LocalSetting: %v; body: %s", err, recorder.Body)
			}
			if saved.Key != c.Key || !sameJSON(t, saved.Value, c.Value) || !sameJSON(t, saved.Effective, c.Effective) {
				t.Errorf("saved row = key %q value %s effective %s, want key %q value %s effective %s", saved.Key, saved.Value, saved.Effective, c.Key, c.Value, c.Effective)
			}
			written, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range c.FileContains {
				if !bytes.Contains(written, []byte(text)) {
					t.Errorf("config.yaml does not hold %q:\n%s", text, written)
				}
			}
			for _, text := range c.FileOmits {
				if bytes.Contains(written, []byte(text)) {
					t.Errorf("config.yaml still holds %q:\n%s", text, written)
				}
			}
			parsed, err := config.Parse(written)
			if err != nil {
				t.Fatalf("the saved config.yaml does not load: %v", err)
			}
			spec, _ := lookupSetting(handler.catalog, c.Key)
			if applied, want := reflect.ValueOf(handler.live.load()).Elem().FieldByIndex(spec.index).Interface(), reflect.ValueOf(parsed).Elem().FieldByIndex(spec.index).Interface(); !reflect.DeepEqual(applied, want) {
				t.Errorf("the running server applies %s = %v, want the saved %v", c.Key, applied, want)
			}
			listed := getSettings(t, handler).Settings
			index := slices.IndexFunc(listed, func(s schema.LocalSetting) bool { return s.Key == c.Key })
			if index < 0 {
				t.Fatalf("GET after the update does not list %s", c.Key)
			}
			if !reflect.DeepEqual(normalizeSetting(t, listed[index]), normalizeSetting(t, saved)) {
				t.Errorf("GET after the update reads %+v, want the saved row %+v", listed[index], saved)
			}
		})
	}
}

// settingReadCase is one GET /api/v1/settings case of
// testdata/settings-reads.yaml.
type settingReadCase struct {
	Name          string             `yaml:"name"`
	Setup         settingUpdateSetup `yaml:"setup"`
	File          string             `yaml:"file"`
	Status        int                `yaml:"status"`
	Key           string             `yaml:"key"`
	Value         string             `yaml:"value"`
	Effective     string             `yaml:"effective"`
	Code          string             `yaml:"code"`
	ErrorContains string             `yaml:"errorContains"`
}

// TestSettingsReadFixtures runs testdata/settings-reads.yaml through GET
// /api/v1/settings: the value the file names beside the value that applies, and
// the refusal when the file names a value no run applies.
func TestSettingsReadFixtures(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string          `yaml:"requiredNames"`
		Cases         []settingReadCase `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(settingsReadsYAML, &fixture); err != nil {
		t.Fatalf("testdata/settings-reads.yaml: %v", err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if (c.Status == http.StatusOK) != (c.Key != "" && c.Code == "") || (c.Status != http.StatusOK) != (c.ErrorContains != "") {
				t.Fatal("a 200 case names a key and its values; a refusal names its code and reason")
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			arrangeConfigPath(t, c.Setup, c.File, path)
			handler := newTestSettingsHandler(t, path)
			if c.Status != http.StatusOK {
				recorder := httptest.NewRecorder()
				handler.handleGetSettings(recorder, httptest.NewRequest(http.MethodGet, defaults.RouteSettings.String(), nil))
				decodeRefusal(t, recorder.Code, recorder.Body.Bytes(), c.Status, c.Code)
				if !strings.Contains(recorder.Body.String(), c.ErrorContains) {
					t.Errorf("refusal %s does not say %q", recorder.Body, c.ErrorContains)
				}
				return
			}
			listed := getSettings(t, handler).Settings
			index := slices.IndexFunc(listed, func(s schema.LocalSetting) bool { return s.Key == c.Key })
			if index < 0 {
				t.Fatalf("GET does not list %s", c.Key)
			}
			effective := strings.ReplaceAll(c.Effective, "{cpu-default}", strconv.Itoa(push.DefaultConcurrencyForCPU(runtime.NumCPU())))
			if got := listed[index]; !sameJSON(t, got.Value, c.Value) || !sameJSON(t, got.Effective, effective) {
				t.Errorf("%s reads value %s effective %s, want value %s effective %s", c.Key, got.Value, got.Effective, c.Value, c.Effective)
			}
		})
	}
}

// TestSettingsReturnNoCredential signs this computer in to Village and reads
// the settings: the stored API key appears nowhere in the response.
func TestSettingsReturnNoCredential(t *testing.T) {
	t.Parallel()
	hs := newTestXDGHomes(t)
	writeSyncDoorCredentials(t, hs.Config, "https://village.example.test")
	path := defaults.ResolveConfigFilePathWith(hs.Config).String()
	cfg := config.BaseConfig()
	cfg.Village.Connected = true
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler := newTestSettingsHandler(t, path)
	// The rules read from the directory that holds the credential, as they
	// do in production.
	handler.rules = &autoPublishHandler{configHome: hs.Config}
	handler.handleGetSettings(recorder, httptest.NewRequest(http.MethodGet, defaults.RouteSettings.String(), nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", recorder.Code, recorder.Body)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"key":"village.connected"`)) {
		t.Fatalf("the response does not list village.connected, so it says nothing about the credential beside it: %s", recorder.Body)
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte("test-key")) {
		t.Errorf("the settings response carries the stored Village API key: %s", recorder.Body)
	}
}

// newTestSettingsHandler serves path with the production catalog, a stub git
// email, and the rule store of a config directory that holds no rules file.
func newTestSettingsHandler(t *testing.T, path string) *settingsHandler {
	t.Helper()
	catalog, err := buildSettingCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return &settingsHandler{
		catalog: catalog,
		path:    path,
		git:     &testutil.StubGitResolver{Email: settingsDefaultEmail},
		rules:   &autoPublishHandler{configHome: t.TempDir()},
	}
}

// getSettings reads GET /api/v1/settings through handler and checks the
// response against the contract.
func getSettings(t *testing.T, handler *settingsHandler) schema.LocalSettingsResponse {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.handleGetSettings(recorder, httptest.NewRequest(http.MethodGet, defaults.RouteSettings.String(), nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d; body: %s", recorder.Code, recorder.Body)
	}
	var response schema.LocalSettingsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("GET response: %v; body: %s", err, recorder.Body)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("GET response breaks the contract: %v", err)
	}
	return response
}

// registryWrittenKeys asks the `peasant config` registry which keys it writes:
// each field copies an empty configuration's value onto a filled one, and the
// YAML paths that change belong to the key that names them or their parent.
func registryWrittenKeys(t *testing.T, keys []string) map[string]bool {
	t.Helper()
	owner := func(path string) string {
		for _, key := range keys {
			if path == key || strings.HasPrefix(path, key+".") {
				return key
			}
		}
		t.Fatalf("YAML path %q belongs to no settings key", path)
		return ""
	}
	untouched := flattenConfigYAML(t, filledConfig())
	written := map[string]bool{}
	kickstart.BuildRegistry(kickstart.Options{}).FieldWrites(func() config.Config { return config.Config{} }, filledConfig, func(_ settings.Field, cfg *config.Config) {
		after := flattenConfigYAML(t, *cfg)
		for path, value := range untouched {
			if after[path] != value {
				written[owner(path)] = true
			}
		}
		for path := range after {
			if _, ok := untouched[path]; !ok {
				written[owner(path)] = true
			}
		}
	})
	return written
}

// flattenConfigYAML renders cfg as config.yaml would and maps each dotted
// mapping path to its value.
func flattenConfigYAML(t *testing.T, cfg config.Config) map[string]string {
	t.Helper()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	flat := map[string]string{}
	var walk func(prefix string, value any)
	walk = func(prefix string, value any) {
		if mapping, ok := value.(map[string]any); ok && len(mapping) > 0 {
			for key, child := range mapping {
				walk(prefix+key+".", child)
			}
			return
		}
		flat[strings.TrimSuffix(prefix, ".")] = fmt.Sprint(value)
	}
	walk("", document)
	return flat
}

func assertExactSettingKeys(t *testing.T, what string, got map[string]bool, want []string) {
	t.Helper()
	var have []string
	for key, set := range got {
		if set {
			have = append(have, key)
		}
	}
	slices.Sort(have)
	expected := slices.Sorted(slices.Values(want))
	if !slices.Equal(have, expected) {
		t.Errorf("%s = %v, want exactly %v", what, have, expected)
	}
}

// arrangeConfigPath puts setup (with file as the file's text) at path and
// returns a snapshot of it.
func arrangeConfigPath(t *testing.T, setup settingUpdateSetup, file, path string) string {
	t.Helper()
	switch setup {
	case settingUpdateMissing:
	case settingUpdateDirectory:
		if err := os.Mkdir(path, defaults.PrivateDirPerm); err != nil {
			t.Fatal(err)
		}
	case settingUpdateFile, settingUpdateUnreadable, settingUpdateReadOnlyDirectory:
		if err := os.WriteFile(path, []byte(file), defaults.PublicFilePerm); err != nil {
			t.Fatal(err)
		}
		if setup == settingUpdateUnreadable {
			if os.Geteuid() == 0 {
				t.Skip("root reads a file whatever its permissions, so this case cannot make it unreadable")
			}
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(path, defaults.PublicFilePerm) })
		}
		if setup == settingUpdateReadOnlyDirectory {
			if os.Geteuid() == 0 {
				t.Skip("root writes to a directory whatever its permissions, so this case cannot refuse the write")
			}
			dir := filepath.Dir(path)
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, defaults.PrivateDirPerm) })
		}
	default:
		t.Fatalf("unknown setup %q", setup)
	}
	return snapshotConfigPath(t, path)
}

// snapshotConfigPath describes what stands at path: nothing, a directory, or a
// file's mode and bytes.
func snapshotConfigPath(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "missing"
	}
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		return "directory"
	}
	if info.Mode().Perm()&0o400 == 0 {
		return fmt.Sprintf("unreadable file of %d bytes", info.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("file %v: %q", info.Mode().Perm(), data)
}

// assertNoTemporaryConfig fails when an atomic write left its temporary file.
func assertNoTemporaryConfig(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".config-*.yaml.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("an update left temporary files: %v", matches)
	}
}

// sameJSON reports whether a setting value holds the JSON text want.
func sameJSON(t *testing.T, got schema.LocalSettingValue, want string) bool {
	t.Helper()
	var left, right any
	if err := json.Unmarshal(got, &left); err != nil {
		t.Fatalf("value %s is not JSON: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &right); err != nil {
		t.Fatalf("fixture value %s is not JSON: %v", want, err)
	}
	return reflect.DeepEqual(left, right)
}

// normalizeSetting decodes a setting's JSON values so two rows compare by
// meaning rather than by spacing.
func normalizeSetting(t *testing.T, setting schema.LocalSetting) map[string]any {
	t.Helper()
	data, err := json.Marshal(setting)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSettingsEveryOfferedChoiceSaves saves every offered value of every
// editable choice key in testdata/settings-keys.yaml and reads it back, so a
// menu cannot offer a value the configuration refuses.
func TestSettingsEveryOfferedChoiceSaves(t *testing.T) {
	t.Parallel()
	var fixture settingsKeysFixture
	if err := testutil.DecodeFixtureYAML(settingsKeysYAML, &fixture); err != nil {
		t.Fatalf("testdata/settings-keys.yaml: %v", err)
	}
	saved := 0
	for key, menu := range fixture.Choices {
		if !slices.Contains(fixture.Editable, key) {
			continue
		}
		for _, option := range menu {
			path := filepath.Join(t.TempDir(), "config.yaml")
			// push.sources lets by-source stand; every other key is unset.
			if err := os.WriteFile(path, []byte("version: 1\npush:\n  sources: [claude-code]\n"), defaults.PublicFilePerm); err != nil {
				t.Fatal(err)
			}
			handler := newTestSettingsHandler(t, path)
			body, _ := json.Marshal(map[string]any{"key": key, "value": option})
			recorder := httptest.NewRecorder()
			handler.handleUpdateSetting(recorder, httptest.NewRequest(http.MethodPatch, defaults.RouteSettings.String(), bytes.NewReader(body)))
			var row schema.LocalSetting
			if err := json.Unmarshal(recorder.Body.Bytes(), &row); recorder.Code != http.StatusOK || err != nil || !sameJSON(t, row.Value, strconv.Quote(option)) {
				t.Errorf("%s = %q: status %d, body %s", key, option, recorder.Code, recorder.Body)
				continue
			}
			listed := getSettings(t, handler).Settings
			if index := slices.IndexFunc(listed, func(s schema.LocalSetting) bool { return s.Key == key }); index < 0 || !sameJSON(t, listed[index].Value, strconv.Quote(option)) {
				t.Errorf("%s = %q does not read back", key, option)
			}
			saved++
		}
	}
	if saved == 0 {
		t.Fatal("no offered choice was saved, so the test proved nothing")
	}
}

// TestLiveConfigKeepsReadOnlyKeysOnSave applies a saved configuration that
// differs from the running one in every key: the running server takes every
// editable key from the saved file and keeps every read-only key, which
// changes another way or which it binds when it starts.
func TestLiveConfigKeepsReadOnlyKeysOnSave(t *testing.T) {
	t.Parallel()
	catalog, err := buildSettingCatalog()
	if err != nil {
		t.Fatal(err)
	}
	running := config.BaseConfig()
	running.Version = 7
	running.Selection.AutoIngestNewBranches = false
	saved := filledConfig()
	live := newLiveConfig(running)
	live.apply(catalog, &saved)
	applied := live.load()
	field := func(cfg *config.Config, spec settingSpec) any {
		return reflect.ValueOf(cfg).Elem().FieldByIndex(spec.index).Interface()
	}
	for _, spec := range catalog {
		want := field(&saved, spec)
		if spec.readOnly != "" {
			if reflect.DeepEqual(field(running, spec), want) {
				t.Fatalf("read-only %s is the same in both configurations, so the test cannot tell which one was kept", spec.key)
			}
			want = field(running, spec)
		}
		if got := field(applied, spec); !reflect.DeepEqual(got, want) {
			t.Errorf("%s (read-only %v) applies %v, want %v", spec.key, spec.readOnly != "", got, want)
		}
	}
	if applied == running || field(running, catalog[0]) != 7 {
		t.Error("apply changed the running snapshot in place instead of replacing it")
	}
}
