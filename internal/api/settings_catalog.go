package api

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

// settingSpec is one key the settings routes serve: a field of config.Config
// that has a yaml key, addressed by its dotted yaml path. A struct field is
// not a key; each of its fields is.
type settingSpec struct {
	key   string
	index []int
	typ   reflect.Type
	kind  schema.LocalSettingKind
	// options is the menu of a choice key.
	options []string
	// readOnly says how the key changes instead of PATCH /api/v1/settings. An
	// empty readOnly means the key is editable.
	readOnly string
	// inPeasantConfig reports that the `peasant config` registry writes the key.
	inPeasantConfig bool
}

// settingChoices names the keys whose value is one of a closed menu, and the
// menu: the values a user may choose, from each type's own closed set. A
// stored value the menu leaves out still reads back: minimal, which applies as
// standard, and maximum or individual, which apply nothing.
var settingChoices = map[string]func() []string{
	"redaction.level":      func() []string { return stringsOf(config.OfferedRedactionLevels) },
	"push.method":          func() []string { return stringsOf(config.OfferedPushMethods) },
	// Keep local wins over stored auto-publish intent.
	"push.autoPublishIntent": func(cfg *config.Config) (any, string) { return cfg.Push.AutoPublishIntended(), "" },
	"push.visibility":      func() []string { return stringsOf(schema.AllVisibilities) },
	"push.sharePreference": func() []string { return stringsOf(config.AllSharePreferences) },
	"push.license":         func() []string { return append([]string{""}, stringsOf(schema.AllLicenses)...) },
	"selection.mode":       func() []string { return stringsOf(config.AllSelectionModes) },
	"display.theme":        func() []string { return stringsOf(config.AllThemes) },
}

// settingReadOnly names the keys, or the key prefixes ending in a dot, that
// PATCH refuses, with how each changes instead. The first match wins, so a
// narrower entry comes before a wider one.
var settingReadOnly = []struct{ key, reason string }{
	{"version", "the configuration format version; peasant sets it"},
	{"village.connected", "changes when you sign in to village or sign out"},
	{"sources.claude.", "retired: renamed to sources.claude-code; a configuration that turns it on is refused"},
	{"push.fields.projectHash", "retired: the project hash is always sent, so this key changes nothing"},
	{"selection.providers", "retired: renamed to selection.harnesses; a configuration that sets it is refused"},
	{"selection.", "the saved selection; change it with peasant kickstart or peasant config"},
	{"sources.mock.", "a development setting the dashboard reads when it starts; edit config.yaml and restart the dashboard"},
	{"output.basePath", "the dashboard reads it when it starts; edit config.yaml and restart the dashboard"},
}

// settingEffective resolves the value that applies for the keys whose stored
// value is not already it: a raised redaction level, a visibility this version
// publishes narrower, a tri-state field that defaults to on, a concurrency of
// 0 that means the CPU-derived default, and a push method no push applies. A
// nil value means no value applies, and refused then says where the stored
// value is refused.
var settingEffective = map[string]func(*config.Config) (value any, refused string){
	"redaction.level": func(cfg *config.Config) (any, string) {
		if level := config.ResolveRedactionPolicy(cfg.Redaction.Level).Effective; level != "" {
			return level, ""
		}
		return nil, "every command that redacts refuses it"
	},
	"push.method": func(cfg *config.Config) (any, string) {
		if cfg.Push.Method == "" || slices.Contains(config.OfferedPushMethods, cfg.Push.Method) {
			return cmp.Or(cfg.Push.Method, config.BaseConfig().Push.Method), ""
		}
		// Individual needs a session picker that does not exist yet: a push
		// applies it only with --source-harness, which the dashboard's publish
		// and a git hook's push never pass.
		return nil, "the dashboard's publish and every git hook push refuse it"
	},
	"push.visibility":         func(cfg *config.Config) (any, string) { return config.EffectiveVisibility("", cfg).Effective, "" },
	"push.fields.gitRemote":   func(cfg *config.Config) (any, string) { return cfg.Push.Fields.Resolve().GitRemote, "" },
	"push.fields.projectPath": func(cfg *config.Config) (any, string) { return cfg.Push.Fields.Resolve().ProjectPath, "" },
	"push.fields.projectName": func(cfg *config.Config) (any, string) { return cfg.Push.Fields.Resolve().ProjectName, "" },
	"push.concurrency": func(cfg *config.Config) (any, string) {
		concurrency, _ := push.ResolveConcurrency(false, 0, cfg.Push.Concurrency, runtime.NumCPU())
		return concurrency, ""
	},
}

