package ingest

import "fmt"

// fixtureDiffStatus parses the names in fixture YAML into the production enum.
// Its values exclude the String method's unknown fallback.
type fixtureDiffStatus string

const (
	fixtureDiffNew       fixtureDiffStatus = "new"
	fixtureDiffUpdated   fixtureDiffStatus = "updated"
	fixtureDiffUnchanged fixtureDiffStatus = "unchanged"
	fixtureDiffActive    fixtureDiffStatus = "active"
)

func (s fixtureDiffStatus) Status() (DiffStatus, error) {
	switch s {
	case fixtureDiffNew:
		return DiffNew, nil
	case fixtureDiffUpdated:
		return DiffUpdated, nil
	case fixtureDiffUnchanged:
		return DiffUnchanged, nil
	case fixtureDiffActive:
		return DiffActive, nil
	default:
		return 0, fmt.Errorf("unknown diff status %q; use %q, %q, %q, or %q in the fixture", s, fixtureDiffNew, fixtureDiffUpdated, fixtureDiffUnchanged, fixtureDiffActive)
	}
}
