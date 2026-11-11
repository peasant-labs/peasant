package onboarding

import (
	"bytes"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"gopkg.in/yaml.v3"
)

// FieldPath is one field's document location. A nil Path writes no config.yaml key.
type FieldPath struct {
	Key         FieldKey
	Destination Destination
	Path        []string
}

// FieldPaths returns each field's destination and YAML path in declaration order.
func (m Model) FieldPaths() []FieldPath {
	out := make([]FieldPath, 0, len(m.Fields()))
	for _, field := range m.Fields() {
		out = append(out, FieldPath{
			Key:         field.Key(),
			Destination: field.Destination(),
			Path:        field.YAMLPath(),
		})
	}
	return out
}

// SetDocumentPath writes value at path without reordering sibling keys or
// dropping their comments. A missing key is appended. Comments on the replaced
// value node are copied onto the new value.
func SetDocumentPath(root *yaml.Node, path []string, value any) error {
	if root == nil {
		return documentPathError("the YAML document is nil", "pass a document node")
	}
	if len(path) == 0 {
		return documentPathError("the path is empty", "pass the field's YAML path")
	}
	mapping, err := ensureMapping(root)
	if err != nil {
		return err
	}
	encoded, err := encodeValue(value)
	if err != nil {
		return err
	}
	return setMappingPath(mapping, path, encoded)
}

// DocumentPath returns the node at path. The bool is false when any segment is missing.
func DocumentPath(root *yaml.Node, path []string) (*yaml.Node, bool) {
	if root == nil || len(path) == 0 {
		return nil, false
	}
	mapping := mappingNode(root)
	if mapping == nil {
		return nil, false
	}
	return lookupMappingPath(mapping, path)
}

// ProjectEdits writes into doc every configuration value the answers set. A
// field is written when the user answered it, and also when applying the answers
// moved its stored value away from base: a choice can move another stored key
// (publishing automatically sets the share preference, keeping transcripts
// local clears the intent), and leaving that key behind would make the file
// say something the user did not choose.
//
// Each value is the one the configuration holds at the field's YAML path, with
// the type the configuration gives it, so a boolean is written as a boolean.
// Fields with no path (retention) are skipped and never enter config.yaml.
// Selection is written as the one mapping at its path. A field the answers did
// not touch is left exactly as doc has it.
func (m Model) ProjectEdits(doc *yaml.Node, base, working *config.Config, answered []FieldKey) error {
	if base == nil || working == nil {
		return documentPathError("a configuration pointer is nil", "pass the configuration before the answers and the one they produced")
	}
	wanted := map[FieldKey]bool{}
	for _, key := range answered {
		wanted[key] = true
	}
	for _, field := range m.Fields() {
		path := field.YAMLPath()
		if len(path) == 0 {
			continue
		}
		after, err := configValueAtPath(working, path)
		if err != nil {
			return err
		}
		if !wanted[field.Key()] {
			before, err := configValueAtPath(base, path)
			if err != nil {
				return err
			}
			if sameYAMLValue(before, after) {
				continue
			}
		}
		if err := SetDocumentPath(doc, path, after); err != nil {
			return err
		}
	}
	return nil
}

// configValueAtPath returns what the configuration holds at a YAML path, found
// through the struct's own yaml tags. The configuration decides the type, so
// this is the representation the terminal's whole-document save writes.
func configValueAtPath(cfg *config.Config, path []string) (any, error) {
	current := reflect.ValueOf(cfg).Elem()
	for depth, segment := range path {
		next, ok := yamlStructField(current, segment)
		if !ok {
			return nil, documentPathError(
				fmt.Sprintf("the configuration has no value at %s (stopped at %q, segment %d)", joinPath(path), segment, depth+1),
				"give the field a YAML path the configuration declares")
		}
		current = next
	}
	return current.Interface(), nil
}

// yamlStructField finds the field of struct v that the YAML key name maps to,
// looking through inline structs. yaml.v3 names an untagged field by its
// lowercased Go name.
func yamlStructField(v reflect.Value, name string) (reflect.Value, bool) {
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		key, options, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if key == "-" {
			continue
		}
		if slices.Contains(strings.Split(options, ","), "inline") {
			if inner, ok := yamlStructField(v.Field(i), name); ok {
				return inner, true
			}
			continue
		}
		if key == "" {
			key = strings.ToLower(field.Name)
		}
		if key == name {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// sameYAMLValue compares two configuration values by what they encode to.
func sameYAMLValue(a, b any) bool {
	left, leftErr := yaml.Marshal(a)
	right, rightErr := yaml.Marshal(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

func documentPathError(why, fix string) error {
	return actionable(
		"write a configuration document path",
		why,
		"onboarding document path",
		"while projecting an answered field into YAML",
		"the document was not changed by this call",
		fix)
}

func encodeValue(value any) (*yaml.Node, error) {
	var node yaml.Node
	if err := node.Encode(value); err != nil {
		return nil, fmt.Errorf("encode a document value: %w", err)
	}
	return &node, nil
}

func ensureMapping(root *yaml.Node) (*yaml.Node, error) {
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			mapping := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			root.Content = []*yaml.Node{mapping}
		}
		if root.Content[0].Kind != yaml.MappingNode {
			return nil, documentPathError("the document root is not a mapping", "start from a mapping document")
		}
		return root.Content[0], nil
	}
	if root.Kind == yaml.MappingNode {
		return root, nil
	}
	if root.Kind == 0 {
		root.Kind = yaml.DocumentNode
		mapping := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = []*yaml.Node{mapping}
		return mapping, nil
	}
	return nil, documentPathError("the YAML node is not a document or mapping", "pass a document node")
}

func mappingNode(root *yaml.Node) *yaml.Node {
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
			return nil
		}
		return root.Content[0]
	}
	if root.Kind == yaml.MappingNode {
		return root
	}
	return nil
}

func setMappingPath(mapping *yaml.Node, path []string, value *yaml.Node) error {
	if len(path) == 0 {
		return documentPathError("the path is empty", "pass the field's YAML path")
	}
	key := path[0]
	index := mappingKeyIndex(mapping, key)
	if len(path) == 1 {
		if index < 0 {
			mapping.Content = append(mapping.Content, scalarKey(key), value)
			return nil
		}
		copyNodeComments(mapping.Content[index+1], value)
		mapping.Content[index+1] = value
		return nil
	}
	var child *yaml.Node
	if index < 0 {
		child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		mapping.Content = append(mapping.Content, scalarKey(key), child)
	} else {
		child = mapping.Content[index+1]
		if child.Kind != yaml.MappingNode {
			replacement := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			copyNodeComments(child, replacement)
			mapping.Content[index+1] = replacement
			child = replacement
		}
	}
	return setMappingPath(child, path[1:], value)
}

func lookupMappingPath(mapping *yaml.Node, path []string) (*yaml.Node, bool) {
	if len(path) == 0 {
		return mapping, true
	}
	index := mappingKeyIndex(mapping, path[0])
	if index < 0 {
		return nil, false
	}
	child := mapping.Content[index+1]
	if len(path) == 1 {
		return child, true
	}
	if child.Kind != yaml.MappingNode {
		return nil, false
	}
	return lookupMappingPath(child, path[1:])
}

func mappingKeyIndex(mapping *yaml.Node, key string) int {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func scalarKey(key string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
}

func copyNodeComments(from, to *yaml.Node) {
	if from == nil || to == nil {
		return
	}
	to.HeadComment = from.HeadComment
	to.LineComment = from.LineComment
	to.FootComment = from.FootComment
}
