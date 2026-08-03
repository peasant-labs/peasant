package defaults

// MockEnabled is a typed flag for whether mock mode is enabled.
type MockEnabled bool

func (m MockEnabled) String() string {
	if m {
		return "true"
	}
	return "false"
}

// DefaultMockEnabled is the default value for mock mode.
const DefaultMockEnabled MockEnabled = false

// MockComponent represents a top-level UI surface that can serve mock data.
type MockComponent string

func (c MockComponent) String() string { return string(c) }

// MockComponents namespaces component identifiers for dot-notation usage.
// Example: defaults.MockComponents.Web
var MockComponents = struct {
	Web MockComponent
	TUI MockComponent
	API MockComponent
}{
	Web: "web",
	TUI: "tui",
	API: "api",
}

// IsValidMockComponent returns true if the component is one of the known variants.
func IsValidMockComponent(c MockComponent) bool {
	switch c {
	case MockComponents.Web, MockComponents.TUI, MockComponents.API:
		return true
	}
	return false
}

// AllMockComponents is the canonical list of all known mock components.
var AllMockComponents = []MockComponent{
	MockComponents.Web, MockComponents.TUI, MockComponents.API,
}

// MockSection represents a granular section within a component that can
// independently serve mock or real data.
type MockSection string

func (s MockSection) String() string { return string(s) }

// MockSections namespaces section identifiers for dot-notation usage.
// Each section corresponds to a DataProvider method and a WebSocket channel:
//
//	Dashboard       → DashboardMetrics()  / channel "dashboard"
//	Sessions        → SessionSummaries()  / channel "sessions"
//	Trends          → TrendsData()        / channel "trends"
//	QualitySessions → QualitySessions()   / channel "quality"
//
// Example: defaults.MockSections.Dashboard
var MockSections = struct {
	Dashboard       MockSection
	Sessions        MockSection
	Trends          MockSection
	QualitySessions MockSection
	Annotations     MockSection
	Familiarity     MockSection
	Map             MockSection
	Review          MockSection
	Search          MockSection
}{
	Dashboard:       "dashboard",
	Sessions:        "sessions",
	Trends:          "trends",
	QualitySessions: "qualitySessions",
	Annotations:     "annotations",
	Familiarity:     "familiarity",
	Map:             "map",
	Review:          "review",
	Search:          "search",
}

// IsValidMockSection returns true if the section is one of the known variants.
func IsValidMockSection(s MockSection) bool {
	switch s {
	case MockSections.Dashboard, MockSections.Sessions, MockSections.Trends,
		MockSections.QualitySessions, MockSections.Annotations, MockSections.Familiarity,
		MockSections.Map, MockSections.Review, MockSections.Search:
		return true
	}
	return false
}

// AllMockSections is the canonical list of all known mock sections.
var AllMockSections = []MockSection{
	MockSections.Dashboard, MockSections.Sessions, MockSections.Trends,
	MockSections.QualitySessions, MockSections.Annotations, MockSections.Familiarity,
	MockSections.Map, MockSections.Review, MockSections.Search,
}

// DefaultMockWebSections is the default set of web sections that use mock data.
// Note: MockSections.Metrics is intentionally excluded — there is no DataProvider
// method or WebSocket channel for "metrics". The home page uses the "quality"
// channel which maps to MockSections.QualitySessions.
// MockSections.Map, MockSections.Review, and MockSections.Search are also
// excluded (like Annotations and Familiarity): they are parameterized,
// on-demand surfaces enabled explicitly via config or
// --mock-data-store=web,map,review,search.
var DefaultMockWebSections = []MockSection{
	MockSections.Dashboard, MockSections.Sessions, MockSections.Trends, MockSections.QualitySessions,
}

// DefaultMockTUISections is the default set of TUI sections that use mock data.
var DefaultMockTUISections = []MockSection{
	MockSections.Dashboard, MockSections.Sessions,
}

// DefaultMockAPISections is the default set of API sections that use mock data.
var DefaultMockAPISections = []MockSection{
	MockSections.Dashboard, MockSections.Sessions,
}
