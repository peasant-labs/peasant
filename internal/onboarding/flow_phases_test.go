package onboarding_test

import (
	_ "embed"
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/flow_phases.yaml
var flowPhasesData []byte

type flowCase struct {
	Name             string         `yaml:"name"`
	Scope            string         `yaml:"scope"`
	VillageConnected bool           `yaml:"villageConnected"`
	LoginAvailable   bool           `yaml:"loginAvailable"`
	ClaudeSessions   bool           `yaml:"claudeSessions"`
	ConnectAnswer    string         `yaml:"connectAnswer"`
	SelectionMode    string         `yaml:"selectionMode"`
	AfterStop        string         `yaml:"afterStop"`
	Offer            *bool          `yaml:"offer"`
	Prompt           string         `yaml:"prompt"`
	Skip             string         `yaml:"skip"`
	VisibleStops     []string       `yaml:"visibleStops"`
	HiddenStops      []string       `yaml:"hiddenStops"`
	Sections         []string       `yaml:"sections"`
	Stops            []string       `yaml:"stops"`
	DropAnswers      map[string]any `yaml:"dropAnswers"`
	Remaining        []string       `yaml:"remaining"`
	Absent           []string       `yaml:"absent"`
	AutoIngest       *bool          `yaml:"autoIngest"`
}

type flowDocument struct {
	RequiredNames []string   `yaml:"requiredNames"`
	Cases         []flowCase `yaml:"cases"`
}

func TestFlowPhases(t *testing.T) {
	t.Parallel()
	var document flowDocument
	decodeFixture(t, "flow_phases.yaml", flowPhasesData, &document)
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
	}
	requireNames(t, document.RequiredNames, names)
	for _, row := range document.Cases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			scope := onboarding.ScopeFull
			if row.Scope == "selection" {
				scope = onboarding.ScopeSelection
			}
			model := onboarding.BuildModel(onboarding.Options{Scope: scope})
			cfg := config.BaseConfig()
			if row.SelectionMode != "" {
				cfg.Selection.Mode = config.SelectionMode(row.SelectionMode)
			}
			answer := onboarding.ConnectUnanswered
			if row.ConnectAnswer != "" {
				parsed, err := onboarding.NewConnectAnswer(row.ConnectAnswer)
				if err != nil {
					t.Fatal(err)
				}
				answer = parsed
			}
			context := onboarding.WriteContext{
				Config:           cfg,
				VillageConnected: row.VillageConnected,
				LoginAvailable:   row.LoginAvailable,
				ClaudeSessions:   row.ClaudeSessions,
				ConnectAnswer:    answer,
			}
			if row.AfterStop != "" {
				key, err := onboarding.NewFlowKey(row.AfterStop)
				if err != nil {
					t.Fatal(err)
				}
				offer, shown := model.ConnectOfferAfter(key, context)
				if row.Offer != nil && shown != *row.Offer {
					t.Fatalf("offer shown = %t, want %t", shown, *row.Offer)
				}
				if shown && row.Prompt != "" && offer.Prompt != row.Prompt {
					t.Fatalf("prompt = %q, want %q", offer.Prompt, row.Prompt)
				}
				if shown && row.Skip != "" && offer.Skip != row.Skip {
					t.Fatalf("skip = %q, want %q", offer.Skip, row.Skip)
				}
			}
			for _, key := range row.VisibleStops {
				stop, ok := model.StopByKey(mustFlow(t, key))
				if !ok {
					t.Fatalf("missing stop %s", key)
				}
				if !stop.Shown(context) {
					t.Errorf("stop %s hidden, want visible", key)
				}
			}
			for _, key := range row.HiddenStops {
				stop, ok := model.StopByKey(mustFlow(t, key))
				if !ok {
					t.Fatalf("missing stop %s", key)
				}
				if stop.Shown(context) {
					t.Errorf("stop %s visible, want hidden", key)
				}
			}
			if row.Sections != nil {
				got := sectionKeys(model)
				if err := yamlEqual(got, row.Sections); err != nil {
					t.Fatalf("sections: %v", err)
				}
			}
			if row.Stops != nil {
				got := stopKeys(model)
				if err := yamlEqual(got, row.Stops); err != nil {
					t.Fatalf("stops: %v", err)
				}
			}
			if row.DropAnswers != nil {
				answers := onboarding.Answers{}
				for key, raw := range row.DropAnswers {
					fieldKey, err := onboarding.NewFieldKey(key)
					if err != nil {
						t.Fatal(err)
					}
					encoded, err := json.Marshal(raw)
					if err != nil {
						t.Fatal(err)
					}
					answers[fieldKey] = schema.LocalSettingValue(encoded)
				}
				remaining, err := model.DropHidden(cfg, answers, context)
				if err != nil {
					t.Fatal(err)
				}
				for _, key := range row.Remaining {
					fieldKey, err := onboarding.NewFieldKey(key)
					if err != nil {
						t.Fatal(err)
					}
					if _, ok := remaining[fieldKey]; !ok {
						t.Errorf("answer %s was dropped", key)
					}
				}
				for _, key := range row.Absent {
					fieldKey, err := onboarding.NewFieldKey(key)
					if err != nil {
						t.Fatal(err)
					}
					if _, ok := remaining[fieldKey]; ok {
						t.Errorf("answer %s remained", key)
					}
				}
				if err := model.ApplyAnswers(cfg, remaining, context); err != nil {
					t.Fatal(err)
				}
				if row.AutoIngest != nil && cfg.Selection.AutoIngestNewBranches != *row.AutoIngest {
					t.Fatalf("auto-ingest = %t, want %t", cfg.Selection.AutoIngestNewBranches, *row.AutoIngest)
				}
			}
		})
	}
}

func mustFlow(t *testing.T, key string) onboarding.FlowKey {
	t.Helper()
	parsed, err := onboarding.NewFlowKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func sectionKeys(model onboarding.Model) []string {
	sections := model.SectionStops()
	out := make([]string, len(sections))
	for i, stop := range sections {
		out[i] = stop.Key.String()
	}
	return out
}

func stopKeys(model onboarding.Model) []string {
	out := make([]string, len(model.Stops))
	for i, stop := range model.Stops {
		out[i] = stop.Key.String()
	}
	return out
}
