package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
)

// TestKickstartServedModelEqualsTheNeutralModel opens a session for each scope
// and compares the stops the server serves with the neutral setup model: the
// same stops in the same order, with the same kinds, titles, visibility, fields,
// labels, descriptions, and option menus. A rule the page could re-derive would
// be one this comparison finds on only one side.
func TestKickstartServedModelEqualsTheNeutralModel(t *testing.T) {
	t.Parallel()
	var sets kickstartListingSets
	if err := yaml.Unmarshal(kickstartListingsYAML, &sets); err != nil {
		t.Fatal(err)
	}
	for _, scope := range onboarding.AllModelScopes {
		t.Run(scope.String(), func(t *testing.T) {
			t.Parallel()
			world := newKickstartWorld(t, kickstartCase{Name: "served-model-" + scope.String(), World: kickstartSpec{Listings: "two-projects", Gate: true}}, sets)
			body, _ := json.Marshal(map[string]string{"scope": scope.String()})
			request, err := http.NewRequest(http.MethodPost, world.baseURL+defaults.RouteKickstartSessions.String(), bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set(defaults.HeaderOrigin, world.baseURL)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			raw, _ := io.ReadAll(response.Body)
			var served schema.LocalKickstartSession
			if err := json.Unmarshal(raw, &served); err != nil {
				t.Fatalf("the served session does not decode under the contract: %v\n%s", err, raw)
			}

			model := onboarding.BuildModel(onboarding.Options{Scope: scope})
			// The session opens before the scan finds anything, signed out, with
			// the defaults every command applies to a missing file.
			facts := onboarding.WriteContext{Config: config.BaseConfig(), LoginAvailable: true}
			if len(served.Stops) != len(model.Stops) {
				t.Fatalf("served %d stops, the %s model has %d", len(served.Stops), scope, len(model.Stops))
			}
			for index, stop := range model.Stops {
				row := served.Stops[index]
				if row.Key != stop.Key.String() || row.Title != stop.Title || row.Kind.String() != stop.Key.Kind().String() {
					t.Errorf("stop %d is %s %q %s, the model has %s %q %s", index, row.Key, row.Title, row.Kind, stop.Key, stop.Title, stop.Key.Kind())
				}
				if row.Visible != stop.Shown(facts) {
					t.Errorf("stop %s visible = %t, the model says %t", stop.Key, row.Visible, stop.Shown(facts))
				}
				if row.Guide.Intro != stop.Guide.Intro || len(row.Guide.Hints) != len(stop.Guide.Hints) {
					t.Errorf("stop %s guide differs from the model's", stop.Key)
				}
				if len(row.Fields) != len(stop.Fields) {
					t.Errorf("stop %s serves %d fields, the model has %d", stop.Key, len(row.Fields), len(stop.Fields))
					continue
				}
				for position, field := range stop.Fields {
					servedField := row.Fields[position]
					label := field.Label()
					if label == "" {
						label = stop.Title
					}
					if servedField.Key != field.Key().String() || servedField.Kind.String() != field.Kind().String() || servedField.Label != label || servedField.Description != field.Description() {
						t.Errorf("field %s differs from the model's: served %s %s %q %q", field.Key(), servedField.Key, servedField.Kind, servedField.Label, servedField.Description)
					}
					options := field.Options()
					if len(servedField.Options) != len(options) {
						t.Errorf("field %s serves %d options, the model has %d", field.Key(), len(servedField.Options), len(options))
						continue
					}
					for i, option := range options {
						if servedField.Options[i].Value != option.Value || servedField.Options[i].Label != option.Label || servedField.Options[i].Description != option.Description {
							t.Errorf("field %s option %d is %+v, the model has %+v", field.Key(), i, servedField.Options[i], option)
						}
					}
				}
			}
			if offers := model.ConnectOffers(); (len(offers) > 0) != served.Connect.Offered {
				t.Errorf("connect offered = %t, the %s model has %d offers", served.Connect.Offered, scope, len(offers))
			}
		})
	}
}
