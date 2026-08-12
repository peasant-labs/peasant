package peasant

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
	"unicode"

	"gopkg.in/yaml.v3"
)

//go:embed README.md docs/*.md docs/install/*.md
var installGuidanceProductionFiles embed.FS

// installGuidanceDir is the production directory whose every Markdown guide must be
// registered in the fixture. Embedding the glob keeps the read hermetic while still
// forcing a new, unregistered guide to fail the coverage guard below.
const installGuidanceDir = "docs/install"

//go:embed testdata/install_guidance.yaml
var installGuidanceFixtureYAML []byte

//go:embed testdata/heading_slug_cases.yaml
var headingSlugFixtureYAML []byte

type installGuideChannel string

const (
	channelMacOSTarball installGuideChannel = "macos-tarball"
	channelDebian       installGuideChannel = "debian-package"
	channelArchTarball  installGuideChannel = "arch-tarball"
	channelNixProfile   installGuideChannel = "nix-profile"
	channelWSLDistro    installGuideChannel = "wsl-distro"
)

type installGuidanceFixture struct {
	DeclaredRows int                         `yaml:"declared_rows"`
	Guides       []installGuidanceFixtureRow `yaml:"guides"`
}

type installGuidanceFixtureRow struct {
	Name             string              `yaml:"name"`
	Path             string              `yaml:"path"`
	Channel          installGuideChannel `yaml:"channel"`
	VersionCommand   string              `yaml:"version_command"`
	KickstartCommand string              `yaml:"kickstart_command"`
	KickstartLink    string              `yaml:"kickstart_link"`
	// ReinstallMarkers are the channel-specific substrings the reinstall/upgrade
	// section must contain. These per-channel cases live in the YAML fixture rather
	// than an inline Go switch so a single guide change updates one file.
	ReinstallMarkers []string `yaml:"reinstall_markers"`
}

// validInstallChannels is the closed set of install-channel enum values a fixture
// row may declare. The set is keyed by channel, not by guide path, so it is not a
// second copy of the guide inventory: which guides ship is discovered from the
// embedded docs/install directory and matched one-to-one against the fixture rows
// by TestInstallGuidanceCoversEveryShippedGuide.
var validInstallChannels = map[installGuideChannel]struct{}{
	channelMacOSTarball: {},
	channelDebian:       {},
	channelArchTarball:  {},
	channelNixProfile:   {},
	channelWSLDistro:    {},
}

const (
	expectedInstallVersionCommand   = "peasant version"
	expectedInstallKickstartCommand = "peasant kickstart"
	expectedInstallKickstartLink    = "../KICKSTART.md#reset-and-standalone-boundaries"
)

func decodeInstallGuidanceFixture(raw []byte) (installGuidanceFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)

	var fixture installGuidanceFixture
	if err := decoder.Decode(&fixture); err != nil {
		return installGuidanceFixture{}, fmt.Errorf("decode install guidance fixture: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture must contain exactly one YAML document")
		}
		return installGuidanceFixture{}, fmt.Errorf("decode trailing install guidance fixture document: %w", err)
	}

	if fixture.DeclaredRows != len(fixture.Guides) {
		return installGuidanceFixture{}, fmt.Errorf("install guidance fixture declared_rows=%d does not match the %d guide rows present; keep the counter in sync", fixture.DeclaredRows, len(fixture.Guides))
	}
	if fixture.DeclaredRows == 0 {
		return installGuidanceFixture{}, fmt.Errorf("install guidance fixture must declare at least one guide row")
	}

	seenNames := make(map[string]struct{}, len(fixture.Guides))
	seenPaths := make(map[string]struct{}, len(fixture.Guides))
	for _, row := range fixture.Guides {
		if strings.TrimSpace(row.Name) == "" || strings.TrimSpace(row.Path) == "" {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture contains a blank guide name or path")
		}
		if _, exists := seenNames[row.Name]; exists {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture guide name %q is duplicated", row.Name)
		}
		if _, exists := seenPaths[row.Path]; exists {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture guide path %q is duplicated", row.Path)
		}
		seenNames[row.Name] = struct{}{}
		seenPaths[row.Path] = struct{}{}

		if !strings.HasPrefix(row.Path, installGuidanceDir+"/") || !strings.HasSuffix(row.Path, ".md") {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture guide path %q must be a Markdown file under %q", row.Path, installGuidanceDir)
		}
		if _, ok := validInstallChannels[row.Channel]; !ok {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture guide %q declares unknown channel %q; add it to validInstallChannels", row.Path, row.Channel)
		}
		if row.VersionCommand != expectedInstallVersionCommand || row.KickstartCommand != expectedInstallKickstartCommand {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture guide %q must use the production commands %q then %q", row.Path, expectedInstallVersionCommand, expectedInstallKickstartCommand)
		}
		if row.KickstartLink != expectedInstallKickstartLink {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture guide %q must link the kickstart reset boundary %q", row.Path, expectedInstallKickstartLink)
		}
		if len(row.ReinstallMarkers) == 0 {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture guide %q must declare at least one channel reinstall marker", row.Path)
		}
		for _, marker := range row.ReinstallMarkers {
			if strings.TrimSpace(marker) == "" {
				return installGuidanceFixture{}, fmt.Errorf("install guidance fixture guide %q declares a blank reinstall marker", row.Path)
			}
		}
	}
	return fixture, nil
}

