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

var expectedInstallGuides = map[string]installGuideChannel{
	"docs/install/macos.md":  channelMacOSTarball,
	"docs/install/ubuntu.md": channelDebian,
	"docs/install/arch.md":   channelArchTarball,
	"docs/install/nix.md":    channelNixProfile,
	"docs/install/wsl.md":    channelWSLDistro,
}

const (
	installGuideRowCount            = 5
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

	if fixture.DeclaredRows != len(fixture.Guides) || fixture.DeclaredRows != installGuideRowCount || len(expectedInstallGuides) != installGuideRowCount {
		return installGuidanceFixture{}, fmt.Errorf("install guidance fixture must contain exactly %d guarded rows: declared=%d actual=%d", installGuideRowCount, fixture.DeclaredRows, len(fixture.Guides))
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

		wantChannel, knownPath := expectedInstallGuides[row.Path]
		if !knownPath {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture names unsupported guide path %q", row.Path)
		}
		if row.Channel != wantChannel {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture guide %q declares channel %q, want %q", row.Path, row.Channel, wantChannel)
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

	shippedGuides := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		shippedGuides++
		guidePath := path.Join(installGuidanceDir, entry.Name())
		if _, ok := expectedInstallGuides[guidePath]; !ok {
			t.Fatalf("shipped install guide %q has no registered channel in expectedInstallGuides; add it and a fixture row", guidePath)
		}
		if _, ok := fixturePaths[guidePath]; !ok {
			t.Fatalf("shipped install guide %q is not represented in testdata/install_guidance.yaml; every guide needs a fixture row", guidePath)
		}
	}

	if shippedGuides != installGuideRowCount {
		t.Fatalf("shipped install guide count %d does not match the guarded row count %d; update the fixture and expected set together", shippedGuides, installGuideRowCount)
	}
	if len(expectedInstallGuides) != installGuideRowCount {
		t.Fatalf("expectedInstallGuides declares %d guides, want %d", len(expectedInstallGuides), installGuideRowCount)
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
	for _, marker := range []string{"reinstall", "upgrad", "config", "data"} {
		if !strings.Contains(lower, marker) {
			t.Fatalf("reinstall/upgrade section is missing the non-destructive %q behavior", marker)
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

func hasMarkdownFragment(content, fragment string) bool {
	inFence := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if isCodeFenceLine(trimmed) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if !strings.HasPrefix(trimmed, "#") {
			continue
		}
		// A real ATX heading separates the marker from the text with a space,
		// so "#comment" is ignored while "## Reset ..." is honored.
		if !strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(trimmed, "## ") &&
			!strings.HasPrefix(trimmed, "### ") && !strings.HasPrefix(trimmed, "#### ") &&
			!strings.HasPrefix(trimmed, "##### ") && !strings.HasPrefix(trimmed, "###### ") {
			continue
		}
		heading := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		if markdownHeadingSlug(heading) == fragment {
			return true
		}
	}
	return false
}

func markdownHeadingSlug(heading string) string {
	var slug strings.Builder
	dashPending := false
	for _, r := range strings.ToLower(strings.TrimSpace(heading)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			slug.WriteRune(r)
			dashPending = false
			continue
		}
		if slug.Len() > 0 {
			dashPending = true
		}
		if dashPending && !strings.HasSuffix(slug.String(), "-") {
			slug.WriteByte('-')
		}
	}
	return strings.Trim(slug.String(), "-")
}
