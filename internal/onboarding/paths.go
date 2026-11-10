package onboarding

import (
	"encoding/json"
	"fmt"

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

// ProjectAnswered writes each answered field that has a YAML path into doc from
// cfg. Retention and other path-less fields are skipped, so they never appear
// in config.yaml. Selection is written as the one mapping at its path.
func (m Model) ProjectAnswered(doc *yaml.Node, cfg *config.Config, answered []FieldKey) error {
	if cfg == nil {
		return documentPathError("the configuration pointer is nil", "pass the configuration the answers produced")
	}
	wanted := map[FieldKey]bool{}
	for _, key := range answered {
		wanted[key] = true
	}
	for _, field := range m.Fields() {
		if !wanted[field.Key()] || len(field.YAMLPath()) == 0 {
			continue
		}
		value, err := projectedValue(cfg, field)
		if err != nil {
			return err
		}
		if err := SetDocumentPath(doc, field.YAMLPath(), value); err != nil {
			return err
		}
	}
	return nil
}

func projectedValue(cfg *config.Config, field Field) (any, error) {
	if field.Key() == FieldSelection {
		return cfg.Selection, nil
	}
	raw, err := field.Read(cfg)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("project %s into the document: %w", field.Key(), err)
	}
	return value, nil
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
