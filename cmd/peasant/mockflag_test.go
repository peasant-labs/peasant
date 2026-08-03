package main

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
)

func TestParseMockDataStore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		expectComp []defaults.MockComponent
		expectSect []defaults.MockSection
		expectErr  bool
	}{
		{
			name:       "all components",
			input:      "web,tui,api",
			expectComp: []defaults.MockComponent{defaults.MockComponents.Web, defaults.MockComponents.TUI, defaults.MockComponents.API},
		},
		{
			name:       "sections only",
			input:      "dashboard,sessions",
			expectSect: []defaults.MockSection{defaults.MockSections.Dashboard, defaults.MockSections.Sessions},
		},
		{
			name:       "mixed",
			input:      "web,sessions",
			expectComp: []defaults.MockComponent{defaults.MockComponents.Web},
			expectSect: []defaults.MockSection{defaults.MockSections.Sessions},
		},
		{
			name:      "invalid target",
			input:     "web,invalid",
			expectErr: true,
		},
		{
			name:  "none disables mock",
			input: "none",
		},
		{
			name:  "none with whitespace",
			input: "  none  ",
		},
		{
			name:  "empty string",
			input: "",
		},
		{
			name:  "whitespace only",
			input: "   ",
		},
		{
			name:       "trims whitespace",
			input:      " web , dashboard ",
			expectComp: []defaults.MockComponent{defaults.MockComponents.Web},
			expectSect: []defaults.MockSection{defaults.MockSections.Dashboard},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := ParseMockDataStore(tc.input)
			if (err != nil) != tc.expectErr {
				t.Errorf("ParseMockDataStore(%q) error = %v, expectErr %v", tc.input, err, tc.expectErr)
				return
			}
			if tc.expectErr {
				return
			}

			if strings.TrimSpace(tc.input) == "" {
				if opts != nil {
					t.Errorf("expected nil opts for %q, got %v", tc.input, opts)
				}
				return
			}

			for _, c := range tc.expectComp {
				if !opts.HasComponent(c) {
					t.Errorf("expected component %q to be mocked", c)
				}
			}
			for _, s := range tc.expectSect {
				if !opts.HasSection(s) {
					t.Errorf("expected section %q to be mocked", s)
				}
			}
		})
	}
}