// buildSettingCatalog lists every key of config.Config, in declaration order,
// each with its metadata, from the Config type and the `peasant config`
// registry. The server builds it once, when it starts.
func buildSettingCatalog() ([]settingSpec, error) {
	var catalog []settingSpec
	var walkErr error
	walkSettingKeys(reflect.TypeOf(config.Config{}), "", nil, func(key string, index []int, typ reflect.Type) {
		if walkErr != nil {
			return
		}
		spec := settingSpec{key: key, index: index, typ: typ}
		if menu, ok := settingChoices[key]; ok {
			spec.kind, spec.options = schema.LocalSettingChoice, menu()
		} else if typ.Kind() == reflect.String && typ.Implements(closedSetType) {
			walkErr = fmt.Errorf("setting %q: %s is a closed set with no menu; name its menu in settingChoices", key, typ)
			return
		} else if spec.kind, walkErr = settingKindOf(typ); walkErr != nil {
			walkErr = fmt.Errorf("setting %q: %w", key, walkErr)
			return
		}
		for _, entry := range settingReadOnly {
			if key == entry.key || (strings.HasSuffix(entry.key, ".") && strings.HasPrefix(key, entry.key)) {
				spec.readOnly = entry.reason
				break
			}
		}
		catalog = append(catalog, spec)
	})
	if walkErr != nil {
		return nil, walkErr
	}
	for key := range settingChoices {
		if !slices.ContainsFunc(catalog, func(spec settingSpec) bool { return spec.key == key }) {
			return nil, fmt.Errorf("settingChoices names %q, which is not a key of config.Config", key)
		}
	}
	for key := range settingEffective {
		if !slices.ContainsFunc(catalog, func(spec settingSpec) bool { return spec.key == key }) {
			return nil, fmt.Errorf("settingEffective names %q, which is not a key of config.Config", key)
		}
	}
	for _, entry := range settingReadOnly {
		if !slices.ContainsFunc(catalog, func(spec settingSpec) bool {
			return spec.key == entry.key || strings.HasPrefix(spec.key, entry.key)
		}) {
			return nil, fmt.Errorf("settingReadOnly names %q, which matches no key of config.Config", entry.key)
		}
	}
	edited := peasantConfigKeys(catalog)
	for i := range catalog {
		catalog[i].inPeasantConfig = edited[catalog[i].key]
	}
	return catalog, nil
}

// closedSetType is the interface a closed string set implements.
var closedSetType = reflect.TypeFor[interface{ IsValid() bool }]()

// lookupSetting returns the catalog entry for key.
func lookupSetting(catalog []settingSpec, key string) (settingSpec, bool) {
	for _, spec := range catalog {
		if spec.key == key {
			return spec, true
		}
	}
	return settingSpec{}, false
}

// walkSettingKeys visits every field of typ that has a yaml key, descending
// into struct fields. A field with no yaml name has the key yaml gives it: its
// name in lower case.
func walkSettingKeys(typ reflect.Type, prefix string, index []int, visit func(key string, index []int, typ reflect.Type)) {
	for i := range typ.NumField() {
		field := typ.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if !field.IsExported() || name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		key := prefix + name
		fieldIndex := append(slices.Clone(index), i)
		if field.Type.Kind() == reflect.Struct {
			walkSettingKeys(field.Type, key+".", fieldIndex, visit)
			continue
		}
		visit(key, fieldIndex, field.Type)
	}
}

// settingKindOf names the contract kind of a Go field type.
func settingKindOf(typ reflect.Type) (schema.LocalSettingKind, error) {
	switch typ.Kind() {
	case reflect.Bool:
		return schema.LocalSettingBoolean, nil
	case reflect.Pointer:
		if typ.Elem().Kind() == reflect.Bool {
			return schema.LocalSettingBoolean, nil
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return schema.LocalSettingInteger, nil
	case reflect.String:
		return schema.LocalSettingString, nil
	case reflect.Slice:
		switch typ.Elem().Kind() {
		case reflect.String:
			return schema.LocalSettingStringList, nil
		case reflect.Struct:
			return schema.LocalSettingStructured, nil
		}
	case reflect.Map:
		return schema.LocalSettingStructured, nil
	}
	return "", fmt.Errorf("Go type %s has no settings kind; map it in settingKindOf", typ)
}

// peasantConfigKeys returns the keys the `peasant config` registry writes. It
// asks the registry: each field copies its value from an empty configuration
// onto one where every key holds something else, and the keys that change are
// the field's.
func peasantConfigKeys(catalog []settingSpec) map[string]bool {
	edited := make(map[string]bool)
	registry := kickstart.BuildRegistry(kickstart.Options{})
	registry.FieldWrites(func() config.Config { return config.Config{} }, filledConfig, func(_ settings.Field, written *config.Config) {
		untouched := filledConfig()
		for _, spec := range catalog {
			before := reflect.ValueOf(&untouched).Elem().FieldByIndex(spec.index).Interface()
			after := reflect.ValueOf(written).Elem().FieldByIndex(spec.index).Interface()
			if !reflect.DeepEqual(before, after) {
				edited[spec.key] = true
			}
		}
	})
	return edited
}

// filledConfig returns a configuration whose every field holds a value that
// differs from its zero value.
func filledConfig() config.Config {
	var cfg config.Config
	fill(reflect.ValueOf(&cfg).Elem())
	return cfg
}

func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Field(i).CanSet() {
				fill(v.Field(i))
			}
		}
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.String:
		v.SetString("filled")
	case reflect.Pointer:
		target := reflect.New(v.Type().Elem())
		fill(target.Elem())
		v.Set(target)
	case reflect.Slice:
		items := reflect.MakeSlice(v.Type(), 1, 1)
		fill(items.Index(0))
		v.Set(items)
	case reflect.Map:
		key := reflect.New(v.Type().Key()).Elem()
		value := reflect.New(v.Type().Elem()).Elem()
		fill(key)
		fill(value)
		entries := reflect.MakeMap(v.Type())
		entries.SetMapIndex(key, value)
		v.Set(entries)
	}
}