func TestInstallGuidanceFixtureIsStrict(t *testing.T) {
	t.Parallel()

	if _, err := decodeInstallGuidanceFixture(installGuidanceFixtureYAML); err != nil {
		t.Fatalf("canonical install guidance fixture: %v", err)
	}

	unknownField := append([]byte("unexpected_fixture_field: true\n"), installGuidanceFixtureYAML...)
	if _, err := decodeInstallGuidanceFixture(unknownField); err == nil {
		t.Fatal("install guidance fixture accepted an unknown field")
	}

	trailingDocument := append(append([]byte{}, installGuidanceFixtureYAML...), []byte("\n---\nunexpected: document\n")...)
	if _, err := decodeInstallGuidanceFixture(trailingDocument); err == nil {
		t.Fatal("install guidance fixture accepted a trailing document")
	}
}

func TestInstallGuidanceProductionFiles(t *testing.T) {
	t.Parallel()

	fixture, err := decodeInstallGuidanceFixture(installGuidanceFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}

	for _, row := range fixture.Guides {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			contentBytes, err := installGuidanceProductionFiles.ReadFile(row.Path)
			if err != nil {
				t.Fatalf("read handwritten install guide %q: %v", row.Path, err)
			}
			content := string(contentBytes)
			assertVersionLeadsToKickstart(t, content, row)
			assertReinstallGuidance(t, content, row)
			assertKickstartLink(t, content, row)
		})
	}

	readme, err := installGuidanceProductionFiles.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	readmeText := strings.ToLower(string(readme))
	if !strings.Contains(readmeText, expectedInstallKickstartCommand) {
		t.Fatalf("README no longer names %q in its quick-start path", expectedInstallKickstartCommand)
	}
	if !strings.Contains(readmeText, "docs/kickstart.md") {
		t.Fatal("README no longer links the kickstart guide")
	}
}

func TestInstallGuidanceCoversEveryShippedGuide(t *testing.T) {
	t.Parallel()

	fixture, err := decodeInstallGuidanceFixture(installGuidanceFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}

	fixturePaths := make(map[string]struct{}, len(fixture.Guides))
	for _, row := range fixture.Guides {
		fixturePaths[row.Path] = struct{}{}
	}

	entries, err := fs.ReadDir(installGuidanceProductionFiles, installGuidanceDir)
	if err != nil {
		t.Fatalf("read shipped install guide directory %q: %v", installGuidanceDir, err)
	}

	shippedPaths := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		shippedPaths[path.Join(installGuidanceDir, entry.Name())] = struct{}{}
	}

	// Require an exact one-to-one correspondence between shipped guides and fixture
	// rows: every shipped guide is registered, and no fixture row names a guide that
	// no longer ships. The guide inventory is discovered here, not duplicated in a
	// hardcoded path map or row-count constant.
	for guidePath := range shippedPaths {
		if _, ok := fixturePaths[guidePath]; !ok {
			t.Errorf("shipped install guide %q is not represented in testdata/install_guidance.yaml; add one fixture row per guide", guidePath)
		}
	}
	for guidePath := range fixturePaths {
		if _, ok := shippedPaths[guidePath]; !ok {
			t.Errorf("fixture registers %q, which is not a shipped install guide; remove the stale row or restore the guide", guidePath)
		}
	}
	if len(shippedPaths) != len(fixturePaths) {
		t.Fatalf("shipped install guide count %d does not match fixture row count %d", len(shippedPaths), len(fixturePaths))
	}
}

