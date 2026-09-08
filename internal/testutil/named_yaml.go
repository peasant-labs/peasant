package testutil

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// DecodeFixtureYAML accepts exactly one document with no unknown fields.
func DecodeFixtureYAML(data []byte, target any) error {
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("fixture must contain exactly one YAML document: %v", err)
	}
	return nil
}

// DecodeNamedFixtureYAML also validates the requiredNames/cases name inventory.
// Decode the complete typed target first so the inventory pass cannot hide an
// unknown field or a malformed model expectation.
func DecodeNamedFixtureYAML(data []byte, target any) error {
	if err := DecodeFixtureYAML(data, target); err != nil {
		return err
	}
	var inventory struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name string `yaml:"name"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &inventory); err != nil {
		return err
	}
	names := make([]string, 0, len(inventory.Cases))
	present := make(map[string]bool)
	for _, c := range inventory.Cases {
		if strings.TrimSpace(c.Name) == "" || present[c.Name] {
			return fmt.Errorf("fixture has empty or duplicate case name %q", c.Name)
		}
		present[c.Name] = true
		names = append(names, c.Name)
	}
	if err := RequireFixtureNames("YAML", "case", inventory.RequiredNames, present); err != nil {
		return err
	}
	return ValidateRequiredNames(RequiredNamesManifest{RequiredNames: inventory.RequiredNames}, names, "YAML")
}
