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
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
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

// settingsDefaultEmail is the email the stub git resolver reports, which a
// write with no configuration file starts from.
const settingsDefaultEmail = "settings-default@example.test"

type settingsKeysFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	ReadOnly      []string `yaml:"readOnly"`
	PeasantConfig []string `yaml:"peasantConfig"`
}

// settingUpdateSetup is what stands at the configuration path before a request.
type settingUpdateSetup string

const (
	settingUpdateFile       settingUpdateSetup = ""
	settingUpdateMissing    settingUpdateSetup = "missing"
	settingUpdateDirectory  settingUpdateSetup = "directory"
	settingUpdateUnreadable settingUpdateSetup = "unreadable"
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
	ErrorContains string             `yaml:"errorContains"`
}

// TestSettingsServeEveryConfigKey reads testdata/settings-keys.yaml: every
// field of config.Config that has a yaml key has an entry, every entry is such
// a field, and GET returns exactly those keys with the read-only and `peasant
// config` flags the fixture pins and the registry implies.
func TestSettingsServeEveryConfigKey(t *testing.T) {
	t.Parallel()
	var fixture settingsKeysFixture
	if err := testutil.DecodeFixtureYAML(settingsKeysYAML, &fixture); err != nil {
		t.Fatalf("testdata/settings-keys.yaml: %v", err)
	}

	fields := map[string]bool{}
	for _, key := range configYAMLKeys(reflect.TypeOf(config.Config{}), "") {
		fields[key] = true
		if !slices.Contains(fixture.RequiredNames, key) {
			t.Errorf("config.Config field %q has a yaml key and no entry in testdata/settings-keys.yaml; add it to requiredNames", key)
		}
	}
	if err := testutil.RequireFixtureNames("testdata/settings-keys.yaml", "config.Config field", fixture.RequiredNames, fields); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(path, config.BaseConfig()); err != nil {
		t.Fatal(err)
	}
	response := getSettings(t, &settingsHandler{path: path})
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
			if (c.Status == http.StatusOK) == (c.ErrorContains != "") {
				t.Fatal("a 200 case names the saved row and a refusal names its reason, never both")
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			before := arrangeSettingUpdate(t, c, path)
			handler := &settingsHandler{path: path, git: &testutil.StubGitResolver{Email: settingsDefaultEmail}}

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
				if refusal.Key != c.Key || !strings.Contains(refusal.Error, c.ErrorContains) || !strings.Contains(refusal.Error, "Nothing was changed") {
					t.Errorf("refusal = %+v, want key %q and a reason that says %q and that nothing was changed", refusal, c.Key, c.ErrorContains)
				}
				if after := snapshotConfigPath(t, path); after != before {
					t.Errorf("a refused update changed the configuration path:\nbefore: %s\nafter:  %s", before, after)
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
			if _, err := config.Parse(written); err != nil {
				t.Errorf("the saved config.yaml does not load: %v", err)
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
	(&settingsHandler{path: path}).handleGetSettings(recorder, httptest.NewRequest(http.MethodGet, defaults.RouteSettings.String(), nil))
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

// configYAMLKeys walks a Config type and names each field that has a yaml
// key by its dotted path, descending into struct fields.
func configYAMLKeys(typ reflect.Type, prefix string) []string {
	var keys []string
	for i := range typ.NumField() {
		field := typ.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "-" || !field.IsExported() {
			continue
		}
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		if field.Type.Kind() == reflect.Struct {
			keys = append(keys, configYAMLKeys(field.Type, prefix+name+".")...)
			continue
		}
		keys = append(keys, prefix+name)
	}
	return keys
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

// arrangeSettingUpdate puts the case's setup at path and returns a snapshot of
// it.
func arrangeSettingUpdate(t *testing.T, c settingUpdateCase, path string) string {
	t.Helper()
	switch c.Setup {
	case settingUpdateMissing:
	case settingUpdateDirectory:
		if err := os.Mkdir(path, defaults.PrivateDirPerm); err != nil {
			t.Fatal(err)
		}
	case settingUpdateFile, settingUpdateUnreadable:
		if err := os.WriteFile(path, []byte(c.File), defaults.PublicFilePerm); err != nil {
			t.Fatal(err)
		}
		if c.Setup == settingUpdateUnreadable {
			if os.Geteuid() == 0 {
				t.Skip("root reads a file whatever its permissions, so this case cannot make it unreadable")
			}
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(path, defaults.PublicFilePerm) })
		}
	default:
		t.Fatalf("unknown setup %q", c.Setup)
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
