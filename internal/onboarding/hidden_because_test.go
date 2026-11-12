package onboarding_test

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/hidden_because.yaml
var hiddenBecauseData []byte

type hiddenBecauseCase struct {
	Name             string `yaml:"name"`
	Field            string `yaml:"field"`
	VillageConnected bool   `yaml:"villageConnected"`
	ClaudeSessions   bool   `yaml:"claudeSessions"`
	SelectionMode    string `yaml:"selectionMode"`
	AnswerSelection  string `yaml:"answerSelection"`
	WantHidden       bool   `yaml:"wantHidden"`
	WantBecause      string `yaml:"wantBecause"`
}

type hiddenBecauseDocument struct {
	RequiredNames []string            `yaml:"requiredNames"`
	Cases         []hiddenBecauseCase `yaml:"cases"`
}

// TestModelHiddenBecause runs the visibility fixture through the one function
// that decides whether a field is shown.
func TestModelHiddenBecause(t *testing.T) {
	t.Parallel()
	var document hiddenBecauseDocument
	decodeFixture(t, "hidden_because.yaml", hiddenBecauseData, &document)
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
	}
	requireNames(t, document.RequiredNames, names)

	model := onboarding.BuildModel(onboarding.Options{Scope: onboarding.ScopeFull})
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			key, err := onboarding.NewFieldKey(row.Field)
			if err != nil {
				t.Fatalf("fixture field %q: %v", row.Field, err)
			}
			field, ok := model.Field(key)
			if !ok {
				t.Fatalf("the full model has no field %q", row.Field)
			}
			cfg := config.BaseConfig()
			cfg.Selection.Mode = config.SelectionMode(row.SelectionMode)
			write := onboarding.WriteContext{
				Config:           cfg,
				VillageConnected: row.VillageConnected,
				ClaudeSessions:   row.ClaudeSessions,
			}
			if row.AnswerSelection != "" {
				write.Answers = onboarding.Answers{onboarding.FieldSelection: schema.LocalSettingValue(row.AnswerSelection)}
			}
			condition, hidden := model.HiddenBecause(field, write)
			if hidden != row.WantHidden {
				t.Fatalf("HiddenBecause(%s) hidden = %t (%s), want %t", row.Field, hidden, condition, row.WantHidden)
			}
			if hidden && condition.String() != row.WantBecause {
				t.Fatalf("HiddenBecause(%s) reason = %q, want %q", row.Field, condition.String(), row.WantBecause)
			}
		})
	}
}
