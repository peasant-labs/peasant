//go:build e2e

package store

import (
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
)

// Hooks for external store tests that drive the build-tagged V39 legacy-fixture
// builder without importing the package's unexported surface.

type V39FixtureRequest = v39FixtureRequest

func BuildV39E2EFixture(request V39FixtureRequest) error { return buildV39E2EFixture(request) }

func ValidateV39FixtureRequest(request V39FixtureRequest) error { return request.validate() }

func UserVersionForTest(t *testing.T, conn *sqlite.Conn) int { return upgradeUserVersion(t, conn) }

func ColumnPresentForTest(t *testing.T, conn *sqlite.Conn, table, column string) bool {
	_, present := upgradeColumn(t, conn, table, column)
	return present
}