// row builds the setting as the file names it and as cfg applies it.
func (s settingSpec) row(document *yaml.Node, cfg *config.Config) (schema.LocalSetting, error) {
	value, err := s.fileValue(document)
	if err != nil {
		return schema.LocalSetting{}, err
	}
	effective, refused, err := s.effectiveValue(cfg)
	if err != nil {
		return schema.LocalSetting{}, err
	}
	if !value.IsUnset() && effective.IsUnset() {
		return schema.LocalSetting{}, &settingNotAppliedError{key: s.key, value: value, refused: refused, options: s.options}
	}
	return schema.LocalSetting{
		Key:             s.key,
		Kind:            s.kind,
		Value:           value,
		Effective:       effective,
		Options:         s.options,
		Editable:        s.readOnly == "",
		InPeasantConfig: s.inPeasantConfig,
		Description:     s.readOnly,
	}, nil
}

// fileValue is the key's value as the configuration document names it, or
// JSON null when it does not name the key.
func (s settingSpec) fileValue(document *yaml.Node) (schema.LocalSettingValue, error) {
	node := findSettingNode(document, strings.Split(s.key, "."))
	if node == nil {
		return schema.LocalSettingNull(), nil
	}
	typed := reflect.New(s.typ)
	if err := node.Decode(typed.Interface()); err != nil {
		return nil, fmt.Errorf("read %s from the configuration file: %w", s.key, err)
	}
	return settingJSON(typed.Elem().Interface())
}

// settingNotAppliedError reports that the configuration file names a value
// that applies nowhere the dashboard uses it. The contract has no row for such
// a value, so the settings read refuses instead of claiming a value applies.
type settingNotAppliedError struct {
	key     string
	value   schema.LocalSettingValue
	refused string
	options []string
}

func (e *settingNotAppliedError) Error() string {
	return fmt.Sprintf("config.yaml sets %s to %s, and %s; set %s to one of %s with PATCH %s or in config.yaml",
		e.key, e.value, e.refused, e.key, quotedOptions(e.options), defaults.RouteSettings)
}

// effectiveValue is the value that applies under cfg. When no value applies,
// refused says where the stored value is refused.
func (s settingSpec) effectiveValue(cfg *config.Config) (effective schema.LocalSettingValue, refused string, err error) {
	var value any
	if resolve, ok := settingEffective[s.key]; ok {
		value, refused = resolve(cfg)
	} else {
		field := reflect.ValueOf(cfg).Elem().FieldByIndex(s.index)
		switch {
		case s.kind == schema.LocalSettingChoice && !slices.Contains(s.options, field.String()):
			if field.String() != "" {
				// Parse accepts only a type's closed set, and a menu without a
				// resolver is that whole set, so this is a catalog defect.
				return nil, "", fmt.Errorf("the stored value %q is outside the menu %s and no resolver names what applies; map it in settingEffective", field.String(), quotedOptions(s.options))
			}
			// An empty choice means the default applies.
			field = reflect.ValueOf(config.BaseConfig()).Elem().FieldByIndex(s.index)
		case (field.Kind() == reflect.Slice || field.Kind() == reflect.Map) && field.IsNil():
			// An unset list or map applies as an empty one.
			if field.Kind() == reflect.Slice {
				field = reflect.MakeSlice(field.Type(), 0, 0)
			} else {
				field = reflect.MakeMap(field.Type())
			}
		}
		value = field.Interface()
	}
	effective, err = settingJSON(value)
	return effective, refused, err
}

