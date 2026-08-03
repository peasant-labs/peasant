package main

import (
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
)

// ParseMockDataStore parses a comma-separated list of components and sections.
// Example: "web,dashboard,sessions"
func ParseMockDataStore(value string) (*config.MockDataStoreOptions, error) {
	if value == "" {
		return nil, nil
	}

	// "none" is the explicit opt-out: disable all mock regardless of config.yaml.
	if strings.TrimSpace(value) == "none" {
		return &config.MockDataStoreOptions{
			Components: make(map[defaults.MockComponent]bool),
			Sections:   make(map[defaults.MockSection]bool),
			Disable:    true,
		}, nil
	}

	parts := strings.Split(value, ",")
	opts := &config.MockDataStoreOptions{
		Components: make(map[defaults.MockComponent]bool),
		Sections:   make(map[defaults.MockSection]bool),
	}

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}

		comp := defaults.MockComponent(p)
		if defaults.IsValidMockComponent(comp) {
			opts.Components[comp] = true
			continue
		}

		sect := defaults.MockSection(p)
		if defaults.IsValidMockSection(sect) {
			opts.Sections[sect] = true
			continue
		}

		return nil, fmt.Errorf("invalid mock target %q", p)
	}

	if len(opts.Components) == 0 && len(opts.Sections) == 0 {
		return nil, nil
	}

	return opts, nil
}