// markdownLinkPattern captures the target of an inline Markdown link `](target)`.
var markdownLinkPattern = regexp.MustCompile(`\]\(([^)]+)\)`)

// TestInstallGuidanceLinksResolve verifies that every in-repo relative link and
// in-page anchor in each guide resolves to a real embedded doc and heading. It is
// the guard that catches an approximate fragment (for example a heading anchor that
// silently dropped a leading section number). External `http(s)` links are out of
// scope and skipped; targets inside fenced code blocks are ignored.
func TestInstallGuidanceLinksResolve(t *testing.T) {
	t.Parallel()

	fixture, err := decodeInstallGuidanceFixture(installGuidanceFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}

	for _, row := range fixture.Guides {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			contentBytes, err := installGuidanceProductionFiles.ReadFile(row.Path)
			if err != nil {
				t.Fatalf("read guide %q: %v", row.Path, err)
			}
			content := string(contentBytes)
			prose := stripFencedBlocks(content)

			for _, match := range markdownLinkPattern.FindAllStringSubmatch(prose, -1) {
				target := strings.TrimSpace(match[1])
				if target == "" || isExternalLink(target) {
					continue
				}
				pathPart, fragment, _ := strings.Cut(target, "#")

				if pathPart == "" {
					if !hasMarkdownFragment(content, fragment) {
						t.Fatalf("guide %q in-page anchor #%s does not resolve to a heading in the same file", row.Path, fragment)
					}
					continue
				}
				if strings.HasPrefix(pathPart, "/") {
					t.Fatalf("guide %q uses absolute link %q; relative doc links keep the tree portable", row.Path, target)
				}

				resolved := path.Clean(path.Join(path.Dir(row.Path), pathPart))
				targetBytes, err := installGuidanceProductionFiles.ReadFile(resolved)
				if err != nil {
					t.Fatalf("guide %q link %q resolves to %q, which is not an embedded doc; embed the target or correct the link: %v", row.Path, target, resolved, err)
				}
				if fragment != "" && !hasMarkdownFragment(string(targetBytes), fragment) {
					t.Fatalf("guide %q link %q points to a missing heading anchor #%s in %q", row.Path, target, fragment, resolved)
				}
			}
		})
	}
}

// isExternalLink reports whether a Markdown link target points outside the repo
// (an absolute URL or mail/tel scheme) and therefore is not resolved locally.
func isExternalLink(target string) bool {
	if strings.Contains(target, "://") {
		return true
	}
	return strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "tel:")
}