// settingJSON renders a configuration value as JSON with the yaml key names
// config.yaml uses, so a structured value reads back the way the file spells it.
func settingJSON(value any) (schema.LocalSettingValue, error) {
	encoded, err := yaml.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("render a setting value: %w", err)
	}
	var generic any
	if err := yaml.Unmarshal(encoded, &generic); err != nil {
		return nil, fmt.Errorf("render a setting value: %w", err)
	}
	data, err := json.Marshal(generic)
	if err != nil {
		return nil, fmt.Errorf("render a setting value: %w", err)
	}
	return schema.LocalSettingValue(data), nil
}

// decodeSettingValue reads a set JSON value into the key's Go type through the
// yaml key names, refusing a field the type does not have.
func (s settingSpec) decodeSettingValue(raw schema.LocalSettingValue) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, err
	}
	encoded, err := yaml.Marshal(plainJSONNumbers(generic))
	if err != nil {
		return nil, err
	}
	typed := reflect.New(s.typ)
	strict := yaml.NewDecoder(bytes.NewReader(encoded))
	strict.KnownFields(true)
	if err := strict.Decode(typed.Interface()); err != nil {
		return nil, err
	}
	return typed.Elem().Interface(), nil
}

// plainJSONNumbers replaces each json.Number with an int64, or a float64 when
// it is not an integer, so YAML writes it as a number rather than a string.
func plainJSONNumbers(value any) any {
	switch typed := value.(type) {
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return integer
		}
		float, _ := typed.Float64()
		return float
	case []any:
		for i := range typed {
			typed[i] = plainJSONNumbers(typed[i])
		}
	case map[string]any:
		for key := range typed {
			typed[key] = plainJSONNumbers(typed[key])
		}
	}
	return value
}

// settingDocumentRoot returns the top-level mapping of a configuration
// document, or nil when the document names nothing.
func settingDocumentRoot(document *yaml.Node) *yaml.Node {
	if document == nil || document.Kind != yaml.DocumentNode || len(document.Content) == 0 {
		return nil
	}
	return document.Content[0]
}

// findSettingNode returns the value node at path, or nil when the document does
// not name it. An explicit null names the key as unset, which reads the same.
func findSettingNode(document *yaml.Node, path []string) *yaml.Node {
	node := settingDocumentRoot(document)
	for _, segment := range path {
		for node != nil && node.Kind == yaml.AliasNode {
			node = node.Alias
		}
		if node == nil || node.Kind != yaml.MappingNode {
			return nil
		}
		node = mappingValue(node, segment)
	}
	for node != nil && node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	return node
}

// mappingValue returns the value mapping names under key, or nil.
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// setSettingNode names value at path in document, creating the mappings on the
// way, or removes the key when value is nil. It refuses to edit through a YAML
// alias, because the change would also land wherever the anchor is used.
func setSettingNode(document *yaml.Node, path []string, value *yaml.Node) error {
	if document.Kind != yaml.DocumentNode {
		*document = yaml.Node{Kind: yaml.DocumentNode}
	}
	if len(document.Content) == 0 || isYAMLNull(document.Content[0]) {
		document.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	node := document.Content[0]
	for depth, segment := range path {
		if node.Kind == yaml.AliasNode {
			return fmt.Errorf("the configuration file names %s through a YAML alias", strings.Join(path[:depth], "."))
		}
		if node.Kind != yaml.MappingNode {
			return fmt.Errorf("the configuration file names %s as a value that is not a mapping", strings.Join(path[:depth], "."))
		}
		last := depth == len(path)-1
		position := -1
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == segment {
				position = i
				break
			}
		}
		switch {
		case last && value == nil:
			if position >= 0 {
				node.Content = slices.Delete(node.Content, position, position+2)
			}
			return nil
		case last:
			if position >= 0 {
				previous := node.Content[position+1]
				if previous.Kind == yaml.AliasNode {
					return fmt.Errorf("the configuration file names %s through a YAML alias", strings.Join(path, "."))
				}
				value.HeadComment, value.LineComment, value.FootComment = previous.HeadComment, previous.LineComment, previous.FootComment
				node.Content[position+1] = value
			} else {
				node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: segment}, value)
			}
			return nil
		case position < 0:
			if value == nil {
				return nil
			}
			child := &yaml.Node{Kind: yaml.MappingNode}
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: segment}, child)
			node = child
		default:
			child := node.Content[position+1]
			if isYAMLNull(child) {
				if value == nil {
					return nil
				}
				child = &yaml.Node{Kind: yaml.MappingNode, HeadComment: child.HeadComment, LineComment: child.LineComment}
				node.Content[position+1] = child
			}
			node = child
		}
	}
	return nil
}

// isYAMLNull reports whether node is an explicit or empty null scalar.
func isYAMLNull(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && node.ShortTag() == "!!null"
}

// stringsOf renders a closed set of string values in its declared order.
func stringsOf[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = string(value)
	}
	return out
}
