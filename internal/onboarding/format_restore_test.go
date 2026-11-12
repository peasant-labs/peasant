package onboarding_test

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/onboarding"
)

//go:embed testdata/format_restore.yaml
var formatRestoreData []byte

type formatRestoreCase struct {
	Name     string `yaml:"name"`
	Original string `yaml:"original"`
	Encoded  string `yaml:"encoded"`
	Want     string `yaml:"want"`
}

type formatRestoreDocument struct {
	RequiredNames []string            `yaml:"requiredNames"`
	Cases         []formatRestoreCase `yaml:"cases"`
}

func TestRestoreDocumentFormat(t *testing.T) {
	t.Parallel()
	var document formatRestoreDocument
	decodeFixture(t, "format_restore.yaml", formatRestoreData, &document)
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
	}
	requireNames(t, document.RequiredNames, names)
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			got := onboarding.RestoreDocumentFormat([]byte(row.Original), []byte(row.Encoded))
			if string(got) != row.Want {
				t.Errorf("restored text differs\n got: %q\nwant: %q", got, row.Want)
			}
		})
	}
}
