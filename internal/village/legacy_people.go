package village

import (
	"bytes"
	"encoding/json"
	"reflect"

	"github.com/peasant-labs/schema"
)

// --- Reading a person from a village that predates the public owner ---
//
// Village API 0.29.0 turned the person references in collective records into a
// schema.VillagePublicOwner: a collective's created_by account id became
// creator, and a linked repository's linked_by account id became a public
// owner. A village that has not moved yet still sends the old account ids.
// This CLI reads those references from collectives and linked repositories
// only to decode the row, never to show or match a person, so an old or
// unreadable reference becomes anonymous instead of failing the read.

// legacyPersonKeys are the object keys whose value Village API 0.29.0 made a
// public owner and which an older village sends as an account id string. The
// GitHub "owner" of a linked repository is a login string in every version and
// is deliberately not listed.
var legacyPersonKeys = map[string]bool{"creator": true, "linked_by": true}

// decodeVillageJSON unmarshals a village answer into into, accepting the
// person references of a village that predates the public owner. Every public
// owner that arrives as an account id string, as null, or not at all is
// decoded as anonymous, so the value always re-encodes.
func decodeVillageJSON(raw []byte, into any) error {
	if bytes.Contains(raw, []byte(`"created_by"`)) || bytes.Contains(raw, []byte(`"creator"`)) || bytes.Contains(raw, []byte(`"linked_by"`)) {
		var tree any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&tree); err == nil {
			if rewritten, err := json.Marshal(anonymizeLegacyPeople(tree)); err == nil {
				raw = rewritten
			}
		}
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return err
	}
	anonymizeUnsetPublicOwners(reflect.ValueOf(into))
	return nil
}

// anonymizeLegacyPeople replaces each listed person reference that is not a
// readable public owner (an account id string, null, or a malformed object)
// with the anonymous shape, and drops the created_by account id an older
// collective record sends in place of creator.
func anonymizeLegacyPeople(node any) any {
	switch typed := node.(type) {
	case []any:
		for i := range typed {
			typed[i] = anonymizeLegacyPeople(typed[i])
		}
	case map[string]any:
		if _, legacy := typed["created_by"]; legacy {
			delete(typed, "created_by")
			if _, current := typed["creator"]; !current {
				typed["creator"] = map[string]any{"kind": string(schema.VillagePublicOwnerAnonymous)}
			}
		}
		for key, value := range typed {
			if legacyPersonKeys[key] {
				if !readablePublicOwner(value) {
					typed[key] = map[string]any{"kind": string(schema.VillagePublicOwnerAnonymous)}
				}
				continue
			}
			typed[key] = anonymizeLegacyPeople(value)
		}
	}
	return node
}

// readablePublicOwner reports whether value decodes as a public owner.
func readablePublicOwner(value any) bool {
	if _, isObject := value.(map[string]any); !isObject {
		return false
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return false
	}
	var owner schema.VillagePublicOwner
	return json.Unmarshal(encoded, &owner) == nil
}

var publicOwnerType = reflect.TypeOf(schema.VillagePublicOwner{})

// anonymizeUnsetPublicOwners walks a decoded value and turns every public
// owner that is not a profile into the anonymous shape, including the zero
// value a missing key leaves behind.
func anonymizeUnsetPublicOwners(value reflect.Value) {
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !value.IsNil() {
			anonymizeUnsetPublicOwners(value.Elem())
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			anonymizeUnsetPublicOwners(value.Index(i))
		}
	case reflect.Map:
		for _, key := range value.MapKeys() {
			element := value.MapIndex(key)
			if containsPublicOwner(element.Type()) {
				copied := reflect.New(element.Type()).Elem()
				copied.Set(element)
				anonymizeUnsetPublicOwners(copied)
				value.SetMapIndex(key, copied)
			}
		}
	case reflect.Struct:
		if value.Type() == publicOwnerType {
			if owner := value.Interface().(schema.VillagePublicOwner); !owner.IsProfile() && value.CanSet() {
				value.Set(reflect.ValueOf(schema.AnonymousVillageOwner()))
			}
			return
		}
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).IsExported() {
				anonymizeUnsetPublicOwners(value.Field(i))
			}
		}
	}
}

func containsPublicOwner(t reflect.Type) bool {
	return containsPublicOwnerSeen(t, map[reflect.Type]bool{})
}

func containsPublicOwnerSeen(t reflect.Type, seen map[reflect.Type]bool) bool {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t == publicOwnerType {
		return true
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return false
	}
	seen[t] = true
	for i := 0; i < t.NumField(); i++ {
		if containsPublicOwnerSeen(t.Field(i).Type, seen) {
			return true
		}
	}
	return false
}
