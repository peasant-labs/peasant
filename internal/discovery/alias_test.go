package discovery_test

import (
	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
)

// Onboarding owns these types; discovery and the terminal reuse them unchanged.
var (
	_ onboarding.SessionListing      = discovery.SessionListing{}
	_ discovery.SessionListing       = ftue.SessionListing{}
	_ onboarding.SessionSource       = discovery.SessionSource{}
	_ discovery.SessionSource        = ftue.SessionSource{}
	_ onboarding.SessionSourceOrigin = ftue.SessionSourceOriginUnset
)