// stripFencedBlocks removes fenced code-block lines so link extraction and heading
// scans never treat literal code as prose.
func stripFencedBlocks(content string) string {
	var builder strings.Builder
	inFence := false
	for _, line := range strings.Split(content, "\n") {
		if isCodeFenceLine(strings.TrimSpace(line)) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	return builder.String()
}

func assertVersionLeadsToKickstart(t *testing.T, content string, row installGuidanceFixtureRow) {
	t.Helper()
	lower := strings.ToLower(content)
	versionIndex := strings.Index(lower, strings.ToLower(row.VersionCommand))
	if versionIndex < 0 {
		t.Fatalf("guide does not show the version verification command %q", row.VersionCommand)
	}
	kickstartIndex := strings.Index(lower, strings.ToLower(row.KickstartCommand))
	if kickstartIndex < 0 {
		t.Fatalf("guide does not show the guided setup command %q", row.KickstartCommand)
	}
	if versionIndex >= kickstartIndex {
		t.Fatalf("guide puts %q before version verification %q", row.KickstartCommand, row.VersionCommand)
	}

	start := kickstartIndex - 240
	if start < 0 {
		start = 0
	}
	end := kickstartIndex + len(row.KickstartCommand) + 240
	if end > len(lower) {
		end = len(lower)
	}
	context := lower[start:end]
	for _, marker := range []string{"guided", "wizard"} {
		if !strings.Contains(context, marker) {
			t.Fatalf("guide's next-step context around %q does not identify the guided setup wizard; missing %q", row.KickstartCommand, marker)
		}
	}
}

func assertReinstallGuidance(t *testing.T, content string, row installGuidanceFixtureRow) {
	t.Helper()
	section := findInstallGuidanceSection(content, "reinstall", "upgrad")
	if section == "" {
		t.Fatal("guide has no section that explains reinstalling and upgrading")
	}
	lower := strings.ToLower(section)
	// The section is located by its heading (which already contains "reinstall"
	// and "upgrad"), so asserting those two words back would be tautological. Check
	// only BODY content: that the section names the replacement action and the
	// preserved configuration and data.
	for _, marker := range []string{"replace", "config", "data"} {
		if !strings.Contains(lower, marker) {
			t.Fatalf("reinstall/upgrade section body does not describe the non-destructive %q behavior", marker)
		}
	}
	if !strings.Contains(lower, "does not") && !strings.Contains(lower, "do not") && !strings.Contains(lower, "without") {
		t.Fatal("reinstall/upgrade section does not say that replacement leaves existing state in place")
	}

	for _, marker := range row.ReinstallMarkers {
		if !strings.Contains(lower, strings.ToLower(marker)) {
			t.Fatalf("%s reinstall/upgrade section is missing channel marker %q", row.Channel, marker)
		}
	}
}

// isCodeFenceLine reports whether a trimmed line opens or closes a Markdown code
// fence (``` or ~~~). Heading scanners toggle on it so a `#`-prefixed shell comment
// inside a fenced block is never mistaken for a heading.
func isCodeFenceLine(trimmed string) bool {
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

func findInstallGuidanceSection(content string, markers ...string) string {
	lines := strings.Split(content, "\n")
	start := -1
	inFence := false
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if isCodeFenceLine(trimmed) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		lowered := strings.ToLower(trimmed)
		if !strings.HasPrefix(lowered, "## ") {
			continue
		}
		matches := true
		for _, marker := range markers {
			if !strings.Contains(lowered, marker) {
				matches = false
				break
			}
		}
		if matches {
			start = index
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	inFence = false
	for index := start + 1; index < len(lines); index++ {
		trimmed := strings.TrimSpace(lines[index])
		if isCodeFenceLine(trimmed) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			end = index
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

func assertKickstartLink(t *testing.T, content string, row installGuidanceFixtureRow) {
	t.Helper()
	if !strings.Contains(content, "]("+row.KickstartLink+")") {
		t.Fatalf("guide does not link the kickstart rerun/reset boundary %q", row.KickstartLink)
	}

	targetPath, fragment, _ := strings.Cut(row.KickstartLink, "#")
	if targetPath == "" || fragment == "" {
		t.Fatalf("fixture link %q must include a relative target and a fragment", row.KickstartLink)
	}
	resolved := path.Clean(path.Join(path.Dir(row.Path), targetPath))
	if resolved == "docs" || !strings.HasPrefix(resolved, "docs/") {
		t.Fatalf("fixture link %q escapes the docs tree", row.KickstartLink)
	}
	targetBytes, err := installGuidanceProductionFiles.ReadFile(resolved)
	if err != nil {
		t.Fatalf("resolve kickstart link %q from %q: %v", row.KickstartLink, row.Path, err)
	}
	target := string(targetBytes)
	if !hasMarkdownFragment(target, fragment) {
		t.Fatalf("kickstart link %q points to a missing heading anchor", row.KickstartLink)
	}
	lowerTarget := strings.ToLower(target)
	for _, marker := range []string{"peasant kickstart", "--reset", "without", "preserves"} {
		if !strings.Contains(lowerTarget, marker) {
			t.Fatalf("kickstart reset boundary is missing the rerun marker %q", marker)
		}
	}
}

// hasMarkdownFragment reports whether an in-page anchor fragment resolves to a
// heading in content. It resolves against the full document-wide fragment set
// (with GitHub's duplicate-heading disambiguation) rather than the first matching
// heading, so a link to the second "#overview" (which GitHub serves as
// "#overview-1") is judged exactly as GitHub would.
func hasMarkdownFragment(content, fragment string) bool {
	_, ok := markdownFragmentSet(content)[fragment]
	return ok
}

// isATXHeading reports whether a trimmed line is a real ATX heading: the level
// marker run is separated from the text by a space, so "#comment" is ignored while
// "## Reset ..." is honored.
func isATXHeading(trimmed string) bool {
	return strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "## ") ||
		strings.HasPrefix(trimmed, "### ") || strings.HasPrefix(trimmed, "#### ") ||
		strings.HasPrefix(trimmed, "##### ") || strings.HasPrefix(trimmed, "###### ")
}

// markdownHeadings returns each ATX heading's text in document order, skipping
// fenced code blocks so a "#"-prefixed shell comment is never read as a heading.
func markdownHeadings(content string) []string {
	var headings []string
	inFence := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if isCodeFenceLine(trimmed) {
			inFence = !inFence
			continue
		}
		if inFence || !isATXHeading(trimmed) {
			continue
		}
		headings = append(headings, strings.TrimSpace(strings.TrimLeft(trimmed, "#")))
	}
	return headings
}

// markdownFragmentSet builds the set of anchor fragments a document exposes,
// following GitHub's GFM heading-anchor rules: each heading's base slug (see
// markdownHeadingSlug) is disambiguated by appending "-1", "-2", ... for the
// second, third, ... occurrence of the same base slug in document order.
func markdownFragmentSet(content string) map[string]struct{} {
	counts := make(map[string]int)
	set := make(map[string]struct{})
	for _, heading := range markdownHeadings(content) {
		base := markdownHeadingSlug(heading)
		fragment := base
		if n := counts[base]; n > 0 {
			fragment = fmt.Sprintf("%s-%d", base, n)
		}
		counts[base]++
		set[fragment] = struct{}{}
	}
	return set
}

// markdownHeadingSlug converts heading text into a GitHub-compatible anchor slug:
// lowercase, DROP every character that is not a letter, digit, space, hyphen, or
// underscore (punctuation is removed, not collapsed into a separator), then turn
// each remaining space into a hyphen. Because punctuation is removed in place, a
// heading such as "Models & providers" yields "models--providers" (the "&" is
// dropped and its two surrounding spaces each become a hyphen), matching GitHub
// rather than a single collapsed hyphen. Duplicate-heading disambiguation is
// applied by markdownFragmentSet, not here.
func markdownHeadingSlug(heading string) string {
	var slug strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(heading)) {
		switch {
		case r == ' ':
			slug.WriteByte('-')
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			slug.WriteRune(r)
		default:
			// Drop punctuation without emitting a separator (GitHub GFM behavior).
		}
	}
	return slug.String()
}

// headingSlugFixture pins the heading-anchor oracle to GitHub's GFM rules. Its
// cases are fixture-owned (not an inline table) so a new slug edge case is one
// YAML row, not a code change.
type headingSlugFixture struct {
	DeclaredSlugCases          int                        `yaml:"declared_slug_cases"`
	SlugCases                  []headingSlugCase          `yaml:"slug_cases"`
	DeclaredDuplicateDocuments int                        `yaml:"declared_duplicate_documents"`
	DuplicateDocuments         []headingDuplicateDocument `yaml:"duplicate_documents"`
}

// headingSlugCase pins one heading to the base slug GitHub GFM generates for it.
type headingSlugCase struct {
	Heading string `yaml:"heading"`
	Slug    string `yaml:"slug"`
}

// headingDuplicateDocument pins the ordered fragment set GitHub exposes for a
// document whose headings collide on their base slug.
type headingDuplicateDocument struct {
	Name      string   `yaml:"name"`
	Headings  []string `yaml:"headings"`
	Fragments []string `yaml:"fragments"`
}

func decodeHeadingSlugFixture(raw []byte) (headingSlugFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)

	var fixture headingSlugFixture
	if err := decoder.Decode(&fixture); err != nil {
		return headingSlugFixture{}, fmt.Errorf("decode heading slug fixture: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return headingSlugFixture{}, fmt.Errorf("heading slug fixture must contain exactly one YAML document")
		}
		return headingSlugFixture{}, fmt.Errorf("decode trailing heading slug fixture document: %w", err)
	}

	if fixture.DeclaredSlugCases != len(fixture.SlugCases) {
		return headingSlugFixture{}, fmt.Errorf("heading slug fixture declared_slug_cases=%d does not match the %d slug cases present", fixture.DeclaredSlugCases, len(fixture.SlugCases))
	}
	if fixture.DeclaredDuplicateDocuments != len(fixture.DuplicateDocuments) {
		return headingSlugFixture{}, fmt.Errorf("heading slug fixture declared_duplicate_documents=%d does not match the %d duplicate documents present", fixture.DeclaredDuplicateDocuments, len(fixture.DuplicateDocuments))
	}
	if fixture.DeclaredSlugCases == 0 || fixture.DeclaredDuplicateDocuments == 0 {
		return headingSlugFixture{}, fmt.Errorf("heading slug fixture must declare at least one slug case and one duplicate document")
	}
	for _, dup := range fixture.DuplicateDocuments {
		if len(dup.Headings) != len(dup.Fragments) {
			return headingSlugFixture{}, fmt.Errorf("heading slug fixture duplicate document %q has %d headings but %d fragments; they must correspond one-to-one", dup.Name, len(dup.Headings), len(dup.Fragments))
		}
	}
	return fixture, nil
}

func loadHeadingSlugFixture(t *testing.T) headingSlugFixture {
	t.Helper()
	fixture, err := decodeHeadingSlugFixture(headingSlugFixtureYAML)
	if err != nil {
		t.Fatalf("canonical heading slug fixture: %v", err)
	}
	return fixture
}

func TestHeadingSlugFixtureIsStrict(t *testing.T) {
	t.Parallel()

	if _, err := decodeHeadingSlugFixture(headingSlugFixtureYAML); err != nil {
		t.Fatalf("canonical heading slug fixture: %v", err)
	}

	unknownField := append([]byte("unexpected_fixture_field: true\n"), headingSlugFixtureYAML...)
	if _, err := decodeHeadingSlugFixture(unknownField); err == nil {
		t.Fatal("heading slug fixture accepted an unknown field")
	}

	trailingDocument := append(append([]byte{}, headingSlugFixtureYAML...), []byte("\n---\nunexpected: document\n")...)
	if _, err := decodeHeadingSlugFixture(trailingDocument); err == nil {
		t.Fatal("heading slug fixture accepted a trailing document")
	}
}

// TestMarkdownHeadingSlugMatchesGitHub verifies the base slug oracle against the
// fixture-owned GitHub GFM cases, including the punctuation cases (ampersand,
// colon, parentheses) where an approximate slugger diverges.
func TestMarkdownHeadingSlugMatchesGitHub(t *testing.T) {
	t.Parallel()

	fixture := loadHeadingSlugFixture(t)
	for _, testCase := range fixture.SlugCases {
		testCase := testCase
		t.Run(testCase.Heading, func(t *testing.T) {
			t.Parallel()
			if got := markdownHeadingSlug(testCase.Heading); got != testCase.Slug {
				t.Fatalf("markdownHeadingSlug(%q) = %q, want GitHub GFM slug %q", testCase.Heading, got, testCase.Slug)
			}
		})
	}
}

// TestMarkdownFragmentSetDisambiguatesDuplicates verifies that a document with
// colliding heading slugs exposes GitHub's -1/-2 disambiguated fragment set, and
// that hasMarkdownFragment resolves each ordinal.
func TestMarkdownFragmentSetDisambiguatesDuplicates(t *testing.T) {
	t.Parallel()

	fixture := loadHeadingSlugFixture(t)
	for _, doc := range fixture.DuplicateDocuments {
		doc := doc
		t.Run(doc.Name, func(t *testing.T) {
			t.Parallel()

			var builder strings.Builder
			for _, heading := range doc.Headings {
				builder.WriteString("## ")
				builder.WriteString(heading)
				builder.WriteString("\n\n")
			}
			content := builder.String()

			set := markdownFragmentSet(content)
			if len(set) != len(doc.Fragments) {
				t.Fatalf("document %q produced %d distinct fragments, want %d: %v", doc.Name, len(set), len(doc.Fragments), set)
			}
			for _, fragment := range doc.Fragments {
				if !hasMarkdownFragment(content, fragment) {
					t.Fatalf("document %q does not expose expected GitHub fragment %q; got %v", doc.Name, fragment, set)
				}
			}
		})
	}
}
