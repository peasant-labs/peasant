//go:build e2e

package e2e

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestHarnessRefreshE2E(t *testing.T) {
	bins := resolveVillageBinaries(t)
	stack := provisionHarnessStack(t, bins)
	if stack.external {
		t.Skip("e2e: refresh regression requires the harness-owned Postgres, MinIO, and Village process; unset external-stack variables")
	}

	configHome := filepath.Join(t.TempDir(), "config")
	apiKey := mintDemoCredentials(t, bins.setupDemo, stack.dsn, stack.villageURL, configHome)
	metadata, content := villageValidPublishFixtures(t, bins.backendDir)
	assertRefreshReferenceDataPresent(t, stack.db)

	status, body := directPublish(t, stack.villageURL, apiKey, metadata, content)
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		fatalActionable(t, actionableFailure{
			title: "pre-refresh control publish failed",
			what:  fmt.Sprintf("Village returned HTTP %d instead of 2xx; body=%s", status, body),
			why:   "the valid pinned contract fixture was rejected before any reset occurred",
			where: "internal/e2e/refresh_e2e_test.go TestHarnessRefreshE2E",
			when:  "seeding non-empty Postgres and MinIO state before refresh",
			means: "the refresh regression has no valid non-empty control state",
			fix:   "confirm the pinned Village fixtures, schema module, migrations, and demo API key agree before evaluating refresh",
		})
	}
	if got := villageTableCount(t, stack.db, "transcripts"); got == 0 {
		fatalActionable(t, actionableFailure{
			title: "pre-refresh database seed missing",
			what:  "the successful control publish left zero transcript rows",
			why:   "Village did not persist the contract fixture in the harness-owned Postgres database",
			where: "internal/e2e/refresh_e2e_test.go TestHarnessRefreshE2E",
			when:  "checking the non-empty database precondition before refresh",
			means: "database clearing cannot be proven by this run",
			fix:   "inspect the publish handler and database connection, then rerun with a freshly provisioned stack",
		})
	}
	if got := transcriptBucketObjectCount(t, stack.minioEndpoint, stack.bucket); got == 0 {
		fatalActionable(t, actionableFailure{
			title: "pre-refresh object seed missing",
			what:  "the successful control publish left zero transcript objects in MinIO",
			why:   "Village did not persist the contract fixture content in the harness-owned bucket",
			where: "internal/e2e/refresh_e2e_test.go TestHarnessRefreshE2E",
			when:  "checking the non-empty object-store precondition before refresh",
			means: "object clearing cannot be proven by this run",
			fix:   "inspect the publish storage path and MinIO configuration, then rerun with a freshly provisioned stack",
		})
	}

	apiKey = stack.refresh(t, bins, configHome)
	assertVillageHealth(t, harnessOptions{assert: true}, stack.villageURL, "after harness refresh")
	assertRefreshReferenceDataPresent(t, stack.db)

	status, body = directPublish(t, stack.villageURL, apiKey, metadata, content)
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		fatalActionable(t, actionableFailure{
			title: "post-refresh control publish failed",
			what:  fmt.Sprintf("restarted Village returned HTTP %d instead of 2xx; body=%s", status, body),
			why:   "refresh may have removed migration-owned reference rows or broken governance attribution",
			where: "internal/e2e/refresh_e2e_test.go TestHarnessRefreshE2E",
			when:  "publishing the same valid fixture after clearing mutable state and restarting Village",
			means: "the warm-stack refresh leaves the next installed-package iteration unusable",
			fix:   "preserve migration-owned tables during refresh and keep all new public tables explicitly classified",
		})
	}
	if got := villageTableCount(t, stack.db, "transcripts"); got == 0 {
		fatalActionable(t, actionableFailure{
			title: "post-refresh database publish missing",
			what:  "the post-refresh 2xx publish left zero transcript rows",
			why:   "the restarted Village did not persist the valid fixture in Postgres",
			where: "internal/e2e/refresh_e2e_test.go TestHarnessRefreshE2E",
			when:  "verifying database usability after warm-stack refresh",
			means: "a later distro iteration could appear healthy without durable publish behavior",
			fix:   "inspect the restarted Village database configuration and publish transaction",
		})
	}
	if got := transcriptBucketObjectCount(t, stack.minioEndpoint, stack.bucket); got == 0 {
		fatalActionable(t, actionableFailure{
			title: "post-refresh object publish missing",
			what:  "the post-refresh 2xx publish left zero transcript objects in MinIO",
			why:   "the restarted Village did not persist the valid fixture content in object storage",
			where: "internal/e2e/refresh_e2e_test.go TestHarnessRefreshE2E",
			when:  "verifying object-store usability after warm-stack refresh",
			means: "a later distro iteration could accept metadata while losing transcript content",
			fix:   "inspect the restarted Village S3 configuration and publish storage path",
		})
	}
}

func villageValidPublishFixtures(t *testing.T, backendDir string) ([]byte, []byte) {
	t.Helper()
	fixtureDir := filepath.Join(backendDir, "internal", "handler", "testdata", "contract", "current", "valid")
	metadata, err := os.ReadFile(filepath.Join(fixtureDir, "metadata.json"))
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "refresh contract fixture missing",
			what:  "the pinned Village valid metadata fixture could not be read",
			why:   err.Error(),
			where: "internal/e2e/refresh_e2e_test.go villageValidPublishFixtures",
			when:  "preparing the before-and-after refresh publish control",
			means: "the regression cannot exercise a contract-valid publish",
			fix:   "point VILLAGE_REPO or VILLAGE_BACKEND_DIR at the pinned checkout containing internal/handler/testdata/contract/current/valid",
		})
	}
	content, err := os.ReadFile(filepath.Join(fixtureDir, "content.json"))
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "refresh contract fixture missing",
			what:  "the pinned Village valid transcript fixture could not be read",
			why:   err.Error(),
			where: "internal/e2e/refresh_e2e_test.go villageValidPublishFixtures",
			when:  "preparing the before-and-after refresh publish control",
			means: "the regression cannot exercise durable object storage across refresh",
			fix:   "point VILLAGE_REPO or VILLAGE_BACKEND_DIR at the pinned checkout containing internal/handler/testdata/contract/current/valid",
		})
	}
	return metadata, content
}

func assertRefreshReferenceDataPresent(t *testing.T, db *sql.DB) {
	t.Helper()
	if got := villageTableCount(t, db, "licenses"); got == 0 {
		fatalActionable(t, actionableFailure{
			title: "migration-owned license references missing",
			what:  "the licenses reference table contains zero rows",
			why:   "migration-seeded contract data was not created or was deleted during refresh",
			where: "internal/e2e/refresh_e2e_test.go assertRefreshReferenceDataPresent",
			when:  "checking Village reference data before or after warm-stack refresh",
			means: "valid licensed publishes will fail and the migrated database is not reusable",
			fix:   "run Village migrations and preserve licenses outside the mutable refresh table allowlist",
		})
	}
	if got := villageTableCount(t, db, "governance_event_types"); got == 0 {
		fatalActionable(t, actionableFailure{
			title: "migration-owned governance references missing",
			what:  "the governance_event_types reference table contains zero rows",
			why:   "migration-seeded audit vocabulary was not created or was deleted during refresh",
			where: "internal/e2e/refresh_e2e_test.go assertRefreshReferenceDataPresent",
			when:  "checking Village reference data before or after warm-stack refresh",
			means: "governed transcript mutations will fail their audit foreign key",
			fix:   "run Village migrations and preserve governance_event_types outside the mutable refresh table allowlist",
		})
	}
}
