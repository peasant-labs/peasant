package e2e

import (
	"os"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
)

const (
	envPeasantBin                 defaults.EnvVar = "PEASANT_BIN"
	envDatabaseURL                defaults.EnvVar = "DATABASE_URL"
	envS3Endpoint                 defaults.EnvVar = "S3_ENDPOINT"
	envS3Bucket                   defaults.EnvVar = "S3_BUCKET"
	envTranscriptKEKActiveVersion defaults.EnvVar = "TRANSCRIPT_KEK_ACTIVE_VERSION"
	envTranscriptKEKKeyring       defaults.EnvVar = "TRANSCRIPT_KEK_KEYRING"
	envVillageURL                 defaults.EnvVar = "VILLAGE_URL"
	envVillageBin                 defaults.EnvVar = "VILLAGE_BIN"
	envSetupDemoBin               defaults.EnvVar = "SETUP_DEMO_BIN"
	envVillageRepo                defaults.EnvVar = "VILLAGE_REPO"
	envVillageBackendDir          defaults.EnvVar = "VILLAGE_BACKEND_DIR"
	envReleaseE2EDist             defaults.EnvVar = "RELEASE_E2E_DIST"
	envPodmanContainer            defaults.EnvVar = "PEASANT_PODMAN_CONTAINER"

	envPeasantBinFatalHelper       defaults.EnvVar = "PEASANT_BIN_FATAL_HELPER"
	envBaselineFatalHelper         defaults.EnvVar = "BASELINE_FATAL_HELPER"
	envRefreshExternalFatalHelper  defaults.EnvVar = "REFRESH_EXTERNAL_FATAL_HELPER"
	envRefreshMissingDBFatalHelper defaults.EnvVar = "REFRESH_MISSING_DB_FATAL_HELPER"
	envVillageProcDeathHelper      defaults.EnvVar = "VILLAGE_PROCDEATH_HELPER"
)

func getenv(name defaults.EnvVar) string {
	return os.Getenv(name.String())
}

const (
	testTranscriptKEKVersion = "1"
	testTranscriptKEKKeyring = `{"1":"MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}`
)

func villageEncryptionEnvAssignments() []string {
	return []string{
		envAssignment(envTranscriptKEKActiveVersion, testTranscriptKEKVersion),
		envAssignment(envTranscriptKEKKeyring, testTranscriptKEKKeyring),
	}
}

func setenv(t *testing.T, name defaults.EnvVar, value string) {
	t.Helper()
	t.Setenv(name.String(), value)
}

func envAssignment(name defaults.EnvVar, value string) string {
	return name.String() + "=" + value
}

func xdgEnvAssignments(dataHome, configHome, stateHome string) []string {
	return []string{
		envAssignment(defaults.EnvXDGDataHome, dataHome),
		envAssignment(defaults.EnvXDGConfigHome, configHome),
		envAssignment(defaults.EnvXDGStateHome, stateHome),
	}
}
