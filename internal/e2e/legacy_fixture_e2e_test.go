//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const v39FixtureRequestEnv = "PEASANT_E2E_V39_FIXTURE"

type v39FixtureRequest struct {
	Destination    string `json:"destination"`
	IngestedSource string `json:"ingestedSource"`
	SessionID      string `json:"sessionID"`
	CommitHash     string `json:"commitHash"`
	Subject        string `json:"subject"`
	AuthorTime     int64  `json:"authorTime"`
	PushedAt       int64  `json:"pushedAt"`
}

func buildV39LegacyFixture(t *testing.T, destination, ingestedSource string, fixture fixtureAssociationRoundTrip) {
	t.Helper()
	request, err := json.Marshal(v39FixtureRequest{
		Destination:    destination,
		IngestedSource: ingestedSource,
		SessionID:      fixture.SessionID,
		CommitHash:     fixture.ObservedCommitHash,
		Subject:        fixture.Subject,
		AuthorTime:     fixture.AuthorTime,
		PushedAt:       fixture.PushedAt,
	})
	if err != nil {
		t.Fatalf("encode V39 fixture request: %v", err)
	}

	root := e2eRepositoryRoot(t)
	cmd := exec.Command("go", "test", "-race", "-tags=e2e", "./internal/store", "-run", "^TestBuildV39E2EFixture$", "-count=1")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%s", v39FixtureRequestEnv, request))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build V39 legacy fixture failed:\nwhat: the store test support could not create the pre-V40 database\nwhy: %v\nwhere: internal/e2e/legacy_fixture_e2e_test.go\nwhen: preparing the current CLI upgrade path\nmeans: the E2E cannot prove historical association replay\nfix: inspect the store fixture-builder output and repair the V39 migration setup\noutput:\n%s", err, out)
	}
}

func e2eRepositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("locate E2E repository root: get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("locate E2E repository root: no go.mod found above %s", dir)
		}
		dir = parent
	}
}
