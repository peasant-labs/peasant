package ingest

import (
	"strings"
	"testing"
)

func TestValidatePathComponent(t *testing.T) {
	tests := []struct {
		name      string
		component string
		wantErr   bool
		errMsg    string
	}{
		// Valid components
		{"simple name", "session-abc", false, ""},
		{"uuid", "99d59925-36bc-424c-a789-8be54d9702ba", false, ""},
		{"with underscore", "my_file", false, ""},
		{"with dot", "metadata.json", false, ""},
		{"numeric", "12345", false, ""},
		{"single char", "a", false, ""},

		// Invalid
		{"empty", "", true, "empty string"},
		{"dot", ".", true, "must not be '.' or '..'"},
		{"dotdot", "..", true, "must not be '.' or '..'"},
		{"forward slash", "foo/bar", true, "path separators"},
		{"backslash", "foo\\bar", true, "path separators"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePathComponent(tt.component)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ValidatePathComponent(%q) = nil, want error containing %q", tt.component, tt.errMsg)
				} else if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("ValidatePathComponent(%q) error = %q, want error containing %q", tt.component, err, tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("ValidatePathComponent(%q) unexpected error: %v", tt.component, err)
				}
			}
		})
	}
}

func TestValidatePathContainment(t *testing.T) {
	tests := []struct {
		name       string
		basePath   string
		targetPath string
		wantErr    bool
		errMsg     string
	}{
		// Contained paths
		{"direct child", "/base", "/base/child", false, ""},
		{"nested child", "/base", "/base/child/grandchild", false, ""},
		{"same path", "/base", "/base", false, ""},

		// Escape attempts
		{"parent traversal", "/base", "/base/../etc/passwd", true, "escapes base directory"},
		{"sibling", "/base/one", "/base/two", true, "escapes base directory"},
		{"completely outside", "/base", "/other/path", true, "escapes base directory"},
		{"root escape", "/base/dir", "/", true, "escapes base directory"},

		// Relative path rejection
		{"relative basePath", "relative/base", "/abs/target", true, "must be absolute"},
		{"relative targetPath", "/abs/base", "relative/target", true, "must be absolute"},
		{"both relative", "rel/base", "rel/target", true, "must be absolute"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePathContainment(tt.basePath, tt.targetPath)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ValidatePathContainment(%q, %q) = nil, want error containing %q", tt.basePath, tt.targetPath, tt.errMsg)
				} else if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("ValidatePathContainment(%q, %q) error = %q, want error containing %q", tt.basePath, tt.targetPath, err, tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("ValidatePathContainment(%q, %q) unexpected error: %v", tt.basePath, tt.targetPath, err)
				}
			}
		})
	}
}
